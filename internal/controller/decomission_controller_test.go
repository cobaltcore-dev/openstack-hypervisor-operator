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
		EOF                 = "EOF"
		serviceId           = "service-1234"
		hypervisorName      = "node-test"
		namespaceName       = "namespace-test"
		AggregateListWithHv = `
{
    "aggregates": [
		{
			"name": "test-aggregate2",
			"availability_zone": "",
			"deleted": false,
			"id": 100001,
			"hosts": ["note-test"]
		}
    ]
}
`
		AggregateRemoveHostBody = `
{
        "aggregate": {
			"name": "test-aggregate2",
			"availability_zone": "",
			"deleted": false,
			"id": 100001
		}
}`
	)

	var (
		decommissionReconciler *NodeDecommissionReconciler
		resourceName           = types.NamespacedName{Name: hypervisorName}
		reconcileReq           = ctrl.Request{
			NamespacedName: resourceName,
		}
		fakeServer testhelper.FakeServer
	)

	BeforeEach(func(ctx SpecContext) {
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

		decommissionReconciler = &NodeDecommissionReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			computeClient:   client.ServiceClient(fakeServer),
			placementClient: client.ServiceClient(fakeServer),
		}

		By("creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
		Expect(k8sclient.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		By("Create the hypervisor resource with lifecycle enabled")
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

	Context("When marking the hypervisor terminating", func() {
		JustBeforeEach(func(ctx SpecContext) {
			By("reconciling first to add the finalizer")
			_, err := decommissionReconciler.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, resourceName, hypervisor)).To(Succeed())

			By("and then marking the hypervisor terminating")
			hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeTerminating,
				Status:  metav1.ConditionTrue,
				Reason:  "dontcare",
				Message: "dontcare",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		When("the hypervisor was set to ready and has been evicted", func() {
			getHypervisorsCalled := 0
			BeforeEach(func(ctx SpecContext) {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, resourceName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions,
					metav1.Condition{
						Type:    kvmv1.ConditionTypeReady,
						Status:  metav1.ConditionTrue,
						Reason:  "dontcare",
						Message: "dontcare",
					},
				)
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

				fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					getHypervisorsCalled++
					Expect(fmt.Fprintf(w, HypervisorWithServers, serviceId, "some reason", hypervisorName)).ToNot(BeNil())
				})

				fakeServer.Mux.HandleFunc("GET /os-aggregates", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)

					_, err := fmt.Fprint(w, AggregateListWithHv)
					Expect(err).NotTo(HaveOccurred())
				})

				fakeServer.Mux.HandleFunc("POST /os-aggregates/100001/action", func(w http.ResponseWriter, r *http.Request) {
					// parse request
					Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
					expectedBody := `{"remove_host":{"host":"hv-test"}}`
					body := make([]byte, r.ContentLength)
					_, err := r.Body.Read(body)
					Expect(err == nil || err.Error() == EOF).To(BeTrue())
					Expect(string(body)).To(MatchJSON(expectedBody))

					// send response
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)

					_, err = fmt.Fprint(w, AggregateRemoveHostBody)
					Expect(err).NotTo(HaveOccurred())
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
				By("reconciling the created resource")
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
				By("reconciling the created resource")
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
		})

	})
})
