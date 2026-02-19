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

package openstack

import (
	"context"
	"errors"
	"fmt"
	"slices"

	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
)

// GetAggregatesByName retrieves all aggregates from nova and returns them as a map keyed by name.
func GetAggregatesByName(ctx context.Context, serviceClient *gophercloud.ServiceClient) (map[string]*aggregates.Aggregate, error) {
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

// AddToAggregate adds the given host to the named aggregate, creating the aggregate if it does not yet exist.
func AddToAggregate(ctx context.Context, serviceClient *gophercloud.ServiceClient, aggs map[string]*aggregates.Aggregate, host, name, zone string) (err error) {
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

// RemoveFromAggregate removes the given host from the named aggregate.
func RemoveFromAggregate(ctx context.Context, serviceClient *gophercloud.ServiceClient, aggs map[string]*aggregates.Aggregate, host, name string) error {
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

// ApplyAggregates ensures a host is in exactly the specified aggregates.
// It adds the host to missing aggregates and removes it from extra ones.
// Pass an empty list to remove the host from all aggregates.
// Aggregates must already exist - this function will not create them.
func ApplyAggregates(ctx context.Context, serviceClient *gophercloud.ServiceClient, host string, desiredAggregates []string) error {
	log := logger.FromContext(ctx)

	aggs, err := GetAggregatesByName(ctx, serviceClient)
	if err != nil {
		return fmt.Errorf("failed to get aggregates: %w", err)
	}

	// Build desired set for O(1) lookups
	desiredSet := make(map[string]bool, len(desiredAggregates))
	for _, name := range desiredAggregates {
		desiredSet[name] = true
	}

	// We need to add the host to aggregates first, because if we first drop
	// an aggregate with a filter criterion and then add a new one, we leave the host
	// open for a period of time. Still, this may fail due to a conflict of aggregates
	// with different availability zones, so we collect all the errors and return them
	// so it hopefully will converge eventually.
	var errs []error
	var toRemove []string

	// First, add to any desired aggregates (including creating them if needed)
	for _, name := range desiredAggregates {
		aggregate, exists := aggs[name]
		if !exists || !slices.Contains(aggregate.Hosts, host) {
			// Aggregate doesn't exist or host not in it - add immediately
			log.Info("Adding to aggregate", "aggregate", name, "host", host)
			if err := AddToAggregate(ctx, serviceClient, aggs, host, name, ""); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Second, collect aggregates to remove from (host is in but shouldn't be)
	for name, aggregate := range aggs {
		if slices.Contains(aggregate.Hosts, host) && !desiredSet[name] {
			toRemove = append(toRemove, name)
		}
	}

	// Remove after all additions are complete
	if len(toRemove) > 0 {
		log.Info("Removing from aggregates", "aggregates", toRemove, "host", host)
		for _, name := range toRemove {
			if err := RemoveFromAggregate(ctx, serviceClient, aggs, host, name); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("encountered errors during aggregate update: %w", errors.Join(errs...))
	}

	return nil
}
