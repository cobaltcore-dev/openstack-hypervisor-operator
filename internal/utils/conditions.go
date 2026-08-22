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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
)

// ConditionFromStatus converts a single metav1.Condition into a
// *k8sacmetav1.ConditionApplyConfiguration, preserving all fields verbatim
// (including LastTransitionTime and ObservedGeneration).
//
// Controllers must seed their apply-config payload with the conditions they
// own before mutating them. With +listType=map / +listMapKey=type on the CRD,
// SSA merges conditions by the "type" key: conditions owned by other field
// managers are left untouched, and conditions previously written by this
// field manager that are absent from the payload are pruned. Therefore each
// controller should include all conditions it has previously set — and only
// those — in every Apply call.
func ConditionFromStatus(c metav1.Condition) *k8sacmetav1.ConditionApplyConfiguration {
	return &k8sacmetav1.ConditionApplyConfiguration{
		Type:               &c.Type,
		Status:             &c.Status,
		Reason:             &c.Reason,
		Message:            &c.Message,
		LastTransitionTime: &c.LastTransitionTime,
		ObservedGeneration: &c.ObservedGeneration,
	}
}

// SetApplyConfigurationStatusCondition sets the corresponding condition in
// conditions to newCondition and returns true if the conditions are changed by
// this call. It mirrors the behaviour of k8s.io/apimachinery/pkg/api/meta.SetStatusCondition:
//
//  1. If a condition of the specified type does not exist, newCondition is
//     appended. LastTransitionTime is set to now if not provided.
//  2. If a condition of the specified type already exists, all fields are
//     updated. LastTransitionTime is updated to now (or the provided value)
//     only when Status changes; otherwise the existing LastTransitionTime is
//     preserved.
func SetApplyConfigurationStatusCondition(conditions *[]k8sacmetav1.ConditionApplyConfiguration, newCondition k8sacmetav1.ConditionApplyConfiguration) (changed bool) {
	if conditions == nil {
		return false
	}
	if newCondition.Type == nil || *newCondition.Type == "" {
		return false
	}

	// Find existing entry by type
	var existing *k8sacmetav1.ConditionApplyConfiguration
	for i := range *conditions {
		if (*conditions)[i].Type != nil && *(*conditions)[i].Type == *newCondition.Type {
			existing = &(*conditions)[i]
			break
		}
	}

	if existing == nil {
		// New condition: set LastTransitionTime if not provided
		if newCondition.LastTransitionTime == nil {
			now := metav1.Now()
			newCondition.LastTransitionTime = &now
		}
		*conditions = append(*conditions, newCondition)
		return true
	}

	// Existing condition: update fields, handle LastTransitionTime
	statusChanged := (existing.Status == nil) != (newCondition.Status == nil) ||
		(existing.Status != nil && *existing.Status != *newCondition.Status)

	if statusChanged {
		existing.Status = newCondition.Status
		if newCondition.LastTransitionTime != nil {
			existing.LastTransitionTime = newCondition.LastTransitionTime
		} else {
			now := metav1.Now()
			existing.LastTransitionTime = &now
		}
		changed = true
	}
	// When status is unchanged, LastTransitionTime is intentionally preserved.

	if strChanged(existing.Reason, newCondition.Reason) {
		existing.Reason = newCondition.Reason
		changed = true
	}
	if strChanged(existing.Message, newCondition.Message) {
		existing.Message = newCondition.Message
		changed = true
	}
	if int64Changed(existing.ObservedGeneration, newCondition.ObservedGeneration) {
		existing.ObservedGeneration = newCondition.ObservedGeneration
		changed = true
	}

	return changed
}

// strChanged reports whether the pointer value of b differs from a.
func strChanged(a, b *string) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

// int64Changed reports whether the pointer value of b differs from a.
func int64Changed(a, b *int64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}
