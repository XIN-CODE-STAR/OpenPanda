// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !lite

package updater

// liteBuild marks binaries compiled with -tags lite. The release channel
// ships them under a separate asset name (panda-<v>-lite-<os>-<arch>.tar.gz),
// so self-update must not pull the full archive over a lite install.
const liteBuild = false

// LiteBuild reports whether this binary was compiled with -tags lite (the
// reduced-footprint build). Self-evolution rebuilds use it to keep a lite
// node lite.
func LiteBuild() bool { return liteBuild }
