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
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/global"
)

// envtest does not actually GC pods when their namespace is deleted, so
// each spec gets a fresh namespace via this counter to keep them isolated.
var agentNamespaceCounter atomic.Uint64

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
				Expect(updatedHypervisor.Status.Conditions).To(ContainElement(
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
				Expect(updatedHypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeTerminating),
						HaveField("Reason", terminatingReason),
						HaveField("Status", metav1.ConditionTrue),
					),
				))
			})
		})
	})

	Context("When node lookup fails", func() {
		It("should return error for non-NotFound errors", func(ctx SpecContext) {
			// Try to reconcile a node that doesn't exist - should be gracefully ignored
			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "non-existent-node"},
			})
			// NotFound errors are ignored, so this should not error
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When node has internal IP", func() {
		It("should set internal IP in hypervisor status", func(ctx SpecContext) {
			// First reconcile to create the hypervisor
			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			// Now add internal IP to node status
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, node)).To(Succeed())
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.100"},
				{Type: corev1.NodeHostName, Address: "test-host"},
			}
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			// Reconcile again to update the hypervisor with the IP
			_, err = hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(hypervisor.Status.InternalIP).To(Equal("192.168.1.100"))
		})
	})

	Context("AgentPodsEvicted condition", func() {
		var agentNamespace string

		BeforeEach(func(ctx SpecContext) {
			agentNamespace = fmt.Sprintf("agent-ns-%d", agentNamespaceCounter.Add(1))
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agentNamespace}}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
			})

			// Restrict pod listing to the test namespace.
			global.AgentNamespaces = []string{agentNamespace}
			DeferCleanup(func() { global.AgentNamespaces = nil })

			// The condition is only computed during termination.
			_, err := hypervisorController.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: resource.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
			hypervisor.Spec.LifecycleEnabled = true
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Default for these specs: VM eviction is done. Subcontexts
			// that exercise the "not yet done" path override this.
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeEvicting,
				Status:  metav1.ConditionFalse,
				Reason:  "Succeeded",
				Message: "All VMs evicted",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())

			// The pod list is only issued once the offboarding taint is on the node.
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, node)).To(Succeed())
			base := node.DeepCopy()
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
				Key:    taintKeyOffboarding,
				Effect: corev1.TaintEffectNoExecute,
			})
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(base))).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				fresh := &corev1.Node{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, fresh); err == nil {
					base := fresh.DeepCopy()
					taints := fresh.Spec.Taints[:0]
					for _, t := range fresh.Spec.Taints {
						if t.Key != taintKeyOffboarding {
							taints = append(taints, t)
						}
					}
					fresh.Spec.Taints = taints
					Expect(client.IgnoreNotFound(k8sClient.Patch(ctx, fresh, client.MergeFrom(base)))).To(Succeed())
				}
			})
		})

		createPod := func(ctx SpecContext, name, namespace string, phase corev1.PodPhase, tolerations ...corev1.Toleration) *corev1.Pod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: corev1.PodSpec{
					NodeName: resource.Name,
					Containers: []corev1.Container{
						{Name: "main", Image: "registry.example.com/whatever:latest"},
					},
					Tolerations: tolerations,
				},
				Status: corev1.PodStatus{Phase: phase},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)).To(Succeed())
			pod.Status.Phase = phase
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed())
			})
			return pod
		}

		When("only pods that tolerate the offboarding taint are running", func() {
			BeforeEach(func(ctx SpecContext) {
				createPod(ctx, "tolerator", agentNamespace, corev1.PodRunning, corev1.Toleration{
					Key:      taintKeyOffboarding,
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoExecute,
				})
				// Phase=Succeeded must not count regardless of tolerations.
				createPod(ctx, "old-job", agentNamespace, corev1.PodSucceeded)
			})

			It("should set AgentPodsEvicted=True without requeue", func(ctx SpecContext) {
				result, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())

				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeAgentPodsEvicted),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", "NoAgentPods"),
					),
				))
			})
		})

		When("an agent pod is running on the node and VM eviction is done", func() {
			BeforeEach(func(ctx SpecContext) {
				createPod(ctx, "nova-compute-xyz", agentNamespace, corev1.PodRunning)
			})

			It("should set AgentPodsEvicted=False with reason AgentPodsRunning and requeue", func(ctx SpecContext) {
				result, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(defaultPollTime))

				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeAgentPodsEvicted),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", "AgentPodsRunning"),
						HaveField("Message", ContainSubstring("nova-compute-xyz")),
					),
				))
			})

			It("should also flush InternalIP and Terminating in the same reconcile", func(ctx SpecContext) {
				// Stage a fresh InternalIP and a node-level Terminating condition
				// that the same reconcile pass would normally pick up. The
				// AgentPodsEvicted=False branch must not skip persisting these
				// fields; otherwise the Hypervisor status remains stale until a
				// later reconcile.
				node := &corev1.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, node)).To(Succeed())
				base := node.DeepCopy()
				node.Status.Addresses = []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.42.7"},
				}
				node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
					Type:    "Terminating",
					Status:  corev1.ConditionTrue,
					Reason:  terminatingReason,
					Message: "Node is terminating",
				})
				Expect(k8sClient.Status().Patch(ctx, node, client.MergeFrom(base))).To(Succeed())

				result, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(defaultPollTime))

				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

				By("persisting AgentPodsEvicted=False")
				Expect(hypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeAgentPodsEvicted),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", "AgentPodsRunning"),
					),
				))

				By("persisting the freshly observed InternalIP")
				Expect(hypervisor.Status.InternalIP).To(Equal("192.168.42.7"))

				By("persisting the propagated Terminating condition")
				Expect(hypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeTerminating),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", terminatingReason),
					),
				))
			})

			It("should still propagate node label changes to the Hypervisor", func(ctx SpecContext) {
				// Stage a node label that the reconcile pass would normally
				// transport to the Hypervisor metadata via
				// syncLabelsAndAnnotations. The AgentPodsEvicted=False branch
				// must not skip this propagation; otherwise the Hypervisor
				// labels remain stale during termination.
				//
				// (The Hypervisor spec is immutable while
				// maintenance=='termination', so only metadata.Labels — not
				// spec-derived fields like aggregates/customTraits — can
				// legitimately change in this state.)
				node := &corev1.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name}, node)).To(Succeed())
				base := node.DeepCopy()
				node.Labels[workerGroupLabel] = workerGroupValue
				Expect(k8sClient.Patch(ctx, node, client.MergeFrom(base))).To(Succeed())

				result, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(defaultPollTime),
					"AgentPodsEvicted=False must still drive a periodic requeue")

				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

				By("transporting the node label to the Hypervisor")
				Expect(hypervisor.Labels).To(HaveKeyWithValue(workerGroupLabel, workerGroupValue))
			})
		})

		When("the only non-tolerating pod is already being deleted", func() {
			// A finalizer keeps the API object around with DeletionTimestamp
			// set, simulating a pod whose containers are shutting down but
			// whose deletion is blocked on some unrelated finalizer.
			const finalizer = "test.kvm.cloud.sap/keep-alive"

			BeforeEach(func(ctx SpecContext) {
				pod := createPod(ctx, "nova-compute-deleting", agentNamespace, corev1.PodRunning)
				pod.Finalizers = []string{finalizer}
				Expect(k8sClient.Update(ctx, pod)).To(Succeed())
				Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

				DeferCleanup(func(ctx SpecContext) {
					fresh := &corev1.Pod{}
					if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, fresh); err == nil {
						fresh.Finalizers = nil
						Expect(client.IgnoreNotFound(k8sClient.Update(ctx, fresh))).To(Succeed())
					}
				})

				// Sanity: the pod must still exist with DeletionTimestamp.
				fresh := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, fresh)).To(Succeed())
				Expect(fresh.DeletionTimestamp).NotTo(BeNil())
			})

			It("should set AgentPodsEvicted=False (deletion-pending pod still counts as running)", func(ctx SpecContext) {
				result, err := hypervisorController.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: resource.Name},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(defaultPollTime))

				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
				Expect(hypervisor.Status.Conditions).To(ContainElement(
					SatisfyAll(
						HaveField("Type", kvmv1.ConditionTypeAgentPodsEvicted),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", "AgentPodsRunning"),
						HaveField("Message", ContainSubstring("nova-compute-deleting")),
					),
				))
			})
		})
	})
})

var _ = Describe("computeAgentPodsEvictedCondition field selector", func() {
	// This test verifies that the pod list issued by computeAgentPodsEvictedCondition
	// uses the spec.nodeName field selector so that only pods on the target node
	// are returned, not all pods in the cluster.
	It("should only list pods scheduled on the target node", func(ctx SpecContext) {
		// Use a client with DisableFor pods, mirroring production config.
		uncachedClient, err := client.New(cfg, client.Options{
			Scheme: k8sscheme.Scheme,
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Pod{}},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "field-selector-test"}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
		})

		// Create a pod on "target-node".
		onTarget := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "on-target", Namespace: ns.Name},
			Spec: corev1.PodSpec{
				NodeName:   "target-node",
				Containers: []corev1.Container{{Name: "c", Image: "registry.example.com/img:latest"}},
			},
		}
		Expect(k8sClient.Create(ctx, onTarget)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, onTarget))).To(Succeed())
		})

		// Create a pod on a different node — must not appear in results.
		onOther := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "on-other", Namespace: ns.Name},
			Spec: corev1.PodSpec{
				NodeName:   "other-node",
				Containers: []corev1.Container{{Name: "c", Image: "registry.example.com/img:latest"}},
			},
		}
		Expect(k8sClient.Create(ctx, onOther)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, onOther))).To(Succeed())
		})

		pods := &corev1.PodList{}
		Expect(uncachedClient.List(ctx, pods,
			client.MatchingFieldsSelector{
				Selector: fields.OneTermEqualSelector("spec.nodeName", "target-node"),
			},
		)).To(Succeed())

		names := make([]string, len(pods.Items))
		for i, p := range pods.Items {
			names[i] = p.Name
		}
		Expect(names).To(ConsistOf("on-target"),
			"field selector spec.nodeName must filter server-side; got pods: %v", names)
	})
})
