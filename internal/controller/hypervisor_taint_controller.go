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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	HypervisorTaintControllerName = "HypervisorTaint"
)

type HypervisorTaintController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;update;patch

func (r *HypervisorTaintController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hypervisor := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hypervisor); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	var condStatus metav1.ConditionStatus
	var reason, message string
	if HasKubectlManagedFields(&hypervisor.ObjectMeta) {
		condStatus = metav1.ConditionTrue
		reason = "Kubectl"
		message = "⚠️"
	} else {
		condStatus = metav1.ConditionFalse
		reason = "NoKubectl"
		message = "🟢"
	}

	// Skip if already at the desired state
	existing := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeTainted)
	if existing != nil && existing.Status == condStatus &&
		existing.Reason == reason && existing.Message == message &&
		existing.ObservedGeneration == hypervisor.Generation {
		return ctrl.Result{}, nil
	}

	// Only set the Tainted condition this controller owns
	statusCfg := apiv1.HypervisorStatus()
	statusCfg.Conditions = utils.ConditionsFromStatus(hypervisor.Status.Conditions)
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeTainted).
			WithStatus(condStatus).
			WithReason(reason).
			WithMessage(message).
			WithObservedGeneration(hypervisor.Generation))

	return ctrl.Result{}, r.Status().Apply(ctx,
		apiv1.Hypervisor(hypervisor.Name, "").WithStatus(statusCfg),
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorTaintControllerName))
}

func (r *HypervisorTaintController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorTaintControllerName).
		For(&kvmv1.Hypervisor{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
