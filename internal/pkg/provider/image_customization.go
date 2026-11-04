// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	infrares "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	talosmeta "github.com/siderolabs/talos/pkg/machinery/meta"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const maxSharedSchematicBytes = 64 << 10

var (
	imageKernelKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	imageExtensionPattern    = regexp.MustCompile(`^siderolabs/[a-z0-9][a-z0-9-]*$`)
	imageOverlayNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
	imageOverlayImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?/(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`)
)

// prepareImageSchematic reads the owned request before the first plan is frozen.
// Build only explicit request customization plus the validated initial extensions
// from its provider data, never ConnectionParams or a mutable MachineClass. Unlike
// the SDK helper, this validates META keys before narrowing uint32 to uint8, and
// preserves kernel-argument order (which may affect repeated options). Historical
// frozen plans never call this function and their source/cache identity is unchanged.
func (p *Provisioner) prepareImageSchematic(ctx context.Context, pctx provision.Context[*resources.Machine], initialExtensions []string) (string, error) {
	r, err := safe.StateGet[*infrares.MachineRequest](ctx, p.imageState, infrares.NewMachineRequest(pctx.GetRequestID()).Metadata())
	if err != nil {
		return "", fmt.Errorf("read image customization request; details suppressed")
	}
	provider, _ := r.Metadata().Labels().Get(omnires.LabelInfraProviderID)
	v := r.TypedSpec().Value
	if provider != meta.ProviderID || r.Metadata().Phase() != resource.PhaseRunning || v.TalosVersion != pctx.GetTalosVersion() {
		return "", fmt.Errorf("image customization request scope or version changed")
	}
	if len(v.KernelArgs) > 256 || len(v.MetaValues) > 256 || len(v.Extensions) > 256 || len(initialExtensions) > 256 {
		return "", fmt.Errorf("image customization exceeds item limits")
	}
	// Additive only: never override Omni's requested extensions. Bound both input
	// lists before deduplication and the resulting union before factory upload.
	extensions := append(slices.Clone(v.Extensions), initialExtensions...)
	slices.Sort(extensions)
	extensions = slices.Compact(extensions)
	if len(extensions) > 256 {
		return "", fmt.Errorf("combined initial and requested extensions exceed 256 entries")
	}
	s := schematic.Schematic{Customization: schematic.Customization{
		ExtraKernelArgs:  slices.Clone(v.KernelArgs),
		SystemExtensions: schematic.SystemExtensions{OfficialExtensions: extensions},
	}}
	for _, value := range v.MetaValues {
		if value == nil || value.Key > 255 {
			return "", fmt.Errorf("image META keys must be non-null integers from 0 to 255")
		}
		s.Customization.Meta = append(s.Customization.Meta, schematic.MetaValue{Key: uint8(value.Key), Value: value.Value})
	}
	if v.Overlay != nil {
		s.Overlay = schematic.Overlay{Name: v.Overlay.Name, Image: v.Overlay.Image}
	}
	// Validate even with alternate factories, without logging/echoing custom data.
	s, err = normalizeSharedSchematic(s)
	if err != nil {
		return "", err
	}
	return p.imageFactory.EnsureSchematic(ctx, s)
}

// normalizeSharedSchematic keeps extensions-only IDs compatible while allowing
// explicit shared-image customizations. No unique token/UUID, join configuration,
// alternate platform/bootloader or Secure Boot changes may enter a shared image.
func normalizeSharedSchematic(s schematic.Schematic) (schematic.Schematic, error) {
	if s.Owner != "" || s.Customization.EmbeddedMachineConfiguration != "" {
		return s, fmt.Errorf("automatic import requires a public shared schematic without embedded machine configuration")
	}
	if s.Customization.DiskImage.SectorSize != 0 {
		return s, fmt.Errorf("automatic import does not support disk image customization")
	}
	if s.Customization.Bootloader != 0 || s.Customization.SecureBoot.IncludeWellKnownCertificates || s.Customization.SecureBoot.EnrollKeys != "" {
		return s, fmt.Errorf("automatic import does not support bootloader or Secure Boot customization")
	}
	if len(s.Customization.ExtraKernelArgs) > 256 || len(s.Customization.Meta) > 256 || len(s.Customization.SystemExtensions.OfficialExtensions) > 256 {
		return s, fmt.Errorf("image customization exceeds item limits")
	}
	for _, arg := range s.Customization.ExtraKernelArgs {
		if arg == "" || len(arg) > 4096 || !utf8.ValidString(arg) || strings.IndexFunc(arg, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return s, fmt.Errorf("image kernel arguments must be bounded individual nonempty tokens")
		}
		key, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !imageKernelKeyPattern.MatchString(key) {
			return s, fmt.Errorf("image kernel argument has an invalid key")
		}
		for _, reserved := range []string{"siderolink", "talos.config", "talos.meta", "talos.platform", "talos.events", "talos.logging", "talos.experimental.wipe"} {
			if key == reserved || strings.HasPrefix(key, reserved+".") {
				return s, fmt.Errorf("image kernel argument overrides reserved connection, metadata or platform settings")
			}
		}
	}
	seen := map[uint8]bool{}
	for _, value := range s.Customization.Meta {
		if seen[value.Key] || len(value.Value) > maxSharedSchematicBytes || !utf8.ValidString(value.Value) || strings.ContainsRune(value.Value, '\x00') {
			return s, fmt.Errorf("image META values must have unique keys and valid text")
		}
		seen[value.Key] = true
		switch value.Key {
		case talosmeta.Upgrade, talosmeta.StagedUpgradeImageRef, talosmeta.StagedUpgradeInstallOptions,
			talosmeta.StateEncryptionConfig, talosmeta.DownloadURLCode, talosmeta.UUIDOverride,
			talosmeta.UniqueMachineToken, talosmeta.DiskImageBootloader:
			return s, fmt.Errorf("image META key is reserved for machine identity, credentials or lifecycle state")
		}
	}
	extensions := slices.Clone(s.Customization.SystemExtensions.OfficialExtensions)
	slices.Sort(extensions)
	extensions = slices.Compact(extensions)
	for _, ext := range extensions {
		if !imageExtensionPattern.MatchString(ext) {
			return s, fmt.Errorf("automatic image import requires official siderolabs extension names")
		}
	}
	s.Customization.SystemExtensions.OfficialExtensions = extensions
	if s.Overlay.Name != "" || s.Overlay.Image != "" || len(s.Overlay.Options) != 0 {
		if len(s.Overlay.Options) != 0 {
			return s, fmt.Errorf("automatic import supports Omni overlay name/image fields only, not overlay options")
		}
		if !imageOverlayNamePattern.MatchString(s.Overlay.Name) || len(s.Overlay.Image) > 512 ||
			!imageOverlayImagePattern.MatchString(s.Overlay.Image) {
			return s, fmt.Errorf("image overlay requires a valid name and credential-free OCI image tag or SHA256 digest")
		}
	}
	raw, err := s.Marshal()
	if err != nil || len(raw) > maxSharedSchematicBytes {
		return s, fmt.Errorf("image customization cannot be encoded or exceeds 64 KiB")
	}
	return s, nil
}
