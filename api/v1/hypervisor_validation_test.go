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

package v1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestHypervisorSpecValidation tests the CEL validation rule for HypervisorSpec
// The rule: oldSelf.maintenance != 'termination' || self.maintenance == 'ha' || self == oldSelf
// This ensures that when maintenance is set to 'termination', the spec cannot be modified
// unless the maintenance field is changed to 'ha'
var _ = Describe("Hypervisor Spec CEL Validation", func() {
	var (
		hypervisor     *Hypervisor
		hypervisorName types.NamespacedName
	)

	BeforeEach(func(ctx SpecContext) {
		hypervisorName = types.NamespacedName{
			Name: "test-hypervisor",
		}

		hypervisor = &Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hypervisorName.Name,
			},
			Spec: HypervisorSpec{
				OperatingSystemVersion: "1.0",
				LifecycleEnabled:       true,
				HighAvailability:       true,
				EvacuateOnReboot:       true,
				InstallCertificate:     true,
				Maintenance:            MaintenanceManual,
			},
		}

		Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
		})
	})

	Context("When maintenance is NOT termination", func() {
		It("should allow changes to any spec fields", func(ctx SpecContext) {
			// Update version and other fields
			hypervisor.Spec.OperatingSystemVersion = "2.0"
			hypervisor.Spec.HighAvailability = false
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Verify the update was successful
			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.OperatingSystemVersion).To(Equal("2.0"))
			Expect(updated.Spec.HighAvailability).To(BeFalse())
		})

		It("should allow changing maintenance to termination", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceTermination
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.Maintenance).To(Equal(MaintenanceTermination))
		})
	})

	Context("When maintenance IS termination", func() {
		BeforeEach(func(ctx SpecContext) {
			// Set maintenance to termination
			hypervisor.Spec.Maintenance = MaintenanceTermination
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Refresh to get latest version
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())
		})

		It("should reject changes to version field", func(ctx SpecContext) {
			hypervisor.Spec.OperatingSystemVersion = "2.0"
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should reject changes to boolean fields", func(ctx SpecContext) {
			hypervisor.Spec.LifecycleEnabled = false
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should reject changes to array fields", func(ctx SpecContext) {
			hypervisor.Spec.CustomTraits = []string{"new-trait"}
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should reject changing maintenance to manual", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceManual
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should reject changing maintenance to auto", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceAuto
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should reject changing maintenance to empty", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceUnset
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable when maintenance is 'termination'"))
		})

		It("should allow changing maintenance to ha", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceHA
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.Maintenance).To(Equal(MaintenanceHA))
		})

		It("should allow changing maintenance to ha with other spec changes", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceHA
			hypervisor.Spec.OperatingSystemVersion = "2.0"
			hypervisor.Spec.LifecycleEnabled = false
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.Maintenance).To(Equal(MaintenanceHA))
			Expect(updated.Spec.OperatingSystemVersion).To(Equal("2.0"))
			Expect(updated.Spec.LifecycleEnabled).To(BeFalse())
		})

		It("should allow no-op updates (no changes)", func(ctx SpecContext) {
			// Update with same values should succeed
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())
		})
	})

	Context("When creating a new Hypervisor", func() {
		It("should allow creation with maintenance set to termination", func(ctx SpecContext) {
			newHypervisor := &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "new-test-hypervisor",
				},
				Spec: HypervisorSpec{
					Maintenance:            MaintenanceTermination,
					OperatingSystemVersion: "1.0",
					LifecycleEnabled:       true,
					HighAvailability:       true,
					EvacuateOnReboot:       true,
					InstallCertificate:     true,
				},
			}

			Expect(k8sClient.Create(ctx, newHypervisor)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, newHypervisor))).To(Succeed())
			})

			created := &Hypervisor{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "new-test-hypervisor"}, created)).To(Succeed())
			Expect(created.Spec.Maintenance).To(Equal(MaintenanceTermination))
		})
	})
})

// TestMaintenanceConstants verifies the maintenance mode constants
var _ = Describe("Maintenance Constants", func() {
	It("should have correct constant values", func() {
		Expect(MaintenanceUnset).To(Equal(""))
		Expect(MaintenanceManual).To(Equal("manual"))
		Expect(MaintenanceAuto).To(Equal("auto"))
		Expect(MaintenanceHA).To(Equal("ha"))
		Expect(MaintenanceTermination).To(Equal("termination"))
	})
})
