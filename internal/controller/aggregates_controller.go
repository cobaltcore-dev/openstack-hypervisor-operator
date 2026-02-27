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
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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

	base := hv.DeepCopy()
	desiredAggregates, desiredCondition := ac.determineDesiredState(hv)

	if !slices.Equal(desiredAggregates, hv.Status.Aggregates) {
		// Apply aggregates to OpenStack and update status
		uuids, err := openstack.ApplyAggregates(ctx, ac.computeClient, hv.Name, desiredAggregates)
		if err != nil {
			// Set error condition
			condition := metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonFailed,
				Message: fmt.Errorf("failed to apply aggregates: %w", err).Error(),
			}

			if meta.SetStatusCondition(&hv.Status.Conditions, condition) {
				if err2 := ac.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(base,
					k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(AggregatesControllerName)); err2 != nil {
					return ctrl.Result{}, errors.Join(err, err2)
				}
			}
			return ctrl.Result{}, err
		}

		hv.Status.Aggregates = desiredAggregates
		hv.Status.AggregateUUIDs = uuids
	}

	// Set the condition based on the determined desired state
	meta.SetStatusCondition(&hv.Status.Conditions, desiredCondition)

	if equality.Semantic.DeepEqual(base, hv) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, ac.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(base,
		k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(AggregatesControllerName))
}

// determineDesiredState returns the desired aggregates and the corresponding condition
// based on the hypervisor's current state. The condition status is True only when
// spec aggregates are being applied. Otherwise, it's False with a reason explaining
// why different aggregates are applied.
func (ac *AggregatesController) determineDesiredState(hv *kvmv1.Hypervisor) ([]string, metav1.Condition) {
	// If terminating AND evicted, remove from all aggregates
	// We must wait for eviction to complete before removing aggregates
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTerminating) {
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
		// Still evicting or eviction not started - keep current aggregates
		return hv.Status.Aggregates, metav1.Condition{
			Type:    kvmv1.ConditionTypeAggregatesUpdated,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonEvictionInProgress,
			Message: "Aggregates unchanged while terminating and eviction in progress",
		}
	}

	// If onboarding is in progress (Initial or Testing), add test aggregate
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition != nil && onboardingCondition.Status == metav1.ConditionTrue {
		if onboardingCondition.Reason == kvmv1.ConditionReasonInitial ||
			onboardingCondition.Reason == kvmv1.ConditionReasonTesting {
			zone := hv.Labels[corev1.LabelTopologyZone]
			return []string{zone, testAggregateName}, metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonTestAggregates,
				Message: "Test aggregate applied during onboarding instead of spec aggregates",
			}
		}

		// If removing test aggregate, use Spec.Aggregates (no test aggregate)
		if onboardingCondition.Reason == kvmv1.ConditionReasonRemovingTestAggregate {
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

// SetupWithManager sets up the controller with the Manager.
func (ac *AggregatesController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if ac.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(AggregatesControllerName).
		For(&kvmv1.Hypervisor{}, builder.WithPredicates(utils.LifecycleEnabledPredicate)).
		Complete(ac)
}
