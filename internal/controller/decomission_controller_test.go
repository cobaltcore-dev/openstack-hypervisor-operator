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
	"os"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Decommission Controller", func() {
	const (
		EOF            = "EOF"
		serviceId      = "service-1234"
		hypervisorName = "node-test"
		namespaceName  = "namespace-test"
	)

	var (
		decommissionReconciler *NodeDecommissionReconciler
		resourceName           = types.NamespacedName{Name: hypervisorName}
		reconcileReq           = ctrl.Request{NamespacedName: resourceName}
		fakeServer             testhelper.FakeServer
	)

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Install default handler to fail unhandled requests
		fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
		})

		By("Setting KVM_HA_SERVICE_URL environment variable")
		os.Setenv("KVM_HA_SERVICE_URL", fakeServer.Endpoint()+"instance-ha")
		DeferCleanup(func() {
			os.Unsetenv("KVM_HA_SERVICE_URL")
		})

		By("Creating the NodeDecommissionReconciler")
		decommissionReconciler = &NodeDecommissionReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			computeClient:   client.ServiceClient(fakeServer),
			placementClient: client.ServiceClient(fakeServer),
		}

		By("Creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
		Expect(k8sclient.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		By("Creating the hypervisor resource with lifecycle enabled")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName.Name,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})
	})

	Context("Happy Path", func() {
		Context("When marking the hypervisor terminating", func() {
			JustBeforeEach(func(ctx SpecContext) {
				By("Reconciling first to add the finalizer")
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())

				By("Marking the hypervisor for termination")
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
				Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

				By("Setting terminating condition")
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeTerminating,
					Status:  metav1.ConditionTrue,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			Context("when the hypervisor was set to ready and has been evicted", func() {
				var getHypervisorsCalled int

				BeforeEach(func(ctx SpecContext) {
					getHypervisorsCalled = 0

					By("Setting hypervisor to ready and evicted")
					hv := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, resourceName, hv)).To(Succeed())
					meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
						Type:    kvmv1.ConditionTypeReady,
						Status:  metav1.ConditionTrue,
						Reason:  "dontcare",
						Message: "dontcare",
					})
					meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
						Type:    kvmv1.ConditionTypeEvicting,
						Status:  metav1.ConditionFalse,
						Reason:  "dontcare",
						Message: "dontcare",
					})
					Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

					By("Mocking OpenStack API endpoints")
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						getHypervisorsCalled++
						Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "some reason", hypervisorName)).ToNot(BeNil())
					})

					// c48f6247-abe4-4a24-824e-ea39e108874f comes from the HypervisorWithServers const
					fakeServer.Mux.HandleFunc("GET /resource_providers/c48f6247-abe4-4a24-824e-ea39e108874f", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprint(w, `{"uuid": "rp-uuid", "name": "hv-test"}`)
						Expect(err).NotTo(HaveOccurred())
					})

					fakeServer.Mux.HandleFunc("GET /resource_providers/rp-uuid/allocations", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprint(w, `{"allocations": {}}}`)
						Expect(err).NotTo(HaveOccurred())
					})

					fakeServer.Mux.HandleFunc("DELETE /resource_providers/rp-uuid", func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusAccepted)
					})

					fakeServer.Mux.HandleFunc("DELETE /os-services/service-1234", func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusNoContent)
					})
				})

				It("should set the hypervisor ready condition", func(ctx SpecContext) {
					_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
					Expect(err).NotTo(HaveOccurred())

					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
					Expect(hypervisor.Status.Conditions).To(ContainElement(
						SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeReady),
							HaveField("Status", metav1.ConditionFalse),
							HaveField("Reason", "Decommissioning"),
						),
					))
				})

				It("should set the hypervisor offboarded condition", func(ctx SpecContext) {
					for range 3 {
						_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
						Expect(err).NotTo(HaveOccurred())
					}
					Expect(getHypervisorsCalled).To(BeNumerically(">", 0))

					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
					Expect(hypervisor.Status.Conditions).To(ContainElement(
						SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeOffboarded),
							HaveField("Status", metav1.ConditionTrue),
						),
					))
				})

				It("should clear Status.Aggregates when removing from all aggregates", func(ctx SpecContext) {
					By("Setting initial aggregates and IDs in status")
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
					hypervisor.Status.Aggregates = []string{"zone-a", "test-aggregate"}
					hypervisor.Status.ServiceID = serviceId
					hypervisor.Status.HypervisorID = "c48f6247-abe4-4a24-824e-ea39e108874f"
					Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

					By("Reconciling - decommission controller will wait for aggregates to be cleared")
					_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
					Expect(err).NotTo(HaveOccurred())

					By("Simulating aggregates controller clearing aggregates")
					Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
					hypervisor.Status.Aggregates = []string{}
					hypervisor.Status.AggregateUUIDs = []string{}
					Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

					By("Reconciling again after aggregates are cleared")
					for range 3 {
						_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
						Expect(err).NotTo(HaveOccurred())
					}

					By("Verifying Status.Aggregates is empty")
					Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
					Expect(hypervisor.Status.Aggregates).To(BeEmpty(), "Status.Aggregates should be cleared after decommissioning")
				})
			})
		})
	})

	Context("Guard Conditions", func() {
		JustBeforeEach(func(ctx SpecContext) {
			result, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		Context("When lifecycle is not enabled, but maintenance is set to termination", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
				hypervisor.Spec.LifecycleEnabled = false
				Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
			})

			It("should not reconcile", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Conditions).To(BeEmpty())
			})
		})

		Context("When maintenance is not set to termination", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				hypervisor.Spec.Maintenance = kvmv1.MaintenanceManual
				Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
			})

			It("should not reconcile", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Conditions).To(BeEmpty())
			})
		})
	})

	Context("Failure Modes", func() {
		var sharedDecommissioningErrorCheck = func(ctx SpecContext, expectedMessageSubstring string) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
			Expect(hypervisor.Status.Conditions).To(ContainElement(
				SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeReady),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", "Decommissioning"),
					HaveField("Message", ContainSubstring(expectedMessageSubstring)),
				),
			))
		}

		BeforeEach(func(ctx SpecContext) {
			By("Setting hypervisor to maintenance termination mode")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
			hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
		})

		Context("When getting hypervisor by name fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should set decommissioning condition with error", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "")
			})
		})

		Context("When hypervisor still has running VMs", func() {
			BeforeEach(func() {
				hvResponse := `{
					"hypervisors": [{
						"id": "c48f6247-abe4-4a24-824e-ea39e108874f",
						"hypervisor_hostname": "node-test",
						"hypervisor_version": 2002000,
						"state": "up",
						"status": "enabled",
						"running_vms": 2,
						"service": {
							"id": "service-1234",
							"host": "node-test",
							"disabled_reason": ""
						}
					}]
				}`

				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, hvResponse)
				})
			})

			It("should set decommissioning condition about running VMs", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "still has 2 running VMs")
			})
		})

		Context("When aggregates are not empty", func() {
			BeforeEach(func(ctx SpecContext) {
				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, `{
						"hypervisors": [{
							"id": "c48f6247-abe4-4a24-824e-ea39e108874f",
							"hypervisor_hostname": "node-test",
							"hypervisor_version": 2002000,
							"state": "up",
							"status": "enabled",
							"running_vms": 0,
							"service": {
								"id": "service-1234",
								"host": "node-test"
							}
						}]
					}`)
				})

				// Simulate aggregates controller hasn't cleared aggregates yet
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())
				hypervisor.Status.Aggregates = []string{"zone-a", "test-aggregate"}
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			It("should wait for aggregates to be cleared", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "Waiting for aggregates to be removed")
			})
		})

		Context("When deleting service fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, `{
						"hypervisors": [{
							"id": "c48f6247-abe4-4a24-824e-ea39e108874f",
							"hypervisor_hostname": "node-test",
							"hypervisor_version": 2002000,
							"state": "up",
							"status": "enabled",
							"running_vms": 0,
							"service": {
								"id": "service-1234",
								"host": "node-test"
							}
						}]
					}`)
				})

				fakeServer.Mux.HandleFunc("DELETE /os-services/service-1234", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should set decommissioning condition with error", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "cannot delete service")
			})
		})

		Context("When getting resource provider fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, `{
						"hypervisors": [{
							"id": "c48f6247-abe4-4a24-824e-ea39e108874f",
							"hypervisor_hostname": "node-test",
							"hypervisor_version": 2002000,
							"state": "up",
							"status": "enabled",
							"running_vms": 0,
							"service": {
								"id": "service-1234",
								"host": "node-test"
							}
						}]
					}`)
				})

				fakeServer.Mux.HandleFunc("DELETE /os-services/service-1234", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})

				fakeServer.Mux.HandleFunc("GET /resource_providers/c48f6247-abe4-4a24-824e-ea39e108874f", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should set decommissioning condition with error", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "cannot get resource provider")
			})
		})

		Context("When cleaning up resource provider fails", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, `{
						"hypervisors": [{
							"id": "c48f6247-abe4-4a24-824e-ea39e108874f",
							"hypervisor_hostname": "node-test",
							"hypervisor_version": 2002000,
							"state": "up",
							"status": "enabled",
							"running_vms": 0,
							"service": {
								"id": "service-1234",
								"host": "node-test"
							}
						}]
					}`)
				})

				fakeServer.Mux.HandleFunc("DELETE /os-services/service-1234", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})

				fakeServer.Mux.HandleFunc("GET /resource_providers/c48f6247-abe4-4a24-824e-ea39e108874f", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"uuid": "rp-uuid", "name": "hv-test"}`)
				})

				fakeServer.Mux.HandleFunc("GET /resource_providers/rp-uuid/allocations", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"allocations": {}}`)
				})

				fakeServer.Mux.HandleFunc("DELETE /resource_providers/rp-uuid", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should set decommissioning condition with error", func(ctx SpecContext) {
				_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				sharedDecommissioningErrorCheck(ctx, "cannot clean up resource provider")
			})
		})
	})
})
