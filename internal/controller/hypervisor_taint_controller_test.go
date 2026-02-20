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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Hypervisor Taint Controller", func() {
	const (
		hypervisorName = "test-hv"
	)
	var (
		controller     *HypervisorTaintController
		resource       *kvmv1.Hypervisor
		namespacedName = types.NamespacedName{Name: hypervisorName}
		reconcileReq   = ctrl.Request{NamespacedName: namespacedName}
	)

	BeforeEach(func(ctx SpecContext) {
		controller = &HypervisorTaintController{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// pregenerate the resource
		resource = &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName,
			},
		}

		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
	})

	Context("When reconciling a new hypervisor", func() {
		It("should successfully reconcile the hypervisor", func(ctx SpecContext) {
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, namespacedName, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(
				SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeTainted),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Message", "🟢"),
					HaveField("ObservedGeneration", resource.Generation),
				)))
		})
	})

	Context("When reconciling an edited hypervisor ", func() {
		BeforeEach(func(ctx SpecContext) {
			resource.Spec.SkipTests = true
			Expect(k8sClient.Update(ctx, resource, client.FieldOwner("kubectl-edit"))).To(Succeed())
		})

		It("should successfully reconcile the hypervisor", func(ctx SpecContext) {
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, namespacedName, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(
				SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeTainted),
					HaveField("Status", metav1.ConditionTrue),
					HaveField("Message", "⚠️"),
					HaveField("ObservedGeneration", resource.Generation),
				)))
		})
	})

	Context("When reconciling a non-existent hypervisor", func() {
		It("should return without error", func(ctx SpecContext) {
			nonExistentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "non-existent-hv"},
			}
			_, err := controller.Reconcile(ctx, nonExistentReq)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When no changes are needed", func() {
		It("should return early without updating status", func(ctx SpecContext) {
			// First reconcile to set initial condition
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile without any changes - should return early without error
			_, err = controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			// Verify the condition is still correct
			Expect(k8sClient.Get(ctx, namespacedName, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(
				SatisfyAll(
					HaveField("Type", kvmv1.ConditionTypeTainted),
					HaveField("Status", metav1.ConditionFalse),
				)))
		})
	})
})
