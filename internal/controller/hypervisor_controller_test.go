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
		resourceName      = "other-node"
		topologyZone      = "test-zone"
		customTrait       = "test-trait"
		aggregate1        = "aggregate1"
		workerGroupLabel  = "worker.garden.sapcloud.io/group"
		workerGroupValue  = "test-group"
		terminatingReason = "ScaleDown"
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

		// pregenerate the resource
		resource = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   resourceName,
				Labels: map[string]string{corev1.LabelTopologyZone: topologyZone},
			},
		}

		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			By("Cleanup the specific hypervisor")
			hypervisor := &kvmv1.Hypervisor{ObjectMeta: metav1.ObjectMeta{Name: resource.Name}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())

			By("Cleanup the specific node")
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, resource))).To(Succeed())
		})
	})

	Context("When reconciling a node", func() {
		BeforeEach(func(ctx SpecContext) {
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
			Expect(hypervisor.Labels[corev1.LabelTopologyZone]).To(Equal(topologyZone))
			Expect(hypervisor.Spec.Maintenance).To(BeEmpty())
			Expect(hypervisor.Spec.CustomTraits).To(BeEmpty())
			Expect(hypervisor.Spec.Aggregates).To(BeEmpty())
			Expect(hypervisor.Spec.LifecycleEnabled).To(BeFalse())
			Expect(hypervisor.Spec.SkipTests).To(BeFalse()) // Doesn't really matter with lifecycle disabled
		})

		When("the aggregate annotation is set but empty", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding an empty aggregate annotation to the node and reconciling again")
				// Add an empty aggregate annotation to the node
				labeledResource := resource.DeepCopy()
				if labeledResource.Annotations == nil {
					labeledResource.Annotations = map[string]string{}
				}
				labeledResource.Annotations[annotationAggregates] = ""
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should set the AZ and test aggregate on the Hypervisor resource", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.Aggregates).To(ContainElements(topologyZone))
			})
		})

		When("the aggregate annotation is set to some value", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding an empty aggregate annotation to the node and reconciling again")
				// Add an empty aggregate annotation to the node
				labeledResource := resource.DeepCopy()
				if labeledResource.Annotations == nil {
					labeledResource.Annotations = map[string]string{}
				}
				labeledResource.Annotations[annotationAggregates] = testAggregateName
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should not set the AZ aggregate on the Hypervisor resource", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.Aggregates).To(ContainElements(topologyZone, testAggregateName))
			})
		})

		When("the custom traits annotation is set but empty", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding an empty custom traits annotation to the node and reconciling again")
				// Add an empty custom traits annotation to the node
				labeledResource := resource.DeepCopy()
				if labeledResource.Annotations == nil {
					labeledResource.Annotations = map[string]string{}
				}
				labeledResource.Annotations[annotationCustomTraits] = ""
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should set no custom traits on the Hypervisor resource", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.CustomTraits).To(BeEmpty())
			})
		})

		When("the custom traits annotation is set to some value", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding a custom traits annotation to the node and reconciling again")
				// Add a custom traits annotation to the node
				labeledResource := resource.DeepCopy()
				if labeledResource.Annotations == nil {
					labeledResource.Annotations = map[string]string{}
				}
				labeledResource.Annotations[annotationCustomTraits] = customTrait
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should set the custom trait on the Hypervisor resource", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.CustomTraits).To(ContainElements(customTrait))
			})
		})

		When("a label is added to the node", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding a label annotation to the node and reconciling again")
				// Add a label annotation to the node
				labeledResource := resource.DeepCopy()
				labeledResource.Labels[workerGroupLabel] = workerGroupValue
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())

				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should set the label on the Hypervisor resource", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Labels).To(HaveKeyWithValue(workerGroupLabel, workerGroupValue))
			})
		})

		When("the node-hypervisor-lifecycle label is set to true", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding the node-hypervisor-lifecycle label to the node and reconciling again")
				// Add the node-hypervisor-lifecycle label to the node
				labeledResource := resource.DeepCopy()
				labeledResource.Labels[labelLifecycleMode] = "true"
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())
				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reflect that to the Hypervisor spec", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.LifecycleEnabled).To(BeTrue())
				Expect(hypervisor.Spec.SkipTests).To(BeFalse())
			})
		})

		When("the node-hypervisor-lifecycle label is set to skip-tests", func() {
			BeforeEach(func(ctx SpecContext) {
				By("Adding the node-hypervisor-lifecycle label to the node and reconciling again")
				// Add the node-hypervisor-lifecycle label to the node
				labeledResource := resource.DeepCopy()
				labeledResource.Labels[labelLifecycleMode] = "skip-tests"
				Expect(k8sClient.Patch(ctx, labeledResource, client.Merge)).To(Succeed())
				_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should reflect that to the Hypervisor spec", func(ctx SpecContext) {
				// Get the Hypervisor resource again
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Spec.LifecycleEnabled).To(BeTrue())
				Expect(hypervisor.Spec.SkipTests).To(BeTrue())
			})
		})
	})

	Context("When reconciling a terminating node", func() {
		BeforeEach(func(ctx SpecContext) {
			// Mark the node as terminating
			resource.Finalizers = append(resource.Finalizers, "example.com/test-finalizer")
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				resource.Finalizers = []string{}
				resource.ResourceVersion = ""
				Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			})

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, resource)).To(Succeed())
			// Update node condition
			resource.Status.Conditions = append(resource.Status.Conditions, corev1.NodeCondition{
				Type:    "Terminating",
				Status:  corev1.ConditionTrue,
				Reason:  terminatingReason,
				Message: "Node is terminating",
			})
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

		})

		Context("and the Hypervisor resource does not exists", func() {
			It("should successfully reconcile the terminating node", func(ctx SpecContext) {
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
				Expect(updatedHypervisor.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Reason", terminatingReason),
						HaveField("Status", metav1.ConditionFalse),
					),
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeTerminating),
						HaveField("Reason", terminatingReason),
						HaveField("Status", metav1.ConditionTrue),
					),
				))
			})
		})

		Context("and the Hypervisor resource does exists", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{
					ObjectMeta: metav1.ObjectMeta{
						Name: resource.Name,
					},
					Spec: kvmv1.HypervisorSpec{
						Maintenance: kvmv1.MaintenanceUnset,
					},
				}
				Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
				DeferCleanup(func(ctx SpecContext) {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
				})
			})

			Context("and the Hypervisor resource already has a Ready Condition set to false", func() {
				BeforeEach(func(ctx SpecContext) {
					hypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
					meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
						Type:   kvmv1.ConditionTypeReady,
						Status: metav1.ConditionFalse,
						Reason: "SomeOtherReason",
					})
					Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
				})

				It("should not update the existing Ready Condition with the new reason", func(ctx SpecContext) {
					for range 2 {
						_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
							NamespacedName: types.NamespacedName{Name: resource.Name},
						})
						Expect(err).NotTo(HaveOccurred())
					}

					// Get the Hypervisor resource again
					updatedHypervisor := &kvmv1.Hypervisor{}
					Expect(k8sClient.Get(ctx, hypervisorName, updatedHypervisor)).To(Succeed())
					Expect(updatedHypervisor.Status.Conditions).To(ContainElement(
						SatisfyAll(
							HaveField("Type", kvmv1.ConditionTypeReady),
							HaveField("Reason", "SomeOtherReason"),
							HaveField("Status", metav1.ConditionFalse),
						),
					))
				})
			})

			It("should successfully reconcile the terminating node", func(ctx SpecContext) {
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
				// Not sure, if that is a good idea, but that is the current behaviour
				// We expect another operator to set the Maintenance field to Termination
				Expect(updatedHypervisor.Spec.Maintenance).NotTo(Equal(kvmv1.MaintenanceTermination))
				Expect(updatedHypervisor.Status.Conditions).To(ContainElements(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeReady),
						HaveField("Reason", terminatingReason),
						HaveField("Status", metav1.ConditionFalse),
					),
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeTerminating),
						HaveField("Reason", terminatingReason),
						HaveField("Status", metav1.ConditionTrue),
					),
				))
			})
		})
	})
})
