// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package sessions

// lockStore is a no-op on platforms without flock. The in-process mutex still
// serializes goroutines; a second PROCESS writing the same sessions directory
// remains a known gap on these platforms (Windows is the shipping case).
func lockStore(string) (func(), error) { return func() {}, nil }
