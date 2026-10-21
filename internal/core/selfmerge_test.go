// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/self"
	"github.com/Xustalis/OpenPanda/internal/updater"
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
	// Keep the bytes as written: the Windows runner's default autocrlf would
	// rewrite the worktree file to CRLF, so the diff (and the patch applied
	// from it) would carry CRLF where the fixture wrote LF.
	run("config", "core.autocrlf", "false")
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

// fakeSelfUpdater records the stage/apply calls the merge pipeline makes.
type fakeSelfUpdater struct {
	stageVersion string
	applied      bool
}

func (f *fakeSelfUpdater) StageLocal(_, version string) error { f.stageVersion = version; return nil }
func (f *fakeSelfUpdater) Apply(context.Context) error        { f.applied = true; return nil }

// TestSelfMergePipelineCompletesAndArchives exercises the approval pipeline
// with the gate/build/swap seams stubbed: claim → apply → gate → stage →
// archive → complete. It also pins the merge-time version allocation.
func TestSelfMergePipelineCompletesAndArchives(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "merge-node", "mesh", mgr)
	pub, priv, ok := c.nodeKeyPair()
	if !ok {
		t.Fatal("node key unavailable")
	}
	set := mintTestSet(t, mgr, pub, priv, "merge me")
	task, err := c.ProposeSelfMerge(ctx, set.Manifest.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	oldGuard, oldGate, oldStage, oldUpd, oldRestart := selfSwapGuard, selfRunGate, selfStageTree, selfNewUpdater, selfRestart
	t.Cleanup(func() {
		selfSwapGuard, selfRunGate, selfStageTree, selfNewUpdater, selfRestart = oldGuard, oldGate, oldStage, oldUpd, oldRestart
	})
	selfSwapGuard = func() error { return nil }
	selfRunGate = func(context.Context, string) self.GateDigest {
		return self.GateDigest{OK: true, Legs: []self.GateLeg{{Name: "stub", OK: true}}}
	}
	selfStageTree = func(*self.Manager, context.Context, string, string, string) error { return nil }
	fake := &fakeSelfUpdater{}
	selfNewUpdater = func(updater.Options) selfUpdater { return fake }
	selfRestart = func(*slog.Logger, bool) {}

	final, res, err := c.ResumeApproved(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if final.State != StateDone || !res.OK {
		t.Fatalf("final state=%s res=%+v, want done", final.State, res)
	}
	if !fake.applied || fake.stageVersion != "0.0.12-preview+self.1" {
		t.Fatalf("updater calls: applied=%v version=%q", fake.applied, fake.stageVersion)
	}
	got, err := mgr.Get(set.Manifest.ID)
	if err != nil || got.State != self.StateMerged {
		t.Fatalf("patch set state = %v err=%v, want merged", got.State, err)
	}
}

// TestRejectSelfMergeArchivesSet pins the other half: a reject retires the
// patch set so it cannot be proposed again.
func TestRejectSelfMergeArchivesSet(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "reject-node", "mesh", mgr)
	pub, priv, ok := c.nodeKeyPair()
	if !ok {
		t.Fatal("node key unavailable")
	}
	set := mintTestSet(t, mgr, pub, priv, "reject me")
	task, err := c.ProposeSelfMerge(ctx, set.Manifest.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if err := c.RejectTree(ctx, task.TaskID, "not this one"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, err := mgr.Get(set.Manifest.ID)
	if err != nil || got.State != self.StateRejected {
		t.Fatalf("patch set state = %v err=%v, want rejected", got.State, err)
	}
}

// TestProposeSelfMergeRefusesStaleBase pins the freshness gate: Apply resets
// the worktree to the manifest's base, so approving a set after HEAD moved
// would build the old tree plus the patch and swap it over newer commits.
func TestProposeSelfMergeRefusesStaleBase(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "stale-node", "mesh", mgr)
	pub, priv, ok := c.nodeKeyPair()
	if !ok {
		t.Fatal("node key unavailable")
	}
	set := mintTestSet(t, mgr, pub, priv, "stale change")

	// Move the repo's HEAD forward without touching the worktree.
	if err := os.WriteFile(filepath.Join(repo, "newer.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "newer"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, out, err)
		}
	}
	if _, err := c.ProposeSelfMerge(ctx, set.Manifest.ID); err == nil {
		t.Fatal("a set whose base is stale must not be proposable")
	}
}

// TestSelfMergeRefusesChangedSet pins the review binding: the artifact on
// disk at approve time must still be the one the reviewer saw. The
// boot-time rebase sweep rewrites a queued set in place, so approving a
// proposal whose set has since been rebased would merge bytes nobody looked
// at.
func TestSelfMergeRefusesChangedSet(t *testing.T) {
	ctx := context.Background()
	repo := selfTestRepo(t)
	mgr := self.New(t.TempDir(), repo, testLogger())
	c := newSelfTestCore(t, "digest-node", "mesh", mgr)
	pub, priv, ok := c.nodeKeyPair()
	if !ok {
		t.Fatal("node key unavailable")
	}
	set := mintTestSet(t, mgr, pub, priv, "digest change")
	task, err := c.ProposeSelfMerge(ctx, set.Manifest.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	// Rewrite the set in place: a rebase re-mints the manifest (new title,
	// new patch bytes) while the set stays pending.
	if err := mgr.RebaseOnto(ctx, set.Manifest.ID, set.Manifest.Base, pub, priv); err != nil {
		t.Fatalf("rebase: %v", err)
	}

	oldGuard, oldGate, oldStage, oldUpd, oldRestart := selfSwapGuard, selfRunGate, selfStageTree, selfNewUpdater, selfRestart
	t.Cleanup(func() {
		selfSwapGuard, selfRunGate, selfStageTree, selfNewUpdater, selfRestart = oldGuard, oldGate, oldStage, oldUpd, oldRestart
	})
	selfSwapGuard = func() error { return nil }
	selfRunGate = func(context.Context, string) self.GateDigest {
		return self.GateDigest{OK: true, Legs: []self.GateLeg{{Name: "stub", OK: true}}}
	}
	selfStageTree = func(*self.Manager, context.Context, string, string, string) error { return nil }
	fake := &fakeSelfUpdater{}
	selfNewUpdater = func(updater.Options) selfUpdater { return fake }
	selfRestart = func(*slog.Logger, bool) {}

	final, res, err := c.ResumeApproved(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.OK || fake.applied {
		t.Fatalf("a changed patch set must not merge: state=%s res=%+v applied=%v", final.State, res, fake.applied)
	}
	if !strings.Contains(res.Stderr, "changed since it was proposed") {
		t.Fatalf("the failure must name the digest mismatch: %q", res.Stderr)
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
