// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements the Nutanix infrastructure provider.
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/uuid"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/stdpatches"
	"go.uber.org/zap"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const (
	vmRetry                = 10 * time.Second
	taskRetry              = 5 * time.Second
	biosAnnotation         = "nutanix.infra.sidero.dev/bios-uuid"
	clusterAnnotation      = "nutanix.infra.sidero.dev/cluster-uuid"
	containerAnnotation    = "nutanix.infra.sidero.dev/container-uuid"
	createIntentAnnotation = "nutanix.infra.sidero.dev/create-intent"
	categoriesAnnotation   = "nutanix.infra.sidero.dev/category-uuids"
	guestTrustAnnotation   = "nutanix.infra.sidero.dev/guest-trust-config"
)

// Provisioner persists creation tasks and never assumes extId equals BIOS UUID.
type Provisioner struct {
	c               client.Client
	guestTrustPatch string
	migrationState  state.State
	migrationMu     sync.Mutex
	migrationOwner  *migrationQueueOwner
	migrationNow    func() time.Time
	imageState      state.State
	imageFactory    ImageFactory
	imageMu         sync.Mutex
}

func NewProvisioner(c client.Client) *Provisioner { return &Provisioner{c: c} }

func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("initialize", p.stepInitialize),
		provision.NewStep("ensureImage", p.stepEnsureImage),
		provision.NewStep("createVM", p.stepCreateVM),
		provision.NewStep("placeVMDisks", p.stepPlaceVMDisks),
		provision.NewStep("powerOnVM", p.stepPowerOnVM),
	}
}

func providerData(pctx provision.Context[*resources.Machine]) (*Data, error) {
	var data Data
	if err := pctx.UnmarshalProviderData(&data); err != nil {
		return nil, fmt.Errorf("decode provider data: %w", err)
	}
	// Submitted legacy verify-only identities may have delegated placement to PC.
	// Preserve their validation/teardown behavior, not this option for new VMs.
	_, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation)
	policy, _ := pctx.State.Metadata().Annotations().Get(storagePolicyAnnotation)
	legacyPlacement := submitted && (policy == storagePolicyVerify || (policy == "" && !data.LegacyMigrateDisks))
	if err := data.validate(!legacyPlacement); err != nil {
		return nil, fmt.Errorf("invalid provider data: %w", err)
	}
	return &data, nil
}

func (p *Provisioner) stepInitialize(_ context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	if _, err := providerData(pctx); err != nil {
		return err
	}
	if id, ok := pctx.State.Metadata().Annotations().Get(biosAnnotation); ok {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid persisted BIOS UUID")
		}
		pctx.SetMachineUUID(id)
		return nil
	}
	// Persist identity before the first remote side effect. It is stable across
	// retries/restarts, but unique if a MachineRequest ID is reused later.
	guestTrust, err := encodeGuestTrustSnapshot(p.guestTrustPatch)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	pctx.State.Metadata().Annotations().Set(biosAnnotation, id)
	// Freeze bootstrap trust with the identity. Restart/config changes must not
	// change the config-drive payload of a retried, idempotent creation request.
	pctx.State.Metadata().Annotations().Set(guestTrustAnnotation, guestTrust)
	pctx.SetMachineUUID(id)
	return provision.NewRetryInterval(time.Millisecond)
}

func (p *Provisioner) stepEnsureImage(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	// Existing lifecycle identities must never reselect or replace their boot image.
	_, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation)
	if submitted || pctx.State.TypedSpec().Value.VmUuid != "" || pctx.State.TypedSpec().Value.CreateTaskUuid != "" {
		return nil
	}
	if _, frozen := pctx.State.Metadata().Annotations().Get(imageImportPlanAnnotation); frozen {
		return p.ensureImportedImage(ctx, pctx)
	}
	data, err := providerData(pctx)
	if err != nil {
		return err
	}
	if data.ImageImport != nil && pctx.State.TypedSpec().Value.ImageUuid == "" {
		return p.freezeImageImport(ctx, pctx, data)
	}
	var img *client.Image
	if id := pctx.State.TypedSpec().Value.ImageUuid; id != "" {
		img, err = p.c.GetImage(ctx, id)
	} else {
		img, err = p.c.ResolveImage(ctx, client.Selector{Name: data.ImageName, UUID: data.ImageUUID})
	}
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "resolve Talos image: %w", err)
	}
	if img == nil || img.UUID == "" {
		return fmt.Errorf("Talos image resolution returned no identity")
	}
	if img.Type != "DISK_IMAGE" {
		return fmt.Errorf("Talos image must be a DISK_IMAGE, got %q", img.Type)
	}
	if img.Status == client.ImageStatusFailed {
		return fmt.Errorf("Talos image import failed")
	}
	if img.Status != client.ImageStatusAvailable {
		return provision.NewRetryInterval(vmRetry)
	}
	if data.DiskGB*(1<<30) < img.SizeBytes {
		return fmt.Errorf("disk_gb is smaller than the uploaded Talos image")
	}
	pctx.State.TypedSpec().Value.ImageUuid = img.UUID
	logger.Info("resolved Talos image", zap.String("image_uuid", img.UUID))
	return nil
}

func verifyVM(vm *client.VM, name, biosUUID, clusterUUID string) error {
	if vm == nil || vm.UUID == "" || vm.Name != name || !strings.EqualFold(vm.BIOSUUID, biosUUID) || vm.ClusterUUID != clusterUUID {
		return fmt.Errorf("VM does not match this MachineRequest's name, BIOS UUID, and cluster; refusing to manage it")
	}
	return nil
}

// verifyVMStorage fails closed: image clones can inherit the image's container
// even when the creation request supplied a different storageContainer. Only the
// separate pre-boot placement step may migrate; never relax the power-on guard.
func verifyVMStorage(vm *client.VM, pctx provision.Context[*resources.Machine]) error {
	if err := verifyStorage(vm, pctx, true); err != nil {
		return err
	}
	if configDriveMigrationEnabled(pctx.State) {
		return verifyConfigDrive(vm, pctx.State, true)
	}
	return nil
}

func verifyStorage(vm *client.VM, pctx provision.Context[*resources.Machine], requirePlacement bool) error {
	data, err := providerData(pctx)
	if err != nil {
		return err
	}
	expected, frozen := pctx.State.Metadata().Annotations().Get(containerAnnotation)
	if !frozen {
		if data.ContainerName == "" && data.ContainerUUID == "" {
			return nil // Placement explicitly delegated to Nutanix.
		}
		return fmt.Errorf("storage container snapshot missing; refusing to power on or complete provisioning (operator review required)")
	}
	if _, err := uuid.Parse(expected); err != nil {
		return fmt.Errorf("invalid persisted storage container UUID")
	}
	sizes := []int64{data.DiskGB}
	for _, disk := range data.AdditionalDisks {
		sizes = append(sizes, disk.Size)
	}
	if vm == nil || len(vm.Disks) != len(sizes) {
		return fmt.Errorf("VM disk inventory does not match requested disks; refusing to power on or complete provisioning")
	}
	seen := make(map[int]bool)
	for _, disk := range vm.Disks {
		if disk.Bus != "SCSI" || disk.Index < 0 || disk.Index >= len(sizes) || seen[disk.Index] ||
			disk.BackingType != "vmm.v4.ahv.config.VmDisk" || disk.SizeBytes != sizes[disk.Index]*(1<<30) {
			return fmt.Errorf("VM disk address, backing or size does not match requested disks; refusing to power on or complete provisioning")
		}
		seen[disk.Index] = true
		if requirePlacement && !strings.EqualFold(disk.ContainerUUID, expected) {
			return fmt.Errorf("VM SCSI disk %d is not in requested storage container %s; refusing to power on or complete provisioning", disk.Index, expected)
		}
		if disk.MigrationInFlight {
			return provision.NewRetryErrorf(vmRetry, "VM SCSI disk %d storage migration is still in progress; refusing to power on or complete provisioning", disk.Index)
		}
	}
	return nil
}

func (p *Provisioner) stepCreateVM(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	state := pctx.State.TypedSpec().Value
	biosUUID, _ := pctx.State.Metadata().Annotations().Get(biosAnnotation)
	clusterUUID, _ := pctx.State.Metadata().Annotations().Get(clusterAnnotation)
	if biosUUID == "" {
		return fmt.Errorf("machine identity was not initialized")
	}
	pctx.SetMachineUUID(biosUUID)
	if state.CreateTaskUuid != "" {
		result, err := p.c.WaitTask(ctx, state.CreateTaskUuid)
		if err != nil {
			if result.State == client.TaskFailed {
				return fmt.Errorf("VM creation task failed: %w", err)
			}
			return provision.NewRetryErrorf(vmRetry, "VM creation task: %w", err)
		}
		if result.State == client.TaskFailed {
			return fmt.Errorf("VM creation task failed; inspect Prism Central")
		}
		if result.State != client.TaskSucceeded {
			return provision.NewRetryInterval(taskRetry)
		}
		if result.VMUUID != "" {
			state.VmUuid = result.VMUUID
		}
	}
	if state.VmUuid != "" {
		vm, err := p.c.GetVM(ctx, state.VmUuid)
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "get created VM: %w", err)
		}
		if err := verifyVM(vm, pctx.GetRequestID(), biosUUID, clusterUUID); err != nil {
			return err
		}
		if err := verifyCreatedStorage(vm, pctx); err != nil {
			return err
		}
		pctx.SetMachineInfraID(vm.UUID)
		state.CreateTaskUuid = ""
		return nil
	}
	data, err := providerData(pctx)
	if err != nil {
		return err
	}
	if clusterUUID == "" {
		clusterUUID, err = p.c.ResolveCluster(ctx, client.Selector{Name: data.ClusterName, UUID: data.ClusterUUID})
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "resolve cluster: %w", err)
		}
		pctx.State.Metadata().Annotations().Set(clusterAnnotation, clusterUUID)
		// Persist placement too, before posting a VM creation request.
		return provision.NewRetryInterval(time.Millisecond)
	}
	// Recover after a lost response/state write. Never adopt a VM by name alone.
	vm, err := p.c.FindVM(ctx, pctx.GetRequestID(), biosUUID, clusterUUID)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "recover VM: %w", err)
	}
	if vm != nil {
		if err := verifyVM(vm, pctx.GetRequestID(), biosUUID, clusterUUID); err != nil {
			return err
		}
		state.VmUuid = vm.UUID
		if err := verifyCreatedStorage(vm, pctx); err != nil {
			return err
		}
		pctx.SetMachineInfraID(vm.UUID)
		state.CreateTaskUuid = ""
		return nil
	}
	if state.CreateTaskUuid != "" {
		return provision.NewRetryErrorf(vmRetry, "creation task completed; waiting for VM to appear in Prism Central")
	}
	if state.ImageUuid == "" {
		return fmt.Errorf("Talos image was not resolved")
	}
	var categoryUUIDs []string
	if snapshot, ok := pctx.State.Metadata().Annotations().Get(categoriesAnnotation); ok {
		if err := json.Unmarshal([]byte(snapshot), &categoryUUIDs); err != nil {
			return fmt.Errorf("invalid persisted category UUIDs: %w", err)
		}
	} else if len(data.Categories) > 0 {
		selectors := make([]client.CategorySelector, 0, len(data.Categories))
		for _, category := range data.Categories {
			selectors = append(selectors, category.selector())
		}
		categoryUUIDs, err = p.c.ResolveCategories(ctx, selectors)
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "resolve VM categories: %w", err)
		}
		if len(categoryUUIDs) == 0 {
			return fmt.Errorf("category resolution returned no UUIDs")
		}
		snapshot, err := json.Marshal(categoryUUIDs)
		if err != nil {
			return err
		}
		// Freeze category identities before POST so retries cannot select a
		// replacement category with different security policy membership.
		pctx.State.Metadata().Annotations().Set(categoriesAnnotation, string(snapshot))
		return provision.NewRetryInterval(time.Millisecond)
	}
	subnetUUID, err := p.c.ResolveSubnet(ctx, data.subnet(clusterUUID))
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "resolve primary subnet: %w", err)
	}
	containerUUID, frozenContainer := pctx.State.Metadata().Annotations().Get(containerAnnotation)
	if frozenContainer {
		if _, err := uuid.Parse(containerUUID); err != nil {
			return fmt.Errorf("invalid persisted storage container UUID")
		}
	} else if data.ContainerName != "" || data.ContainerUUID != "" {
		if _, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation); submitted {
			return fmt.Errorf("storage container snapshot missing after submission intent; operator review required")
		}
		containerUUID, err = p.c.ResolveContainer(ctx, client.Selector{Name: data.ContainerName, UUID: data.ContainerUUID}, clusterUUID)
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "resolve storage container: %w", err)
		}
		if _, err := uuid.Parse(containerUUID); err != nil {
			return fmt.Errorf("storage container resolution returned an invalid UUID")
		}
		// Persist the expected disk container before submission so recovery
		// cannot quietly accept an image clone placed elsewhere.
		pctx.State.Metadata().Annotations().Set(containerAnnotation, containerUUID)
		return provision.NewRetryInterval(time.Millisecond)
	}
	var additionalSubnets []string
	if err := freezeStoragePolicy(pctx, data); err != nil {
		return err
	}
	gpus, err := p.freezeRequestedGPUs(ctx, pctx, data)
	if err != nil {
		return err
	}
	for i, nic := range data.AdditionalNICs {
		id, err := p.c.ResolveSubnet(ctx, nic.subnet(clusterUUID))
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "resolve additional_nics[%d]: %w", i, err)
		}
		additionalSubnets = append(additionalSubnets, id)
	}
	var additionalDisks []int64
	for _, disk := range data.AdditionalDisks {
		additionalDisks = append(additionalDisks, disk.Size)
	}
	snapshot, _ := pctx.State.Metadata().Annotations().Get(guestTrustAnnotation)
	guestTrust, err := decodeGuestTrustSnapshot(snapshot)
	if err != nil {
		return err
	}
	joinConfig, err := joinConfigWithGuestTrust(pctx.ConnectionParams.JoinConfig, guestTrust)
	if err != nil {
		return err
	}
	description, err := creationDescription(pctx.State, data.Description)
	if err != nil {
		return err
	}
	bootMode, err := creationBootMode(pctx.State)
	if err != nil {
		return err
	}
	if _, ok := pctx.State.Metadata().Annotations().Get(createIntentAnnotation); !ok {
		// A lost POST response must not allow teardown to assume no VM was
		// submitted. Persist intent before any submission attempt.
		pctx.State.Metadata().Annotations().Set(createIntentAnnotation, uefiCreateIntent)
		return provision.NewRetryInterval(time.Millisecond)
	}
	contract, err := talosconfig.ParseContractFromVersion(pctx.GetTalosVersion())
	if err != nil {
		return fmt.Errorf("parse Talos version: %w", err)
	}
	patch, err := stdpatches.WithStaticHostname(contract, pctx.GetRequestID())
	if err != nil {
		return fmt.Errorf("build hostname patch: %w", err)
	}
	if err := pctx.CreateConfigPatch(ctx, "000-hostname-"+pctx.GetRequestID(), patch); err != nil {
		return provision.NewRetryErrorf(vmRetry, "create hostname patch: %w", err)
	}
	if guestTrust != "" {
		if err := pctx.CreateConfigPatch(ctx, "001-guest-trust-"+pctx.GetRequestID(), []byte(guestTrust)); err != nil {
			return provision.NewRetryErrorf(vmRetry, "create guest trust patch: %w", err)
		}
	}
	spec := client.VMCreateSpec{
		Name: pctx.GetRequestID(), Description: description, BIOSUUID: biosUUID,
		BootMode:    bootMode,
		RequestUUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("nutanix-v4/create/"+biosUUID)).String(),
		ClusterUUID: clusterUUID, ImageUUID: state.ImageUuid, ContainerUUID: containerUUID,
		SubnetUUID: subnetUUID, AdditionalSubnetUUIDs: additionalSubnets,
		CategoryUUIDs: categoryUUIDs,
		GPUs:          gpus,
		CPU:           data.CPU, Cores: data.Cores, MemoryMB: data.MemoryMB,
		BootDiskGB: data.DiskGB, AdditionalDiskGB: additionalDisks,
		UserDataB64: base64.StdEncoding.EncodeToString([]byte(joinConfig)),
	}
	taskUUID, err := p.c.CreateVM(ctx, spec)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "create VM: %w", err)
	}
	if taskUUID == "" {
		return fmt.Errorf("VM creation returned no task ID")
	}
	state.CreateTaskUuid = taskUUID
	logger.Info("VM creation submitted", zap.String("task_uuid", taskUUID), zap.String("bios_uuid", biosUUID))
	return provision.NewRetryInterval(taskRetry)
}

func (p *Provisioner) stepPowerOnVM(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	state := pctx.State.TypedSpec().Value
	if state.VmUuid == "" {
		return fmt.Errorf("VM creation has not completed")
	}
	if state.PowerOnTaskUuid != "" {
		result, err := p.c.WaitTask(ctx, state.PowerOnTaskUuid)
		if err != nil {
			if result.State == client.TaskFailed {
				return fmt.Errorf("power-on task failed: %w", err)
			}
			return provision.NewRetryErrorf(vmRetry, "power-on task: %w", err)
		}
		if result.State == client.TaskFailed {
			return fmt.Errorf("power-on task failed")
		}
		if result.State != client.TaskSucceeded {
			return provision.NewRetryInterval(taskRetry)
		}
	}
	vm, err := p.c.GetVM(ctx, state.VmUuid)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "get VM power state: %w", err)
	}
	biosUUID, _ := pctx.State.Metadata().Annotations().Get(biosAnnotation)
	clusterUUID, _ := pctx.State.Metadata().Annotations().Get(clusterAnnotation)
	if err := verifyVM(vm, pctx.GetRequestID(), biosUUID, clusterUUID); err != nil {
		return err
	}
	if vm.UUID != state.VmUuid {
		return fmt.Errorf("VM GET returned a different extId; refusing power-on")
	}
	if err := verifyVMStorage(vm, pctx); err != nil {
		return err
	}
	if err := verifyMigrationCompletion(vm, pctx); err != nil {
		return err
	}
	if err := verifyVMGPUs(vm, pctx); err != nil {
		return err
	}
	if snapshot, ok := pctx.State.Metadata().Annotations().Get(categoriesAnnotation); ok {
		var expected []string
		if err := json.Unmarshal([]byte(snapshot), &expected); err != nil {
			return fmt.Errorf("invalid persisted category UUIDs: %w", err)
		}
		for _, id := range expected {
			found := false
			for _, actual := range vm.CategoryUUIDs {
				if strings.EqualFold(actual, id) {
					found = true
					break
				}
			}
			if !found {
				return provision.NewRetryErrorf(vmRetry, "VM category %s is missing; refusing to complete provisioning or power on", id)
			}
		}
	}
	pctx.SetMachineUUID(biosUUID)
	pctx.SetMachineInfraID(vm.UUID)
	if vm.PowerState == "ON" {
		state.PowerOnTaskUuid = ""
		logger.Info("VM powered on", zap.String("vm_uuid", vm.UUID))
		return nil
	}
	if state.PowerOnTaskUuid != "" {
		return provision.NewRetryInterval(taskRetry)
	}
	if vm.PowerState != "OFF" {
		return provision.NewRetryErrorf(vmRetry, "VM power state is not OFF; refusing power-on")
	}
	taskUUID, err := p.c.PowerOnVM(ctx, vm.UUID)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "power on VM: %w", err)
	}
	if taskUUID == "" {
		return fmt.Errorf("power-on returned no task ID")
	}
	state.PowerOnTaskUuid = taskUUID
	return provision.NewRetryInterval(taskRetry)
}

// waitTask blocks only during teardown: Omni does not persist state changes
// made by Deprovision. Stable mutation request IDs allow restart-safe retries.
func (p *Provisioner) waitTask(ctx context.Context, id string) (client.TaskResult, error) {
	if id == "" {
		return client.TaskResult{}, fmt.Errorf("operation returned no task ID")
	}
	for {
		result, err := p.c.WaitTask(ctx, id)
		if err != nil {
			return result, err
		}
		if result.State == client.TaskFailed {
			return result, fmt.Errorf("task %s failed", id)
		}
		if result.State == client.TaskSucceeded {
			return result, nil
		}
		timer := time.NewTimer(taskRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return client.TaskResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// Deprovision retains Omni's finalizer until deletion succeeds and GET returns 404.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, request *infra.MachineRequest) (err error) {
	defer func() {
		if err == nil {
			p.migrationMu.Lock()
			defer p.migrationMu.Unlock()
			if p.migrationOwner != nil && p.migrationOwner.request == request.Metadata().ID() {
				p.migrationOwner = nil
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	state := machine.TypedSpec().Value
	biosUUID, _ := machine.Metadata().Annotations().Get(biosAnnotation)
	clusterUUID, _ := machine.Metadata().Annotations().Get(clusterAnnotation)
	vmUUID := state.VmUuid
	if state.CreateTaskUuid != "" {
		if biosUUID == "" || clusterUUID == "" {
			return fmt.Errorf("creation task lacks frozen VM identity/cluster; retaining finalizer")
		}
		result, err := p.waitTask(ctx, state.CreateTaskUuid)
		if err != nil {
			// A failed creation can still have left a VM behind. Recover only
			// after the task is terminal, never while creation is in flight.
			if result.State != client.TaskFailed {
				return fmt.Errorf("wait for creation before teardown: %w", err)
			}
		}
		if result.VMUUID != "" {
			id, err := uuid.Parse(result.VMUUID)
			if err != nil || id == uuid.Nil {
				return fmt.Errorf("creation task has an invalid VM reference; retaining finalizer")
			}
			if vmUUID != "" && !strings.EqualFold(vmUUID, id.String()) {
				return fmt.Errorf("creation task conflicts with persisted VM identity; retaining finalizer")
			}
			vmUUID = id.String() // Terminal FAILED/CANCELED also permits exact absence GET.
		}
	}
	var vm *client.VM
	if vmUUID == "" {
		if biosUUID == "" || clusterUUID == "" {
			if _, submitted := machine.Metadata().Annotations().Get(createIntentAnnotation); submitted {
				return fmt.Errorf("VM submission outcome lacks frozen identity/cluster; retaining finalizer")
			}
			return nil
		}
		vm, err = p.c.FindVM(ctx, request.Metadata().ID(), biosUUID, clusterUUID)
	} else {
		vm, err = p.c.GetVM(ctx, vmUUID)
	}
	if err != nil {
		if vmUUID != "" && p.c.IsNotFound(err) {
			return nil
		}
		return err
	}
	if vm == nil {
		if vmUUID != "" {
			return fmt.Errorf("VM GET returned no identity without HTTP 404; retaining finalizer")
		}
		if state.CreateTaskUuid != "" {
			return provision.NewRetryErrorf(vmRetry, "creation task is terminal but VM identity has not been recovered; retaining finalizer")
		}
		if _, submitted := machine.Metadata().Annotations().Get(createIntentAnnotation); submitted && state.CreateTaskUuid == "" {
			return provision.NewRetryErrorf(vmRetry, "VM submission outcome is unknown; retaining finalizer until recovery (verify the BIOS UUID and request ID in Prism Central)")
		}
		return nil
	}
	if vmUUID != "" && !strings.EqualFold(vm.UUID, vmUUID) {
		return fmt.Errorf("VM GET returned a different extId; retaining finalizer")
	}
	if err := verifyVM(vm, request.Metadata().ID(), biosUUID, clusterUUID); err != nil {
		return err
	}
	vm, err = p.waitMigrationBeforeTeardown(ctx, machine, request, vm)
	if err != nil {
		return err
	}
	if state.PowerOnTaskUuid != "" {
		result, err := p.waitTask(ctx, state.PowerOnTaskUuid)
		if err != nil && result.State != client.TaskFailed {
			return fmt.Errorf("wait for power-on before teardown: %w", err)
		}
		vm, err = p.c.GetVM(ctx, vm.UUID)
		if err != nil {
			if p.c.IsNotFound(err) {
				return nil
			}
			return err
		}
		if err := verifyVM(vm, request.Metadata().ID(), biosUUID, clusterUUID); err != nil {
			return err
		}
	}
	if vm.PowerState != "OFF" {
		task, err := p.c.PowerOffVM(ctx, vm.UUID)
		if err != nil {
			if p.c.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("power off VM: %w", err)
		}
		if _, err := p.waitTask(ctx, task); err != nil {
			return fmt.Errorf("wait for power-off: %w", err)
		}
	}
	task, err := p.c.DeleteVM(ctx, vm.UUID)
	if err != nil {
		if p.c.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete VM: %w", err)
	}
	if _, err := p.waitTask(ctx, task); err != nil {
		return fmt.Errorf("wait for deletion: %w", err)
	}
	if _, err := p.c.GetVM(ctx, vm.UUID); p.c.IsNotFound(err) {
		logger.Info("VM deletion confirmed", zap.String("vm_uuid", vm.UUID))
		return nil
	} else if err != nil {
		return err
	}
	return provision.NewRetryErrorf(taskRetry, "deletion task completed; waiting for VM to disappear")
}
