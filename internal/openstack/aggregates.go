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

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

// ApplyAggregates ensures a host is in exactly the specified aggregates.
//
// The function performs a two-phase operation to maintain security:
// 1. Verifies that all desired aggregates exist
// 2. Adds the host to all aggregates it should be in but isn't already
// 3. Removes the host from any aggregates it shouldn't be in
//
// This ordering prevents leaving the host unprotected between operations when
// aggregates have filter criteria. However, conflicts may still occur with
// aggregates in different availability zones, in which case errors are collected
// and returned together for eventual convergence.
//
// All specified aggregates must already exist in OpenStack. If any desired
// aggregate is not found, an error is returned listing the missing aggregates.
//
// Pass an empty list to remove the host from all aggregates.
func ApplyAggregates(ctx context.Context, serviceClient *gophercloud.ServiceClient, host string, desiredAggregates []string) ([]kvmv1.Aggregate, error) {
	log := logger.FromContext(ctx)

	oldMicroVersion := serviceClient.Microversion
	serviceClient.Microversion = "2.93" // Something bigger than 2.41 for UUIDs
	defer func() {
		serviceClient.Microversion = oldMicroVersion
	}()

	// Fetch all aggregates
	pages, err := aggregates.List(serviceClient).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list aggregates: %w", err)
	}

	allAggregates, err := aggregates.ExtractAggregates(pages)
	if err != nil {
		return nil, fmt.Errorf("failed to extract aggregates: %w", err)
	}

	// Compare current aggregates of the host with the desired ones
	aggregateMap := make(map[string]*aggregates.Aggregate, len(allAggregates))
	var currentAggregates []string
	for i := range allAggregates {
		agg := &allAggregates[i]
		aggregateMap[agg.Name] = agg
		if slices.Contains(agg.Hosts, host) {
			currentAggregates = append(currentAggregates, agg.Name)
		}
	}

	toAdd := difference(currentAggregates, desiredAggregates)
	toRemove := difference(desiredAggregates, currentAggregates)

	// Verify all desired aggregates exist
	var missingAggregates []string
	for _, name := range desiredAggregates {
		if _, exists := aggregateMap[name]; !exists {
			missingAggregates = append(missingAggregates, name)
		}
	}
	if len(missingAggregates) > 0 {
		return nil, fmt.Errorf("aggregates not found: %v", missingAggregates)
	}

	// We need to add the host to aggregates first, because if we first drop
	// an aggregate with a filter criterion and then add a new one, we leave the host
	// open for a period of time. Still, this may fail due to a conflict of aggregates
	// with different availability zones, so we collect all the errors and return them
	// so it hopefully will converge eventually.
	var errs []error
	var result []kvmv1.Aggregate

	if len(toAdd) > 0 {
		log.Info("Adding to aggregates", "aggregates", toAdd, "host", host)
		for _, name := range toAdd {
			agg := aggregateMap[name]
			_, err := aggregates.AddHost(ctx, serviceClient, agg.ID, aggregates.AddHostOpts{Host: host}).Extract()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to add host %v to aggregate %v: %w", host, name, err))
			}
		}
	}

	if len(toRemove) > 0 {
		log.Info("Removing from aggregates", "aggregates", toRemove, "host", host)
		for _, name := range toRemove {
			agg := aggregateMap[name]
			_, err := aggregates.RemoveHost(ctx, serviceClient, agg.ID, aggregates.RemoveHostOpts{Host: host}).Extract()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to remove host %v from aggregate %v: %w", host, name, err))
			}
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Collect aggregates with names and UUIDs
	for _, name := range desiredAggregates {
		agg := aggregateMap[name] // exists as per "Verify all desired aggregates exist" check
		result = append(result, kvmv1.Aggregate{
			Name:     agg.Name,
			UUID:     agg.UUID,
			Metadata: agg.Metadata,
		})
	}

	return result, nil
}

// difference returns all elements in s2 that are not in s1
func difference(s1, s2 []string) []string {
	diff := make([]string, 0)
	for _, item := range s2 {
		if !slices.Contains(s1, item) {
			diff = append(diff, item)
		}
	}
	return diff
}
