/*
SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, LibVirtVersion 2.0 (the "License");
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

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
// Important: Run "make" to regenerate code after modifying this file

// HypervisorConfigOverrideSpec overrides the desired state of Hypervisor
type HypervisorConfigOverrideSpec struct {
	// Reason describes the reason for the override
	Reason   string               `json:"reason"`
	Override HypervisorConfigSpec `json:"override"`
}

// HypervisorConfigOverrideStatus defines the observed state of HypervisorConfigOverride
type HypervisorConfigOverrideStatus struct {
	// Represents the Hypervisor node conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	SpecHash string `json:"specHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hvco
// +kubebuilder:printcolumn:JSONPath=".spec.reason",name="Reason",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.override.lifecycleEnabled",name="Lifecycle",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".spec.override.highAvailability",name="High Availability",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".spec.override.skipTests",name="Skip Tests",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".metadata.creationTimestamp",name="Age",type="date"

// HypervisorConfigOverride is the Schema for overriding the config of one hypervisor crd API
type HypervisorConfigOverride struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorConfigOverrideSpec   `json:"spec,omitempty"`
	Status HypervisorConfigOverrideStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorConfigOverrideList contains a list of HypervisorConfigOverride
type HypervisorConfigOverrideList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorConfigOverride `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorConfigOverride{}, &HypervisorConfigOverrideList{})
}
