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
	"slices"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/uuid"
	"github.com/siderolabs/omni/client/pkg/infra/provision"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const maxImageReplacements = 16

type imageReplacementEvidence struct {
	ParentCacheID        string `json:"parent_cache_id"`
	ParentJournalSHA256  string `json:"parent_journal_sha256"`
	ParentJournalVersion string `json:"parent_journal_version"`
	ParentJournalCreated string `json:"parent_journal_created"`
	RetiredImageUUID     string `json:"retired_image_uuid"`
	Checksum             string `json:"checksum"`
}

type imageReplacementIntent struct {
	Version int                      `json:"version"`
	Parent  imageReplacementEvidence `json:"parent"`
	Spec    client.ImageCreateSpec   `json:"spec"`
}

type imageReplacementPlan struct {
	Parent             imageReplacementEvidence `json:"parent"`
	ReplacementSHA256  string                   `json:"replacement_sha256"`
	ReplacementVersion string                   `json:"replacement_version"`
	ReplacementCreated string                   `json:"replacement_created"`
}

func canonicalImageTimestamp(raw string) bool {
	v, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && !v.IsZero() && v.UTC().Format(time.RFC3339Nano) == raw
}

func (e imageReplacementEvidence) validate(parent string) error {
	id, err := uuid.Parse(e.RetiredImageUUID)
	if e.ParentCacheID != parent || !imageHashPattern.MatchString(parent) || !imageHashPattern.MatchString(e.ParentJournalSHA256) ||
		!imageHashPattern.MatchString(e.Checksum) || !imageVersionPattern.MatchString(e.ParentJournalVersion) ||
		!canonicalImageTimestamp(e.ParentJournalCreated) || err != nil || id == uuid.Nil || id.String() != e.RetiredImageUUID {
		return fmt.Errorf("invalid completed-image replacement evidence")
	}
	return nil
}

// A completed parent has exactly one deterministic successor. No random attempt
// rotation is possible after a crash, failed claim, lost POST response or restart.
func imageReplacementIdentity(original client.ImageCreateSpec, e imageReplacementEvidence) (string, client.ImageCreateSpec) {
	raw, _ := json.Marshal(struct {
		Contract, Provider string
		Parent             imageReplacementEvidence
	}{"image-import-deleted-completed-cache-v1", meta.ProviderID, e})
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	spec := original
	spec.Name = "omni-talos-" + id
	spec.Description = "Omni provider " + meta.ProviderID + " image cache " + id
	spec.RequestUUID = uuid.NewSHA1(uuid.NameSpaceURL, raw).String()
	return id, spec
}

func baseImageImportPlan(plan *imageImportPlan) imageImportPlan {
	base := *plan
	base.Replacements = nil
	base.Version = 1
	base.CacheID, base.Spec = imageCacheIdentity(plan.Spec.SourceURL, plan.Spec.ClusterUUIDs)
	if base.Recovery != nil {
		base.Version = 2
		base.CacheID, base.Spec = imageRecoveryIdentity(base.Spec, base.Recovery.AttemptUUID)
	}
	return base
}

func validateImageReplacementPlans(plan *imageImportPlan, id string, spec client.ImageCreateSpec) (string, client.ImageCreateSpec, error) {
	if len(plan.Replacements) == 0 {
		return id, spec, nil
	}
	if plan.Version != 3 || len(plan.Replacements) > maxImageReplacements {
		return "", spec, fmt.Errorf("invalid persisted image replacement chain")
	}
	checksum := ""
	if plan.Recovery != nil {
		checksum = plan.Recovery.Checksum
	}
	for _, ref := range plan.Replacements {
		if err := ref.Parent.validate(id); err != nil {
			return "", spec, err
		}
		if !imageHashPattern.MatchString(ref.ReplacementSHA256) || !imageVersionPattern.MatchString(ref.ReplacementVersion) ||
			!canonicalImageTimestamp(ref.ReplacementCreated) || (checksum != "" && checksum != ref.Parent.Checksum) {
			return "", spec, fmt.Errorf("invalid frozen image replacement reference")
		}
		checksum = ref.Parent.Checksum
		id, spec = imageReplacementIdentity(spec, ref.Parent)
	}
	return id, spec, nil
}

func replacementEvidence(r *resources.ImageImport, j *imageImportJournal) imageReplacementEvidence {
	return imageReplacementEvidence{ParentCacheID: r.Metadata().ID(), ParentJournalSHA256: imageResourceHash(r),
		ParentJournalVersion: r.Metadata().Version().String(), ParentJournalCreated: r.Metadata().Created().UTC().Format(time.RFC3339Nano),
		RetiredImageUUID: j.ImageUUID, Checksum: j.Spec.SHA256}
}

func (p *Provisioner) replacementParent(ctx context.Context, plan *imageImportPlan, e imageReplacementEvidence) (*imageImportJournal, error) {
	if err := e.validate(plan.CacheID); err != nil {
		return nil, err
	}
	r, err := safe.StateGet[*resources.ImageImport](ctx, p.imageState, resources.NewImageImport(plan.CacheID).Metadata())
	if err != nil || imageResourceHash(r) != e.ParentJournalSHA256 || r.Metadata().Version().String() != e.ParentJournalVersion ||
		r.Metadata().Created().UTC().Format(time.RFC3339Nano) != e.ParentJournalCreated {
		return nil, fmt.Errorf("completed replacement parent disappeared or changed; no import")
	}
	j, err := decodeImageJournal(r, plan)
	if err != nil {
		return nil, err
	}
	if !j.Submitted || j.ImageUUID != e.RetiredImageUUID || j.Spec.SHA256 != e.Checksum {
		return nil, fmt.Errorf("replacement requires the exact completed image journal; uncertain submissions cannot be replaced")
	}
	return j, nil
}

func decodeImageReplacement(r *resources.ImageImportReplacement, parent *imageImportPlan) (*imageReplacementIntent, error) {
	var intent imageReplacementIntent
	if r.Metadata().Phase() != resource.PhaseRunning || len(r.TypedSpec().Value.Value) > maxImageJournalBytes ||
		decodeImageJSON(r.TypedSpec().Value.Value, &intent) != nil || intent.Version != 1 || r.Metadata().ID() != parent.CacheID {
		return nil, fmt.Errorf("invalid image replacement intent; no import")
	}
	if err := intent.Parent.validate(parent.CacheID); err != nil {
		return nil, err
	}
	_, expected := imageReplacementIdentity(parent.Spec, intent.Parent)
	expected.SHA256 = intent.Parent.Checksum
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(intent.Spec)
	if string(a) != string(b) {
		return nil, fmt.Errorf("image replacement identity/checksum drift; no import")
	}
	if err := intent.Spec.Validate(); err != nil {
		return nil, err
	}
	return &intent, nil
}

func appendImageReplacement(plan *imageImportPlan, r *resources.ImageImportReplacement, intent *imageReplacementIntent) {
	plan.Version = 3
	plan.CacheID, plan.Spec = imageReplacementIdentity(plan.Spec, intent.Parent)
	plan.Replacements = append(slices.Clone(plan.Replacements), imageReplacementPlan{Parent: intent.Parent,
		ReplacementSHA256: imageResourceHash(r), ReplacementVersion: r.Metadata().Version().String(),
		ReplacementCreated: r.Metadata().Created().UTC().Format(time.RFC3339Nano)})
}

// Only an exact UUID HTTP 404 AND successful exact-name absence qualify. Empty
// responses, 401/403, timeouts, failed lists and vanished LIST candidates do not.
func (p *Provisioner) verifyDeletedImage(ctx context.Context, e imageReplacementEvidence, name string) error {
	img, err := p.c.GetImage(ctx, e.RetiredImageUUID)
	if err == nil || !p.c.IsNotFound(err) || img != nil {
		return provision.NewRetryErrorf(vmRetry, "replacement requires confirmed deleted-image HTTP 404; details suppressed; no import")
	}
	img, err = p.c.FindImage(ctx, name)
	if err != nil || img != nil {
		return provision.NewRetryErrorf(vmRetry, "replacement parent image name is present, ambiguous or lookup failed; details suppressed; no import")
	}
	return nil
}

// Called only before the first plan is frozen. Previously frozen requests and
// all VM submissions remain on their original image/plan, even if it is deleted.
func (p *Provisioner) selectImageReplacement(ctx context.Context, plan *imageImportPlan) error {
	for {
		if len(plan.Replacements) > 0 {
			if err := p.loadImageReplacements(ctx, plan); err != nil {
				return err
			}
		}
		marker, err := safe.StateGet[*resources.ImageImportReplacement](ctx, p.imageState, resources.NewImageImportReplacement(plan.CacheID).Metadata())
		if err == nil {
			if len(plan.Replacements) >= maxImageReplacements {
				return fmt.Errorf("image replacement chain limit reached; operator review required")
			}
			intent, err := decodeImageReplacement(marker, plan)
			if err != nil {
				return err
			}
			if _, err := p.replacementParent(ctx, plan, intent.Parent); err != nil {
				return err
			}
			appendImageReplacement(plan, marker, intent)
			// Seeded successor journals are never automatically recreated.
			r, err := safe.StateGet[*resources.ImageImport](ctx, p.imageState, resources.NewImageImport(plan.CacheID).Metadata())
			if err != nil {
				return fmt.Errorf("image replacement successor disappeared or cannot be read; no automatic recreation")
			}
			if _, err := decodeImageJournal(r, plan); err != nil {
				return err
			}
			continue
		}
		if !state.IsNotFoundError(err) {
			return provision.NewRetryErrorf(vmRetry, "read image replacement selection: details suppressed")
		}
		r, err := safe.StateGet[*resources.ImageImport](ctx, p.imageState, resources.NewImageImport(plan.CacheID).Metadata())
		if state.IsNotFoundError(err) && len(plan.Replacements) == 0 {
			return nil // Initial import, or missing explicit recovery seed: ensure handles it.
		}
		if err != nil {
			return fmt.Errorf("image replacement journal unavailable; no import")
		}
		j, err := decodeImageJournal(r, plan)
		if err != nil {
			return err
		}
		if j.ImageUUID == "" {
			return nil // Unsubmitted or uncertain: never rotate/replace this attempt.
		}
		img, err := p.c.GetImage(ctx, j.ImageUUID)
		if err == nil && img != nil {
			return nil // ensureImportedImage still validates every metadata field.
		}
		if err == nil || !p.c.IsNotFound(err) || img != nil {
			return provision.NewRetryErrorf(vmRetry, "read completed image for replacement: details suppressed; no import")
		}
		if len(plan.Replacements) >= maxImageReplacements {
			return fmt.Errorf("image replacement chain limit reached; operator review required")
		}
		evidence := replacementEvidence(r, j)
		if _, err := p.replacementParent(ctx, plan, evidence); err != nil {
			return err
		}
		if err := p.verifyDeletedImage(ctx, evidence, j.Spec.Name); err != nil {
			return err
		}
		id, spec := imageReplacementIdentity(j.Spec, evidence)
		intent := imageReplacementIntent{Version: 1, Parent: evidence, Spec: spec}
		raw, err := json.Marshal(intent)
		if err != nil || len(raw) > maxImageJournalBytes {
			return fmt.Errorf("image replacement intent exceeds limit")
		}
		// Seed before publishing the immutable pointer. A crash in between is
		// harmless: no machine can select the seed without this marker.
		seed := resources.NewImageImport(id)
		if err := writeImageJournal(seed, &imageImportJournal{Version: 1, Spec: spec}); err != nil {
			return err
		}
		if err := p.imageState.Create(ctx, seed); err != nil {
			if !state.IsConflictError(err) {
				return provision.NewRetryErrorf(vmRetry, "seed image replacement: details suppressed")
			}
			// A competing publisher may already have submitted the successor.
			// Resolve its immutable marker first; never reset the seed.
			other, readErr := safe.StateGet[*resources.ImageImportReplacement](ctx, p.imageState, resources.NewImageImportReplacement(plan.CacheID).Metadata())
			if readErr == nil {
				if other.TypedSpec().Value.Value != string(raw) {
					return fmt.Errorf("conflicting image replacement marker; no import")
				}
				continue
			}
			if !state.IsNotFoundError(readErr) {
				return provision.NewRetryErrorf(vmRetry, "read competing image replacement: details suppressed")
			}
			existing, readErr := safe.StateGet[*resources.ImageImport](ctx, p.imageState, seed.Metadata())
			if readErr != nil || existing.Metadata().Phase() != resource.PhaseRunning || existing.TypedSpec().Value.Value != seed.TypedSpec().Value.Value {
				return fmt.Errorf("replacement seed conflicts or was already submitted without its marker; no reset")
			}
		}
		if _, err := p.replacementParent(ctx, plan, evidence); err != nil {
			return err
		}
		if err := p.verifyDeletedImage(ctx, evidence, j.Spec.Name); err != nil {
			return err
		}
		marker = resources.NewImageImportReplacement(plan.CacheID)
		marker.TypedSpec().Value.Value = string(raw)
		if err := p.imageState.Create(ctx, marker); err != nil && !state.IsConflictError(err) {
			return provision.NewRetryErrorf(vmRetry, "publish image replacement intent: details suppressed")
		}
		// Read the winning marker including its durable hash/version/timestamp.
	}
}

func (p *Provisioner) loadImageReplacements(ctx context.Context, plan *imageImportPlan) error {
	parent := baseImageImportPlan(plan)
	if parent.Recovery != nil {
		if _, err := p.loadImageRecovery(ctx, &parent); err != nil {
			return err
		}
	}
	for _, ref := range plan.Replacements {
		r, err := safe.StateGet[*resources.ImageImportReplacement](ctx, p.imageState, resources.NewImageImportReplacement(parent.CacheID).Metadata())
		if err != nil || imageResourceHash(r) != ref.ReplacementSHA256 || r.Metadata().Version().String() != ref.ReplacementVersion ||
			r.Metadata().Created().UTC().Format(time.RFC3339Nano) != ref.ReplacementCreated {
			return fmt.Errorf("frozen image replacement intent disappeared or changed; no fallback")
		}
		intent, err := decodeImageReplacement(r, &parent)
		if err != nil {
			return err
		}
		if intent.Parent != ref.Parent {
			return fmt.Errorf("frozen image replacement parent mismatch")
		}
		if _, err := p.replacementParent(ctx, &parent, ref.Parent); err != nil {
			return err
		}
		appendImageReplacement(&parent, r, intent)
	}
	if parent.CacheID != plan.CacheID {
		return fmt.Errorf("frozen image replacement chain identity mismatch")
	}
	return nil
}

func (p *Provisioner) verifyImageReplacementPrism(ctx context.Context, plan *imageImportPlan) error {
	if err := p.loadImageReplacements(ctx, plan); err != nil {
		return err
	}
	parent := baseImageImportPlan(plan)
	for _, ref := range plan.Replacements {
		if err := p.verifyDeletedImage(ctx, ref.Parent, parent.Spec.Name); err != nil {
			return err
		}
		parent.CacheID, parent.Spec = imageReplacementIdentity(parent.Spec, ref.Parent)
	}
	return nil
}
