// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// stageTree lays out a minimal release-shaped tree: bin/<exe> plus an adapter
// script, the two things the apply step consumes.
func stageTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, exeName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	adDir := filepath.Join(root, "adapters")
	if err := os.MkdirAll(adDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adDir, "claude_code.py"), []byte("# adapter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestStageLocalCopiesAndStages pins the self-evolution seam: a local build
// tree is copied into a manager-owned staging dir (the caller may delete its
// own tree afterwards) and lands in the same staged state Apply consumes.
func TestStageLocalCopiesAndStages(t *testing.T) {
	root := stageTree(t)
	m := New(Options{Logger: testDiscardLogger()})
	if err := m.StageLocal(root, "0.0.12-preview+self.3"); err != nil {
		t.Fatalf("stage local: %v", err)
	}
	defer m.Cancel()

	if m.stage != StageStaged {
		t.Fatalf("stage = %v, want staged", m.stage)
	}
	if m.staged == nil || !m.staged.local {
		t.Fatalf("staged = %+v, want a local release", m.staged)
	}
	if m.latest != "0.0.12-preview+self.3" {
		t.Fatalf("latest = %q", m.latest)
	}
	if !strings.HasPrefix(m.staged.dir, os.TempDir()) {
		t.Fatalf("staged dir %q is not manager-owned temp state", m.staged.dir)
	}
	// The copy is independent of the caller's tree.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join("bin", exeName()),
		filepath.Join("adapters", "claude_code.py"),
	} {
		if _, err := os.Stat(filepath.Join(m.staged.root, rel)); err != nil {
			t.Fatalf("staged copy is missing %s: %v", rel, err)
		}
	}

	// A busy queue refuses the swap, leaving the staged tree intact for retry.
	m.opts.Idle = func(context.Context) bool { return false }
	if err := m.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("apply on a busy queue = %v, want busy", err)
	}
	m.opts.Idle = func(context.Context) bool { return true }
	if m.staged == nil {
		t.Fatal("a refused apply must keep the staged release")
	}
}

// TestStageLocalRefusesBareTree pins the layout check: staging a tree without
// the binary would let Apply replace the install with nothing.
func TestStageLocalRefusesBareTree(t *testing.T) {
	m := New(Options{Logger: testDiscardLogger()})
	if err := m.StageLocal(t.TempDir(), "0.0.12+self.1"); err == nil {
		t.Fatal("a tree without bin/<exe> must not stage")
	}
}
