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

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	HypervisorComputeServiceControllerName = "HypervisorComputeService"
)

type HypervisorComputeServiceController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;update;patch
func (r *HypervisorComputeServiceController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// We can only control the compute service if there is a serviceID
	serviceId := hv.Status.ServiceID
	if serviceId == "" {
		return ctrl.Result{}, nil // Service not yet registered, nothing to do
	}

	// If onboarding hasn't started yet, no compute service exists
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition == nil {
		return ctrl.Result{}, nil // Onboarding not started, so no compute service
	}

	old := hv.DeepCopy()

	// Determine if compute service should be disabled and the reason
	onboardingInProgress := onboardingCondition.Status == metav1.ConditionTrue &&
		(onboardingCondition.Reason == kvmv1.ConditionReasonTesting ||
			onboardingCondition.Reason == kvmv1.ConditionReasonHandover)
	maintenanceDisabling := hv.Spec.Maintenance == kvmv1.MaintenanceNoSchedule ||
		hv.Spec.Maintenance == kvmv1.MaintenanceManual ||
		hv.Spec.Maintenance == kvmv1.MaintenanceAuto ||
		hv.Spec.Maintenance == kvmv1.MaintenanceTermination
	aborted := onboardingCondition.Status == metav1.ConditionFalse &&
		onboardingCondition.Reason == kvmv1.ConditionReasonAborted

	// During onboarding (testing/handover), always enable the compute service
	// Otherwise, disable if maintenance mode requires it or onboarding was aborted
	shouldDisable := !onboardingInProgress && (maintenanceDisabling || aborted)

	if shouldDisable {
		// Determine the reason based on why compute service is being disabled
		var reason string
		var message string
		switch {
		case aborted:
			reason = kvmv1.ConditionReasonAborted
			message = "Compute service disabled due to aborted onboarding"
		case hv.Spec.Maintenance == kvmv1.MaintenanceNoSchedule:
			reason = kvmv1.ConditionReasonSucceeded
			message = "Compute service disabled per spec (no-schedule)"
		case hv.Spec.Maintenance == kvmv1.MaintenanceTermination:
			reason = kvmv1.ConditionReasonTerminating
			message = "Compute service disabled due to termination"
		default:
			reason = kvmv1.ConditionReasonSucceeded
			message = fmt.Sprintf("Compute service disabled per spec (maintenance: %s)", hv.Spec.Maintenance)
		}

		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionTrue,
			Message: message,
			Reason:  reason,
		}) {
			// Desired state already achieved
			return ctrl.Result{}, nil
		}

		// Update Ready condition
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonReadyMaintenance,
			Message: "Hypervisor compute service is disabled",
		})

		log.Info("Disabling compute service", "id", serviceId, "reason", reason)
		opts := services.UpdateOpts{
			Status:         services.ServiceDisabled,
			DisabledReason: message,
		}
		if _, err := services.Update(ctx, r.computeClient, serviceId, opts).Extract(); err != nil {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeHypervisorDisabled,
				Status:  metav1.ConditionUnknown,
				Message: err.Error(),
				Reason:  kvmv1.ConditionReasonFailed,
			})

			if patchErr := r.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(old, k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(HypervisorComputeServiceControllerName)); patchErr != nil {
				return ctrl.Result{}, errors.Join(err, patchErr)
			}
			return ctrl.Result{}, err
		}
	} else {
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionFalse,
			Message: "Compute service is enabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Desired state already achieved
			return ctrl.Result{}, nil
		}

		// Only update Ready condition if we're not in other maintenance states
		if hv.Spec.Maintenance == kvmv1.MaintenanceUnset || hv.Spec.Maintenance == kvmv1.MaintenanceHA {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeReady,
				Status:  metav1.ConditionTrue,
				Reason:  kvmv1.ConditionReasonReadyReady,
				Message: "Hypervisor is ready",
			})
		}

		log.Info("Enabling compute service", "id", serviceId)
		// The service may be forced down previously due to an HA event,
		// so we need to ensure it not only enabled, but also not forced to be down.
		falseVal := false
		opts := openstack.UpdateServiceOpts{
			Status:     services.ServiceEnabled,
			ForcedDown: &falseVal,
		}
		if _, err := services.Update(ctx, r.computeClient, serviceId, opts).Extract(); err != nil {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeHypervisorDisabled,
				Status:  metav1.ConditionUnknown,
				Message: err.Error(),
				Reason:  kvmv1.ConditionReasonFailed,
			})

			if patchErr := r.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(old, k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(HypervisorComputeServiceControllerName)); patchErr != nil {
				return ctrl.Result{}, errors.Join(err, patchErr)
			}
			return ctrl.Result{}, err
		}
	}

	if equality.Semantic.DeepEqual(hv, old) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(old, k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(HypervisorComputeServiceControllerName))
}

// SetupWithManager sets up the controller with the Manager.
func (r *HypervisorComputeServiceController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.90" // Xena (or later)

	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorComputeServiceControllerName).
		For(&kvmv1.Hypervisor{}). // trigger the r.Reconcile whenever a hypervisor is created/updated/deleted.
		Complete(r)
}
