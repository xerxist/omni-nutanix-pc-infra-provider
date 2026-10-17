// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/uuid"
	"github.com/siderolabs/omni/client/pkg/infra"
	infrares "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"google.golang.org/protobuf/proto"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

var imageHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var imageVersionPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// ImageRecoveryInput is explicit operator approval, never an automatic retry.
// TaskIDs must cover the complete reported task family for the retired image.
type ImageRecoveryInput struct {
	CacheID          string   `json:"cache_id"`
	JournalSHA256    string   `json:"journal_sha256"`
	JournalVersion   string   `json:"journal_version"`
	JournalCreated   string   `json:"journal_created"`
	RetiredImageUUID string   `json:"retired_image_uuid"`
	TaskIDs          []string `json:"task_ids"`
	AttemptUUID      string   `json:"attempt_uuid"`
}

type imageRecoveryApproval struct {
	Version         int                    `json:"version"`
	Input           ImageRecoveryInput     `json:"input"`
	CacheID         string                 `json:"cache_id"`
	Spec            client.ImageCreateSpec `json:"spec"`
	OriginalName    string                 `json:"original_name"`
	TaskWindowStart time.Time              `json:"task_window_start"`
	TaskWindowEnd   time.Time              `json:"task_window_end"`
}

type imageRecoveryPlan struct {
	ParentCacheID   string `json:"parent_cache_id"`
	AttemptUUID     string `json:"attempt_uuid"`
	ApprovalSHA256  string `json:"approval_sha256"`
	ApprovalVersion string `json:"approval_version"`
	ApprovalCreated string `json:"approval_created"`
	Checksum        string `json:"checksum"`
}

// ImageRecoverySummary deliberately omits specs, credentials and source URLs.
type ImageRecoverySummary struct {
	ApprovalCreated     bool   `json:"approval_created"`
	AlreadyApproved     bool   `json:"already_approved"`
	OriginalCacheID     string `json:"original_cache_id"`
	NewCacheID          string `json:"new_cache_id"`
	NewRequestUUID      string `json:"new_request_uuid"`
	RetiredImageUUID    string `json:"retired_image_uuid"`
	SuccessfulTaskCount int    `json:"successful_task_count"`
}

func (in ImageRecoveryInput) Validate() error {
	if !imageHashPattern.MatchString(in.CacheID) || !imageHashPattern.MatchString(in.JournalSHA256) || !imageVersionPattern.MatchString(in.JournalVersion) {
		return fmt.Errorf("recovery requires the exact cache ID, journal SHA256 and resource version")
	}
	created, err := time.Parse(time.RFC3339Nano, in.JournalCreated)
	if err != nil || created.IsZero() || created.UTC().Format(time.RFC3339Nano) != in.JournalCreated {
		return fmt.Errorf("recovery requires the exact original journal creation timestamp in canonical UTC")
	}
	image, err := uuid.Parse(in.RetiredImageUUID)
	if err != nil || image == uuid.Nil || image.String() != in.RetiredImageUUID {
		return fmt.Errorf("recovery requires a canonical nonzero retired image UUID")
	}
	attempt, err := uuid.Parse(in.AttemptUUID)
	if err != nil || attempt == uuid.Nil || attempt.String() != in.AttemptUUID || attempt.Version() != 4 {
		return fmt.Errorf("recovery requires an explicitly supplied canonical random v4 attempt UUID")
	}
	if len(in.TaskIDs) == 0 || len(in.TaskIDs) > 16 {
		return fmt.Errorf("recovery requires 1 to 16 exact task IDs")
	}
	seen := map[string]bool{}
	for _, id := range in.TaskIDs {
		if err := client.ValidateTaskID(id); err != nil {
			return err
		}
		if seen[id] {
			return fmt.Errorf("recovery task IDs must be unique")
		}
		seen[id] = true
	}
	return nil
}

func registerImageImportResources() error {
	journal := resources.NewImageImport("")
	if err := protobuf.RegisterResource(journal.Metadata().Type(), journal); err != nil && !strings.Contains(err.Error(), "is already registered") {
		return err
	}
	approval := resources.NewImageImportRecovery("")
	if err := protobuf.RegisterResource(approval.Metadata().Type(), approval); err != nil && !strings.Contains(err.Error(), "is already registered") {
		return err
	}
	replacement := resources.NewImageImportReplacement("")
	if err := protobuf.RegisterResource(replacement.Metadata().Type(), replacement); err != nil && !strings.Contains(err.Error(), "is already registered") {
		return err
	}
	return nil
}

func imageResourceHash(r interface {
	TypedSpec() *resources.ImageImportSpec
}) string {
	raw, err := proto.Marshal(r.TypedSpec().Value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func imageRecoveryIdentity(original client.ImageCreateSpec, attempt string) (string, client.ImageCreateSpec) {
	parent, _ := imageCacheIdentity(original.SourceURL, original.ClusterUUIDs)
	raw, _ := json.Marshal(struct{ Contract, Provider, Parent, Attempt string }{"image-import-explicit-recovery-v1", meta.ProviderID, parent, attempt})
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	spec := original
	spec.Name = "omni-talos-" + id
	spec.Description = "Omni provider " + meta.ProviderID + " image cache " + id
	spec.RequestUUID = uuid.NewSHA1(uuid.NameSpaceURL, raw).String()
	return id, spec
}

func recoveryParent(ctx context.Context, st state.State, in ImageRecoveryInput) (*imageImportJournal, error) {
	r, err := safe.StateGet[*resources.ImageImport](ctx, st, resources.NewImageImport(in.CacheID).Metadata())
	if err != nil {
		return nil, fmt.Errorf("recovery original journal unavailable; details suppressed")
	}
	if r.Metadata().Version().String() != in.JournalVersion || imageResourceHash(r) != in.JournalSHA256 || r.Metadata().Created().UTC().Format(time.RFC3339Nano) != in.JournalCreated {
		return nil, fmt.Errorf("recovery original journal changed; refusing recovery")
	}
	var raw imageImportJournal
	if len(r.TypedSpec().Value.Value) > maxImageJournalBytes || decodeImageJSON(r.TypedSpec().Value.Value, &raw) != nil {
		return nil, fmt.Errorf("invalid recovery original journal")
	}
	id, original := imageCacheIdentity(raw.Spec.SourceURL, raw.Spec.ClusterUUIDs)
	if id != in.CacheID {
		return nil, fmt.Errorf("recovery original cache identity mismatch")
	}
	j, err := decodeImageJournal(r, &imageImportPlan{Version: 1, CacheID: id, Spec: original})
	if err != nil {
		return nil, err
	}
	if !j.Submitted {
		return nil, fmt.Errorf("recovery original journal was not submitted")
	}
	if j.ImageUUID != "" && j.ImageUUID != in.RetiredImageUUID {
		return nil, fmt.Errorf("recovery retired image differs from recorded identity")
	}
	if j.TaskUUID != "" && !slices.Contains(in.TaskIDs, j.TaskUUID) {
		return nil, fmt.Errorf("recovery excludes the recorded submission task")
	}
	return j, nil
}

func decodeImageRecovery(r *resources.ImageImportRecovery) (*imageRecoveryApproval, error) {
	var a imageRecoveryApproval
	if r.Metadata().Phase() != resource.PhaseRunning || len(r.TypedSpec().Value.Value) > maxImageJournalBytes || decodeImageJSON(r.TypedSpec().Value.Value, &a) != nil || a.Version != 1 {
		return nil, fmt.Errorf("invalid recovery approval; no import")
	}
	if err := a.Input.Validate(); err != nil {
		return nil, err
	}
	if a.Input.CacheID != r.Metadata().ID() {
		return nil, fmt.Errorf("recovery approval parent identity mismatch")
	}
	created, _ := time.Parse(time.RFC3339Nano, a.Input.JournalCreated)
	if !a.TaskWindowStart.Equal(created.Add(-2*time.Minute)) || !a.TaskWindowEnd.Equal(created.Add(5*time.Minute)) {
		return nil, fmt.Errorf("recovery task submission window mismatch")
	}
	parent, original := imageCacheIdentity(a.Spec.SourceURL, a.Spec.ClusterUUIDs)
	original.SHA256 = a.Spec.SHA256
	id, expected := imageRecoveryIdentity(original, a.Input.AttemptUUID)
	x, _ := json.Marshal(expected)
	y, _ := json.Marshal(a.Spec)
	if parent != a.Input.CacheID || id != a.CacheID || string(x) != string(y) || a.OriginalName != original.Name || a.Spec.RequestUUID == original.RequestUUID {
		return nil, fmt.Errorf("recovery approval intent mismatch")
	}
	if err := a.Spec.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

func verifyImageRecoveryPrism(ctx context.Context, c client.Client, a *imageRecoveryApproval) error {
	for _, id := range a.Input.TaskIDs {
		task, err := c.GetImageTask(ctx, id)
		if err != nil || task == nil || task.ID != id || task.State != client.TaskSucceeded {
			return fmt.Errorf("recovery requires fresh successful exact task evidence; details suppressed")
		}
		if task.CreatedAt.IsZero() || task.CompletedAt.IsZero() || task.CreatedAt.Before(a.TaskWindowStart) || task.CreatedAt.After(a.TaskWindowEnd) || task.CompletedAt.Before(task.CreatedAt) {
			return fmt.Errorf("recovery task timing does not match the original submission window")
		}
		for _, cluster := range a.Spec.ClusterUUIDs {
			if !slices.Contains(task.ClusterUUIDs, cluster) {
				return fmt.Errorf("recovery task does not reference every frozen destination cluster")
			}
		}
		matched := false
		for _, e := range task.Entities {
			if e.Rel == "vmm:ahv:vm" || e.Rel == "vmm:ahv:config:vm" {
				return fmt.Errorf("recovery task references a VM")
			}
			if e.UUID == a.Input.RetiredImageUUID && e.Name == a.OriginalName && e.Rel == "vmm:content:image" {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("recovery task does not identify the exact retired image")
		}
		for _, family := range task.FamilyIDs {
			if !slices.Contains(a.Input.TaskIDs, family) {
				return fmt.Errorf("recovery task family is incomplete; supply every reported task ID")
			}
		}
	}
	image, err := c.GetImage(ctx, a.Input.RetiredImageUUID)
	if err == nil || !c.IsNotFound(err) || image != nil {
		return fmt.Errorf("recovery requires explicit retired-image HTTP 404; no import")
	}
	image, err = c.FindImage(ctx, a.OriginalName)
	if err != nil || image != nil {
		return fmt.Errorf("recovery original image name is present, ambiguous or lookup failed; no import")
	}
	return nil
}

func requireImageRecoveryQuiescence(ctx context.Context, st state.State) error {
	requests, err := safe.StateListAll[*infrares.MachineRequest](ctx, st, state.WithLabelQuery(resource.LabelEqual(omnires.LabelInfraProviderID, meta.ProviderID)))
	if err != nil || requests.Len() != 0 {
		return fmt.Errorf("recovery approval requires zero provider requests; details suppressed")
	}
	machines, err := st.List(ctx, resources.NewMachine(infra.ResourceNamespace(meta.ProviderID), "").Metadata(), state.WithListUnmarshalOptions(state.WithSkipProtobufUnmarshal()))
	if err != nil || len(machines.Items) != 0 {
		return fmt.Errorf("recovery approval requires zero provider machines; details suppressed")
	}
	return nil
}

// ApproveImageImportRecovery performs GETs only by default. apply creates exactly
// an unsubmitted successor journal and one immutable approval resource, never an
// image/VM or a reset of the old journal. An interrupted journal-only creation is
// harmless: no request selects it without the approval, and repeating the exact
// command can finish activation without changing the existing journal.
// Only future, user-created MachineRequests can select the successor attempt.
func ApproveImageImportRecovery(ctx context.Context, st state.State, c client.Client, in ImageRecoveryInput, apply bool) (*ImageRecoverySummary, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if st == nil || c == nil {
		return nil, fmt.Errorf("recovery requires durable provider state and a Prism client")
	}
	if err := registerImageImportResources(); err != nil {
		return nil, err
	}
	in.TaskIDs = slices.Clone(in.TaskIDs)
	slices.Sort(in.TaskIDs)
	parent, err := recoveryParent(ctx, st, in)
	if err != nil {
		return nil, err
	}
	id, spec := imageRecoveryIdentity(parent.Spec, in.AttemptUUID)
	created, _ := time.Parse(time.RFC3339Nano, in.JournalCreated)
	approval := imageRecoveryApproval{Version: 1, Input: in, CacheID: id, Spec: spec, OriginalName: parent.Spec.Name, TaskWindowStart: created.Add(-2 * time.Minute), TaskWindowEnd: created.Add(5 * time.Minute)}
	r := resources.NewImageImportRecovery(in.CacheID)
	raw, _ := json.Marshal(approval)
	if len(raw) > maxImageJournalBytes {
		return nil, fmt.Errorf("recovery approval exceeds limit")
	}
	r.TypedSpec().Value.Value = string(raw)
	summary := &ImageRecoverySummary{OriginalCacheID: in.CacheID, NewCacheID: id, NewRequestUUID: spec.RequestUUID, RetiredImageUUID: in.RetiredImageUUID, SuccessfulTaskCount: len(in.TaskIDs)}
	existing, err := safe.StateGet[*resources.ImageImportRecovery](ctx, st, r.Metadata())
	if err == nil {
		if _, err := decodeImageRecovery(existing); err != nil {
			return nil, err
		}
		if existing.TypedSpec().Value.Value != r.TypedSpec().Value.Value {
			return nil, fmt.Errorf("a different recovery is already approved; no replacement or retry")
		}
		summary.AlreadyApproved = true
		return summary, nil // Idempotent receipt; never resubmit or modify it.
	}
	if !state.IsNotFoundError(err) {
		return nil, fmt.Errorf("read recovery approval failed; details suppressed")
	}
	if err := requireImageRecoveryQuiescence(ctx, st); err != nil {
		return nil, err
	}
	if err := verifyImageRecoveryPrism(ctx, c, &approval); err != nil {
		return nil, err
	}
	if !apply {
		return summary, nil
	}
	if _, err := recoveryParent(ctx, st, in); err != nil {
		return nil, err
	}
	if err := requireImageRecoveryQuiescence(ctx, st); err != nil {
		return nil, err
	}
	// Seed before publishing the approval. Recovery requests NEVER recreate a
	// missing journal, including brand-new requests that have not seen it before.
	attempt := resources.NewImageImport(id)
	if err := writeImageJournal(attempt, &imageImportJournal{Version: 1, Spec: spec}); err != nil {
		return nil, err
	}
	if err := st.Create(ctx, attempt); err != nil {
		if !state.IsConflictError(err) {
			return nil, fmt.Errorf("persist recovery successor intent failed; details suppressed")
		}
		other, readErr := safe.StateGet[*resources.ImageImport](ctx, st, attempt.Metadata())
		if readErr != nil || other.Metadata().Phase() != resource.PhaseRunning || other.TypedSpec().Value.Value != attempt.TypedSpec().Value.Value {
			return nil, fmt.Errorf("recovery successor journal conflicts or was already submitted; no reset")
		}
	}
	if _, err := recoveryParent(ctx, st, in); err != nil {
		return nil, err
	}
	if err := requireImageRecoveryQuiescence(ctx, st); err != nil {
		return nil, err
	}
	r.Metadata().SetCreated(time.Now().UTC()) // Request cutoff belongs to activation, not the initial review.
	if err := st.Create(ctx, r); err != nil {
		if state.IsConflictError(err) {
			other, readErr := safe.StateGet[*resources.ImageImportRecovery](ctx, st, r.Metadata())
			if readErr == nil && other.TypedSpec().Value.Value == r.TypedSpec().Value.Value {
				summary.AlreadyApproved = true
				return summary, nil
			}
		}
		return nil, fmt.Errorf("persist recovery approval failed or conflicted; details suppressed")
	}
	summary.ApprovalCreated = true
	return summary, nil
}

func validateImageRecoveryPlan(plan *imageImportPlan, parent string) error {
	if len(plan.Replacements) > 0 {
		if plan.Version != 3 {
			return fmt.Errorf("invalid persisted image replacement plan version")
		}
		base := baseImageImportPlan(plan)
		return validateImageRecoveryPlan(&base, parent)
	}
	if plan.Version == 1 && plan.Recovery == nil {
		return nil
	}
	r := plan.Recovery
	if plan.Version != 2 || r == nil || r.ParentCacheID != parent || !imageHashPattern.MatchString(r.ApprovalSHA256) || !imageHashPattern.MatchString(r.Checksum) || !imageVersionPattern.MatchString(r.ApprovalVersion) {
		return fmt.Errorf("invalid persisted image recovery plan")
	}
	created, err := time.Parse(time.RFC3339Nano, r.ApprovalCreated)
	if err != nil || created.IsZero() || created.UTC().Format(time.RFC3339Nano) != r.ApprovalCreated {
		return fmt.Errorf("invalid persisted recovery approval creation timestamp")
	}
	id, err := uuid.Parse(r.AttemptUUID)
	if err != nil || id.String() != r.AttemptUUID || id.Version() != 4 {
		return fmt.Errorf("invalid persisted recovery attempt UUID")
	}
	return nil
}

func (p *Provisioner) selectImageRecovery(ctx context.Context, plan *imageImportPlan, requestID string) error {
	r, err := safe.StateGet[*resources.ImageImportRecovery](ctx, p.imageState, resources.NewImageImportRecovery(plan.CacheID).Metadata())
	if state.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read image recovery selection failed; details suppressed")
	}
	a, err := decodeImageRecovery(r)
	if err != nil {
		return err
	}
	request, err := safe.StateGet[*infrares.MachineRequest](ctx, p.imageState, infrares.NewMachineRequest(requestID).Metadata())
	if err != nil {
		return fmt.Errorf("recovery requires a durable new Omni request; details suppressed")
	}
	providerID, _ := request.Metadata().Labels().Get(omnires.LabelInfraProviderID)
	if providerID != meta.ProviderID || request.Metadata().Phase() != resource.PhaseRunning || !request.Metadata().Created().After(r.Metadata().Created()) {
		return fmt.Errorf("recovery is restricted to this provider's requests created after approval; existing requests are never retargeted")
	}
	parent, err := recoveryParent(ctx, p.imageState, a.Input)
	if err != nil {
		return err
	}
	if a.Spec.SHA256 != parent.Spec.SHA256 {
		return fmt.Errorf("recovery approval checksum differs from original journal")
	}
	plan.Version, plan.CacheID, plan.Spec = 2, a.CacheID, a.Spec
	plan.Spec.SHA256 = ""
	plan.Recovery = &imageRecoveryPlan{ParentCacheID: a.Input.CacheID, AttemptUUID: a.Input.AttemptUUID, ApprovalSHA256: imageResourceHash(r), ApprovalVersion: r.Metadata().Version().String(), ApprovalCreated: r.Metadata().Created().UTC().Format(time.RFC3339Nano), Checksum: a.Spec.SHA256}
	return nil
}

func (p *Provisioner) loadImageRecovery(ctx context.Context, plan *imageImportPlan) (*imageRecoveryApproval, error) {
	frozen := plan.Recovery
	r, err := safe.StateGet[*resources.ImageImportRecovery](ctx, p.imageState, resources.NewImageImportRecovery(frozen.ParentCacheID).Metadata())
	if err != nil {
		return nil, fmt.Errorf("frozen recovery approval disappeared or cannot be read; no automatic fallback")
	}
	if imageResourceHash(r) != frozen.ApprovalSHA256 || r.Metadata().Version().String() != frozen.ApprovalVersion || r.Metadata().Created().UTC().Format(time.RFC3339Nano) != frozen.ApprovalCreated {
		return nil, fmt.Errorf("frozen recovery approval changed")
	}
	a, err := decodeImageRecovery(r)
	if err != nil {
		return nil, err
	}
	parent, err := recoveryParent(ctx, p.imageState, a.Input)
	if err != nil {
		return nil, err
	}
	if a.CacheID != plan.CacheID || a.Input.AttemptUUID != frozen.AttemptUUID || a.Spec.SHA256 != frozen.Checksum || parent.Spec.SHA256 != frozen.Checksum {
		return nil, fmt.Errorf("frozen recovery intent mismatch")
	}
	return a, nil
}
