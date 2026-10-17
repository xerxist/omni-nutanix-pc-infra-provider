// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
)

// Data is per-machine configuration supplied by an Omni MachineClass.
type Data struct {
	Description   string `yaml:"description,omitempty"`
	CPU           int    `yaml:"cpu"`
	Cores         int    `yaml:"cores"`
	MemoryMB      int64  `yaml:"memory_mb"`
	DiskGB        int64  `yaml:"disk_gb"`
	ClusterName   string `yaml:"cluster_name,omitempty"`
	ClusterUUID   string `yaml:"cluster_uuid,omitempty"`
	ContainerName string `yaml:"container_name,omitempty"`
	ContainerUUID string `yaml:"container_uuid,omitempty"`
	// Read historical YAML only; never controls policy for a new request and is
	// deliberately absent from the MachineClass schema.
	LegacyMigrateDisks bool             `yaml:"migrate_disks,omitempty"`
	SubnetName         string           `yaml:"subnet_name,omitempty"`
	SubnetUUID         string           `yaml:"subnet_uuid,omitempty"`
	VPCName            string           `yaml:"vpc_name,omitempty"`
	VPCUUID            string           `yaml:"vpc_uuid,omitempty"`
	ImageName          string           `yaml:"image_name,omitempty"`
	ImageUUID          string           `yaml:"image_uuid,omitempty"`
	ImageImport        *ImageImport     `yaml:"image_import,omitempty"`
	InitialExtensions  []string         `yaml:"initial_extensions,omitempty"`
	AdditionalDisks    []AdditionalDisk `yaml:"additional_disks,omitempty"`
	AdditionalNICs     []AdditionalNIC  `yaml:"additional_nics,omitempty"`
	Categories         []Category       `yaml:"categories,omitempty"`
	GPUNames           []string         `yaml:"gpu,omitempty"`
	// Compatibility with earlier explicit-ID YAML; the UI uses gpu name strings.
	GPUs []client.GPUConfig `yaml:"gpus,omitempty"`
}

// ImageImport enables import to the VM cluster. Historical explicit destinations
// remain readable for old MachineRequests but are absent from the new schema.
type ImageImport struct {
	Clusters     []ImageCluster `yaml:"clusters"`
	UseVMCluster bool           `yaml:"-"`
	disabled     bool
}

func (i *ImageImport) UnmarshalYAML(unmarshal func(any) error) error {
	var value any
	if err := unmarshal(&value); err != nil {
		return err
	}
	if enabled, ok := value.(bool); ok {
		*i = ImageImport{UseVMCluster: true, disabled: !enabled}
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("image_import must be a boolean")
	}
	type plain ImageImport
	var decoded plain
	if err := unmarshal(&decoded); err != nil {
		return err
	}
	*i = ImageImport(decoded)
	return nil
}

func (i ImageImport) MarshalYAML() (any, error) {
	if i.UseVMCluster {
		return !i.disabled, nil
	}
	return struct {
		Clusters []ImageCluster `yaml:"clusters"`
	}{i.Clusters}, nil
}

// ImageCluster selects a registered AHV cluster by UUID or unambiguous name.
type ImageCluster struct {
	Name string `yaml:"name,omitempty"`
	UUID string `yaml:"uuid,omitempty"`
}

// YAML otherwise coerces numeric scalars into strings. Reject non-string entries
// in extension/GPU name lists even outside the UI schema.
func (d *Data) UnmarshalYAML(unmarshal func(any) error) error {
	var fields map[string]any
	if err := unmarshal(&fields); err != nil {
		return err
	}
	if value, present := fields["description"]; present {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("description must be a string")
		}
	}
	if value, present := fields["image_import"]; present {
		if _, boolean := value.(bool); !boolean {
			options, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("image_import must be a boolean")
			}
			for key := range options {
				if key != "clusters" {
					return fmt.Errorf("unknown image_import option")
				}
			}
			clusters, ok := options["clusters"].([]any)
			if !ok {
				return fmt.Errorf("image_import.clusters must be a list of explicit selectors")
			}
			for _, value := range clusters {
				selector, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("image_import.clusters entries must be objects")
				}
				for key, value := range selector {
					if key != "name" && key != "uuid" {
						return fmt.Errorf("unknown image destination selector field")
					}
					if text, ok := value.(string); !ok || strings.TrimSpace(text) == "" {
						return fmt.Errorf("image destination selectors must be nonempty strings")
					}
				}
			}
		}
	}
	if value, present := fields["gpu"]; present {
		names, ok := value.([]any)
		if !ok {
			return fmt.Errorf("gpu must be a list of name strings, one entry per GPU")
		}
		for _, name := range names {
			if _, ok := name.(string); !ok {
				return fmt.Errorf("gpu entries must be name strings, not counts or device IDs")
			}
		}
	}
	if value, present := fields["initial_extensions"]; present {
		names, ok := value.([]any)
		if !ok {
			return fmt.Errorf("initial_extensions must be a list of official extension name strings")
		}
		for _, name := range names {
			if _, ok := name.(string); !ok {
				return fmt.Errorf("initial_extensions entries must be strings")
			}
		}
	}
	type plain Data
	var decoded plain
	if err := unmarshal(&decoded); err != nil {
		return err
	}
	*d = Data(decoded)
	if d.ImageImport != nil && d.ImageImport.disabled {
		d.ImageImport = nil
	}
	return nil
}

// Category selects an existing PC category, which may select Flow security policies.
type Category struct {
	UUID  string `yaml:"uuid,omitempty"`
	Key   string `yaml:"key,omitempty"`
	Value string `yaml:"value,omitempty"`
}

func (c Category) selector() client.CategorySelector {
	return client.CategorySelector{UUID: c.UUID, Key: c.Key, Value: c.Value}
}

// AdditionalDisk is an ephemeral raw block device, sized in GiB.
type AdditionalDisk struct {
	Size int64 `yaml:"size"`
}

// AdditionalNIC independently selects a VLAN or overlay subnet.
type AdditionalNIC struct {
	SubnetName string `yaml:"subnet_name,omitempty"`
	SubnetUUID string `yaml:"subnet_uuid,omitempty"`
	VPCName    string `yaml:"vpc_name,omitempty"`
	VPCUUID    string `yaml:"vpc_uuid,omitempty"`
}

func validateSelector(kind, name, id string, required bool) error {
	if required && name == "" && id == "" {
		return fmt.Errorf("%s_name or %s_uuid is required", kind, kind)
	}
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("%s_uuid must be a UUID", kind)
		}
	}
	return nil
}

// Validate prevents silent omission of invalid disks and ambiguous placement.
func (d *Data) Validate() error {
	return d.validate(true)
}

func (d *Data) validate(requireContainer bool) error {
	if len(d.InitialExtensions) > 256 {
		return fmt.Errorf("initial_extensions must contain at most 256 entries")
	}
	if len(d.InitialExtensions) > 0 && d.ImageImport == nil {
		return fmt.Errorf("initial_extensions requires automatic image import")
	}
	for _, name := range d.InitialExtensions {
		if len(name) > 128 || !imageExtensionPattern.MatchString(name) {
			return fmt.Errorf("initial_extensions requires official siderolabs extension names of at most 128 bytes")
		}
	}
	if d.ImageImport != nil {
		if d.ImageName != "" || d.ImageUUID != "" {
			return fmt.Errorf("image_import and manual image selectors are mutually exclusive")
		}
		if d.ImageImport.UseVMCluster && len(d.ImageImport.Clusters) != 0 {
			return fmt.Errorf("automatic VM-cluster import cannot include explicit destinations")
		}
		if !d.ImageImport.UseVMCluster && (len(d.ImageImport.Clusters) == 0 || len(d.ImageImport.Clusters) > 200) {
			return fmt.Errorf("image_import.clusters must contain 1 to 200 explicit destinations")
		}
		for i, cluster := range d.ImageImport.Clusters {
			if err := validateSelector("cluster", cluster.Name, cluster.UUID, true); err != nil {
				return fmt.Errorf("image_import.clusters[%d]: %w", i, err)
			}
		}
	}
	if err := client.ValidateVMDescription(d.Description); err != nil {
		return err
	}
	if d.CPU <= 0 || d.Cores <= 0 {
		return fmt.Errorf("cpu and cores must be > 0")
	}
	if d.MemoryMB < 512 || d.MemoryMB > math.MaxInt64/(1<<20) {
		return fmt.Errorf("memory_mb must be >= 512 and fit in bytes")
	}
	if d.DiskGB < 5 || d.DiskGB > math.MaxInt64/(1<<30) {
		return fmt.Errorf("disk_gb must be >= 5 and fit in bytes")
	}
	for _, selector := range []struct {
		kind, name, id string
		required       bool
	}{
		{"cluster", d.ClusterName, d.ClusterUUID, true},
		{"image", d.ImageName, d.ImageUUID, d.ImageImport == nil},
		{"subnet", d.SubnetName, d.SubnetUUID, true},
		{"vpc", d.VPCName, d.VPCUUID, false},
		{"container", d.ContainerName, d.ContainerUUID, requireContainer || len(d.AdditionalDisks) > 0 || d.LegacyMigrateDisks},
	} {
		if err := validateSelector(selector.kind, selector.name, selector.id, selector.required); err != nil {
			return err
		}
	}
	if len(d.AdditionalDisks) > 1023 {
		return fmt.Errorf("additional_disks must contain at most 1023 entries")
	}
	for i, disk := range d.AdditionalDisks {
		if disk.Size <= 0 || disk.Size > math.MaxInt64/(1<<30) {
			return fmt.Errorf("additional_disks[%d].size must be > 0 and fit in bytes", i)
		}
	}
	for i, nic := range d.AdditionalNICs {
		if err := validateSelector("subnet", nic.SubnetName, nic.SubnetUUID, true); err != nil {
			return fmt.Errorf("additional_nics[%d]: %w", i, err)
		}
		if err := validateSelector("vpc", nic.VPCName, nic.VPCUUID, false); err != nil {
			return fmt.Errorf("additional_nics[%d]: %w", i, err)
		}
	}
	if len(d.Categories) > 256 {
		return fmt.Errorf("categories must contain at most 256 entries")
	}
	for i, category := range d.Categories {
		if err := category.selector().Validate(); err != nil {
			return fmt.Errorf("categories[%d]: %w", i, err)
		}
	}
	if len(d.GPUNames) > 0 && len(d.GPUs) > 0 {
		return fmt.Errorf("use gpu names or legacy gpus configuration, not both")
	}
	if err := client.ValidateGPUNames(d.GPUNames); err != nil {
		return err
	}
	return client.ValidateGPUs(d.GPUs)
}

func (d *Data) subnet(clusterUUID string) client.SubnetSelector {
	return client.SubnetSelector{Selector: client.Selector{Name: d.SubnetName, UUID: d.SubnetUUID}, VPC: client.Selector{Name: d.VPCName, UUID: d.VPCUUID}, ClusterUUID: clusterUUID}
}

func (n AdditionalNIC) subnet(clusterUUID string) client.SubnetSelector {
	return client.SubnetSelector{Selector: client.Selector{Name: n.SubnetName, UUID: n.SubnetUUID}, VPC: client.Selector{Name: n.VPCName, UUID: n.VPCUUID}, ClusterUUID: clusterUUID}
}
