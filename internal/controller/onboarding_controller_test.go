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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

var _ = Describe("Onboarding Controller", func() {
	const (
		hypervisorName = "some-test"
	)

	var (
		onboardingReconciler *OnboardingController
		namespacedName       = types.NamespacedName{Name: hypervisorName}
	)

	Context("When reconciling a hypervisor", func() {
		ctx := context.Background() //nolint:govet

		reconcileLoop := func(steps int) (res ctrl.Result, err error) {
			req := ctrl.Request{NamespacedName: namespacedName}
			for range steps {
				res, err = onboardingReconciler.Reconcile(ctx, req)
				if err != nil {
					return
				}
			}
			return
		}

		BeforeEach(func() {
			onboardingReconciler = &OnboardingController{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the resource for the Kind Hypervisor")
			hv := &kvmv1.Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: hypervisorName,
				},
				Spec: kvmv1.HypervisorSpec{},
			}
			Expect(k8sClient.Create(ctx, hv)).To(Succeed())

			DeferCleanup(func(ctx context.Context) {
				By("Cleanup the specific hypervisor CRO")
				Expect(client.IgnoreAlreadyExists(k8sClient.Delete(ctx, hv))).To(Succeed())
			})
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			_, err := reconcileLoop(1)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
