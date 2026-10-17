// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
)

const guestTrustName = "nutanix-provider-bootstrap"

// Omni v1.12.4 limits each annotation value to 16KiB and rejects control
// characters, including YAML newlines. Persist only canonical single-line base64.
const maxGuestTrustSnapshotBytes = 16 * 1024

// NewProvisionerWithGuestCA configures optional initial and ongoing Talos trust.
// The public bundle must contain only CA certificates, never private keys.
func NewProvisionerWithGuestCA(c client.Client, bundle []byte) (*Provisioner, error) {
	p := NewProvisioner(c)
	if len(bundle) == 0 {
		return p, nil
	}
	patch, err := guestTrustPatch(bundle)
	if err != nil {
		return nil, err
	}
	if _, err := encodeGuestTrustSnapshot(string(patch)); err != nil {
		return nil, err
	}
	p.guestTrustPatch = string(patch)
	return p, nil
}

func encodeGuestTrustSnapshot(patch string) (string, error) {
	if base64.StdEncoding.EncodedLen(len(patch)) > maxGuestTrustSnapshotBytes {
		return "", fmt.Errorf("encoded guest trust configuration exceeds Omni's 16KiB annotation limit")
	}
	return base64.StdEncoding.EncodeToString([]byte(patch)), nil
}

func decodeGuestTrustSnapshot(snapshot string) (string, error) {
	if len(snapshot) > maxGuestTrustSnapshotBytes {
		return "", fmt.Errorf("persisted guest trust configuration exceeds Omni's 16KiB annotation limit")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(snapshot)
	if err != nil || base64.StdEncoding.EncodeToString(data) != snapshot {
		// Never include annotation contents in errors.
		return "", fmt.Errorf("persisted guest trust configuration is not canonical base64")
	}
	return string(data), nil
}

func guestTrustPatch(bundle []byte) ([]byte, error) {
	if len(bundle) > 1<<20 {
		return nil, fmt.Errorf("guest CA bundle exceeds 1MiB")
	}
	rest := bytes.TrimSpace(bundle)
	var normalized []byte
	for len(rest) > 0 {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, fmt.Errorf("guest CA bundle must contain only PEM CA certificates")
		}
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fmt.Errorf("guest CA bundle must contain only PEM CA certificates")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return nil, fmt.Errorf("guest CA bundle contains an invalid or non-CA certificate")
		}
		normalized = append(normalized, pem.EncodeToMemory(block)...)
		rest = bytes.TrimSpace(tail)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("guest CA bundle contains no CA certificates")
	}
	doc := security.NewTrustedRootsConfigV1Alpha1()
	doc.MetaName = guestTrustName
	doc.Certificates = string(normalized)
	cfg, err := container.New(doc)
	if err != nil {
		return nil, fmt.Errorf("construct guest TrustedRootsConfig")
	}
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

func joinConfigWithGuestTrust(joinConfig, patch string) (string, error) {
	if patch == "" {
		return joinConfig, nil
	}
	join, err := configloader.NewFromBytes([]byte(joinConfig))
	if err != nil {
		// Parser errors can include the raw join token: never propagate them.
		return "", fmt.Errorf("cannot parse Omni join configuration for guest CA injection (details suppressed)")
	}
	trust, err := configloader.NewFromBytes([]byte(patch))
	if err != nil {
		return "", fmt.Errorf("cannot parse persisted guest trust configuration")
	}
	documents := append(join.Documents(), trust.Documents()...)
	combined, err := container.New(documents...)
	if err != nil {
		return "", fmt.Errorf("guest trust conflicts with Omni join configuration (details suppressed)")
	}
	data, err := combined.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return "", fmt.Errorf("cannot encode guest join configuration (details suppressed)")
	}
	return string(data), nil
}
