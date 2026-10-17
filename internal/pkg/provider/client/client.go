// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package client implements the Prism Central v4 API surface used by the provider.
package client

import "context"

// ImageStatus is a coarse status used by the provisioner.
type ImageStatus int

const (
	// ImageStatusPending indicates the image is still uploading.
	ImageStatusPending ImageStatus = iota
	// ImageStatusAvailable indicates the image is ready for use.
	ImageStatusAvailable
	// ImageStatusFailed indicates the image upload failed.
	ImageStatusFailed
)

// TaskState is a coarse task state used by the provisioner.
type TaskState int

const (
	// TaskPending indicates the task is queued or running.
	TaskPending TaskState = iota
	// TaskSucceeded indicates the task finished successfully.
	TaskSucceeded
	// TaskFailed indicates the task finished with an error.
	TaskFailed
)

// TaskResult is the outcome of WaitTask.
type TaskResult struct {
	State  TaskState
	VMUUID string // Unambiguous VM extId from a terminal task, including failure; never its BIOS UUID.
	// MigrationLimitRejected is true only for FAILED with exclusively the known
	// VMM-30716 error code. Callers must still prove VM identity and unchanged OFF
	// source inventory before preparing a new, bounded migration attempt.
	MigrationLimitRejected bool
}

// Image is a slim representation of a Nutanix Image.
type Image struct {
	UUID         string
	Name         string
	Status       ImageStatus
	SizeBytes    int64
	Type         string
	Description  string
	SourceURL    string
	SourceType   string
	SHA256       string
	ClusterUUIDs []string
}

// ImageCreateSpec is immutable and contains no guest or authentication material.
type ImageCreateSpec struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SourceURL    string   `json:"source_url"`
	SHA256       string   `json:"sha256"`
	ClusterUUIDs []string `json:"cluster_uuids"`
	RequestUUID  string   `json:"request_uuid"`
}

// VM is a slim representation of a Nutanix VM.
type VM struct {
	UUID                          string
	Name                          string
	Description                   string
	PowerState                    string // "ON" | "OFF" | other
	BIOSUUID                      string
	ClusterUUID                   string
	ETag                          string
	CategoryUUIDs                 []string
	Disks                         []VMDisk
	CDROMs                        []VMCDROM
	GPUs                          []GPUConfig
	CrossClusterMigrationInFlight bool
	CrossClusterMigrationKnown    bool
}

// VMCDROM contains only media identity/placement, never ISO or bootstrap content.
type VMCDROM struct {
	VMDisk
	ISOType string
}

// VMDisk contains only placement metadata, never guest configuration.
type VMDisk struct {
	UUID              string // backingInfo.diskExtId; migration references this, not the device extId
	DeviceUUID        string
	Bus               string
	Index             int // -1 when the API omitted the disk index
	BackingType       string
	SizeBytes         int64
	ContainerUUID     string
	MigrationInFlight bool
	MigrationKnown    bool
}

// DiskMigrationSpec is an immutable operation. Replays preserve ETag as well as
// the idempotence token: never turn an unknown outcome into a new mutation.
type DiskMigrationSpec struct {
	VMUUID, ContainerUUID, RequestUUID, ETag string
	DiskUUIDs                                []string
}

// Selector identifies a Prism Central entity by ID or an unambiguous name.
type Selector struct {
	UUID string
	Name string
}

// SubnetSelector scopes names to a VPC for overlays and a cluster for VLANs.
type SubnetSelector struct {
	Selector
	VPC         Selector
	ClusterUUID string
}

// CategorySelector identifies an existing Prism Central category by UUID or key/value.
// When both forms are supplied they must identify the same category.
type CategorySelector struct {
	UUID  string
	Key   string
	Value string
}

// VMCreateSpec describes the VM the provisioner wants to create.
type VMCreateSpec struct {
	Name                  string
	Description           string // Optional literal Prism VM description, frozen with creation intent.
	BIOSUUID              string
	RequestUUID           string
	ClusterUUID           string
	SubnetUUID            string   // primary NIC subnet
	AdditionalSubnetUUIDs []string // extra NIC subnets, resolved at the provisioner layer
	CategoryUUIDs         []string // existing categories to associate during VM creation
	ImageUUID             string
	ContainerUUID         string
	CPU                   int // sockets
	Cores                 int // cores per socket
	MemoryMB              int64
	BootDiskGB            int64
	BootMode              string // UEFI by default; legacy is explicit only for historical create replay.
	AdditionalDiskGB      []int64
	GPUs                  []GPUConfig
	UserDataB64           string
}

// Client abstracts Prism Central discovery and asynchronous VM operations.
type Client interface {
	// CheckHealth probes required discovery reads only; it never tests mutations.
	CheckHealth(ctx context.Context) error
	GetImage(ctx context.Context, uuid string) (*Image, error)
	ResolveImage(ctx context.Context, selector Selector) (*Image, error)
	FindImage(ctx context.Context, name string) (*Image, error)
	CreateImage(ctx context.Context, spec ImageCreateSpec) (taskUUID string, err error)
	GetImageTask(ctx context.Context, taskID string) (*ImageTaskInfo, error)
	ResolveCluster(ctx context.Context, selector Selector) (string, error)
	ResolveSubnet(ctx context.Context, selector SubnetSelector) (string, error)
	ResolveContainer(ctx context.Context, selector Selector, clusterUUID string) (string, error)
	ResolveCategories(ctx context.Context, selectors []CategorySelector) ([]string, error)
	ResolveGPUs(ctx context.Context, clusterUUID string, names []string) ([]GPUConfig, error)

	CreateVM(ctx context.Context, spec VMCreateSpec) (taskUUID string, err error)
	FindVM(ctx context.Context, name, biosUUID, clusterUUID string) (*VM, error)
	GetVM(ctx context.Context, uuid string) (*VM, error)
	PowerOnVM(ctx context.Context, vmUUID string) (taskUUID string, err error)
	PowerOffVM(ctx context.Context, vmUUID string) (taskUUID string, err error)
	DeleteVM(ctx context.Context, vmUUID string) (taskUUID string, err error)
	MigrateVMDisks(ctx context.Context, spec DiskMigrationSpec) (taskUUID string, err error)

	WaitTask(ctx context.Context, taskUUID string) (TaskResult, error)

	IsNotFound(err error) bool
}
