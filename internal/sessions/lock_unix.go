// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package sessions

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockStore takes an advisory exclusive lock on the sessions root so a
// read-modify-write cycle is atomic ACROSS PROCESSES, not just across
// goroutines. The CLI and the web panel (and the standalone sidecar) share
// one sessions directory: without this, two processes could each load the
// old JSON and the last writer would silently drop the other's turn.
//
// The lock is advisory — every writer in this package takes it, and a foreign
// process that ignores it is outside the contract. LOCK_EX blocks, so the
// caller simply waits its turn.
func lockStore(root string) (func(), error) {
	if root == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
