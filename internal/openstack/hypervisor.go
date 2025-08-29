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
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
)

type HyperVisorsDetails struct {
	Hypervisors []hypervisors.Hypervisor `json:"hypervisors"`
}

var ErrNoHypervisor = fmt.Errorf("no hypervisor found")
var ErrMultipleHypervisors = fmt.Errorf("multiple hypervisors found")

func GetHypervisorByName(ctx context.Context, sc *gophercloud.ServiceClient, hypervisorHostnamePattern string, withServers bool) (*hypervisors.Hypervisor, error) {
	listOpts := hypervisors.ListOpts{
		HypervisorHostnamePattern: &hypervisorHostnamePattern,
		WithServers:               &withServers,
	}

	pages, err := hypervisors.List(sc, listOpts).AllPages(ctx)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil, ErrNoHypervisor
		}
		return nil, err
	}

	// due some(tm) bug, gopherclouds hypervisors.ExtractPage is failing
	h := &HyperVisorsDetails{}
	if err = (pages.(hypervisors.HypervisorPage)).ExtractInto(h); err != nil {
		return nil, err
	}

	if len(h.Hypervisors) == 0 {
		return nil, ErrNoHypervisor
	} else if len(h.Hypervisors) > 1 {
		return nil, ErrMultipleHypervisors
	}

	return &h.Hypervisors[0], nil
}
