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
	"math/rand"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Eviction Controller", func() {
	const resourceName = "test-resource"
	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("Setting up the OpenStack http mock server")
		testhelper.SetupHTTP()
	})

	AfterEach(func() {
		resource := &kvmv1.Eviction{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err == nil || !errors.IsNotFound(err) {
			By("Cleanup the specific resource instance Eviction")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}

		By("Tearing down the OpenStack http mock server")
		testhelper.TeardownHTTP()
	})

	Describe("Reconciling a eviction resource", func() {
		When("creating an eviction without reason", func() {
			It("it should fail creating the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
				}
				expected := fmt.Sprintf(`Eviction.kvm.cloud.sap "%s" is invalid: spec.reason: Invalid value: "": spec.reason in body should be at least 1 chars long`, resourceName)
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(expected))
			})
		})

		When("creating an eviction with reason", func() {
			It("it should successfully create the resource", func() {
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason: "test-reason",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				controllerReconciler := &EvictionReconciler{
					Client:        k8sClient,
					Scheme:        k8sClient.Scheme(),
					ServiceClient: client.ServiceClient(),
					rand:          rand.New(rand.NewSource(42)),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Describe("creating an eviction for 'test-hypervisor' and reconciling", func() {
			var controllerReconciler *EvictionReconciler
			BeforeEach(func() {
				By("Creating the resource")
				resource := &kvmv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kvmv1.EvictionSpec{
						Reason:     "test-reason",
						Hypervisor: "test-hypervisor",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Creating the controller")
				controllerReconciler = &EvictionReconciler{
					Client:        k8sClient,
					Scheme:        k8sClient.Scheme(),
					ServiceClient: client.ServiceClient(),
					rand:          rand.New(rand.NewSource(42)),
				}
			})

			When("hypervisor is not found in openstack", func() {
				It("should fail reconciliation", func() {
					_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
					Expect(err).NotTo(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect evicition condition to be false due to missing hypervisor
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, "Eviction")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionFalse))
					Expect(reconcileStatus.Reason).To(Equal("Failed"))
					Expect(reconcileStatus.Message).To(And(
						ContainSubstring("404 page not found"),
						ContainSubstring("os-hypervisors")),
					)
				})
			})
			When("hypervisor has no servers", func() {
				BeforeEach(func() {
					testhelper.Mux.HandleFunc("/os-hypervisors/detail", func(w http.ResponseWriter, r *http.Request) {
						Expect(r.Method).To(Equal(http.MethodGet))
						w.Header().Add("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, `{"hypervisors": [{"service": {"id": "test-id"}, "servers": []}]}`)).ToNot(BeNil())
					})
					testhelper.Mux.HandleFunc("/os-services/test-id", func(w http.ResponseWriter, r *http.Request) {
						Expect(r.Method).To(Equal(http.MethodPut))
						w.WriteHeader(http.StatusOK)
						Expect(fmt.Fprintf(w, `{"service": {"id": "test-id", "status": "disabled"}}`)).ToNot(BeNil())
					})
				})
				It("should succeed the reconciliation", func() {
					_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
					Expect(err).NotTo(HaveOccurred())

					resource := &kvmv1.Eviction{}
					err = k8sClient.Get(ctx, typeNamespacedName, resource)
					Expect(err).NotTo(HaveOccurred())

					// expect evicition condition to be true
					reconcileStatus := meta.FindStatusCondition(resource.Status.Conditions, "Eviction")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionTrue))
					Expect(reconcileStatus.Reason).To(Equal("Update"))
					Expect(reconcileStatus.Message).To(ContainSubstring("Host disabled"))

					// expect reconciliation to be successfully finished
					reconcileStatus = meta.FindStatusCondition(resource.Status.Conditions, "Reconciling")
					Expect(reconcileStatus).NotTo(BeNil())
					Expect(reconcileStatus.Status).To(Equal(metav1.ConditionFalse))
					Expect(reconcileStatus.Reason).To(Equal("Reconciled"))
				})
			})
		})
	})
})
