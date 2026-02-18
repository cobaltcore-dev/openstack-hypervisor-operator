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
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("AggregatesController", func() {
	const (
		EOF                    = "EOF"
		AggregateListBodyEmpty = `
{
    "aggregates": []
}
`

		AggregateListBodyFull = `
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

		AggregatesPostBody = `
{
    "aggregate": {
		"name": "test-aggregate1",
        "availability_zone": "",
        "deleted": false,
        "id": 42
    }
}`

		AggregateRemoveHostBody = `
{
        "aggregate": {
			"name": "test-aggregate3",
			"availability_zone": "",
			"deleted": false,
			"id": 99
		}
}`

		AggregateAddHostBody = `
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
	)

	var (
		aggregatesController *AggregatesController
		fakeServer           testhelper.FakeServer
		hypervisorName       = types.NamespacedName{Name: "hv-test"}
	)

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Install default handler to fail unhandled requests
		fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
		})

		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName.Name,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})

		Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionFalse,
			Reason:  "dontcare",
			Message: "dontcare",
		})
		Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

		By("Creating the AggregatesController")
		aggregatesController = &AggregatesController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}
	})

	JustBeforeEach(func(ctx SpecContext) {
		// This will be overridden in error test cases
	})

	// Tests
	Context("Adding new Aggregate", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Setting a missing aggregate")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Aggregates = []string{"test-aggregate1"}
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

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

		It("should update Aggregates and set status condition as Aggregates differ", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(ContainElements("test-aggregate1"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
		})
	})

	Context("Removing Aggregate", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
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

		It("should update Aggregates and set status condition when Aggregates differ", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
		})
	})

	Context("before onboarding", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			// Remove the onboarding condition
			hypervisor.Status.Conditions = []metav1.Condition{}
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should neither update Aggregates and nor set status condition", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeFalse())
		})
	})

	Context("when terminating", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			// Remove the onboarding condition
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeTerminating,
				Status:  metav1.ConditionTrue,
				Reason:  "dontcare",
				Message: "dontcare",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should neither update Aggregates and nor set status condition", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Aggregates).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeFalse())
		})
	})

	Context("when GetAggregates fails", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Setting a missing aggregate")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Aggregates = []string{"test-aggregate1"}
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Mock GetAggregates to fail
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, err := fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should set error condition", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).To(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
			cond := meta.FindStatusCondition(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonFailed))
			Expect(cond.Message).To(ContainSubstring("failed listing aggregates"))
		})
	})

	Context("when AddToAggregate fails", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Setting a missing aggregate")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Aggregates = []string{"test-aggregate1"}
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

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

			// Mock AddHost to fail
			fakeServer.Mux.HandleFunc("POST /os-aggregates/42/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, err := fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot add host to aggregate", "code": 409}}`)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should set error condition", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).To(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
			cond := meta.FindStatusCondition(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonFailed))
			Expect(cond.Message).To(ContainSubstring("encountered errors during aggregate update"))
		})
	})

	Context("when RemoveFromAggregate fails", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			// update status to have existing aggregate
			hypervisor.Status.Aggregates = []string{"test-aggregate2"}
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			// Mock resourceproviders.GetAggregates
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, AggregateListBodyFull)
				Expect(err).NotTo(HaveOccurred())
			})

			// Mock RemoveHost to fail
			fakeServer.Mux.HandleFunc("POST /os-aggregates/100001/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, err := fmt.Fprint(w, `{"conflictingRequest": {"message": "Cannot remove host from aggregate", "code": 409}}`)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should set error condition", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).To(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
			cond := meta.FindStatusCondition(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonFailed))
			Expect(cond.Message).To(ContainSubstring("encountered errors during aggregate update"))
		})
	})

	Context("when setErrorCondition does not change status", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Setting a missing aggregate and pre-existing error condition")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Aggregates = []string{"test-aggregate1"}
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Pre-set the exact same error condition that would be set
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeAggregatesUpdated,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonFailed,
				Message: "failed listing aggregates: test error",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			// Mock GetAggregates to fail
			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, err := fmt.Fprint(w, `{"error": "test error"}`)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should not update status when condition is already set", func(ctx SpecContext) {
			_, err := aggregatesController.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
			Expect(err).To(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(aggregatesController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)).To(BeTrue())
		})
	})

})
