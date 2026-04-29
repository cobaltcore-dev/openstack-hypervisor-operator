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
	"fmt"

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
				MaintenanceReason:      "Test maintenance reason",
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

// TestMaintenanceReasonValidation tests the CEL validation rule for MaintenanceReason
// The rule: !has(self.maintenance) || self.maintenance != 'manual' || (has(self.maintenanceReason) && self.maintenanceReason != ”)
// This ensures that when maintenance is set to 'manual', maintenanceReason must be non-empty
var _ = Describe("MaintenanceReason CEL Validation", func() {
	var (
		hypervisor     *Hypervisor
		hypervisorName types.NamespacedName
	)

	Context("When creating a new Hypervisor", func() {
		AfterEach(func(ctx SpecContext) {
			if hypervisor != nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
			}
		})

		It("should allow creation with maintenance='manual' and a non-empty maintenanceReason", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-hypervisor-manual-with-reason",
				},
				Spec: HypervisorSpec{
					OperatingSystemVersion: "1.0",
					LifecycleEnabled:       true,
					Maintenance:            MaintenanceManual,
					MaintenanceReason:      "Hardware upgrade required",
				},
			}

			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			created := &Hypervisor{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hypervisor.Name}, created)).To(Succeed())
			Expect(created.Spec.Maintenance).To(Equal(MaintenanceManual))
			Expect(created.Spec.MaintenanceReason).To(Equal("Hardware upgrade required"))
		})

		It("should reject creation with maintenance='manual' but empty maintenanceReason", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-hypervisor-manual-empty-reason",
				},
				Spec: HypervisorSpec{
					Maintenance:       MaintenanceManual,
					MaintenanceReason: "",
				},
			}

			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maintenanceReason must be non-empty when maintenance is 'manual'"))
		})

		It("should reject creation with maintenance='manual' but missing maintenanceReason", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-hypervisor-manual-no-reason",
				},
				Spec: HypervisorSpec{
					Maintenance: MaintenanceManual,
				},
			}

			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maintenanceReason must be non-empty when maintenance is 'manual'"))
		})

		It("should allow creation with non-manual maintenance modes without maintenanceReason", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-hypervisor-auto-no-reason",
				},
				Spec: HypervisorSpec{
					Maintenance: MaintenanceAuto,
				},
			}

			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			created := &Hypervisor{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hypervisor.Name}, created)).To(Succeed())
			Expect(created.Spec.Maintenance).To(Equal(MaintenanceAuto))
		})

		It("should allow creation with empty maintenance and no maintenanceReason", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-hypervisor-no-maintenance",
				},
				Spec: HypervisorSpec{
					LifecycleEnabled: true,
				},
			}

			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			created := &Hypervisor{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hypervisor.Name}, created)).To(Succeed())
			Expect(created.Spec.Maintenance).To(Equal(""))
		})
	})

	Context("When updating an existing Hypervisor", func() {
		BeforeEach(func(ctx SpecContext) {
			hypervisorName = types.NamespacedName{
				Name: "test-hypervisor-update",
			}

			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{
					Name: hypervisorName.Name,
				},
				Spec: HypervisorSpec{
					LifecycleEnabled: true,
					Maintenance:      MaintenanceAuto,
				},
			}

			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})

		AfterEach(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
		})

		It("should allow updating to maintenance='manual' with a non-empty maintenanceReason", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceManual
			hypervisor.Spec.MaintenanceReason = "Planned maintenance window"
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.Maintenance).To(Equal(MaintenanceManual))
			Expect(updated.Spec.MaintenanceReason).To(Equal("Planned maintenance window"))
		})

		It("should reject updating to maintenance='manual' without maintenanceReason", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceManual
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maintenanceReason must be non-empty when maintenance is 'manual'"))
		})

		It("should reject updating to maintenance='manual' with empty maintenanceReason", func(ctx SpecContext) {
			hypervisor.Spec.Maintenance = MaintenanceManual
			hypervisor.Spec.MaintenanceReason = ""
			err := k8sClient.Update(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maintenanceReason must be non-empty when maintenance is 'manual'"))
		})

		It("should allow updating from manual to another maintenance mode", func(ctx SpecContext) {
			// First set to manual with reason
			hypervisor.Spec.Maintenance = MaintenanceManual
			hypervisor.Spec.MaintenanceReason = "Initial reason"
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Refresh hypervisor
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

			// Update to auto (reason becomes optional and can be cleared)
			hypervisor.Spec.Maintenance = MaintenanceAuto
			hypervisor.Spec.MaintenanceReason = ""
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.Maintenance).To(Equal(MaintenanceAuto))
		})

		It("should allow updating maintenanceReason when maintenance is already 'manual'", func(ctx SpecContext) {
			// First set to manual with reason
			hypervisor.Spec.Maintenance = MaintenanceManual
			hypervisor.Spec.MaintenanceReason = "Initial reason"
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			// Refresh hypervisor
			Expect(k8sClient.Get(ctx, hypervisorName, hypervisor)).To(Succeed())

			// Update the reason
			hypervisor.Spec.MaintenanceReason = "Updated reason"
			Expect(k8sClient.Update(ctx, hypervisor)).To(Succeed())

			updated := &Hypervisor{}
			Expect(k8sClient.Get(ctx, hypervisorName, updated)).To(Succeed())
			Expect(updated.Spec.MaintenanceReason).To(Equal("Updated reason"))
		})
	})
})

// TestGroupsCELValidation tests the CEL validation rules for spec.groups:
// 1. Exactly one group type must be set per entry (union rule on Group)
// 2. Field-level validation (minLength) on trait name, aggregate name, and aggregate UUID
var _ = Describe("Groups CEL Validation", func() {
	var (
		hypervisor     *Hypervisor
		hypervisorName types.NamespacedName
		counter        int
	)

	BeforeEach(func(ctx SpecContext) {
		counter++
		hypervisorName = types.NamespacedName{
			Name: fmt.Sprintf("test-groups-hv-%d", counter),
		}
	})

	AfterEach(func(ctx SpecContext) {
		if hypervisor != nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, hypervisor))).To(Succeed())
			hypervisor = nil
		}
	})

	Context("Union rule: exactly one group type per entry", func() {
		It("should accept a group with only trait set", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Trait: &TraitGroup{Name: "HW_CPU_X86_AVX2"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})

		It("should accept a group with only aggregate set", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Aggregate: &AggregateGroup{Name: "fast-storage", UUID: "abc-123"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})

		It("should accept mixed trait and aggregate entries", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Trait: &TraitGroup{Name: "HW_CPU_X86_AVX2"}},
						{Aggregate: &AggregateGroup{Name: "fast-storage", UUID: "abc-123"}},
						{Trait: &TraitGroup{Name: "COMPUTE_STATUS_DISABLED"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})

		It("should reject a group with both trait and aggregate set", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{
							Trait:     &TraitGroup{Name: "HW_CPU_X86_AVX2"},
							Aggregate: &AggregateGroup{Name: "fast-storage", UUID: "abc-123"},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one group type must be set"))
		})

		It("should reject a group with neither trait nor aggregate set", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{},
					},
				},
			}
			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one group type must be set"))
		})
	})

	Context("Field validation", func() {
		It("should reject a trait with empty name", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Trait: &TraitGroup{Name: ""}},
					},
				},
			}
			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.groups[0].trait.name"))
		})

		It("should reject an aggregate with empty name", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Aggregate: &AggregateGroup{Name: "", UUID: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}},
					},
				},
			}
			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.groups[0].aggregate.name"))
		})

		It("should reject an aggregate with empty UUID", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Aggregate: &AggregateGroup{Name: "fast-storage", UUID: ""}},
					},
				},
			}
			err := k8sClient.Create(ctx, hypervisor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.groups[0].aggregate.uuid"))
		})

		It("should accept an aggregate without metadata", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Aggregate: &AggregateGroup{Name: "fast-storage", UUID: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})

		It("should accept an aggregate with metadata", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{
						{Aggregate: &AggregateGroup{
							Name:     "fast-storage",
							UUID:     "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
							Metadata: map[string]string{"ssd": "true"},
						}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())

			created := &Hypervisor{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hypervisor), created)).To(Succeed())
			Expect(created.Spec.Groups).To(HaveLen(1))
			Expect(created.Spec.Groups[0].Aggregate).NotTo(BeNil())
			Expect(created.Spec.Groups[0].Aggregate.Metadata).To(HaveKeyWithValue("ssd", "true"))
		})

		It("should accept an empty groups list", func(ctx SpecContext) {
			hypervisor = &Hypervisor{
				ObjectMeta: metav1.ObjectMeta{Name: hypervisorName.Name},
				Spec: HypervisorSpec{
					Groups: []Group{},
				},
			}
			Expect(k8sClient.Create(ctx, hypervisor)).To(Succeed())
		})
	})
})

var _ = Describe("Group Helper Functions", func() {
	groups := []Group{
		{Trait: &TraitGroup{Name: "HW_CPU_X86_AVX2"}},
		{Trait: &TraitGroup{Name: "COMPUTE_STATUS_DISABLED"}},
		{Aggregate: &AggregateGroup{Name: "fast-storage", UUID: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", Metadata: map[string]string{"ssd": "true"}}},
		{Aggregate: &AggregateGroup{Name: "slow-storage", UUID: "b1ffbc99-9c0b-4ef8-bb6d-6bb9bd380a22"}},
	}

	Context("HasTrait", func() {
		It("should return true for an existing trait", func() {
			Expect(HasTrait(groups, "HW_CPU_X86_AVX2")).To(BeTrue())
		})

		It("should return false for a missing trait", func() {
			Expect(HasTrait(groups, "NONEXISTENT")).To(BeFalse())
		})

		It("should return false for an empty list", func() {
			Expect(HasTrait(nil, "HW_CPU_X86_AVX2")).To(BeFalse())
		})
	})

	Context("GetTraits", func() {
		It("should return all trait entries", func() {
			traits := GetTraits(groups)
			Expect(traits).To(HaveLen(2))
			Expect(traits[0].Name).To(Equal("HW_CPU_X86_AVX2"))
			Expect(traits[1].Name).To(Equal("COMPUTE_STATUS_DISABLED"))
		})

		It("should return empty for a list with no traits", func() {
			aggs := []Group{{Aggregate: &AggregateGroup{Name: "a", UUID: "d0eebc99-9c0b-4ef8-bb6d-6bb9bd380a33"}}}
			Expect(GetTraits(aggs)).To(BeEmpty())
		})

		It("should return nil for an empty list", func() {
			Expect(GetTraits(nil)).To(BeNil())
		})
	})

	Context("HasAggregate", func() {
		It("should return true for an existing aggregate UUID", func() {
			Expect(HasAggregate(groups, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")).To(BeTrue())
		})

		It("should return false for a missing aggregate UUID", func() {
			Expect(HasAggregate(groups, "c2ffbc99-9c0b-4ef8-bb6d-6bb9bd380a99")).To(BeFalse())
		})

		It("should return false for an empty list", func() {
			Expect(HasAggregate(nil, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")).To(BeFalse())
		})
	})

	Context("GetAggregates", func() {
		It("should return all aggregate entries", func() {
			aggs := GetAggregates(groups)
			Expect(aggs).To(HaveLen(2))
			Expect(aggs[0].Name).To(Equal("fast-storage"))
			Expect(aggs[0].UUID).To(Equal("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"))
			Expect(aggs[0].Metadata).To(HaveKeyWithValue("ssd", "true"))
			Expect(aggs[1].Name).To(Equal("slow-storage"))
			Expect(aggs[1].UUID).To(Equal("b1ffbc99-9c0b-4ef8-bb6d-6bb9bd380a22"))
		})

		It("should return empty for a list with no aggregates", func() {
			traits := []Group{{Trait: &TraitGroup{Name: "T"}}}
			Expect(GetAggregates(traits)).To(BeEmpty())
		})

		It("should return nil for an empty list", func() {
			Expect(GetAggregates(nil)).To(BeNil())
		})
	})
})
