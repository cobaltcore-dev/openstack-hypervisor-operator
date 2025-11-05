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
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	customPrefix               = "CUSTOM_"
	ConditionTypeTraitsUpdated = "TraitsUpdated"
	ConditionTraitsSuccess     = "Success"
	ConditionTraitsFailed      = "Failed"
	TraitsControllerName       = "traits"
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

	toAdd := Difference(customTraitsApplied, hv.Spec.CustomTraits)
	toRemove := Difference(hv.Spec.CustomTraits, customTraitsApplied)

	// fetch current traits, to ensure we don't add duplicates
	current, err := resourceproviders.GetTraits(ctx, tc.serviceClient, hv.Status.HypervisorID).Extract()
	if err != nil {
		if meta.SetStatusCondition(&hv.Status.Conditions,
			getTraitCondition(err, "Failed to get current traits from placement")) {
			err = errors.Join(tc.Status().Update(ctx, hv))
		}
		return ctrl.Result{}, err
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

		if result.Err != nil {
			// set status condition
			if meta.SetStatusCondition(&hv.Status.Conditions,
				getTraitCondition(err, "Failed to update traits in placement")) {
				err = errors.Join(tc.Status().Update(ctx, hv))
			}
			return ctrl.Result{}, err
		}
	}

	// update status unconditionally, since we want always to propagate the current traits
	hv.Status.Traits = targetTraits
	meta.SetStatusCondition(&hv.Status.Conditions, getTraitCondition(nil, "Traits successfully updated"))
	return ctrl.Result{}, tc.Status().Update(ctx, hv)
}

// getTraitCondition creates a Condition object for trait updates
func getTraitCondition(err error, msg string) metav1.Condition {
	// set status condition
	var (
		reason  = ConditionTraitsSuccess
		message = msg
		status  = metav1.ConditionTrue
	)

	if err != nil {
		status = metav1.ConditionFalse
		reason = ConditionTraitsFailed
		if msg != "" {
			message = msg + ": " + err.Error()
		} else {
			message = err.Error()
		}
	}

	return metav1.Condition{
		Type:    ConditionTypeTraitsUpdated,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (tc *TraitsController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if tc.serviceClient, err = openstack.GetServiceClient(ctx, "placement", nil); err != nil {
		return err
	}
	tc.serviceClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named(TraitsControllerName).
		For(&kvmv1.Hypervisor{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(tc)
}
