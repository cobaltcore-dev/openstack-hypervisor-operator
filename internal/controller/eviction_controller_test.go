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
	"math/rand"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlRuntimeClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var HypervisorWithServers = `{
    "hypervisors": [
        {
            "service": {
                "host": "e6a37ee802d74863ab8b91ade8f12a67",
                "id": "%s",
                "disabled_reason": "%s"
            },
            "cpu_info": {
                "arch": "x86_64",
                "model": "Nehalem",
                "vendor": "Intel",
                "features": [
                    "pge",
                    "clflush"
                ],
                "topology": {
                    "cores": 1,
                    "threads": 1,
                    "sockets": 4
                }
            },
            "current_workload": 0,
            "status": "enabled",
            "state": "up",
            "disk_available_least": 0,
            "host_ip": "1.1.1.1",
            "free_disk_gb": 1028,
            "free_ram_mb": 7680,
            "hypervisor_hostname": %q,
            "hypervisor_type": "fake",
            "hypervisor_version": 2002000,
            "id": "c48f6247-abe4-4a24-824e-ea39e108874f",
            "local_gb": 1028,
            "local_gb_used": 0,
            "memory_mb": 8192,
            "memory_mb_used": 512,
            "running_vms": 0,
            "vcpus": 1,
            "vcpus_used": 0
        }
    ]
}`

var _ = Describe("Eviction Controller", func() {
	const (
		resourceName   = "test-resource"
		namespaceName  = "default"
		hypervisorName = "test-hypervisor"
		serviceId      = "test-id"
	)
	var (
		typeNamespacedName = types.NamespacedName{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		reconcileRequest     = reconcile.Request{NamespacedName: typeNamespacedName}
		controllerReconciler *EvictionReconciler
	)

	ctx := context.Background()

	BeforeEach(func() {
		By("Setting up the OpenStack http mock server")
		testhelper.SetupHTTP()
	})

	AfterEach(func() {
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

		By("Tearing down the OpenStack http mock server")
		testhelper.TeardownHTTP()
		controllerReconciler = nil
		hv := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName,
			},
		}
		Expect(ctrlRuntimeClient.IgnoreNotFound(k8sClient.Delete(ctx, hv))).To(Succeed())
	})

	Describe("API validation", func() {
		When("creating an eviction without hypervisor", func() {
			It("it should fail creating the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason: "test-reason",
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.hypervisor: Invalid value: "": spec.hypervisor in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction without reason", func() {
			It("it should fail creating the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Hypervisor: hypervisorName,
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.reason: Invalid value: "": spec.reason in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction with reason and hypervisor", func() {
			BeforeEach(func() {
				By("creating the hypervisor resource")
				hypervisor := &kvmv1.Hypervisor{
					ObjectMeta: metav1.ObjectMeta{
						Name: hypervisorName,
					},
				}
				Expect(ctrlRuntimeClient.IgnoreAlreadyExists(k8sClient.Create(ctx, hypervisor))).To(Succeed())
			})
			It("should successfully create the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})
		})
	})

	Describe("Reconciliation", func() {
		Describe("an eviction for 'test-hypervisor'", func() {
			BeforeEach(func() {
				By("Creating the resource")
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Creating the controller")
				controllerReconciler = &EvictionReconciler{
					Client:        k8sClient,
					Scheme:        k8sClient.Scheme(),
					computeClient: client.ServiceClient(),
					rand:          rand.New(rand.NewSource(42)),
				}
			})

			When("hypervisor is not found in openstack", func() {
				BeforeEach(func() {
					testhelper.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, `{"hypervisors": []}`)).ToNot(BeNil())
					})
				})

				It("should fail reconciliation", func() {
					for i := 0; i < 3; i++ {
						_, err := controllerReconciler.Reconcile(ctx, reconcileRequest)
						Expect(err).NotTo(HaveOccurred())
					}

					resource := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be false due to missing hypervisor
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, kvmv1.ConditionTypeEvicting)
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionFalse))
					Expect(reconcileStatus.Reason).To(Equal("Failed"))
					Expect(reconcileStatus.Message).To(ContainSubstring("no hypervisor found"))
					Expect(resource.Status.HypervisorServiceId).To(Equal(""))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})

			})
			When("enabled hypervisor has no servers", func() {
				BeforeEach(func() {
					testhelper.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "", hypervisorName)).ToNot(BeNil())
					})
					testhelper.Mux.HandleFunc("PUT /os-services/test-id", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)).ToNot(BeNil())
					})
					By("creating the hypervisor resource")
					hypervisor := &kvmv1.Hypervisor{
						ObjectMeta: metav1.ObjectMeta{
							Name: hypervisorName,
						},
					}
					Expect(ctrlRuntimeClient.IgnoreAlreadyExists(k8sClient.Create(ctx, hypervisor))).To(Succeed())
				})
				It("should succeed the reconciliation", func() {
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
						Expect(result).To(Equal(reconcile.Result{}))
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
				BeforeEach(func() {
					testhelper.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "some reason", hypervisorName)).ToNot(BeNil())
					})
					testhelper.Mux.HandleFunc("PUT /os-services/test-id", func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)).ToNot(BeNil())
					})
					By("creating the hypervisor resource")
					hypervisor := &kvmv1.Hypervisor{
						ObjectMeta: metav1.ObjectMeta{
							Name: hypervisorName,
						},
					}
					Expect(ctrlRuntimeClient.IgnoreAlreadyExists(k8sClient.Create(ctx, hypervisor))).To(Succeed())
				})
				It("should succeed the reconciliation", func() {
					for i := 0; i < 3; i++ {
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
