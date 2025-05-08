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
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
)

const (
	annotationCustomTraits        = "nova.openstack.cloud.sap/custom-traits"
	annotationAppliedCustomTraits = "nova.openstack.cloud.sap/custom-traits-applied"
	customPrefix                  = "CUSTOM_"
)

type TraitsController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch

func (r *TraitsController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	if !hasAnyLabel(node.Labels, labelHypervisor) {
		return ctrl.Result{}, nil
	}

	hypervisorId, found := node.Labels[labelHypervisorID]
	if !found {
		return ctrl.Result{}, nil // That is expected, the label will be set eventually
	}

	toApply := extractTraits(node, annotationCustomTraits)
	applied := extractTraits(node, annotationAppliedCustomTraits)

	if maps.Equal(toApply, applied) {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	toRemove := difference(toApply, applied)
	toAdd := difference(applied, toApply)

	current, err := resourceproviders.GetTraits(ctx, r.serviceClient, hypervisorId).Extract()
	if err != nil {
		return ctrl.Result{}, err
	}

	targetTraitsSet := make(map[string]bool, len(current.Traits))
	for _, trait := range current.Traits {
		_, found := toRemove[trait]
		if !found {
			targetTraitsSet[trait] = true
		}
	}

	for item, _ := range toAdd {
		targetTraitsSet[item] = true
	}

	targetTraits := slices.Collect(maps.Keys(targetTraitsSet))
	slices.Sort(targetTraits)

	result := openstack.UpdateTraits(ctx, r.serviceClient, hypervisorId, openstack.UpdateTraitsOpts{
		ResourceProviderGeneration: current.ResourceProviderGeneration,
		Traits:                     targetTraits,
	})

	if result.Err != nil {
		return ctrl.Result{}, result.Err
	}

	_, err = setNodeAnnotations(ctx, r.Client, node, map[string]string{annotationAppliedCustomTraits: node.Annotations[annotationCustomTraits]})

	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *TraitsController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(ctx, "placement"); err != nil {
		return err
	}
	r.serviceClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named("traits").
		For(&corev1.Node{}).
		Complete(r)
}

func extractTraits(node *corev1.Node, key string) (values map[string]bool) {
	value, found := node.Annotations[key]
	if !found {
		values = make(map[string]bool, 0)
		return
	}

	unparsed := strings.Split(value, ",")
	values = make(map[string]bool, len(unparsed))
	for _, item := range unparsed {
		if item == "" {
			continue
		}
		item = strings.ToUpper(item)
		if !strings.HasPrefix(item, customPrefix) {
			item = customPrefix + item
		}

		values[item] = true
	}
	return
}

// returns all elements in b not in a
func difference(a, b map[string]bool) (diff map[string]bool) {
	diff = make(map[string]bool, 0)
	for item, _ := range b {
		_, found := a[item]
		if !found {
			diff[item] = true
		}
	}

	return
}
