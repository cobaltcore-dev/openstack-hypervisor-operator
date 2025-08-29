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
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
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
	annotationAggregates        = "nova.openstack.cloud.sap/aggregates"
	annotationAggregatesApplied = "nova.openstack.cloud.sap/aggregates-applied"
)

type AggregatesController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
func (r *AggregatesController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	hv := kvmv1.Hypervisor{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Name: node.Name}, &hv); k8sclient.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	if !hv.Spec.LifecycleEnabled {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	computeHost := node.Name
	toApply := extractAnnotationList(node, annotationAggregates)
	applied := extractAnnotationList(node, annotationAggregatesApplied)

	if maps.Equal(toApply, applied) {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	aggs, err := aggregatesByName(ctx, r.computeClient)
	if err != nil {
		return ctrl.Result{}, err
	}

	toRemove := difference(toApply, applied)
	toAdd := difference(applied, toApply)

	if len(toAdd) > 0 {
		log.Info("Adding", "aggregates", toAdd)
		for item := range toAdd {
			err = addToAggregate(ctx, r.computeClient, aggs, computeHost, item, "")
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if len(toRemove) > 0 {
		log.Info("Removing", "aggregates", toRemove)
		for item := range toRemove {
			err = removeFromAggregate(ctx, r.computeClient, aggs, computeHost, item)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	err = setNodeAnnotations(ctx, r.Client, node, map[string]string{annotationAggregatesApplied: node.Annotations[annotationAggregates]})

	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *AggregatesController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.40" // gophercloud only supports numeric ids

	return ctrl.NewControllerManagedBy(mgr).
		Named("aggregates").
		For(&corev1.Node{}).
		Complete(r)
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

	for _, aggHost := range aggregate.Hosts {
		if aggHost == host {
			log.Info("Found host in aggregate", "host", host, "name", name)
			return nil
		}
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

func extractAnnotationList(node *corev1.Node, key string) (values map[string]bool) {
	return extractAnnotationListInternal(node, key, nil)
}

type normalizerFunc func(string) string

func extractAnnotationListInternal(node *corev1.Node, key string, normalizer normalizerFunc) (values map[string]bool) {
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
		if normalizer != nil {
			item = normalizer(item)
		}

		values[item] = true
	}
	return
}
