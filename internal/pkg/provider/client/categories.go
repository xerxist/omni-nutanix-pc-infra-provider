// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Validate rejects incomplete selectors before any remote operation.
func (s CategorySelector) Validate() error {
	if s.UUID == "" && (strings.TrimSpace(s.Key) == "" || strings.TrimSpace(s.Value) == "") {
		return fmt.Errorf("category requires uuid or both key and value")
	}
	if (s.Key == "") != (s.Value == "") || (s.Key != "" && (strings.TrimSpace(s.Key) == "" || strings.TrimSpace(s.Value) == "")) {
		return fmt.Errorf("category key and value must both be nonempty when supplied")
	}
	if s.UUID != "" {
		if _, err := uuid.Parse(s.UUID); err != nil {
			return fmt.Errorf("category uuid must be a UUID")
		}
	}
	return nil
}

type category struct {
	UUID  string `json:"extId"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ResolveCategories only reads existing definitions. It never creates categories
// or security policies, and fails rather than silently omitting a missing category.
func (c *v4Client) ResolveCategories(ctx context.Context, selectors []CategorySelector) ([]string, error) {
	if len(selectors) > 256 {
		return nil, fmt.Errorf("at most 256 categories are allowed per VM")
	}
	for i, selector := range selectors {
		if err := selector.Validate(); err != nil {
			return nil, fmt.Errorf("categories[%d]: %w", i, err)
		}
	}
	ids := make([]string, 0, len(selectors))
	seen := make(map[string]bool)
	for i, selector := range selectors {
		id, err := c.resolveCategory(ctx, selector)
		if err != nil {
			return nil, fmt.Errorf("categories[%d]: %w", i, err)
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

func (c *v4Client) resolveCategory(ctx context.Context, selector CategorySelector) (string, error) {
	path := c.prism + "/categories"
	var candidates []category
	if selector.UUID != "" {
		var response envelope[category]
		if _, err := c.do(ctx, http.MethodGet, path+"/"+url.PathEscape(selector.UUID), nil, &response, nil); err != nil {
			return "", err
		}
		candidates = []category{response.Data}
	} else {
		filter := "key eq '" + strings.ReplaceAll(selector.Key, "'", "''") + "' and value eq '" + strings.ReplaceAll(selector.Value, "'", "''") + "'"
		var err error
		candidates, err = list[category](ctx, c, path, filter)
		if err != nil {
			return "", err
		}
	}
	var found string
	for _, candidate := range candidates {
		id, err := uuid.Parse(candidate.UUID)
		if err != nil {
			return "", fmt.Errorf("category response has no valid extId")
		}
		if selector.UUID != "" && !strings.EqualFold(candidate.UUID, selector.UUID) {
			continue
		}
		if selector.Key != "" && (candidate.Key != selector.Key || candidate.Value != selector.Value) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("ambiguous category %q:%q; use uuid", selector.Key, selector.Value)
		}
		found = id.String()
	}
	if found == "" {
		return "", fmt.Errorf("category uuid=%q key=%q value=%q not found or does not match; create it in Prism Central first", selector.UUID, selector.Key, selector.Value)
	}
	return found, nil
}
