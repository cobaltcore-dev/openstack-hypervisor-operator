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

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
)

type hypervisorServer struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

type Hypervisor struct {
	CPUInfo            string `json:"cpu_info"`
	CurrentWorkload    int    `json:"current_workload"`
	DiskAvailableLeast any    `json:"disk_available_least"`
	FreeDiskGb         int    `json:"free_disk_gb"`
	FreeRAMMb          int    `json:"free_ram_mb"`
	HostIP             string `json:"host_ip"`
	HypervisorHostname string `json:"hypervisor_hostname"`
	HypervisorType     string `json:"hypervisor_type"`
	HypervisorVersion  int    `json:"hypervisor_version"`
	ID                 string `json:"id"`
	LocalGb            int    `json:"local_gb"`
	LocalGbUsed        int    `json:"local_gb_used"`
	MemoryMb           int    `json:"memory_mb"`
	MemoryMbUsed       int    `json:"memory_mb_used"`
	RunningVms         int    `json:"running_vms"`
	Service            struct {
		DisabledReason any    `json:"disabled_reason"`
		Host           string `json:"host"`
		ID             string `json:"id"`
	} `json:"service"`
	State     string              `json:"state"`
	Status    string              `json:"status"`
	Vcpus     int                 `json:"vcpus"`
	VcpusUsed int                 `json:"vcpus_used"`
	Servers   *[]hypervisorServer `json:"servers"`
}
type HyperVisorsDetails struct {
	Hypervisors []Hypervisor `json:"hypervisors"`
}

func GetHypervisorByName(ctx context.Context, sc *gophercloud.ServiceClient, hypervisorHostnamePattern string, withServers bool) (*Hypervisor, error) {
	listOpts := hypervisors.ListOpts{
		HypervisorHostnamePattern: &hypervisorHostnamePattern,
		WithServers:               &withServers,
	}

	pages, err := hypervisors.List(sc, listOpts).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	// due some(tm) bug, gohperclouds hypervisors.ExtractPage is failing
	h := &HyperVisorsDetails{}
	if err = (pages.(hypervisors.HypervisorPage)).ExtractInto(h); err != nil {
		return nil, err
	}

	if len(h.Hypervisors) == 0 {
		return nil, errors.New("no hypervisor found")
	} else if len(h.Hypervisors) > 1 {
		return nil, errors.New("multiple hypervisors found")
	}

	return &h.Hypervisors[0], nil
}
