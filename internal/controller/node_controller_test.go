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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Node Controller", func() {
	Context("When reconciling a node", func() {
		const resourceName = "node-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "monsoon3",
		}
		node := corev1.Node{}
		generateReconcileRequest := func() request {
			return request{NamespacedName: typeNamespacedName, clusterName: "self", client: k8sClient}
		}

		BeforeEach(func() {
			By("creating the core resource for the Kind Node")
			err := k8sClient.Get(ctx, typeNamespacedName, &node)
			if err != nil && errors.IsNotFound(err) {
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "monsoon3",
					},
				}
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())

				resource := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:      typeNamespacedName.Name,
						Namespace: typeNamespacedName.Namespace,
						Labels:    map[string]string{HOST_LABEL: "test", MAINTENANCE_NEEDED_LABEL: "true"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1.Node{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Eviction")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			testhelper.SetupHTTP()
			defer testhelper.TeardownHTTP()

			controllerReconciler := &NodeReconciler{}

			_, err := controllerReconciler.Reconcile(ctx, generateReconcileRequest())
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
