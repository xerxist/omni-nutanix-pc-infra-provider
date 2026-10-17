// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const healthCheckTimeout = 15 * time.Second
const maxHealthResponseBytes = 64 << 10

// healthResponse verifies a bounded GA list envelope without retaining inventory.
// Empty inventories are healthy; absent/null/error data is not a successful read.
type healthResponse struct {
	valid               bool
	allowContainerExtID bool
}

func (r *healthResponse) UnmarshalJSON(raw []byte) error {
	r.valid = false
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Data) == 0 || response.Data[0] != '[' {
		return fmt.Errorf("invalid health response")
	}
	var items []json.RawMessage
	if json.Unmarshal(response.Data, &items) != nil || len(items) > 1 {
		return fmt.Errorf("invalid health response")
	}
	for _, item := range items {
		var entity struct {
			ExtID          string `json:"extId"`
			ContainerExtID string `json:"containerExtId"`
		}
		if json.Unmarshal(item, &entity) != nil {
			return fmt.Errorf("invalid health response")
		}
		// GA StorageContainer uses containerExtId. Match provisioning's extId
		// compatibility without accepting container IDs for unrelated APIs.
		if entity.ExtID == "" && (!r.allowContainerExtID || entity.ContainerExtID == "") {
			return fmt.Errorf("invalid health response")
		}
	}
	r.valid = true
	return nil
}

// CheckHealth shares the client's existing authentication, TLS roots, GA namespace
// versions, redirect refusal and bounded GET retries. It is reporting only: no
// writes, task polling, capacity reservation or provisioning/teardown gate.
func (c *v4Client) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	for _, probe := range []struct {
		name, path          string
		allowContainerExtID bool
	}{
		{"cluster discovery", c.cluster + "/clusters", false},
		{"storage-container discovery", c.cluster + "/storage-containers", true},
		{"subnet discovery", c.networking + "/subnets", false},
		{"image discovery", c.vmm + "/content/images", false},
		{"VM discovery", c.vmm + "/ahv/config/vms", false},
	} {
		if ctx.Err() != nil {
			return fmt.Errorf("Prism Central health: %s interrupted or timed out", probe.name)
		}
		response := healthResponse{allowContainerExtID: probe.allowContainerExtID}
		_, err := c.doWithResponseLimit(ctx, http.MethodGet, probe.path+"?$limit=1&$page=0", nil, &response, nil, maxHealthResponseBytes)
		if err == nil && response.valid {
			continue
		}
		// Only fixed probe names/status codes reach Omni. Never wrap transport or
		// decode errors: they may contain credentials, URLs or echoed response data.
		var status *httpError
		if errors.As(err, &status) {
			return fmt.Errorf("Prism Central health: %s failed (HTTP %d)", probe.name, status.status)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("Prism Central health: %s interrupted or timed out", probe.name)
		}
		return fmt.Errorf("Prism Central health: %s unavailable or invalid response; details suppressed", probe.name)
	}
	return nil
}
