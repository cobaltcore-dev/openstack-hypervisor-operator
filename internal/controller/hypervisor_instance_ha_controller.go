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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
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

	// Determine if HA should be disabled and the reason
	evicted := meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeEvicting)
	testing := onboardingCondition.Status == metav1.ConditionTrue &&
		onboardingCondition.Reason != kvmv1.ConditionReasonHandover
	aborted := onboardingCondition.Status == metav1.ConditionFalse &&
		onboardingCondition.Reason == kvmv1.ConditionReasonAborted
	shouldDisable := !hv.Spec.HighAvailability || evicted || testing || aborted

	var desiredStatus metav1.ConditionStatus
	var reason, message string

	if shouldDisable {
		desiredStatus = metav1.ConditionFalse
		// Determine the reason based on why HA is being disabled
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
	} else {
		desiredStatus = metav1.ConditionTrue
		reason = kvmv1.ConditionReasonSucceeded
		message = "HA is enabled"
	}

	// Skip if already at desired state
	existing := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
	if existing != nil && existing.Status == desiredStatus &&
		existing.Reason == reason && existing.Message == message {
		return ctrl.Result{}, nil
	}

	// Perform the HA enable/disable action
	var actionErr error
	if shouldDisable {
		actionErr = disableInstanceHA(hv)
	} else {
		actionErr = enableInstanceHA(hv)
	}

	condStatus := desiredStatus
	condReason := reason
	condMessage := message
	if actionErr != nil {
		condStatus = metav1.ConditionUnknown
		condReason = kvmv1.ConditionReasonFailed
		condMessage = actionErr.Error()
	}

	// Only set the HaEnabled condition this controller owns
	statusCfg := apiv1.HypervisorStatus()
	statusCfg.Conditions = utils.ConditionsFromStatus(hv.Status.Conditions)
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeHaEnabled).
			WithStatus(condStatus).
			WithReason(condReason).
			WithMessage(condMessage))

	applyErr := r.Status().Apply(ctx,
		apiv1.Hypervisor(hv.Name, "").WithStatus(statusCfg),
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorInstanceHaControllerName))

	return ctrl.Result{}, errors.Join(actionErr, applyErr)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HypervisorInstanceHaController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorInstanceHaControllerName).
		For(&kvmv1.Hypervisor{}).
		Complete(r)
}
