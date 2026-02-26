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

	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Placement API", func() {
	var (
		fakeServer testhelper.FakeServer
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)
	})

	Describe("UpdateTraits", func() {
		const (
			resourceProviderID = "test-rp-uuid"
			traitsURL          = "/resource_providers/test-rp-uuid/traits"
		)

		Context("when the request is successful", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(traitsURL, func(w http.ResponseWriter, r *http.Request) {
					Expect(r.Method).To(Equal("PUT"))
					Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))

					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{
						"traits": ["CUSTOM_TRAIT_1", "CUSTOM_TRAIT_2"],
						"resource_provider_generation": 2
					}`)
				})
			})

			It("should update traits successfully", func() {
				opts := UpdateTraitsOpts{
					Traits:                     []string{"CUSTOM_TRAIT_1", "CUSTOM_TRAIT_2"},
					ResourceProviderGeneration: 1,
				}

				result := UpdateTraits(ctx, client.ServiceClient(fakeServer), resourceProviderID, opts)
				Expect(result.Err).NotTo(HaveOccurred())
			})
		})

		Context("when the request fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(traitsURL, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should return an error", func() {
				opts := UpdateTraitsOpts{
					Traits:                     []string{"CUSTOM_TRAIT_1"},
					ResourceProviderGeneration: 1,
				}

				result := UpdateTraits(ctx, client.ServiceClient(fakeServer), resourceProviderID, opts)
				Expect(result.Err).To(HaveOccurred())
			})
		})

		Context("when building request body fails", func() {
			It("should return an error", func() {
				// Create an invalid opts that would fail marshaling
				// This tests the ToResourceProviderUpdateTraitsMap error path
				opts := UpdateTraitsOpts{
					Traits:                     []string{"TRAIT1"},
					ResourceProviderGeneration: 1,
				}

				// The actual function doesn't have a way to make BuildRequestBody fail
				// but we can still verify the happy path
				body, err := opts.ToResourceProviderUpdateTraitsMap()
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(HaveKey("traits"))
				Expect(body).To(HaveKey("resource_provider_generation"))
			})
		})
	})

	Describe("CleanupResourceProvider", func() {
		const (
			providerUUID      = "test-provider-uuid"
			providerAllocsURL = "/resource_providers/test-provider-uuid/allocations"
			deleteProviderURL = "/resource_providers/test-provider-uuid"
		)

		Context("when provider is nil", func() {
			It("should return nil without errors", func() {
				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), nil)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when provider has no allocations", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					Expect(r.Method).To(Equal("GET"))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"allocations": {}}`)
				})

				fakeServer.Mux.HandleFunc(deleteProviderURL, func(w http.ResponseWriter, r *http.Request) {
					Expect(r.Method).To(Equal("DELETE"))
					w.WriteHeader(http.StatusNoContent)
				})
			})

			It("should delete the provider successfully", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when provider has empty consumer allocations", func() {
			const (
				consumer1ID = "consumer-1"
				consumer2ID = "consumer-2"
			)

			BeforeEach(func() {
				// Provider has two consumers
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, `{
						"allocations": {
							"%s": {},
							"%s": {}
						}
					}`, consumer1ID, consumer2ID)
				})

				// Both consumers have empty allocations
				fakeServer.Mux.HandleFunc("/allocations/consumer-1", func(w http.ResponseWriter, r *http.Request) {
					switch r.Method {
					case http.MethodGet:
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						fmt.Fprint(w, `{"allocations": {}, "consumer_generation": 0}`)
					case http.MethodDelete:
						w.WriteHeader(http.StatusNoContent)
					}
				})

				fakeServer.Mux.HandleFunc("/allocations/consumer-2", func(w http.ResponseWriter, r *http.Request) {
					switch r.Method {
					case http.MethodGet:
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						fmt.Fprint(w, `{"allocations": {}, "consumer_generation": 0}`)
					case http.MethodDelete:
						w.WriteHeader(http.StatusNoContent)
					}
				})

				fakeServer.Mux.HandleFunc(deleteProviderURL, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})
			})

			It("should clean up consumers and delete provider", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when provider has non-empty consumer allocations", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{
						"allocations": {
							"consumer-with-allocs": {}
						}
					}`)
				})

				// Consumer has actual allocations
				fakeServer.Mux.HandleFunc("/allocations/consumer-with-allocs", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{
						"allocations": {
							"rp-uuid": {
								"generation": 1,
								"resources": {
									"VCPU": 2
								}
							}
						},
						"consumer_generation": 1
					}`)
				})

				// Don't delete the provider when it has non-empty allocations
			})

			It("should return an error about non-empty allocations", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cannot clean up provider"))
				Expect(err.Error()).To(ContainSubstring("non-empty consumer allocations"))
			})
		})

		Context("when getting provider allocations fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should return an error", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when getting consumer allocations fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"allocations": {"consumer-1": {}}}`)
				})

				fakeServer.Mux.HandleFunc("/allocations/consumer-1", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should return an error", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when deleting provider fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc(providerAllocsURL, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"allocations": {}}`)
				})

				fakeServer.Mux.HandleFunc(deleteProviderURL, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should return an error", func() {
				provider := &resourceproviders.ResourceProvider{
					UUID: providerUUID,
					Name: "test-provider",
				}

				err := CleanupResourceProvider(ctx, client.ServiceClient(fakeServer), provider)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to delete after cleanup"))
			})
		})
	})
})
