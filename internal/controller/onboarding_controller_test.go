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
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

const (
	HypervisorWithServers = `{
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
)

// hypervisorRecordJSON builds a single GET /os-hypervisors/detail record with
// a parameterized identity, service liveness and service status, for
// identity-resolution scenarios.
func hypervisorRecordJSON(hypervisorID, serviceID, state, status, hostname string) string {
	return fmt.Sprintf(`{
            "service": {
                "host": %q,
                "id": "%s",
                "disabled_reason": ""
            },
            "cpu_info": {
                "arch": "x86_64",
                "model": "Nehalem",
                "vendor": "Intel",
                "features": ["pge", "clflush"],
                "topology": {"cores": 1, "threads": 1, "sockets": 4}
            },
            "current_workload": 0,
            "status": "%s",
            "state": "%s",
            "disk_available_least": 0,
            "host_ip": "1.1.1.1",
            "free_disk_gb": 1028,
            "free_ram_mb": 7680,
            "hypervisor_hostname": %q,
            "hypervisor_type": "fake",
            "hypervisor_version": 2002000,
            "id": "%s",
            "local_gb": 1028,
            "local_gb_used": 0,
            "memory_mb": 8192,
            "memory_mb_used": 512,
            "running_vms": 0,
            "vcpus": 1,
            "vcpus_used": 0
        }`, hostname, serviceID, status, state, hostname, hypervisorID)
}

// hypervisorListJSON wraps records into a GET /os-hypervisors/detail response.
func hypervisorListJSON(records ...string) string {
	return `{"hypervisors": [` + strings.Join(records, ",") + `]}`
}

var _ = Describe("Onboarding Controller", func() {
	const (
		region           = "test-region"
		availabilityZone = "test-az"
		hypervisorName   = "test-host"
		serviceId        = "service-id"
		hypervisorId     = "c48f6247-abe4-4a24-824e-ea39e108874f"

		flavorDetailsBody = `{
    "flavors": [
        {
            "OS-FLV-DISABLED:disabled": false,
            "disk": 0,
            "OS-FLV-EXT-DATA:ephemeral": 0,
            "os-flavor-access:is_public": true,
            "id": "1",
            "links": [],
            "name": "c_k_c2_m2_v2",
            "ram": 2048,
            "swap": 0,
            "vcpus": 2,
            "rxtx_factor": 1.0,
            "description": null,
            "extra_specs": {}
        }
	]
}`
		imagesBody = `{
    "images": [
        {
            "status": "active",
            "name": "cirros-kvm",
            "tags": [],
            "container_format": "bare",
            "created_at": "2014-11-07T17:07:06Z",
            "disk_format": "qcow2",
            "updated_at": "2014-11-07T17:19:09Z",
            "visibility": "public",
            "self": "/v2/images/1bea47ed-f6a9-463b-b423-14b9cca9ad27",
            "min_disk": 0,
            "protected": false,
            "id": "1bea47ed-f6a9-463b-b423-14b9cca9ad27",
            "file": "/v2/images/1bea47ed-f6a9-463b-b423-14b9cca9ad27/file",
            "checksum": "64d7c1cd2b6f60c92c14662941cb7913",
            "os_hash_algo": "sha512",
            "os_hash_value": "...",
            "os_hidden": false,
            "owner": "5ef70662f8b34079a6eddb8da9d75fe8",
            "size": 13167616,
            "min_ram": 0,
            "schema": "/v2/schemas/image",
            "virtual_size": null
        }
	]
}`

		networksBody = `{
    "networks": [
        {
            "admin_state_up": true,
            "id": "network-id",
            "name": "net1",
            "provider:network_type": "vlan",
            "provider:physical_network": "physnet1",
            "provider:segmentation_id": 1000,
            "router:external": false,
            "shared": false,
            "status": "ACTIVE",
            "subnets": [],
            "tenant_id": "project-id",
            "project_id": "project-id"
        }
    ]
}`

		emptyServersBody = `{"servers": [], "servers_links": []}`

		// The body actually doesn't return a status, but it simplifies the test
		createServerBody = `{
    "server": {
        "id": "server-id",
		"status": "ACTIVE"
    }
}`
	)

	var (
		onboardingReconciler *OnboardingController
		namespacedName       = types.NamespacedName{Name: hypervisorName}
		fakeServer           testhelper.FakeServer
		reconcileReq         = ctrl.Request{NamespacedName: namespacedName}
		reconcileLoop        = func(ctx SpecContext, steps int) (err error) {
			for range steps {
				_, err = onboardingReconciler.Reconcile(ctx, reconcileReq)
				if err != nil {
					return
				}
			}
			return
		}
	)

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the OpenStack http mock server")
		fakeServer = testhelper.SetupHTTP()
		DeferCleanup(fakeServer.Teardown)

		// Install default handler to fail unhandled requests
		fakeServer.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			Fail("Unhandled request to fake server: " + r.Method + " " + r.URL.Path)
		})

		os.Setenv("KVM_HA_SERVICE_URL", fakeServer.Endpoint()+"instance-ha")
		DeferCleanup(func() {
			os.Unsetenv("KVM_HA_SERVICE_URL")
		})

		By("creating the resource for the Kind Hypervisor")
		hv := &kvmv1.Hypervisor{
			Name: hypervisorName,
			Labels: map[string]string{
				corev1.LabelTopologyRegion: region,
				corev1.LabelTopologyZone:   availabilityZone,
				corev1.LabelHostname:       hypervisorName,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hv)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hv)).To(Succeed())
		})

		onboardingReconciler = &OnboardingController{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			TestFlavorID:      "1",
			Clock:             clock.RealClock{},
			computeClient:     client.ServiceClient(fakeServer),
			testComputeClient: client.ServiceClient(fakeServer),
			testImageClient:   client.ServiceClient(fakeServer),
			testNetworkClient: client.ServiceClient(fakeServer),
		}

		DeferCleanup(func() {
			onboardingReconciler = nil
		})
	})

	Context("aborting onboarding", func() {
		DescribeTable("preserves successful onboarding without OpenStack cleanup",
			func(ctx SpecContext, maintenance string, lifecycleEnabled bool) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Spec.Maintenance = maintenance
				hv.Spec.LifecycleEnabled = lifecycleEnabled
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:               kvmv1.ConditionTypeOnboarding,
					Status:             metav1.ConditionFalse,
					Reason:             kvmv1.ConditionReasonSucceeded,
					Message:            "Onboarding completed",
					LastTransitionTime: metav1.NewTime(time.Date(2026, time.September, 21, 12, 59, 6, 0, time.UTC)),
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
				originalStatus := hv.Status.DeepCopy()
				originalResourceVersion := hv.ResourceVersion

				// The default mock handler fails any OpenStack request, including cleanup.
				Expect(reconcileLoop(ctx, 2)).To(Succeed())
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status).To(Equal(*originalStatus))
				Expect(hv.ResourceVersion).To(Equal(originalResourceVersion))
			},
			Entry("when termination begins", kvmv1.MaintenanceTermination, true),
			Entry("when lifecycle management is disabled", kvmv1.MaintenanceUnset, false),
		)

		DescribeTable("cleans up test servers and aborts incomplete onboarding",
			func(ctx SpecContext, reason, maintenance string, lifecycleEnabled bool, message string) {
				listRequests, deleteRequests := 0, 0
				fakeServer.Mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
					listRequests++
					Expect(r.URL.Query().Get("name")).To(Equal(testPrefixName + "-" + hypervisorName))
					w.Header().Set("Content-Type", "application/json")
					_, err := fmt.Fprint(w, `{"servers": [{"id": "test-server-id", "name": "ohooc-test-host"}]}`)
					Expect(err).NotTo(HaveOccurred())
				})
				fakeServer.Mux.HandleFunc("DELETE /servers/test-server-id", func(w http.ResponseWriter, _ *http.Request) {
					deleteRequests++
					w.WriteHeader(http.StatusAccepted)
				})

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Spec.Maintenance = maintenance
				hv.Spec.LifecycleEnabled = lifecycleEnabled
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionTrue,
					Reason:  reason,
					Message: "Onboarding in progress",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				Expect(reconcileLoop(ctx, 2)).To(Succeed())
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonAborted))
				Expect(condition.Message).To(Equal(message))
				Expect(listRequests).To(Equal(1))
				Expect(deleteRequests).To(Equal(1))
			},
			Entry("Initial during termination", kvmv1.ConditionReasonInitial, kvmv1.MaintenanceTermination, true, "Aborted due to MaintenanceTermination"),
			Entry("Testing during termination", kvmv1.ConditionReasonTesting, kvmv1.MaintenanceTermination, true, "Aborted due to MaintenanceTermination"),
			Entry("Handover during termination", kvmv1.ConditionReasonHandover, kvmv1.MaintenanceTermination, true, "Aborted due to MaintenanceTermination"),
			Entry("Initial with lifecycle disabled", kvmv1.ConditionReasonInitial, kvmv1.MaintenanceUnset, false, "Aborted due to LifecycleEnabled being false"),
			Entry("Testing with lifecycle disabled", kvmv1.ConditionReasonTesting, kvmv1.MaintenanceUnset, false, "Aborted due to LifecycleEnabled being false"),
			Entry("Handover with lifecycle disabled", kvmv1.ConditionReasonHandover, kvmv1.MaintenanceUnset, false, "Aborted due to LifecycleEnabled being false"),
		)
	})

	Context("initial setup of a new hypervisor", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "", hypervisorName)).ToNot(BeNil())
			})

			fakeServer.Mux.HandleFunc("PUT /os-services/service-id", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "enabled"}}`, serviceId)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		When("it is a clean setup", func() {
			It("should set the Service- and HypervisorId from Nova", func(ctx SpecContext) {
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.ServiceID).To(Equal(serviceId))
				Expect(hv.Status.HypervisorID).To(Equal(hypervisorId))
			})

			It("should update the status accordingly", func(ctx SpecContext) {
				By("Reconciling the created resource to set IDs")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating aggregates controller setting aggregates")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
					{Name: testAggregateName, UUID: "uuid-" + testAggregateName},
				}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Reconciling again to process onboarding")
				err = reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonTesting),
					),
				))
			})
		})

		When("it the host is already in an unexpected aggregate", func() {
			It("should set the Service- and HypervisorId from Nova", func(ctx SpecContext) {
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.ServiceID).To(Equal(serviceId))
				Expect(hv.Status.HypervisorID).To(Equal(hypervisorId))
			})

			It("should update the status accordingly", func(ctx SpecContext) {
				By("Reconciling the created resource to set IDs")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating aggregates controller setting aggregates")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
					{Name: testAggregateName, UUID: "uuid-" + testAggregateName},
				}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Reconciling again to process onboarding")
				err = reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonTesting),
					),
				))
			})
		})
	})

	Context("Nova identity resolution", func() {
		var (
			hypervisorListBody string
			listCalls          int
		)

		BeforeEach(func() {
			listCalls = 0
			hypervisorListBody = hypervisorListJSON(hypervisorRecordJSON(hypervisorId, serviceId, "up", "enabled", hypervisorName))
			fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
				listCalls++
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, hypervisorListBody)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		setStatus := func(ctx SpecContext, hypervisorID, serviceID, reason string) {
			hv := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
			hv.Status.HypervisorID = hypervisorID
			hv.Status.ServiceID = serviceID
			if reason != "" {
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:   kvmv1.ConditionTypeOnboarding,
					Status: metav1.ConditionTrue,
					Reason: reason,
				})
			}
			Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
		}

		When("the only compute record has no live service", func() {
			BeforeEach(func() {
				// Leftover of a replaced node: the agent is gone, so the
				// record must not be adopted even though it is the only match.
				hypervisorListBody = hypervisorListJSON(hypervisorRecordJSON("hv-old", "service-old", "down", "enabled", hypervisorName))
			})

			It("waits instead of adopting the stale record", func(ctx SpecContext) {
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.ServiceID).To(BeEmpty())
				Expect(hv.Status.HypervisorID).To(BeEmpty())
				Expect(meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)).To(BeNil())
			})
		})

		When("the stored identity points at a replaced compute service", func() {
			var enableNewCalls, enableOldCalls int

			BeforeEach(func(ctx SpecContext) {
				setStatus(ctx, "hv-old", "service-old", kvmv1.ConditionReasonTesting)

				// The replacement agent registered a new service; with
				// compute/disable_new_services it starts disabled.
				hypervisorListBody = hypervisorListJSON(hypervisorRecordJSON("hv-new", "service-new", "up", "disabled", hypervisorName))

				fakeServer.Mux.HandleFunc("PUT /os-services/service-new", func(w http.ResponseWriter, r *http.Request) {
					enableNewCalls++
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, `{"service": {"id": "service-new", "status": "enabled"}}`)
					Expect(err).NotTo(HaveOccurred())
				})
				fakeServer.Mux.HandleFunc("PUT /os-services/service-old", func(w http.ResponseWriter, r *http.Request) {
					enableOldCalls++
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, `{"service": {"id": "service-old", "status": "enabled"}}`)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("repairs the identity and enables the current service", func(ctx SpecContext) {
				By("Repairing the stale identity and restarting at the enable step")
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.HypervisorID).To(Equal("hv-new"))
				Expect(hv.Status.ServiceID).To(Equal("service-new"))
				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonInitial))
				Expect(condition.Message).To(ContainSubstring("identity changed"))

				By("Running the enable step against the new service once aggregates are in place")
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
					{Name: testAggregateName, UUID: "uuid-" + testAggregateName},
				}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				_, err = onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())

				Expect(enableNewCalls).To(Equal(1))
				Expect(enableOldCalls).To(BeZero())

				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				condition = meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonTesting))
			})
		})

		When("the current compute service is disabled during testing", func() {
			BeforeEach(func(ctx SpecContext) {
				setStatus(ctx, hypervisorId, serviceId, kvmv1.ConditionReasonTesting)
				hypervisorListBody = hypervisorListJSON(hypervisorRecordJSON(hypervisorId, serviceId, "up", "disabled", hypervisorName))
			})

			It("returns to the enable step instead of testing a disabled host", func(ctx SpecContext) {
				_, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonInitial))
				Expect(condition.Message).To(ContainSubstring("disabled"))
			})
		})

		When("several live records match the hostname", func() {
			BeforeEach(func() {
				hypervisorListBody = hypervisorListJSON(
					hypervisorRecordJSON("hv-a", "service-a", "up", "enabled", hypervisorName),
					hypervisorRecordJSON("hv-b", "service-b", "up", "enabled", hypervisorName),
				)
			})

			It("refuses to guess and records the ambiguity", func(ctx SpecContext) {
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.ServiceID).To(BeEmpty())
				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonInitial))
				Expect(condition.Message).To(ContainSubstring("multiple"))
			})
		})

		When("records for other hostnames match the search pattern", func() {
			BeforeEach(func() {
				// The hostname pattern is a substring search, so records for
				// other hosts can be returned; only an exact match counts.
				hypervisorListBody = hypervisorListJSON(
					hypervisorRecordJSON("hv-other", "service-other", "up", "enabled", "test-host-other"),
					hypervisorRecordJSON(hypervisorId, serviceId, "up", "enabled", hypervisorName),
				)
			})

			It("ignores records that do not exactly match the hostname", func(ctx SpecContext) {
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.HypervisorID).To(Equal(hypervisorId))
				Expect(hv.Status.ServiceID).To(Equal(serviceId))
				condition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Reason).To(Equal(kvmv1.ConditionReasonInitial))
			})
		})

		When("the stored identity is still current among duplicates", func() {
			BeforeEach(func(ctx SpecContext) {
				setStatus(ctx, "hv-b", "service-b", kvmv1.ConditionReasonInitial)
				hypervisorListBody = hypervisorListJSON(
					hypervisorRecordJSON("hv-a", "service-a", "up", "enabled", hypervisorName),
					hypervisorRecordJSON("hv-b", "service-b", "up", "enabled", hypervisorName),
				)
			})

			It("keeps the stored identity", func(ctx SpecContext) {
				// No aggregates are set, so initialOnboarding requeues after
				// the identity check; the stored identity must survive it.
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.HypervisorID).To(Equal("hv-b"))
				Expect(hv.Status.ServiceID).To(Equal("service-b"))
			})
		})

		When("onboarding already succeeded", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Status.ServiceID = "service-old"
				hv.Status.HypervisorID = "hv-old"
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Onboarding completed",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
			})

			It("does not contact Nova and keeps the stored identity", func(ctx SpecContext) {
				result, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
				Expect(listCalls).To(BeZero())

				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.ServiceID).To(Equal("service-old"))
			})
		})
	})

	Context("running tests after initial setup", func() {
		var (
			serverActionHandler func(http.ResponseWriter, *http.Request)
			serverDeleteHandler func(http.ResponseWriter, *http.Request)
			serverDetailHandler func(http.ResponseWriter, *http.Request)
		)

		BeforeEach(func(ctx SpecContext) {
			hv := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
			hv.Status.HypervisorID = hypervisorId
			hv.Status.ServiceID = serviceId
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionTrue,
				Reason: kvmv1.ConditionReasonInitial,
			})
			Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

			fakeServer.Mux.HandleFunc("PUT /os-services/service-id", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "enabled"}}`, serviceId)
				Expect(err).NotTo(HaveOccurred())
			})

			// The onboarding controller re-validates the Nova identity on
			// every reconcile, so the smoke-test phases still need a
			// hypervisor listing that matches the stored identity.
			fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, hypervisorListJSON(hypervisorRecordJSON(hypervisorId, serviceId, "up", "enabled", hypervisorName)))
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, emptyServersBody)
				Expect(err).NotTo(HaveOccurred())
			})

			serverDetailHandler = func(w http.ResponseWriter, _ *http.Request) {
				_, err := fmt.Fprint(w, emptyServersBody)
				Expect(err).NotTo(HaveOccurred())
			}
			fakeServer.Mux.HandleFunc("GET /servers/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				serverDetailHandler(w, r)
			})

			fakeServer.Mux.HandleFunc("POST /instance-ha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, `{}`)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, imagesBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("GET /networks", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, networksBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("POST /servers", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, createServerBody)
				Expect(err).NotTo(HaveOccurred())
			})

			serverActionHandler = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"output": "FAKE CONSOLE OUTPUT\nANOTHER\nLAST LINE\nohooc--%v-%v\n"}`, hv.Name, hv.UID)
				Expect(err).NotTo(HaveOccurred())
			}
			fakeServer.Mux.HandleFunc("POST /servers/server-id/action", func(w http.ResponseWriter, r *http.Request) {
				serverActionHandler(w, r)
			})

			serverDeleteHandler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			}
			fakeServer.Mux.HandleFunc("DELETE /servers/server-id", func(w http.ResponseWriter, r *http.Request) {
				serverDeleteHandler(w, r)
			})
		})

		When("SkipTests is set to true", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Spec.SkipTests = true
				hv.Spec.HighAvailability = true
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				// Simulate aggregates being set by aggregates controller
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
					{Name: testAggregateName, UUID: "uuid-" + testAggregateName},
				}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
			})

			It("should update the conditions", func(ctx SpecContext) {
				By("Reconciling to move from Initial to Testing state")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating HypervisorInstanceHa controller enabling HA")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeHaEnabled,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "HA is enabled",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Reconciling again to call completeOnboarding and set RemovingTestAggregate")
				err = reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Verifying onboarding is in RemovingTestAggregate state")
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				onboardingCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(onboardingCond).NotTo(BeNil())
				Expect(onboardingCond.Reason).To(Equal(kvmv1.ConditionReasonHandover))

				By("Simulating aggregates controller updating aggregates and setting condition")
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
				}
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeAggregatesUpdated,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Aggregates updated successfully",
				})
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeTraitsUpdated,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Traits updated successfully",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Reconciling once more to complete onboarding and set Ready")
				err = reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Verifying final state")
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", kvmv1.ConditionReasonSucceeded),
					),
				))
			})
		})
		When("SkipTests is set to false", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Spec.SkipTests = false
				hv.Spec.HighAvailability = true
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())

				// Simulate aggregates being set by aggregates controller
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
					{Name: testAggregateName, UUID: "uuid-" + testAggregateName},
				}
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
			})

			It("should update the conditions", func(ctx SpecContext) {
				By("Reconciling to run tests")
				err := reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Reconciling again after server is active")
				err = reconcileLoop(ctx, 1)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating HypervisorInstanceHa controller enabling HA")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeHaEnabled,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "HA is enabled",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Simulating aggregates controller setting condition after removing test aggregate")
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Status.Aggregates = []kvmv1.Aggregate{
					{Name: availabilityZone, UUID: "uuid-" + availabilityZone},
				}
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeAggregatesUpdated,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Aggregates updated successfully",
				})
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeTraitsUpdated,
					Status:  metav1.ConditionTrue,
					Reason:  kvmv1.ConditionReasonSucceeded,
					Message: "Traits updated successfully",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				By("Reconciling to complete onboarding after aggregates and traits conditions are set")
				err = reconcileLoop(ctx, 5)
				Expect(err).NotTo(HaveOccurred())

				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", kvmv1.ConditionReasonSucceeded),
					),
				))
			})
		})

		When("smoke test times out waiting for console output", func() {
			var serverDeletedCalled bool

			BeforeEach(func(ctx SpecContext) {
				By("Overriding HV status to Testing state")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:   kvmv1.ConditionTypeOnboarding,
					Status: metav1.ConditionTrue,
					Reason: kvmv1.ConditionReasonTesting,
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				// Construct the server name the controller will look for.
				serverName := fmt.Sprintf("%s-%s-%s", testPrefixName, hypervisorName, string(hv.UID))
				detailCallCount := 0

				// On the first reconcile GET /servers/detail returns empty so the controller
				// creates the server via POST (no launched_at → timeout cannot fire yet).
				// On the second reconcile GET /servers/detail returns the ACTIVE server with a
				// stale launched_at so createOrGetTestServer takes the "already-running" path and
				// smokeTest fires the timeout.
				serverDetailHandler = func(w http.ResponseWriter, _ *http.Request) {
					if detailCallCount == 0 {
						detailCallCount++
						_, err := fmt.Fprint(w, emptyServersBody)
						Expect(err).NotTo(HaveOccurred())
					} else {
						_, err := fmt.Fprintf(w,
							`{"servers": [{"id": "server-id", "name": %q, "status": "ACTIVE", "OS-SRV-USG:launched_at": "2025-01-01T12:00:00.000000"}], "servers_links": []}`,
							serverName)
						Expect(err).NotTo(HaveOccurred())
					}
				}

				// Set the clock to 6 minutes after the launched_at above (past the 5-minute deadline).
				onboardingReconciler.Clock = clocktesting.NewFakeClock(time.Date(2025, 1, 1, 12, 6, 0, 0, time.UTC))
				serverDeletedCalled = false

				// Console output that does NOT contain the server name, so the timeout path is exercised.
				serverActionHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, `{"output": "some unrelated console output\n"}`)
					Expect(err).NotTo(HaveOccurred())
				}
				serverDeleteHandler = func(w http.ResponseWriter, _ *http.Request) {
					serverDeletedCalled = true
					w.WriteHeader(http.StatusAccepted)
				}
			})

			It("should delete the stalled server and record a timeout in the status", func(ctx SpecContext) {
				By("First reconcile: controller creates the ACTIVE server; launched_at is absent so timeout does not fire yet")
				_, err := onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())

				By("Second reconcile: GET /servers/detail returns the stale server; timeout fires and the server is deleted")
				_, err = onboardingReconciler.Reconcile(ctx, reconcileReq)
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the timed-out server was deleted")
				Expect(serverDeletedCalled).To(BeTrue())

				By("Verifying the onboarding condition message indicates a timeout")
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				onboardingCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				Expect(onboardingCond).NotTo(BeNil())
				Expect(onboardingCond.Reason).To(Equal(kvmv1.ConditionReasonTesting))
				Expect(onboardingCond.Message).To(ContainSubstring("timeout"))
			})
		})

	})
})
