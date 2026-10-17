// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ImageTaskInfo carries exact task/entity/family identities for explicit recovery.
// No localized operation descriptions, error arguments or response bodies escape.
type ImageTaskInfo struct {
	ID           string
	State        TaskState
	Entities     []ImageTaskEntity
	FamilyIDs    []string
	ClusterUUIDs []string
	CreatedAt    time.Time
	CompletedAt  time.Time
}

type ImageTaskEntity struct{ UUID, Name, Rel string }

func (c *v4Client) GetImageTask(ctx context.Context, id string) (*ImageTaskInfo, error) {
	if err := ValidateTaskID(id); err != nil {
		return nil, err
	}
	var response envelope[struct {
		ID       string `json:"extId"`
		Status   string `json:"status"`
		Entities []struct {
			UUID string `json:"extId"`
			Name string `json:"name"`
			Rel  string `json:"rel"`
		} `json:"entitiesAffected"`
		Parent       taskRef   `json:"parentTask"`
		Root         taskRef   `json:"rootTask"`
		Subtasks     []taskRef `json:"subTasks"`
		SubtaskCount *int      `json:"numberOfSubtasks"`
		ClusterUUIDs []string  `json:"clusterExtIds"`
		CreatedAt    time.Time `json:"createdTime"`
		CompletedAt  time.Time `json:"completedTime"`
	}]
	if _, err := c.do(ctx, http.MethodGet, c.prism+"/tasks/"+url.PathEscape(id), nil, &response, nil); err != nil {
		return nil, fmt.Errorf("read image recovery task failed; details suppressed")
	}
	data := response.Data
	if data.ID != id {
		return nil, fmt.Errorf("image recovery task identity mismatch")
	}
	result := &ImageTaskInfo{ID: id, State: TaskPending, ClusterUUIDs: data.ClusterUUIDs, CreatedAt: data.CreatedAt, CompletedAt: data.CompletedAt}
	switch data.Status {
	case "SUCCEEDED":
		result.State = TaskSucceeded
	case "FAILED", "CANCELED":
		result.State = TaskFailed
	case "QUEUED", "RUNNING", "CANCELING", "SUSPENDED":
	default:
		return nil, fmt.Errorf("image recovery task status unknown; details suppressed")
	}
	if len(data.Subtasks) > 16 || data.SubtaskCount != nil && (*data.SubtaskCount < 0 || *data.SubtaskCount != len(data.Subtasks)) {
		return nil, fmt.Errorf("image recovery task family incomplete or exceeds limit")
	}
	for _, ref := range append([]taskRef{data.Parent, data.Root}, data.Subtasks...) {
		if ref.UUID == "" {
			continue
		}
		if err := ValidateTaskID(ref.UUID); err != nil {
			return nil, err
		}
		result.FamilyIDs = append(result.FamilyIDs, ref.UUID)
	}
	for _, entity := range data.Entities {
		if entity.Rel == "vmm:ahv:vm" || entity.Rel == "vmm:ahv:config:vm" {
			return nil, fmt.Errorf("image recovery task references a VM; refusing recovery")
		}
		result.Entities = append(result.Entities, ImageTaskEntity{UUID: entity.UUID, Name: entity.Name, Rel: entity.Rel})
	}
	return result, nil
}
