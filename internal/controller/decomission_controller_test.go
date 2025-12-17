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
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	osclient "github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Decommission Controller", func() {
	const (
		namespaceName = "namespace-test"
	)
	var (
		r            *NodeDecommissionReconciler
		nodeName     = types.NamespacedName{Name: "node-test"}
		reconcileReq = ctrl.Request{
			NamespacedName: nodeName,
		}
		fakeServer testhelper.FakeServer
	)

	BeforeEach(func(ctx SpecContext) {
		fakeServer = testhelper.SetupHTTP()
		r = &NodeDecommissionReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			computeClient: osclient.ServiceClient(fakeServer),
		}

		By("creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		By("creating the core resource for the Kind Node")
		hv := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName.Name,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
				Maintenance:      kvmv1.MaintenanceTermination,
			},
		}
		Expect(k8sClient.Create(ctx, hv)).To(Succeed())

		// set ready condition
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionTrue,
			Reason:  kvmv1.ConditionReasonReadyReady,
			Message: "Setting initial ready condition for testing",
		})
		Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hv))).To(Succeed())
			fakeServer.Teardown()
		})
	})

	Context("When reconciling a hypervisor", func() {
		It("It should set the ready status", func(ctx context.Context) {
			By("reconciling the created resource")
			_, err := r.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			By("checking that the ready condition is set to false with the decommissioning reason")
			hv := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, nodeName, hv)).To(Succeed())
			cond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonDecommissioning))

			fakeServer.Mux.HandleFunc("GET /os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)

				Expect(err).NotTo(HaveOccurred())
			})
			By("reconciling the created resource")
			_, err = r.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			By("checking that the eviction succeeded")
			hv = &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, nodeName, hv)).To(Succeed())
			Expect(hv.Status.Evicted).To(BeTrue())
			cond = meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(kvmv1.ConditionReasonDecommissioning))
			Expect(cond.Message).To(Equal("Node not registered in nova anymore, proceeding with deletion"))
		})
	})
})
