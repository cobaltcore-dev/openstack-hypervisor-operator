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
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	customPrefix         = "CUSTOM_"
	TraitsControllerName = "traits"
)

type TraitsController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete

func (tc *TraitsController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hv := &kvmv1.Hypervisor{}
	if err := tc.Get(ctx, req.NamespacedName, hv); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// apply traits only when lifecycle management is enabled
	if !hv.Spec.LifecycleEnabled {
		return ctrl.Result{}, nil
	}

	// ensure hypervisorID is set
	if hv.Status.HypervisorID == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if hv.Spec.Maintenance == kvmv1.MaintenanceTermination {
		return ctrl.Result{}, nil
	}

	// Only run when onboarding is complete (False) or in Handover phase
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition == nil {
		// Onboarding hasn't started yet
		return ctrl.Result{}, nil
	}
	if onboardingCondition.Status == metav1.ConditionTrue && onboardingCondition.Reason != kvmv1.ConditionReasonHandover {
		// Onboarding is in progress (Initial/Testing) — not yet at Handover
		return ctrl.Result{}, nil
	}

	customTraitsApplied := slices.Collect(func(yield func(string) bool) {
		for _, trait := range hv.Status.Traits {
			if strings.HasPrefix(trait, customPrefix) && !yield(trait) {
				return
			}
		}
	})

	if slices.Equal(hv.Spec.CustomTraits, customTraitsApplied) {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	toAdd := utils.Difference(customTraitsApplied, hv.Spec.CustomTraits)
	toRemove := utils.Difference(hv.Spec.CustomTraits, customTraitsApplied)

	// fetch current traits, to ensure we don't add duplicates
	current, err := resourceproviders.GetTraits(ctx, tc.serviceClient, hv.Status.HypervisorID).Extract()
	if err != nil {
		condition := getTraitCondition(err, "Failed to get current traits from placement")
		patchErr := utils.PatchHypervisorStatusWithRetry(ctx, tc.Client, hv.Name, TraitsControllerName, func(h *kvmv1.Hypervisor) {
			meta.SetStatusCondition(&h.Status.Conditions, condition)
		})
		return ctrl.Result{}, errors.Join(err, patchErr)
	}

	var targetTraits []string
	slices.Sort(current.Traits)
	for _, trait := range current.Traits {
		if !slices.Contains(toRemove, trait) {
			targetTraits = append(targetTraits, trait)
		}
	}

	for _, traitToAdd := range toAdd {
		// avoid duplicates in case the trait is already present
		if !slices.Contains(targetTraits, traitToAdd) {
			targetTraits = append(targetTraits, traitToAdd)
		}
	}
	slices.Sort(targetTraits)

	if !slices.Equal(current.Traits, targetTraits) {
		result := openstack.UpdateTraits(ctx, tc.serviceClient, hv.Status.HypervisorID, openstack.UpdateTraitsOpts{
			ResourceProviderGeneration: current.ResourceProviderGeneration,
			Traits:                     targetTraits,
		})
		err = result.Err
		if err != nil {
			condition := getTraitCondition(err, "Failed to update traits in placement")
			patchErr := utils.PatchHypervisorStatusWithRetry(ctx, tc.Client, hv.Name, TraitsControllerName, func(h *kvmv1.Hypervisor) {
				meta.SetStatusCondition(&h.Status.Conditions, condition)
			})
			return ctrl.Result{}, errors.Join(err, patchErr)
		}
	}

	// update status unconditionally, since we want always to propagate the current traits
	err = utils.PatchHypervisorStatusWithRetry(ctx, tc.Client, hv.Name, TraitsControllerName, func(h *kvmv1.Hypervisor) {
		h.Status.Traits = targetTraits
		meta.SetStatusCondition(&h.Status.Conditions, getTraitCondition(nil, "Traits successfully updated"))
	})
	return ctrl.Result{}, err
}

// getTraitCondition creates a Condition object for trait updates
func getTraitCondition(err error, msg string) metav1.Condition {
	// set status condition
	var (
		reason  = kvmv1.ConditionReasonSucceeded
		message = msg
		status  = metav1.ConditionTrue
	)

	if err != nil {
		status = metav1.ConditionFalse
		reason = kvmv1.ConditionReasonFailed
		if msg != "" {
			message = msg + ": " + err.Error()
		} else {
			message = err.Error()
		}
	}

	return metav1.Condition{
		Type:    kvmv1.ConditionTypeTraitsUpdated,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (tc *TraitsController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if tc.serviceClient, err = openstack.GetServiceClient(ctx, "placement", nil); err != nil {
		return err
	}
	tc.serviceClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named(TraitsControllerName).
		For(&kvmv1.Hypervisor{}).
		Complete(tc)
}
