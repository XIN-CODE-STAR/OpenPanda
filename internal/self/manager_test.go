// SPDX-License-Identifier: AGPL-3.0-or-later

package self

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a throwaway git repo with committed content — the "upstream"
// a self-evolution worktree tracks.
func initRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := gitOut(ctx, repo, args...)
		if err != nil {
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

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	repo := initRepo(t)
	return New(t.TempDir(), repo, slog.New(slog.DiscardHandler)), repo
}

// mintChange writes a file in the worktree, intents it (git add -N) so the
// diff sees it, and mints the set against the repo's HEAD.
func mintChange(t *testing.T, m *Manager, repo, file, body, title string) Set {
	t.Helper()
	ctx := context.Background()
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	wt := m.WorktreePath()
	p := filepath.Join(wt, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitOut(ctx, wt, "add", "-N", file); err != nil {
		t.Fatalf("git add -N: %s", out)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	head, err := GitHead(ctx, repo)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	set, err := m.Mint(ctx, head, title, m.NextVersion("0.0.12-preview"), pub, priv, GateDigest{OK: true, Maker: "test"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return set
}

func TestEnsureWorktreeAndReset(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	wt := m.WorktreePath()
	if !strings.HasPrefix(wt, m.Root()) {
		t.Fatalf("worktree must live under manager root: %q", wt)
	}
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatal("worktree .git must exist")
	}
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatalf("ensure must be idempotent: %v", err)
	}
	if err := m.Reset(ctx, false); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("reset must remove the worktree")
	}
	// A manager with no repo configured must refuse worktree ops but still
	// serve queue inspection (import-only node).
	bare := New(t.TempDir(), "", slog.New(slog.DiscardHandler))
	if err := bare.EnsureWorktree(ctx); err == nil {
		t.Fatal("no repo → ensure must fail with a clear error")
	}
	if _, err := bare.Queue(); err != nil {
		t.Fatalf("queue inspection must work without a repo: %v", err)
	}
}

func TestMintValidateSignQueue(t *testing.T) {
	m, repo := testManager(t)
	set := mintChange(t, m, repo, "bar.txt", "new file\n", "add bar")

	mf := set.Manifest
	if mf.Kind != ManifestKind || mf.ID == "" || mf.Version != "0.0.12-preview+self.1" {
		t.Fatalf("bad manifest: %+v", mf)
	}
	if mf.Producer == "" || !strings.HasPrefix(mf.Producer, "k:") {
		t.Fatalf("producer must be k:<pub>: %q", mf.Producer)
	}
	if err := mf.Validate(); err != nil {
		t.Fatalf("minted manifest must validate: %v", err)
	}
	if err := mf.VerifySignature(); err != nil {
		t.Fatalf("minted manifest must verify: %v", err)
	}
	if mf.TouchesCritical || set.Critical {
		t.Fatal("bar.txt is not a critical path")
	}
	// Queue + Get round-trip.
	q, err := m.Queue()
	if err != nil || len(q) != 1 || q[0].Manifest.ID != mf.ID || q[0].State != StatePending {
		t.Fatalf("queue: %+v err=%v", q, err)
	}
	got, err := m.Get(mf.ID)
	if err != nil || got.Manifest.Title != "add bar" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := m.Get("nope"); err == nil {
		t.Fatal("unknown id must error")
	}
	// A second pending set still targets +self.1 — the counter only moves on
	// merged sets; whichever proposal merges first claims the stamp.
	set2 := mintChange(t, m, repo, "baz.txt", "z\n", "add baz")
	if set2.Manifest.Version != "0.0.12-preview+self.1" {
		t.Fatalf("version counter: %q", set2.Manifest.Version)
	}
}

// TestMintPinsBaseAndDigests pins two integrity properties: a symbolic base
// ("HEAD") is resolved to a commit so the rebase sweep can tell staleness, and
// the signed digest makes a post-mint patch edit unloadable.
func TestMintPinsBaseAndDigests(t *testing.T) {
	m, repo := testManager(t)
	ctx := context.Background()
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatal(err)
	}
	wt := m.WorktreePath()
	if err := os.WriteFile(filepath.Join(wt, "pinned.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitOut(ctx, wt, "add", "-N", "pinned.txt"); err != nil {
		t.Fatalf("git add -N: %s", out)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	set, err := m.Mint(ctx, "HEAD", "pin", "0.0.12-preview+self.1", pub, priv, GateDigest{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	head, err := GitHead(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.Base != head {
		t.Fatalf("base = %q, want the resolved commit %q", set.Manifest.Base, head)
	}
	if set.Manifest.Digests["0001-worktree.patch"] == "" {
		t.Fatal("minted manifest must bind the patch digest")
	}
	// Rewrite the patch after minting: the manifest signature still verifies,
	// but the content no longer matches, so loading must fail.
	patchPath := filepath.Join(set.Dir, "0001-worktree.patch")
	body, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patchPath, append(body, []byte("+smuggled\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(set.Manifest.ID); err == nil {
		t.Fatal("a patch edited after signing must not load")
	}
}

func TestMintRefusesUntracked(t *testing.T) {
	m, repo := testManager(t)
	ctx := context.Background()
	if err := m.EnsureWorktree(ctx); err != nil {
		t.Fatal(err)
	}
	wt := m.WorktreePath()
	// A change with no diff surface: untracked file without add -N → the diff
	// would silently drop it, so mint must refuse outright.
	if err := os.WriteFile(filepath.Join(wt, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	head, _ := GitHead(ctx, repo)
	if _, err := m.Mint(ctx, head, "loose", "v", pub, priv, GateDigest{}); err == nil {
		t.Fatal("untracked worktree files must refuse the mint")
	}
}

func TestApplyConflictedGateAndArchive(t *testing.T) {
	m, repo := testManager(t)
	ctx := context.Background()
	set := mintChange(t, m, repo, "delta.txt", "delta\n", "delta")
	id := set.Manifest.ID

	// Conflicted sets can never apply.
	if err := m.MarkConflicted(id); err != nil {
		t.Fatalf("mark conflicted: %v", err)
	}
	if err := m.Apply(ctx, id); err == nil {
		t.Fatal("conflicted set must not apply")
	}
	got, _ := m.Get(id)
	if got.State != StateConflicted {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ClearConflicted(id); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// Apply resets the worktree to the base and replays the patch — the
	// minted change (still sitting uncommitted in the worktree) gets wiped
	// and re-materialised by the diff itself.
	if err := m.Apply(ctx, id); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(m.WorktreePath(), "delta.txt"))
	if err != nil || string(body) != "delta\n" {
		t.Fatalf("patch did not materialise: %q %v", body, err)
	}

	// Archive is terminal: out of the pending queue, into archive/.
	if err := m.Archive(id, StateRejected); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := m.Archive(id, StateRejected); err == nil {
		t.Fatal("double archive must error")
	}
	if err := m.Apply(ctx, id); err == nil {
		t.Fatal("archived set must not apply")
	}
	got, _ = m.Get(id)
	if got.State != StateRejected {
		t.Fatalf("archived state: %s", got.State)
	}
}

func TestImportValidatesAndDedupes(t *testing.T) {
	m, repo := testManager(t)
	set := mintChange(t, m, repo, "imp.txt", "i\n", "imported")

	// Import into a second manager (the "receiving" node).
	dst := New(t.TempDir(), repo, slog.New(slog.DiscardHandler))
	imp, err := dst.Import(set.Dir)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imp.Manifest.ID != set.Manifest.ID || imp.State != StatePending {
		t.Fatalf("imported set: %+v", imp)
	}
	if err := imp.Manifest.VerifySignature(); err != nil {
		t.Fatalf("imported manifest must still verify: %v", err)
	}
	if _, err := dst.Import(set.Dir); err == nil {
		t.Fatal("re-import must be refused")
	}
	// Tampered source must not import: break the signature.
	src2 := filepath.Join(t.TempDir(), "evil")
	if err := copyDir(set.Dir, src2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src2, "manifest.json"), []byte(`{"kind":"openpanda-patchset/1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Import(src2); err == nil {
		t.Fatal("manifest that fails validation must not import")
	}
}

func TestRebaseOnto(t *testing.T) {
	m, repo := testManager(t)
	ctx := context.Background()
	set := mintChange(t, m, repo, "reb.txt", "r\n", "rebase me")

	// Move upstream HEAD forward — the set's base is now stale.
	if err := os.WriteFile(filepath.Join(repo, "upstream.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitOut(ctx, repo, "add", "-A"); err != nil {
		t.Fatalf("add: %s", out)
	}
	if out, err := gitOut(ctx, repo, "commit", "-qm", "upstream move"); err != nil {
		t.Fatalf("commit: %s", out)
	}
	newHead, err := GitHead(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if SameRef(ctx, repo, set.Manifest.Base, newHead) {
		t.Fatal("stale base must differ from the new head")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	if err := m.RebaseOnto(ctx, set.Manifest.ID, newHead, pub, priv); err != nil {
		t.Fatalf("rebase: %v", err)
	}
	rebased, err := m.Get(set.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebased.Manifest.Base != newHead {
		t.Fatalf("base not moved: %s != %s", rebased.Manifest.Base, newHead)
	}
	if !strings.Contains(rebased.Manifest.Title, "rebased") {
		t.Fatalf("rebase must annotate the title: %q", rebased.Manifest.Title)
	}
	if err := rebased.Manifest.VerifySignature(); err != nil {
		t.Fatalf("rebased manifest must verify under the re-baser's key: %v", err)
	}
	// The re-minted diff still carries the change.
	hits := 0
	for _, f := range rebased.Manifest.Files {
		body, _ := os.ReadFile(filepath.Join(rebased.Dir, f))
		if strings.Contains(string(body), "reb.txt") {
			hits++
		}
	}
	if hits == 0 {
		t.Fatal("rebased patch lost its change")
	}
	if err := m.RebaseOnto(ctx, "nope", newHead, pub, priv); err == nil {
		t.Fatal("rebase of a missing set must error")
	}
}

// TestWorktreeLockSerializes pins the cross-process guard: a second holder is
// refused while the first is live, and the release makes it available again.
func TestWorktreeLockSerializes(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	release, err := m.AcquireWorktreeLock(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := m.AcquireWorktreeLock(ctx); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("second acquire = %v, want ErrWorktreeBusy", err)
	}
	release()
	release2, err := m.AcquireWorktreeLock(ctx)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

// TestRejectedMarkerBlocksApply pins the local fallback for a failed archive:
// a rejected set stays in queue/ but can never apply or be proposed.
func TestRejectedMarkerBlocksApply(t *testing.T) {
	ctx := context.Background()
	m, repo := testManager(t)
	set := mintChange(t, m, repo, "rej.txt", "r\n", "reject me")
	if err := m.MarkRejected(set.Manifest.ID); err != nil {
		t.Fatalf("mark rejected: %v", err)
	}
	got, err := m.Get(set.Manifest.ID)
	if err != nil || got.State != StateRejected {
		t.Fatalf("state = %v err=%v, want rejected", got.State, err)
	}
	if err := m.Apply(ctx, set.Manifest.ID); err == nil {
		t.Fatal("a rejected set must not apply")
	}
}

func TestNextVersion(t *testing.T) {
	m, repo := testManager(t)
	if v := m.NextVersion("0.0.12-preview"); v != "0.0.12-preview+self.1" {
		t.Fatalf("first self version: %q", v)
	}
	// A base already stamped +self.N continues the counter.
	if v := m.NextVersion("0.0.12-preview+self.4"); v != "0.0.12-preview+self.5" {
		t.Fatalf("chained self version: %q", v)
	}
	set := mintChange(t, m, repo, "v.txt", "v\n", "v")
	if err := m.Archive(set.Manifest.ID, StateMerged); err != nil {
		t.Fatal(err)
	}
	if v := m.NextVersion("0.0.12-preview"); v != "0.0.12-preview+self.2" {
		t.Fatalf("merged sets must bump the counter: %q", v)
	}
}
