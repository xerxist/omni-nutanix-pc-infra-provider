// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"fmt"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/resources"
)

const bootModeAnnotation = "nutanix.infra.sidero.dev/boot-mode"
const uefiCreateIntent = "uefi-v1"

// The intent contract distinguishes a new UEFI submission from historical BIOS
// state even if its boot annotation disappears. Never change a replay body under
// an already-submitted request UUID, and never update the boot mode of a live VM.
func creationBootMode(m *resources.Machine) (string, error) {
	intent, submitted := m.Metadata().Annotations().Get(createIntentAnnotation)
	mode, frozen := m.Metadata().Annotations().Get(bootModeAnnotation)
	if submitted {
		switch intent {
		case "true":
			if frozen && mode != client.BootModeLegacy {
				return "", fmt.Errorf("historical BIOS creation intent cannot be changed to UEFI")
			}
			return client.BootModeLegacy, nil
		case uefiCreateIntent:
			if !frozen || mode != client.BootModeUEFI {
				return "", fmt.Errorf("UEFI boot snapshot missing or changed after creation intent; refusing replay")
			}
			return mode, nil
		default:
			return "", fmt.Errorf("unknown VM boot creation contract; refusing replay")
		}
	}
	if frozen && mode != client.BootModeUEFI {
		return "", fmt.Errorf("invalid boot snapshot for a new VM; UEFI is required")
	}
	m.Metadata().Annotations().Set(bootModeAnnotation, client.BootModeUEFI)
	return client.BootModeUEFI, nil
}
