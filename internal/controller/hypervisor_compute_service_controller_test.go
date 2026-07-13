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

package controller

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("HypervisorComputeServiceController", Label("test"), func() {
	const (
		EOF       = "EOF"
		hostName  = "test-hostname"
		region    = "region"
		zone      = "zone"
		serviceId = "test-service-id"
	)
	var (
		controller     *HypervisorComputeServiceController
		fakeServer     testhelper.FakeServer
		hypervisorName = types.NamespacedName{Name: "test-node"}
		numApiRequest  = 0
	)

	mockApi := func(expectedStatus string, expectedReason string) {
		fakeServer.Mux.HandleFunc(fmt.Sprintf("/os-services/%v", serviceId), func(w http.ResponseWriter, r *http.Request) {
			numApiRequest++
			Expect(r.Method).To(Equal("PUT"))
			Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
			body := make([]byte, r.ContentLength)
			_, err := r.Body.Read(body)
			Expect(err == nil || err.Error() == EOF).To(BeTrue())

			bodyStr := string(body)
			Expect(bodyStr).To(ContainSubstring(fmt.Sprintf(`"status":"%s"`, expectedStatus)))
			if expectedReason != "" {
				Expect(bodyStr).To(ContainSubstring(expectedReason))
			}

			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, err = fmt.Fprintf(w, `{"service": {"id": "%v", "status": "%s"}}`, serviceId, expectedStatus)
			Expect(err).NotTo(HaveOccurred())
		})
	}

	mockApiEnable := func() {
		mockApi("enabled", "")
	}

	mockApiDisable := func(reason string) {
		mockApi("disabled", reason)
	}

	BeforeEach(func(ctx SpecContext) {
		fakeServer = testhelper.SetupHTTP()
		numApiRequest = 0
		DeferCleanup(fakeServer.Teardown)

		controller = &HypervisorComputeServiceController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}

		By("creating the hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName.Name,
				Labels: map[string]string{
					corev1.LabelHostname:       hostName,
					corev1.LabelTopologyRegion: region,
					corev1.LabelTopologyZone:   zone,
				},
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})

		By("setting service ID and onboarding to completed successfully")
		hypervisor.Status.ServiceID = serviceId
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonSucceeded,
			Message: "Onboarding succeeded",
		})
		Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
	})

	Context("Happy path", func() {
		JustBeforeEach(func(ctx context.Context) {
			By("reconciling twice")
			for range 2 {
				_, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
				Expect(err).To(Succeed())
			}
		})

		Context("with maintenance=no-schedule", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting hv.Spec.Maintenance=no-schedule")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				hv.Spec.Maintenance = kvmv1.MaintenanceNoSchedule
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				mockApiDisable("no-schedule")
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
				Expect(condition.Message).To(ContainSubstring("no-schedule"))
			})
		})

		Context("with maintenance=manual", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting hv.Spec.Maintenance=manual")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				hv.Spec.Maintenance = kvmv1.MaintenanceManual
				hv.Spec.MaintenanceReason = "testing"
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				mockApiDisable("manual")
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
			})
		})

		Context("with onboarding aborted", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting onboarding to aborted")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonAborted,
					Message: "Onboarding aborted",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				mockApiDisable("aborted")
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonAborted))
			})
		})

		Context("with onboarding in testing phase", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting onboarding to testing (in progress)")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonTesting,
					Message: "Testing in progress",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				mockApiEnable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the enabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
			})
		})

		Context("with onboarding in handover phase", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting onboarding to handover (ready for completion)")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonHandover,
					Message: "Handover in progress",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				mockApiEnable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the enabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
			})
		})

		Context("with compute service enabled (default)", func() {
			BeforeEach(func() {
				mockApiEnable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the enabled state in the status", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
				Expect(condition.Message).To(Equal("Compute service is enabled"))
			})
		})
	})

	Context("Failure modes", func() {
		JustBeforeEach(func(ctx context.Context) {
			By("reconciling once (expecting error)")
			//nolint:errcheck // Ignoring error - failure tests expect errors
			controller.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
		})

		Context("when enabling compute service fails", func() {
			BeforeEach(func(ctx SpecContext) {
				// Mock API to return error
				fakeServer.Mux.HandleFunc(fmt.Sprintf("/os-services/%v", serviceId), func(w http.ResponseWriter, r *http.Request) {
					numApiRequest++
					w.WriteHeader(http.StatusInternalServerError)
					_, err := fmt.Fprint(w, `{"error": "internal server error"}`)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("should reflect the error in status with Unknown condition", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonFailed))
				Expect(numApiRequest).To(BeNumerically(">", 0))
			})
		})

		Context("when disabling compute service fails", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting hv.Spec.Maintenance=no-schedule")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				hv.Spec.Maintenance = kvmv1.MaintenanceNoSchedule
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				// Mock API to return error
				fakeServer.Mux.HandleFunc(fmt.Sprintf("/os-services/%v", serviceId), func(w http.ResponseWriter, r *http.Request) {
					numApiRequest++
					w.WriteHeader(http.StatusInternalServerError)
					_, err := fmt.Fprint(w, `{"error": "internal server error"}`)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("should reflect the error in status with Unknown condition", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonFailed))
				Expect(numApiRequest).To(BeNumerically(">", 0))
			})
		})
	})
})
