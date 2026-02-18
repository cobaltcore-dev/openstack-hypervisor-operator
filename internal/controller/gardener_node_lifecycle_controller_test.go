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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Gardener Maintenance Controller", func() {
	const nodeName = "node-test"
	var (
		controller      *GardenerNodeLifecycleController
		name            = types.NamespacedName{Name: nodeName}
		reconcileReq    = ctrl.Request{NamespacedName: name}
		maintenanceName = types.NamespacedName{Name: fmt.Sprintf("maint-%v", nodeName), Namespace: "kube-system"}
	)

	BeforeEach(func(ctx SpecContext) {
		controller = &GardenerNodeLifecycleController{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		By("creating the core resource for the Kind Node")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			By("Cleanup the specific node")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		By("creating the core resource for the Kind hypervisor")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			err := k8sClient.Delete(ctx, hypervisor)
			Expect(k8sclient.IgnoreNotFound(err)).To(Succeed())
		})
	})

	Context("When reconciling a node", func() {
		JustBeforeEach(func(ctx SpecContext) {
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())
		})
		It("should create a poddisruptionbudget", func(ctx SpecContext) {
			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, maintenanceName, pdb)).To(Succeed())
			Expect(pdb.Spec.MinAvailable).To(HaveField("IntVal", BeNumerically("==", 1)))
		})

		It("should create a failing deployment to signal onboarding not being completed", func(ctx SpecContext) {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, maintenanceName, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].StartupProbe.Exec.Command).To(Equal([]string{"/bin/false"}))
		})

		When("the node has been onboarded", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, name, hypervisor)).To(Succeed())
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			It("should create a deployment with onboarding completed", func(ctx SpecContext) {
				dep := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, maintenanceName, dep)).To(Succeed())
				Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
				Expect(dep.Spec.Template.Spec.Containers[0].StartupProbe.Exec.Command).To(Equal([]string{"/bin/true"}))
			})
		})

		When("the node has been offboarded", func() {
			BeforeEach(func(ctx SpecContext) {
				hypervisor := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, name, hypervisor)).To(Succeed())
				meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeOffboarded,
					Status:  metav1.ConditionTrue,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
			})

			It("should update the poddisruptionbudget to minAvailable 0", func(ctx SpecContext) {
				pdb := &policyv1.PodDisruptionBudget{}
				Expect(k8sClient.Get(ctx, maintenanceName, pdb)).To(Succeed())
				Expect(pdb.Spec.MinAvailable).To(HaveField("IntVal", BeNumerically("==", int32(0))))
			})
		})

	})

	Context("When hypervisor does not exist", func() {
		It("should succeed without error", func(ctx SpecContext) {
			// Delete the hypervisor - controller should handle this gracefully with IgnoreNotFound
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, name, hypervisor)).To(Succeed())
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())

			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When lifecycle is not enabled", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, name, hypervisor)).To(Succeed())
			hypervisor.Spec.LifecycleEnabled = false
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
		})

		It("should return early without error", func(ctx SpecContext) {
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When node is terminating and offboarded", func() {
		BeforeEach(func(ctx SpecContext) {
			// Set node as terminating and add required labels for disableInstanceHA
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, name, node)).To(Succeed())
			node.Labels = map[string]string{
				corev1.LabelHostname:          nodeName,
				"topology.kubernetes.io/zone": "test-zone",
			}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "Terminating",
				Status: corev1.ConditionTrue,
			})
			Expect(k8sClient.Update(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			// Set hypervisor as onboarded and offboarded
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, name, hypervisor)).To(Succeed())
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOnboarding,
				Status:  metav1.ConditionFalse,
				Reason:  "Onboarded",
				Message: "Onboarding completed",
			})
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOffboarded,
				Status:  metav1.ConditionTrue,
				Reason:  "Offboarded",
				Message: "Offboarding successful",
			})
			Expect(k8sClient.Status().Update(ctx, hypervisor)).To(Succeed())
		})

		It("should allow pod eviction by setting the PDB to minAvailable 0", func(ctx SpecContext) {
			_, err := controller.Reconcile(ctx, reconcileReq)
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, maintenanceName, pdb)).To(Succeed())
			Expect(pdb.Spec.MinAvailable).To(HaveField("IntVal", BeNumerically("==", int32(0))))
		})
	})
})
