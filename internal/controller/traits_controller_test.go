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

var _ = Describe("TraitsController", func() {
	const (
		TraitsBody = `
{
    "resource_provider_generation": 1,
    "traits": [
        "CUSTOM_FOO",
        "HW_CPU_X86_VMX"
    ]
}`
		TraitsBodyUpdated = `
{
	"resource_provider_generation": 2,
	"traits": [
		"CUSTOM_FOO",
		"CUSTOM_BAR",
		"HW_CPU_X86_VMX"
	]
}`
	)

	var (
		traitsController *TraitsController
		fakeServer       testhelper.FakeServer
		hypervisorName   = types.NamespacedName{Name: "hv-test"}
	)

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Install default handler to fail unhandled requests
		fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
		})

		By("Creating the TraitsController")
		traitsController = &TraitsController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			serviceClient: client.ServiceClient(fakeServer),
		}

		By("Creating a Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName.Name,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
				CustomTraits:     []string{"CUSTOM_FOO", "CUSTOM_BAR"},
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})
	})

	Context("Reconcile after onboarding before decommissioning", func() {
		BeforeEach(func(ctx SpecContext) {
			// Mock resourceproviders.GetTraits
			fakeServer.Mux.HandleFunc("GET /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err := fmt.Fprint(w, TraitsBody)
				Expect(err).NotTo(HaveOccurred())
			})
			// Mock resourceproviders.UpdateTraits
			fakeServer.Mux.HandleFunc("PUT /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				// parse request
				Expect(r.Method).To(Equal("PUT"))
				Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))

				// verify request body
				expectedBody := `{
					"resource_provider_generation": 1,
					"traits": [
						"CUSTOM_BAR",
						"CUSTOM_FOO",
						"HW_CPU_X86_VMX"
						]
				}`
				body := make([]byte, r.ContentLength)
				_, err := r.Body.Read(body)
				Expect(err == nil || err.Error() == "EOF").To(BeTrue())
				Expect(string(body)).To(MatchJSON(expectedBody))

				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err = fmt.Fprint(w, TraitsBodyUpdated)
				Expect(err).NotTo(HaveOccurred())
			})

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: "UnitTest",
			})
			hypervisor.Status.HypervisorID = "1234"
			hypervisor.Status.Traits = []string{"CUSTOM_FOO", "HW_CPU_X86_VMX"}
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should update traits and set status condition when traits differ", func(ctx SpecContext) {
			req := ctrl.Request{NamespacedName: hypervisorName}
			_, err := traitsController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(traitsController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Traits).To(ContainElements("CUSTOM_FOO", "CUSTOM_BAR", "HW_CPU_X86_VMX"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated)).To(BeTrue())
		})
	})

	Context("Reconcile before onboarding", func() {
		BeforeEach(func(ctx SpecContext) {
			// Mock resourceproviders.GetTraits
			fakeServer.Mux.HandleFunc("GET /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				defer GinkgoRecover()
				Fail("should not be called")
			})
			// Mock resourceproviders.UpdateTraits
			fakeServer.Mux.HandleFunc("PUT /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				defer GinkgoRecover()
				Fail("should not be called")
			})

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Status.HypervisorID = "1234"
			hypervisor.Status.Traits = []string{"CUSTOM_FOO", "HW_CPU_X86_VMX"}
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should not update traits", func(ctx SpecContext) {
			req := ctrl.Request{NamespacedName: hypervisorName}
			_, err := traitsController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(traitsController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Traits).NotTo(ContainElements("CUSTOM_FOO", "CUSTOM_BAR", "HW_CPU_X86_VMX"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated)).To(BeFalse())
		})
	})

	Context("Reconcile when terminating", func() {
		BeforeEach(func(ctx SpecContext) {
			// Mock resourceproviders.GetTraits
			fakeServer.Mux.HandleFunc("GET /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				defer GinkgoRecover()
				Fail("should not be called")
			})
			// Mock resourceproviders.UpdateTraits
			fakeServer.Mux.HandleFunc("PUT /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				defer GinkgoRecover()
				Fail("should not be called")
			})

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: "UnitTest",
			})
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeTerminating,
				Status: metav1.ConditionTrue,
				Reason: "UnitTest",
			})
			hypervisor.Status.Traits = []string{"CUSTOM_FOO", "HW_CPU_X86_VMX"}
			hypervisor.Status.HypervisorID = "1234"
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

		})

		It("should not update traits", func(ctx SpecContext) {
			req := ctrl.Request{NamespacedName: hypervisorName}
			_, err := traitsController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(traitsController.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Status.Traits).NotTo(ContainElements("CUSTOM_FOO", "CUSTOM_BAR", "HW_CPU_X86_VMX"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated)).To(BeFalse())
		})
	})
})
