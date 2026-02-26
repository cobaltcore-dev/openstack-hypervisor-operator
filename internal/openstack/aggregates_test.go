/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ApplyAggregates", func() {
	const (
		aggregateListWithHost = `{
			"aggregates": [
				{
					"name": "agg1",
					"availability_zone": "az1",
					"deleted": false,
					"id": 1,
					"uuid": "uuid-agg1",
					"hosts": ["test-host"]
				},
				{
					"name": "agg2",
					"availability_zone": "az2",
					"deleted": false,
					"id": 2,
					"uuid": "uuid-agg2",
					"hosts": ["test-host"]
				},
				{
					"name": "agg3",
					"availability_zone": "az3",
					"deleted": false,
					"id": 3,
					"uuid": "uuid-agg3",
					"hosts": []
				}
			]
		}`

		aggregateAddHostResponse = `{
			"aggregate": {
				"name": "agg3",
				"availability_zone": "az3",
				"deleted": false,
				"id": 3,
				"uuid": "uuid-agg3",
				"hosts": ["test-host"]
			}
		}`

		aggregateRemoveHostResponse = `{
			"aggregate": {
				"name": "agg1",
				"availability_zone": "az1",
				"deleted": false,
				"id": 1,
				"uuid": "uuid-agg1",
				"hosts": []
			}
		}`
	)

	var (
		fakeServer testhelper.FakeServer
		ctx        context.Context
	)

	BeforeEach(func() {
		fakeServer = testhelper.SetupHTTP()
		ctx = context.Background()
	})

	AfterEach(func() {
		fakeServer.Teardown()
	})

	Context("when adding host to new aggregate", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/3/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateAddHostResponse)
			})
		})

		It("should add host to agg3", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg1", "agg2", "agg3"})
			Expect(err).NotTo(HaveOccurred())
			Expect(uuids).To(ConsistOf("uuid-agg1", "uuid-agg2", "uuid-agg3"))
		})
	})

	Context("when removing host from aggregate", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/1/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateRemoveHostResponse)
			})
		})

		It("should remove host from agg1", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(uuids).To(ConsistOf("uuid-agg2"))
		})
	})

	Context("when removing host from all aggregates", func() {
		var removeCalls int

		BeforeEach(func() {
			removeCalls = 0

			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/1/action", func(w http.ResponseWriter, r *http.Request) {
				removeCalls++
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateRemoveHostResponse)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/2/action", func(w http.ResponseWriter, r *http.Request) {
				removeCalls++
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"aggregate": {"name": "agg2", "id": 2, "uuid": "uuid-agg2", "hosts": []}}`)
			})
		})

		It("should remove host from all aggregates", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(removeCalls).To(Equal(2))
			Expect(uuids).To(BeEmpty())
		})
	})

	Context("when host already in desired aggregates", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})
		})

		It("should not make any changes", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg1", "agg2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(uuids).To(ConsistOf("uuid-agg1", "uuid-agg2"))
		})
	})

	Context("when adding and removing simultaneously", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/3/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateAddHostResponse)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/1/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateRemoveHostResponse)
			})
		})

		It("should replace agg1 with agg3", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg2", "agg3"})
			Expect(err).NotTo(HaveOccurred())
			Expect(uuids).To(ConsistOf("uuid-agg2", "uuid-agg3"))
		})
	})

	Context("when listing aggregates fails", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error": "Internal Server Error"}`)
			})
		})

		It("should return an error", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg1"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list aggregates"))
		})
	})

	Context("when adding host fails", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/3/action", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot add host", "code": 409}}`)
			})
		})

		It("should return an error", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg1", "agg2", "agg3"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add host"))
		})
	})

	Context("when removing host fails", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/1/action", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot remove host", "code": 409}}`)
			})
		})

		It("should return an error", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg2"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to remove host"))
		})
	})

	Context("when host not in any aggregates", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/3/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateAddHostResponse)
			})
		})

		It("should add new host to aggregate", func() {
			serviceClient := client.ServiceClient(fakeServer)
			uuids, err := ApplyAggregates(ctx, serviceClient, "new-host", []string{"agg3"})
			Expect(err).NotTo(HaveOccurred())
			Expect(uuids).To(ConsistOf("uuid-agg3"))
		})
	})

	Context("when both adding and removing fail", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})

			// Add to agg3 fails
			fakeServer.Mux.HandleFunc("POST /os-aggregates/3/action", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot add host", "code": 409}}`)
			})

			// Remove from agg1 fails
			fakeServer.Mux.HandleFunc("POST /os-aggregates/1/action", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot remove host", "code": 409}}`)
			})
		})

		It("should return combined errors", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg2", "agg3"})
			Expect(err).To(HaveOccurred())
			// Verify it's a joined error with multiple failures
			Expect(err.Error()).To(And(ContainSubstring("failed to add host"), ContainSubstring("failed to remove host")))
		})
	})

	Context("when desired aggregate does not exist", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})
		})

		It("should return an error about missing aggregate", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg1", "agg2", "agg4"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("aggregates not found"))
			Expect(err.Error()).To(ContainSubstring("agg4"))
		})
	})

	Context("when multiple desired aggregates do not exist", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, aggregateListWithHost)
			})
		})

		It("should return an error listing all missing aggregates", func() {
			serviceClient := client.ServiceClient(fakeServer)
			_, err := ApplyAggregates(ctx, serviceClient, "test-host", []string{"agg4", "agg5", "agg6"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("aggregates not found"))
			// Check that all missing aggregates are mentioned in the error
			Expect(err.Error()).To(ContainSubstring("agg4"))
			Expect(err.Error()).To(ContainSubstring("agg5"))
			Expect(err.Error()).To(ContainSubstring("agg6"))
		})
	})
})
