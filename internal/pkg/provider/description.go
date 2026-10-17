// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/base64"
	"fmt"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const descriptionAnnotation = "nutanix.infra.sidero.dev/vm-description"

// The caller persists this snapshot together with creation intent before POST.
// Base64 preserves literal multiline/Unicode text without annotation controls.
// Already-created/recovered VMs never enter this path; no description updates or
// backfill are performed. Older submitted payloads continue omitting the field.
func creationDescription(machine *resources.Machine, requested string) (string, error) {
	if snapshot, present := machine.Metadata().Annotations().Get(descriptionAnnotation); present {
		if len(snapshot) > 16*1024 {
			return "", fmt.Errorf("persisted VM description exceeds annotation limit")
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(snapshot)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != snapshot {
			return "", fmt.Errorf("invalid persisted VM description encoding; contents suppressed")
		}
		description := string(raw)
		if err := client.ValidateVMDescription(description); err != nil {
			return "", fmt.Errorf("invalid persisted VM description: %w", err)
		}
		return description, nil // Replays never replace a saved value, including empty.
	}
	if _, submitted := machine.Metadata().Annotations().Get(createIntentAnnotation); submitted {
		if requested != "" {
			return "", fmt.Errorf("VM description snapshot missing after creation intent; refusing description backfill")
		}
		return "", nil // Preserve earlier submitted payload and annotations unchanged.
	}
	if err := client.ValidateVMDescription(requested); err != nil {
		return "", err
	}
	machine.Metadata().Annotations().Set(descriptionAnnotation, base64.StdEncoding.EncodeToString([]byte(requested)))
	return requested, nil
}
