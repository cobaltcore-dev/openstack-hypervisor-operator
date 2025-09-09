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
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
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
	hostname := req.Name
	log := logger.FromContext(ctx).WithName(req.Name).WithValues("hostname", hostname)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Name: hv.Name}, node); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil // Node not found, nothing to do
		}
		return ctrl.Result{}, err
	}

	if !hv.Spec.LifecycleEnabled {
		// Get out of the way
		return r.removeFinalizer(ctx, node)
	}

	if !controllerutil.ContainsFinalizer(node, decommissionFinalizerName) {
		return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			patch := k8sclient.MergeFrom(node.DeepCopy())
			controllerutil.AddFinalizer(node, decommissionFinalizerName)
			if err := r.Patch(ctx, node, patch); err != nil {
				return fmt.Errorf("failed to add finalizer due to %w", err)
			}
			log.Info("Added finalizer")
			return nil
		})
	}

	// Not yet deleting hv, nothing more to do
	if node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Someone is just deleting the hv, without going through termination
	// See: https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L658-L659
	if !IsNodeConditionTrue(node.Status.Conditions, "Terminating") {
		log.Info("removing finalizer since not terminating")
		// So we just get out of the way for now
		return r.removeFinalizer(ctx, node)
	}

	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeReady) {
		return r.setDecommissioningCondition(ctx, hv, "Node is being decommissioned, removing host from nova")
	}

	log.Info("removing host from nova")

	hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, hostname, true)
	if errors.Is(err, openstack.ErrNoHypervisor) {
		// We are (hopefully) done
		return r.removeFinalizer(ctx, node)
	}

	// TODO: remove since RunningVMs is only available until micro-version 2.87, and also is updated asynchronously
	// so it might be not accurate
	if hypervisor.RunningVMs > 0 {
		// Still running VMs, cannot delete the service
		msg := fmt.Sprintf("Node is being decommissioned, but still has %d running VMs", hypervisor.RunningVMs)
		return r.setDecommissioningCondition(ctx, hv, msg)
	}

	if hypervisor.Servers != nil && len(*hypervisor.Servers) > 0 {
		// Still VMs assigned to the host, cannot delete the service
		msg := fmt.Sprintf("Node is being decommissioned, but still has %d assigned VMs, "+
			"check with `openstack server list --all-projects --host %s`", len(*hypervisor.Servers), hostname)
		return r.setDecommissioningCondition(ctx, hv, msg)
	}

	// Before removing the service, first take the node out of the aggregates,
	// so when the node comes back, it doesn't up with the old associations
	aggs, err := aggregatesByName(ctx, r.computeClient)
	if err != nil {
		return r.setDecommissioningCondition(ctx, hv, fmt.Sprintf("cannot list aggregates due to %v", err))
	}

	host := node.Name
	for name, aggregate := range aggs {
		if slices.Contains(aggregate.Hosts, host) {
			opts := aggregates.RemoveHostOpts{Host: host}
			if err = aggregates.RemoveHost(ctx, r.computeClient, aggregate.ID, opts).Err; err != nil {
				msg := fmt.Sprintf("failed to remove host %v from aggregate %v due to %v", name, host, err)
				return r.setDecommissioningCondition(ctx, hv, msg)
			}
		}
	}

	// Deleting and evicted, so better delete the service
	err = services.Delete(ctx, r.computeClient, hypervisor.Service.ID).ExtractErr()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		msg := fmt.Sprintf("cannot delete service %s due to %v", hypervisor.Service.ID, err)
		return r.setDecommissioningCondition(ctx, hv, msg)
	}

	rp, err := resourceproviders.Get(ctx, r.placementClient, hypervisor.ID).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return r.setDecommissioningCondition(ctx, hv, fmt.Sprintf("cannot get resource provider: %v", err))
	}

	if err = openstack.CleanupResourceProvider(ctx, r.placementClient, rp); err != nil {
		return r.setDecommissioningCondition(ctx, hv, fmt.Sprintf("cannot clean up resource provider: %v", err))
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

func (r *NodeDecommissionReconciler) setDecommissioningCondition(ctx context.Context, hv *kvmv1.Hypervisor, message string) (ctrl.Result, error) {
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Decommissioning",
		Message: message,
	})
	if err := r.Status().Update(ctx, hv); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot update hypervisor status due to %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDecommissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}

	r.computeClient.Microversion = "2.93"

	r.placementClient, err = openstack.GetServiceClient(ctx, "placement", nil)
	if err != nil {
		return err
	}
	r.placementClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeDecommission").
		For(&kvmv1.Hypervisor{}).
		Owns(&corev1.Node{}).
		Complete(r)
}
