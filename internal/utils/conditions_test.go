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

package utils_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

var _ = Describe("ConditionFromStatus", func() {
	It("preserves all fields verbatim, including ObservedGeneration", func() {
		ts := metav1.Now()
		c := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllGood",
			Message:            "everything is fine",
			LastTransitionTime: ts,
			ObservedGeneration: 42,
		}
		got := utils.ConditionFromStatus(c)
		Expect(got.Type).NotTo(BeNil())
		Expect(*got.Type).To(Equal("Ready"))
		Expect(*got.Status).To(Equal(metav1.ConditionTrue))
		Expect(*got.Reason).To(Equal("AllGood"))
		Expect(*got.Message).To(Equal("everything is fine"))
		Expect(*got.LastTransitionTime).To(Equal(ts))
		Expect(got.ObservedGeneration).NotTo(BeNil(), "ObservedGeneration must be copied")
		Expect(*got.ObservedGeneration).To(Equal(int64(42)))
	})
})

var _ = Describe("SetApplyConfigurationStatusCondition", func() {
	makeCondition := func(condType string, status metav1.ConditionStatus, reason, message string) k8sacmetav1.ConditionApplyConfiguration {
		return *k8sacmetav1.Condition().
			WithType(condType).
			WithStatus(status).
			WithReason(reason).
			WithMessage(message)
	}

	It("appends a new condition and sets LastTransitionTime", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		changed := utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "AllGood", "ok"))
		Expect(changed).To(BeTrue())
		Expect(conditions).To(HaveLen(1))
		Expect(*conditions[0].Type).To(Equal("Ready"))
		Expect(conditions[0].LastTransitionTime).NotTo(BeNil(), "LastTransitionTime must be set on append")
	})

	It("returns false and does not modify for nil conditions slice", func() {
		changed := utils.SetApplyConfigurationStatusCondition(nil, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))
		Expect(changed).To(BeFalse())
	})

	It("returns false for a condition with nil Type", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		blank := k8sacmetav1.ConditionApplyConfiguration{}
		changed := utils.SetApplyConfigurationStatusCondition(&conditions, blank)
		Expect(changed).To(BeFalse())
		Expect(conditions).To(BeEmpty())
	})

	It("updates fields and marks changed when status changes", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionFalse, "NotReady", "broken"))

		changed := utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "AllGood", "fixed"))
		Expect(changed).To(BeTrue())
		Expect(*conditions[0].Status).To(Equal(metav1.ConditionTrue))
		Expect(*conditions[0].Reason).To(Equal("AllGood"))
		Expect(*conditions[0].Message).To(Equal("fixed"))
	})

	It("preserves LastTransitionTime when status is unchanged", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))
		original := *conditions[0].LastTransitionTime

		// Same status — only message changes.
		changed := utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "updated message"))
		Expect(changed).To(BeTrue(), "message change is still a change")
		Expect(*conditions[0].LastTransitionTime).To(Equal(original), "LastTransitionTime must not advance when status is unchanged")
	})

	It("returns false when nothing changes", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))

		changed := utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))
		Expect(changed).To(BeFalse())
	})

	It("updates ObservedGeneration independently of status", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))

		next := makeCondition("Ready", metav1.ConditionTrue, "R", "m")
		gen := int64(7)
		next.ObservedGeneration = &gen
		changed := utils.SetApplyConfigurationStatusCondition(&conditions, next)
		Expect(changed).To(BeTrue())
		Expect(conditions[0].ObservedGeneration).NotTo(BeNil())
		Expect(*conditions[0].ObservedGeneration).To(Equal(int64(7)))
	})

	It("manages multiple conditions independently", func() {
		conditions := []k8sacmetav1.ConditionApplyConfiguration{}
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionTrue, "R", "m"))
		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Onboarding", metav1.ConditionFalse, "Initial", "pending"))
		Expect(conditions).To(HaveLen(2))

		utils.SetApplyConfigurationStatusCondition(&conditions, makeCondition("Ready", metav1.ConditionFalse, "Degraded", "issue"))
		Expect(conditions).To(HaveLen(2))
		Expect(*conditions[0].Status).To(Equal(metav1.ConditionFalse))
		Expect(*conditions[1].Status).To(Equal(metav1.ConditionFalse))
	})
})
