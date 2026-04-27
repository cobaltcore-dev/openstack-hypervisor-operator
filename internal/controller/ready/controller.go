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

package ready

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	ControllerName = "ready"
)

// StatusChangedPredicate triggers reconciliation only when the status subresource changes.
// This is the inverse of GenerationChangedPredicate which triggers on spec changes.
type StatusChangedPredicate struct{}

var _ predicate.Predicate = StatusChangedPredicate{}

func (StatusChangedPredicate) Create(_ event.CreateEvent) bool {
	// Reconcile on create to compute initial Ready condition
	return true
}

func (StatusChangedPredicate) Delete(_ event.DeleteEvent) bool {
	// No need to reconcile on delete
	return false
}

func (StatusChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}

	oldHv, ok := e.ObjectOld.(*kvmv1.Hypervisor)
	if !ok {
		return false
	}
	newHv, ok := e.ObjectNew.(*kvmv1.Hypervisor)
	if !ok {
		return false
	}

	// Trigger if status changed
	if !equality.Semantic.DeepEqual(oldHv.Status, newHv.Status) {
		return true
	}

	// Also trigger if Maintenance field changed (affects Ready condition)
	if oldHv.Spec.Maintenance != newHv.Spec.Maintenance {
		return true
	}

	return false
}

func (StatusChangedPredicate) Generic(_ event.GenericEvent) bool {
	return true
}

// Controller reconciles the Ready condition based on other conditions
type Controller struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;patch

func (r *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	base := hv.DeepCopy()

	// Compute Ready condition based on other conditions
	readyCondition := ComputeReadyCondition(hv)
	meta.SetStatusCondition(&hv.Status.Conditions, readyCondition)

	if equality.Semantic.DeepEqual(hv.Status, base.Status) {
		return ctrl.Result{}, nil
	}

	log.Info("Updating Ready condition", "status", readyCondition.Status, "reason", readyCondition.Reason)
	return ctrl.Result{}, utils.PatchHypervisorStatusWithRetry(ctx, r.Client, req.Name, ControllerName, func(h *kvmv1.Hypervisor) {
		meta.SetStatusCondition(&h.Status.Conditions, readyCondition)
	})
}

// ComputeReadyCondition determines the Ready condition based on other conditions
func ComputeReadyCondition(hv *kvmv1.Hypervisor) metav1.Condition {
	// Priority 1: Offboarded
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeOffboarded) {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Offboarded",
			Message: "Hypervisor has been offboarded",
		}
	}

	// Priority 2: Terminating
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTerminating) {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonTerminating,
			Message: "Hypervisor is terminating",
		}
	}

	// Priority 3: Active onboarding (Status=True means onboarding is in progress)
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition != nil && onboardingCondition.Status == metav1.ConditionTrue {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonOnboarding,
			Message: "Onboarding in progress: " + onboardingCondition.Message,
		}
	}

	// Priority 4: Maintenance mode
	if hv.Spec.Maintenance != kvmv1.MaintenanceUnset {
		evictingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting)
		if evictingCondition != nil {
			if evictingCondition.Status == metav1.ConditionTrue {
				return metav1.Condition{
					Type:    kvmv1.ConditionTypeReady,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonReadyEvicting,
					Message: "Hypervisor is disabled and evicting",
				}
			}
			if evictingCondition.Status == metav1.ConditionFalse {
				return metav1.Condition{
					Type:    kvmv1.ConditionTypeReady,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonReadyEvicted,
					Message: "Hypervisor is disabled and evicted",
				}
			}
		}
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonReadyMaintenance,
			Message: "Hypervisor is in maintenance mode",
		}
	}

	// Priority 5: Onboarding not started, aborted, or not succeeded
	if onboardingCondition == nil {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonOnboarding,
			Message: "Onboarding not started",
		}
	}

	if onboardingCondition.Reason == kvmv1.ConditionReasonAborted {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonOnboarding,
			Message: "Onboarding was aborted",
		}
	}

	if onboardingCondition.Reason != kvmv1.ConditionReasonSucceeded {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonOnboarding,
			Message: "Onboarding not yet completed",
		}
	}

	// Priority 6: All checks passed - Ready
	return metav1.Condition{
		Type:    kvmv1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  kvmv1.ConditionReasonReadyReady,
		Message: "Hypervisor is ready",
	}
}

func (r *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&kvmv1.Hypervisor{}, builder.WithPredicates(StatusChangedPredicate{})).
		Complete(r)
}
