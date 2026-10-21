// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/self"
)

// selfMergeTask makes a real self-merge review row: a throwaway git repo, a
// signed patch set, and a Core wired to it. producers=mesh admits the test
// key without needing the node's persisted identity.
func selfMergeTask(t *testing.T) (*core.Core, *self.Manager, core.Task) {
	t.Helper()
	ctx := context.Background()
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
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")

	logger := slog.New(slog.DiscardHandler)
	mgr := self.New(t.TempDir(), repo, logger)
	if err := mgr.EnsureWorktree(ctx); err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	wt := mgr.WorktreePath()
	if err := os.WriteFile(filepath.Join(wt, "change.txt"), []byte("self\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wt, "add", "-N", "change.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add -N: %s (%v)", out, err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	set, err := mgr.Mint(ctx, "HEAD", "panel test", "0.0.12-preview+self.1", pub, priv, self.GateDigest{OK: true})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	db := newTestDB(t)
	c := core.NewCore(db, "panel-node", ledger.Card{Device: "panel-node"}, 5, logger, config.ModelConfig{})
	c.SetSelfEvolution(mgr, true, "mesh")
	task, err := c.ProposeSelfMerge(ctx, set.Manifest.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if task.State != core.StateReview {
		t.Fatalf("proposal state = %s, want review", task.State)
	}
	return c, mgr, task
}

// TestSidecarRefusesSelfMergeApproval pins the binary-safety boundary: the
// sidecar's executable is panda-webui, so approving a merge there would
// replace the console with the daemon build. The request must fail before any
// approval is remembered or the pipeline starts.
func TestSidecarRefusesSelfMergeApproval(t *testing.T) {
	c, mgr, task := selfMergeTask(t)
	h := New(Deps{
		Store:     c.TaskStore(),
		Sidecar:   true,
		StaticDir: t.TempDir(),
		Token:     testToken,
	})

	rr := authedReqWriter(t, h, authedReq(http.MethodPost, "/api/tasks/"+task.TaskID+"/approve", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("sidecar approve = %d %s, want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "panda approve") {
		t.Fatalf("refusal must point at the CLI: %s", rr.Body.String())
	}
	cur, err := c.TaskStore().Get(context.Background(), task.TaskID)
	if err != nil || cur.State != core.StateReview {
		t.Fatalf("task state = %+v err=%v, want review (nothing consumed)", cur.State, err)
	}
	set, err := mgr.Get(patchSetID(t, task))
	if err != nil || set.State != self.StatePending {
		t.Fatalf("patch set state = %v err=%v, want pending", set.State, err)
	}
}

// patchSetID decodes the id out of the task's self_merge envelope.
func patchSetID(tb testing.TB, task core.Task) string {
	tb.Helper()
	spec, ok := core.SelfMergeSpec(task.SpecJSON)
	if !ok {
		tb.Fatalf("task %s carries no self_merge spec", task.TaskID)
	}
	return spec.PatchSet
}
