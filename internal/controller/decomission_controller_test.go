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
	)

	BeforeEach(func(ctx SpecContext) {
		r = &NodeDecommissionReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		By("creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		By("creating the core resource for the Kind Node")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName.Name,
				Labels: map[string]string{labelEvictionRequired: "true"},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		By("Create the hypervisor resource with lifecycle enabled")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName.Name,
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

	AfterEach(func(ctx SpecContext) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName.Name}}
		By("Cleanup the specific node and hypervisor resource")
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())

		// Due to the decommissioning finalizer, we need to reconcile once more to delete the node completely
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: nodeName.Name},
		}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		nodelist := &corev1.NodeList{}
		Expect(k8sClient.List(ctx, nodelist)).To(Succeed())
		Expect(nodelist.Items).To(BeEmpty())
	})

	Context("When reconciling a node", func() {
		It("should set the finalizer", func(ctx SpecContext) {
			By("reconciling the created resource")
			_, err := r.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())
			node := &corev1.Node{}

			Expect(k8sClient.Get(ctx, nodeName, node)).To(Succeed())
			Expect(node.Finalizers).To(ContainElement(decommissionFinalizerName))
		})
	})
})
