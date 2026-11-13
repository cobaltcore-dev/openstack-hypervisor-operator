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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// hvRecord builds a minimal compute-node record as returned by
// GET /os-hypervisors/detail.
func hvRecord(hypervisorID, serviceID, state, status string) hypervisors.Hypervisor {
	return hypervisors.Hypervisor{
		ID:                 hypervisorID,
		HypervisorHostname: "node010-bb631",
		State:              state,
		Status:             status,
		Service:            hypervisors.Service{ID: serviceID, Host: "node010-bb631"},
		HypervisorVersion:  2002000,
		MemoryMB:           8192,
		VCPUs:              4,
	}
}

var _ = Describe("SelectCurrentIdentity", func() {
	DescribeTable("selection rules",
		func(matches []hypervisors.Hypervisor, storedServiceID string, expected Identity, expectedErr error) {
			identity, err := SelectCurrentIdentity(matches, storedServiceID)
			if expectedErr != nil {
				Expect(err).To(MatchError(expectedErr))
				Expect(identity).To(Equal(Identity{}))
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(identity).To(Equal(expected))
			}
		},
		Entry("no records at all",
			nil, "stored",
			Identity{}, ErrNoHypervisor),
		Entry("single live record is selected",
			[]hypervisors.Hypervisor{hvRecord("hv-1", "svc-1", "up", "enabled")},
			"",
			Identity{HypervisorID: "hv-1", ServiceID: "svc-1", ServiceState: "up", ServiceStatus: "enabled"}, nil),
		Entry("single down record is never adopted, even as the only match",
			[]hypervisors.Hypervisor{hvRecord("hv-old", "svc-old", "down", "enabled")},
			"",
			Identity{}, ErrHypervisorDown),
		Entry("stored identity pointing at a down record is not kept",
			[]hypervisors.Hypervisor{hvRecord("hv-old", "svc-old", "down", "enabled")},
			"svc-old",
			Identity{}, ErrHypervisorDown),
		Entry("live replacement is selected over the stored down record",
			[]hypervisors.Hypervisor{
				hvRecord("hv-old", "svc-old", "down", "enabled"),
				hvRecord("hv-new", "svc-new", "up", "disabled"),
			},
			"svc-old",
			Identity{HypervisorID: "hv-new", ServiceID: "svc-new", ServiceState: "up", ServiceStatus: "disabled"}, nil),
		Entry("stored live identity is kept when duplicates exist",
			[]hypervisors.Hypervisor{
				hvRecord("hv-a", "svc-a", "up", "enabled"),
				hvRecord("hv-b", "svc-b", "up", "enabled"),
			},
			"svc-b",
			Identity{HypervisorID: "hv-b", ServiceID: "svc-b", ServiceState: "up", ServiceStatus: "enabled"}, nil),
		Entry("live duplicates without a stored match are ambiguous",
			[]hypervisors.Hypervisor{
				hvRecord("hv-a", "svc-a", "up", "enabled"),
				hvRecord("hv-b", "svc-b", "up", "enabled"),
			},
			"",
			Identity{}, ErrAmbiguousHypervisor),
		Entry("live duplicates with a stored match pointing elsewhere are ambiguous",
			[]hypervisors.Hypervisor{
				hvRecord("hv-a", "svc-a", "up", "enabled"),
				hvRecord("hv-b", "svc-b", "up", "enabled"),
			},
			"svc-gone",
			Identity{}, ErrAmbiguousHypervisor),
	)

	It("reports the identity with the service disabled (disable_new_services)", func() {
		// A freshly registered compute service starts disabled when Nova runs
		// with compute/disable_new_services=true; the record must still be
		// selected so onboarding can enable it.
		identity, err := SelectCurrentIdentity(
			[]hypervisors.Hypervisor{hvRecord("hv-1", "svc-1", "up", "disabled")}, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.ServiceStatus).To(Equal("disabled"))
	})

	It("uses errors.Is for sentinel detection", func() {
		_, err := SelectCurrentIdentity(
			[]hypervisors.Hypervisor{hvRecord("hv-old", "svc-old", "down", "enabled")}, "")
		Expect(errors.Is(err, ErrHypervisorDown)).To(BeTrue())
	})
})
