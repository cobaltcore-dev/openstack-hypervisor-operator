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

package ready

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

func TestReadyController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ready Controller Suite")
}

var _ = Describe("StatusChangedPredicate", func() {
	var predicate StatusChangedPredicate

	BeforeEach(func() {
		predicate = StatusChangedPredicate{}
	})

	Describe("Create", func() {
		It("should return true for create events", func() {
			e := event.CreateEvent{Object: &kvmv1.Hypervisor{}}
			Expect(predicate.Create(e)).To(BeTrue())
		})
	})

	Describe("Delete", func() {
		It("should return false for delete events", func() {
			e := event.DeleteEvent{Object: &kvmv1.Hypervisor{}}
			Expect(predicate.Delete(e)).To(BeFalse())
		})
	})

	Describe("Update", func() {
		It("should return false when only non-maintenance spec changes", func() {
			oldHv := &kvmv1.Hypervisor{
				Spec: kvmv1.HypervisorSpec{SkipTests: false},
			}
			newHv := &kvmv1.Hypervisor{
				Spec: kvmv1.HypervisorSpec{SkipTests: true},
			}
			e := event.UpdateEvent{ObjectOld: oldHv, ObjectNew: newHv}
			Expect(predicate.Update(e)).To(BeFalse())
		})

		It("should return true when status changes", func() {
			oldHv := &kvmv1.Hypervisor{}
			newHv := &kvmv1.Hypervisor{
				Status: kvmv1.HypervisorStatus{
					Conditions: []metav1.Condition{
						{Type: kvmv1.ConditionTypeOnboarding, Status: metav1.ConditionTrue},
					},
				},
			}
			e := event.UpdateEvent{ObjectOld: oldHv, ObjectNew: newHv}
			Expect(predicate.Update(e)).To(BeTrue())
		})

		It("should return true when maintenance field changes", func() {
			oldHv := &kvmv1.Hypervisor{
				Spec: kvmv1.HypervisorSpec{Maintenance: kvmv1.MaintenanceUnset},
			}
			newHv := &kvmv1.Hypervisor{
				Spec: kvmv1.HypervisorSpec{Maintenance: kvmv1.MaintenanceManual},
			}
			e := event.UpdateEvent{ObjectOld: oldHv, ObjectNew: newHv}
			Expect(predicate.Update(e)).To(BeTrue())
		})

		It("should return false when nothing changes", func() {
			hv := &kvmv1.Hypervisor{
				Spec: kvmv1.HypervisorSpec{SkipTests: true},
			}
			e := event.UpdateEvent{ObjectOld: hv, ObjectNew: hv}
			Expect(predicate.Update(e)).To(BeFalse())
		})

		It("should return false when ObjectOld is nil", func() {
			e := event.UpdateEvent{ObjectOld: nil, ObjectNew: &kvmv1.Hypervisor{}}
			Expect(predicate.Update(e)).To(BeFalse())
		})

		It("should return false when ObjectNew is nil", func() {
			e := event.UpdateEvent{ObjectOld: &kvmv1.Hypervisor{}, ObjectNew: nil}
			Expect(predicate.Update(e)).To(BeFalse())
		})
	})

	Describe("Generic", func() {
		It("should return true for generic events", func() {
			e := event.GenericEvent{Object: &kvmv1.Hypervisor{}}
			Expect(predicate.Generic(e)).To(BeTrue())
		})
	})
})

var _ = Describe("ComputeReadyCondition", func() {
	var hv *kvmv1.Hypervisor

	BeforeEach(func() {
		hv = &kvmv1.Hypervisor{}
	})

	Context("Priority 1: Offboarded", func() {
		It("should return Ready=False with Reason=Offboarded when offboarded", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOffboarded,
				Status: metav1.ConditionTrue,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal("Offboarded"))
		})
	})

	Context("Priority 2: Terminating", func() {
		It("should return Ready=False with Reason=Terminating when terminating", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeTerminating,
				Status: metav1.ConditionTrue,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonTerminating))
		})

		It("should prioritize Offboarded over Terminating", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOffboarded,
				Status: metav1.ConditionTrue,
			})
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeTerminating,
				Status: metav1.ConditionTrue,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Reason).To(Equal("Offboarded"))
		})
	})

	Context("Priority 3: Onboarding in progress", func() {
		It("should return Ready=False when onboarding is in progress", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  kvmv1.ConditionReasonTesting,
				Message: "Running smoke tests",
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonOnboarding))
			Expect(result.Message).To(ContainSubstring("in progress"))
		})

		It("should prioritize onboarding in progress over maintenance", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceManual
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionTrue,
				Reason: kvmv1.ConditionReasonTesting,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonOnboarding))
			Expect(result.Message).To(ContainSubstring("in progress"))
		})
	})

	Context("Priority 4: Maintenance mode", func() {
		It("should return Ready=False with Reason=Evicting when evicting", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceAuto
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeEvicting,
				Status: metav1.ConditionTrue,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyEvicting))
		})

		It("should return Ready=False with Reason=Evicted when evicted", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceAuto
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeEvicting,
				Status: metav1.ConditionFalse,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyEvicted))
		})

		It("should return Ready=False with Reason=Maintenance when in maintenance without eviction", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceManual

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyMaintenance))
		})

		It("should prioritize maintenance over onboarding not started", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceManual
			// No onboarding condition set

			result := ComputeReadyCondition(hv)

			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyMaintenance))
		})

		It("should prioritize maintenance over onboarding aborted", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceManual
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonAborted,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyMaintenance))
		})

		It("should prioritize maintenance over onboarding not succeeded", func() {
			hv.Spec.Maintenance = kvmv1.MaintenanceManual
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonFailed,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyMaintenance))
		})
	})

	Context("Priority 5: Onboarding not started/aborted/not succeeded", func() {
		It("should return Ready=False when no onboarding condition exists", func() {
			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonOnboarding))
			Expect(result.Message).To(ContainSubstring("not started"))
		})

		It("should return Ready=False when onboarding was aborted", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonAborted,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonOnboarding))
			Expect(result.Message).To(ContainSubstring("aborted"))
		})

		It("should return Ready=False when onboarding has non-succeeded reason", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonFailed,
			})

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionFalse))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonOnboarding))
		})
	})

	Context("Priority 6: All checks pass - Ready", func() {
		It("should return Ready=True when all conditions are satisfied", func() {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:   kvmv1.ConditionTypeOnboarding,
				Status: metav1.ConditionFalse,
				Reason: kvmv1.ConditionReasonSucceeded,
			})
			hv.Spec.Maintenance = kvmv1.MaintenanceUnset

			result := ComputeReadyCondition(hv)

			Expect(result.Type).To(Equal(kvmv1.ConditionTypeReady))
			Expect(result.Status).To(Equal(metav1.ConditionTrue))
			Expect(result.Reason).To(Equal(kvmv1.ConditionReasonReadyReady))
		})
	})
})
