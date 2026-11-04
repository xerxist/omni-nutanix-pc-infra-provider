// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package buildinfo contains public metadata embedded at build time.
package buildinfo

// Version is set at build time: CI uses the release tag without its leading "v",
// make builds use git describe.
// Unstamped local builds deliberately identify themselves as development builds.
var Version = "dev"
