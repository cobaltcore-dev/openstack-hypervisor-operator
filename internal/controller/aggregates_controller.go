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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	AggregatesControllerName = "aggregates"
)

type AggregatesController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete

func (ac *AggregatesController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hv := &kvmv1.Hypervisor{}
	if err := ac.Get(ctx, req.NamespacedName, hv); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// Wait for onboarding controller to populate HypervisorID and ServiceID
	// before attempting to modify aggregates
	if hv.Status.HypervisorID == "" || hv.Status.ServiceID == "" {
		return ctrl.Result{}, nil
	}

	desiredAggregateNames, desiredCondition := ac.determineDesiredState(hv)

	// Extract current aggregate names for comparison
	currentAggregateNames := make([]string, len(hv.Status.Aggregates))
	for i, agg := range hv.Status.Aggregates {
		currentAggregateNames[i] = agg.Name
	}

	var newAggregates []kvmv1.Aggregate
	aggregatesChanged := false
	if !slicesEqualUnordered(desiredAggregateNames, currentAggregateNames) {
		// Apply aggregates to OpenStack and update status
		aggregates, err := openstack.ApplyAggregates(ctx, ac.computeClient, hv.Name, desiredAggregateNames)
		if err != nil {
			// Set error condition with retry on conflict
			condition := metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonFailed,
				Message: fmt.Errorf("failed to apply aggregates: %w", err).Error(),
			}

			if err2 := utils.PatchHypervisorStatusWithRetry(ctx, ac.Client, hv.Name, AggregatesControllerName, func(h *kvmv1.Hypervisor) {
				meta.SetStatusCondition(&h.Status.Conditions, condition)
			}); err2 != nil {
				return ctrl.Result{}, errors.Join(err, err2)
			}
			return ctrl.Result{}, err
		}

		newAggregates = aggregates
		aggregatesChanged = true
	}

	// Skip the round-trip when nothing would change
	existing := meta.FindStatusCondition(hv.Status.Conditions, desiredCondition.Type)
	conditionUnchanged := existing != nil &&
		existing.Status == desiredCondition.Status &&
		existing.Reason == desiredCondition.Reason &&
		existing.Message == desiredCondition.Message
	if !aggregatesChanged && conditionUnchanged {
		return ctrl.Result{}, nil
	}

	// Patch status with retry on conflict
	err := utils.PatchHypervisorStatusWithRetry(ctx, ac.Client, hv.Name, AggregatesControllerName, func(h *kvmv1.Hypervisor) {
		if aggregatesChanged {
			h.Status.Aggregates = newAggregates
		}
		meta.SetStatusCondition(&h.Status.Conditions, desiredCondition)
	})

	return ctrl.Result{}, err
}

// determineDesiredState returns the desired aggregates and the corresponding condition
// based on the hypervisor's current state. The condition status is True only when
// spec aggregates are being applied. Otherwise, it's False with a reason explaining
// why different aggregates are applied.
func (ac *AggregatesController) determineDesiredState(hv *kvmv1.Hypervisor) ([]string, metav1.Condition) {
	// If terminating AND evicted, remove from all aggregates
	// We must wait for eviction to complete before removing aggregates
	if hv.Spec.Maintenance == kvmv1.MaintenanceTermination {
		evictingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting)
		// Only remove aggregates if eviction is complete (Evicting=False)
		// If Evicting condition is not set or still True, keep current aggregates
		if evictingCondition != nil && evictingCondition.Status == metav1.ConditionFalse {
			return []string{}, metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonTerminating,
				Message: "Aggregates cleared due to termination after eviction",
			}
		}
		// Still evicting or eviction not started - keep current aggregate names
		currentAggregateNames := make([]string, len(hv.Status.Aggregates))
		for i, agg := range hv.Status.Aggregates {
			currentAggregateNames[i] = agg.Name
		}
		return currentAggregateNames, metav1.Condition{
			Type:    kvmv1.ConditionTypeAggregatesUpdated,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonEvictionInProgress,
			Message: "Aggregates unchanged while terminating and eviction in progress",
		}
	}

	// If onboarding is in progress (Initial or Testing), add test aggregate
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition != nil && onboardingCondition.Status == metav1.ConditionTrue {
		if onboardingCondition.Reason == kvmv1.ConditionReasonInitial || onboardingCondition.Reason == kvmv1.ConditionReasonTesting {
			zone := hv.Labels[corev1.LabelTopologyZone]
			return []string{zone, testAggregateName}, metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonTestAggregates,
				Message: "Test aggregate applied during onboarding instead of spec aggregates",
			}
		}

		// If the onboarding is almost complete, it will wait (among other things) for this controller to switch to Spec.Aggregates.
		// We wait for traits to be applied first to ensure sequential ordering: Traits → Aggregates.
		if onboardingCondition.Reason == kvmv1.ConditionReasonHandover {
			if !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated) {
				// Traits not yet applied — keep test aggregates and signal we're waiting
				zone := hv.Labels[corev1.LabelTopologyZone]
				return []string{zone, testAggregateName}, metav1.Condition{
					Type:    kvmv1.ConditionTypeAggregatesUpdated,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonWaitingForTraits,
					Message: "Waiting for traits to be applied before switching to spec aggregates",
				}
			}
			return hv.Spec.Aggregates, metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionTrue,
				Reason:  kvmv1.ConditionReasonSucceeded,
				Message: "Aggregates from spec applied successfully",
			}
		}
	}

	// Normal operations or onboarding complete: use Spec.Aggregates
	return hv.Spec.Aggregates, metav1.Condition{
		Type:    kvmv1.ConditionTypeAggregatesUpdated,
		Status:  metav1.ConditionTrue,
		Reason:  kvmv1.ConditionReasonSucceeded,
		Message: "Aggregates from spec applied successfully",
	}
}

// slicesEqualUnordered compares two string slices without considering order.
// Returns true if both slices contain the same elements, regardless of order.
func slicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	// Create a map to count occurrences in slice a
	counts := make(map[string]int)
	for _, s := range a {
		counts[s]++
	}

	// Verify all elements in b exist in a with correct counts
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (ac *AggregatesController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if ac.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(AggregatesControllerName).
		For(&kvmv1.Hypervisor{}, builder.WithPredicates(utils.LifecycleEnabledPredicate)).
		Complete(ac)
}
