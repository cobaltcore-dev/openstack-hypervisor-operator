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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

const (
	HypervisorTaintControllerName = "HypervisorTaint"
)

type HypervisorTaintController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch

func (r *HypervisorTaintController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hypervisor := &kvmv1.Hypervisor{
		ObjectMeta: metav1.ObjectMeta{
			Name:   req.Name,
			Labels: map[string]string{},
		},
		Spec: kvmv1.HypervisorSpec{
			HighAvailability:   true,
			InstallCertificate: true,
		},
	}

	// Check if hypervisor already exists
	if err := r.Get(ctx, req.NamespacedName, hypervisor); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	before := hypervisor.DeepCopy()
	if HasKubectlManagedFields(&hypervisor.ObjectMeta) {
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:               kvmv1.ConditionTypeTainted,
			Status:             metav1.ConditionTrue,
			Reason:             "Kubectl",
			Message:            "⚠️",
			ObservedGeneration: hypervisor.Generation,
		})
	} else {
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:               kvmv1.ConditionTypeTainted,
			Status:             metav1.ConditionFalse,
			Reason:             "NoKubectl",
			Message:            "🟢",
			ObservedGeneration: hypervisor.Generation,
		})
	}

	if equality.Semantic.DeepEqual(hypervisor, before) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.Status().Patch(ctx, hypervisor, k8sclient.MergeFromWithOptions(before,
		k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(HypervisorTaintControllerName))
}

func (r *HypervisorTaintController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorTaintControllerName).
		For(&kvmv1.Hypervisor{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
