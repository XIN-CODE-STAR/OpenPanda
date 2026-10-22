// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package sessions

// lockStore is a no-op on platforms without a file-lock primitive. The
// in-process mutex still serializes goroutines; a second PROCESS writing the
// same sessions directory remains a known gap on these platforms.
func lockStore(string) (func(), error) { return func() {}, nil }
