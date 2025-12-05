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
type HypervisorConfigSetSpec struct {
	Selector *metav1.LabelSelector       `json:"selector"`
	Template HypervisorConfigSetTemplate `json:"template"`
}

// HypervisorConfigSpec defines (in parts) the desired state of Hypervisor
type HypervisorConfigSpec struct {
	// +kubebuilder:validation:Optional
	// OperatingSystemVersion represents the desired operating system version.
	OperatingSystemVersion *string `json:"version,omitempty"`

	// +kubebuilder:validation:Optional
	// Reboot request an reboot after successful installation of an upgrade.
	Reboot *bool `json:"reboot,omitempty"`

	// +kubebuilder:validation:Optional
	// EvacuateOnReboot request an evacuation of all instances before reboot.
	EvacuateOnReboot *bool `json:"evacuateOnReboot,omitempty"`

	// +kubebuilder:optional
	// LifecycleEnabled enables the lifecycle management of the hypervisor via hypervisor-operator.
	LifecycleEnabled *bool `json:"lifecycleEnabled,omitempty"`

	// +kubebuilder:validation:Optional
	// SkipTests skips the tests during the onboarding process.
	SkipTests *bool `json:"skipTests,omitempty"`

	// +kubebuilder:optional
	// CustomTraits are used to apply custom traits to the hypervisor.
	CustomTraits *[]string `json:"customTraits,omitempty"`

	// +kubebuilder:optional
	// Aggregates are used to apply aggregates to the hypervisor.
	Aggregates *[]string `json:"aggregates,omitempty"`

	// +kubebuilder:optional
	// HighAvailability is used to enable the high availability handling of the hypervisor.
	HighAvailability *bool `json:"highAvailability,omitempty"`

	// +kubebuilder:optional
	// Require to issue a certificate from cert-manager for the hypervisor, to be used for
	// secure communication with the libvirt API.
	CreateCertManagerCertificate *bool `json:"createCertManagerCertificate,omitempty"`

	// +kubebuilder:optional
	// InstallCertificate is used to enable the installations of the certificates via kvm-node-agent.
	InstallCertificate *bool `json:"installCertificate,omitempty"`

	// +kubebuilder:optional
	// +kubebuilder:validation:Enum:="";manual;auto;ha
	// Maintenance indicates whether the hypervisor is in maintenance mode.
	Maintenance *string `json:"maintenance,omitempty"`
}

// A template for the hypervisor defining the values for a whole set
type HypervisorConfigSetTemplate struct {
	Spec HypervisorConfigSpec `json:"spec"`
}

// HypervisorConfigSetStatus defines the observed state of HypervisorConfigSet
type HypervisorConfigSetStatus struct {
	// Represents the Hypervisor node conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	SpecHash string `json:"specHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hvcs
// +kubebuilder:printcolumn:JSONPath=".metadata.creationTimestamp",name="Age",type="date"

// HypervisorConfigSet is the Schema for a set of hypervisor configs
type HypervisorConfigSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorConfigSetSpec   `json:"spec,omitempty"`
	Status HypervisorConfigSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// HypervisorConfigSetList contains a list of HypervisorConfigSet
type HypervisorConfigSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorConfigSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorConfigSet{}, &HypervisorConfigSetList{})
}
