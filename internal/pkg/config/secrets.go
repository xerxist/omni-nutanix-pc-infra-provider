// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package config

import (
	"fmt"
	"os"
	"strings"
)

// ReadSecretEnvironment supports an environment value or a mounted secret file.
// Environment/file sources override YAML; configuring both fails closed. Strip
// one file-ending newline, but preserve spaces and all other secret bytes.
// Errors never include the supplied value, path, or file contents.
func ReadSecretEnvironment(name string) (string, bool, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", false, fmt.Errorf("configure %s or %s_FILE, not both", name, name)
	}
	if path == "" {
		return value, value != "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("cannot read secret file configured by %s_FILE", name)
	}
	value = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if value == "" {
		return "", false, fmt.Errorf("secret file configured by %s_FILE is empty", name)
	}
	return value, true, nil
}
