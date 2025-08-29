/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1alpha1 "github.com/cobaltcore-dev/kvm-node-agent/api/v1alpha1"
)

type HypervisorController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;create;delete

func (hv *HypervisorController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)

	node := &corev1.Node{}
	if err := hv.Get(ctx, req.NamespacedName, node); err != nil {
		// Ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	hypervisor := &kvmv1alpha1.Hypervisor{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
			Labels: map[string]string{
				corev1.LabelHostname: node.Name,
			},
		},
	}

	// Ensure corresponding hypervisor exists
	if err := hv.Get(ctx, k8sclient.ObjectKeyFromObject(hypervisor), hypervisor); err != nil {
		if k8serrors.IsNotFound(err) {
			// attach ownerReference for cascading deletion
			if err = controllerutil.SetControllerReference(node, hypervisor, hv.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed setting controller reference: %w", err)
			}

			log.Info("Setup hypervisor", "name", node.Name)
			if err = hv.Create(ctx, hypervisor); err != nil {
				return ctrl.Result{}, err
			}

			// Requeue to update status
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if node.DeletionTimestamp != nil {
		// node is being deleted, cleanup hypervisor
		if err := hv.Delete(ctx, hypervisor); k8sclient.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed cleaning up hypervisor: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (hv *HypervisorController) SetupWithManager(mgr ctrl.Manager) error {
	novaVirtLabeledPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      labelHypervisor,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create label selector predicate: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Owns(&kvmv1alpha1.Hypervisor{}).
		WithEventFilter(novaVirtLabeledPredicate).
		Complete(hv)
}
