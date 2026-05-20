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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	v1ac "k8s.io/client-go/applyconfigurations/meta/v1"
)

// IsNodeConditionPresentAndEqual returns true when conditionType is present and equal to status.
func IsNodeConditionPresentAndEqual(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType, status corev1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}

// FindNodeStatusCondition returns the condition of the given type if it exists.
// Note: Returns a pointer into the original slice element, not a copy.
func FindNodeStatusCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// returns a OwnerReference for the given object and groupversionkind info as returned by apiutil.GVKForObject
func OwnerReference(obj metav1.Object, gvk *schema.GroupVersionKind) *v1ac.OwnerReferenceApplyConfiguration {
	return v1ac.OwnerReference().
		WithAPIVersion(gvk.Group + "/" + gvk.Version).
		WithKind(gvk.Kind).
		WithName(obj.GetName()).
		WithUID(obj.GetUID())
}

// returns if any ManagedField of the object has been modified by kubectl
func HasKubectlManagedFields(object *metav1.ObjectMeta) bool {
	for _, field := range object.GetManagedFields() {
		if strings.HasPrefix(field.Manager, "kubectl") {
			return true
		}
	}
	return false
}
