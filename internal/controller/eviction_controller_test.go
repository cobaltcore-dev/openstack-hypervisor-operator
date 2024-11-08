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
	"errors"
	"fmt"
	"math/rand"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/scheduler"
)

type MockScheduler struct {
	scheduler.Scheduler
	hypervisor                *openstack.Hypervisor
	tryAcquireHypervisorError error
	pollHypervisorError       error
	disableHypervisorError    error
	enableHypervisorError     error
}

func (s *MockScheduler) TryAcquireHypervisor(ctx context.Context, hypervisorName string) (*openstack.Hypervisor, error) {
	return s.hypervisor, s.tryAcquireHypervisorError
}

func (s *MockScheduler) UndoAcquireHypervisor(hypervisorName string) {
}

func (s *MockScheduler) PollHypervisor(ctx context.Context, hypervisorName string, withServers bool) (*openstack.Hypervisor, error) {
	return s.hypervisor, s.pollHypervisorError
}

func (s *MockScheduler) DisableHypervisor(ctx context.Context, hypervisorName, reason string) error {
	return s.disableHypervisorError
}

func (s *MockScheduler) EnableHypervisor(ctx context.Context, hypervisorName, reason string) error {
	return s.enableHypervisorError
}

var _ = Describe("Eviction Controller", func() {
	const resourceName = "test-resource"
	const hypervisorName = "test-hypervisor"
	var controllerReconciler *EvictionReconciler
	var mockScheduler *MockScheduler

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	reconcileLoop := func(steps int) (res ctrl.Result, err error) {
		for i := 0; i < steps; i++ {
			res, err = controllerReconciler.Reconcile(ctx, request{NamespacedName: typeNamespacedName, clusterName: "self", client: k8sClient})
			if err != nil {
				return
			}
		}

		return
	}

	BeforeEach(func() {
		By("Setting up the OpenStack http mock server")
		testhelper.SetupHTTP()
	})

	AfterEach(func() {
		resource := &kvmv1.Eviction{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				Expect(err).ShouldNot(HaveOccurred())
			}
		} else {
			By("Cleanup the specific resource instance Eviction")
			Expect(controllerReconciler).NotTo(BeNil())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err := reconcileLoop(1)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).Should(HaveOccurred())
		}

		By("Tearing down the OpenStack http mock server")
		testhelper.TeardownHTTP()
		controllerReconciler = nil
	})

	Describe("API validation", func() {
		When("creating an eviction without hypervisor", func() {
			It("it should fail creating the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason: "test-reason",
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.hypervisor: Invalid value: "": spec.hypervisor in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction without reason", func() {
			It("it should fail creating the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Hypervisor: hypervisorName,
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.reason: Invalid value: "": spec.reason in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction with reason and hypervisor", func() {
			It("it should successfully create the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})
		})
	})

	Describe("Reconciliation", func() {
		Describe("an eviction for 'test-hypervisor'", func() {
			BeforeEach(func() {
				By("Creating the resource")
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: hypervisorName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Creating the scheduler")

				mockScheduler = &MockScheduler{}

				By("Creating the controller")
				controllerReconciler = &EvictionReconciler{
					serviceClient: client.ServiceClient(),
					scheduler:     mockScheduler,
					rand:          rand.New(rand.NewSource(42)),
				}
			})

			When("hypervisor is not found in openstack", func() {
				const errorString = "no hypervisor found"
				BeforeEach(func() {
					mockScheduler.tryAcquireHypervisorError = errors.New(errorString)
				})
				It("should fail reconciliation", func() {
					_, err := reconcileLoop(1)
					Expect(err).To(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be false due to missing hypervisor
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, "Eviction")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionFalse))
					Expect(reconcileStatus.Reason).To(Equal("Failed"))
					Expect(reconcileStatus.Message).To(ContainSubstring(errorString))
					Expect(resource.Status.HypervisorServiceId).To(Equal(""))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})

			})
			When("enabled hypervisor has no servers", func() {
				BeforeEach(func() {
					mockScheduler.hypervisor = &openstack.Hypervisor{HypervisorHostname: hypervisorName, Status: "enabled"}
				})
				It("should succeed the reconciliation", func() {
					_, err := reconcileLoop(4)
					Expect(err).NotTo(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be true
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, "Eviction")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal("Update"))
					Expect(reconcileStatus.Message).To(ContainSubstring("Host disabled"))

					// expect reconciliation to be successfully finished
					reconcileStatus = meta.FindStatusCondition(resource.Status.Conditions, "Reconciling")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal("Reconciled"))

					Expect(resource.GetFinalizers()).NotTo(BeEmpty())
				})
			})
			When("disabled hypervisor has no servers", func() {
				BeforeEach(func() {
					mockScheduler.hypervisor = &openstack.Hypervisor{HypervisorHostname: hypervisorName, Status: "disabled"}
					mockScheduler.hypervisor.Service.DisabledReason = "Who knows"
				})
				It("should succeed the reconciliation", func() {
					_, err := reconcileLoop(4)
					Expect(err).NotTo(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect eviction condition to be true
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, "Eviction")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal("Update"))
					Expect(reconcileStatus.Message).To(ContainSubstring("already disabled"))

					// expect reconciliation to be successfully finished
					reconcileStatus = meta.FindStatusCondition(resource.Status.Conditions, "Reconciling")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal("Reconciled"))

					Expect(resource.GetFinalizers()).To(BeEmpty())
				})
			})
		})
	})
})
