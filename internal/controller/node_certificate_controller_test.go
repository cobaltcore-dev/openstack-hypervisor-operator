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
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Node Certificate Controller", func() {
	var nodeCertificateController *NodeCertificateController
	var fakeClient client.Client
	const (
		nodeName   = "random-node"
		issuerName = "test-issuer"
		namespace  = "test-namespace"
	)

	// Setup

	BeforeEach(func(ctx SpecContext) {
		By("Setting up the test environment")
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(cmapi.AddToScheme(scheme)).To(Succeed())

		// We need to use the fake client because the envtest environment does include
		// cert-manager CRDs out of the box.
		By("Creating the fake client")
		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		nodeCertificateController = &NodeCertificateController{
			Client:     fakeClient,
			Scheme:     fakeClient.Scheme(),
			issuerName: issuerName,
			namespace:  namespace,
		}

		By("creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreAlreadyExists(fakeClient.Create(ctx, ns))).To(Succeed())

		By("creating the core resource for the Kind Node")
		resource := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{labelHypervisor: "test"},
			},
		}
		Expect(fakeClient.Create(ctx, resource)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
		By("Cleanup the specific node")
		Expect(client.IgnoreAlreadyExists(fakeClient.Delete(ctx, node))).To(Succeed())

		By("Cleaning up the test environment")
	})

	// Tests

	Context("When reconciling a node with nova virt label", func() {
		It("should successfully create a new certificate", func(ctx SpecContext) {
			By("Reconciling the node")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			}
			_, err := nodeCertificateController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("Checking if the certificate was created")
			_, certName := getSecretAndCertName(nodeName)
			certificate := &cmapi.Certificate{}
			err = fakeClient.Get(ctx, types.NamespacedName{Name: certName, Namespace: namespace}, certificate)
			Expect(err).NotTo(HaveOccurred())
			Expect(certificate.Spec.IssuerRef.Name).To(Equal(issuerName))
			Expect(certificate.Spec.DNSNames).To(ContainElement(nodeName))
		})
	})
})
