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

var _ = Describe("Node Controller", func() {
	var nodeReconciler *NodeReconciler

	Context("When reconciling a node", func() {
		const nodeName = "node-test"

		ctx := context.Background()

		reconcileNodeLoop := func(steps int) (res ctrl.Result, err error) {
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			}
			for range steps {
				res, err = nodeReconciler.Reconcile(ctx, req)
				if err != nil {
					return
				}
			}
			return
		}

		BeforeEach(func() {
			nodeReconciler = &NodeReconciler{
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
					Labels: map[string]string{HOST_LABEL: "test", EVICTION_REQUIRED_LABEL: "true"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			By("Cleanup the specific node")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			testhelper.SetupHTTP()
			defer testhelper.TeardownHTTP()

			_, err := reconcileNodeLoop(1)
			Expect(err).NotTo(HaveOccurred())

			// expect node controller to create an eviction for the node
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "maintenance-required-test",
				Namespace: "monsoon3",
			}, &kvmv1.Eviction{})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
