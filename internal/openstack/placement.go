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

	"github.com/gophercloud/gophercloud/v2"
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
	resp, err := client.Put(ctx, getResourceProviderTraitsURL(client, resourceProviderID), b, &r.Body, &gophercloud.RequestOpts{
		OkCodes: []int{200},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}
