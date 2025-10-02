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

	"github.com/gophercloud/gophercloud/v2/testhelper"
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
	var decomReconciler *NodeDecommissionReconciler
	const nodeName = "node-test"

	BeforeEach(func(ctx context.Context) {
		decomReconciler = &NodeDecommissionReconciler{
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
				Labels: map[string]string{labelEvictionRequired: "true"}, //nolint:goconst
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		By("Create the hypervisor resource")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
	})

	AfterEach(func(ctx context.Context) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
		By("Cleanup the specific node and hypervisor resource")
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())

		// Due to the decommissioning finalizer, we need to reconcile once more to delete the node completely
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: nodeName},
		}
		_, err := decomReconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		nodelist := &corev1.NodeList{}
		Expect(k8sClient.List(ctx, nodelist)).To(Succeed())
		Expect(nodelist.Items).To(BeEmpty())
	})

	Context("When reconciling a node", func() {

		It("should successfully reconcile the resource", func(ctx context.Context) {
			By("ConditionType the created resource")
			testhelper.SetupHTTP()
			defer testhelper.TeardownHTTP()

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			}
			_, err := decomReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
