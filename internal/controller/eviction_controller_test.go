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
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Eviction Controller", func() {
	const (
		resourceName   = "test-resource"
		namespaceName  = "default"
		hypervisorName = "test-hypervisor"
		serviceId      = "test-id"
		hypervisorId   = "test-hv-id"
		hypervisorTpl  = `{
    "hypervisor": {
        "host_ip": "192.168.1.135",
        "hypervisor_hostname": "fake-mini",
        "hypervisor_type": "fake",
        "hypervisor_version": 1000,
        "id": "test-hv-id",
        "servers": [],
        "service": {
            "disabled_reason": %v,
            "host": "compute",
            "id": "test-id"
        },
        "state": "up",
        "status": "%v",
        "uptime": null
	}
}`
	)
	var (
		typeNamespacedName = types.NamespacedName{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		evictionObjectMeta = metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		reconcileRequest     = ctrl.Request{NamespacedName: typeNamespacedName}
		controllerReconciler *EvictionReconciler
		fakeServer           testhelper.FakeServer
	)

	AfterEach(func(ctx SpecContext) {
		resource := &kvmv1.Eviction{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err != nil {
			if !errors.IsNotFound(err) {
				Expect(err).ShouldNot(HaveOccurred())
			}
		} else {
			By("Cleanup the specific resource instance Eviction")
			Expect(controllerReconciler).NotTo(BeNil())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err := controllerReconciler.Reconcile(ctx, reconcileRequest)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).Should(HaveOccurred())
		}
	})

	Describe("API validation", func() {
		When("creating an eviction without hypervisor", func() {
			It("it should fail creating the resource", func(ctx SpecContext) {
				resource := &kvmv1.Eviction{
					ObjectMeta: evictionObjectMeta,
					Spec: kvmv1.EvictionSpec{
						Reason: "test-reason",
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.hypervisor: Invalid value: "": spec.hypervisor in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction without reason", func() {
			It("it should fail creating the resource", func(ctx SpecContext) {
				resource := &kvmv1.Eviction{
					ObjectMeta: evictionObjectMeta,
					Spec: kvmv1.EvictionSpec{
						Hypervisor: hypervisorName,
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.reason: Invalid value: "": spec.reason in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction with reason and hypervisor", func() {
			BeforeEach(func(ctx SpecContext) {
				By("creating the hypervisor resource")
				hypervisor := &kvmv1.Hypervisor{
					ObjectMeta: metav1.ObjectMeta{
						Name: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
				DeferCleanup(func(ctx SpecContext) {
					Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
				})
			})
			It("should successfully create the resource", func(ctx SpecContext) {
				eviction := &kvmv1.Eviction{
					ObjectMeta: evictionObjectMeta,
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, eviction)).To(Succeed())
				Expect(k8sClient.Delete(ctx, eviction)).To(Succeed())
			})
		})
	})

	Describe("Reconciliation", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Setting up the OpenStack http mock server")
			fakeServer = testhelper.SetupHTTP()

			DeferCleanup(func(ctx SpecContext) {
				fakeServer.Teardown()
			})

			// Install default handler to fail unhandled requests
			fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
			})

			By("Creating the EvictionReconciler")

			controllerReconciler = &EvictionReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				computeClient: client.ServiceClient(fakeServer),
			}

			DeferCleanup(func() {
				controllerReconciler = nil
			})
		})

		Describe("an eviction for an onboarded 'test-hypervisor'", func() {
			BeforeEach(func(ctx SpecContext) {
				By("creating the hypervisor resource")
				hypervisor := &kvmv1.Hypervisor{
					ObjectMeta: metav1.ObjectMeta{
						Name: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
				DeferCleanup(func(ctx SpecContext) {
					Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
				})

				hypervisor.Status.HypervisorID = hypervisorId
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

				By("creating the eviction")
				eviction := &kvmv1.Eviction{
					ObjectMeta: evictionObjectMeta,
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, eviction)).To(Succeed())
			})

			When("hypervisor is not found in openstack", func() {
				BeforeEach(func() {
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusNotFound)
					})
				})

				It("should fail reconciliation", func(ctx SpecContext) {
					for range 3 {
						_, err := controllerReconciler.Reconcile(ctx, reconcileRequest)
						Expect(err).NotTo(HaveOccurred())
					}

					resource := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be false due to missing hypervisor
					Expect(resource.Status.Conditions).To(ContainElements(SatisfyAll(
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Type", kvmv1.ConditionTypeEvicting),
						HaveField("Reason", "Failed"),
						HaveField("Message", ContainSubstring("got 404")),
					)))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})

			})
			When("enabled hypervisor has no servers", func() {
				BeforeEach(func(ctx SpecContext) {
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						rHypervisorId := r.PathValue("hypervisor_id")
						Expect(rHypervisorId).To(Equal(hypervisorId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, hypervisorTpl, "null", "enabled")
						Expect(err).To(Succeed())
					})

					fakeServer.Mux.HandleFunc("PUT /os-services/{service_id}", func(w http.ResponseWriter, r *http.Request) {
						rServiceId := r.PathValue("service_id")
						Expect(rServiceId).To(Equal(serviceId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)
						Expect(err).To(Succeed())
					})
				})
				It("should succeed the reconciliation", func(ctx SpecContext) {
					runningCond := &metav1.Condition{
						Type:    kvmv1.ConditionTypeEvicting,
						Status:  metav1.ConditionTrue,
						Reason:  kvmv1.ConditionReasonRunning,
						Message: "Running",
					}

					hypervisorDisabledCond := &metav1.Condition{
						Type:    kvmv1.ConditionTypeHypervisorDisabled,
						Status:  metav1.ConditionTrue,
						Reason:  kvmv1.ConditionReasonSucceeded,
						Message: "Hypervisor disabled successfully",
					}

					preflightCond := &metav1.Condition{
						Type:    kvmv1.ConditionTypePreflight,
						Status:  metav1.ConditionTrue,
						Reason:  kvmv1.ConditionReasonSucceeded,
						Message: "Preflight checks passed",
					}

					expectations := []struct {
						conditions []*metav1.Condition
						finalizers []string
					}{
						// 1. expect the Condition Evicting to be true
						{conditions: []*metav1.Condition{runningCond}, finalizers: nil},

						// 2. expect the Finalizer to be added
						{conditions: []*metav1.Condition{runningCond}, finalizers: []string{evictionFinalizerName}},

						// 3. expect the hypervisor to be disabled
						{
							conditions: []*metav1.Condition{runningCond, hypervisorDisabledCond},
							finalizers: []string{evictionFinalizerName},
						},

						// 4. expect the preflight condition to be set to succeeded
						{
							conditions: []*metav1.Condition{runningCond, hypervisorDisabledCond, preflightCond},
							finalizers: []string{evictionFinalizerName},
						},

						// 5. expect the eviction condition to be set to succeeded
						{
							conditions: []*metav1.Condition{{
								Type:    kvmv1.ConditionTypeEvicting,
								Status:  metav1.ConditionFalse,
								Reason:  kvmv1.ConditionReasonSucceeded,
								Message: "eviction completed successfully"}},
							finalizers: []string{evictionFinalizerName}},
					}

					for i, expectation := range expectations {
						By(fmt.Sprintf("Reconciliation step %d", i+1))
						// Reconcile the resource
						result, err := controllerReconciler.Reconcile(ctx, reconcileRequest)
						Expect(result).To(Equal(ctrl.Result{}))
						Expect(err).NotTo(HaveOccurred())

						resource := &kvmv1.Eviction{}
						Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).NotTo(HaveOccurred())

						// Check the condition
						for _, expect := range expectation.conditions {
							reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, expect.Type)
							Expect(reconcileStatus).NotTo(BeNil())
							Expect(reconcileStatus.Status).To(Equal(expect.Status))
							Expect(reconcileStatus.Reason).To(Equal(expect.Reason))
							Expect(reconcileStatus.Message).To(ContainSubstring(expect.Message))
						}
						// Check finalizers
						Expect(resource.GetFinalizers()).To(Equal(expectation.finalizers))
					}
				})
			})
			When("disabled hypervisor has no servers", func() {
				BeforeEach(func(ctx SpecContext) {
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						rHypervisorId := r.PathValue("hypervisor_id")
						Expect(rHypervisorId).To(Equal(hypervisorId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, hypervisorTpl, `"some reason"`, "disabled")
						Expect(err).To(Succeed())
					})
					fakeServer.Mux.HandleFunc("PUT /os-services/{service_id}", func(w http.ResponseWriter, r *http.Request) {
						rServiceId := r.PathValue("service_id")
						Expect(rServiceId).To(Equal(serviceId))
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)
						Expect(err).To(Succeed())
					})
				})
				It("should succeed the reconciliation", func(ctx SpecContext) {
					for range 3 {
						_, err := controllerReconciler.Reconcile(ctx, reconcileRequest)
						Expect(err).NotTo(HaveOccurred())
					}

					resource := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be true
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, kvmv1.ConditionTypeEvicting)
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal(kvmv1.ConditionReasonRunning))

					// expect hypervisor disabled condition to be true for reason of already disabled
					reconcileStatus = meta.FindStatusCondition(resource.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
					Expect(reconcileStatus.Message).To(ContainSubstring("already disabled"))

					_, err = controllerReconciler.Reconcile(ctx, reconcileRequest)
					Expect(err).NotTo(HaveOccurred())
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect reconciliation to be successfully finished
					reconcileStatus = meta.FindStatusCondition(resource.Status.Conditions, kvmv1.ConditionTypeEvicting)
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionFalse))
					Expect(reconcileStatus.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})
			})
		})
	})
})
