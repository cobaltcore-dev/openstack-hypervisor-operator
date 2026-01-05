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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
// Important: Run "make" to regenerate code after modifying this file

// Hypervisor Condition Types
// type of condition in CamelCase or in foo.example.com/CamelCase.
const (
	// ConditionTypeOnboarding is the type of condition for onboarding status
	ConditionTypeOnboarding = "Onboarding"

	// ConditionTypeReady is the type of condition for ready status of a hypervisor
	ConditionTypeReady = "Ready"

	// ConditionTypeTerminating is the type of condition for terminating status of a hypervisor
	ConditionTypeTerminating = "Terminating"

	// ConditionTypeTainted is the type of condition for tainted status of a hypervisor
	ConditionTypeTainted = "Tainted"

	// ConditionTypeTraitsUpdated is the type of condition for traits updated status of a hypervisor
	ConditionTypeTraitsUpdated = "TraitsUpdated"

	// ConditionTypeAggregatesUpdated is the type of condition for aggregates updated status of a hypervisor
	ConditionTypeAggregatesUpdated = "AggregatesUpdated"
)

// Condition Reasons
// The value should be a CamelCase string.
const (
	// ConditionTypeReady reasons
	ConditionReasonReadyReady       = "Ready"
	ConditionReasonReadyMaintenance = "Maintenance"
	ConditionReasonReadyEvicted     = "Evicted"

	// ConditionTypeOnboarding reasons
	ConditionReasonInitial    = "Initial"
	ConditionReasonOnboarding = "Onboarding"
	ConditionReasonTesting    = "Testing"
	ConditionReasonAborted    = "Aborted"
)

// HypervisorSpec defines the desired state of Hypervisor
type HypervisorSpec struct {
	// +kubebuilder:validation:Optional
	// OperatingSystemVersion represents the desired operating system version.
	OperatingSystemVersion string `json:"version,omitempty"`

	// +kubebuilder:default:=false
	// Reboot request an reboot after successful installation of an upgrade.
	Reboot bool `json:"reboot"`

	// +kubebuilder:default:=true
	// EvacuateOnReboot request an evacuation of all instances before reboot.
	EvacuateOnReboot bool `json:"evacuateOnReboot"`

	// +kubebuilder:default:=true
	// LifecycleEnabled enables the lifecycle management of the hypervisor via hypervisor-operator.
	LifecycleEnabled bool `json:"lifecycleEnabled"`

	// +kubebuilder:default:=false
	// SkipTests skips the tests during the onboarding process.
	SkipTests bool `json:"skipTests"`

	// +kubebuilder:default:={}
	// CustomTraits are used to apply custom traits to the hypervisor.
	CustomTraits []string `json:"customTraits"`

	// +kubebuilder:default:={}
	// Aggregates are used to apply aggregates to the hypervisor.
	Aggregates []string `json:"aggregates"`

	// +kubebuilder:default:={}
	// AllowedProjects defines which openstack projects are allowed to schedule
	// instances on this hypervisor. The values of this list should be project
	// uuids. If left empty, all projects are allowed.
	AllowedProjects []string `json:"allowedProjects"`

	// +kubebuilder:default:=true
	// HighAvailability is used to enable the high availability handling of the hypervisor.
	HighAvailability bool `json:"highAvailability"`

	// +kubebuilder:default:=false
	// Require to issue a certificate from cert-manager for the hypervisor, to be used for
	// secure communication with the libvirt API.
	CreateCertManagerCertificate bool `json:"createCertManagerCertificate"`

	// +kubebuilder:default:=true
	// InstallCertificate is used to enable the installations of the certificates via kvm-node-agent.
	InstallCertificate bool `json:"installCertificate"`

	// +kubebuilder:optional
	// +kubebuilder:validation:Enum:="";manual;auto;ha;termination
	// Maintenance indicates whether the hypervisor is in maintenance mode.
	Maintenance string `json:"maintenance,omitempty"`
}

const (
	// HypervisorMaintenance "enum"
	MaintenanceUnset       = ""
	MaintenanceManual      = "manual"      // manual maintenance mode by external user
	MaintenanceAuto        = "auto"        // automatic maintenance mode
	MaintenanceHA          = "ha"          // high availability maintenance mode
	MaintenanceTermination = "termination" // internal use only, when node is terminating state
)

type Instance struct {
	// Represents the instance ID (uuidv4).
	ID string `json:"id"`

	// Represents the instance name.
	Name string `json:"name"`

	// Represents the instance state.
	Active bool `json:"active"`
}

type HyperVisorUpdateStatus struct {
	// +kubebuilder:default:=false
	// Represents a running Operating System update.
	InProgress bool `json:"inProgress"`

	// +kubebuilder:default:=unknown
	// Represents the Operating System installed update version.
	Installed string `json:"installed,omitempty"`

	// +kubebuilder:default:=3
	// Represents the number of retries.
	Retry int `json:"retry"`
}

type OperatingSystemStatus struct {
	// Represents the Operating System version.
	Version string `json:"version,omitempty"`

	// Identifying a specific variant or edition of the operating system
	VariantID string `json:"variantID,omitempty"`

	// PrettyVersion
	PrettyVersion string `json:"prettyVersion,omitempty"`

	// KernelName
	KernelName string `json:"kernelName,omitempty"`

	// KernelRelease
	KernelRelease string `json:"kernelRelease,omitempty"`

	// KernelVersion
	KernelVersion string `json:"kernelVersion,omitempty"`

	// HardwareVendor
	HardwareVendor string `json:"hardwareVendor,omitempty"`

	// HardwareModel
	HardwareModel string `json:"hardwareModel,omitempty"`

	// HardwareSerial
	HardwareSerial string `json:"hardwareSerial,omitempty"`

	// FirmwareVersion
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// FirmwareVendor
	FirmwareVendor string `json:"firmwareVendor,omitempty"`

	// FirmwareDate
	FirmwareDate metav1.Time `json:"firmwareDate,omitempty"`

	// Represents the Garden Linux build commit id
	GardenLinuxCommitID string `json:"gardenLinuxCommitID,omitempty"`

	// Represents the Garden Linux Feature Set
	GardenLinuxFeatures []string `json:"gardenLinuxFeatures,omitempty"`
}

// Capabilities of the hypervisor as reported by libvirt.
type Capabilities struct {
	// +kubebuilder:default:=unknown
	// The hosts CPU architecture (not the guests).
	HostCpuArch string `json:"cpuArch,omitempty"`
	// Total host memory available as a sum of memory over all numa cells.
	HostMemory resource.Quantity `json:"memory,omitempty"`
	// Total host cpus available as a sum of cpus over all numa cells.
	HostCpus resource.Quantity `json:"cpus,omitempty"`
}

// Domain capabilities of the hypervisor as reported by libvirt.
// These details are relevant to check if a VM can be scheduled on the hypervisor.
type DomainCapabilities struct {
	// The available domain cpu architecture.
	// +kubebuilder:default:=unknown
	Arch string `json:"arch,omitempty"`

	// The supported type of virtualization for domains, such as "ch".
	// +kubebuilder:default:=unknown
	HypervisorType string `json:"hypervisorType,omitempty"`

	// Supported devices for domains.
	//
	// The format of this list is the device type, and if specified, a specific
	// model. For example, the take the following xml domain device definition:
	//
	// <video supported='yes'>
	//   <enum name='modelType'>
	//     <value>nvidia</value>
	//   </enum>
	// </video>
	//
	// The corresponding entries in this list would be "video" and "video/nvidia".
	//
	// +kubebuilder:default:={}
	SupportedDevices []string `json:"supportedDevices,omitempty"`

	// Supported cpu modes for domains.
	//
	// The format of this list is cpu mode, and if specified, a specific
	// submode. For example, the take the following xml domain cpu definition:
	//
	// <mode name='host-passthrough' supported='yes'>
	//   <enum name='hostPassthroughMigratable'/>
	// </mode>
	//
	// The corresponding entries in this list would be "host-passthrough" and
	// "host-passthrough/migratable".
	//
	// +kubebuilder:default:={}
	SupportedCpuModes []string `json:"supportedCpuModes,omitempty"`

	// Supported features for domains, such as "sev" or "sgx".
	//
	// This is a flat list of supported features, meaning the following xml:
	//
	// <features>
	//   <sev supported='no'/>
	//   <sgx supported='no'/>
	// </features>
	//
	// Would correspond to the entries "sev" and "sgx" in this list.
	//
	// +kubebuilder:default:={}
	SupportedFeatures []string `json:"supportedFeatures,omitempty"`
}

// Cell represents a NUMA cell on the hypervisor.
type Cell struct {
	// Cell ID.
	CellID uint64 `json:"cellID"`

	// Auto-discovered resource allocation of all hosted VMs in this cell.
	// +kubebuilder:validation:Optional
	Allocation map[string]resource.Quantity `json:"allocation"`

	// Auto-discovered capacity of this cell.
	// +kubebuilder:validation:Optional
	Capacity map[string]resource.Quantity `json:"capacity"`
}

// HypervisorStatus defines the observed state of Hypervisor
type HypervisorStatus struct {
	// +kubebuilder:default:=unknown
	// Represents the LibVirt version.
	LibVirtVersion string `json:"libVirtVersion,omitempty"`

	// Represents the Operating System status.
	OperatingSystem OperatingSystemStatus `json:"operatingSystem,omitempty"`

	// Represents the Hypervisor update status.
	Update HyperVisorUpdateStatus `json:"updateStatus,omitempty"`

	// Represents the Hypervisor hosted Virtual Machines
	Instances []Instance `json:"instances,omitempty"`

	// Auto-discovered capabilities as reported by libvirt.
	// +kubebuilder:validation:Optional
	Capabilities Capabilities `json:"capabilities"`

	// Auto-discovered domain capabilities relevant to check if a VM
	// can be scheduled on the hypervisor.
	// +kubebuilder:validation:Optional
	DomainCapabilities DomainCapabilities `json:"domainCapabilities"`

	// Auto-discovered resource allocation of all hosted VMs.
	// +kubebuilder:validation:Optional
	Allocation map[string]resource.Quantity `json:"allocation"`

	// Auto-discovered capacity of the hypervisor.
	// +kubebuilder:validation:Optional
	Capacity map[string]resource.Quantity `json:"capacity"`

	// Auto-discovered cells on this hypervisor.
	// +kubebuilder:validation:Optional
	Cells []Cell `json:"cells,omitempty"`

	// +kubebuilder:default:=0
	// Represent the num of instances
	NumInstances int `json:"numInstances"`

	// HypervisorID is the unique identifier of the hypervisor in OpenStack.
	HypervisorID string `json:"hypervisorId,omitempty"`

	// ServiceID is the unique identifier of the compute service in OpenStack.
	ServiceID string `json:"serviceId,omitempty"`

	// Traits are the applied traits of the hypervisor.
	Traits []string `json:"traits,omitempty"`

	// Aggregates are the applied aggregates of the hypervisor.
	Aggregates []string `json:"aggregates,omitempty"`

	// InternalIP is the internal IP address of the hypervisor.
	InternalIP string `json:"internalIp,omitempty"`

	// Evicted indicates whether the hypervisor is evicted. (no instances left with active maintenance mode)
	Evicted bool `json:"evicted,omitempty"`

	// Represents the Hypervisor node conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	SpecHash string `json:"specHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hv
// +kubebuilder:printcolumn:JSONPath=.metadata.labels.topology\.kubernetes\.io/zone,name="Zone",type="string",priority=2
// +kubebuilder:printcolumn:JSONPath=.metadata.labels.kubernetes\.metal\.cloud\.sap/bb,name="Building Block",type="string",priority=2
// +kubebuilder:printcolumn:JSONPath=.metadata.labels.worker\.garden\.sapcloud\.io/group,name="Group",type="string",priority=2
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Ready\")].status",name="Ready",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Ready\")].reason",name="State",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Tainted\")].message",name="Taint",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.lifecycleEnabled",name="Lifecycle",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".spec.highAvailability",name="High Availability",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".spec.skipTests",name="Skip Tests",type="boolean"
// +kubebuilder:printcolumn:JSONPath=".status.operatingSystem.prettyVersion",name="Version",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.internalIp",name="IP",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.numInstances",name="Instances",type="integer"
// +kubebuilder:printcolumn:JSONPath=".status.operatingSystem.hardwareModel",name="Hardware",type="string",priority=2
// +kubebuilder:printcolumn:JSONPath=".status.operatingSystem.kernelRelease",name="Kernel",type="string",priority=2
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Onboarding\")].reason",name="Onboarding",type="string",priority=3
// +kubebuilder:printcolumn:JSONPath=".status.serviceId",name="Service ID",type="string",priority=3
// +kubebuilder:printcolumn:JSONPath=".status.hypervisorId",name="Hypervisor ID",type="string",priority=3
// +kubebuilder:printcolumn:JSONPath=".metadata.creationTimestamp",name="Age",type="date"

// Hypervisor is the Schema for the hypervisors API
type Hypervisor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorSpec   `json:"spec,omitempty"`
	Status HypervisorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorList contains a list of Hypervisor
type HypervisorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Hypervisor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Hypervisor{}, &HypervisorList{})
}
