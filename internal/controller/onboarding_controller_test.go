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

var _ = Describe("Onboarding Controller", func() {
	const (
		region                = "test-region"
		availabilityZone      = "test-az"
		hypervisorName        = "test-host"
		serviceId             = "service-id"
		hypervisorId          = "c48f6247-abe4-4a24-824e-ea39e108874f"
		aggregatesBodyInitial = `{
	"aggregates": [
		{
			"name": "test-az",
			"availability_zone": "test-az",
			"deleted": false,
			"id": 100001,
			"hosts": []
		},
		{
			"name": "tenant_filter_tests",
			"availability_zone": "",
			"deleted": false,
			"id": 99,
			"hosts": []
		}
	]
}`

		aggregatesBodyUnexpected = `{
	"aggregates": [
		{
			"name": "test-az",
			"availability_zone": "test-az",
			"deleted": false,
			"id": 100001,
			"hosts": []
		},
		{
			"name": "tenant_filter_tests",
			"availability_zone": "",
			"deleted": false,
			"id": 99,
			"hosts": []
		},
		{
			"name": "unexpected",
			"availability_zone": "",
			"deleted": false,
			"id": -1,
			"hosts": ["test-host"]
		}
	]
}`

		aggregatesBodySetup = `{
    "aggregates": [
		{
			"name": "test-az",
			"availability_zone": "test-az",
			"deleted": false,
			"id": 100001,
			"hosts": ["test-host"]
		},
		{
			"name": "tenant_filter_tests",
			"availability_zone": "",
			"deleted": false,
			"id": 99,
			"hosts": ["test-host"]
		}
    ]
}`
		addedHostToAzBody = `{
    "aggregate": {
            "name": "test-az",
            "availability_zone": "test-az",
            "deleted": false,
            "hosts": [
                "test-host"
            ],
            "id": 100001
        }
}`

		addedHostToTestBody = `{
	"aggregate": {
		"name": "tenant_filter_tests",
		"availability_zone": "",
		"deleted": false,
		"hosts": [
			"test-host"
		],
		"id": 99
	}
}`

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
            "name": "cirros-d240801-kvm",
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
		By("creating the resource for the Kind Hypervisor")
		hv := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName,
				Labels: map[string]string{
					corev1.LabelTopologyRegion: region,
					corev1.LabelTopologyZone:   availabilityZone,
					corev1.LabelHostname:       hypervisorName,
				},
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hv)).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			By("Cleanup the specific hypervisor CRO")
			Expect(k8sClient.Delete(ctx, hv)).To(Succeed())
		})

		fakeServer = testhelper.SetupHTTP()
		os.Setenv("KVM_HA_SERVICE_URL", fakeServer.Endpoint()+"instance-ha")

		DeferCleanup(func() {
			os.Unsetenv("KVM_HA_SERVICE_URL")
			fakeServer.Teardown()
		})

		onboardingReconciler = &OnboardingController{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			computeClient:     client.ServiceClient(fakeServer),
			testComputeClient: client.ServiceClient(fakeServer),
			testImageClient:   client.ServiceClient(fakeServer),
			testNetworkClient: client.ServiceClient(fakeServer),
		}

		DeferCleanup(func() {
			onboardingReconciler = nil
		})
	})

	Context("initial setup of a new hypervisor", func() {
		BeforeEach(func() {
			fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "", hypervisorName)).ToNot(BeNil())
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/100001/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, addedHostToAzBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/99/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, addedHostToTestBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("PUT /os-services/service-id", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "enabled"}}`, serviceId)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		When("it is a clean setup", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, aggregatesBodyInitial)
					Expect(err).NotTo(HaveOccurred())
				})
			})

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
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 2)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", kvmv1.ConditionReasonOnboarding),
					),
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonTesting),
					),
				))
			})
		})

		When("it the host is already in an unexpected aggregate", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, aggregatesBodyUnexpected)
					Expect(err).NotTo(HaveOccurred())
				})

				fakeServer.Mux.HandleFunc("POST /os-aggregates/-1/action", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, err := fmt.Fprint(w, addedHostToTestBody)
					Expect(err).NotTo(HaveOccurred())
				})
			})

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
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 2)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", kvmv1.ConditionReasonOnboarding),
					),
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", kvmv1.ConditionReasonTesting),
					),
				))
			})
		})
	})

	Context("running tests after initial setup", func() {
		BeforeEach(func(ctx SpecContext) {
			hv := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
			hv.Status.HypervisorID = hypervisorId
			hv.Status.ServiceID = serviceId
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeReady,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonOnboarding,
			})
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionTrue,
				Reason: kvmv1.ConditionReasonInitial,
			})
			Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

			fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, aggregatesBodySetup)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("PUT /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, aggregatesBodySetup)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("PUT /os-services/service-id", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"service": {"id": "%v", "status": "enabled"}}`, serviceId)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, emptyServersBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("GET /servers/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, emptyServersBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("POST /os-aggregates/99/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, addedHostToTestBody)
				Expect(err).NotTo(HaveOccurred())
			})

			fakeServer.Mux.HandleFunc("POST /instance-ha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, `{}`)
				Expect(err).NotTo(HaveOccurred())
			})

			// Only needed for mocking the test
			fakeServer.Mux.HandleFunc("GET /flavors/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprint(w, flavorDetailsBody)
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

			fakeServer.Mux.HandleFunc("POST /servers/server-id/action", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, err := fmt.Fprintf(w, `{"output": "FAKE CONSOLE OUTPUT\nANOTHER\nLAST LINE\nohooc--%v-%v\n"}`, hv.Name, hv.UID)
				Expect(err).NotTo(HaveOccurred())

			})
			fakeServer.Mux.HandleFunc("DELETE /servers/server-id", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			})
		})

		When("SkipTests is set to true", func() {
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				hv.Spec.SkipTests = true
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())
			})

			It("should update the conditions", func(ctx SpecContext) {
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 3)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Status", metav1.ConditionTrue),
					),
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
				Expect(k8sClient.Update(ctx, hv)).To(Succeed())
			})

			It("should update the conditions", func(ctx SpecContext) {
				By("Reconciling the created resource")
				err := reconcileLoop(ctx, 3)
				Expect(err).NotTo(HaveOccurred())
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, namespacedName, hv)).To(Succeed())
				Expect(hv.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Status", metav1.ConditionTrue),
					),
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeOnboarding),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", kvmv1.ConditionReasonSucceeded),
					),
				))
			})
		})

	})
})
