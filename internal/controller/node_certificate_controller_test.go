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
	"regexp"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
		ns := &corev1.Namespace{Name: namespace}
		Expect(client.IgnoreAlreadyExists(fakeClient.Create(ctx, ns))).To(Succeed())

		By("creating the core resource for the Kind Node")
		resource := &corev1.Node{
			Name:   nodeName,
			Labels: map[string]string{labelHypervisor: "test"},
		}
		Expect(fakeClient.Create(ctx, resource)).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			node := &corev1.Node{Name: nodeName}
			By("Cleanup the specific node")
			Expect(client.IgnoreAlreadyExists(fakeClient.Delete(ctx, node))).To(Succeed())

			By("Cleaning up the test environment")
		})
	})

	// Tests

	Context("When reconciling a node with nova virt label", func() {
		It("should successfully create a new certificate", func(ctx SpecContext) {
			By("Reconciling the node")
			req := ctrl.Request{
				Name: nodeName,
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

	Context("When reconciling a node with various address types", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Updating node with different address types")
			node := &corev1.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "hostname.example.com"},
				{Type: corev1.NodeInternalDNS, Address: "internal.example.com"},
				{Type: corev1.NodeExternalDNS, Address: "external.example.com"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeHostName, Address: ""}, // Empty address should be skipped
			}
			Expect(fakeClient.Status().Update(ctx, node)).To(Succeed())
		})

		It("should create certificate with all DNS names and IP addresses", func(ctx SpecContext) {
			By("Reconciling the node")
			req := ctrl.Request{
				Name: nodeName,
			}
			_, err := nodeCertificateController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("Checking if the certificate includes all addresses")
			_, certName := getSecretAndCertName(nodeName)
			certificate := &cmapi.Certificate{}
			err = fakeClient.Get(ctx, types.NamespacedName{Name: certName, Namespace: namespace}, certificate)
			Expect(err).NotTo(HaveOccurred())

			// Check DNS names
			Expect(certificate.Spec.DNSNames).To(ContainElements(
				nodeName,
				"hostname.example.com",
				"internal.example.com",
				"external.example.com",
			))

			// Check IP addresses
			Expect(certificate.Spec.IPAddresses).To(ContainElements(
				"10.0.0.1",
				"203.0.113.1",
			))
		})
	})

	Context("When reconciling a node without nova virt label", func() {
		BeforeEach(func(ctx SpecContext) {
			By("Creating a node without the hypervisor label")
			nodeWithoutLabel := &corev1.Node{
				Name: "node-without-label",
			}
			Expect(fakeClient.Create(ctx, nodeWithoutLabel)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(fakeClient.Delete(ctx, nodeWithoutLabel)).To(Succeed())
			})
		})

		It("should still create certificate when Reconcile is called directly", func(ctx SpecContext) {
			// Note: The label filter is enforced by the controller predicate in SetupWithManager,
			// not in the Reconcile method itself. When testing Reconcile directly, it will still
			// create the certificate even without the label.
			By("Reconciling the node without label")
			req := ctrl.Request{
				Name: "node-without-label",
			}
			_, err := nodeCertificateController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying certificate was created despite missing label")
			_, certName := getSecretAndCertName("node-without-label")
			certificate := &cmapi.Certificate{}
			err = fakeClient.Get(ctx, types.NamespacedName{Name: certName, Namespace: namespace}, certificate)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a non-existent node", func() {
		It("should return without error", func(ctx SpecContext) {
			By("Reconciling a non-existent node")
			req := ctrl.Request{
				Name: "non-existent-node",
			}
			_, err := nodeCertificateController.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a node with the Calico interfaces annotation", func() {
		// updateCalicoAnnotation sets the projectcalico.org/Interfaces annotation on the node.
		updateCalicoAnnotation := func(ctx SpecContext, value string) {
			node := &corev1.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
			if node.Annotations == nil {
				node.Annotations = map[string]string{}
			}
			node.Annotations[annotationCalicoInterfaces] = value
			Expect(fakeClient.Update(ctx, node)).To(Succeed())
		}

		// setNodeInternalIP sets the node status InternalIP.
		setNodeInternalIP := func(ctx SpecContext, ip string) {
			node := &corev1.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
			node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
			Expect(fakeClient.Status().Update(ctx, node)).To(Succeed())
		}

		// reconcileNode reconciles the node under test and expects no error.
		reconcileNode := func(ctx SpecContext) {
			_, err := nodeCertificateController.Reconcile(ctx, ctrl.Request{Name: nodeName})
			Expect(err).NotTo(HaveOccurred())
		}

		// fetchCertificate returns the certificate created for the node under test.
		fetchCertificate := func(ctx SpecContext) *cmapi.Certificate {
			_, certName := getSecretAndCertName(nodeName)
			certificate := &cmapi.Certificate{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: certName, Namespace: namespace}, certificate)).To(Succeed())
			return certificate
		}

		type annotationCase struct {
			includeRegex string
			excludeRegex string
			annotation   string
			statusIP     string
			expectedIPs  []string
		}

		DescribeTable("computing the certificate IP addresses from the Calico annotation",
			func(ctx SpecContext, testCase annotationCase) {
				if testCase.includeRegex != "" {
					nodeCertificateController.includeInterfaceNames = regexp.MustCompile(testCase.includeRegex)
				}
				if testCase.excludeRegex != "" {
					nodeCertificateController.excludeInterfaceNames = regexp.MustCompile(testCase.excludeRegex)
				}

				updateCalicoAnnotation(ctx, testCase.annotation)
				if testCase.statusIP != "" {
					setNodeInternalIP(ctx, testCase.statusIP)
				}

				reconcileNode(ctx)

				certificate := fetchCertificate(ctx)
				Expect(certificate.Spec.IPAddresses).To(Equal(testCase.expectedIPs))
			},
			Entry("includes only interfaces matching the include regex",
				annotationCase{
					includeRegex: `^vlan_[0-9]+$`,
					annotation:   `[{"name":"vlan_100","addresses":["10.10.0.5"]},{"name":"enp1s0","addresses":["10.0.0.9"]},{"name":"vlan_abc","addresses":["10.10.0.6"]}]`,
					statusIP:     "10.0.0.1",
					expectedIPs:  []string{"10.0.0.1", "10.10.0.5"},
				}),
			Entry("drops interfaces matching the exclude regex even if they are included",
				annotationCase{
					includeRegex: `^vlan_.*$`,
					excludeRegex: `^vlan_100$`,
					annotation:   `[{"name":"vlan_100","addresses":["10.10.0.5"]},{"name":"vlan_200","addresses":["10.10.0.6"]}]`,
					expectedIPs:  []string{"10.10.0.6"},
				}),
			Entry("merges annotation addresses with status addresses and deduplicates",
				annotationCase{
					includeRegex: `^vlan_[0-9]+$`,
					annotation:   `[{"name":"vlan_100","addresses":["10.0.0.1","10.10.0.5"]}]`,
					statusIP:     "10.0.0.1",
					expectedIPs:  []string{"10.0.0.1", "10.10.0.5"},
				}),
			Entry("skips non-routable and invalid addresses",
				annotationCase{
					includeRegex: `.*`,
					annotation:   `[{"name":"vlan_100","addresses":["not-an-ip","fe80::1","127.0.0.1","10.10.0.5"]}]`,
					expectedIPs:  []string{"10.10.0.5"},
				}),
			Entry("still creates the certificate when the annotation is malformed JSON",
				annotationCase{
					includeRegex: `^vlan_[0-9]+$`,
					annotation:   `this-is-not-json`,
					statusIP:     "10.0.0.1",
					expectedIPs:  []string{"10.0.0.1"},
				}),
		)

		It("should remove annotation addresses from the certificate when the annotation is deleted", func(ctx SpecContext) {
			By("Configuring an include filter for numbered vlan interfaces")
			nodeCertificateController.includeInterfaceNames = regexp.MustCompile(`^vlan_[0-9]+$`)

			By("Annotating the node with Calico interfaces")
			updateCalicoAnnotation(ctx, `[{"name":"vlan_100","addresses":["10.10.0.5"]}]`)

			By("Setting the node status InternalIP")
			setNodeInternalIP(ctx, "10.0.0.1")

			By("Reconciling the node")
			reconcileNode(ctx)

			By("Checking that both addresses are present")
			certificate := fetchCertificate(ctx)
			Expect(certificate.Spec.IPAddresses).To(Equal([]string{"10.0.0.1", "10.10.0.5"}))

			By("Deleting the Calico interfaces annotation from the node")
			node := &corev1.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
			delete(node.Annotations, annotationCalicoInterfaces)
			Expect(fakeClient.Update(ctx, node)).To(Succeed())

			By("Reconciling the node again")
			reconcileNode(ctx)

			By("Checking that the annotation IP is removed and the status IP is retained")
			certificate = fetchCertificate(ctx)
			Expect(certificate.Spec.IPAddresses).To(Equal([]string{"10.0.0.1"}))
		})
	})
})
