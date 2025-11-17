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

var _ = Describe("HypervisorServiceController", func() {
	var (
		tc             *HypervisorMaintenanceController
		fakeServer     testhelper.FakeServer
		hypervisorName = types.NamespacedName{Name: "hv-test"}
	)

	const (
		ServiceEnabledResponse = `{
			"service": {
        		"id": "e81d66a4-ddd3-4aba-8a84-171d1cb4d339",
        		"binary": "nova-compute",
        		"disabled_reason": "maintenance",
        		"host": "host1",
        		"state": "up",
        		"status": "disabled",
        		"updated_at": "2012-10-29T13:42:05.000000",
        		"forced_down": false,
        		"zone": "nova"
    		}
		}`
	)

	mockServiceUpdate := func(expectedBody string) {
		// Mock services.Update
		fakeServer.Mux.HandleFunc("PUT /os-services/1234", func(w http.ResponseWriter, r *http.Request) {
			// parse request
			Expect(r.Method).To(Equal("PUT"))
			Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))

			// verify request body
			body := make([]byte, r.ContentLength)
			_, err := r.Body.Read(body)
			Expect(err == nil || err.Error() == "EOF").To(BeTrue())
			Expect(string(body)).To(MatchJSON(expectedBody))

			w.WriteHeader(http.StatusOK)
			_, err = fmt.Fprint(w, ServiceEnabledResponse)
			Expect(err).NotTo(HaveOccurred())
		})
	}

	// Setup and teardown
	BeforeEach(func(ctx context.Context) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()

		By("Creating the HypervisorServiceController")
		tc = &HypervisorMaintenanceController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}

		By("Creating a blank Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: v1.ObjectMeta{
				Name:      hypervisorName.Name,
				Namespace: hypervisorName.Namespace,
			},
			Spec: kvmv1.HypervisorSpec{},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
	})

	AfterEach(func() {
		By("Deleting the Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{}
		Expect(tc.Client.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
		Expect(tc.Client.Delete(ctx, hypervisor)).To(Succeed())

		By("Tearing down the OpenStack http mock server")
		fakeServer.Teardown()
	})

	// Tests
	Context("Onboarded Hypervisor", func() {
		BeforeEach(func() {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(tc.Client.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Status.ServiceID = "1234"
			meta.SetStatusCondition(&hypervisor.Status.Conditions,
				v1.Condition{
					Type:    ConditionTypeOnboarding,
					Status:  v1.ConditionFalse,
					Reason:  v1.StatusSuccess,
					Message: "random text",
				},
			)

			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		Describe("Enabling or Disabling the Nova Service", func() {
			Context("Spec.Maintenance=\"\"", func() {
				BeforeEach(func() {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(tc.Client.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = ""
					Expect(tc.Client.Update(ctx, hypervisor)).To(Succeed())
					expectedBody := `{"status": "enabled"}`
					mockServiceUpdate(expectedBody)
					req := ctrl.Request{NamespacedName: hypervisorName}
					_, err := tc.Reconcile(ctx, req)
					Expect(err).NotTo(HaveOccurred())
				})

				It("should set the ConditionTypeHypervisorDisabled to false", func() {
					updated := &kvmv1.Hypervisor{}
					Expect(tc.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
					Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)).To(BeTrue())
				})
			}) // Spec.Maintenance=""
		})

		for _, mode := range []string{"auto", "manual", "ha"} {
			Context(fmt.Sprintf("Spec.Maintenance=\"%v\"", mode), func() {
				BeforeEach(func() {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(tc.Client.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = mode
					Expect(tc.Client.Update(ctx, hypervisor)).To(Succeed())
					expectedBody := fmt.Sprintf(`{"disabled_reason": "Hypervisor CRD: spec.maintenance=%v", "status": "disabled"}`, mode)
					mockServiceUpdate(expectedBody)
					req := ctrl.Request{NamespacedName: hypervisorName}
					_, err := tc.Reconcile(ctx, req)
					Expect(err).NotTo(HaveOccurred())
				})

				It("should set the ConditionTypeHypervisorDisabled to true", func() {
					updated := &kvmv1.Hypervisor{}
					Expect(tc.Client.Get(ctx, hypervisorName, updated)).To(Succeed())
					Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)).To(BeTrue())
				})
			}) // Spec.Maintenance="<mode>"
		}

	}) // Context Onboarded Hypervisor
})
