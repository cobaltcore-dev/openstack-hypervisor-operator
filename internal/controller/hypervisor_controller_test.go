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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Hypervisor Controller", func() {
	var hypervisorController *HypervisorController

	Context("When reconciling a node", func() {
		const nodeName = "node-test"

		BeforeEach(func(ctx SpecContext) {
			hypervisorController = &HypervisorController{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the namespace for the reconciler")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monsoon3"}}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

			By("creating the core resource for the Kind Node")
			resource := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   nodeName,
					Labels: map[string]string{corev1.LabelTopologyZone: "test-zone"}, //nolint:goconst
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func(ctx SpecContext) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			By("Cleanup the specific node")
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		It("should successfully reconcile the node", func(ctx SpecContext) {
			By("Reconciling the created resource")
			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should have created the Hypervisor resource", func(ctx SpecContext) {
			// Get the Hypervisor resource
			hypervisor := &kvmv1.Hypervisor{}
			hypervisorName := types.NamespacedName{Name: nodeName}
			Expect(hypervisorController.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(hypervisor.Name).To(Equal(nodeName))
			Expect(hypervisor.Labels).ToNot(BeNil())
			Expect(hypervisor.Labels[corev1.LabelTopologyZone]).To(Equal("test-zone"))
		})
	})
})
