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
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
)

// UpdateTraitsResult is the response of a Put traits operations. Call its Extract method
// to interpret it as a ResourceProviderTraits.
type UpdateTraitsResult struct {
	gophercloud.Result
}

// UpdateTraitsOptsBuilder allows extensions to add additional parameters to the
// UpdateTraits request.
type UpdateTraitsOptsBuilder interface {
	ToResourceProviderUpdateTraitsMap() (map[string]any, error)
}

// UpdateTraitsOpts represents options used to create a resource provider.
type UpdateTraitsOpts struct {
	Traits                     []string `json:"traits"`
	ResourceProviderGeneration int      `json:"resource_provider_generation"`
}

// ToResourceProviderUpdateTraitsMap constructs a request body from UpdateTraitsOpts.
func (opts UpdateTraitsOpts) ToResourceProviderUpdateTraitsMap() (map[string]any, error) {
	b, err := gophercloud.BuildRequestBody(opts, "")
	if err != nil {
		return nil, err
	}

	return b, nil
}

func getResourceProviderTraitsURL(client *gophercloud.ServiceClient, resourceProviderID string) string {
	return client.ServiceURL("resource_providers", resourceProviderID, "traits")
}

func UpdateTraits(ctx context.Context, client *gophercloud.ServiceClient, resourceProviderID string, opts UpdateTraitsOptsBuilder) (r UpdateTraitsResult) {
	b, err := opts.ToResourceProviderUpdateTraitsMap()
	if err != nil {
		r.Err = err
		return
	}
	resp, err := client.Put(ctx, getResourceProviderTraitsURL(client, resourceProviderID), b, &r.Body, &gophercloud.RequestOpts{ // nolint:bodyclose
		OkCodes: []int{200},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

func getAllocationsURL(client *gophercloud.ServiceClient, consumerID string) string {
	return client.ServiceURL("allocations", consumerID)
}

// ListAllocationsResult is the response of a Get allocations operations. Call its Extract method
// to interpret it as a Allocations.
type ListAllocationsResult struct {
	gophercloud.Result
}

type ConsumerAllocations struct {
	Allocations        map[string]resourceproviders.Allocation `json:"allocations"`
	ConsumerGeneration int                                     `json:"consumer_generation"`
	ProjectID          string                                  `json:"project_id"`
	UserID             string                                  `json:"user_id"`
	ConsumerType       string                                  `json:"consumer_type"`
}

// Extract interprets a ListAllocationsResult as a Allocations.
func (r ListAllocationsResult) Extract() (*ConsumerAllocations, error) {
	var s ConsumerAllocations
	err := r.ExtractInto(&s)
	return &s, err
}

// List Allocations for a certain consumer
func ListAllocations(ctx context.Context, client *gophercloud.ServiceClient, consumerID string) (r ListAllocationsResult) {
	resp, err := client.Get(ctx, getAllocationsURL(client, consumerID), nil, &gophercloud.RequestOpts{ // nolint:bodyclose
		OkCodes: []int{200},
	})
	if err != nil {
		r.Err = err
		return
	}

	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

// Delete all Allocations for a certain consumer
func DeleteConsumerAllocations(ctx context.Context, client *gophercloud.ServiceClient, consumerID string) (r ListAllocationsResult) {
	resp, err := client.Delete(ctx, getAllocationsURL(client, consumerID), &gophercloud.RequestOpts{ // nolint:bodyclose
		OkCodes: []int{204, 404},
	})
	if err != nil {
		r.Err = err
		return
	}

	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

// Remove all empty Allocations for a certain provider, and if it is empty, delete it
func CleanupResourceProvider(ctx context.Context, client *gophercloud.ServiceClient, provider *resourceproviders.ResourceProvider) error {
	if provider == nil {
		return nil
	}

	providerAllocations, err := resourceproviders.GetAllocations(ctx, client, provider.UUID).Extract()
	if err != nil {
		return err
	}

	// It is a map of consumer-ids to their alloctions, we just go over their ids
	// to cross-check, what is stored for the consumer itself
	for consumerID := range providerAllocations.Allocations {
		// Allocations of the consumer mapped by the resource provider, so the
		// "reverse" of what we got before
		result := ListAllocations(ctx, client, consumerID)
		consumerAllocations, err := result.Extract()
		if err != nil {
			return err
		}

		if len(consumerAllocations.Allocations) > 0 {
			return fmt.Errorf("cannot clean up provider, cannot handle non-empty consumer allocations")
		}

		// The consumer actually doesn't have *any* allocations, so it is just
		// inconsistent, and we can drop them all
		DeleteConsumerAllocations(ctx, client, consumerID)
	}

	// We are done, let's clean it up
	err = resourceproviders.Delete(ctx, client, provider.UUID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to delete after cleanup due to %w", err)
	}

	return nil
}
