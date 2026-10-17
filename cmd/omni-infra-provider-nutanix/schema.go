// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Match Omni v1.12.4's schema compiler, including its default Go regexp engine
// and draft 2020-12 metaschema. Python/JavaScript regex acceptance is not enough.
// Refuse startup before any connection or schema publication if compilation fails.
func compileProviderSchema(raw string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	const filename = "nutanix-pc-schema.json"
	if err := compiler.AddResource(filename, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(filename)
}
