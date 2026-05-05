/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
)

// ssaConditionCfg builds a HypervisorApplyConfiguration containing exactly one status condition.
// Using apply configurations (all-pointer structs) ensures that only the explicitly set fields
// appear in the apply body — critical for SSA to claim only those fields and leave all others
// untouched. A plain kvmv1.Hypervisor struct cannot be used because non-pointer struct fields
// (e.g. Status.OperatingSystem) are serialised even when zero-valued, causing the apply to claim
// ownership of those fields and fail CRD validation.
func ssaConditionCfg(name, condType string, condStatus metav1.ConditionStatus) *apiv1.HypervisorApplyConfiguration {
	return apiv1.Hypervisor(name, "").
		WithStatus(apiv1.HypervisorStatus().
			WithConditions(
				k8sacmetav1.Condition().
					WithType(condType).
					WithStatus(condStatus).
					WithReason("SpikeTest").
					WithMessage("set by " + condType).
					WithLastTransitionTime(metav1.Now()),
			),
		)
}

// Spike test demonstrating that Server-Side Apply with +listType=map / +listMapKey=type on
// the Conditions field gives each field manager independent per-condition ownership.
// Without the x-kubernetes-list-type: map annotation in the CRD schema, the API server would
// treat the whole conditions array as atomic — one manager would own all conditions and a second
// manager applying a different condition would silently overwrite the first manager's condition.
var _ = Describe("SSA per-condition field ownership (spike)", func() {
	const (
		hvName     = "hv-ssa-spike"
		managerA   = "controller-a"
		managerB   = "controller-b"
		conditionA = "ConditionA"
		conditionB = "ConditionB"
	)

	BeforeEach(func(ctx SpecContext) {
		hv := &kvmv1.Hypervisor{
			ObjectMeta: metav1.ObjectMeta{
				Name: hvName,
			},
			Spec: kvmv1.HypervisorSpec{
				LifecycleEnabled: true,
			},
		}
		Expect(k8sClient.Create(ctx, hv)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, hv)).To(Succeed())
		})
	})

	It("allows two managers to independently own different conditions without overwriting each other", func(ctx SpecContext) {
		// --- Step 1: manager-a applies ConditionA=True ---
		By("manager-a applies ConditionA=True")
		Expect(k8sClient.Status().Apply(ctx,
			ssaConditionCfg(hvName, conditionA, metav1.ConditionTrue),
			k8sclient.FieldOwner(managerA),
			k8sclient.ForceOwnership,
		)).To(Succeed())

		// --- Step 2: manager-b applies ConditionB=True ---
		By("manager-b applies ConditionB=True")
		Expect(k8sClient.Status().Apply(ctx,
			ssaConditionCfg(hvName, conditionB, metav1.ConditionTrue),
			k8sclient.FieldOwner(managerB),
			k8sclient.ForceOwnership,
		)).To(Succeed())

		// --- Step 3: both conditions must coexist ---
		By("both conditions coexist on the same object")
		hv := &kvmv1.Hypervisor{}
		Expect(k8sClient.Get(ctx, k8sclient.ObjectKey{Name: hvName}, hv)).To(Succeed())

		Expect(hv.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", conditionA),
			HaveField("Status", metav1.ConditionTrue),
		)), "ConditionA should still be present after manager-b applied ConditionB")
		Expect(hv.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", conditionB),
			HaveField("Status", metav1.ConditionTrue),
		)), "ConditionB should be present after manager-b applied it")

		// --- Step 4: managed fields record per-condition ownership ---
		By("managed fields record per-condition ownership for each manager")
		managedFields := hv.GetManagedFields()

		var entryA, entryB *metav1.ManagedFieldsEntry
		for i := range managedFields {
			e := &managedFields[i]
			switch {
			case e.Manager == managerA &&
				e.Operation == metav1.ManagedFieldsOperationApply &&
				e.Subresource == "status":
				entryA = e
			case e.Manager == managerB &&
				e.Operation == metav1.ManagedFieldsOperationApply &&
				e.Subresource == "status":
				entryB = e
			}
		}

		Expect(entryA).NotTo(BeNil(), "expected a managed-fields entry for %s on the status subresource", managerA)
		Expect(entryB).NotTo(BeNil(), "expected a managed-fields entry for %s on the status subresource", managerB)

		// The FieldsV1 JSON encodes owned list-map entries as k:{"type":"<condType>"},
		// so each manager's entry should contain only its own condition type value.
		Expect(string(entryA.FieldsV1.Raw)).To(ContainSubstring(conditionA),
			"manager-a's FieldsV1 should contain the key for ConditionA")
		Expect(string(entryA.FieldsV1.Raw)).NotTo(ContainSubstring(conditionB),
			"manager-a's FieldsV1 should NOT contain the key for ConditionB")
		Expect(string(entryB.FieldsV1.Raw)).To(ContainSubstring(conditionB),
			"manager-b's FieldsV1 should contain the key for ConditionB")
		Expect(string(entryB.FieldsV1.Raw)).NotTo(ContainSubstring(conditionA),
			"manager-b's FieldsV1 should NOT contain the key for ConditionA")

		// --- Step 5: manager-a updates ConditionA; ConditionB must be untouched ---
		By("manager-a updates ConditionA to False; ConditionB must remain True")
		Expect(k8sClient.Status().Apply(ctx,
			ssaConditionCfg(hvName, conditionA, metav1.ConditionFalse),
			k8sclient.FieldOwner(managerA),
			k8sclient.ForceOwnership,
		)).To(Succeed())

		Expect(k8sClient.Get(ctx, k8sclient.ObjectKey{Name: hvName}, hv)).To(Succeed())
		Expect(hv.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", conditionA),
			HaveField("Status", metav1.ConditionFalse),
		)), "ConditionA should now be False")
		Expect(hv.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", conditionB),
			HaveField("Status", metav1.ConditionTrue),
		)), "ConditionB must be untouched by manager-a's update")

		// --- Step 6: manager-b cannot take over ConditionA without ForceOwnership ---
		By("manager-b cannot apply ConditionA without ForceOwnership — expects a 409 Conflict")
		err := k8sClient.Status().Apply(ctx,
			ssaConditionCfg(hvName, conditionA, metav1.ConditionTrue),
			k8sclient.FieldOwner(managerB),
			// intentionally no ForceOwnership
		)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsConflict(err)).To(BeTrue(),
			"expected a 409 Conflict when taking over another manager's condition without ForceOwnership, got: %v", err)
	})
})
