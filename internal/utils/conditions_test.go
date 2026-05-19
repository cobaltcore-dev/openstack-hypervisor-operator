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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

// ptr returns a pointer to v; used to construct ConditionApplyConfiguration literals.
func ptr[T any](v T) *T { return &v }

// --- ConditionsFromStatus ---

func TestConditionsFromStatus_Empty(t *testing.T) {
	result := utils.ConditionsFromStatus(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty slice, got %d elements", len(result))
	}

	result = utils.ConditionsFromStatus([]metav1.Condition{})
	if len(result) != 0 {
		t.Fatalf("expected empty slice, got %d elements", len(result))
	}
}

func TestConditionsFromStatus_ConvertsAllFields(t *testing.T) {
	ts := metav1.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	input := []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllGood",
			Message:            "everything is fine",
			LastTransitionTime: ts,
			ObservedGeneration: 7,
		},
	}

	result := utils.ConditionsFromStatus(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 element, got %d", len(result))
	}

	c := result[0]
	if c.Type == nil || *c.Type != "Ready" {
		t.Errorf("Type: got %v, want Ready", c.Type)
	}
	if c.Status == nil || *c.Status != metav1.ConditionTrue {
		t.Errorf("Status: got %v, want True", c.Status)
	}
	if c.Reason == nil || *c.Reason != "AllGood" {
		t.Errorf("Reason: got %v, want AllGood", c.Reason)
	}
	if c.Message == nil || *c.Message != "everything is fine" {
		t.Errorf("Message: got %v, want 'everything is fine'", c.Message)
	}
	if c.LastTransitionTime == nil || !c.LastTransitionTime.Equal(&ts) {
		t.Errorf("LastTransitionTime: got %v, want %v", c.LastTransitionTime, ts)
	}
}

func TestConditionsFromStatus_MultipleConditions(t *testing.T) {
	input := []metav1.Condition{
		{Type: "A", Status: metav1.ConditionTrue, Reason: "r", Message: "m", LastTransitionTime: metav1.Now()},
		{Type: "B", Status: metav1.ConditionFalse, Reason: "r2", Message: "m2", LastTransitionTime: metav1.Now()},
	}
	result := utils.ConditionsFromStatus(input)
	if len(result) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(result))
	}
	if result[0].Type == nil || *result[0].Type != "A" {
		t.Errorf("first condition type: got %v, want A", result[0].Type)
	}
	if result[1].Type == nil || *result[1].Type != "B" {
		t.Errorf("second condition type: got %v, want B", result[1].Type)
	}
}

// --- SetApplyConfigurationStatusCondition ---

func TestSetApplyConfigurationStatusCondition_NilSlicePointer(t *testing.T) {
	changed := utils.SetApplyConfigurationStatusCondition(nil,
		*k8sacmetav1.Condition().WithType("T").WithStatus(metav1.ConditionTrue).WithReason("R").WithMessage("M"))
	if changed {
		t.Error("expected false for nil conditions pointer")
	}
}

func TestSetApplyConfigurationStatusCondition_AppendNew(t *testing.T) {
	var conditions []k8sacmetav1.ConditionApplyConfiguration
	before := time.Now()

	changed := utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("AllGood").
			WithMessage("fine"))

	after := time.Now()

	if !changed {
		t.Error("expected changed=true for new condition")
	}
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}
	c := conditions[0]
	if c.Type == nil || *c.Type != "Ready" {
		t.Errorf("Type: got %v", c.Type)
	}
	if c.Status == nil || *c.Status != metav1.ConditionTrue {
		t.Errorf("Status: got %v", c.Status)
	}
	if c.LastTransitionTime == nil {
		t.Fatal("LastTransitionTime must be set for a new condition")
	}
	ltt := c.LastTransitionTime.Time
	if ltt.Before(before) || ltt.After(after) {
		t.Errorf("LastTransitionTime %v not in [%v, %v]", ltt, before, after)
	}
}

func TestSetApplyConfigurationStatusCondition_AppendNew_ExplicitLastTransitionTime(t *testing.T) {
	var conditions []k8sacmetav1.ConditionApplyConfiguration
	ts := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("R").
			WithMessage("M").
			WithLastTransitionTime(ts))

	if conditions[0].LastTransitionTime == nil || !conditions[0].LastTransitionTime.Equal(&ts) {
		t.Errorf("expected explicit LastTransitionTime %v, got %v", ts, conditions[0].LastTransitionTime)
	}
}

func TestSetApplyConfigurationStatusCondition_StatusUnchanged_PreservesLastTransitionTime(t *testing.T) {
	ts := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	conditions := []k8sacmetav1.ConditionApplyConfiguration{
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("OldReason").
			WithMessage("old message").
			WithLastTransitionTime(ts),
	}

	changed := utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("NewReason").
			WithMessage("new message"))

	if !changed {
		t.Error("expected changed=true because Reason/Message changed")
	}
	c := conditions[0]
	if c.Reason == nil || *c.Reason != "NewReason" {
		t.Errorf("Reason not updated: got %v", c.Reason)
	}
	if c.Message == nil || *c.Message != "new message" {
		t.Errorf("Message not updated: got %v", c.Message)
	}
	// LastTransitionTime must NOT have changed
	if c.LastTransitionTime == nil || !c.LastTransitionTime.Equal(&ts) {
		t.Errorf("LastTransitionTime must be preserved when status unchanged: got %v, want %v",
			c.LastTransitionTime, ts)
	}
}

func TestSetApplyConfigurationStatusCondition_StatusUnchanged_NoFieldChange_ReturnsFalse(t *testing.T) {
	ts := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	conditions := []k8sacmetav1.ConditionApplyConfiguration{
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("R").
			WithMessage("M").
			WithLastTransitionTime(ts),
	}

	changed := utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("R").
			WithMessage("M"))

	if changed {
		t.Error("expected changed=false when nothing changed")
	}
}

func TestSetApplyConfigurationStatusCondition_StatusChanged_UpdatesLastTransitionTime(t *testing.T) {
	old := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	conditions := []k8sacmetav1.ConditionApplyConfiguration{
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("R").
			WithMessage("M").
			WithLastTransitionTime(old),
	}

	before := time.Now()
	changed := utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionFalse).
			WithReason("Broken").
			WithMessage("it broke"))
	after := time.Now()

	if !changed {
		t.Error("expected changed=true for status change")
	}
	c := conditions[0]
	if c.Status == nil || *c.Status != metav1.ConditionFalse {
		t.Errorf("Status not updated: got %v", c.Status)
	}
	if c.LastTransitionTime == nil {
		t.Fatal("LastTransitionTime must be set after status change")
	}
	ltt := c.LastTransitionTime.Time
	if ltt.Before(before) || ltt.After(after) {
		t.Errorf("LastTransitionTime %v not in [%v, %v]", ltt, before, after)
	}
}

func TestSetApplyConfigurationStatusCondition_StatusChanged_ExplicitLastTransitionTime(t *testing.T) {
	old := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	explicit := metav1.NewTime(time.Date(2021, 6, 15, 0, 0, 0, 0, time.UTC))
	conditions := []k8sacmetav1.ConditionApplyConfiguration{
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionTrue).
			WithReason("R").
			WithMessage("M").
			WithLastTransitionTime(old),
	}

	utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionFalse).
			WithReason("R").
			WithMessage("M").
			WithLastTransitionTime(explicit))

	if conditions[0].LastTransitionTime == nil || !conditions[0].LastTransitionTime.Equal(&explicit) {
		t.Errorf("expected explicit LastTransitionTime %v, got %v", explicit, conditions[0].LastTransitionTime)
	}
}

func TestSetApplyConfigurationStatusCondition_OtherConditionsUntouched(t *testing.T) {
	ts := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	conditions := []k8sacmetav1.ConditionApplyConfiguration{
		*k8sacmetav1.Condition().WithType("Other").WithStatus(metav1.ConditionTrue).
			WithReason("R").WithMessage("M").WithLastTransitionTime(ts),
		*k8sacmetav1.Condition().WithType("Ready").WithStatus(metav1.ConditionTrue).
			WithReason("R").WithMessage("M").WithLastTransitionTime(ts),
	}

	utils.SetApplyConfigurationStatusCondition(&conditions,
		*k8sacmetav1.Condition().
			WithType("Ready").
			WithStatus(metav1.ConditionFalse).
			WithReason("Broken").
			WithMessage("broke"))

	// "Other" must be completely unchanged
	other := conditions[0]
	if other.Type == nil || *other.Type != "Other" {
		t.Errorf("Other condition type changed: %v", other.Type)
	}
	if other.Status == nil || *other.Status != metav1.ConditionTrue {
		t.Errorf("Other condition status changed: %v", other.Status)
	}
	if other.LastTransitionTime == nil || !other.LastTransitionTime.Equal(&ts) {
		t.Errorf("Other condition LastTransitionTime changed: %v", other.LastTransitionTime)
	}
}
