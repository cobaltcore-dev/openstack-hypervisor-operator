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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

const (
	EVICTION_REQUIRED_LABEL = "cloud.sap/hypervisor-eviction-required"
	EVICTION_APPROVED_LABEL = "cloud.sap/hypervisor-eviction-succeeded"
	HOST_LABEL              = "kubernetes.metal.cloud.sap/host" // metal3
	NAME_LABEL              = "kubernetes.metal.cloud.sap/name" // metal
	HYPERVISOR_LABEL        = "nova.openstack.cloud.sap/virt-driver"
)

type NodeEvictionLabelReconciler struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete

func (r *NodeEvictionLabelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	hostname, found := node.Labels["kubernetes.io/hostname"]
	if !found {
		// Should never happen (tm)
		return ctrl.Result{}, nil
	}

	value, found := node.Labels[EVICTION_REQUIRED_LABEL]
	name := fmt.Sprintf("maintenance-required-%v", hostname)
	eviction := kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: kvmv1.EvictionSpec{
			Hypervisor: hostname,
			Reason: fmt.Sprintf("openstack-hypervisor-operator: label %v=%v", EVICTION_REQUIRED_LABEL,
				value),
		},
	}

	var err error
	if !found {
		err = r.Delete(ctx, &eviction)
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// check for existing eviction, else create it
	if err = r.Get(ctx, k8sclient.ObjectKeyFromObject(&eviction), &eviction); err != nil {
		if k8serrors.IsNotFound(err) {
			// attach ownerReference to the eviction, so we get notified about its changes
			if err = controllerutil.SetControllerReference(node, &eviction, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Creating new eviction", "name", name)
			if err = r.Create(ctx, &eviction); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, err
	}

	// check if the eviction is already succeeded
	switch eviction.Status.EvictionState {
	case "Succeeded":
		value = "true"
	case "Failed":
		value = "false"
	default:
		value = ""
	}

	if value != "" {
		_, err = setNodeLabels(ctx, r, node, map[string]string{EVICTION_APPROVED_LABEL: value})
	}

	return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeEvictionLabelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeEvictionLabel").
		For(&corev1.Node{}).     // trigger the r.Reconcile whenever a node is created/updated/deleted.
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
