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

package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceName is the name identifying a hypervisor resource.
// Note: this type is similar to the type defined in the kubernetes core api,
// but may be extended to support additional resource types in the future.
// See: https://github.com/kubernetes/api/blob/7e7aaba/core/v1/types.go#L6954-L6970
type ResourceName string

// Resource names must be not more than 63 characters, consisting of upper- or
// lower-case alphanumeric characters, with the -, _, and . characters allowed
// anywhere, except the first or last character. The default convention,
// matching that for annotations, is to use lower-case names, with dashes,
// rather than camel case, separating compound words. Fully-qualified resource
// typenames are constructed from a DNS-style subdomain, followed by a slash `/`
// and a name.
const (
	// CPU, in cores. Note that currently, it is not supported to provide
	// fractional cpu resources, such as 500m for 0.5 cpu.
	ResourceCPU ResourceName = "cpu"
	// Memory, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceMemory ResourceName = "memory"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
// Important: Run "make" to regenerate code after modifying this file

// Hypervisor Condition Types
// type of condition in CamelCase or in foo.example.com/CamelCase.
const (
	// ConditionTypeOnboarding is the type of condition for onboarding status
	ConditionTypeOnboarding = "Onboarding"

	// ConditionTypeOffboarded is the type of condition for the completed offboarding
	ConditionTypeOffboarded = "Offboarded"

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
	ConditionReasonReadyEvicting    = "Evicting"

	// ConditionTypeOnboarding reasons
	ConditionReasonInitial    = "Initial"
	ConditionReasonOnboarding = "Onboarding"
	ConditionReasonTesting    = "Testing"
	ConditionReasonHandover   = "Handover" // Indicates that the onboarding is almost completed, save for other controllers doing their part
	ConditionReasonAborted    = "Aborted"

	// ConditionTypeAggregatesUpdated reasons
	// Note: ConditionReasonSucceeded and ConditionReasonFailed are shared with eviction_types.go
	ConditionReasonTestAggregates     = "TestAggregates"
	ConditionReasonTerminating        = "Terminating"
	ConditionReasonEvictionInProgress = "EvictionInProgress"
	ConditionReasonWaitingForTraits   = "WaitingForTraits"

	// ConditionTypeHaEnabled reasons
	ConditionReasonHaEvicted    = "Evicted"    // HA disabled due to eviction
	ConditionReasonHaOnboarding = "Onboarding" // HA disabled during onboarding
)

// HypervisorSpec defines the desired state of Hypervisor
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.maintenance) || oldSelf.maintenance != 'termination' || self.maintenance == 'ha' || self == oldSelf",message="spec is immutable when maintenance is 'termination'; can only change maintenance to 'ha'"
// +kubebuilder:validation:XValidation:rule="!has(self.maintenance) || self.maintenance != 'manual' || (has(self.maintenanceReason) && self.maintenanceReason.size() > 0)",message="maintenanceReason must be non-empty when maintenance is 'manual'"
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

	// Groups defines typed group memberships for this hypervisor.
	//
	// Both traits and aggregates are forms of grouping: traits group
	// hypervisors by capability, aggregates group them by administrative
	// assignment. Each entry follows the field-presence union pattern
	// (as used by PodSpec.volumes in core Kubernetes): exactly one
	// type-specific sub-field must be populated per entry.
	//
	// The Cortex Placement shim and scheduler read group memberships
	// directly from this field.
	//
	// Note: uniqueness of trait names and aggregate UUIDs is not enforced
	// via CEL because the required O(n^2) comparison exceeds the
	// Kubernetes CEL cost budget. Enforce uniqueness in the consuming
	// controller or via a validating webhook if needed.
	//
	// +kubebuilder:validation:Optional
	Groups []Group `json:"groups,omitempty"`

	// Bookings records all resource claims on this hypervisor as seen
	// by the Placement API. Each entry is either a consumer (instance
	// allocation) or a reservation (capacity held back from scheduling).
	//
	// The Cortex Placement shim writes bookings via the Kubernetes API;
	// they are not auto-discovered. Compare with status.allocation which
	// reflects actual libvirt-reported usage.
	//
	// +kubebuilder:validation:Optional
	Bookings []Booking `json:"bookings,omitempty"`

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

	// +kubebuilder:optional
	// MaintenanceReason provides the reason for manual maintenance mode.
	MaintenanceReason string `json:"maintenanceReason,omitempty"`

	// Overcommit specifies the desired overcommit ratio by resource type.
	//
	// If no overcommit is specified for a resource type, the default overcommit
	// ratio of 1.0 should be applied, i.e. the effective capacity is the same
	// as the actual capacity.
	//
	// If the overcommit ratio results in a fractional effective capacity,
	// the effective capacity is expected to be rounded down. This allows
	// gradually adjusting the hypervisor capacity.
	//
	// +kubebuilder:validation:Optional
	//
	// It is validated that all overcommit ratios are greater than or equal to
	// 1.0, if specified. For this we don't need extra validating webhooks.
	// See: https://kubernetes.io/blog/2022/09/23/crd-validation-rules-beta/#crd-transition-rules
	// +kubebuilder:validation:XValidation:rule="self.all(k, self[k] >= 1.0)",message="overcommit ratios must be >= 1.0"
	Overcommit map[ResourceName]float64 `json:"overcommit,omitempty"`
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

// Aggregate represents an OpenStack aggregate with its name and UUID.
type Aggregate struct {
	// Name is the name of the aggregate.
	Name string `json:"name"`

	// UUID is the unique identifier of the aggregate.
	UUID string `json:"uuid"`

	// Metadata is the metadata of the aggregate as key-value pairs.
	// +kubebuilder:validation:Optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// TraitGroup represents a capability trait, such as an OpenStack
// Placement trait (e.g. HW_CPU_X86_AVX2, COMPUTE_STATUS_DISABLED).
type TraitGroup struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AggregateGroup represents an administrative grouping, such as an
// OpenStack host aggregate.
type AggregateGroup struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	UUID string `json:"uuid"`

	// +kubebuilder:validation:Optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Group is a typed group membership entry for a hypervisor.
//
// This follows the field-presence union pattern (as used by
// PodSpec.volumes in core Kubernetes): each entry populates exactly
// one type-specific sub-field, and the populated field identifies
// the group type.
//
// +kubebuilder:validation:XValidation:rule="(has(self.trait) ? 1 : 0) + (has(self.aggregate) ? 1 : 0) == 1",message="exactly one group type must be set"
type Group struct {
	// +kubebuilder:validation:Optional
	Trait *TraitGroup `json:"trait,omitempty"`

	// +kubebuilder:validation:Optional
	Aggregate *AggregateGroup `json:"aggregate,omitempty"`
}

// HasTrait reports whether groups contains a trait entry with the given name.
func HasTrait(groups []Group, name string) bool {
	for _, g := range groups {
		if g.Trait != nil && g.Trait.Name == name {
			return true
		}
	}
	return false
}

// GetTraits returns all TraitGroup entries from groups.
func GetTraits(groups []Group) []TraitGroup {
	var out []TraitGroup
	for _, g := range groups {
		if g.Trait != nil {
			out = append(out, *g.Trait)
		}
	}
	return out
}

// HasAggregate reports whether groups contains an aggregate entry with the given UUID.
func HasAggregate(groups []Group, uuid string) bool {
	for _, g := range groups {
		if g.Aggregate != nil && g.Aggregate.UUID == uuid {
			return true
		}
	}
	return false
}

// GetAggregates returns all AggregateGroup entries from groups.
func GetAggregates(groups []Group) []AggregateGroup {
	var out []AggregateGroup
	for _, g := range groups {
		if g.Aggregate != nil {
			out = append(out, *g.Aggregate)
		}
	}
	return out
}

// ConsumerBooking represents an instance allocation — a consumer that
// holds resources on this hypervisor as recorded by the Placement API.
//
// Nova creates consumers via PUT /allocations/{consumer_uuid}. Each
// consumer corresponds to an instance (or migration UUID) that holds
// resources on this provider. A consumer can appear on multiple
// Hypervisor CRs simultaneously during live migration.
type ConsumerBooking struct {
	// UUID is the Placement consumer UUID, typically the Nova instance
	// UUID or a migration UUID.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	UUID string `json:"uuid"`

	// Resources maps resource names to the quantity claimed by this
	// consumer on this hypervisor.
	// +kubebuilder:validation:MinProperties=1
	Resources map[ResourceName]resource.Quantity `json:"resources"`

	// ConsumerGeneration is the Placement consumer generation counter
	// used for optimistic concurrency control. Nil means the consumer
	// was just created (first allocation).
	// +kubebuilder:validation:Optional
	ConsumerGeneration *int64 `json:"consumerGeneration,omitempty"`

	// ConsumerType identifies the kind of consumer.
	// See: https://docs.openstack.org/api-ref/placement/#update-allocations
	// +kubebuilder:validation:Optional
	ConsumerType string `json:"consumerType,omitempty"`

	// ProjectID is the OpenStack project that owns this consumer.
	// +kubebuilder:validation:Optional
	ProjectID string `json:"projectID,omitempty"`

	// UserID is the OpenStack user that owns this consumer.
	// +kubebuilder:validation:Optional
	UserID string `json:"userID,omitempty"`
}

// ReservationBooking represents capacity held back from scheduling,
// corresponding to Nova's reserved_host_* configuration
// (reserved_host_memory_mb, reserved_host_cpus, reserved_host_disk_mb).
//
// The Cortex Placement shim reads reservation bookings and serves them
// as the "reserved" field in GET /resource_providers/{uuid}/inventories
// responses. Reserved capacity is subtracted from available inventory
// before scheduling decisions are made.
type ReservationBooking struct {
	// Name identifies this reservation (e.g. "nova-reserved").
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Resources maps resource names to the quantity reserved
	// (held back from scheduling) on this hypervisor.
	// +kubebuilder:validation:MinProperties=1
	Resources map[ResourceName]resource.Quantity `json:"resources"`
}

// Booking is a typed resource claim entry on this hypervisor.
//
// This follows the field-presence union pattern (as used by
// spec.groups and PodSpec.volumes in core Kubernetes): each entry
// populates exactly one type-specific sub-field, and the populated
// field identifies the booking type.
//
// +kubebuilder:validation:XValidation:rule="(has(self.consumer) ? 1 : 0) + (has(self.reservation) ? 1 : 0) == 1",message="exactly one booking type must be set"
type Booking struct {
	// +kubebuilder:validation:Optional
	Consumer *ConsumerBooking `json:"consumer,omitempty"`

	// +kubebuilder:validation:Optional
	Reservation *ReservationBooking `json:"reservation,omitempty"`
}

// GetConsumers returns all ConsumerBooking entries from bookings.
func GetConsumers(bookings []Booking) []ConsumerBooking {
	var out []ConsumerBooking
	for _, b := range bookings {
		if b.Consumer != nil {
			out = append(out, *b.Consumer)
		}
	}
	return out
}

// GetConsumer returns the ConsumerBooking with the given UUID, or nil.
func GetConsumer(bookings []Booking, uuid string) *ConsumerBooking {
	for _, b := range bookings {
		if b.Consumer != nil && b.Consumer.UUID == uuid {
			return b.Consumer
		}
	}
	return nil
}

// HasConsumer reports whether bookings contains a consumer with the given UUID.
func HasConsumer(bookings []Booking, uuid string) bool {
	for _, b := range bookings {
		if b.Consumer != nil && b.Consumer.UUID == uuid {
			return true
		}
	}
	return false
}

// GetReservations returns all ReservationBooking entries from bookings.
func GetReservations(bookings []Booking) []ReservationBooking {
	var out []ReservationBooking
	for _, b := range bookings {
		if b.Reservation != nil {
			out = append(out, *b.Reservation)
		}
	}
	return out
}

// GetReservation returns the ReservationBooking with the given name, or nil.
func GetReservation(bookings []Booking, name string) *ReservationBooking {
	for _, b := range bookings {
		if b.Reservation != nil && b.Reservation.Name == name {
			return b.Reservation
		}
	}
	return nil
}

// SumResources sums resources across all booking entries (consumers and reservations).
func SumResources(bookings []Booking) map[ResourceName]resource.Quantity {
	out := make(map[ResourceName]resource.Quantity)
	for _, b := range bookings {
		var res map[ResourceName]resource.Quantity
		if b.Consumer != nil {
			res = b.Consumer.Resources
		} else if b.Reservation != nil {
			res = b.Reservation.Resources
		}
		for name, qty := range res {
			existing := out[name]
			existing.Add(qty)
			out[name] = existing
		}
	}
	return out
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

	// KernelCommandLine contains the raw kernel boot parameters from /proc/cmdline.
	KernelCommandLine string `json:"kernelCommandLine,omitempty"`

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
	Allocation map[ResourceName]resource.Quantity `json:"allocation"`

	// Auto-discovered capacity of this cell.
	//
	// Note that this capacity does not include the applied overcommit ratios,
	// and represents the actual capacity of the cell. Use the effective capacity
	// field to get the capacity considering the applied overcommit ratios.
	//
	// +kubebuilder:validation:Optional
	Capacity map[ResourceName]resource.Quantity `json:"capacity"`

	// Auto-discovered capacity of this cell, considering the
	// applied overcommit ratios.
	//
	// In case no overcommit ratio is specified for a resource type, the default
	// overcommit ratio of 1 should be applied, meaning the effective capacity
	// is the same as the actual capacity.
	//
	// If the overcommit ratio results in a fractional effective capacity, the
	// effective capacity is expected to be rounded down.
	//
	// +kubebuilder:validation:Optional
	EffectiveCapacity map[ResourceName]resource.Quantity `json:"effectiveCapacity,omitempty"`
}

// HypervisorStatus defines the observed state of Hypervisor
type HypervisorStatus struct {
	// +kubebuilder:default:=unknown
	// Represents the LibVirt version.
	LibVirtVersion string `json:"libVirtVersion,omitempty"`

	// +kubebuilder:default:=unknown
	// Represents the Hypervisor version
	HypervisorVersion string `json:"hypervisorVersion,omitempty"`

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
	Allocation map[ResourceName]resource.Quantity `json:"allocation"`

	// Auto-discovered capacity of the hypervisor.
	//
	// Note that this capacity does not include the applied overcommit ratios,
	// and represents the actual capacity of the hypervisor. Use the
	// effective capacity field to get the capacity considering the applied
	// overcommit ratios.
	//
	// +kubebuilder:validation:Optional
	Capacity map[ResourceName]resource.Quantity `json:"capacity"`

	// Auto-discovered capacity of the hypervisor, considering the
	// applied overcommit ratios.
	//
	// In case no overcommit ratio is specified for a resource type, the default
	// overcommit ratio of 1 should be applied, meaning the effective capacity
	// is the same as the actual capacity.
	//
	// If the overcommit ratio results in a fractional effective capacity, the
	// effective capacity is expected to be rounded down.
	//
	// +kubebuilder:validation:Optional
	EffectiveCapacity map[ResourceName]resource.Quantity `json:"effectiveCapacity,omitempty"`

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

	// Aggregates are the applied aggregates of the hypervisor with their names and UUIDs.
	Aggregates []Aggregate `json:"aggregates,omitempty"`

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
