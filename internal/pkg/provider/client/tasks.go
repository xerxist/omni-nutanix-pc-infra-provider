// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"fmt"
	"regexp"
	"strings"
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_+/:=-]*$`)

// ValidateTaskID checks a bounded, path-escapable GA TaskReference.extId. Prism
// task IDs are opaque strings, not entity UUIDs; encoded IDs must be persisted
// and passed to the task endpoint unchanged, without decoding or normalization.
// The bound/alphabet are local safety limits, not a claim about the API schema.
func ValidateTaskID(id string) error {
	if len(id) == 0 || len(id) > 256 || !taskIDPattern.MatchString(id) || strings.Contains(id, "://") {
		return fmt.Errorf("invalid Prism Central task ID; details suppressed")
	}
	return nil
}
