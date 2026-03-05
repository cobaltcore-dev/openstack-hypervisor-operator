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
	"os"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("HypervisorInstanceHaController", Label("test"), func() {
	const (
		EOF      = "EOF"
		hostName = "test-hostname"
		region   = "region"
		zone     = "zone"
	)
	var (
		controller     *HypervisorInstanceHaController
		fakeServer     testhelper.FakeServer
		hypervisorName = types.NamespacedName{Name: "test-node"}
		numApiRequest  = 0
	)

	mockApi := func(expectedBody string) {
		fakeServer.Mux.HandleFunc(fmt.Sprintf("POST /api/hypervisors/%v", hostName), func(w http.ResponseWriter, r *http.Request) {
			numApiRequest++
			Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
			body := make([]byte, r.ContentLength)
			_, err := r.Body.Read(body)
			Expect(err == nil || err.Error() == EOF).To(BeTrue())
			Expect(string(body)).To(MatchJSON(expectedBody))

			w.Header().Add("Content-Type", "application/json")
			_, err = fmt.Fprint(w, `{}`)
			Expect(err).NotTo(HaveOccurred())
		})
	}

	mockApiDisable := func() {
		mockApi(`{"enabled": false}`)
	}

	mockApiEnable := func() {
		mockApi(`{"enabled": true}`)
	}

	BeforeEach(func(ctx SpecContext) {
		fakeServer = testhelper.SetupHTTP()
		numApiRequest = 0
		DeferCleanup(fakeServer.Teardown)

		Expect(os.Setenv("KVM_HA_SERVICE_URL", fakeServer.Endpoint())).To(Succeed())
		DeferCleanup(os.Unsetenv, "KVM_HA_SERVICE_URL")

		controller = &HypervisorInstanceHaController{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		By("creating the hypervisor resource with ha-enabled")
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
				HighAvailability: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})

		By("setting onboarding to completed successfully")
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

		Context("with ha disabled", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting hv.Spec.HighAvailability=false")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				hv.Spec.HighAvailability = false
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				mockApiDisable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status with Succeeded reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
				Expect(condition.Message).To(Equal("HA disabled per spec"))
			})
		})

		Context("with hypervisor evicted", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting ConditionTypeEvicting to false (evicted)")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				mockApiDisable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status with Evicted reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonHaEvicted))
				Expect(condition.Message).To(Equal("HA disabled due to eviction"))
			})
		})

		Context("with hypervisor in Initial/Testing phase during onboarding", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting onboarding to Testing (still in progress)")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonTesting,
					Message: "Testing in progress",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				mockApiDisable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status with Onboarding reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonHaOnboarding))
				Expect(condition.Message).To(Equal("HA disabled before onboarding"))
			})
		})

		Context("with hypervisor in Handover phase during onboarding", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting onboarding to Handover (ready for HA)")
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

			It("should reflect the enabled state in the status with Succeeded reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
				Expect(condition.Message).To(Equal("HA is enabled"))
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

				mockApiDisable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the disabled state in the status with Onboarding reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonHaOnboarding))
				Expect(condition.Message).To(Equal("HA disabled before onboarding"))
			})
		})

		Context("with ha enabled", func() {
			BeforeEach(func() {
				mockApiEnable()
			})

			It("should have called the api server once", func() {
				Expect(numApiRequest).To(Equal(1))
			})

			It("should reflect the enabled state in the status with Succeeded reason", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
				Expect(condition.Message).To(Equal("HA is enabled"))
			})
		})
	})

	Context("Failure modes", func() {
		JustBeforeEach(func(ctx context.Context) {
			By("reconciling once (expecting error)")
			//nolint:errcheck // Ignoring error - failure tests expect errors
			controller.Reconcile(ctx, ctrl.Request{NamespacedName: hypervisorName})
		})

		Context("when enabling HA fails", func() {
			BeforeEach(func(ctx SpecContext) {
				// Mock API to return error BEFORE JustBeforeEach runs
				fakeServer.Mux.HandleFunc(fmt.Sprintf("POST /api/hypervisors/%v", hostName), func(w http.ResponseWriter, r *http.Request) {
					numApiRequest++
					w.WriteHeader(http.StatusInternalServerError)
					_, err := fmt.Fprint(w, `{"error": "internal server error"}`)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("should reflect the error in status with Unknown condition", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonFailed))
				Expect(condition.Message).To(ContainSubstring("unexpected response"))
				Expect(numApiRequest).To(BeNumerically(">", 0))
			})
		})

		Context("when disabling HA fails", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				By("setting hv.Spec.HighAvailability=false")
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				hv.Spec.HighAvailability = false
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				// Mock API to return error BEFORE JustBeforeEach runs
				fakeServer.Mux.HandleFunc(fmt.Sprintf("POST /api/hypervisors/%v", hostName), func(w http.ResponseWriter, r *http.Request) {
					numApiRequest++
					w.WriteHeader(http.StatusInternalServerError)
					_, err := fmt.Fprint(w, `{"error": "internal server error"}`)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("should reflect the error in status with Unknown condition", func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())

				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonFailed))
				Expect(condition.Message).To(ContainSubstring("unexpected response"))
				Expect(numApiRequest).To(BeNumerically(">", 0))
			})
		})
	})
})
