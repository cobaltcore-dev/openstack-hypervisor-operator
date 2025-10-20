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
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	ConditionTypeAggregatesUpdated = "AggregatesUpdated"
	ConditionAggregatesSuccess     = "Success"
	ConditionAggregatesFailed      = "Failed"
	AggregatesControllerName       = "aggregates"
)

type AggregatesController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete

func (ac *AggregatesController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := ac.Get(ctx, req.NamespacedName, hv); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// apply traits only when lifecycle management is enabled
	if !hv.Spec.LifecycleEnabled {
		return ctrl.Result{}, nil
	}

	if slices.Equal(hv.Spec.Aggregates, hv.Status.Aggregates) {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	aggs, err := aggregatesByName(ctx, ac.computeClient)
	if err != nil {
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeAggregatesUpdated,
			Status:  metav1.ConditionFalse,
			Reason:  ConditionAggregatesFailed,
			Message: err.Error(),
		})
		return ctrl.Result{}, ac.Status().Update(ctx, hv)
	}

	toAdd := Difference(hv.Status.Aggregates, hv.Spec.Aggregates)
	toRemove := Difference(hv.Spec.Aggregates, hv.Status.Aggregates)

	if len(toAdd) > 0 {
		log.Info("Adding", "aggregates", toAdd)
		for item := range slices.Values(toAdd) {
			err = addToAggregate(ctx, ac.computeClient, aggs, hv.Name, item, "")
			if err != nil {
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    ConditionTypeAggregatesUpdated,
					Status:  metav1.ConditionFalse,
					Reason:  ConditionAggregatesFailed,
					Message: err.Error(),
				})
				return ctrl.Result{}, ac.Status().Update(ctx, hv)
			}
		}
	}

	if len(toRemove) > 0 {
		log.Info("Removing", "aggregates", toRemove)
		for item := range slices.Values(toRemove) {
			err = removeFromAggregate(ctx, ac.computeClient, aggs, hv.Name, item)
			if err != nil {
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    ConditionTypeAggregatesUpdated,
					Status:  metav1.ConditionFalse,
					Reason:  ConditionAggregatesFailed,
					Message: err.Error(),
				})
				return ctrl.Result{}, ac.Status().Update(ctx, hv)
			}
		}
	}

	hv.Status.Aggregates = hv.Spec.Aggregates
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeAggregatesUpdated,
		Status:  metav1.ConditionTrue,
		Reason:  ConditionAggregatesSuccess,
		Message: "Aggregates updated successfully",
	})
	return ctrl.Result{}, ac.Status().Update(ctx, hv)
}

// SetupWithManager sets up the controller with the Manager.
func (ac *AggregatesController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if ac.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	ac.computeClient.Microversion = "2.40" // gophercloud only supports numeric ids

	return ctrl.NewControllerManagedBy(mgr).
		Named(AggregatesControllerName).
		For(&kvmv1.Hypervisor{}).
		Complete(ac)
}

func aggregatesByName(ctx context.Context, serviceClient *gophercloud.ServiceClient) (map[string]*aggregates.Aggregate, error) {
	pages, err := aggregates.List(serviceClient).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list aggregates due to %w", err)
	}

	aggs, err := aggregates.ExtractAggregates(pages)
	if err != nil {
		return nil, fmt.Errorf("cannot list aggregates due to %w", err)
	}

	aggregateMap := make(map[string]*aggregates.Aggregate, len(aggs))
	for _, aggregate := range aggs {
		aggregateMap[aggregate.Name] = &aggregate
	}
	return aggregateMap, nil
}

func addToAggregate(ctx context.Context, serviceClient *gophercloud.ServiceClient, aggs map[string]*aggregates.Aggregate, host, name, zone string) (err error) {
	aggregate, found := aggs[name]
	log := logger.FromContext(ctx)
	if !found {
		aggregate, err = aggregates.Create(ctx, serviceClient,
			aggregates.CreateOpts{
				Name:             name,
				AvailabilityZone: zone,
			}).Extract()
		if err != nil {
			return fmt.Errorf("failed to create aggregate %v due to %w", name, err)
		}
		aggs[name] = aggregate
	}

	if slices.Contains(aggregate.Hosts, host) {
		log.Info("Found host in aggregate", "host", host, "name", name)
		return nil
	}

	result, err := aggregates.AddHost(ctx, serviceClient, aggregate.ID, aggregates.AddHostOpts{Host: host}).Extract()
	if err != nil {
		return fmt.Errorf("failed to add host %v to aggregate %v due to %w", host, name, err)
	}
	log.Info("Added host to aggregate", "host", host, "name", name)
	aggs[name] = result

	return nil
}

func removeFromAggregate(ctx context.Context, serviceClient *gophercloud.ServiceClient, aggs map[string]*aggregates.Aggregate, host, name string) error {
	aggregate, found := aggs[name]
	log := logger.FromContext(ctx)
	if !found {
		log.Info("cannot find aggregate", "name", name)
		return nil
	}

	found = false
	for _, aggHost := range aggregate.Hosts {
		if aggHost == host {
			found = true
		}
	}

	if !found {
		log.Info("cannot find host in aggregate", "host", host, "name", name)
		return nil
	}

	result, err := aggregates.RemoveHost(ctx, serviceClient, aggregate.ID, aggregates.RemoveHostOpts{Host: host}).Extract()
	if err != nil {
		return fmt.Errorf("failed to add host %v to aggregate %v due to %w", host, name, err)
	}
	aggs[name] = result
	log.Info("removed host from aggregate", "host", host, "name", name)

	return nil
}
