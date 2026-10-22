// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package sessions

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Windows has no flock, but the same contract is available through
// LockFileEx. It matters here for the same reason as the unix lock: the CLI,
// the web panel and the standalone sidecar share one sessions directory, and
// a read-modify-write without it loses the other writer's turn — on Windows
// the loss is louder, because WriteFileAtomic's rename fails with "Access is
// denied" while another process has the target open.
var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x00000002

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
	// The OVERLAPPED must stay alive until the unlock: LockFileEx records the
	// locked range in it even when the handle is synchronous.
	ol := new(syscall.Overlapped)
	r1, _, callErr := procLockFileEx.Call(
		uintptr(f.Fd()), lockfileExclusiveLock, 0, 1, 0,
		uintptr(unsafe.Pointer(ol)))
	if r1 == 0 {
		_ = f.Close()
		return nil, callErr
	}
	return func() {
		_, _, _ = procUnlockFileEx.Call(uintptr(f.Fd()), 0, 1, 0, uintptr(unsafe.Pointer(ol)))
		_ = f.Close()
	}, nil
}
