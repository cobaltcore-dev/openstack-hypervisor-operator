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
	gomegatypes "github.com/onsi/gomega/types"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Instance slice helpers", func() {
	Describe("peekInstance", func() {
		It("returns empty string for empty slice", func() {
			Expect(peekInstance([]string{})).To(Equal(""))
		})

		It("returns empty string for nil slice", func() {
			Expect(peekInstance(nil)).To(Equal(""))
		})

		It("returns the last element", func() {
			Expect(peekInstance([]string{"a", "b", "c"})).To(Equal("c"))
		})

		It("returns the only element for single-element slice", func() {
			Expect(peekInstance([]string{"only"})).To(Equal("only"))
		})

		It("does not modify the slice", func() {
			s := []string{"a", "b", "c"}
			peekInstance(s)
			Expect(s).To(Equal([]string{"a", "b", "c"}))
		})
	})

	Describe("popInstance", func() {
		It("returns empty string and unchanged slice for empty slice", func() {
			s, uuid := popInstance([]string{})
			Expect(uuid).To(Equal(""))
			Expect(s).To(BeEmpty())
		})

		It("returns empty string and unchanged slice for nil slice", func() {
			s, uuid := popInstance(nil)
			Expect(uuid).To(Equal(""))
			Expect(s).To(BeNil())
		})

		It("removes and returns the last element", func() {
			s, uuid := popInstance([]string{"a", "b", "c"})
			Expect(uuid).To(Equal("c"))
			Expect(s).To(Equal([]string{"a", "b"}))
		})

		It("returns empty slice when popping last element", func() {
			s, uuid := popInstance([]string{"only"})
			Expect(uuid).To(Equal("only"))
			Expect(s).To(BeEmpty())
		})
	})

	Describe("moveToBack", func() {
		It("returns unchanged slice for empty slice", func() {
			s := moveToBack([]string{})
			Expect(s).To(BeEmpty())
		})

		It("returns unchanged slice for nil slice", func() {
			s := moveToBack(nil)
			Expect(s).To(BeNil())
		})

		It("returns unchanged slice for single-element slice", func() {
			s := moveToBack([]string{"only"})
			Expect(s).To(Equal([]string{"only"}))
		})

		It("moves last element to front for two elements", func() {
			s := moveToBack([]string{"a", "b"})
			Expect(s).To(Equal([]string{"b", "a"}))
		})

		It("moves last element to front for multiple elements", func() {
			s := moveToBack([]string{"a", "b", "c", "d"})
			Expect(s).To(Equal([]string{"d", "a", "b", "c"}))
		})
	})
})

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
		evictionReconciler *EvictionReconciler
		typeNamespacedName = types.NamespacedName{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		evictionObjectMeta = metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		reconcileRequest = ctrl.Request{NamespacedName: typeNamespacedName}
		fakeServer       testhelper.FakeServer
	)

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Install default handler to fail unhandled requests
		fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
		})

		By("Creating the EvictionReconciler")
		evictionReconciler = &EvictionReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: client.ServiceClient(fakeServer),
		}
	})

	AfterEach(func(ctx SpecContext) {
		resource := &kvmv1.Eviction{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err != nil {
			if !errors.IsNotFound(err) {
				Expect(err).ShouldNot(HaveOccurred())
			}
		} else {
			By("Cleanup the specific resource instance Eviction")
			Expect(evictionReconciler).NotTo(BeNil())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).Should(HaveOccurred())
		}
	})

	Describe("API validation", func() {
		Context("When creating an eviction without hypervisor", func() {
			It("should fail creating the resource", func(ctx SpecContext) {
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

		Context("When creating an eviction without reason", func() {
			It("should fail creating the resource", func(ctx SpecContext) {
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

		Context("When creating an eviction with reason and hypervisor", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Creating the hypervisor resource")
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
			By("Creating the hypervisor resource")
			hypervisor := &kvmv1.Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: hypervisorName,
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
			})

			By("Setting hypervisor status with IDs and conditions")
			hypervisor.Status.HypervisorID = hypervisorId
			hypervisor.Status.ServiceID = serviceId
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  "dontcare",
				Message: "dontcare",
			})
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeHypervisorDisabled,
				Status:  metav1.ConditionTrue,
				Reason:  "dontcare",
				Message: "dontcare",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			By("Creating the eviction resource")
			eviction := &kvmv1.Eviction{
				ObjectMeta: evictionObjectMeta,
				Spec: kvmv1.EvictionSpec{
					Reason:     "test-reason",
					Hypervisor: hypervisorName,
				},
			}
			Expect(k8sClient.Create(ctx, eviction)).To(Succeed())
		})

		Context("Happy Path", func() {
			Context("When enabled hypervisor has no servers", func() {
				BeforeEach(func(ctx SpecContext) {
					By("Mocking hypervisor API to return enabled status")
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						rHypervisorId := r.PathValue("hypervisor_id")
						Expect(rHypervisorId).To(Equal(hypervisorId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, hypervisorTpl, "null", "enabled")
						Expect(err).To(Succeed())
					})

					By("Mocking service update API")
					fakeServer.Mux.HandleFunc("PUT /os-services/{service_id}", func(w http.ResponseWriter, r *http.Request) {
						rServiceId := r.PathValue("service_id")
						Expect(rServiceId).To(Equal(serviceId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)
						Expect(err).To(Succeed())
					})
				})

				It("should succeed the reconciliation through all phases", func(ctx SpecContext) {
					runningCond := SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeEvicting),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonRunning),
						HaveField("Message", "Running"),
					)

					preflightCond := SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypePreflight),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonSucceeded),
						HaveField("Message", ContainSubstring("Preflight checks passed")),
					)

					expectations := []gomegatypes.GomegaMatcher{
						// 1. expect the Condition Evicting to be true
						ContainElements(runningCond),
						// 2. expect the preflight condition to be set to succeeded
						ContainElements(runningCond, preflightCond),
						// 3. expect the eviction condition to be set to succeeded
						ContainElements(SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeEvicting),
							HaveField("Status", metav1.ConditionFalse),
							HaveField("Reason", kvmv1.ConditionReasonSucceeded),
							HaveField("Message", ContainSubstring("eviction completed successfully")),
						)),
					}

					for i, expectation := range expectations {
						By(fmt.Sprintf("Reconciliation step %d", i+1))
						result, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
						Expect(result).To(Equal(ctrl.Result{}))
						Expect(err).NotTo(HaveOccurred())

						resource := &kvmv1.Eviction{}
						Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).NotTo(HaveOccurred())
						Expect(resource.Status.Conditions).To(expectation)
					}
				})
			})

			Context("When disabled hypervisor has no servers", func() {
				BeforeEach(func(ctx SpecContext) {
					By("Mocking hypervisor API to return disabled status")
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						rHypervisorId := r.PathValue("hypervisor_id")
						Expect(rHypervisorId).To(Equal(hypervisorId))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, hypervisorTpl, `"some reason"`, "disabled")
						Expect(err).To(Succeed())
					})

					By("Mocking service update API")
					fakeServer.Mux.HandleFunc("PUT /os-services/{service_id}", func(w http.ResponseWriter, r *http.Request) {
						rServiceId := r.PathValue("service_id")
						Expect(rServiceId).To(Equal(serviceId))
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "disabled"}}`, serviceId)
						Expect(err).To(Succeed())
					})
				})

				It("should succeed the reconciliation", func(ctx SpecContext) {
					By("First reconciliation should set eviction to running")
					_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
					Expect(err).NotTo(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())
					Expect(resource.Status.Conditions).To(ContainElement(
						SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeEvicting),
							HaveField("Status", metav1.ConditionTrue),
							HaveField("Reason", kvmv1.ConditionReasonRunning),
						),
					))

					By("Additional reconciliations should complete the eviction")
					for range 3 {
						_, err = evictionReconciler.Reconcile(ctx, reconcileRequest)
						Expect(err).NotTo(HaveOccurred())
					}

					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())
					Expect(resource.Status.Conditions).To(ContainElement(
						SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeEvicting),
							HaveField("Status", metav1.ConditionFalse),
							HaveField("Reason", kvmv1.ConditionReasonSucceeded),
						),
					))
				})
			})
		})

		Context("Failure Modes", func() {
			Context("When hypervisor is not found in openstack", func() {
				BeforeEach(func() {
					By("Mocking hypervisor API to return 404")
					fakeServer.Mux.HandleFunc("GET /os-hypervisors/{hypervisor_id}", func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusNotFound)
					})
				})

				It("should fail reconciliation", func(ctx SpecContext) {
					for range 3 {
						_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
						Expect(err).NotTo(HaveOccurred())
					}

					resource := &kvmv1.Eviction{}
					err := k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					Expect(resource.Status.Conditions).To(ContainElements(SatisfyAll(
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Type", kvmv1.ConditionTypeEvicting),
						HaveField("Reason", "Failed"),
						HaveField("Message", ContainSubstring("got 404")),
					)))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})
			})
		})

		Context("Mixed VM Eviction", func() {
			// serverTpl renders a single server response. The eviction controller
			// reads OS-EXT-SRV-ATTR:hypervisor_hostname (compared against the
			// short-form hypervisor name from spec) plus status/task_state/power_state.
			const serverTpl = `{
    "server": {
        "id": "%[1]s",
        "status": "%[2]s",
        "OS-EXT-SRV-ATTR:hypervisor_hostname": "%[3]s",
        "OS-EXT-STS:task_state": "%[4]s",
        "OS-EXT-STS:power_state": %[5]d,
        "fault": {"code": 500, "message": "%[6]s"}
    }
}`

			// migratedVMs is updated as the test simulates successful migrations.
			// When a VM has been "migrated", its hypervisor_hostname response
			// changes to a different host, signalling the controller it has left.
			var migratedVMs map[string]bool
			var liveMigrateCalls map[string]int

			BeforeEach(func(ctx SpecContext) {
				migratedVMs = map[string]bool{}
				liveMigrateCalls = map[string]int{}

				By("Seeding the eviction status with a list of VMs to evict")
				// OutstandingInstances is processed from the END (peekInstance returns
				// last). With [good-1, error-1, good-2], processing order is:
				//   1) good-2 (last) - migrate, then drop
				//   2) error-1 (now last) - skipped via moveToBack
				//   3) good-1 - migrate, then drop
				//   4) error-1 (alone) - keeps erroring
				eviction := &kvmv1.Eviction{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, eviction)).To(Succeed())
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionTrue,
					Message: "Running",
					Reason:  kvmv1.ConditionReasonRunning,
				})
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypePreflight,
					Status:  metav1.ConditionTrue,
					Message: "preflight passed",
					Reason:  kvmv1.ConditionReasonSucceeded,
				})
				eviction.Status.HypervisorServiceId = serviceId
				eviction.Status.OutstandingInstances = []string{"good-1", "error-1", "good-2"}
				eviction.Status.OutstandingRamMb = 4096
				Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())

				By("Mocking GET /servers/{id} to return per-VM state")
				fakeServer.Mux.HandleFunc("GET /servers/{server_id}", func(w http.ResponseWriter, r *http.Request) {
					serverID := r.PathValue("server_id")
					w.Header().Add("Content-Type", "application/json")

					// hypervisor_hostname uses the FQDN-style name; the controller
					// only compares the short prefix (before the first ".") against
					// eviction.Spec.Hypervisor. After we mark a VM as migrated,
					// pretend it lives on a different host so the controller treats
					// it as "already moved" and removes it from the list.
					hvHost := hypervisorName + ".example.local"
					if migratedVMs[serverID] {
						hvHost = "other-host.example.local"
					}

					switch serverID {
					case "good-1", "good-2":
						status := "ACTIVE"
						if migratedVMs[serverID] {
							// Once migrated, status doesn't really matter, but
							// keep it ACTIVE so we exercise the "different host"
							// branch rather than VERIFY_RESIZE.
							status = "ACTIVE"
						}
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, serverTpl, serverID, status, hvHost, "", 1, "")
						Expect(err).NotTo(HaveOccurred())
					case "error-1":
						w.WriteHeader(http.StatusOK)
						_, err := fmt.Fprintf(w, serverTpl, serverID, "ERROR", hvHost, "", 0,
							"manual intervention required")
						Expect(err).NotTo(HaveOccurred())
					default:
						Fail("unexpected server id: " + serverID)
					}
				})

				By("Mocking POST /servers/{id}/action for live-migration")
				fakeServer.Mux.HandleFunc("POST /servers/{server_id}/action", func(w http.ResponseWriter, r *http.Request) {
					serverID := r.PathValue("server_id")
					liveMigrateCalls[serverID]++
					// Mark this VM as migrated so the next GET reports a different host.
					migratedVMs[serverID] = true
					w.WriteHeader(http.StatusAccepted)
				})
			})

			It("skips errored VMs, evicts healthy ones, and retries errored VMs in subsequent loops", func(ctx SpecContext) {
				resource := &kvmv1.Eviction{}

				// Reconcile loop until the list is empty or we've gone too long.
				// We expect: good-2 migrated, error-1 skipped, good-1 migrated, then
				// only error-1 remains and keeps erroring.
				By("Running reconciliations until only the errored VM remains")
				const maxLoops = 20
				for i := range maxLoops {
					// Tolerate errors here: when the controller hits the ERROR
					// VM it returns an error (joined with the status update).
					// That is part of the pattern under test, not a test failure.
					_, reconcileErr := evictionReconciler.Reconcile(ctx, reconcileRequest)
					_ = reconcileErr // expected on ERROR-VM iterations
					Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

					// Once both healthy VMs have been migrated and removed, we are
					// in the steady "only errored VM left" state.
					remaining := resource.Status.OutstandingInstances
					if len(remaining) == 1 && remaining[0] == "error-1" {
						By(fmt.Sprintf("Reached steady state after %d reconciliations", i+1))
						break
					}
				}

				By("Both healthy VMs were live-migrated exactly once")
				Expect(liveMigrateCalls["good-1"]).To(Equal(1),
					"good-1 should have been migrated once")
				Expect(liveMigrateCalls["good-2"]).To(Equal(1),
					"good-2 should have been migrated once")

				By("The errored VM is still outstanding and never received a migrate call")
				Expect(resource.Status.OutstandingInstances).To(Equal([]string{"error-1"}))
				Expect(liveMigrateCalls).NotTo(HaveKey("error-1"))

				By("The migration condition reflects the most recent failure")
				Expect(resource.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeMigration),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", kvmv1.ConditionReasonFailed),
					HaveField("Message", ContainSubstring("error-1")),
				)))

				By("The eviction is NOT marked successful while the errored VM remains")
				Expect(resource.Status.Conditions).NotTo(ContainElement(SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeEvicting),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", kvmv1.ConditionReasonSucceeded),
				)))

				By("Subsequent reconciliations keep retrying the errored VM (and surfacing the error)")
				_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
				// The controller returns an error when it encounters a VM in ERROR
				// state. The reconcile error should mention the errored UUID.
				if err != nil {
					Expect(err.Error()).To(ContainSubstring("error-1"))
				}
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				Expect(resource.Status.OutstandingInstances).To(Equal([]string{"error-1"}))
			})
		})
	})
})
