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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Hypervisor Controller", func() {
	const (
		resourceName = "other-node"
	)
	var (
		hypervisorController *HypervisorController
		resource             *corev1.Node
		hypervisorName       = types.NamespacedName{Name: resourceName}
	)

	BeforeEach(func(ctx SpecContext) {
		hypervisorController = &HypervisorController{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		DeferCleanup(func() {
			hypervisorController = nil
		})

		By("creating the namespace for the reconciler")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monsoon3"}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		// pregenerate the resource
		resource = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        resourceName,
				Labels:      map[string]string{corev1.LabelTopologyZone: "test-zone"},
				Annotations: map[string]string{annotationCustomTraits: "test-trait"},
			},
		}
	})

	AfterEach(func(ctx SpecContext) {
		By("Cleanup the specific node")
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, resource))).To(Succeed())

		hypervisor := &kvmv1.Hypervisor{ObjectMeta: metav1.ObjectMeta{Name: resource.Name}}
		By("Cleanup the specific hypervisor")
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
	})

	Context("When reconciling a node", func() {

		It("should successfully reconcile the node", func(ctx SpecContext) {
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("Reconciling the created resource")
			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			By("should have created the Hypervisor resource")
			// Get the Hypervisor resource
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(hypervisor.Name).To(Equal(resource.Name))
			Expect(hypervisor.Labels).ToNot(BeNil())
			Expect(hypervisor.Labels[corev1.LabelTopologyZone]).To(Equal("test-zone"))
			Expect(hypervisor.Spec.Maintenance).To(BeEmpty())

			By("Adding a label annotation to the node and reconciling again")
			// Add an aggregate annotation to the node
			labeledResource := resource.DeepCopy()
			labeledResource.Labels["worker.garden.sapcloud.io/group"] = "new-group"
			Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

			_, err = hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			By("should have updated the Hypervisor resource with the aggregate")
			// Get the Hypervisor resource again
			updatedHypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updatedHypervisor)).To(Succeed())
			Expect(updatedHypervisor.Name).To(Equal(resource.Name))
			Expect(updatedHypervisor.Spec.CustomTraits).ToNot(BeNil())
			Expect(updatedHypervisor.Spec.CustomTraits).To(ContainElement("test-trait"))
			Expect(updatedHypervisor.Labels).To(HaveKeyWithValue("worker.garden.sapcloud.io/group", "new-group"))
			Expect(updatedHypervisor.Spec.Maintenance).To(BeEmpty())
		})
	})

	Context("When reconciling a terminating node", func() {
		It("should successfully reconcile the terminating node", func(ctx SpecContext) {
			// Mark the node as terminating
			resource.DeletionTimestamp = &metav1.Time{}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// Update node condition
			resource.Status.Conditions = append(resource.Status.Conditions, corev1.NodeCondition{
				Type:    "Terminating",
				Status:  corev1.ConditionTrue,
				Reason:  "Terminating",
				Message: "Node is terminating",
			})
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(hypervisor.Spec.Maintenance).To(Equal(kvmv1.MaintenanceTermination))

			By("Reconciling the created resource")
			for range 2 {
				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			}

			By("should have set the terminating condition on the Hypervisor resource")
			// Get the Hypervisor resource
			updatedHypervisor := &kvmv1.Hypervisor{}
			Expect(hypervisorController.Get(ctx, hypervisorName, updatedHypervisor)).To(Succeed())
			Expect(updatedHypervisor.Name).To(Equal(resource.Name))
			Expect(updatedHypervisor.Status.Conditions).ToNot(BeNil())
			condition := meta.FindStatusCondition(updatedHypervisor.Status.Conditions, kvmv1.ConditionTypeReady)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Reason).To(Equal("Terminating"))
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))

			condition = meta.FindStatusCondition(updatedHypervisor.Status.Conditions, kvmv1.ConditionTypeTerminating)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Reason).To(Equal("Terminating"))
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
