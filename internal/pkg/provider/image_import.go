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
	"io"
	"slices"
	"strings"
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

const imageImportPlanAnnotation = "nutanix.infra.sidero.dev/image-import-plan"
const imageCacheSeenAnnotation = "nutanix.infra.sidero.dev/image-cache-seen"
const maxImageJournalBytes = 32768

type imageImportPlan struct {
	Version       int                    `json:"version"`
	CacheID       string                 `json:"cache_id"`
	VMClusterUUID string                 `json:"vm_cluster_uuid"`
	SchematicID   string                 `json:"schematic_id"`
	Spec          client.ImageCreateSpec `json:"spec"` // checksum is stored only in the shared journal
	Recovery      *imageRecoveryPlan     `json:"recovery,omitempty"`
	Replacements  []imageReplacementPlan `json:"replacements,omitempty"`
}

type imageImportJournal struct {
	Version   int                    `json:"version"`
	Spec      client.ImageCreateSpec `json:"spec"`
	Submitted bool                   `json:"submitted"`
	TaskUUID  string                 `json:"task_uuid,omitempty"` // Historical field name; opaque GA task extId.
	ImageUUID string                 `json:"image_uuid,omitempty"`
}

// ConfigureImageImport enables the explicitly opted-in path. Shared state is
// mandatory; never rely on an in-memory-only cache for remote image mutations.
func (p *Provisioner) ConfigureImageImport(st state.State, factory ImageFactory) error {
	if st == nil || factory == nil {
		return fmt.Errorf("image import requires durable Omni state and an Image Factory")
	}
	if err := registerImageImportResources(); err != nil {
		return err
	}
	p.imageState, p.imageFactory = st, factory
	return nil
}

func imageCacheIdentity(source string, clusters []string) (string, client.ImageCreateSpec) {
	payload, _ := json.Marshal(struct {
		Contract, Provider, Source string
		Clusters                   []string
	}{"extensions-openstack-qcow2-v1", meta.ProviderID, source, clusters})
	digest := sha256.Sum256(payload)
	key := hex.EncodeToString(digest[:])
	return key, client.ImageCreateSpec{Name: "omni-talos-" + key, Description: "Omni provider " + meta.ProviderID + " image cache " + key,
		SourceURL: source, ClusterUUIDs: slices.Clone(clusters), RequestUUID: uuid.NewSHA1(uuid.NameSpaceURL, payload).String()}
}

func (p *Provisioner) freezeImageImport(ctx context.Context, pctx provision.Context[*resources.Machine], data *Data) error {
	if p.imageState == nil || p.imageFactory == nil {
		return fmt.Errorf("image_import is disabled; enable IMAGE_IMPORT_ENABLED or select a manual image")
	}
	if _, err := p.imageFactory.Source(pctx.GetTalosVersion(), strings.Repeat("0", 64)); err != nil {
		return err
	}
	// VM placement is the sole destination for checkbox-based imports. Historical
	// requests keep their explicit destination set; frozen plans are never retargeted.
	vmCluster, err := p.c.ResolveCluster(ctx, client.Selector{Name: data.ClusterName, UUID: data.ClusterUUID})
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "resolve VM cluster for image import: %w", err)
	}
	if frozen, ok := pctx.State.Metadata().Annotations().Get(clusterAnnotation); ok && frozen != vmCluster {
		return fmt.Errorf("VM cluster identity changed before image import")
	}
	parsed, err := uuid.Parse(vmCluster)
	if err != nil || parsed.String() != vmCluster || parsed == uuid.Nil {
		return fmt.Errorf("VM cluster resolved to an invalid UUID")
	}
	clusters := []string{vmCluster}
	if !data.ImageImport.UseVMCluster {
		clusters = make([]string, 0, len(data.ImageImport.Clusters))
		for i, cluster := range data.ImageImport.Clusters {
			id, err := p.c.ResolveCluster(ctx, client.Selector{Name: cluster.Name, UUID: cluster.UUID})
			if err != nil {
				return provision.NewRetryErrorf(vmRetry, "resolve image destination %d: %w", i, err)
			}
			parsed, err := uuid.Parse(id)
			if err != nil || parsed.String() != id {
				return fmt.Errorf("image destination resolved to an invalid UUID")
			}
			clusters = append(clusters, id)
		}
		slices.Sort(clusters)
		if len(slices.Compact(slices.Clone(clusters))) != len(clusters) {
			return fmt.Errorf("image_import.clusters resolve to duplicate destinations")
		}
		if !slices.Contains(clusters, vmCluster) {
			return fmt.Errorf("image_import.clusters must include the VM target cluster")
		}
	}
	// Explicit request customization and additive class extensions copied into its
	// provider data only; no live MachineClass lookup, SDK logging or connection
	// parameters. Identity remains source/schematic/destinations for old caches too.
	id, err := p.prepareImageSchematic(ctx, pctx, data.InitialExtensions)
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "prepare generic Talos schematic: %w", err)
	}
	source, err := p.imageFactory.Source(pctx.GetTalosVersion(), id)
	if err != nil {
		return err
	}
	key, spec := imageCacheIdentity(source.URL, clusters)
	plan := imageImportPlan{Version: 1, CacheID: key, VMClusterUUID: vmCluster, SchematicID: id, Spec: spec}
	if err := p.selectImageRecovery(ctx, &plan, pctx.GetRequestID()); err != nil {
		return err
	}
	if err := p.selectImageReplacement(ctx, &plan); err != nil {
		return err
	}
	raw, _ := json.Marshal(plan)
	if len(raw) > maxImageJournalBytes {
		return fmt.Errorf("image import plan exceeds annotation limit")
	}
	pctx.State.Metadata().Annotations().Set(clusterAnnotation, vmCluster)
	pctx.State.Metadata().Annotations().Set(imageImportPlanAnnotation, string(raw))
	return provision.NewRetryInterval(time.Millisecond) // persist before image mutation or download
}

func decodeImageJSON(raw string, out any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected trailing image journal data")
	}
	return nil
}

func decodeImagePlan(m *resources.Machine) (*imageImportPlan, error) {
	raw, ok := m.Metadata().Annotations().Get(imageImportPlanAnnotation)
	var plan imageImportPlan
	if !ok || len(raw) > maxImageJournalBytes || decodeImageJSON(raw, &plan) != nil {
		return nil, fmt.Errorf("invalid persisted image import plan")
	}
	key, expected := imageCacheIdentity(plan.Spec.SourceURL, plan.Spec.ClusterUUIDs)
	if err := validateImageRecoveryPlan(&plan, key); err != nil {
		return nil, err
	}
	if plan.Recovery != nil {
		key, expected = imageRecoveryIdentity(expected, plan.Recovery.AttemptUUID)
	}
	key, expected, err := validateImageReplacementPlans(&plan, key, expected)
	if err != nil {
		return nil, err
	}
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(plan.Spec)
	cluster, _ := m.Metadata().Annotations().Get(clusterAnnotation)
	if !strings.Contains(plan.Spec.SourceURL, "/image/"+plan.SchematicID+"/") || len(plan.SchematicID) != 64 {
		return nil, fmt.Errorf("invalid frozen schematic/source identity")
	}
	if plan.CacheID != key || string(a) != string(b) || plan.VMClusterUUID != cluster || !slices.Contains(plan.Spec.ClusterUUIDs, cluster) {
		return nil, fmt.Errorf("persisted image import identity mismatch; operator review required")
	}
	// Validate the otherwise complete immutable spec without inventing a checksum.
	check := plan.Spec
	check.SHA256 = strings.Repeat("0", 64)
	if err := check.Validate(); err != nil {
		return nil, err
	}
	return &plan, nil
}

func decodeImageJournal(r *resources.ImageImport, plan *imageImportPlan) (*imageImportJournal, error) {
	raw := r.TypedSpec().Value.Value
	var journal imageImportJournal
	if r.Metadata().Phase() != resource.PhaseRunning || len(raw) > maxImageJournalBytes || decodeImageJSON(raw, &journal) != nil || journal.Version != 1 {
		return nil, fmt.Errorf("invalid image cache journal; operator review required")
	}
	if err := journal.Spec.Validate(); err != nil {
		return nil, err
	}
	if plan.Recovery != nil && journal.Spec.SHA256 != plan.Recovery.Checksum {
		return nil, fmt.Errorf("image recovery checksum differs from frozen approval")
	}
	if len(plan.Replacements) > 0 && journal.Spec.SHA256 != plan.Replacements[len(plan.Replacements)-1].Parent.Checksum {
		return nil, fmt.Errorf("image replacement checksum differs from completed parent")
	}
	copy := journal.Spec
	copy.SHA256 = ""
	a, _ := json.Marshal(copy)
	b, _ := json.Marshal(plan.Spec)
	if string(a) != string(b) || (!journal.Submitted && (journal.TaskUUID != "" || journal.ImageUUID != "")) {
		return nil, fmt.Errorf("image cache intent mismatch; operator review required")
	}
	if journal.TaskUUID != "" {
		if err := client.ValidateTaskID(journal.TaskUUID); err != nil {
			return nil, err
		}
	}
	if journal.ImageUUID != "" {
		if _, err := uuid.Parse(journal.ImageUUID); err != nil {
			return nil, fmt.Errorf("invalid image cache image UUID")
		}
	}
	return &journal, nil
}

func writeImageJournal(r *resources.ImageImport, journal *imageImportJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil || len(raw) > maxImageJournalBytes {
		return fmt.Errorf("image cache journal exceeds limit")
	}
	r.TypedSpec().Value.Value = string(raw)
	return nil
}

func verifyImportedImage(img *client.Image, spec client.ImageCreateSpec) error {
	if img == nil {
		return fmt.Errorf("image cache ownership/source/checksum mismatch (fields: image); refusing reuse or mutation")
	}
	// Only fixed field names reach errors. Never echo Prism values, descriptions,
	// source URLs (which may contain credentials), checksums or response bodies.
	fields := []string{}
	for _, check := range []struct {
		name string
		ok   bool
	}{
		{"extId", img.UUID != ""},
		{"name", img.Name == spec.Name},
		{"type", img.Type == "DISK_IMAGE"},
		{"description", img.Description == spec.Description},
		{"source.$objectType", img.SourceType == "vmm.v4.content.UrlSource"},
		{"source.url", img.SourceURL == spec.SourceURL},
		{"checksum.hexDigest", img.SHA256 == spec.SHA256},
	} {
		if !check.ok {
			fields = append(fields, check.name)
		}
	}
	if len(fields) > 0 {
		return fmt.Errorf("image cache ownership/source/checksum mismatch (fields: %s); refusing reuse or mutation", strings.Join(fields, ", "))
	}
	if _, err := uuid.Parse(img.UUID); err != nil {
		return fmt.Errorf("invalid imported image UUID")
	}
	return nil
}

func (p *Provisioner) ensureImportedImage(ctx context.Context, pctx provision.Context[*resources.Machine]) error {
	if p.imageState == nil || p.imageFactory == nil {
		return fmt.Errorf("image import is disabled; frozen intent retained")
	}
	plan, err := decodeImagePlan(pctx.State)
	if err != nil {
		return err
	}
	p.imageMu.Lock()
	defer p.imageMu.Unlock()
	cache := resources.NewImageImport(plan.CacheID)
	if len(plan.Replacements) > 0 {
		if err := p.loadImageReplacements(ctx, plan); err != nil {
			return err
		}
	} else if plan.Recovery != nil {
		if _, err := p.loadImageRecovery(ctx, plan); err != nil {
			return err
		}
	}
	r, err := safe.StateGet[*resources.ImageImport](ctx, p.imageState, cache.Metadata())
	if state.IsNotFoundError(err) {
		if len(plan.Replacements) > 0 {
			return fmt.Errorf("image replacement journal disappeared; operator review required; no automatic recreation")
		}
		if plan.Recovery != nil {
			return fmt.Errorf("approved recovery journal disappeared; operator review required; no automatic reimport")
		}
		if _, seen := pctx.State.Metadata().Annotations().Get(imageCacheSeenAnnotation); seen || pctx.State.TypedSpec().Value.ImageUuid != "" {
			return fmt.Errorf("image cache journal disappeared; operator review required; no automatic reimport")
		}
		checksum, err := p.imageFactory.Checksum(ctx, ImageSource{URL: plan.Spec.SourceURL, SchematicID: plan.SchematicID})
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "hash Talos image: %w", err)
		}
		journal := imageImportJournal{Version: 1, Spec: plan.Spec}
		journal.Spec.SHA256 = checksum
		if err := journal.Spec.Validate(); err != nil {
			return err
		}
		if err := writeImageJournal(cache, &journal); err != nil {
			return err
		}
		if err := p.imageState.Create(ctx, cache); err != nil && !state.IsConflictError(err) {
			return provision.NewRetryErrorf(vmRetry, "persist image import intent: details suppressed")
		}
		pctx.State.Metadata().Annotations().Set(imageCacheSeenAnnotation, plan.CacheID)
		return provision.NewRetryInterval(time.Millisecond)
	}
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "read image cache journal: details suppressed")
	}
	journal, err := decodeImageJournal(r, plan)
	if err != nil {
		return err
	}
	if seen, ok := pctx.State.Metadata().Annotations().Get(imageCacheSeenAnnotation); !ok {
		pctx.State.Metadata().Annotations().Set(imageCacheSeenAnnotation, plan.CacheID)
		return provision.NewRetryInterval(time.Millisecond)
	} else if seen != plan.CacheID {
		return fmt.Errorf("image cache reference changed; operator review required")
	}
	if journal.TaskUUID != "" && journal.ImageUUID == "" {
		result, err := p.c.WaitTask(ctx, journal.TaskUUID)
		if result.VMUUID != "" {
			return fmt.Errorf("image creation task unexpectedly references a VM; operator review required")
		}
		if result.State == client.TaskFailed {
			return fmt.Errorf("Talos image creation task failed; inspect Prism Central; no automatic reimport")
		}
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "image creation task outcome unknown; details suppressed")
		}
		if result.State != client.TaskSucceeded {
			return provision.NewRetryInterval(taskRetry)
		}
	}
	var img *client.Image
	if journal.ImageUUID != "" {
		img, err = p.c.GetImage(ctx, journal.ImageUUID)
	} else {
		img, err = p.c.FindImage(ctx, journal.Spec.Name)
	}
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "discover cached image: %w", err)
	}
	if img == nil {
		if journal.Submitted {
			return provision.NewRetryErrorf(vmRetry, "image submission outcome unresolved; waiting for exact image; operator review may be required; no resubmission")
		}
		if len(plan.Replacements) > 0 {
			checksum, err := p.imageFactory.Checksum(ctx, ImageSource{URL: plan.Spec.SourceURL, SchematicID: plan.SchematicID})
			if err != nil {
				return provision.NewRetryErrorf(vmRetry, "hash replacement Talos image: details suppressed")
			}
			if checksum != journal.Spec.SHA256 {
				return fmt.Errorf("image replacement source checksum changed; no import")
			}
			if err := p.verifyImageReplacementPrism(ctx, plan); err != nil {
				return err
			}
		} else if plan.Recovery != nil {
			checksum, err := p.imageFactory.Checksum(ctx, ImageSource{URL: plan.Spec.SourceURL, SchematicID: plan.SchematicID})
			if err != nil {
				return provision.NewRetryErrorf(vmRetry, "hash recovery Talos image: %w", err)
			}
			if checksum != plan.Recovery.Checksum {
				return fmt.Errorf("image recovery source checksum changed; no import")
			}
			approval, err := p.loadImageRecovery(ctx, plan)
			if err != nil {
				return err
			}
			if err := verifyImageRecoveryPrism(ctx, p.c, approval); err != nil {
				return err
			}
		}
		// Claim the shared journal atomically BEFORE POST. On a lost response or
		// restart, nobody sends a second mutation, even after request-ID expiry.
		claimed := false
		_, err := safe.StateUpdateWithConflicts(ctx, p.imageState, r.Metadata(), func(current *resources.ImageImport) error {
			claimed = false
			j, err := decodeImageJournal(current, plan)
			if err != nil {
				return err
			}
			if j.Spec.SHA256 != journal.Spec.SHA256 {
				return fmt.Errorf("immutable image checksum journal changed")
			}
			if j.Submitted {
				return nil
			}
			j.Submitted = true
			if err := writeImageJournal(current, j); err != nil {
				return err
			}
			claimed = true
			return nil
		})
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "claim image import: details suppressed")
		}
		if !claimed {
			return provision.NewRetryInterval(vmRetry)
		}
		if len(plan.Replacements) > 0 {
			if err := p.verifyImageReplacementPrism(ctx, plan); err != nil {
				return err // The claim stays consumed; never reset/rotate after drift.
			}
		} else if plan.Recovery != nil {
			// Recheck after winning the claim as well. Any approval/task/image
			// drift consumes this attempt safely rather than authorizing a POST.
			approval, err := p.loadImageRecovery(ctx, plan)
			if err != nil {
				return err
			}
			if err := verifyImageRecoveryPrism(ctx, p.c, approval); err != nil {
				return err
			}
		}
		task, err := p.c.CreateImage(ctx, journal.Spec)
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "image submission outcome unknown; intent retained, no resubmission: %w", err)
		}
		if err := client.ValidateTaskID(task); err != nil {
			return provision.NewRetryErrorf(vmRetry, "image submission returned an invalid task; intent retained, no resubmission")
		}
		_, err = safe.StateUpdateWithConflicts(ctx, p.imageState, r.Metadata(), func(current *resources.ImageImport) error {
			j, err := decodeImageJournal(current, plan)
			if err != nil {
				return err
			}
			if j.Spec.SHA256 != journal.Spec.SHA256 {
				return fmt.Errorf("immutable image checksum journal changed")
			}
			if !j.Submitted || j.TaskUUID != "" && j.TaskUUID != task {
				return fmt.Errorf("image task journal conflict")
			}
			j.TaskUUID = task
			return writeImageJournal(current, j)
		})
		if err != nil {
			return provision.NewRetryErrorf(vmRetry, "persist image task outcome: details suppressed; no resubmission")
		}
		return provision.NewRetryInterval(taskRetry)
	}
	if err := verifyImportedImage(img, journal.Spec); err != nil {
		return err
	}
	if img.Status == client.ImageStatusFailed {
		return fmt.Errorf("cached Talos image failed; no automatic reimport")
	}
	if img.Status != client.ImageStatusAvailable || img.SizeBytes <= 0 {
		return provision.NewRetryInterval(vmRetry)
	}
	for _, cluster := range journal.Spec.ClusterUUIDs {
		if !slices.Contains(img.ClusterUUIDs, cluster) {
			return provision.NewRetryErrorf(vmRetry, "Talos image not yet available at every selected cluster")
		}
	}
	data, err := providerData(pctx)
	if err != nil {
		return err
	}
	if data.DiskGB*(1<<30) < img.SizeBytes {
		return fmt.Errorf("disk_gb is smaller than the imported Talos image")
	}
	if frozen := pctx.State.TypedSpec().Value.ImageUuid; frozen != "" && frozen != img.UUID {
		return fmt.Errorf("frozen image UUID mismatch; refusing reselection")
	}
	_, err = safe.StateUpdateWithConflicts(ctx, p.imageState, r.Metadata(), func(current *resources.ImageImport) error {
		j, err := decodeImageJournal(current, plan)
		if err != nil {
			return err
		}
		if j.Spec.SHA256 != journal.Spec.SHA256 {
			return fmt.Errorf("immutable image checksum journal changed")
		}
		if j.ImageUUID != "" && j.ImageUUID != img.UUID {
			return fmt.Errorf("cached image identity changed")
		}
		j.Submitted, j.ImageUUID = true, img.UUID
		return writeImageJournal(current, j)
	})
	if err != nil {
		return provision.NewRetryErrorf(vmRetry, "freeze imported image identity: details suppressed")
	}
	pctx.State.TypedSpec().Value.ImageUuid = img.UUID
	return nil
}
