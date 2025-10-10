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

package controller

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("AggregatesController", func() {
	var (
		tc         *AggregatesController
		fakeServer testhelper.FakeServer
	)
	const EOF = "EOF"
	const AggregateListBodyEmpty = `
{
    "aggregates": []
}
`

	const AggregateListBodyFull = `
{
    "aggregates": [
		{
			"name": "test-aggregate2",
			"availability_zone": "",
			"deleted": false,
			"id": 100001,
			"hosts": ["hv-test"]
		},
		{
			"name": "test-aggregate3",
			"availability_zone": "",
			"deleted": false,
			"id": 99,
			"hosts": ["hv-test"]
		}
    ]
}
`

	const AggregatesPostBody = `
{
    "aggregate": {
		"name": "test-aggregate1",
        "availability_zone": "",
        "deleted": false,
        "id": 42
    }
}`

	const AggregateRemoveHostBody = `
{
        "aggregate": {
			"name": "test-aggregate3",
			"availability_zone": "",
			"deleted": false,
			"id": 99
		}
}`

	const AggregateAddHostBody = `
{
    "aggregate": {
            "name": "test-aggregate1",
            "availability_zone": "",
            "deleted": false,
            "hosts": [
                "hv-test"
            ],
            "id": 42
        }
}`

	// Setup and teardown

	BeforeEach(func(ctx context.Context) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()

		By("Creating the AggregatesController")
		tc = &AggregatesController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}
	})

	AfterEach(func() {
		By("Tearing down the OpenStack http mock server")
		fakeServer.Teardown()

		By("Deleting the Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{}
		Expect(tc.Client.Get(ctx, types.NamespacedName{Name: "hv-test", Namespace: "default"}, hypervisor)).To(Succeed())
		Expect(tc.Client.Delete(ctx, hypervisor)).To(Succeed())
	})

	// Tests

	Context("Adding new Aggregate", func() {
		BeforeEach(func() {
			By("Creating a Hypervisor resource")
			hypervisor := &kvmv1.Hypervisor{
				ObjectMeta: v1.ObjectMeta{
					Name:      "hv-test",
					Namespace: "default",
				},
				Spec: kvmv1.HypervisorSpec{
					LifecycleEnabled: true,
					Aggregates:       []string{"test-aggregate1"},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			// Mock resourceproviders.GetAggregates
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err := fmt.Fprint(w, AggregateListBodyEmpty)
				Expect(err).NotTo(HaveOccurred())
			})
			fakeServer.Mux.HandleFunc("POST /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err := fmt.Fprint(w, AggregatesPostBody)
				Expect(err).NotTo(HaveOccurred())
			})

			// Mock resourceproviders.UpdateAggregates
			fakeServer.Mux.HandleFunc("POST /os-aggregates/42/action", func(w http.ResponseWriter, r *http.Request) {
				// parse request
				Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
				expectedBody := `{"add_host":{"host":"hv-test"}}`
				body := make([]byte, r.ContentLength)
				_, err := r.Body.Read(body)
				Expect(err == nil || err.Error() == EOF).To(BeTrue())
				Expect(string(body)).To(MatchJSON(expectedBody))

				// send response
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err = fmt.Fprint(w, AggregateAddHostBody)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should update Aggregates and set status condition when Aggregates differ", func() {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "hv-test", Namespace: "default"}}
			_, err := tc.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(tc.Client.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(ContainElements("test-aggregate1"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, ConditionTypeAggregatesUpdated)).To(BeTrue())
		})
	})

	Context("Removing Aggregate", func() {
		BeforeEach(func() {
			By("Creating a Hypervisor resource")
			hypervisor := &kvmv1.Hypervisor{
				ObjectMeta: v1.ObjectMeta{
					Name:      "hv-test",
					Namespace: "default",
				},
				Spec: kvmv1.HypervisorSpec{
					LifecycleEnabled: true,
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			// update status to have existing aggregate
			hypervisor.Status.Aggregates = []string{"test-aggregate2", "test-aggregate3"}
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			// Mock resourceproviders.GetAggregates
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err := fmt.Fprint(w, AggregateListBodyFull)
				Expect(err).NotTo(HaveOccurred())
			})
			expectRemoveHostFromAggregate := func(w http.ResponseWriter, r *http.Request) {
				// parse request
				Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
				expectedBody := `{"remove_host":{"host":"hv-test"}}`
				body := make([]byte, r.ContentLength)
				_, err := r.Body.Read(body)
				Expect(err == nil || err.Error() == EOF).To(BeTrue())
				Expect(string(body)).To(MatchJSON(expectedBody))

				// send response
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err = fmt.Fprint(w, AggregateRemoveHostBody)
				Expect(err).NotTo(HaveOccurred())
			}
			fakeServer.Mux.HandleFunc("POST /os-aggregates/100001/action", expectRemoveHostFromAggregate)
			fakeServer.Mux.HandleFunc("POST /os-aggregates/99/action", expectRemoveHostFromAggregate)
		})

		It("should update Aggregates and set status condition when Aggregates differ", func() {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "hv-test", Namespace: "default"}}
			_, err := tc.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(tc.Client.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, ConditionTypeAggregatesUpdated)).To(BeTrue())
		})
	})
})
