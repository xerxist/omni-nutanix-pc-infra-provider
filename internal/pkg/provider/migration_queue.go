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

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/uuid"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"go.uber.org/zap"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const migrationRetryAnnotation = "nutanix.infra.sidero.dev/disk-migration-limit-retries"
const migrationSubmissionAnnotation = "nutanix.infra.sidero.dev/disk-migration-submission"
const maxMigrationLimitRetries = 5
const migrationQueueRetry = 10 * time.Second

type migrationRejection struct {
	TaskUUID    string `json:"task_uuid"`
	RequestUUID string `json:"request_uuid"`
	ETag        string `json:"etag"`
}

// The original intent is immutable. This bounded journal retains every rejected
// task/operation and the current attempt's fresh ETag, stable ID and backoff.
type migrationLimitRetries struct {
	Rejected    []migrationRejection `json:"rejected"`
	RequestUUID string               `json:"request_uuid"`
	ETag        string               `json:"etag"`
	NotBefore   time.Time            `json:"not_before"`
}

type migrationQueueOwner struct{ request, vm, task string }

// ConfigureMigrationQueue enables restart-aware admission for one active provider
// process. Deployment must retain one replica/Recreate; it is not a distributed
// lease for multiple active providers sharing the same provider identity.
func (p *Provisioner) ConfigureMigrationQueue(st state.State) { p.migrationState = st }

func (p *Provisioner) migrationTime() time.Time {
	if p.migrationNow != nil {
		return p.migrationNow()
	}
	return time.Now()
}

func migrationRetryRequestID(original, task string, attempt int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("nutanix-v4/migration-limit-retry/%s/%s/%d", original, task, attempt))).String()
}

func decodeMigrationRetries(m *resources.Machine, intent *migrationIntent) (*migrationLimitRetries, error) {
	raw, exists := m.Metadata().Annotations().Get(migrationRetryAnnotation)
	if !exists {
		return nil, nil
	}
	var retries migrationLimitRetries
	if intent == nil || len(raw) > maxMigrationIntentBytes || json.Unmarshal([]byte(raw), &retries) != nil ||
		len(retries.Rejected) == 0 || len(retries.Rejected) > maxMigrationLimitRetries || retries.ETag == "" || retries.NotBefore.IsZero() {
		return nil, fmt.Errorf("invalid persisted migration limit retry journal")
	}
	request, etag := intent.RequestUUID, intent.ETag
	seen := map[string]bool{}
	for i, rejection := range retries.Rejected {
		if rejection.TaskUUID == "" || len(rejection.TaskUUID) > 512 || strings.ContainsAny(rejection.TaskUUID, "\r\n") || seen[rejection.TaskUUID] || rejection.RequestUUID != request || rejection.ETag == "" || (i == 0 && rejection.ETag != etag) {
			return nil, fmt.Errorf("migration limit retry history conflicts with frozen operation")
		}
		seen[rejection.TaskUUID] = true
		request = migrationRetryRequestID(intent.RequestUUID, rejection.TaskUUID, i+1)
	}
	task, _ := m.Metadata().Annotations().Get(migrationTaskAnnotation)
	if request != retries.RequestUUID || seen[task] {
		return nil, fmt.Errorf("migration limit retry operation/task conflicts with history")
	}
	if _, marked := m.Metadata().Annotations().Get(migrationSubmissionAnnotation); !marked {
		return nil, fmt.Errorf("migration retry journal lacks submission state")
	}
	return &retries, nil
}

func activeMigrationSpec(m *resources.Machine, intent *migrationIntent) (client.DiskMigrationSpec, error) {
	spec := intent.spec()
	retries, err := decodeMigrationRetries(m, intent)
	if err != nil {
		return spec, err
	}
	if retries != nil {
		spec.RequestUUID, spec.ETag = retries.RequestUUID, retries.ETag
	}
	return spec, nil
}

func (p *Provisioner) prepareMigrationLimitRetry(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine], intent *migrationIntent, task string, result client.TaskResult) error {
	if !result.MigrationLimitRejected || result.VMUUID != intent.VMUUID {
		return fmt.Errorf("disk migration task failed/canceled; inspect Prism Central before retrying")
	}
	retries, err := decodeMigrationRetries(pctx.State, intent)
	if err != nil {
		return err
	}
	if retries == nil {
		retries = &migrationLimitRetries{}
	}
	if len(retries.Rejected) >= maxMigrationLimitRetries {
		return fmt.Errorf("migration limit retry budget exhausted; operator review required")
	}
	// Fresh GET after the terminal rejection. Partial placement, changed layout,
	// unknown/active migration or ON prevents a new operation even for this code.
	vm, err := p.c.GetVM(ctx, intent.VMUUID)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "verify rejected migration: %w", err)
	}
	bios, _ := pctx.State.Metadata().Annotations().Get(biosAnnotation)
	cluster, _ := pctx.State.Metadata().Annotations().Get(clusterAnnotation)
	if err := verifyVM(vm, pctx.GetRequestID(), bios, cluster); err != nil {
		return err
	}
	if vm.UUID != intent.VMUUID || vm.PowerState != "OFF" || vm.ETag == "" || pctx.State.TypedSpec().Value.PowerOnTaskUuid != "" {
		return fmt.Errorf("migration retry requires fresh OFF VM, ETag and no power task")
	}
	if err := verifyMigrationInventory(vm, pctx); err != nil {
		return err
	}
	if err := intent.verifyDisks(vm, true); err != nil {
		return err
	}
	spec, err := activeMigrationSpec(pctx.State, intent)
	if err != nil {
		return err
	}
	retries.Rejected = append(retries.Rejected, migrationRejection{TaskUUID: task, RequestUUID: spec.RequestUUID, ETag: spec.ETag})
	retries.RequestUUID = migrationRetryRequestID(intent.RequestUUID, task, len(retries.Rejected))
	retries.ETag = vm.ETag
	delay := 30 * time.Second * time.Duration(1<<(len(retries.Rejected)-1))
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	retries.NotBefore = p.migrationTime().Add(delay).UTC()
	raw, err := json.Marshal(retries)
	if err != nil || len(raw) > maxMigrationIntentBytes {
		return fmt.Errorf("migration limit retry journal exceeds annotation limit")
	}
	pctx.State.Metadata().Annotations().Set(migrationRetryAnnotation, string(raw))
	pctx.State.Metadata().Annotations().Delete(migrationTaskAnnotation)
	pctx.State.Metadata().Annotations().Set(migrationSubmissionAnnotation, "queued")
	if p.migrationOwner != nil && p.migrationOwner.request == pctx.GetRequestID() {
		p.migrationOwner = nil
	}
	logger.Info("migration limit rejection queued for bounded retry", zap.String("vm_uuid", vm.UUID), zap.String("rejected_task_uuid", task), zap.Int("attempt", len(retries.Rejected)))
	return provision.NewRetryErrorf(delay, "Prism migration limit VMM-30716; retry %d/%d queued", len(retries.Rejected), maxMigrationLimitRetries)
}

// Examine persisted tasks before every POST, including after restart. An unknown
// outcome reserves the slot; terminal tasks release it only after fresh idle VM
// metadata. Queued no-task intents use creation time/ID order to avoid bursts.
func (p *Provisioner) admitMigration(ctx context.Context, pctx provision.Context[*resources.Machine]) error {
	if p.migrationOwner != nil && p.migrationOwner.request != pctx.GetRequestID() {
		if p.migrationOwner.task == "" {
			return provision.NewRetryErrorf(migrationQueueRetry, "migration queue waiting for an unresolved submission")
		}
		result, err := p.c.WaitTask(ctx, p.migrationOwner.task)
		if result.State == client.TaskPending || (err != nil && result.State != client.TaskFailed) || (result.VMUUID != "" && result.VMUUID != p.migrationOwner.vm) {
			return provision.NewRetryErrorf(migrationQueueRetry, "migration queue waiting for active/unknown task")
		}
		// Persisted scope below performs identity/idle validation before release.
	}
	if p.migrationState == nil {
		if p.migrationOwner != nil && p.migrationOwner.request != pctx.GetRequestID() {
			return provision.NewRetryInterval(migrationQueueRetry)
		}
		return nil // Isolated offline contexts; production always configures state.
	}
	machines, err := safe.StateListAll[*resources.Machine](ctx, p.migrationState)
	if err != nil {
		return provision.NewRetryErrorf(migrationQueueRetry, "read migration queue scope: details suppressed")
	}
	ownerFound := p.migrationOwner == nil || p.migrationOwner.request == pctx.GetRequestID()
	ownSubmitted, ownMarked := pctx.State.Metadata().Annotations().Get(migrationSubmissionAnnotation)
	ownReserved := !ownMarked || ownSubmitted != "queued" // Older no-task state has an unknown submission outcome.
	for m := range machines.All() {
		if m.Metadata().ID() == pctx.GetRequestID() {
			continue
		}
		intent, err := decodeMigrationIntent(m)
		if err != nil {
			return fmt.Errorf("migration queue contains invalid state; operator review required")
		}
		if intent == nil {
			continue
		}
		if p.migrationOwner != nil && m.Metadata().ID() == p.migrationOwner.request {
			ownerFound = true
		}
		complete, _ := m.Metadata().Annotations().Get(migrationCompleteAnnotation)
		if complete != "" {
			continue
		}
		task, _ := m.Metadata().Annotations().Get(migrationTaskAnnotation)
		if task == "" {
			submitted, _ := m.Metadata().Annotations().Get(migrationSubmissionAnnotation)
			if submitted != "queued" {
				return provision.NewRetryErrorf(migrationQueueRetry, "migration queue waiting for unresolved persisted submission")
			}
			retries, err := decodeMigrationRetries(m, intent)
			if err != nil {
				return err
			}
			if retries != nil && p.migrationTime().Before(retries.NotBefore) {
				continue
			}
			if ownReserved {
				continue // Never deadlock an already-reserved replay behind a newer queued intent.
			}
			created, own := m.Metadata().Created(), pctx.State.Metadata().Created()
			if created.Before(own) || (created.Equal(own) && m.Metadata().ID() < pctx.GetRequestID()) {
				return provision.NewRetryErrorf(migrationQueueRetry, "migration queued behind earlier persisted operation")
			}
			continue
		}
		result, err := p.c.WaitTask(ctx, task)
		if result.State == client.TaskPending || (err != nil && result.State != client.TaskFailed) || (result.VMUUID != "" && result.VMUUID != intent.VMUUID) {
			return provision.NewRetryErrorf(migrationQueueRetry, "migration queue waiting for active/unknown task")
		}
		vm, err := p.c.GetVM(ctx, intent.VMUUID)
		if err != nil {
			return provision.NewRetryErrorf(migrationQueueRetry, "migration queue cannot prove terminal VM idle")
		}
		bios, _ := m.Metadata().Annotations().Get(biosAnnotation)
		cluster, _ := m.Metadata().Annotations().Get(clusterAnnotation)
		if verifyVM(vm, m.Metadata().ID(), bios, cluster) != nil || vm.UUID != intent.VMUUID || verifyMigrationMetadata(vm) != nil || intent.verifyDisks(vm, false) != nil || (configDriveMigrationEnabled(m) && verifyConfigDrive(vm, m, false) != nil) {
			return provision.NewRetryErrorf(migrationQueueRetry, "migration queue terminal VM identity/idle metadata not proven")
		}
	}
	if !ownerFound {
		return fmt.Errorf("migration queue owner state missing; operator review required")
	}
	if p.migrationOwner != nil && p.migrationOwner.request != pctx.GetRequestID() {
		p.migrationOwner = nil
	}
	return nil
}
