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

var _ = Describe("TraitsController", func() {
	var tc *TraitsController
	const TraitsBody = `
{
    "resource_provider_generation": 1,
    "traits": [
        "CUSTOM_FOO",
        "HW_CPU_X86_VMX"
    ]
}`
	const TraitsBodyUpdated = `
{
	"resource_provider_generation": 2,
	"traits": [
		"CUSTOM_FOO",
		"CUSTOM_BAR",
		"HW_CPU_X86_VMX"
	]
}`

	// Setup and teardown

	BeforeEach(func(ctx context.Context) {
		By("Setting up the OpenStack http mock server")
		testhelper.SetupHTTP()

		By("Creating the TraitsController")
		tc = &TraitsController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			serviceClient: client.ServiceClient(),
		}

		By("Creating a Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: v1.ObjectMeta{
				Name:      "hv-test",
				Namespace: "default",
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
				CustomTraits:     []string{"CUSTOM_FOO", "CUSTOM_BAR"},
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		// Ensure the resource status is being updated
		meta.SetStatusCondition(&hypervisor.Status.Conditions, v1.Condition{
			Type:   kvmv1.ConditionTypeReady,
			Status: v1.ConditionTrue,
			Reason: "UnitTest",
		})
		hypervisor.Status.Traits = []string{"CUSTOM_FOO", "HW_CPU_X86_VMX"}
		hypervisor.Status.HypervisorID = "1234"
		Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
	})

	AfterEach(func() {
		By("Tearing down the OpenStack http mock server")
		testhelper.TeardownHTTP()

		By("Deleting the Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{}
		Expect(tc.Client.Get(ctx, types.NamespacedName{Name: "hv-test", Namespace: "default"}, hypervisor)).To(Succeed())
		Expect(tc.Client.Delete(ctx, hypervisor)).To(Succeed())
	})

	// Tests

	Context("Reconcile", func() {
		BeforeEach(func() {
			// Mock resourceproviders.GetTraits
			testhelper.Mux.HandleFunc("GET /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				_, err := fmt.Fprint(w, TraitsBody)
				Expect(err).NotTo(HaveOccurred())
			})
			// Mock resourceproviders.UpdateTraits
			testhelper.Mux.HandleFunc("PUT /resource_providers/1234/traits", func(w http.ResponseWriter, r *http.Request) {
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
		})

		It("should update traits and set status condition when traits differ", func() {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "hv-test", Namespace: "default"}}
			_, err := tc.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &kvmv1.Hypervisor{}
			Expect(tc.Client.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Traits).To(ContainElements("CUSTOM_FOO", "CUSTOM_BAR", "HW_CPU_X86_VMX"))
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, ConditionTypeTraitsUpdated)).To(BeTrue())
		})
	})
})
