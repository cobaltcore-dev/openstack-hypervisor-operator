/*
SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
SPDX-FileCopyrightText: Copyright Gophercloud authors
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

/* Temporary workaround until gophercloud v3 has been released.
 * Functions and structs copied from:
 * https://github.com/gophercloud/gophercloud/tree/main/openstack/compute/v2/services
 * and renamed to contain "Service".
 */

package openstack

import (
	"context"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
)

type UpdateServiceOpts struct {
	// Status represents the new service status. One of enabled or disabled.
	Status services.ServiceStatus `json:"status,omitempty"`

	// DisabledReason represents the reason for disabling a service.
	DisabledReason string `json:"disabled_reason,omitempty"`

	// ForcedDown is a manual override to tell nova that the service in question
	// has been fenced manually by the operations team.
	ForcedDown *bool `json:"forced_down,omitempty"`
}

// ToServiceUpdateMap formats an UpdateServiceOpts structure into a request body.
func (opts UpdateServiceOpts) ToServiceUpdateMap() (map[string]any, error) {
	return gophercloud.BuildRequestBody(opts, "")
}

func updateServiceURL(c *gophercloud.ServiceClient, id string) string {
	return c.ServiceURL("os-services", id)
}

// UpdateService requests that various attributes of the indicated service be changed.
func UpdateService(ctx context.Context, client *gophercloud.ServiceClient, id string, opts UpdateServiceOpts) (r services.UpdateResult) {
	b, err := opts.ToServiceUpdateMap()
	if err != nil {
		r.Err = err
		return
	}
	resp, err := client.Put(ctx, updateServiceURL(client, id), b, &r.Body, &gophercloud.RequestOpts{
		OkCodes: []int{200},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}
