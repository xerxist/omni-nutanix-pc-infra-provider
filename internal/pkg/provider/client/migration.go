// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// GA v4.0 DiskMigrationParams uses MigrationPlans -> ADSFDiskMigrationPlan ->
// MigrateDiskReference.diskExtId. Explicit disk references avoid migrating an
// unexpected disk added outside the frozen inventory.
// This also carries the exact generated CD-ROM backing disk for v2 requests.
// PC/AOS eligibility must be validated live; never drop it after rejection.
type diskMigrationBody struct {
	ObjectType    string `json:"$objectType"`
	Discriminator string `json:"$migrateDisksItemDiscriminator"`
	MigrateDisks  struct {
		ObjectType string                  `json:"$objectType"`
		Plans      []adsfDiskMigrationPlan `json:"plans"`
	} `json:"migrateDisks"`
}

type adsfDiskMigrationPlan struct {
	ObjectType string                 `json:"$objectType"`
	Container  reference              `json:"storageContainer"`
	Disks      []migrateDiskReference `json:"vmDisks"`
}

type migrateDiskReference struct {
	ObjectType string `json:"$objectType"`
	UUID       string `json:"diskExtId"`
}

func (c *v4Client) MigrateVMDisks(ctx context.Context, spec DiskMigrationSpec) (string, error) {
	for _, id := range []string{spec.VMUUID, spec.ContainerUUID, spec.RequestUUID} {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
			return "", fmt.Errorf("disk migration requires valid VM, container and request UUIDs")
		}
	}
	if spec.ETag == "" || len(spec.DiskUUIDs) == 0 || len(spec.DiskUUIDs) > 1034 {
		return "", fmt.Errorf("disk migration requires an ETag and 1..1034 disk UUIDs")
	}
	body := diskMigrationBody{ObjectType: "vmm.v4.ahv.config.DiskMigrationParams", Discriminator: "vmm.v4.ahv.config.MigrationPlans"}
	body.MigrateDisks.ObjectType = body.Discriminator
	plan := adsfDiskMigrationPlan{ObjectType: "vmm.v4.ahv.config.ADSFDiskMigrationPlan", Container: reference{"vmm.v4.ahv.config.VmDiskContainerReference", spec.ContainerUUID}}
	seen := map[string]bool{}
	for _, id := range spec.DiskUUIDs {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || seen[parsed.String()] {
			return "", fmt.Errorf("disk migration requires unique valid disk UUIDs")
		}
		seen[parsed.String()] = true
		plan.Disks = append(plan.Disks, migrateDiskReference{"vmm.v4.ahv.config.MigrateDiskReference", parsed.String()})
	}
	body.MigrateDisks.Plans = []adsfDiskMigrationPlan{plan}
	return c.mutation(ctx, http.MethodPost, c.vmm+"/ahv/config/vms/"+url.PathEscape(spec.VMUUID)+"/$actions/migrate-vm-disks", body, spec.RequestUUID, spec.ETag)
}
