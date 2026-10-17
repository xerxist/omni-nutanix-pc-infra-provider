// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package buildinfo contains public metadata embedded at build time.
package buildinfo

// Version is set by CI and make builds to the repository's semantic VERSION.
// Unstamped local builds deliberately identify themselves as development builds.
var Version = "dev"
