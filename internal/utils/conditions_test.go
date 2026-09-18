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

package utils

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
)

func TestUtils(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Utils Suite")
}

var _ = Describe("ConditionFromStatus", func() {
	It("copies all fields verbatim", func() {
		ts := metav1.NewTime(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		gen := int64(42)
		c := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllGood",
			Message:            "everything is fine",
			LastTransitionTime: ts,
			ObservedGeneration: gen,
		}
		got := ConditionFromStatus(c)
		Expect(*got.Type).To(Equal("Ready"))
		Expect(*got.Status).To(Equal(metav1.ConditionTrue))
		Expect(*got.Reason).To(Equal("AllGood"))
		Expect(*got.Message).To(Equal("everything is fine"))
		Expect(*got.LastTransitionTime).To(Equal(ts))
		Expect(*got.ObservedGeneration).To(Equal(gen))
	})

	It("includes ObservedGeneration even when zero", func() {
		c := metav1.Condition{Type: "T", Status: metav1.ConditionFalse, Reason: "R"}
		got := ConditionFromStatus(c)
		Expect(got.ObservedGeneration).NotTo(BeNil())
		Expect(*got.ObservedGeneration).To(Equal(int64(0)))
	})
})

var _ = Describe("SetApplyConfigurationStatusCondition", func() {
	ptr := func(s string) *string { return &s }
	condStatus := func(s metav1.ConditionStatus) *metav1.ConditionStatus { return &s }

	Describe("nil / empty-type guards", func() {
		It("does not panic on nil slice pointer", func() {
			SetApplyConfigurationStatusCondition(nil,
				*k8sacmetav1.Condition().WithType("T").WithStatus(metav1.ConditionTrue).WithReason("R"))
		})

		It("does not modify the slice when Type is nil", func() {
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				k8sacmetav1.ConditionApplyConfiguration{})
			Expect(conditions).To(BeEmpty())
		})

		It("does not modify the slice when Type is empty string", func() {
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().WithType(""))
			Expect(conditions).To(BeEmpty())
		})
	})

	Describe("appending a new condition", func() {
		It("appends when the type is not yet present", func() {
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionTrue).
					WithReason("OK"))
			Expect(conditions).To(HaveLen(1))
			Expect(*conditions[0].Type).To(Equal("Ready"))
		})

		It("sets LastTransitionTime to now when not provided", func() {
			before := time.Now()
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("T").
					WithStatus(metav1.ConditionTrue).
					WithReason("R"))
			Expect(conditions[0].LastTransitionTime).NotTo(BeNil())
			Expect(conditions[0].LastTransitionTime.Time).To(BeTemporally(">=", before))
		})

		It("sets LastTransitionTime to now when a zero time is provided", func() {
			before := time.Now()
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("T").
					WithStatus(metav1.ConditionTrue).
					WithLastTransitionTime(metav1.Time{}))
			Expect(conditions[0].LastTransitionTime).NotTo(BeNil())
			Expect(conditions[0].LastTransitionTime.Time).To(BeTemporally(">=", before))
		})

		It("preserves a caller-supplied LastTransitionTime", func() {
			ts := metav1.NewTime(time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC))
			conditions := []k8sacmetav1.ConditionApplyConfiguration{}
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("T").
					WithStatus(metav1.ConditionTrue).
					WithReason("R").
					WithLastTransitionTime(ts))
			Expect(*conditions[0].LastTransitionTime).To(Equal(ts))
		})
	})

	Describe("updating an existing condition", func() {
		var (
			oldTS      metav1.Time
			conditions []k8sacmetav1.ConditionApplyConfiguration
		)

		BeforeEach(func() {
			oldTS = metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
			conditions = []k8sacmetav1.ConditionApplyConfiguration{
				{
					Type:               ptr("Ready"),
					Status:             condStatus(metav1.ConditionFalse),
					Reason:             ptr("NotReady"),
					Message:            ptr("waiting"),
					LastTransitionTime: &oldTS,
				},
			}
		})

		It("updates Status and refreshes LastTransitionTime when Status changes", func() {
			before := time.Now()
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionTrue).
					WithReason("OK").
					WithMessage("done"))
			Expect(*conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(conditions[0].LastTransitionTime.Time).To(BeTemporally(">=", before))
		})

		It("preserves LastTransitionTime when Status is unchanged", func() {
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("StillNotReady").
					WithMessage("still waiting"))
			Expect(*conditions[0].LastTransitionTime).To(Equal(oldTS))
		})

		It("preserves the condition when nothing changed", func() {
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("NotReady").
					WithMessage("waiting"))
			Expect(*conditions[0].LastTransitionTime).To(Equal(oldTS))
		})

		It("updates Reason independently of Status", func() {
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("DifferentReason").
					WithMessage("waiting"))
			Expect(*conditions[0].Reason).To(Equal("DifferentReason"))
			Expect(*conditions[0].LastTransitionTime).To(Equal(oldTS))
		})

		It("uses a caller-supplied LastTransitionTime when Status changes", func() {
			newTS := metav1.NewTime(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionTrue).
					WithReason("OK").
					WithLastTransitionTime(newTS))
			Expect(*conditions[0].LastTransitionTime).To(Equal(newTS))
		})

		It("sets LastTransitionTime to now when a zero time is provided on Status change", func() {
			before := time.Now()
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionTrue).
					WithLastTransitionTime(metav1.Time{}))
			Expect(conditions[0].LastTransitionTime.Time).To(BeTemporally(">=", before))
		})

		It("does not affect other conditions in the slice", func() {
			otherTS := metav1.NewTime(time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC))
			conditions = append(conditions, k8sacmetav1.ConditionApplyConfiguration{
				Type:               ptr("Other"),
				Status:             condStatus(metav1.ConditionTrue),
				LastTransitionTime: &otherTS,
			})
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionTrue).
					WithReason("OK"))
			Expect(*conditions[1].LastTransitionTime).To(Equal(otherTS))
		})

		It("updates ObservedGeneration", func() {
			gen := int64(5)
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("NotReady").
					WithMessage("waiting").
					WithObservedGeneration(gen))
			Expect(*conditions[0].ObservedGeneration).To(Equal(gen))
			Expect(*conditions[0].LastTransitionTime).To(Equal(oldTS))
		})

		It("updates an existing nil Status", func() {
			conditions[0].Status = nil
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("NotReady").
					WithMessage("waiting"))
			Expect(*conditions[0].Status).To(Equal(metav1.ConditionFalse))
		})

		It("preserves LastTransitionTime when both Status values are nil", func() {
			conditions[0].Status = nil
			SetApplyConfigurationStatusCondition(&conditions,
				k8sacmetav1.ConditionApplyConfiguration{
					Type:    ptr("Ready"),
					Reason:  ptr("NotReady"),
					Message: ptr("waiting"),
					// Status intentionally nil
				})
			Expect(*conditions[0].LastTransitionTime).To(Equal(oldTS))
		})

		It("updates an existing nil Reason", func() {
			conditions[0].Reason = nil
			SetApplyConfigurationStatusCondition(&conditions,
				*k8sacmetav1.Condition().
					WithType("Ready").
					WithStatus(metav1.ConditionFalse).
					WithReason("NotReady").
					WithMessage("waiting"))
			Expect(*conditions[0].Reason).To(Equal("NotReady"))
		})

		It("clears an existing Message when the new Message is nil", func() {
			SetApplyConfigurationStatusCondition(&conditions,
				k8sacmetav1.ConditionApplyConfiguration{
					Type:   ptr("Ready"),
					Status: condStatus(metav1.ConditionFalse),
					Reason: ptr("NotReady"),
					// Message intentionally nil
				})
			Expect(conditions[0].Message).To(BeNil())
		})
	})
})
