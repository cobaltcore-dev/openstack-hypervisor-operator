/*
SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
)

const (
	decommissionFinalizerName = "cobaltcore.cloud.sap/decommission-hypervisor"
)

type NodeDecommissionReconciler struct {
	k8sclient.Client
	Scheme          *runtime.Scheme
	computeClient   *gophercloud.ServiceClient
	placementClient *gophercloud.ServiceClient
}

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=nodes/finalizers,verbs=update

func (r *NodeDecommissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	found := (labels.Set)(node.Labels).Has(labelLifecycleMode)
	if !found {
		// Get out of the way
		return r.removeFinalizer(ctx, node)
	}

	if !controllerutil.ContainsFinalizer(node, decommissionFinalizerName) {
		log.Info("Added finalizer")
		nodeBase := node.DeepCopy()
		controllerutil.AddFinalizer(node, decommissionFinalizerName)
		err := r.Patch(ctx, node, k8sclient.MergeFromWithOptions(nodeBase, k8sclient.MergeFromWithOptimisticLock{}))
		if err != nil {
			err = fmt.Errorf("failed to add finalizer due to %w", err)
		}
		return ctrl.Result{}, err
	}

	// Not yet deleting node, nothing more to do
	if node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Someone is just deleting the node, without going through termination
	if !isTerminating(node) {
		log.Info("removing finalizer since not terminating")
		// So we just get out of the way for now
		return r.removeFinalizer(ctx, node)
	}

	log.Info("removing host from nova")
	return r.shutdownService(ctx, node)
}

func (r *NodeDecommissionReconciler) shutdownService(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	hypervisorID, found := node.Labels[labelHypervisorID]
	if !found {
		hostname := node.Labels[corev1.LabelHostname]
		allPages, err := hypervisors.List(r.computeClient, hypervisors.ListOpts{HypervisorHostnamePattern: &hostname}).AllPages(ctx)
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return r.removeFinalizer(ctx, node)
		}

		hypervisorList, err := hypervisors.ExtractHypervisors(allPages)
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) || len(hypervisorList) == 0 {
			return r.removeFinalizer(ctx, node)
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot query hypervisor")
		}

		hypervisorID = hypervisorList[0].ID
	}

	hypervisor, err := hypervisors.Get(ctx, r.computeClient, hypervisorID).Extract()
	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		// We are (hopefully) done
		return r.removeFinalizer(ctx, node)
	}

	if hypervisor.RunningVMs > 0 {
		return ctrl.Result{}, fmt.Errorf("cannot shutdown service, VMs still running %v", hypervisor.RunningVMs)
	}

	// Before removing the service, first take the node out of the aggregates,
	// so when the node comes back, it doesn't up with the old associations
	aggs, err := aggregatesByName(ctx, r.computeClient)

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot list aggregates %w", err)
	}

	host := node.Name
	for name, aggregate := range aggs {
		if slices.Contains(aggregate.Hosts, host) {
			err := aggregates.RemoveHost(ctx, r.computeClient, aggregate.ID, aggregates.RemoveHostOpts{Host: host}).Err
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove host %v from aggregate %v due to %w", host, name, err)
			}
		}
	}

	// Deleting and evicted, so better delete the service
	err = services.Delete(ctx, r.computeClient, hypervisor.Service.ID).ExtractErr()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ctrl.Result{}, fmt.Errorf("cannot delete service due to %w", err)
	}

	rp, err := resourceproviders.Get(ctx, r.placementClient, hypervisorID).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ctrl.Result{}, fmt.Errorf("cannot get resource provider due to %w", err)
	}

	err = openstack.CleanupResourceProvider(ctx, r.placementClient, rp)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot clean up resource provider due to %w", err)
	}

	return r.removeFinalizer(ctx, node)
}

func (r *NodeDecommissionReconciler) removeFinalizer(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(node, decommissionFinalizerName) {
		return ctrl.Result{}, nil
	}

	nodeBase := node.DeepCopy()
	controllerutil.RemoveFinalizer(node, decommissionFinalizerName)
	err := r.Patch(ctx, node, k8sclient.MergeFromWithOptions(nodeBase, k8sclient.MergeFromWithOptimisticLock{}))
	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDecommissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute"); err != nil {
		return err
	}

	r.computeClient.Microversion = "2.93"

	r.placementClient, err = openstack.GetServiceClient(ctx, "placement")
	if err != nil {
		return err
	}
	r.placementClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeDecommission").
		For(&corev1.Node{}).
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
