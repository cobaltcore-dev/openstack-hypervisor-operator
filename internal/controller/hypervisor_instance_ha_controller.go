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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	HypervisorInstanceHaControllerName = "HypervisorInstanceHa"
)

type HypervisorInstanceHaController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;update;patch
func (r *HypervisorInstanceHaController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)

	if onboardingCondition == nil {
		return ctrl.Result{}, nil // Onboarding not started, so no hypervisor and nothing to do
	}

	old := hv.DeepCopy()

	// Determine if HA should be disabled and the reason
	evicted := meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeEvicting)
	testing := onboardingCondition.Status == metav1.ConditionTrue &&
		onboardingCondition.Reason != kvmv1.ConditionReasonHandover // Onboarding still testing (not yet in Handover phase)
	aborted := onboardingCondition.Status == metav1.ConditionFalse &&
		onboardingCondition.Reason == kvmv1.ConditionReasonAborted // Onboarding was aborted
	shouldDisable := !hv.Spec.HighAvailability || // HA not requested
		evicted || // HA not needed as it is empty
		testing || // HA not needed for test VMs
		aborted // HA not needed when onboarding aborted

	if shouldDisable {
		// Determine the reason based on why HA is being disabled
		var reason string
		var message string
		switch {
		case evicted:
			reason = kvmv1.ConditionReasonHaEvicted
			message = "HA disabled due to eviction"
		case testing, aborted:
			reason = kvmv1.ConditionReasonHaOnboarding
			message = "HA disabled before onboarding"
		default:
			reason = kvmv1.ConditionReasonSucceeded
			message = "HA disabled per spec"
		}

		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHaEnabled,
			Status:  metav1.ConditionFalse,
			Message: message,
			Reason:  reason,
		}) {
			// Desired state already achieved
			return ctrl.Result{}, nil
		}

		if err := disableInstanceHA(hv); err != nil {
			condition := metav1.Condition{
				Type:    kvmv1.ConditionTypeHaEnabled,
				Status:  metav1.ConditionUnknown,
				Message: err.Error(),
				Reason:  kvmv1.ConditionReasonFailed,
			}

			patchErr := utils.PatchHypervisorStatusWithRetry(ctx, r.Client, hv.Name, HypervisorInstanceHaControllerName, func(h *kvmv1.Hypervisor) {
				meta.SetStatusCondition(&h.Status.Conditions, condition)
			})
			return ctrl.Result{}, errors.Join(err, patchErr)
		}
	} else {
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHaEnabled,
			Status:  metav1.ConditionTrue,
			Message: "HA is enabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Desired state already achieved
			return ctrl.Result{}, nil
		}

		if err := enableInstanceHA(hv); err != nil {
			condition := metav1.Condition{
				Type:    kvmv1.ConditionTypeHaEnabled,
				Status:  metav1.ConditionUnknown,
				Message: err.Error(),
				Reason:  kvmv1.ConditionReasonFailed,
			}

			patchErr := utils.PatchHypervisorStatusWithRetry(ctx, r.Client, hv.Name, HypervisorInstanceHaControllerName, func(h *kvmv1.Hypervisor) {
				meta.SetStatusCondition(&h.Status.Conditions, condition)
			})
			return ctrl.Result{}, errors.Join(err, patchErr)
		}
	}

	if equality.Semantic.DeepEqual(hv, old) {
		return ctrl.Result{}, nil
	}

	// Only set the HaEnabled condition this controller owns
	haCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
	return ctrl.Result{}, utils.PatchHypervisorStatusWithRetry(ctx, r.Client, hv.Name, HypervisorInstanceHaControllerName, func(h *kvmv1.Hypervisor) {
		if haCondition != nil {
			meta.SetStatusCondition(&h.Status.Conditions, *haCondition)
		}
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *HypervisorInstanceHaController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorInstanceHaControllerName).
		For(&kvmv1.Hypervisor{}). // trigger the r.Reconcile whenever a hypervisor is created/updated/deleted.
		Complete(r)
}
