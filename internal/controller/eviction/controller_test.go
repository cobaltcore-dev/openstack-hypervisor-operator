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

package eviction

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
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/global"
)

var _ = Describe("Instance slice helpers", func() {
	Describe("removeInstance", func() {
		It("returns unchanged slice for empty slice", func() {
			Expect(removeInstance([]string{}, "a")).To(BeEmpty())
		})

		It("returns nil for nil slice", func() {
			Expect(removeInstance(nil, "a")).To(BeNil())
		})

		It("removes the matching element preserving order", func() {
			Expect(removeInstance([]string{"a", "b", "c"}, "b")).To(Equal([]string{"a", "c"}))
		})

		It("removes the first element", func() {
			Expect(removeInstance([]string{"a", "b", "c"}, "a")).To(Equal([]string{"b", "c"}))
		})

		It("removes the last element", func() {
			Expect(removeInstance([]string{"a", "b", "c"}, "c")).To(Equal([]string{"a", "b"}))
		})

		It("removes only the first occurrence", func() {
			Expect(removeInstance([]string{"a", "b", "a"}, "a")).To(Equal([]string{"b", "a"}))
		})

		It("returns the slice unchanged when the uuid is absent", func() {
			Expect(removeInstance([]string{"a", "b"}, "z")).To(Equal([]string{"a", "b"}))
		})

		It("does not mutate the input backing array", func() {
			s := []string{"a", "b", "c"}
			_ = removeInstance(s, "b")
			Expect(s).To(Equal([]string{"a", "b", "c"}))
		})
	})

	Describe("deprioritize", func() {
		It("returns unchanged slice for empty slice", func() {
			Expect(deprioritize([]string{}, "a")).To(BeEmpty())
		})

		It("returns nil for nil slice", func() {
			Expect(deprioritize(nil, "a")).To(BeNil())
		})

		It("returns unchanged slice for single-element slice", func() {
			Expect(deprioritize([]string{"only"}, "only")).To(Equal([]string{"only"}))
		})

		It("is a no-op when the uuid is already at the back (index 0)", func() {
			Expect(deprioritize([]string{"a", "b", "c"}, "a")).To(Equal([]string{"a", "b", "c"}))
		})

		It("is a no-op when the uuid is absent", func() {
			Expect(deprioritize([]string{"a", "b", "c"}, "z")).To(Equal([]string{"a", "b", "c"}))
		})

		It("moves the tail (head-of-queue) element to the back", func() {
			// Queue is processed from the tail; "c" is next, deprioritizing it
			// puts it at index 0 (the back).
			Expect(deprioritize([]string{"a", "b", "c"}, "c")).To(Equal([]string{"c", "a", "b"}))
		})

		It("moves a specific middle element to the back", func() {
			Expect(deprioritize([]string{"a", "b", "c", "d"}, "c")).To(Equal([]string{"c", "a", "b", "d"}))
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
				// With the default concurrency of 1, each reconcile pass scans the
				// whole outstanding set but starts at most one new migration. The
				// ERROR VM is deprioritized (moved to the back of the queue) and
				// never migrated, while
				// the two healthy VMs migrate one after another and are removed as
				// soon as they report a different host.
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
					// Reconcile no longer returns an error for an ERROR-state VM
					// (it's recorded on the condition and retried via RequeueAfter),
					// so no error is expected here.
					_, reconcileErr := evictionReconciler.Reconcile(ctx, reconcileRequest)
					Expect(reconcileErr).NotTo(HaveOccurred())
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

				By("Subsequent reconciliations keep retrying the errored VM without surfacing a reconcile error")
				result, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
				// An ERROR-state instance is an expected, recoverable condition:
				// the controller records it on the MigratingInstance condition and
				// retries via RequeueAfter, rather than returning a reconcile error
				// (which would discard the RequeueAfter and spam warnings).
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				Expect(resource.Status.OutstandingInstances).To(Equal([]string{"error-1"}))
				By("The failure remains visible on the MigratingInstance condition")
				Expect(resource.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeMigration),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", kvmv1.ConditionReasonFailed),
					HaveField("Message", ContainSubstring("error-1")),
				)))
			})
		})

		Context("Parallel Eviction", func() {
			const serverTpl = `{
    "server": {
        "id": "%[1]s",
        "status": "%[2]s",
        "OS-EXT-SRV-ATTR:hypervisor_hostname": "%[3]s",
        "OS-EXT-STS:task_state": "",
        "OS-EXT-STS:power_state": 1
    }
}`

			var migrateCalls map[string]int
			// seedVMs seeds the eviction with n ACTIVE VMs already past preflight,
			// and installs the per-VM mock. A VM that has received a migrate call
			// reports a different host on the next GET, so it leaves the queue.
			seedVMs := func(ctx SpecContext, n int) []string {
				migrateCalls = map[string]int{}
				ids := make([]string, n)
				for i := range ids {
					ids[i] = fmt.Sprintf("vm-%d", i)
				}

				eviction := &kvmv1.Eviction{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, eviction)).To(Succeed())
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type: kvmv1.ConditionTypeEvicting, Status: metav1.ConditionTrue,
					Message: "Running", Reason: kvmv1.ConditionReasonRunning,
				})
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type: kvmv1.ConditionTypePreflight, Status: metav1.ConditionTrue,
					Message: "preflight passed", Reason: kvmv1.ConditionReasonSucceeded,
				})
				eviction.Status.HypervisorServiceId = serviceId
				eviction.Status.OutstandingInstances = append([]string(nil), ids...)
				Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())

				fakeServer.Mux.HandleFunc("GET /servers/{server_id}", func(w http.ResponseWriter, r *http.Request) {
					serverID := r.PathValue("server_id")
					hvHost := hypervisorName + ".example.local"
					if migrateCalls[serverID] > 0 {
						hvHost = "other-host.example.local"
					}
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprintf(w, serverTpl, serverID, "ACTIVE", hvHost)
					Expect(err).NotTo(HaveOccurred())
				})
				fakeServer.Mux.HandleFunc("POST /servers/{server_id}/action", func(w http.ResponseWriter, r *http.Request) {
					migrateCalls[r.PathValue("server_id")]++
					w.WriteHeader(http.StatusAccepted)
				})
				return ids
			}

			// inFlight counts VMs that have been told to migrate but have not yet
			// been observed leaving the host (i.e. still outstanding).
			inFlight := func(outstanding []string) int {
				n := 0
				for _, id := range outstanding {
					if migrateCalls[id] > 0 {
						n++
					}
				}
				return n
			}

			It("starts at most `limit` migrations per pass and never exceeds it in flight", func(ctx SpecContext) {
				orig := global.EvictionConcurrency
				global.EvictionConcurrency = 3
				DeferCleanup(func() { global.EvictionConcurrency = orig })

				seedVMs(ctx, 5)
				resource := &kvmv1.Eviction{}

				By("First pass triggers exactly the limit (3) migrations")
				_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
				Expect(err).NotTo(HaveOccurred())
				started := 0
				for _, c := range migrateCalls {
					started += c
				}
				Expect(started).To(Equal(3), "should start exactly limit migrations in the first pass")

				By("Draining to completion, never exceeding the limit in flight")
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				for range 20 {
					if len(resource.Status.OutstandingInstances) == 0 {
						break
					}
					Expect(inFlight(resource.Status.OutstandingInstances)).To(BeNumerically("<=", 3))
					_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
					Expect(err).NotTo(HaveOccurred())
					Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				}

				Expect(resource.Status.OutstandingInstances).To(BeEmpty())
				By("Every VM was migrated exactly once")
				for id, c := range migrateCalls {
					Expect(c).To(Equal(1), "VM %s migrated exactly once", id)
				}
				Expect(migrateCalls).To(HaveLen(5))
			})

			It("does not exceed the limit when a triggered migration has not yet flipped to MIGRATING status", func(ctx SpecContext) {
				// Regression: nova reports a freshly-triggered instance as ACTIVE
				// with task_state "migrating" for a while before Status becomes
				// MIGRATING. If those aren't counted as in-flight, the loop keeps
				// triggering and exceeds the concurrency limit.
				orig := global.EvictionConcurrency
				global.EvictionConcurrency = 2
				DeferCleanup(func() { global.EvictionConcurrency = orig })

				const tmpl = `{
    "server": {
        "id": "%[1]s",
        "status": "%[2]s",
        "OS-EXT-SRV-ATTR:hypervisor_hostname": "%[3]s",
        "OS-EXT-STS:task_state": "%[4]s",
        "OS-EXT-STS:power_state": 1
    }
}`
				calls := map[string]int{}
				// polls-since-trigger per VM; a migrated VM lingers ACTIVE with
				// task_state=migrating for 2 polls (as nova does), then leaves.
				pollsSince := map[string]int{}

				ids := make([]string, 5)
				for i := range ids {
					ids[i] = fmt.Sprintf("tsvm-%d", i)
				}
				eviction := &kvmv1.Eviction{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, eviction)).To(Succeed())
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type: kvmv1.ConditionTypeEvicting, Status: metav1.ConditionTrue,
					Message: "Running", Reason: kvmv1.ConditionReasonRunning,
				})
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type: kvmv1.ConditionTypePreflight, Status: metav1.ConditionTrue,
					Message: "preflight passed", Reason: kvmv1.ConditionReasonSucceeded,
				})
				eviction.Status.HypervisorServiceId = serviceId
				eviction.Status.OutstandingInstances = append([]string(nil), ids...)
				Expect(k8sClient.Status().Update(ctx, eviction)).To(Succeed())

				fakeServer.Mux.HandleFunc("GET /servers/{server_id}", func(w http.ResponseWriter, r *http.Request) {
					serverID := r.PathValue("server_id")
					host := hypervisorName + ".example.local"
					status, task := "ACTIVE", ""
					if calls[serverID] > 0 {
						pollsSince[serverID]++
						if pollsSince[serverID] <= 2 {
							// Still on the source host, ACTIVE, task_state migrating -
							// the window that used to be miscounted as a free slot.
							task = "migrating"
						} else {
							host = "other-host.example.local" // finally left
						}
					}
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprintf(w, tmpl, serverID, status, host, task)
					Expect(err).NotTo(HaveOccurred())
				})
				fakeServer.Mux.HandleFunc("POST /servers/{server_id}/action", func(w http.ResponseWriter, r *http.Request) {
					calls[r.PathValue("server_id")]++
					w.WriteHeader(http.StatusAccepted)
				})

				resource := &kvmv1.Eviction{}
				concurrentlyTriggered := func() int {
					// VMs triggered but not yet observed off-host.
					n := 0
					for id, c := range calls {
						if c > 0 && pollsSince[id] <= 2 {
							n++
						}
					}
					return n
				}

				By("Draining, asserting no more than `limit` are ever in flight")
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				for range 40 {
					if len(resource.Status.OutstandingInstances) == 0 {
						break
					}
					_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
					Expect(err).NotTo(HaveOccurred())
					Expect(concurrentlyTriggered()).To(BeNumerically("<=", 2),
						"in-flight migrations must never exceed the limit")
					Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				}

				Expect(resource.Status.OutstandingInstances).To(BeEmpty())
				By("Every VM migrated exactly once")
				for id, c := range calls {
					Expect(c).To(Equal(1), "VM %s migrated exactly once", id)
				}
				Expect(calls).To(HaveLen(5))
			})

			It("forces serial migration for hosts carrying the exclusive trait", func(ctx SpecContext) {
				orig := global.EvictionConcurrency
				origMap := global.EvictionTraitConcurrency
				global.EvictionConcurrency = 4
				global.EvictionTraitConcurrency = map[string]int{"CUSTOM_HANA_EXCLUSIVE_HOST": 1}
				DeferCleanup(func() {
					global.EvictionConcurrency = orig
					global.EvictionTraitConcurrency = origMap
				})

				By("Marking the hypervisor with the exclusive trait")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hypervisorName}, hv)).To(Succeed())
				hv.Status.Traits = []string{"CUSTOM_HANA_EXCLUSIVE_HOST"}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				seedVMs(ctx, 4)
				resource := &kvmv1.Eviction{}

				By("First pass triggers only ONE migration despite the higher global default")
				_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
				Expect(err).NotTo(HaveOccurred())
				started := 0
				for _, c := range migrateCalls {
					started += c
				}
				Expect(started).To(Equal(1), "exclusive-trait host must migrate serially")

				By("Never more than one in flight through to completion")
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				for range 30 {
					if len(resource.Status.OutstandingInstances) == 0 {
						break
					}
					Expect(inFlight(resource.Status.OutstandingInstances)).To(BeNumerically("<=", 1))
					_, err := evictionReconciler.Reconcile(ctx, reconcileRequest)
					Expect(err).NotTo(HaveOccurred())
					Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				}
				Expect(resource.Status.OutstandingInstances).To(BeEmpty())
				Expect(migrateCalls).To(HaveLen(4))
			})
		})
	})
})
