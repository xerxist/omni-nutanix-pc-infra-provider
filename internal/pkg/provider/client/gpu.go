// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// GPUConfig selects one GA v4.0 GPU resource. Repeat an entry to request several
// identical GPUs; PC handles placement/capacity and profile/device eligibility.
// A pointer distinguishes omitted deviceId from valid zero in PC observations.
type GPUConfig struct {
	DeviceID *int64 `yaml:"device_id" json:"device_id"`
	Mode     string `yaml:"mode" json:"mode"`
	Vendor   string `yaml:"vendor" json:"vendor"`
}

// Both YAML versions used by the SDK/examples support this callback signature.
// Reject unsupported nested fields (especially count) rather than silently
// provisioning fewer GPUs when providerData bypasses the UI's schema validator.
func (gpu *GPUConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var fields map[string]any
	if err := unmarshal(&fields); err != nil {
		return err
	}
	for field := range fields {
		if field != "device_id" && field != "vendor" && field != "mode" {
			return fmt.Errorf("unsupported GPU field; use device_id, vendor and mode, one entry per GPU")
		}
	}
	type plain GPUConfig
	var decoded plain
	if err := unmarshal(&decoded); err != nil {
		return err
	}
	*gpu = GPUConfig(decoded)
	return nil
}

// Only writable GPU selection fields are sent. pciAddress and GPU name, memory,
// fraction and guest-driver properties are observations, not selectors.
type vmGPU struct {
	ObjectType string `json:"$objectType"`
	DeviceID   *int64 `json:"deviceId"`
	Mode       string `json:"mode"`
	Vendor     string `json:"vendor"`
}

func ValidateGPUs(gpus []GPUConfig) error {
	if len(gpus) > 16 {
		return fmt.Errorf("gpus must contain at most 16 entries")
	}
	for i, gpu := range gpus {
		if gpu.DeviceID == nil || *gpu.DeviceID < 0 || *gpu.DeviceID > math.MaxInt32 {
			return fmt.Errorf("gpus[%d].device_id must be a nonnegative int32", i)
		}
		switch gpu.Mode {
		case "PASSTHROUGH_GRAPHICS", "PASSTHROUGH_COMPUTE", "VIRTUAL":
		default:
			return fmt.Errorf("gpus[%d].mode must be PASSTHROUGH_GRAPHICS, PASSTHROUGH_COMPUTE or VIRTUAL", i)
		}
		switch gpu.Vendor {
		case "NVIDIA", "INTEL", "AMD":
		default:
			return fmt.Errorf("gpus[%d].vendor must be NVIDIA, INTEL or AMD", i)
		}
	}
	return nil
}

// ValidateGPUNames preserves exact PC names; repeated entries request repeated
// devices. Never deduplicate this list or apply fuzzy/case-insensitive matching.
func ValidateGPUNames(names []string) error {
	if len(names) > 16 {
		return fmt.Errorf("gpu must contain at most 16 names")
	}
	for i, name := range names {
		if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 128 || strings.ContainsFunc(name, unicode.IsControl) {
			return fmt.Errorf("gpu[%d] must be a nonempty name of at most 128 characters without control characters", i)
		}
	}
	return nil
}

type gpuCatalogConfig struct {
	DeviceID *int64 `json:"deviceId"`
	Name     string `json:"deviceName"`
	Vendor   string `json:"vendorName"`
	Mode     string `json:"type"` // clustermgmt GpuType, not physicalGpuConfig.mode (host usage state).
	InUse    *bool  `json:"isInUse"`
}

type gpuCatalogProfile struct {
	Physical *gpuCatalogConfig `json:"physicalGpuConfig"`
	Virtual  *gpuCatalogConfig `json:"virtualGpuConfig"`
}

func catalogGPUVendor(vendor string) string {
	// PC's inventory uses the k-prefixed names also handled by CAPX, whereas
	// VM creation expects the GA GpuVendor enum. Unknown vendors never fall back.
	switch vendor {
	case "kNvidia", "NVIDIA":
		return "NVIDIA"
	case "kIntel", "INTEL":
		return "INTEL"
	case "kAmd", "AMD":
		return "AMD"
	default:
		return ""
	}
}

// ResolveGPUs implements CAPX-style device/vGPU name lookup using the GA v4.0
// cluster catalogs. These are distinct from v4.3 named AHV profile references.
// PC/AHV remain responsible for allocating enough colocated hardware; this GET
// is not a reservation or a promise of capacity for repeated names.
func (c *v4Client) ResolveGPUs(ctx context.Context, clusterUUID string, names []string) ([]GPUConfig, error) {
	if err := ValidateGPUNames(names); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []GPUConfig{}, nil
	}
	if id, err := uuid.Parse(clusterUUID); err != nil || id == uuid.Nil {
		return nil, fmt.Errorf("GPU name resolution requires the selected cluster UUID")
	}
	requested := map[string]bool{}
	ordered := []string{}
	for _, name := range names {
		if !requested[name] {
			requested[name] = true
			ordered = append(ordered, name)
		}
	}
	type selection struct {
		config    GPUConfig
		available bool
	}
	found := map[string]selection{}
	for _, catalog := range []string{"physical", "virtual"} {
		filters := make([]string, 0, len(ordered))
		for _, name := range ordered {
			filters = append(filters, catalog+"GpuConfig/deviceName eq '"+strings.ReplaceAll(name, "'", "''")+"'")
		}
		profiles, err := list[gpuCatalogProfile](ctx, c, c.cluster+"/clusters/"+url.PathEscape(clusterUUID)+"/"+catalog+"-gpu-profiles", strings.Join(filters, " or "))
		if err != nil {
			return nil, fmt.Errorf("read %s GPU catalog: %w", catalog, err)
		}
		for _, profile := range profiles {
			config, other := profile.Physical, profile.Virtual
			if catalog == "virtual" {
				config, other = profile.Virtual, profile.Physical
			}
			if config == nil || other != nil {
				return nil, fmt.Errorf("GPU catalog configuration missing or conflicting; refusing name resolution")
			}
			if !requested[config.Name] {
				continue
			} // Do not trust a server filter to be exact.
			gpu := GPUConfig{DeviceID: config.DeviceID, Mode: config.Mode, Vendor: catalogGPUVendor(config.Vendor)}
			if ValidateGPUs([]GPUConfig{gpu}) != nil || config.InUse == nil || (catalog == "virtual" && gpu.Mode != "VIRTUAL") || (catalog == "physical" && gpu.Mode == "VIRTUAL") {
				return nil, fmt.Errorf("GPU %q has unknown/inconsistent device, vendor, mode or availability metadata", config.Name)
			}
			previous, exists := found[config.Name]
			if exists && (*previous.config.DeviceID != *gpu.DeviceID || previous.config.Mode != gpu.Mode || previous.config.Vendor != gpu.Vendor) {
				return nil, fmt.Errorf("GPU name %q maps to different devices/vendors/modes in the selected cluster; refusing ambiguous selection", config.Name)
			}
			// Identical logical resources may occur on several hosts. Do not pick
			// randomly, require an explicitly unused candidate, and preserve count.
			found[config.Name] = selection{config: gpu, available: previous.available || !*config.InUse}
		}
	}
	result := make([]GPUConfig, 0, len(names))
	for _, name := range names {
		gpu, exists := found[name]
		if !exists {
			return nil, fmt.Errorf("GPU name %q not found in the selected cluster", name)
		}
		if !gpu.available {
			return nil, fmt.Errorf("no explicitly unused GPU for name %q in the selected cluster", name)
		}
		result = append(result, gpu.config)
	}
	return result, nil
}
