// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
)

const defaultFactoryEndpoint = "https://factory.talos.dev"
const maxFactoryImageBytes int64 = 8 << 30

type ImageSource struct{ URL, SchematicID, SHA256 string }

// ImageFactory never receives connection parameters, join config or credentials.
type ImageFactory interface {
	provision.FactoryClient
	Source(string, string) (ImageSource, error)
	Checksum(context.Context, ImageSource) (string, error)
}

type httpImageFactory struct {
	endpoint string
	http     *http.Client
}

func NewImageFactory(endpoint, caFile string) (ImageFactory, error) {
	if endpoint == "" {
		endpoint = defaultFactoryEndpoint
	}
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, fmt.Errorf("Image Factory endpoint must be a credential-free HTTPS URL")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read Image Factory CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("Image Factory CA contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	// Schematic uploads never redirect. Image downloads use a separate, narrowly
	// scoped redirect policy; PC still receives the original TLS-verified URL.
	return &httpImageFactory{endpoint: u.String(), http: &http.Client{Transport: transport, Timeout: 10 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (f *httpImageFactory) Source(version, schematicID string) (ImageSource, error) {
	version = strings.TrimPrefix(version, "v")
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[a-z0-9]+([.-][a-z0-9]+)*)?$`).MatchString(version) {
		return ImageSource{}, fmt.Errorf("automatic image import requires a canonical Talos version")
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(schematicID) {
		return ImageSource{}, fmt.Errorf("invalid generic schematic ID")
	}
	return ImageSource{URL: f.endpoint + "/image/" + schematicID + "/v" + version + "/openstack-amd64.qcow2", SchematicID: schematicID}, nil
}

func (f *httpImageFactory) EnsureSchematic(ctx context.Context, s schematic.Schematic) (string, error) {
	var err error
	s, err = normalizeSharedSchematic(s)
	if err != nil {
		return "", err
	}
	id, err := s.ID()
	if err != nil {
		return "", fmt.Errorf("calculate generic schematic ID")
	}
	payload, err := s.Marshal()
	if err != nil {
		return "", fmt.Errorf("encode generic schematic")
	}
	apiCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(apiCtx, http.MethodPost, f.endpoint+"/schematics", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("construct Image Factory request")
	}
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := f.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Image Factory schematic request failed; details suppressed")
	}
	var result struct {
		ID string `json:"id"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated || decodeErr != nil || result.ID != id {
		return "", fmt.Errorf("Image Factory schematic response invalid (HTTP %d); details suppressed", resp.StatusCode)
	}
	return id, nil
}

func (f *httpImageFactory) Checksum(ctx context.Context, source ImageSource) (string, error) {
	u, err := url.Parse(source.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return "", fmt.Errorf("invalid frozen image URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return "", fmt.Errorf("construct image download request")
	}
	req.Header.Set("Accept-Encoding", "identity")
	// Clone per download, leaving concurrent schematic uploads and the shared
	// client policy unchanged. Signed asset URLs are transient, never persisted.
	downloadClient := *f.http
	downloadClient.Jar = nil
	downloadClient.CheckRedirect = factoryImageRedirect
	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Image Factory download failed; details suppressed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxFactoryImageBytes || resp.Header.Get("Content-Encoding") != "" {
		return "", fmt.Errorf("Image Factory download refused (HTTP %d); details suppressed", resp.StatusCode)
	}
	// QCOW2 magic is checked without retaining any image contents on disk.
	prefix := make([]byte, 72)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil || !bytes.Equal(prefix[:4], []byte{'Q', 'F', 'I', 0xfb}) {
		return "", fmt.Errorf("Image Factory returned no QCOW2 disk image")
	}
	version := binary.BigEndian.Uint32(prefix[4:8])
	clusterBits := binary.BigEndian.Uint32(prefix[20:24])
	if version != 2 && version != 3 || clusterBits < 9 || clusterBits > 21 || binary.BigEndian.Uint64(prefix[24:32]) == 0 {
		return "", fmt.Errorf("Image Factory returned an invalid QCOW2 header")
	}
	hash := sha256.New()
	hash.Write(prefix)
	n, err := io.Copy(hash, io.LimitReader(resp.Body, maxFactoryImageBytes-71))
	if err != nil || n+72 > maxFactoryImageBytes || resp.ContentLength >= 0 && n+72 != resp.ContentLength {
		return "", fmt.Errorf("Image Factory download incomplete or exceeds 8 GiB; details suppressed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// factoryImageRedirect permits only one GET redirect from a canonical public
// Factory image to its exact HTTPS asset origin. Custom factories, additional
// hops, nonstandard ports, credentials and downgrades remain fail-closed.
func factoryImageRedirect(req *http.Request, via []*http.Request) error {
	if req == nil || req.URL == nil || len(via) != 1 || via[0] == nil || via[0].URL == nil {
		return http.ErrUseLastResponse
	}
	from, to := via[0].URL, req.URL
	if via[0].Method != http.MethodGet || req.Method != http.MethodGet ||
		from.Scheme != "https" || from.Host != "factory.talos.dev" || from.User != nil || from.RawQuery != "" || from.Fragment != "" || from.RawPath != "" ||
		!regexp.MustCompile(`^/image/[a-f0-9]{64}/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[a-z0-9]+([.-][a-z0-9]+)*)?/openstack-amd64\.qcow2$`).MatchString(from.Path) ||
		to.Scheme != "https" || to.Host != "assets.factory.talos.dev" || to.User != nil || to.Fragment != "" || to.Opaque != "" {
		return http.ErrUseLastResponse
	}
	// The asset query may contain a short-lived signature. Forward no credentials,
	// cookies or referrer, and never include the query in logs or errors.
	req.Header = make(http.Header)
	req.Header.Set("Accept-Encoding", "identity")
	return nil
}
