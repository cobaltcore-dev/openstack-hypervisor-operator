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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
)

const (
	decommissionFinalizerName = "cobaltcore.cloud.sap/decommission-hypervisor"
	LIFECYCLE_OPT_IN          = "cobaltcore.cloud.sap/node-hypervisor-lifecycle"
)

type NodeDecommissionReconciler struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
}

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
func (r *NodeDecommissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	found := hasAnyLabel(node.Labels, LIFECYCLE_OPT_IN)

	if !found {
		// Get out of the way
		var err error
		if controllerutil.RemoveFinalizer(node, decommissionFinalizerName) {
			err = r.Update(ctx, node)
			if err != nil {
				err = fmt.Errorf("failed to remove finalizer due to %w", err)
			}
		}

		return ctrl.Result{}, err
	}

	if controllerutil.AddFinalizer(node, decommissionFinalizerName) {
		log.Info("Added finalizer")
		err := r.Update(ctx, node)
		if err != nil {
			err = fmt.Errorf("failed to add finalizer due to %w", err)
		}
		return ctrl.Result{}, err
	}

	conditions := node.Status.Conditions
	if conditions == nil {
		return ctrl.Result{}, nil
	}

	// First event exposed by
	// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L658-L659
	terminating := false
	for _, condition := range conditions {
		if condition.Type == "Terminating" {
			terminating = true
			break
		}
	}

	onboarded := hasAnyLabel(node.Labels, ONBOARDING_STATE_LABEL)

	var eviction *kvmv1.Eviction
	if terminating && onboarded {
		name := fmt.Sprintf("decomissioning-%v", node.Name)
		eviction = &kvmv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: kvmv1.EvictionSpec{
				Hypervisor: node.Name,
				Reason:     "openstack-hypervisor-operator: decommissioning",
			},
		}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, eviction, func() error {
			addNodeOwnerReference(&eviction.ObjectMeta, node)
			// attach ownerReference to the eviction, so we get notified about its changes
			return controllerutil.SetControllerReference(node, eviction, r.Scheme)
		})

		if err != nil {
			return ctrl.Result{}, k8sclient.IgnoreAlreadyExists(err)
		}
	}

	// Not yet deleting node, nothing more to do
	if node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if !terminating {
		// Someone just deleted the node without shutting it down, so it might as well come back
		return r.removeFinalizer(ctx, node)

	}

	if !onboarded {
		return r.shutdownService(ctx, node)
	}

	key := k8sclient.ObjectKeyFromObject(eviction)
	if err := r.Client.Get(ctx, key, eviction); err != nil {
		return ctrl.Result{}, err
	}

	// Only allow continue deletion when the node is evicted
	switch eviction.Status.EvictionState {
	case "Succeeded", "Failed":
		return r.shutdownService(ctx, node)
	default:
		return ctrl.Result{}, nil
	}
}

func (r *NodeDecommissionReconciler) shutdownService(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	// Remove the label for the hypervisor to shutdown the agents
	changed, err := unsetNodeLabels(ctx, r.Client, node, HYPERVISOR_LABEL)
	if err != nil || changed { // reconcile again or retry
		return ctrl.Result{}, err
	}

	host, err := normalizeName(node)

	if err != nil {
		return ctrl.Result{}, err
	}

	// Getting deleted, so we better clean-up
	computeHostQuery := services.ListOpts{Binary: "nova-compute", Host: host}
	hostPages, err := services.List(r.serviceClient, computeHostQuery).AllPages(ctx)

	var anyError error
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		serviceList, err := services.ExtractServices(hostPages)
		if err != nil {
			return ctrl.Result{}, err
		}

		for _, service := range serviceList {
			// Deleting and evicted, so better delete the service
			err = services.Delete(ctx, r.serviceClient, service.ID).ExtractErr()
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				anyError = err
			}
		}

		if anyError != nil {
			return ctrl.Result{}, err
		}
	}

	// Host empty, service deleted, we are good to go
	return r.removeFinalizer(ctx, node)
}

func (r *NodeDecommissionReconciler) removeFinalizer(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	if controllerutil.RemoveFinalizer(node, decommissionFinalizerName) {
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// normalizeName returns the host name of the node.
func normalizeName(node *corev1.Node) (string, error) {
	if name, found := node.Labels[NAME_LABEL]; found {
		return name, nil
	}

	if host, found := node.Labels[HOST_LABEL]; found {
		return host, nil
	}

	return "", errors.New("could not find node name")
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDecommissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}

	if err != nil {
		return fmt.Errorf("could not create label selector due to %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeDecommission").
		For(&corev1.Node{}).
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
