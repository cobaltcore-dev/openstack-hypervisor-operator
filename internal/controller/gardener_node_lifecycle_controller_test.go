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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("GardenerNodeLifecycleController", func() {
	var (
		controller     *GardenerNodeLifecycleController
		hypervisorName = types.NamespacedName{Name: "hv-test"}
		podName        = types.NamespacedName{Name: fmt.Sprintf("maint-%v", hypervisorName.Name), Namespace: "kube-system"}
	)

	BeforeEach(func() {
		controller = &GardenerNodeLifecycleController{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		By("Creating a Hypervisor resource with LifecycleEnabled")
		hypervisor := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hypervisorName.Name,
				Namespace: hypervisorName.Namespace,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		DeferCleanup(func(ctx context.Context) {
			By("Deleting the Hypervisor resource")
			hypervisor := &kvmv1.Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
			Expect(k8sClient.Delete(ctx, hypervisor)).To(Succeed())
		})
	})

	// After the setup in JustBefore, we want to reconcile
	JustBeforeEach(func(ctx context.Context) {
		req := ctrl.Request{NamespacedName: hypervisorName}
		for range 3 {
			_, err := controller.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	Context("setting the pod disruption budget", func() {
		When("not evicted", func() {
			It("should set the pdb to minimum 1", func() {
				pdb := &policyv1.PodDisruptionBudget{}
				Expect(k8sClient.Get(ctx, podName, pdb)).To(Succeed())
				Expect(pdb.Spec.MinAvailable).To(HaveValue(HaveField("IntVal", BeEquivalentTo(1))))
			})
		})
		When("evicted", func() {
			BeforeEach(func() {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    kvmv1.ConditionTypeEvicting,
					Status:  metav1.ConditionFalse,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
			})

			It("should set the pdb to minimum 0", func() {
				pdb := &policyv1.PodDisruptionBudget{}
				Expect(k8sClient.Get(ctx, podName, pdb)).To(Succeed())
				Expect(pdb.Spec.MinAvailable).To(HaveValue(HaveField("IntVal", BeEquivalentTo(0))))
			})
		})
	})

	Context("create a signalling deployment", func() {
		When("onboarding not completed", func() {
			It("should create a failing deployment for the node", func() {
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, podName, deployment)).To(Succeed())
				Expect(deployment.Spec.Template.Spec.Containers).To(
					ContainElement(
						HaveField("StartupProbe",
							HaveField("ProbeHandler",
								HaveField("Exec",
									HaveField("Command", ContainElements("/bin/false")))))))
			})
		})
		When("onboarding is completed", func() {
			BeforeEach(func() {
				hv := &kvmv1.Hypervisor{}
				Expect(k8sClient.Get(ctx, hypervisorName, hv)).To(Succeed())
				meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
					Type:    ConditionTypeOnboarding,
					Status:  metav1.ConditionFalse,
					Reason:  "dontcare",
					Message: "dontcare",
				})
				Expect(k8sClient.Status().Update(ctx, hv)).To(Succeed())
			})

			It("should create a succeeding deployment for the node", func() {
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, podName, deployment)).To(Succeed())
				Expect(deployment.Spec.Template.Spec.Containers).To(
					ContainElement(
						HaveField("StartupProbe",
							HaveField("ProbeHandler",
								HaveField("Exec",
									HaveField("Command", ContainElements("/bin/true")))))))
			})
		})
	})
})
