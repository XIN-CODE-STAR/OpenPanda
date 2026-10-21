// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/self"
)

// selfTestRepo makes a throwaway git checkout the self manager can worktree.
func selfTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, out, err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@panda")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "foo.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	return repo
}

// mintTestSet dirties the manager's worktree and mints a signed set with the
// given key — the producing half of the pipeline.
func mintTestSet(t *testing.T, m *self.Manager, pub ed25519.PublicKey, priv ed25519.PrivateKey, title string) self.Set {
	t.Helper()
	ctx := context.Background()
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	// A distinct file per call keeps the worktree diff non-empty and the set
	// content different, so repeated mints do not collide.
	name := "change-" + strings.ReplaceAll(title, " ", "-") + ".txt"
	path := filepath.Join(m.WorktreePath(), name)
	if err := os.WriteFile(path, []byte(title+"\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	cmd := exec.Command("git", "add", "-N", name)
	cmd.Dir = m.WorktreePath()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add -N: %s (%v)", out, err)
	}
	set, err := m.Mint(ctx, "HEAD", title, "0.0.12-preview+self.1", pub, priv, self.GateDigest{OK: true})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return set
}

func newSelfTestCore(t *testing.T, id, producers string, mgr *self.Manager) *Core {
	t.Helper()
	db := openTestDB(t)
	card := ledger.Card{Device: id, ResourceClass: "Standard"}
	c := NewCore(db, id, card, 5, testLogger(), config.ModelConfig{})
	c.SetWorkDir(t.TempDir())
	c.SetSelfEvolution(mgr, true, producers)
	return c
}

// TestSelfSwapGuardBinaryName pins the exact-name rule: the sidecar binary
// starts with "panda" too, and a prefix match let it through even though a
// swap there overwrites panda-webui instead of the install target.
func TestSelfSwapGuardBinaryName(t *testing.T) {
	for _, exe := range []string{"panda", "panda.exe", "PANDA.EXE", "/usr/local/bin/panda"} {
		if err := selfSwapGuardFor(exe); err != nil {
			t.Errorf("selfSwapGuardFor(%q) = %v, want nil", exe, err)
		}
	}
	for _, exe := range []string{"panda-webui", "panda-webui.exe", "core.test", "panel", ""} {
		if err := selfSwapGuardFor(exe); err == nil {
			t.Errorf("selfSwapGuardFor(%q) = nil, want refusal", exe)
		}
	}
}

// TestIdleExceptIgnoresOwnMergeTask pins the deadlock fix: a self-merge task
// is RUNNING while it swaps the binary, and the updater's idle gate must not
// count it as busy work.
func TestIdleExceptIgnoresOwnMergeTask(t *testing.T) {
	ctx := context.Background()
	c := newSelfTestCore(t, "idle-node", "self", nil)
	mergeTask, err := c.store.Create(ctx, "", "", "self-merge", c.nodeID, []string{c.nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE tasks SET state = ? WHERE task_id = ?`, StateRunning, mergeTask.TaskID); err != nil {
		t.Fatal(err)
	}
	if !c.idleExcept(ctx, mergeTask.TaskID) {
		t.Fatal("the merge task's own RUNNING row must not count as busy")
	}
	if c.idleExcept(ctx, "someone-else") {
		t.Fatal("a running task must count as busy for anyone else")
	}
	other, err := c.store.Create(ctx, "", "", "real work", c.nodeID, []string{c.nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE tasks SET state = ? WHERE task_id = ?`, StateRunning, other.TaskID); err != nil {
		t.Fatal(err)
	}
	if c.idleExcept(ctx, mergeTask.TaskID) {
		t.Fatal("another running task must keep the merge gate closed")
	}
}

// TestProposeSelfMergeParksReview pins the proposal shape: a merge proposal IS
// a review task carrying the self_merge envelope, and a bare store approve
// must refuse it (only the pipeline may settle it).
func TestProposeSelfMergeParksReview(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "propose-node", "self", mgr)
	pub, priv, ok := c.nodeKeyPair()
	if !ok {
		t.Fatal("node key unavailable")
	}
	set := mintTestSet(t, mgr, pub, priv, "add a note")

	task, err := c.ProposeSelfMerge(ctx, set.Manifest.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if task.State != StateReview {
		t.Fatalf("proposal state = %s, want review", task.State)
	}
	disposition, err := c.store.ApprovalDisposition(ctx, task.TaskID)
	if err != nil || disposition != ApprovalSelfMerge {
		t.Fatalf("disposition = %q err = %v, want %q", disposition, err, ApprovalSelfMerge)
	}
	spec, ok := SelfMergeSpec(task.SpecJSON)
	if !ok || spec.PatchSet != set.Manifest.ID || spec.Version != set.Manifest.Version {
		t.Fatalf("merge envelope = %+v (ok=%v)", spec, ok)
	}
	if err := c.store.Approve(ctx, task.TaskID); !errors.Is(err, ErrIllegal) {
		t.Fatalf("bare store approve of a self-merge = %v, want ErrIllegal", err)
	}
}

// TestProposeSelfMergeRefusesForeignProducer pins self_evolution.producers:
// the default admits only this node's own key, so a set signed elsewhere must
// never become mergeable here.
func TestProposeSelfMergeRefusesForeignProducer(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "foreign-node", "self", mgr)
	foreignPub, foreignPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	set := mintTestSet(t, mgr, foreignPub, foreignPriv, "foreign change")

	if _, err := c.ProposeSelfMerge(ctx, set.Manifest.ID); err == nil {
		t.Fatal("a set signed by another key must not be proposable under producers=self")
	}
}
