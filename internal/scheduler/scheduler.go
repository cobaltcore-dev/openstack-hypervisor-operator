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

package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
)

type Scheduler interface {
	EnsureHypervisorAvailabilityZone(ctx context.Context, hypervisorName, hypervisorAvailabilityZone string) error
	TryAcquireHypervisor(ctx context.Context, hypervisorName string) (*openstack.Hypervisor, error)
	UndoAcquireHypervisor(hypervisorName string)
	PollHypervisor(ctx context.Context, hypervisorName string, withServers bool) (*openstack.Hypervisor, error)
	DisableHypervisor(ctx context.Context, hypervisorName, reason string) error
	EnableHypervisor(ctx context.Context, hypervisorName, reason string) error
}

type scheduler struct {
	serviceClient              *gophercloud.ServiceClient
	hypervisorsMutex           sync.Mutex
	hypervisors                map[string]*openstack.Hypervisor
	acquiredHypervisors        map[string]bool
	hypervisorAvailabilityZone map[string]string
	hypervisorByClass          map[uint64]map[string]bool
}

func NewScheduler(ctx context.Context) (Scheduler, error) {
	var err error
	s := &scheduler{}

	if s.serviceClient, err = openstack.GetServiceClient(ctx, "compute"); err != nil {
		return nil, err
	}

	s.serviceClient.Microversion = "2.54" // To get cpu_info from the hypervisor

	s.hypervisors = make(map[string]*openstack.Hypervisor)
	s.acquiredHypervisors = make(map[string]bool)
	s.hypervisorAvailabilityZone = make(map[string]string)
	s.hypervisorByClass = make(map[uint64]map[string]bool)

	return s, nil
}

// Tries to get a hypervisor in the assumption no one else will modify things
// "too quickly". I.e. the data we have is reasonably up to date.
// This should be the first step.
func (s *scheduler) TryAcquireHypervisor(ctx context.Context, hypervisorName string) (*openstack.Hypervisor, error) {
	hypervisor, err := s.getHypervisor(ctx, hypervisorName)
	if err != nil {
		return nil, fmt.Errorf("failed to get hypervisor %w", err)
	}

	if hypervisor.Status == "disabled" {
		// Already disabled, so we might as well get on with it
		return hypervisor, nil
	} else if _, found := s.acquiredHypervisors[hypervisorName]; found {
		// Already acquired
		return hypervisor, nil
	}

	// getHypervisor might acquire mutex, so we can't do this before
	// but now this has to be done "atomically"
	s.hypervisorsMutex.Lock()
	defer s.hypervisorsMutex.Unlock()

	classId := hypervisor.GetHypervisorClassId()
	hypervisorsInClass := s.hypervisorByClass[classId]

	var freeRamInClass int64 = -hypervisor.MemoryMbUsed // That has to be moved out
	for name := range hypervisorsInClass {
		if name == hypervisorName {
			continue
		}
		hypervisor := s.hypervisors[name]
		_, isAcquired := s.acquiredHypervisors[name]
		if isAcquired || !isHypervisorAvailable(hypervisor) {
			// Being migrated out, so the RAM of those VMs has to go somewhere eventually
			freeRamInClass -= s.hypervisors[name].MemoryMbUsed
		} else {
			// And this is where it hopefully comes from
			freeRamInClass += s.hypervisors[name].FreeRAMMb
		}
	}

	// Will we have enough afterwards?
	if freeRamInClass > 0 {
		s.acquiredHypervisors[hypervisorName] = true
		return hypervisor, nil
	} else {
		logger.FromContext(ctx).Info("Not enough RAM free in class", "hypervisorName", hypervisorName)
		return nil, nil
	}
}

// Undoes any reservation done in the AcquireHypervisor
// The caller only needs the reservation for the time between having acquired it
// and having disabled it.
func (s *scheduler) UndoAcquireHypervisor(hypervisorName string) {
	s.hypervisorsMutex.Lock()
	defer s.hypervisorsMutex.Unlock()
	delete(s.acquiredHypervisors, hypervisorName)
}

func (s *scheduler) PollHypervisor(ctx context.Context, hypervisorName string, withServers bool) (*openstack.Hypervisor, error) {
	hypervisor, err := openstack.GetHypervisorByName(ctx, s.serviceClient, hypervisorName, withServers)
	if err != nil {
		return nil, err
	}

	s.hypervisorsMutex.Lock()
	defer s.hypervisorsMutex.Unlock()
	s.updateBookkeeping(hypervisor)

	return hypervisor, err
}

func (s *scheduler) DisableHypervisor(ctx context.Context, hypervisorName, reason string) error {
	hypervisor, err := s.getHypervisor(ctx, hypervisorName)
	if err != nil {
		return err
	}

	disableService := services.UpdateOpts{Status: services.ServiceDisabled,
		DisabledReason: reason}

	service, err := services.Update(ctx, s.serviceClient, hypervisor.Service.ID, disableService).Extract()
	if err != nil {
		return err
	}

	updateHypervisorFromService(hypervisor, service)

	return nil
}

func (s *scheduler) ensureHypervisorAvailabilityZone(hypervisorName, hypervisorAvailabilityZone string) bool {
	s.hypervisorsMutex.Lock()
	defer s.hypervisorsMutex.Unlock()
	if az, found := s.hypervisorAvailabilityZone[hypervisorName]; found && az == hypervisorAvailabilityZone {
		return true
	}

	s.hypervisorAvailabilityZone[hypervisorName] = hypervisorAvailabilityZone
	return false
}

func (s *scheduler) EnsureHypervisorAvailabilityZone(ctx context.Context, hypervisorName, hypervisorAvailabilityZone string) (err error) {
	if s.ensureHypervisorAvailabilityZone(hypervisorName, hypervisorAvailabilityZone) {
		return
	}

	_, err = s.PollHypervisor(ctx, hypervisorName, false)
	return
}

func (s *scheduler) EnableHypervisor(ctx context.Context, hypervisorName, reason string) error {
	hypervisor, err := s.getHypervisor(ctx, hypervisorName)
	if err != nil {
		return err
	}

	if hypervisor.Service.DisabledReason == "" {
		// Nothing to be done
		return nil
	} else if hypervisor.Service.DisabledReason != reason {
		log := logger.FromContext(ctx)
		err := fmt.Errorf("expected reason %q vs %q", reason, hypervisor.Service.DisabledReason)
		log.Error(err, "could not enable hypervisor")
		return nil
	}

	enableService := services.UpdateOpts{Status: services.ServiceEnabled}
	service, err := services.Update(ctx, s.serviceClient, hypervisor.Service.ID, enableService).Extract()

	if err != nil {
		return err
	}

	updateHypervisorFromService(hypervisor, service)
	return nil
}

func updateHypervisorFromService(hypervisor *openstack.Hypervisor, service *services.Service) {
	hypervisor.Service.DisabledReason = service.DisabledReason
	hypervisor.Service.Host = service.Host
	hypervisor.State = service.State
	hypervisor.Status = service.Status
}

func normalizeHypervisorName(hypervisor *openstack.Hypervisor) string {
	name, _, _ := strings.Cut(hypervisor.HypervisorHostname, ".")
	return name
}

func (s *scheduler) updateBookkeeping(hypervisor *openstack.Hypervisor) {
	hypervisorName := normalizeHypervisorName(hypervisor)
	classId := hypervisor.GetHypervisorClassId()

	// Remove the old stuff
	if oldRecord, oldRecordFound := s.hypervisors[hypervisorName]; oldRecordFound {
		oldClassId := oldRecord.GetHypervisorClassId()
		if oldClassId != classId {
			hypervisorsInClass := s.hypervisorByClass[oldClassId]
			delete(hypervisorsInClass, hypervisorName)
		}
	}

	// Store the new record under the name
	s.hypervisors[hypervisorName] = hypervisor
	hypervisorsInClass, found := s.hypervisorByClass[classId]
	if !found {
		hypervisorsInClass = make(map[string]bool)
		s.hypervisorByClass[classId] = hypervisorsInClass
	}
	hypervisorsInClass[hypervisorName] = true
}

func isHypervisorAvailable(hv *openstack.Hypervisor) bool {
	return hv.State == "up" && hv.Status == "enabled"
}

func (s *scheduler) getHypervisor(ctx context.Context, hypervisorName string) (*openstack.Hypervisor, error) {
	if hypervisor, found := s.hypervisors[hypervisorName]; found {
		return hypervisor, nil
	}

	return s.PollHypervisor(ctx, hypervisorName, false)
}
