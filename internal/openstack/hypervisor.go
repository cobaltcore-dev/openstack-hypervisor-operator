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
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	hypervisorsv2 "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
)

var _QUOTED = regexp.MustCompile(`"([^"]+)"`)
var _KV = regexp.MustCompile(`"([^"]+)":([0-9]+)`)

type topology struct {
	Cells   int `json:"cells"`
	Cores   int `json:"cores"`
	Sockets int `json:"sockets"`
	Threads int `json:"threads"`
}

func (t *topology) UnmarshalJSON(b []byte) (err error) {
	s := string(b)
	for _, match := range _KV.FindAllStringSubmatch(s, -1) {
		var i int
		i, err = strconv.Atoi(match[2])
		if err != nil {
			return
		}
		switch match[1] {
		case "cells":
			t.Cells = i
		case "cores":
			t.Cores = i
		case "sockets":
			t.Sockets = i
		case "threads":
			t.Threads = i
		}
	}
	return
}

type features []string

func (f *features) UnmarshalJSON(b []byte) error {
	s := string(b)
	for _, match := range _QUOTED.FindAllStringSubmatch(s, -1) {
		*f = append(*f, match[1])
	}
	slices.Sort(*f)
	return nil
}

type cpuInfo struct {
	Arch     string    `json:"arch"`
	Model    string    `json:"model"`
	Vendor   string    `json:"vendor"`
	Features features  `json:"features"` // Neither []string, nor *[]string worked
	Topology *topology `json:"topology"`
}

type hypervisorServer struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

type Hypervisor struct {
	CPUInfo            cpuInfo `json:"cpu_info"`
	CurrentWorkload    int     `json:"current_workload"`
	DiskAvailableLeast any     `json:"disk_available_least"`
	FreeDiskGb         int64   `json:"free_disk_gb"`
	FreeRAMMb          int64   `json:"free_ram_mb"`
	HostIP             string  `json:"host_ip"`
	HypervisorHostname string  `json:"hypervisor_hostname"`
	HypervisorType     string  `json:"hypervisor_type"`
	HypervisorVersion  int     `json:"hypervisor_version"`
	ID                 string  `json:"id"`
	LocalGb            int64   `json:"local_gb"`
	LocalGbUsed        int64   `json:"local_gb_used"`
	MemoryMb           int64   `json:"memory_mb"`
	MemoryMbUsed       int64   `json:"memory_mb_used"`
	RunningVms         int     `json:"running_vms"`
	Service            struct {
		DisabledReason string `json:"disabled_reason"`
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

// Gives a unique identifier for all hypervisors which should be compatible
// Missing is the availability-zone
func (h Hypervisor) GetHypervisorClassId() uint64 {
	hasher := fnv.New64()
	hasher.Write([]byte(h.HypervisorType))
	hasher.Write([]byte(h.CPUInfo.Model))
	/* That seems too specific. Need to figure out, which feature flags matter
	for _, feature := range h.CPUInfo.Features {
		hasher.Write([]byte(feature))
	}
	*/

	if h.CPUInfo.Topology != nil {
		bs := make([]byte, 4)
		binary.LittleEndian.PutUint32(bs, uint32(h.CPUInfo.Topology.Cells))
		hasher.Write(bs)
		binary.LittleEndian.PutUint32(bs, uint32(h.CPUInfo.Topology.Sockets))
		hasher.Write(bs)
		binary.LittleEndian.PutUint32(bs, uint32(h.CPUInfo.Topology.Cores))
		hasher.Write(bs)
		binary.LittleEndian.PutUint32(bs, uint32(h.CPUInfo.Topology.Threads))
		hasher.Write(bs)
	}

	return hasher.Sum64()
}

func GetHypervisorByName(ctx context.Context, sc *gophercloud.ServiceClient, hypervisorHostname string, withServers bool) (*Hypervisor, error) {
	listOpts := hypervisorsv2.ListOpts{
		HypervisorHostnamePattern: &hypervisorHostname,
		WithServers:               &withServers,
	}

	pages, err := hypervisorsv2.List(sc, listOpts).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	// due some(tm) bug, gohperclouds hypervisors.ExtractPage is failing
	h := &HyperVisorsDetails{}
	if err = (pages.(hypervisorsv2.HypervisorPage)).ExtractInto(h); err != nil {
		return nil, err
	}

	if len(h.Hypervisors) == 0 {
		return nil, errors.New("no hypervisor found")
	} else if len(h.Hypervisors) > 1 {
		return nil, errors.New("multiple hypervisors found")
	}

	for _, hypervisor := range h.Hypervisors {
		if strings.HasPrefix(hypervisor.HypervisorHostname, hypervisorHostname) {
			return &hypervisor, nil
		}
	}

	return nil, fmt.Errorf("could not find exact match")
}

// GetHypervisors returns a list of hypervisors that are candidates for migration.
func GetHypervisors(ctx context.Context, sc *gophercloud.ServiceClient) (hypervisors []Hypervisor, err error) {
	pages, err := hypervisorsv2.List(sc, hypervisorsv2.ListOpts{}).AllPages(ctx)

	if err != nil {
		return nil, err
	}

	// due some(tm) bug, gopherclouds hypervisors.ExtractPage is failing
	var h HyperVisorsDetails
	if err = (pages.(hypervisorsv2.HypervisorPage)).ExtractInto(&h); err != nil {
		return nil, err
	}

	for _, hv := range h.Hypervisors {
		if hv.HypervisorType != "QEMU" {
			continue
		}

		hypervisors = append(hypervisors, hv)
	}

	return
}

func GetHypervisorById(ctx context.Context, sc *gophercloud.ServiceClient, hypervisorID string) (hypervisor *Hypervisor, err error) {
	hypervisor = &Hypervisor{}
	err = hypervisorsv2.Get(ctx, sc, hypervisorID).ExtractInto(hypervisor)
	return
}
