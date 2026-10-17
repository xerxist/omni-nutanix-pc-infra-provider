// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/google/uuid"
	omnispecs "github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const (
	storagePolicyAnnotation         = "nutanix.infra.sidero.dev/storage-policy"
	migrationIntentAnnotation       = "nutanix.infra.sidero.dev/disk-migration-intent"
	migrationTaskAnnotation         = "nutanix.infra.sidero.dev/disk-migration-task"
	migrationCompleteAnnotation     = "nutanix.infra.sidero.dev/disk-migration-complete"
	storagePolicyMigrate            = "migrate-before-boot-v1"
	storagePolicyMigrateConfigDrive = "migrate-before-boot-with-config-drive-v2"
	storagePolicyVerify             = "verify-only-v1"
	maxMigrationIntentBytes         = 16 * 1024
)

// These annotations belong to one new creation identity, not to a mutable
// MachineClass. Never upgrade previously submitted state to the new default.
func freezeStoragePolicy(pctx provision.Context[*resources.Machine], data *Data) error {
	if _, exists := pctx.State.Metadata().Annotations().Get(storagePolicyAnnotation); exists {
		_, err := storageMigrationEnabled(pctx)
		return err
	}
	if _, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation); submitted {
		if data.LegacyMigrateDisks {
			return fmt.Errorf("storage policy snapshot missing after creation intent; operator review required")
		}
		return nil // Old verify-only requests retain their original behavior.
	}
	if _, frozen := pctx.State.Metadata().Annotations().Get(containerAnnotation); !frozen {
		return fmt.Errorf("storage container snapshot required before freezing default migration policy")
	}
	pctx.State.Metadata().Annotations().Set(storagePolicyAnnotation, storagePolicyMigrateConfigDrive)
	return provision.NewRetryInterval(time.Millisecond)
}

func storageMigrationEnabled(pctx provision.Context[*resources.Machine]) (bool, error) {
	policy, present := pctx.State.Metadata().Annotations().Get(storagePolicyAnnotation)
	if policy != storagePolicyMigrate && policy != storagePolicyMigrateConfigDrive {
		for _, key := range []string{migrationIntentAnnotation, migrationTaskAnnotation, migrationCompleteAnnotation, migrationRetryAnnotation, migrationSubmissionAnnotation} {
			if _, exists := pctx.State.Metadata().Annotations().Get(key); exists {
				return false, fmt.Errorf("persisted migration state lacks its frozen policy; operator review required")
			}
		}
	}
	if !present {
		data, err := providerData(pctx)
		if err != nil {
			return false, err
		}
		_, submitted := pctx.State.Metadata().Annotations().Get(createIntentAnnotation)
		if !submitted || data.LegacyMigrateDisks {
			return false, fmt.Errorf("storage policy snapshot missing; operator review required")
		}
		return false, nil
	}
	switch policy {
	case storagePolicyMigrate, storagePolicyMigrateConfigDrive:
		return true, nil
	case storagePolicyVerify:
		return false, nil
	default:
		return false, fmt.Errorf("invalid persisted storage policy")
	}
}

func verifyCreatedStorage(vm *client.VM, pctx provision.Context[*resources.Machine]) error {
	if err := verifyVMGPUs(vm, pctx); err != nil {
		return err
	}
	enabled, err := storageMigrationEnabled(pctx)
	if err != nil {
		return err
	}
	if !enabled {
		return verifyVMStorage(vm, pctx)
	}
	if vm == nil || vm.PowerState != "OFF" {
		return fmt.Errorf("migration-enabled creation requires an OFF VM; refusing live correction")
	}
	return verifyMigrationInventory(vm, pctx)
}

// Validate everything except destination placement, which is the dedicated
// step's job. Missing migration status is unknown, never an implicit false.
func verifyMigrationInventory(vm *client.VM, pctx provision.Context[*resources.Machine]) error {
	if err := verifyVMGPUs(vm, pctx); err != nil {
		return err
	}
	if _, frozen := pctx.State.Metadata().Annotations().Get(containerAnnotation); !frozen {
		return fmt.Errorf("storage container snapshot missing; operator review required")
	}
	if err := verifyStorage(vm, pctx, false); err != nil {
		return err
	}
	if configDriveMigrationEnabled(pctx.State) {
		if err := verifyConfigDrive(vm, pctx.State, false); err != nil {
			return err
		}
	}
	return verifyMigrationMetadata(vm)
}

func configDriveMigrationEnabled(machine *resources.Machine) bool {
	policy, _ := machine.Metadata().Annotations().Get(storagePolicyAnnotation)
	return policy == storagePolicyMigrateConfigDrive
}

// New v2 requests must have exactly the generated, attached config-drive. Do not
// migrate arbitrary ISO media or silently accept missing/unknown device metadata.
func verifyConfigDrive(vm *client.VM, machine *resources.Machine, requirePlacement bool) error {
	if vm == nil || len(vm.CDROMs) != 1 {
		return fmt.Errorf("expected exactly one guest-customization config-drive CD-ROM")
	}
	cd := vm.CDROMs[0]
	if cd.ISOType != "GUEST_CUSTOMIZATION" || (cd.Bus != "IDE" && cd.Bus != "SATA") || cd.Index < 0 ||
		cd.BackingType != "vmm.v4.ahv.config.VmDisk" || cd.SizeBytes <= 0 {
		return fmt.Errorf("config-drive CD-ROM type/address/backing is missing or unexpected")
	}
	ids, devices := map[string]bool{}, map[string]bool{}
	for _, disk := range vm.Disks {
		id, err := uuid.Parse(disk.UUID)
		if err != nil || id == uuid.Nil {
			return fmt.Errorf("invalid backing disk UUID alongside config-drive")
		}
		device, err := uuid.Parse(disk.DeviceUUID)
		if err != nil || device == uuid.Nil {
			return fmt.Errorf("invalid device UUID alongside config-drive")
		}
		ids[id.String()], devices[device.String()] = true, true
	}
	if err := verifyDiskMigrationMetadata(cd.VMDisk, ids, devices); err != nil {
		return fmt.Errorf("config-drive CD-ROM migration metadata: %w", err)
	}
	expected, _ := machine.Metadata().Annotations().Get(containerAnnotation)
	if requirePlacement && !strings.EqualFold(cd.ContainerUUID, expected) {
		return fmt.Errorf("config-drive CD-ROM is not in requested storage container; refusing power-on/completion")
	}
	return nil
}

func verifyDiskMigrationMetadata(disk client.VMDisk, ids, devices map[string]bool) error {
	id, err := uuid.Parse(disk.UUID)
	if err != nil || id == uuid.Nil || ids[id.String()] {
		return fmt.Errorf("migration requires unique nonzero backing disk UUIDs")
	}
	device, err := uuid.Parse(disk.DeviceUUID)
	if err != nil || device == uuid.Nil || devices[device.String()] {
		return fmt.Errorf("migration requires unique nonzero device UUIDs")
	}
	container, err := uuid.Parse(disk.ContainerUUID)
	if err != nil || container == uuid.Nil {
		return fmt.Errorf("migration requires existing nonzero source container UUIDs")
	}
	if !disk.MigrationKnown || disk.MigrationInFlight {
		return fmt.Errorf("migration status missing or active; refusing migration/power-on")
	}
	ids[id.String()], devices[device.String()] = true, true
	return nil
}

func verifyMigrationMetadata(vm *client.VM) error {
	if vm == nil || len(vm.Disks) == 0 {
		return fmt.Errorf("VM disk migration metadata is missing")
	}
	if !vm.CrossClusterMigrationKnown || vm.CrossClusterMigrationInFlight {
		return fmt.Errorf("VM cross-cluster migration status is missing or active; refusing disk migration/power-on")
	}
	ids, devices := map[string]bool{}, map[string]bool{}
	for _, disk := range vm.Disks {
		if err := verifyDiskMigrationMetadata(disk, ids, devices); err != nil {
			return err
		}
	}
	return nil
}

type migrationDisk struct {
	UUID            string `json:"uuid"`
	DeviceUUID      string `json:"device_uuid"`
	Index           int    `json:"index"`
	SizeBytes       int64  `json:"size_bytes"`
	SourceContainer string `json:"source_container"`
}

type migrationIntent struct {
	VMUUID        string           `json:"vm_uuid"`
	ContainerUUID string           `json:"container_uuid"`
	RequestUUID   string           `json:"request_uuid"`
	ETag          string           `json:"etag"`
	Disks         []migrationDisk  `json:"disks"`
	CDROMs        []migrationCDROM `json:"cd_roms,omitempty"`
}

type migrationCDROM struct {
	migrationDisk
	Bus     string `json:"bus"`
	ISOType string `json:"iso_type"`
}

func migrationRequestID(bios, vm, container string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("nutanix-v4/migrate-before-boot/"+bios+"/"+vm+"/"+container)).String()
}

func migrationRequestIDForPolicy(policy, bios, vm, container string) string {
	if policy == storagePolicyMigrateConfigDrive {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte("nutanix-v4/migrate-before-boot-with-config-drive-v2/"+bios+"/"+vm+"/"+container)).String()
	}
	return migrationRequestID(bios, vm, container)
}

func (intent migrationIntent) spec() client.DiskMigrationSpec {
	spec := client.DiskMigrationSpec{VMUUID: intent.VMUUID, ContainerUUID: intent.ContainerUUID, RequestUUID: intent.RequestUUID, ETag: intent.ETag}
	for _, disk := range intent.Disks {
		if !strings.EqualFold(disk.SourceContainer, intent.ContainerUUID) {
			spec.DiskUUIDs = append(spec.DiskUUIDs, disk.UUID)
		}
	}
	for _, cd := range intent.CDROMs {
		if !strings.EqualFold(cd.SourceContainer, intent.ContainerUUID) {
			spec.DiskUUIDs = append(spec.DiskUUIDs, cd.UUID)
		}
	}
	return spec
}

func decodeMigrationIntent(machine *resources.Machine) (*migrationIntent, error) {
	snapshot, present := machine.Metadata().Annotations().Get(migrationIntentAnnotation)
	if !present {
		for _, key := range []string{migrationTaskAnnotation, migrationCompleteAnnotation, migrationRetryAnnotation, migrationSubmissionAnnotation} {
			if _, exists := machine.Metadata().Annotations().Get(key); exists {
				return nil, fmt.Errorf("migration state lacks persisted intent; operator review required")
			}
		}
		return nil, nil
	}
	var intent migrationIntent
	if len(snapshot) > maxMigrationIntentBytes || json.Unmarshal([]byte(snapshot), &intent) != nil {
		return nil, fmt.Errorf("invalid persisted disk migration intent")
	}
	policy, _ := machine.Metadata().Annotations().Get(storagePolicyAnnotation)
	container, _ := machine.Metadata().Annotations().Get(containerAnnotation)
	bios, _ := machine.Metadata().Annotations().Get(biosAnnotation)
	if (policy != storagePolicyMigrate && policy != storagePolicyMigrateConfigDrive) || container == "" || intent.ContainerUUID != container ||
		intent.VMUUID != machine.TypedSpec().Value.VmUuid || intent.ETag == "" ||
		intent.RequestUUID != migrationRequestIDForPolicy(policy, bios, intent.VMUUID, container) || len(intent.Disks) == 0 {
		return nil, fmt.Errorf("disk migration intent conflicts with frozen VM identity/placement")
	}
	for _, id := range []string{intent.VMUUID, intent.ContainerUUID, intent.RequestUUID} {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
			return nil, fmt.Errorf("invalid disk migration intent UUID")
		}
	}
	ids, devices := map[string]bool{}, map[string]bool{}
	for index, disk := range intent.Disks {
		id, err := uuid.Parse(disk.UUID)
		if err != nil || id == uuid.Nil || ids[id.String()] {
			return nil, fmt.Errorf("invalid persisted disk migration inventory")
		}
		device, err := uuid.Parse(disk.DeviceUUID)
		if err != nil || device == uuid.Nil || devices[device.String()] || disk.Index != index || disk.SizeBytes <= 0 {
			return nil, fmt.Errorf("invalid persisted disk migration inventory")
		}
		if parsed, err := uuid.Parse(disk.SourceContainer); err != nil || parsed == uuid.Nil {
			return nil, fmt.Errorf("invalid persisted disk migration source container")
		}
		ids[id.String()], devices[device.String()] = true, true
	}
	if configDriveMigrationEnabled(machine) {
		if len(intent.CDROMs) != 1 {
			return nil, fmt.Errorf("config-drive migration intent must freeze exactly one CD-ROM")
		}
		cd := intent.CDROMs[0]
		if cd.Index < 0 || cd.SizeBytes <= 0 || (cd.Bus != "IDE" && cd.Bus != "SATA") || cd.ISOType != "GUEST_CUSTOMIZATION" {
			return nil, fmt.Errorf("invalid persisted config-drive layout")
		}
		if err := verifyDiskMigrationMetadata(client.VMDisk{UUID: cd.UUID, DeviceUUID: cd.DeviceUUID, ContainerUUID: cd.SourceContainer, MigrationKnown: true}, ids, devices); err != nil {
			return nil, fmt.Errorf("invalid persisted config-drive inventory: %w", err)
		}
	} else if len(intent.CDROMs) != 0 {
		return nil, fmt.Errorf("config-drive inventory cannot be backfilled into a legacy migration policy")
	}
	if len(intent.spec().DiskUUIDs) == 0 || len(intent.spec().DiskUUIDs) > 1034 {
		return nil, fmt.Errorf("persisted migration intent has no valid migration plan")
	}
	spec, err := activeMigrationSpec(machine, &intent)
	if err != nil {
		return nil, err
	}
	if submitted, exists := machine.Metadata().Annotations().Get(migrationSubmissionAnnotation); exists && submitted != "queued" && submitted != spec.RequestUUID {
		return nil, fmt.Errorf("invalid persisted migration submission marker")
	}
	if complete, exists := machine.Metadata().Annotations().Get(migrationCompleteAnnotation); exists {
		task, _ := machine.Metadata().Annotations().Get(migrationTaskAnnotation)
		submitted, marked := machine.Metadata().Annotations().Get(migrationSubmissionAnnotation)
		if complete != spec.RequestUUID || task == "" || (marked && submitted == "queued") {
			return nil, fmt.Errorf("invalid persisted disk migration completion")
		}
	}
	return &intent, nil
}

func (intent *migrationIntent) verifyDisks(vm *client.VM, requireSource bool) error {
	if vm.UUID != intent.VMUUID || len(vm.Disks) != len(intent.Disks) {
		return fmt.Errorf("VM disk inventory changed after migration intent; operator review required")
	}
	seen := map[int]bool{}
	for _, disk := range vm.Disks {
		if disk.Index < 0 || disk.Index >= len(intent.Disks) || seen[disk.Index] {
			return fmt.Errorf("VM disk address changed after migration intent")
		}
		expected := intent.Disks[disk.Index]
		if disk.Bus != "SCSI" || disk.BackingType != "vmm.v4.ahv.config.VmDisk" ||
			!strings.EqualFold(disk.UUID, expected.UUID) || !strings.EqualFold(disk.DeviceUUID, expected.DeviceUUID) || disk.SizeBytes != expected.SizeBytes ||
			(requireSource && !strings.EqualFold(disk.ContainerUUID, expected.SourceContainer)) {
			return fmt.Errorf("VM disk identity/layout changed after migration intent; operator review required")
		}
		seen[disk.Index] = true
	}
	if len(intent.CDROMs) > 0 {
		if len(vm.CDROMs) != len(intent.CDROMs) {
			return fmt.Errorf("config-drive inventory changed after migration intent")
		}
		cd, expected := vm.CDROMs[0], intent.CDROMs[0]
		if cd.ISOType != expected.ISOType || cd.Bus != expected.Bus || cd.Index != expected.Index || cd.BackingType != "vmm.v4.ahv.config.VmDisk" ||
			!strings.EqualFold(cd.UUID, expected.UUID) || !strings.EqualFold(cd.DeviceUUID, expected.DeviceUUID) || cd.SizeBytes != expected.SizeBytes ||
			(requireSource && !strings.EqualFold(cd.ContainerUUID, expected.SourceContainer)) {
			return fmt.Errorf("config-drive identity/layout changed after migration intent")
		}
	}
	return nil
}

func verifyMigrationCompletion(vm *client.VM, pctx provision.Context[*resources.Machine]) error {
	enabled, err := storageMigrationEnabled(pctx)
	if err != nil || !enabled {
		return err
	}
	if err := verifyMigrationInventory(vm, pctx); err != nil {
		return err
	}
	intent, err := decodeMigrationIntent(pctx.State)
	if err != nil || intent == nil {
		return err // No migration was needed when all disks were already placed.
	}
	complete, _ := pctx.State.Metadata().Annotations().Get(migrationCompleteAnnotation)
	task, _ := pctx.State.Metadata().Annotations().Get(migrationTaskAnnotation)
	spec, err := activeMigrationSpec(pctx.State, intent)
	if err != nil {
		return err
	}
	if complete != spec.RequestUUID || task == "" {
		return fmt.Errorf("disk migration has not been verified complete; refusing power-on")
	}
	return intent.verifyDisks(vm, false)
}

func (p *Provisioner) stepPlaceVMDisks(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) (err error) {
	defer func() {
		var retry *controller.RequeueError
		if errors.As(err, &retry) && retry.Err() != nil && pctx.MachineRequestStatus.TypedSpec().Value.Stage != omnispecs.MachineRequestStatusSpec_FAILED {
			pctx.MachineRequestStatus.TypedSpec().Value.Status = "Waiting for storage placement: " + retry.Err().Error()
		}
	}()
	// Serialize admission and POST within one active process. Persisted scope and
	// submission markers carry the slot across reconciliations/restarts.
	p.migrationMu.Lock()
	defer p.migrationMu.Unlock()
	enabled, err := storageMigrationEnabled(pctx)
	if err != nil {
		return err
	}
	state := pctx.State.TypedSpec().Value
	if state.VmUuid == "" || state.CreateTaskUuid != "" {
		return fmt.Errorf("VM creation has not completed before storage placement")
	}
	vm, err := p.c.GetVM(ctx, state.VmUuid)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "get VM storage placement: %w", err)
	}
	bios, _ := pctx.State.Metadata().Annotations().Get(biosAnnotation)
	cluster, _ := pctx.State.Metadata().Annotations().Get(clusterAnnotation)
	if err := verifyVM(vm, pctx.GetRequestID(), bios, cluster); err != nil {
		return err
	}
	if vm.UUID != state.VmUuid {
		return fmt.Errorf("VM GET returned a different extId; refusing storage placement")
	}
	if !enabled {
		return verifyVMStorage(vm, pctx)
	}
	intent, err := decodeMigrationIntent(pctx.State)
	if err != nil {
		return err
	}
	if intent != nil {
		complete, _ := pctx.State.Metadata().Annotations().Get(migrationCompleteAnnotation)
		if complete != "" {
			if err := verifyVMStorage(vm, pctx); err != nil {
				return err
			}
			return verifyMigrationCompletion(vm, pctx)
		}
	}
	if vm.PowerState != "OFF" || state.PowerOnTaskUuid != "" {
		return fmt.Errorf("pre-boot disk placement requires an OFF VM and no power-on task; refusing live migration")
	}
	if intent == nil {
		if err := verifyMigrationInventory(vm, pctx); err != nil {
			return err
		}
		if verifyVMStorage(vm, pctx) == nil {
			return nil // Already correctly placed; no mutation or intent needed.
		}
		if vm.ETag == "" {
			return fmt.Errorf("VM has no ETag; refusing disk migration")
		}
		container, _ := pctx.State.Metadata().Annotations().Get(containerAnnotation)
		policy, _ := pctx.State.Metadata().Annotations().Get(storagePolicyAnnotation)
		intent = &migrationIntent{VMUUID: vm.UUID, ContainerUUID: container, ETag: vm.ETag, RequestUUID: migrationRequestIDForPolicy(policy, bios, vm.UUID, container)}
		for _, disk := range vm.Disks {
			intent.Disks = append(intent.Disks, migrationDisk{UUID: disk.UUID, DeviceUUID: disk.DeviceUUID, Index: disk.Index, SizeBytes: disk.SizeBytes, SourceContainer: disk.ContainerUUID})
		}
		sort.Slice(intent.Disks, func(i, j int) bool { return intent.Disks[i].Index < intent.Disks[j].Index })
		if configDriveMigrationEnabled(pctx.State) {
			cd := vm.CDROMs[0]
			intent.CDROMs = []migrationCDROM{{migrationDisk: migrationDisk{UUID: cd.UUID, DeviceUUID: cd.DeviceUUID, Index: cd.Index, SizeBytes: cd.SizeBytes, SourceContainer: cd.ContainerUUID}, Bus: cd.Bus, ISOType: cd.ISOType}}
		}
		snapshot, err := json.Marshal(intent)
		if err != nil || len(snapshot) > maxMigrationIntentBytes {
			return fmt.Errorf("disk migration intent exceeds Omni's 16KiB annotation limit")
		}
		pctx.State.Metadata().Annotations().Set(migrationIntentAnnotation, string(snapshot))
		if p.migrationState != nil {
			pctx.State.Metadata().Annotations().Set(migrationSubmissionAnnotation, "queued")
		}
		return provision.NewRetryInterval(time.Millisecond) // Persist before POST.
	}
	task, _ := pctx.State.Metadata().Annotations().Get(migrationTaskAnnotation)
	if task == "" {
		spec, err := activeMigrationSpec(pctx.State, intent)
		if err != nil {
			return err
		}
		retries, err := decodeMigrationRetries(pctx.State, intent)
		if err != nil {
			return err
		}
		if retries != nil && p.migrationTime().Before(retries.NotBefore) {
			return provision.NewRetryErrorf(retries.NotBefore.Sub(p.migrationTime()), "Prism migration limit retry waiting for backoff")
		}
		if retries != nil {
			last := retries.Rejected[len(retries.Rejected)-1]
			result, _ := p.c.WaitTask(ctx, last.TaskUUID)
			if result.State != client.TaskFailed || !result.MigrationLimitRejected || result.VMUUID != vm.UUID {
				return fmt.Errorf("persisted migration rejection not proven; refusing new attempt")
			}
		}
		// A changed ETag may indicate an accepted operation whose response was
		// lost. Do not replay against a new version or infer task success from
		// placement: fail closed until the original task can be identified.
		if vm.ETag != spec.ETag {
			return fmt.Errorf("disk migration submission outcome unknown (VM ETag changed); operator review required")
		}
		if err := verifyMigrationInventory(vm, pctx); err != nil {
			return err
		}
		if err := intent.verifyDisks(vm, true); err != nil {
			return err
		}
		if err := p.admitMigration(ctx, pctx); err != nil {
			return err
		}
		if p.migrationState != nil {
			submitted, _ := pctx.State.Metadata().Annotations().Get(migrationSubmissionAnnotation)
			if submitted == "queued" {
				pctx.State.Metadata().Annotations().Set(migrationSubmissionAnnotation, spec.RequestUUID)
				return provision.NewRetryInterval(time.Millisecond) // Reserve durable slot before POST.
			}
		}
		if _, marked := pctx.State.Metadata().Annotations().Get(migrationSubmissionAnnotation); marked {
			pctx.State.Metadata().Annotations().Set(migrationSubmissionAnnotation, spec.RequestUUID)
		}
		p.migrationOwner = &migrationQueueOwner{request: pctx.GetRequestID(), vm: vm.UUID}
		task, err = p.c.MigrateVMDisks(ctx, spec)
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "submit disk migration (same persisted operation): %w", err)
		}
		if task == "" {
			return fmt.Errorf("disk migration returned no task ID; operator review required")
		}
		pctx.State.Metadata().Annotations().Set(migrationTaskAnnotation, task)
		p.migrationOwner.task = task
		logger.Info("pre-boot disk migration submitted", zap.String("vm_uuid", vm.UUID), zap.String("task_uuid", task))
		return provision.NewRetryInterval(taskRetry)
	}
	result, err := p.c.WaitTask(ctx, task)
	if result.State == client.TaskFailed {
		err := p.prepareMigrationLimitRetry(ctx, logger, pctx, intent, task, result)
		var retry *controller.RequeueError
		if !errors.As(err, &retry) {
			// SDK v1.7.1 returns before publishing FAILED for ordinary step errors.
			// Use its persisted requeue path for terminal migration failures only.
			pctx.MachineRequestStatus.TypedSpec().Value.Stage = omnispecs.MachineRequestStatusSpec_FAILED
			pctx.MachineRequestStatus.TypedSpec().Value.Error = err.Error()
			return provision.NewRetryError(err, time.Minute)
		}
		return err
	}
	if err != nil {
		return provision.NewRetryErrorf(taskRetry, "wait disk migration: %w", err)
	}
	if result.State != client.TaskSucceeded {
		return provision.NewRetryInterval(taskRetry)
	}
	if result.VMUUID != "" && result.VMUUID != vm.UUID {
		return fmt.Errorf("disk migration task affected a foreign VM")
	}
	// Fresh VM GET after task success, not the possibly pre-completion metadata.
	vm, err = p.c.GetVM(ctx, state.VmUuid)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "verify migrated disks: %w", err)
	}
	if err := verifyVM(vm, pctx.GetRequestID(), bios, cluster); err != nil {
		return err
	}
	if vm.UUID != state.VmUuid {
		return fmt.Errorf("VM GET returned a different extId after migration")
	}
	if vm.PowerState != "OFF" {
		return fmt.Errorf("VM is no longer OFF after disk migration; operator review required")
	}
	if err := verifyMigrationInventory(vm, pctx); err != nil {
		return err
	}
	if err := verifyVMStorage(vm, pctx); err != nil {
		return err
	}
	if err := intent.verifyDisks(vm, false); err != nil {
		return err
	}
	spec, err := activeMigrationSpec(pctx.State, intent)
	if err != nil {
		return err
	}
	pctx.State.Metadata().Annotations().Set(migrationCompleteAnnotation, spec.RequestUUID)
	if p.migrationOwner != nil && p.migrationOwner.request == pctx.GetRequestID() {
		p.migrationOwner = nil
	}
	return provision.NewRetryInterval(time.Millisecond) // Persist verification before power-on.
}

func (p *Provisioner) waitMigrationBeforeTeardown(ctx context.Context, machine *resources.Machine, request *infra.MachineRequest, vm *client.VM) (*client.VM, error) {
	intent, err := decodeMigrationIntent(machine)
	if err != nil {
		return nil, err
	}
	if intent != nil {
		spec, err := activeMigrationSpec(machine, intent)
		if err != nil {
			return nil, err
		}
		complete, _ := machine.Metadata().Annotations().Get(migrationCompleteAnnotation)
		if complete != "" && complete != spec.RequestUUID {
			return nil, fmt.Errorf("invalid migration completion snapshot; retaining finalizer")
		}
		if complete == "" {
			task, _ := machine.Metadata().Annotations().Get(migrationTaskAnnotation)
			if task == "" {
				// Only an explicitly persisted queued attempt proves no POST yet.
				// Retry history is independently rechecked; never submit on teardown.
				queued, _ := machine.Metadata().Annotations().Get(migrationSubmissionAnnotation)
				if queued != "queued" {
					return nil, fmt.Errorf("disk migration submission outcome unknown; retaining finalizer for operator review")
				}
				retries, err := decodeMigrationRetries(machine, intent)
				if err != nil {
					return nil, err
				}
				if retries != nil {
					last := retries.Rejected[len(retries.Rejected)-1]
					result, _ := p.c.WaitTask(ctx, last.TaskUUID)
					if result.State != client.TaskFailed || !result.MigrationLimitRejected || result.VMUUID != vm.UUID {
						return nil, fmt.Errorf("queued migration rejection not proven; retaining finalizer")
					}
				}
				if vm.PowerState != "OFF" || vm.ETag != spec.ETag || intent.verifyDisks(vm, true) != nil {
					return nil, fmt.Errorf("queued migration VM changed; retaining finalizer")
				}
			} else {
				result, err := p.waitTask(ctx, task)
				if err != nil && result.State != client.TaskFailed {
					return nil, fmt.Errorf("wait for migration before teardown: %w", err)
				}
				if result.VMUUID != "" && result.VMUUID != vm.UUID {
					return nil, fmt.Errorf("migration task affected a foreign VM; retaining finalizer")
				}
			}
			vm, err = p.c.GetVM(ctx, vm.UUID)
			if err != nil {
				return nil, err
			}
			bios, _ := machine.Metadata().Annotations().Get(biosAnnotation)
			cluster, _ := machine.Metadata().Annotations().Get(clusterAnnotation)
			if err := verifyVM(vm, request.Metadata().ID(), bios, cluster); err != nil {
				return nil, err
			}
			if task == "" && (vm.PowerState != "OFF" || vm.ETag != spec.ETag || intent.verifyDisks(vm, true) != nil) {
				return nil, fmt.Errorf("queued migration VM changed on fresh read; retaining finalizer")
			}
		}
		if err := verifyMigrationMetadata(vm); err != nil {
			return nil, fmt.Errorf("migration state not idle before teardown: %w", err)
		}
		if configDriveMigrationEnabled(machine) {
			if err := verifyConfigDrive(vm, machine, false); err != nil {
				return nil, fmt.Errorf("config-drive state unknown/active before teardown: %w", err)
			}
			if err := intent.verifyDisks(vm, false); err != nil {
				return nil, err
			}
		}
	} else {
		// Preserve older identity-verified teardown, but never delete during an
		// explicitly reported external migration.
		if vm.CrossClusterMigrationInFlight {
			return nil, fmt.Errorf("cross-cluster migration active; retaining finalizer")
		}
		for _, disk := range vm.Disks {
			if disk.MigrationInFlight {
				return nil, fmt.Errorf("disk migration active; retaining finalizer")
			}
		}
	}
	for _, cd := range vm.CDROMs {
		if cd.MigrationInFlight {
			return nil, fmt.Errorf("CD-ROM migration active; retaining finalizer")
		}
	}
	return vm, nil
}
