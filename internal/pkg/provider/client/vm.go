// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"encoding/base64"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/google/uuid"
)

// These wire types follow the Nutanix GA v4.0 AHV models, including union tags.
type reference struct {
	ObjectType string `json:"$objectType"`
	UUID       string `json:"extId"`
}
type vmDisk struct {
	ObjectType string `json:"$objectType"`
	Address    struct {
		Bus   string `json:"busType"`
		Index int    `json:"index"`
	} `json:"diskAddress"`
	Backing diskBacking `json:"backingInfo"`
}
type diskBacking struct {
	ObjectType string     `json:"$objectType"`
	Size       int64      `json:"diskSizeBytes"`
	Container  *reference `json:"storageContainer,omitempty"`
	Source     *struct {
		Reference imageReference `json:"reference"`
	} `json:"dataSource,omitempty"`
}

// ImageReference differs from other v4 references: it uses imageExtId.
type imageReference struct {
	ObjectType string `json:"$objectType"`
	UUID       string `json:"imageExtId"`
}
type vmNIC struct {
	ObjectType string `json:"$objectType"`
	Backing    struct {
		ObjectType string `json:"$objectType"`
		Model      string `json:"model"`
		Connected  bool   `json:"isConnected"`
	} `json:"backingInfo"`
	Network struct {
		ObjectType string    `json:"$objectType"`
		Type       string    `json:"nicType"`
		Subnet     reference `json:"subnet"`
	} `json:"networkInfo"`
}
type vmBody struct {
	ObjectType  string `json:"$objectType"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// powerState is observation-only for creation: PC rejects even OFF with
	// VMM-30109. The provisioner must verify actual OFF before disk migration.
	BIOSUUID   string      `json:"biosUuid"`
	Cluster    reference   `json:"cluster"`
	Sockets    int         `json:"numSockets"`
	Cores      int         `json:"numCoresPerSocket"`
	Threads    int         `json:"numThreadsPerCore"`
	Memory     int64       `json:"memorySizeBytes"`
	Disks      []vmDisk    `json:"disks"`
	NICs       []vmNIC     `json:"nics"`
	Categories []reference `json:"categories,omitempty"`
	GPUs       []vmGPU     `json:"gpus,omitempty"`
	Boot       struct {
		ObjectType    string   `json:"$objectType"`
		Order         []string `json:"bootOrder"`
		SecureEnabled *bool    `json:"isSecureBootEnabled,omitempty"`
	} `json:"bootConfig"`
	Guest struct {
		ObjectType string `json:"$objectType"`
		Config     struct {
			ObjectType string `json:"$objectType"`
			Datasource string `json:"datasourceType"`
			Metadata   string `json:"metadata"`
			Script     struct {
				ObjectType string `json:"$objectType"`
				Value      string `json:"value"`
			} `json:"cloudInitScript"`
		} `json:"config"`
	} `json:"guestCustomization"`
}

// MaxVMDescriptionLength is a provider limit, not an advertised Prism limit.
// Base64-encoded worst-case UTF-8 stays below Omni's 16KiB annotation limit.
const MaxVMDescriptionLength = 2048

const BootModeUEFI = "uefi"
const BootModeLegacy = "legacy"

func ValidateVMDescription(description string) error {
	if !utf8.ValidString(description) || utf8.RuneCountInString(description) > MaxVMDescriptionLength {
		return fmt.Errorf("description must be valid UTF-8 with at most %d characters", MaxVMDescriptionLength)
	}
	return nil
}

func buildVM(spec VMCreateSpec) (*vmBody, error) {
	if err := ValidateVMDescription(spec.Description); err != nil {
		return nil, err
	}
	if spec.Name == "" || spec.ClusterUUID == "" || spec.ImageUUID == "" || spec.SubnetUUID == "" {
		return nil, fmt.Errorf("VM requires name, cluster, image, and subnet")
	}
	if _, err := uuid.Parse(spec.BIOSUUID); err != nil {
		return nil, fmt.Errorf("VM requires a valid BIOS UUID")
	}
	if _, err := uuid.Parse(spec.RequestUUID); err != nil {
		return nil, fmt.Errorf("VM requires a valid request UUID")
	}
	if spec.CPU <= 0 || spec.Cores <= 0 || spec.MemoryMB <= 0 || spec.MemoryMB > math.MaxInt64/(1<<20) {
		return nil, fmt.Errorf("invalid CPU or memory size")
	}
	if len(spec.AdditionalDiskGB) > 0 && spec.ContainerUUID == "" {
		return nil, fmt.Errorf("additional disks require a storage container")
	}
	if len(spec.AdditionalDiskGB) > 1023 {
		return nil, fmt.Errorf("at most 1023 additional disks are allowed per VM")
	}
	if data, err := base64.StdEncoding.DecodeString(spec.UserDataB64); err != nil || len(data) == 0 {
		return nil, fmt.Errorf("VM requires nonempty base64 Talos join configuration")
	}
	body := &vmBody{
		ObjectType: "vmm.v4.ahv.config.Vm", Name: spec.Name, Description: spec.Description, BIOSUUID: spec.BIOSUUID,
		Cluster: reference{"vmm.v4.ahv.config.ClusterReference", spec.ClusterUUID},
		Sockets: spec.CPU, Cores: spec.Cores, Threads: 1, Memory: spec.MemoryMB * (1 << 20),
	}
	if err := ValidateGPUs(spec.GPUs); err != nil {
		return nil, err
	}
	for _, gpu := range spec.GPUs {
		body.GPUs = append(body.GPUs, vmGPU{ObjectType: "vmm.v4.ahv.config.Gpu", Mode: gpu.Mode, Vendor: gpu.Vendor, DeviceID: gpu.DeviceID})
	}
	if len(spec.CategoryUUIDs) > 256 {
		return nil, fmt.Errorf("at most 256 categories are allowed per VM")
	}
	seenCategories := make(map[string]bool)
	for _, id := range spec.CategoryUUIDs {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("category reference must be a UUID")
		}
		id = parsed.String()
		if !seenCategories[id] {
			body.Categories = append(body.Categories, reference{"vmm.v4.ahv.config.CategoryReference", id})
			seenCategories[id] = true
		}
	}
	for index, size := range append([]int64{spec.BootDiskGB}, spec.AdditionalDiskGB...) {
		if size <= 0 || size > math.MaxInt64/(1<<30) {
			return nil, fmt.Errorf("disk %d has invalid size", index)
		}
		disk := vmDisk{ObjectType: "vmm.v4.ahv.config.Disk", Backing: diskBacking{ObjectType: "vmm.v4.ahv.config.VmDisk", Size: size * (1 << 30)}}
		disk.Address.Bus, disk.Address.Index = "SCSI", index
		if spec.ContainerUUID != "" {
			disk.Backing.Container = &reference{"vmm.v4.ahv.config.VmDiskContainerReference", spec.ContainerUUID}
		}
		if index == 0 {
			disk.Backing.Source = &struct {
				Reference imageReference `json:"reference"`
			}{imageReference{"vmm.v4.ahv.config.ImageReference", spec.ImageUUID}}
		}
		body.Disks = append(body.Disks, disk)
	}
	for _, subnet := range append([]string{spec.SubnetUUID}, spec.AdditionalSubnetUUIDs...) {
		if subnet == "" {
			return nil, fmt.Errorf("NIC requires a subnet UUID")
		}
		nic := vmNIC{ObjectType: "vmm.v4.ahv.config.Nic"}
		nic.Backing.ObjectType, nic.Backing.Model, nic.Backing.Connected = "vmm.v4.ahv.config.EmulatedNic", "VIRTIO", true
		nic.Network.ObjectType, nic.Network.Type = "vmm.v4.ahv.config.NicNetworkInfo", "NORMAL_NIC"
		nic.Network.Subnet = reference{"vmm.v4.ahv.config.SubnetReference", subnet}
		body.NICs = append(body.NICs, nic)
	}
	body.Boot.Order = []string{"DISK", "CDROM", "NETWORK"}
	switch spec.BootMode {
	case "", BootModeUEFI:
		body.Boot.ObjectType = "vmm.v4.ahv.config.UefiBoot"
		secureBoot := false // The generic openstack image is not a Secure Boot image.
		body.Boot.SecureEnabled = &secureBoot
	case BootModeLegacy:
		body.Boot.ObjectType = "vmm.v4.ahv.config.LegacyBoot"
	default:
		return nil, fmt.Errorf("invalid frozen VM boot mode")
	}
	body.Guest.ObjectType = "vmm.v4.ahv.config.GuestCustomizationParams"
	body.Guest.Config.ObjectType, body.Guest.Config.Datasource = "vmm.v4.ahv.config.CloudInit", "CONFIG_DRIVE_V2"
	body.Guest.Config.Metadata = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"uuid":%q,"hostname":%q}`, spec.BIOSUUID, spec.Name)))
	body.Guest.Config.Script.ObjectType, body.Guest.Config.Script.Value = "vmm.v4.ahv.config.Userdata", spec.UserDataB64
	return body, nil
}
