// SPDX-License-Identifier: AGPL-3.0-or-later

package self

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SetState is the patch-set lifecycle, derived from which directory the set
// lives in plus an optional marker file: queue/ holds sets awaiting (or mid-)
// review, archive/ holds terminal sets. A "conflicted" marker inside a queue
// dir marks a set whose rebase onto a newer base failed (design §7 q3).
type SetState string

const (
	StatePending    SetState = "pending"
	StateMerged     SetState = "merged"
	StateRejected   SetState = "rejected"
	StateConflicted SetState = "conflicted"
)

// Set is one patch set on disk: its manifest plus where it sits.
type Set struct {
	Manifest Manifest
	Dir      string // queue/<id>/ or archive/<id>/
	State    SetState
	// Critical is the RECOMPUTED flag — never the manifest's claim.
	Critical      bool
	CriticalPaths []string
}

// Manager owns the <data>/self/ tree. All state lives under it so `panda self
// reset` is a directory removal and nothing outside the node knows it exists.
//
//	root/
//	  worktree/          git worktree of the project checkout (created lazily)
//	  queue/<id>/        manifest.json + *.patch awaiting review
//	  archive/<id>/      terminal sets (merged/rejected/conflicted)
type Manager struct {
	root   string // <data>/self
	repo   string // project checkout the worktree clones from
	logger *slog.Logger
}

// New returns a Manager over root. repo may be empty — worktree operations
// then fail with a clear error while queue inspection still works.
func New(root, repo string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{root: root, repo: repo, logger: logger}
}

// Root reports where this manager's state lives.
func (m *Manager) Root() string { return m.root }

// Repo reports the configured project checkout.
func (m *Manager) Repo() string { return m.repo }

// WorktreePath is where the merge worktree lives.
func (m *Manager) WorktreePath() string { return filepath.Join(m.root, "worktree") }

func (m *Manager) queueDir() string   { return filepath.Join(m.root, "queue") }
func (m *Manager) archiveDir() string { return filepath.Join(m.root, "archive") }

// criticalOrFailClosed recomputes the critical flag from the patches on disk.
// A read failure must never downgrade a set to "not critical" — the flag is a
// merge-gate input — so an error is reported as critical, with the error text
// carried in the path list the review surface renders.
func criticalOrFailClosed(dir string, files []string) (bool, []string) {
	crit, hits, err := TouchesCritical(dir, files)
	if err != nil {
		return true, []string{"unreadable patch: " + err.Error()}
	}
	return crit, hits
}

// Queue lists every patch set in queue/ and archive/, newest first.
func (m *Manager) Queue() ([]Set, error) {
	var out []Set
	for _, top := range []struct {
		dir   string
		state SetState
	}{{m.queueDir(), StatePending}, {m.archiveDir(), ""}} {
		ents, err := os.ReadDir(top.dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(top.dir, e.Name())
			mf, err := LoadManifest(dir)
			if err != nil {
				// A malformed set still shows up — as a listing line marked by
				// the read error, never as something mergeable.
				out = append(out, Set{Dir: dir, State: StateConflicted,
					Manifest: Manifest{ID: e.Name(), Title: "unreadable: " + err.Error()}})
				continue
			}
			st := top.state
			if st == "" {
				st = markerState(dir)
			}
			if st == "" {
				st = StateMerged
			}
			crit, hits := criticalOrFailClosed(dir, mf.Files)
			out = append(out, Set{Manifest: mf, Dir: dir, State: st, Critical: crit, CriticalPaths: hits})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID > out[j].Manifest.ID })
	return out, nil
}

// markerState reads the one-word state marker an archived dir carries.
func markerState(dir string) SetState {
	b, err := os.ReadFile(filepath.Join(dir, ".state"))
	if err != nil {
		return ""
	}
	switch SetState(strings.TrimSpace(string(b))) {
	case StateMerged, StateRejected, StateConflicted:
		return SetState(strings.TrimSpace(string(b)))
	}
	return ""
}

// Get resolves one set by id across queue and archive.
func (m *Manager) Get(id string) (Set, error) {
	for _, dir := range []string{filepath.Join(m.queueDir(), id), filepath.Join(m.archiveDir(), id)} {
		if _, err := os.Stat(dir); err == nil {
			mf, err := LoadManifest(dir)
			if err != nil {
				return Set{}, err
			}
			st := StatePending
			if filepath.Base(filepath.Dir(dir)) == "archive" {
				if st = markerState(dir); st == "" {
					st = StateMerged
				}
			} else if markerState(dir) == StateConflicted {
				st = StateConflicted
			}
			crit, hits := criticalOrFailClosed(dir, mf.Files)
			return Set{Manifest: mf, Dir: dir, State: st, Critical: crit, CriticalPaths: hits}, nil
		}
	}
	return Set{}, fmt.Errorf("no patch set %q", id)
}

// Import copies a patch-set directory into the queue after full validation —
// the entry point for sets minted elsewhere (mesh transfer lands here in P5)
// or by an agent task on this node.
func (m *Manager) Import(src string) (Set, error) {
	mf, err := LoadManifest(src)
	if err != nil {
		return Set{}, fmt.Errorf("import: %w", err)
	}
	dst := filepath.Join(m.queueDir(), mf.ID)
	if _, err := os.Stat(dst); err == nil {
		return Set{}, fmt.Errorf("patch set %q already queued", mf.ID)
	}
	if err := os.MkdirAll(m.queueDir(), 0o755); err != nil {
		return Set{}, err
	}
	if err := copyDir(src, dst); err != nil {
		return Set{}, err
	}
	crit, hits := criticalOrFailClosed(dst, mf.Files)
	return Set{Manifest: mf, Dir: dst, State: StatePending, Critical: crit, CriticalPaths: hits}, nil
}

// Mint assembles a patch set from the worktree's diff against baseRef, signs
// it with the node key, and queues it — the producing half of the pipeline.
func (m *Manager) Mint(ctx context.Context, baseRef, title, version string, pub ed25519.PublicKey, priv ed25519.PrivateKey, gate GateDigest) (Set, error) {
	if err := m.EnsureWorktree(ctx); err != nil {
		return Set{}, err
	}
	wt := m.WorktreePath()
	// Pin the base to a commit SHA. Storing a symbolic ref ("HEAD") would make
	// the set look current forever (SameRef resolves it to the moving head), so
	// the rebase sweep would never carry it forward and git apply would later
	// reset to a different tree than the diff was produced against.
	base, err := gitOut(ctx, wt, "rev-parse", baseRef)
	if err != nil {
		return Set{}, fmt.Errorf("resolve base %s: %s", baseRef, base)
	}
	baseRef = strings.TrimSpace(base)
	diff, err := gitOut(ctx, wt, "diff", baseRef, "--binary")
	if err != nil {
		return Set{}, fmt.Errorf("worktree diff against %s: %w", baseRef, err)
	}
	if strings.TrimSpace(diff) == "" {
		return Set{}, fmt.Errorf("worktree has no changes against %s", baseRef)
	}
	// Untracked files the diff misses would silently drop out of the set —
	// refuse instead, forcing the producer to git add -N them.
	if out, _ := gitOut(ctx, wt, "ls-files", "--others", "--exclude-standard"); strings.TrimSpace(out) != "" {
		return Set{}, fmt.Errorf("worktree has untracked files (git add -N them first):\n%s", out)
	}
	// Nanosecond + random suffix: two mints inside the same wall-clock second
	// must not overwrite each other's directory.
	id := fmt.Sprintf("ps-%d-%04x", time.Now().UnixNano(), rand.IntN(1<<16))
	dir := filepath.Join(m.queueDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Set{}, err
	}
	patch := "0001-worktree.patch"
	if err := os.WriteFile(filepath.Join(dir, patch), []byte(diff), 0o644); err != nil {
		return Set{}, err
	}
	digests, err := patchDigests(dir, []string{patch})
	if err != nil {
		return Set{}, err
	}
	mf := Manifest{
		Kind:     ManifestKind,
		ID:       id,
		Base:     baseRef,
		Version:  version,
		Producer: "k:" + hex.EncodeToString(pub),
		Title:    title,
		Files:    []string{patch},
		Digests:  digests,
		Gate:     gate,
	}
	crit, _, cerr := TouchesCritical(dir, mf.Files)
	if cerr != nil {
		return Set{}, cerr
	}
	mf.TouchesCritical = crit
	mf.Sign(priv)
	body, _ := json.MarshalIndent(mf, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o644); err != nil {
		return Set{}, err
	}
	return m.Get(id)
}

// Archive moves a queued set into archive/ with a terminal state marker.
func (m *Manager) Archive(id string, state SetState) error {
	src := filepath.Join(m.queueDir(), id)
	dst := filepath.Join(m.archiveDir(), id)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no queued patch set %q", id)
	}
	if err := os.MkdirAll(m.archiveDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(src, ".state"), []byte(state), 0o644); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// MarkConflicted flags a queued set whose rebase or merge failed — it stays
// visible in the queue (the operator decides: rebase it, or reject it to the
// archive) but can never be proposed for merge while marked.
func (m *Manager) MarkConflicted(id string) error {
	dir := filepath.Join(m.queueDir(), id)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no queued patch set %q", id)
	}
	return os.WriteFile(filepath.Join(dir, ".state"), []byte(StateConflicted), 0o644)
}

// ClearConflicted lifts the marker after a successful rebase — a set that
// was never conflicted has nothing to lift.
func (m *Manager) ClearConflicted(id string) error {
	err := os.Remove(filepath.Join(m.queueDir(), id, ".state"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnsureWorktree creates the merge worktree on first use: a detached git
// worktree of the configured repo. It refuses to reuse a directory that is
// not a worktree of the right repo — the merge pipeline's blast radius must
// be provably contained.
func (m *Manager) EnsureWorktree(ctx context.Context) error {
	if m.repo == "" {
		return fmt.Errorf("self_evolution.repo is not configured — point it at this node's OpenPanda checkout")
	}
	wt := m.WorktreePath()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return err
	}
	out, err := gitOut(ctx, m.repo, "worktree", "add", "--detach", wt, "HEAD")
	if err != nil {
		return fmt.Errorf("git worktree add: %s", out)
	}
	return nil
}

// Apply applies a queued set's patches to the worktree in manifest order.
// The worktree is reset hard to the manifest's base first: each set lands on
// a clean, declared foundation, never on top of a previous set's leftovers.
func (m *Manager) Apply(ctx context.Context, id string) error {
	set, err := m.Get(id)
	if err != nil {
		return err
	}
	if set.State != StatePending {
		return fmt.Errorf("patch set %s is %s — only pending sets apply", id, set.State)
	}
	if err := m.EnsureWorktree(ctx); err != nil {
		return err
	}
	wt := m.WorktreePath()
	// reset --hard (not checkout) also clears intent-to-add index entries —
	// files minted via `git add -N` must not survive into the clean base or
	// the patch below fails with "already exists".
	if out, err := gitOut(ctx, wt, "reset", "--hard", set.Manifest.Base); err != nil {
		return fmt.Errorf("worktree reset %s: %s", set.Manifest.Base, out)
	}
	if out, err := gitOut(ctx, wt, "clean", "-fd"); err != nil {
		return fmt.Errorf("worktree clean: %s", out)
	}
	for _, f := range set.Manifest.Files {
		if out, err := gitOut(ctx, wt, "apply", "--whitespace=nowarn", filepath.Join(set.Dir, f)); err != nil {
			return fmt.Errorf("git apply %s: %s", f, out)
		}
	}
	return nil
}

// RebaseOnto rewrites a queued set onto a new base (design §7 q3): apply onto
// the new base, regenerate the diff, re-mint the manifest with the SAME id
// and producer signature preserved is impossible — the local node re-signs
// the rebased set with its own key and notes the origin in the title. This
// node's approval gate then sees exactly one signer to judge: the re-baser.
func (m *Manager) RebaseOnto(ctx context.Context, id, newBase string, pub ed25519.PublicKey, priv ed25519.PrivateKey) error {
	set, err := m.Get(id)
	if err != nil {
		return err
	}
	if err := m.EnsureWorktree(ctx); err != nil {
		return err
	}
	wt := m.WorktreePath()
	// Same pinning as Mint: the recorded base must be a commit, not a ref that
	// will move out from under the archived manifest.
	if out, err := gitOut(ctx, wt, "rev-parse", newBase); err != nil {
		return fmt.Errorf("resolve base %s: %s", newBase, out)
	} else {
		newBase = strings.TrimSpace(out)
	}
	if out, err := gitOut(ctx, wt, "reset", "--hard", newBase); err != nil {
		return fmt.Errorf("worktree reset %s: %s", newBase, out)
	}
	if out, err := gitOut(ctx, wt, "clean", "-fd"); err != nil {
		return fmt.Errorf("worktree clean: %s", out)
	}
	for _, f := range set.Manifest.Files {
		if out, err := gitOut(ctx, wt, "apply", "--3way", "--whitespace=nowarn", filepath.Join(set.Dir, f)); err != nil {
			return fmt.Errorf("rebase apply %s: %s", f, out)
		}
	}
	diff, err := gitOut(ctx, wt, "diff", newBase, "--binary")
	if err != nil || strings.TrimSpace(diff) == "" {
		return fmt.Errorf("rebased set is empty against %s", newBase)
	}
	patch := "0001-rebased.patch"
	for _, f := range set.Manifest.Files {
		os.Remove(filepath.Join(set.Dir, f))
	}
	if err := os.WriteFile(filepath.Join(set.Dir, patch), []byte(diff), 0o644); err != nil {
		return err
	}
	digests, err := patchDigests(set.Dir, []string{patch})
	if err != nil {
		return err
	}
	mf := set.Manifest
	mf.Base = newBase
	mf.Files = []string{patch}
	mf.Digests = digests
	// The re-baser signs — so the producer id must become the re-baser's key;
	// the original producer's signature cannot be forged and must not be
	// implied. The title preserves who authored the change.
	mf.Producer = "k:" + hex.EncodeToString(pub)
	mf.Title = set.Manifest.Title + " (rebased " + shortBase(set.Manifest.Base) + " → " + shortBase(newBase) +
		"; orig " + shortBase(strings.TrimPrefix(set.Manifest.Producer, "k:")) + ")"
	crit, _, cerr := TouchesCritical(set.Dir, mf.Files)
	if cerr != nil {
		return cerr
	}
	mf.TouchesCritical = crit
	mf.Sign(priv)
	body, _ := json.MarshalIndent(mf, "", "  ")
	if err := os.WriteFile(filepath.Join(set.Dir, "manifest.json"), body, 0o644); err != nil {
		return err
	}
	return m.ClearConflicted(id)
}

// Reset removes the worktree and (optionally) every queued/archived set —
// the `panda self reset` escape hatch back to "no self-evolution state".
func (m *Manager) Reset(ctx context.Context, purgeQueue bool) error {
	wt := m.WorktreePath()
	if _, err := os.Stat(wt); err == nil {
		if m.repo != "" {
			// Ask git to drop the worktree registration, then remove whatever
			// is left; a manually damaged worktree still gets cleaned.
			_, _ = gitOut(ctx, m.repo, "worktree", "remove", "--force", wt)
		}
		if err := os.RemoveAll(wt); err != nil {
			return err
		}
	}
	if purgeQueue {
		for _, d := range []string{m.queueDir(), m.archiveDir()} {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
	}
	return nil
}

func shortBase(b string) string {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}

// GitHead resolves repo's HEAD commit — the base a rebase sweep targets.
func GitHead(ctx context.Context, repo string) (string, error) {
	out, err := gitOut(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %s", out)
	}
	return strings.TrimSpace(out), nil
}

// SameRef reports whether ref and head resolve to the same commit in repo —
// a patch set minted against a tag/branch name stays current while that ref
// still points at the same sha.
func SameRef(ctx context.Context, repo, ref, head string) bool {
	out, err := gitOut(ctx, repo, "rev-parse", ref)
	return err == nil && strings.TrimSpace(out) == head
}

// gitOut runs git in dir and returns combined output; errors carry the
// command's own stderr so callers surface git's words, not a wrapper's guess.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyDir duplicates a patch-set directory (manifest + patches + markers).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, body, info.Mode().Perm())
	})
}
