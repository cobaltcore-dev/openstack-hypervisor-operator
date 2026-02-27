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

// ApplyAggregates ensures a host is in exactly the specified aggregates.
//
// The function performs a two-phase operation to maintain security:
// 1. First, adds the host to all desired aggregates (if not already present)
// 2. Then, removes the host from any aggregates it shouldn't be in
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
func ApplyAggregates(ctx context.Context, serviceClient *gophercloud.ServiceClient, host string, desiredAggregates []string) ([]string, error) {
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

	// Build desired set for lookups
	desiredSet := make(map[string]bool, len(desiredAggregates))
	for _, name := range desiredAggregates {
		desiredSet[name] = true
	}

	var uuids []string
	var errs []error
	var toRemove []aggregates.Aggregate

	// Single pass: handle adds immediately, collect removes for later
	for _, agg := range allAggregates {
		hostInAggregate := slices.Contains(agg.Hosts, host)
		aggregateDesired := desiredSet[agg.Name]

		if aggregateDesired {
			// Mark as found
			delete(desiredSet, agg.Name)
			uuids = append(uuids, agg.UUID)

			if !hostInAggregate {
				// Add host to this aggregate
				log.Info("Adding to aggregate", "aggregate", agg.Name, "host", host)
				_, err := aggregates.AddHost(ctx, serviceClient, agg.ID, aggregates.AddHostOpts{Host: host}).Extract()
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to add host %v to aggregate %v: %w", host, agg.Name, err))
				}
			}
		} else if hostInAggregate {
			// Collect for removal (after all adds complete)
			toRemove = append(toRemove, agg)
		}
	}

	// Error if any desired aggregates don't exist
	if len(desiredSet) > 0 {
		var missing []string
		for name := range desiredSet {
			missing = append(missing, name)
		}
		errs = append(errs, fmt.Errorf("aggregates not found: %v", missing))
	}

	// Remove host from unwanted aggregates (after all adds complete)
	if len(toRemove) > 0 {
		for _, agg := range toRemove {
			log.Info("Removing from aggregate", "aggregate", agg.Name, "host", host)
			_, err := aggregates.RemoveHost(ctx, serviceClient, agg.ID, aggregates.RemoveHostOpts{Host: host}).Extract()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to remove host %v from aggregate %v: %w", host, agg.Name, err))
			}
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	} else {
		return uuids, nil
	}
}
