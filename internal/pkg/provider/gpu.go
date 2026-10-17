// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/siderolabs/omni/client/pkg/infra/provision"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const gpusAnnotation = "nutanix.infra.sidero.dev/gpu-config"
const gpuNamesAnnotation = "nutanix.infra.sidero.dev/gpu-names"

func gpuSnapshot(machine *resources.Machine) ([]client.GPUConfig, bool, error) {
	raw, ok := machine.Metadata().Annotations().Get(gpusAnnotation)
	if !ok {
		if _, orphan := machine.Metadata().Annotations().Get(gpuNamesAnnotation); orphan {
			return nil, false, fmt.Errorf("GPU names lack their frozen device configuration")
		}
		return nil, false, nil
	}
	var gpus []client.GPUConfig
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > maxMigrationIntentBytes || !json.Valid([]byte(raw)) || decoder.Decode(&gpus) != nil || gpus == nil {
		return nil, true, fmt.Errorf("invalid persisted GPU configuration")
	}
	if err := client.ValidateGPUs(gpus); err != nil {
		return nil, true, fmt.Errorf("invalid persisted GPU configuration: %w", err)
	}
	if namesRaw, present := machine.Metadata().Annotations().Get(gpuNamesAnnotation); present {
		var names []string
		if len(namesRaw) > maxMigrationIntentBytes || json.Unmarshal([]byte(namesRaw), &names) != nil || names == nil || len(names) != len(gpus) || client.ValidateGPUNames(names) != nil {
			return nil, true, fmt.Errorf("invalid persisted GPU names/count")
		}
	}
	return gpus, true, nil
}

func (p *Provisioner) freezeRequestedGPUs(ctx context.Context, pctx provision.Context[*resources.Machine], data *Data) ([]client.GPUConfig, error) {
	gpus, present, err := gpuSnapshot(pctx.State)
	if err != nil || present {
		return gpus, err
	} // Never resolve a saved name again on restart/replay.
	if len(data.GPUNames) == 0 {
		return freezeGPUs(pctx, data.GPUs)
	}
	if len(data.GPUs) > 0 {
		return nil, fmt.Errorf("use gpu names or legacy gpus configuration, not both")
	}
	if err := client.ValidateGPUNames(data.GPUNames); err != nil {
		return nil, err
	}
	if _, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation); submitted {
		return nil, fmt.Errorf("GPU snapshot missing after creation intent; refusing name-based GPU backfill")
	}
	cluster, _ := pctx.State.Metadata().Annotations().Get(clusterAnnotation)
	gpus, err = p.c.ResolveGPUs(ctx, cluster, data.GPUNames)
	if err != nil {
		return nil, provision.NewRetryErrorf(vmRetry, "resolve GPU names: %w", err)
	}
	if len(gpus) != len(data.GPUNames) || client.ValidateGPUs(gpus) != nil {
		return nil, fmt.Errorf("GPU name resolution did not preserve valid requested devices/count")
	}
	namesRaw, err := json.Marshal(data.GPUNames)
	if err != nil || len(namesRaw) > maxMigrationIntentBytes {
		return nil, fmt.Errorf("GPU names exceed annotation limit")
	}
	resolved, err := freezeGPUs(pctx, gpus)
	if _, saved := pctx.State.Metadata().Annotations().Get(gpusAnnotation); saved {
		pctx.State.Metadata().Annotations().Set(gpuNamesAnnotation, string(namesRaw)) // Both snapshots saved in the same SDK state write.
	}
	return resolved, err
}

func freezeGPUs(pctx provision.Context[*resources.Machine], requested []client.GPUConfig) ([]client.GPUConfig, error) {
	gpus, present, err := gpuSnapshot(pctx.State)
	if err != nil || present {
		return gpus, err
	}
	if _, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation); submitted {
		if len(requested) > 0 {
			return nil, fmt.Errorf("GPU snapshot missing after creation intent; refusing GPU backfill")
		}
		return nil, nil // Submitted pre-GPU requests keep their original payload.
	}
	if err := client.ValidateGPUs(requested); err != nil {
		return nil, err
	}
	if requested == nil {
		requested = []client.GPUConfig{}
	}
	raw, err := json.Marshal(requested)
	if err != nil {
		return nil, err
	}
	pctx.State.Metadata().Annotations().Set(gpusAnnotation, string(raw))
	return nil, provision.NewRetryInterval(time.Millisecond) // Persist before POST, including the empty selection.
}

func verifyVMGPUs(vm *client.VM, pctx provision.Context[*resources.Machine]) error {
	expected, present, err := gpuSnapshot(pctx.State)
	if err != nil {
		return err
	}
	if !present {
		var data Data
		if err := pctx.UnmarshalProviderData(&data); err != nil {
			return fmt.Errorf("decode GPU configuration")
		}
		if len(data.GPUNames) > 0 || len(data.GPUs) > 0 {
			return fmt.Errorf("GPU snapshot missing; refusing creation completion/power-on")
		}
		return nil // Historical requests without GPU selection are not retrofitted.
	}
	if vm == nil || len(vm.GPUs) != len(expected) {
		return fmt.Errorf("VM GPU inventory does not match frozen selection; refusing completion/power-on")
	}
	if err := client.ValidateGPUs(vm.GPUs); err != nil {
		return fmt.Errorf("VM GPU metadata missing or unexpected; refusing completion/power-on")
	}
	type key struct {
		id           int64
		mode, vendor string
	}
	counts := map[key]int{}
	for _, gpu := range expected {
		counts[key{*gpu.DeviceID, gpu.Mode, gpu.Vendor}]++
	}
	for _, gpu := range vm.GPUs {
		k := key{*gpu.DeviceID, gpu.Mode, gpu.Vendor}
		if counts[k] == 0 {
			return fmt.Errorf("VM GPU device/vendor/mode differs from frozen selection; refusing completion/power-on")
		}
		counts[k]--
	}
	return nil
}
