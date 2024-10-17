/*
Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EvictionSpec defines the desired state of Eviction
type EvictionSpec struct {
	// Name of hypervisor to evict
	Hypervisor string `json:"hypervisor"`
}

// EvictionStatus defines the observed state of Eviction
type EvictionStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Succeeded
	// +kubebuilder:default=Pending
	EvictionState string `json:"evictionState"`

	// Conditions is an array of current conditions
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=".spec.hypervisor",name="Hypervisor",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.evictionState",name="State",type="string"

// Eviction is the Schema for the evictions API
type Eviction struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EvictionSpec   `json:"spec,omitempty"`
	Status EvictionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EvictionList contains a list of Eviction
type EvictionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Eviction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Eviction{}, &EvictionList{})
}
