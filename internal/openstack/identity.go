/*
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	"errors"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
)

// Identity is the authoritative hypervisor/service pair that Nova currently
// reports for a compute hostname.
type Identity struct {
	// HypervisorID is the UUID of the compute node record.
	HypervisorID string
	// ServiceID is the UUID of the nova-compute service the record belongs to.
	ServiceID string
	// ServiceState is the liveness of the compute service ("up" or "down").
	ServiceState string
	// ServiceStatus is the administrative status ("enabled" or "disabled").
	ServiceStatus string
}

var (
	// ErrHypervisorDown means records exist for the hostname, but none has a
	// live compute service. Their data belongs to a dead or replaced agent
	// and must not be adopted.
	ErrHypervisorDown = errors.New("no compute record with a live service for hostname")

	// ErrAmbiguousHypervisor means several records with live services exist
	// and the stored identity does not disambiguate them. Picking one
	// arbitrarily could act on the wrong compute service.
	ErrAmbiguousHypervisor = errors.New("multiple compute records with live services for hostname")
)

// SelectCurrentIdentity picks the authoritative hypervisor/service pair for a
// hostname from the exact-match candidates reported by Nova.
//
// A record whose compute service is not up is the leftover of a dead or
// replaced agent; adopting it stores an identity that goes stale the moment
// the replacement agent registers. Such records are never selected — not even
// when they are the only match, and not even when they match the stored
// identity. When several records are live, the stored identity is kept if it
// is among them (to avoid flapping between duplicates); otherwise the
// situation is ambiguous and is surfaced as an error instead of being
// guessed.
func SelectCurrentIdentity(matches []hypervisors.Hypervisor, storedServiceID string) (Identity, error) {
	if len(matches) == 0 {
		return Identity{}, ErrNoHypervisor
	}

	var live []hypervisors.Hypervisor
	for _, h := range matches {
		if h.State == "up" {
			live = append(live, h)
		}
	}

	if len(live) == 0 {
		return Identity{}, ErrHypervisorDown
	}

	if len(live) == 1 {
		return identityOf(live[0]), nil
	}

	// Several live records: keep the stored identity if it is among them, so
	// that a duplicated record does not cause flapping between reconciles.
	for _, h := range live {
		if h.Service.ID == storedServiceID {
			return identityOf(h), nil
		}
	}

	return Identity{}, ErrAmbiguousHypervisor
}

func identityOf(h hypervisors.Hypervisor) Identity {
	return Identity{
		HypervisorID:  h.ID,
		ServiceID:     h.Service.ID,
		ServiceState:  h.State,
		ServiceStatus: h.Status,
	}
}
