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
	"time"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	applyv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
)

var _ = Describe("HypervisorMaintenanceController", func() {
	var (
		controller         *HypervisorMaintenanceController
		fakeServer         testhelper.FakeServer
		hypervisorName     = types.NamespacedName{Name: "hv-test"}
		migrationsResponse string // mutable: tests can set this to control /os-migrations responses
		deletedMigrations  []string
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
			Expect(err).To(Succeed())
		})
	}

	// Setup and teardown
	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Default: no incoming migrations
		migrationsResponse = `{"migrations": []}`
		deletedMigrations = nil

		fakeServer.Mux.HandleFunc("GET /os-migrations", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			mt := r.URL.Query().Get("migration_type")
			// Only return migrations for the live-migration type query;
			// evacuation query returns empty by default.
			if mt == "evacuation" {
				fmt.Fprint(w, `{"migrations": []}`)
			} else {
				fmt.Fprint(w, migrationsResponse)
			}
		})

		fakeServer.Mux.HandleFunc("DELETE /servers/", func(w http.ResponseWriter, r *http.Request) {
			deletedMigrations = append(deletedMigrations, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		})

		By("Creating the HypervisorMaintenanceController")
		controller = &HypervisorMaintenanceController{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}

		By("Creating a blank Hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hypervisorName.Name,
				Namespace: hypervisorName.Namespace,
			},
			Spec: kvmv1.HypervisorSpec{},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			By("Deleting the Hypervisor resource")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})
	})

	// After the setup in JustBefore, we want to reconcile
	JustBeforeEach(func(ctx SpecContext) {
		req := ctrl.Request{NamespacedName: hypervisorName}
		_, err := controller.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func(ctx SpecContext) {
		eviction := &kvmv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hypervisorName.Name,
				Namespace: hypervisorName.Namespace,
			},
		}
		Expect(k8sclient.IgnoreNotFound(k8sClient.Delete(ctx, eviction))).To(Succeed())
	})

	// Tests
	Context("Onboarded Hypervisor", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Status.ServiceID = "1234"
			meta.SetStatusCondition(&hypervisor.Status.Conditions,
				metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  metav1.StatusSuccess,
					Message: "random text",
				},
			)
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		Describe("Enabling or Disabling the Nova Service", func() {
			Context("Spec.Maintenance=\"\"", func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = ""
					Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
					expectedBody := `{"status": "enabled", "forced_down": false}`
					mockServiceUpdate(expectedBody)
				})

				It("should set the ConditionTypeHypervisorDisabled to false", func(ctx SpecContext) {
					updated := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
					Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)).To(BeTrue())
				})
			}) // Spec.Maintenance=""
		})

		for _, mode := range []string{"auto", "manual"} {
			Context(fmt.Sprintf("Spec.Maintenance=\"%v\"", mode), func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = mode
					if mode == "manual" {
						hypervisor.Spec.MaintenanceReason = "Test maintenance reason"
					}
					Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
					expectedBody := fmt.Sprintf(`{"disabled_reason": "Hypervisor CRD: spec.maintenance=%v", "status": "disabled"}`, mode)
					mockServiceUpdate(expectedBody)
				})

				It("should set the ConditionTypeHypervisorDisabled to true", func(ctx SpecContext) {
					updated := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
					Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)).To(BeTrue())
				})
			}) // Spec.Maintenance="<mode>"
		}

		Context("Spec.Maintenance=\"ha\"", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				hypervisor.Spec.Maintenance = kvmv1.MaintenanceHA
				Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
			})

			It("should not call the Nova API (kvm-ha-service owns enable/disable for ha mode)", func(ctx SpecContext) {
				// The controller takes no action for ha mode; kvm-ha-service handles it.
				// Verify the hypervisor is still accessible and no condition is set by this controller.
				updated := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
				Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)).To(BeFalse())
			})
		}) // Spec.Maintenance="ha"

		Describe("Eviction reconciliation", func() {
			Context("Spec.Maintenance=\"\"", func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = ""
					Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					meta.SetStatusCondition(&hypervisor.Status.Conditions,
						metav1.Condition{
							Type:    kvmv1.ConditionTypeEvicting,
							Reason:  "dontcare",
							Status:  metav1.ConditionUnknown,
							Message: "dontcare",
						})
					Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
					expectedBody := `{"status": "enabled", "forced_down": false}`
					mockServiceUpdate(expectedBody)

					eviction := &kvmv1.Eviction{
						ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
						Spec: kvmv1.EvictionSpec{
							Hypervisor: hypervisorName.Name,
							Reason:     "test",
						},
					}
					Expect(controllerutil.SetControllerReference(hypervisor, eviction, controller.Scheme)).To(Succeed())
					Expect(k8sClient.Create(ctx, eviction)).To(Succeed())
				})

				It("should delete the created eviction", func(ctx SpecContext) {
					eviction := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, hypervisorName, eviction)
					By(fmt.Sprintf("%+v", *eviction))
					Expect(err).To(HaveOccurred())
					Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
				})
			}) // Spec.Maintenance=""

			Context("Spec.Maintenance=\"\" with only stale IncomingMigrationsSettled condition", func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

					// First, simulate that the controller previously owned the condition
					// by applying it via SSA with the same field manager.
					statusCfg := applyv1.HypervisorStatus().
						WithEvicted(false).
						WithConditions(applymetav1.Condition().
							WithType(kvmv1.ConditionTypeIncomingMigrationsSettled).
							WithStatus(metav1.ConditionFalse).
							WithReason(kvmv1.ConditionReasonWaiting).
							WithMessage("incoming migrations still pending").
							WithLastTransitionTime(metav1.Now()))
					Expect(k8sClient.Status().Apply(ctx,
						applyv1.Hypervisor(hypervisor.Name).WithStatus(statusCfg),
						k8sclient.ForceOwnership,
						k8sclient.FieldOwner(HypervisorMaintenanceControllerName),
					)).To(Succeed())

					// Now cancel maintenance.
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = ""
					Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
					expectedBody := `{"status": "enabled", "forced_down": false}`
					mockServiceUpdate(expectedBody)
				})

				It("should remove the stale IncomingMigrationsSettled condition", func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					Expect(meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeIncomingMigrationsSettled)).To(BeNil())
				})
			})

			Context("Spec.Maintenance=\"ha\"", func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					hypervisor.Spec.Maintenance = "ha"
					Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
				})
				It("should not create an eviction resource", func(ctx SpecContext) {
					eviction := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, hypervisorName, eviction)
					Expect(err).To(HaveOccurred())
					Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
				})
			}) // Spec.Maintenance="ha"

			for _, mode := range []string{"auto", "manual"} {
				Context(fmt.Sprintf("Spec.Maintenance=\"%v\"", mode), func() {
					BeforeEach(func(ctx SpecContext) {
						hypervisor := &kvmv1.Hypervisor{}
						Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
						hypervisor.Spec.Maintenance = mode
						if mode == "manual" {
							hypervisor.Spec.MaintenanceReason = "Test maintenance reason"
						}
						Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
						expectedBody := fmt.Sprintf(`{"disabled_reason": "Hypervisor CRD: spec.maintenance=%v", "status": "disabled"}`, mode)
						mockServiceUpdate(expectedBody)
					})

					When("there is no eviction yet", func() {
						It("should create an eviction resource named as the hypervisor", func(ctx SpecContext) {
							eviction := &kvmv1.Eviction{}
							Expect(k8sClient.Get(ctx, hypervisorName, eviction)).To(Succeed())
						})

						It("should create an evicting condition", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionTrue),
									HaveField("Reason", kvmv1.ConditionReasonRunning),
								),
							))
						})

						It("should reflect it in the hypervisor evicted status", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Evicted).To(BeFalse())
						})
					})

					When("there is an ongoing eviction", func() {
						BeforeEach(func(ctx SpecContext) {
							eviction := &kvmv1.Eviction{
								ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
								Spec: kvmv1.EvictionSpec{
									Hypervisor: hypervisorName.Name,
									Reason:     "test",
								},
							}
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(controllerutil.SetControllerReference(hypervisor, eviction, controller.Scheme)).To(Succeed())
							Expect(k8sClient.Create(ctx, eviction)).To(Succeed())

							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
								Type:    kvmv1.ConditionTypeEvicting,
								Status:  metav1.ConditionTrue,
								Message: "whatever",
								Reason:  kvmv1.ConditionReasonRunning,
							})
							Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())
						})

						It("should reflect it in the hypervisor evicting condition", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionTrue),
									HaveField("Reason", kvmv1.ConditionReasonRunning),
								),
							))
						})

						It("should reflect it in the hypervisor evicted status", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Evicted).To(BeFalse())
						})

						It("should set the ConditionTypeEvicting to true", func(ctx SpecContext) {
							updated := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
							Expect(updated.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionTrue),
								)))
						})
					})

					When("there is a finished eviction", func() {
						BeforeEach(func(ctx SpecContext) {
							eviction := &kvmv1.Eviction{
								ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
								Spec: kvmv1.EvictionSpec{
									Hypervisor: hypervisorName.Name,
									Reason:     "test",
								},
							}
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(controllerutil.SetControllerReference(hypervisor, eviction, controller.Scheme)).To(Succeed())
							Expect(k8sClient.Create(ctx, eviction)).To(Succeed())

							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
								Type:    kvmv1.ConditionTypeEvicting,
								Status:  metav1.ConditionFalse,
								Message: "whatever",
								Reason:  kvmv1.ConditionReasonSucceeded,
							})
							Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())
						})

						It("should reflect it in the hypervisor evicting condition", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionFalse),
									HaveField("Reason", kvmv1.ConditionReasonSucceeded),
								),
							))
						})

						It("should reflect it in the hypervisor evicted status", func(ctx SpecContext) {
							hypervisor := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
							Expect(hypervisor.Status.Evicted).To(BeTrue())
						})

						It("should set the ConditionTypeEvicting to false when evicted", func(ctx SpecContext) {
							updated := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
							Expect(updated.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionFalse),
								)))
						})

						It("should keep the evicting condition on a repeated reconcile (must not be pruned by SSA)", func(ctx SpecContext) {
							// Reconciling again with the succeeded state already
							// recorded on the Hypervisor takes the early-return
							// branch in reconcileEviction. The apply must still
							// seed the succeeded evicting condition — otherwise
							// SSA prunes it because this controller is its sole
							// owner.
							req := ctrl.Request{NamespacedName: hypervisorName}
							_, err := controller.Reconcile(ctx, req)
							Expect(err).NotTo(HaveOccurred())

							updated := &kvmv1.Hypervisor{}
							Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
							Expect(updated.Status.Conditions).To(ContainElement(
								SatisfyAll(
									HaveField("Type", kvmv1.ConditionTypeEvicting),
									HaveField("Status", metav1.ConditionFalse),
									HaveField("Reason", kvmv1.ConditionReasonSucceeded),
								),
							))
							Expect(updated.Status.Evicted).To(BeTrue())
						})
					})
				}) // Spec.Maintenance="<mode>"
			}
		})
	}) // Context Onboarded Hypervisor

	Context("Non-existent Hypervisor", func() {
		It("should handle gracefully with IgnoreNotFound", func(ctx SpecContext) {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "non-existent-hv"}}
			_, err := controller.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Hypervisor still onboarding", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions,
				metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  "InProgress",
					Message: "Still onboarding",
				},
			)
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should return early without error", func(ctx SpecContext) {
			// JustBeforeEach already called Reconcile, just verify no errors occurred
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			// Verify onboarding condition is still true (not modified)
			Expect(meta.IsStatusConditionTrue(hypervisor.Status.Conditions, kvmv1.ConditionTypeOnboarding)).To(BeTrue())
		})
	})

	Context("Hypervisor with empty ServiceID", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Status.ServiceID = ""
			hypervisor.Spec.Maintenance = "auto"
			meta.SetStatusCondition(&hypervisor.Status.Conditions,
				metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  metav1.StatusSuccess,
					Message: "Onboarded",
				},
			)
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should skip reconcileComputeService without error", func(ctx SpecContext) {
			// JustBeforeEach already called Reconcile
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			// ServiceID should still be empty
			Expect(hypervisor.Status.ServiceID).To(BeEmpty())
		})
	})

	Context("Incoming migrations settling", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Status.ServiceID = "1234"
			meta.SetStatusCondition(&hypervisor.Status.Conditions,
				metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  metav1.StatusSuccess,
					Message: "Onboarded",
				},
			)
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			// Re-read to get fresh resourceVersion, then update spec
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Maintenance = "auto"
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Permissive service mock: accept any enable/disable call
			fakeServer.Mux.HandleFunc("PUT /os-services/1234", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, ServiceEnabledResponse)
			})
		})

		Context("when there are no incoming migrations", func() {
			It("should set IncomingMigrationsSettled=True and proceed to create eviction", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(meta.IsStatusConditionTrue(hypervisor.Status.Conditions, kvmv1.ConditionTypeIncomingMigrationsSettled)).To(BeTrue())
				// Eviction should be created
				eviction := &kvmv1.Eviction{}
				Expect(k8sClient.Get(ctx, hypervisorName, eviction)).To(Succeed())
			})
		})

		Context("when there is a running incoming migration", func() {
			BeforeEach(func(_ SpecContext) {
				migrationsResponse = `{"migrations": [
					{
						"id": 42,
						"uuid": "mig-uuid-1",
						"instance_uuid": "inst-uuid-1",
						"status": "running",
						"source_compute": "node003",
						"dest_compute": "hv-test",
						"migration_type": "live-migration"
					}
				]}`
			})

			It("should set IncomingMigrationsSettled=False with Aborting reason", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				cond := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeIncomingMigrationsSettled)
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonAborting))
			})

			It("should issue a DELETE to abort the migration", func(_ SpecContext) {
				Expect(deletedMigrations).To(HaveLen(1))
				Expect(deletedMigrations[0]).To(ContainSubstring("inst-uuid-1"))
				Expect(deletedMigrations[0]).To(ContainSubstring("42"))
			})

			It("should not create an eviction resource", func(ctx SpecContext) {
				eviction := &kvmv1.Eviction{}
				err := k8sClient.Get(ctx, hypervisorName, eviction)
				Expect(err).To(HaveOccurred())
				Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
			})

			It("should requeue after settleRequeueInterval", func(ctx SpecContext) {
				req := ctrl.Request{NamespacedName: hypervisorName}
				result, err := controller.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			})
		})

		Context("when there is a post-migrating incoming migration", func() {
			BeforeEach(func(_ SpecContext) {
				migrationsResponse = `{"migrations": [
					{
						"id": 99,
						"uuid": "mig-uuid-2",
						"instance_uuid": "inst-uuid-2",
						"status": "post-migrating",
						"source_compute": "node003",
						"dest_compute": "hv-test",
						"migration_type": "live-migration"
					}
				]}`
			})

			It("should set IncomingMigrationsSettled=False with Waiting reason", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				cond := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeIncomingMigrationsSettled)
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonWaiting))
			})

			It("should not issue any DELETE", func(_ SpecContext) {
				Expect(deletedMigrations).To(BeEmpty())
			})

			It("should not create an eviction resource", func(ctx SpecContext) {
				eviction := &kvmv1.Eviction{}
				err := k8sClient.Get(ctx, hypervisorName, eviction)
				Expect(err).To(HaveOccurred())
				Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
			})

			It("should requeue after settleRequeueInterval", func(ctx SpecContext) {
				req := ctrl.Request{NamespacedName: hypervisorName}
				result, err := controller.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			})
		})

		Context("when eviction succeeded but instances remain (incident regression)", func() {
			BeforeEach(func(ctx SpecContext) {
				// Simulate: eviction CR exists and reports Succeeded
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

				eviction := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
					Spec: kvmv1.EvictionSpec{
						Hypervisor: hypervisorName.Name,
						Reason:     "test",
					},
				}
				Expect(controllerutil.SetControllerReference(hypervisor, eviction, controller.Scheme)).To(Succeed())
				Expect(k8sClient.Create(ctx, eviction)).To(Succeed())

				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Message: "done",
					Reason:  kvmv1.ConditionReasonSucceeded,
				})
				Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())

				// Set NumInstances=1 on the Hypervisor (simulating late-arriving instance)
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				hypervisor.Status.NumInstances = 1
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Evicted",
				})
				hypervisor.Status.Evicted = true
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			It("should flip Evicted back to false", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Evicted).To(BeFalse())
			})

			It("should set Evicting back to Running", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				cond := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeEvicting)
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonRunning))
			})

			It("should delete the eviction CR to re-enter drain", func(ctx SpecContext) {
				eviction := &kvmv1.Eviction{}
				err := k8sClient.Get(ctx, hypervisorName, eviction)
				Expect(err).To(HaveOccurred())
				Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
			})
		})

		Context("when eviction just finished but instances remain (no pre-existing Succeeded condition)", func() {
			BeforeEach(func(ctx SpecContext) {
				// Simulate: eviction CR exists and reports finished, but the
				// Hypervisor does NOT have Evicting=Succeeded yet (first reconcile
				// after the Eviction CR transitioned).
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

				eviction := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
					Spec: kvmv1.EvictionSpec{
						Hypervisor: hypervisorName.Name,
						Reason:     "test",
					},
				}
				Expect(controllerutil.SetControllerReference(hypervisor, eviction, controller.Scheme)).To(Succeed())
				Expect(k8sClient.Create(ctx, eviction)).To(Succeed())

				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Message: "done",
					Reason:  kvmv1.ConditionReasonSucceeded,
				})
				Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())

				// Set NumInstances=1 but leave Evicting condition as Running (not Succeeded)
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				hypervisor.Status.NumInstances = 1
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonRunning,
					Message: "Evicting",
				})
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			It("should set Evicting to True/Running and not declare Evicted", func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Evicted).To(BeFalse())
				cond := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeEvicting)
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonRunning))
			})

			It("should delete the eviction CR to restart drain", func(ctx SpecContext) {
				eviction := &kvmv1.Eviction{}
				err := k8sClient.Get(ctx, hypervisorName, eviction)
				Expect(err).To(HaveOccurred())
				Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
			})
		})
	})
})
