// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

func (s ImageCreateSpec) Validate() error {
	u, err := url.Parse(s.SourceURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("image source must be a credential-free HTTPS URL")
	}
	if s.Name == "" || len(s.Name) > 256 || len(s.Description) > 1000 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(s.SHA256) {
		return fmt.Errorf("invalid image identity or SHA256 checksum")
	}
	if _, err := uuid.Parse(s.RequestUUID); err != nil {
		return fmt.Errorf("invalid image request UUID")
	}
	if len(s.ClusterUUIDs) == 0 || len(s.ClusterUUIDs) > 200 {
		return fmt.Errorf("explicit image destination clusters are required")
	}
	previous := ""
	for _, id := range s.ClusterUUIDs {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || strings.Compare(previous, id) >= 0 {
			return fmt.Errorf("image clusters must be canonical, sorted, unique UUIDs")
		}
		previous = id
	}
	return nil
}

// CreateImage uses GA createImage with UrlSource, not PE legacy-image import.
func (c *v4Client) CreateImage(ctx context.Context, spec ImageCreateSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	body := map[string]any{
		"name": spec.Name, "description": spec.Description, "type": "DISK_IMAGE",
		"source":                map[string]any{"$objectType": "vmm.v4.content.UrlSource", "url": spec.SourceURL, "shouldAllowInsecureUrl": false},
		"checksum":              map[string]any{"$objectType": "vmm.v4.content.ImageSha256Checksum", "hexDigest": spec.SHA256},
		"clusterLocationExtIds": spec.ClusterUUIDs,
	}
	return c.mutation(ctx, http.MethodPost, c.vmm+"/content/images", body, spec.RequestUUID, "")
}
