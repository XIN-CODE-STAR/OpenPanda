// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/cliui"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/security"
	"github.com/Xustalis/OpenPanda/internal/self"
	"github.com/Xustalis/OpenPanda/internal/updater"
	versionpkg "github.com/Xustalis/OpenPanda/internal/version"
)

// runSelf implements `panda self <verb>` — the self-evolution pipeline's
// operator surface (design doc docs/author/2026-10-21). All verbs are local:
// the queue, worktree and archive live under <data>/self/, and the only
// mutation with cross-process effect is `propose`, which parks a review task
// on the shared store for `panda approve` / the console to settle.
func runSelf(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: panda self <status|queue|doctor|mint|propose|rebase|rollback|reset> [args]")
		os.Exit(2)
	}
	switch args[0] {
	case "status":
		runSelfStatus(args[1:])
	case "queue":
		runSelfQueue(args[1:])
	case "doctor":
		runSelfDoctor(args[1:])
	case "mint":
		runSelfMint(args[1:])
	case "propose":
		runSelfPropose(args[1:])
	case "rebase":
		runSelfRebase(args[1:])
	case "rollback":
		runSelfRollback(args[1:])
	case "reset":
		runSelfReset(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "panda self: unknown verb %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: panda self <status|queue|doctor|mint|propose|rebase|rollback|reset>")
		os.Exit(2)
	}
}

// selfFlags wires the shared --config flag every verb accepts.
func selfFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("self "+name, flag.ExitOnError)
	configPath := fs.String("config", cliConfigPath, "path to config.yaml")
	return fs, configPath
}

// selfManager builds the manager over the configured state root. Strict mode
// fails when the feature is disabled — mint/propose/rebase refuse on a node
// that never opted in, while status/doctor stay readable either way.
func selfManager(cfg *config.Config, strict bool) *self.Manager {
	if strict && !cfg.SelfEvolution.Enabled {
		fatal("self", fmt.Errorf("self_evolution.enabled is false — enable it (and set self_evolution.repo) in config.yaml first"))
	}
	return self.New(cfg.SelfEvolutionDir(), cfg.SelfEvolution.Repo, nil)
}

func runSelfStatus(args []string) {
	fs, configPath := selfFlags("status")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self status")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	p := pal()
	mgr := selfManager(cfg, false)
	fmt.Println(p.Bold("self-evolution"))
	fmt.Printf("  enabled:    %v\n", cfg.SelfEvolution.Enabled)
	fmt.Printf("  provenance: %s\n", versionpkg.ProvenanceClass())
	fmt.Printf("  state dir:  %s\n", mgr.Root())
	fmt.Printf("  repo:       %s\n", orDash(cfg.SelfEvolution.Repo))
	if fb := updater.ReleaseFallbackPath(); fb != "" {
		mark := "present"
		if _, err := os.Stat(fb); err != nil {
			mark = "MISSING — the first release-channel update creates it"
		}
		fmt.Printf("  rollback:   %s (%s)\n", fb, mark)
	}
	if _, err := os.Stat(filepath.Join(mgr.WorktreePath(), ".git")); err == nil {
		fmt.Printf("  worktree:   %s\n", mgr.WorktreePath())
	}
	sets, err := mgr.Queue()
	if err != nil {
		fatal("read queue", err)
	}
	if len(sets) == 0 {
		fmt.Println("  queue:      empty")
		return
	}
	fmt.Println("  queue:")
	for _, s := range sets {
		printSetLine(p, s)
	}
}

func runSelfQueue(args []string) {
	fs, configPath := selfFlags("queue")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self queue")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	mgr := selfManager(cfg, false)
	sets, err := mgr.Queue()
	if err != nil {
		fatal("read queue", err)
	}
	p := pal()
	if len(sets) == 0 {
		fmt.Println(p.Muted("no patch sets"))
		return
	}
	for _, s := range sets {
		printSetLine(p, s)
	}
}

func printSetLine(p cliui.Palette, s self.Set) {
	crit := ""
	if s.Critical {
		crit = " " + p.Warn("[critical]")
	}
	fmt.Printf("  %-22s %-10s %-26s %s%s\n", s.Manifest.ID, s.State, s.Manifest.Version, s.Manifest.Title, crit)
}

func runSelfDoctor(args []string) {
	fs, configPath := selfFlags("doctor")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self doctor")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	mgr := selfManager(cfg, false)
	checks := mgr.Doctor(context.Background())
	p := pal()
	bad := 0
	for _, c := range checks {
		mark := p.MarkOK()
		if !c.OK {
			mark = p.MarkFail()
			bad++
		}
		line := fmt.Sprintf("  %-28s %s", c.Name, mark)
		if c.Note != "" {
			line += "  " + p.Muted(c.Note)
		}
		fmt.Println(line)
	}
	if bad > 0 {
		fmt.Println(p.Warn(fmt.Sprintf("%d check(s) missing — the merge gate cannot run here yet", bad)))
		os.Exit(1)
	}
}

// runSelfMint packages the worktree's diff against --base as a signed patch
// set — the producing half of the loop. Whether the diff was authored by a
// human or an agent task is irrelevant to the format.
func runSelfMint(args []string) {
	fs, configPath := selfFlags("mint")
	base := fs.String("base", "HEAD", "git ref the worktree diff is taken against")
	title := fs.String("title", "", "human summary shown in review (required)")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self mint --title TEXT [--base REF]")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	if strings.TrimSpace(*title) == "" {
		fatal("mint", fmt.Errorf("--title is required — the review gate shows it"))
	}
	mgr := selfManager(cfg, true)
	db, _, err := panelStore(cfg)
	if err != nil {
		fatal("open store", err)
	}
	defer db.Close()
	pub, priv, ok := core.LoadNodeKey(db)
	if !ok {
		fatal("node key", fmt.Errorf("no Ed25519 identity yet — run the daemon once"))
	}
	set, err := mgr.Mint(context.Background(), *base, *title,
		mgr.NextVersion(versionpkg.Version), pub, priv, self.GateDigest{})
	if err != nil {
		fatal("mint", err)
	}
	fmt.Printf("queued %s → %s%s\n", set.Manifest.ID, set.Manifest.Version, critNote(set))
	fmt.Println(pal().Muted("review with: panda self propose " + set.Manifest.ID))
}

func runSelfPropose(args []string) {
	fs, configPath := selfFlags("propose")
	fs.Parse(reorderFlags(fs, args))
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: panda self propose <patchset-id|dir>")
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	mgr := selfManager(cfg, true)
	// A path argument imports the set first — that is how agent-produced or
	// (phase-5) mesh-delivered sets enter the queue.
	if st, err := os.Stat(id); err == nil && st.IsDir() {
		set, err := mgr.Import(id)
		if err != nil {
			fatal("import patch set", err)
		}
		fmt.Println(pal().Muted("imported " + set.Manifest.ID))
		id = set.Manifest.ID
	}
	db, _, err := panelStore(cfg)
	if err != nil {
		fatal("open store", err)
	}
	defer db.Close()
	eng := selfCore(cfg, db, true)
	t, err := eng.ProposeSelfMerge(context.Background(), id)
	if err != nil {
		fatal("propose", err)
	}
	fmt.Printf("merge proposal %s parked for review\n", t.TaskID)
	fmt.Println(pal().Muted(fmt.Sprintf("review: panda task show %s · approve: panda approve %s", t.TaskID, t.TaskID)))
}

func runSelfRebase(args []string) {
	fs, configPath := selfFlags("rebase")
	fs.Parse(reorderFlags(fs, args))
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: panda self rebase <patchset-id> <new-base-ref>")
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	mgr := selfManager(cfg, true)
	db, _, err := panelStore(cfg)
	if err != nil {
		fatal("open store", err)
	}
	defer db.Close()
	pub, priv, ok := core.LoadNodeKey(db)
	if !ok {
		fatal("node key", fmt.Errorf("no Ed25519 identity yet — run the daemon once"))
	}
	if err := mgr.RebaseOnto(context.Background(), rest[0], rest[1], pub, priv); err != nil {
		fatal("rebase", err)
	}
	fmt.Printf("%s rebased onto %s\n", rest[0], rest[1])
}

// runSelfRollback restores the retained release binary over the current one —
// the escape hatch when a self build misbehaves. It is the one `self` verb
// that works with the feature disabled: rollback must never be gated behind
// the thing it rescues you from.
func runSelfRollback(args []string) {
	fs, configPath := selfFlags("rollback")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self rollback")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	db, _, err := panelStore(cfg)
	if err != nil {
		fatal("open store", err)
	}
	defer db.Close()
	if err := updater.RollbackToRelease(); err != nil {
		fatal("rollback", err)
	}
	// Audit the rollback on the signed chain — reverting the running binary
	// is exactly the kind of action the log exists for.
	_ = core.NewSignedAudit(db).Record(context.Background(), security.Entry{
		Who:    core.RuntimeNodeID(cfg.Node.Name, cfg.Node.Kind, cfg.Node.EffectiveIdentity()),
		What:   "self:rollback",
		Result: "applied",
		Detail: "restored " + updater.ReleaseFallbackPath() + " over " + versionpkg.Version,
	})
	fmt.Println("restored " + updater.ReleaseFallbackPath())
	fmt.Println(pal().Muted("restart the daemon to run it (launchd/systemd: restart the service)"))
}

func runSelfReset(args []string) {
	fs, configPath := selfFlags("reset")
	purge := fs.Bool("purge", false, "also delete every queued and archived patch set")
	fs.Parse(reorderFlags(fs, args))
	rejectExtraArgs(fs, "usage: panda self reset [--purge]")
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	// reset is the escape hatch back to a pure release node (design §5): put
	// the retained release binary back first, then remove the state. It stays
	// available after the feature was disabled — the state may predate that.
	mgr := selfManager(cfg, false)
	if versionpkg.ProvenanceClass() == versionpkg.ProvenanceSelf {
		if err := updater.RollbackToRelease(); err != nil {
			fmt.Println(pal().Warn("release binary not restored: " + err.Error()))
		} else {
			fmt.Println("restored " + updater.ReleaseFallbackPath())
		}
	}
	if err := mgr.Reset(context.Background(), *purge); err != nil {
		fatal("reset", err)
	}
	fmt.Println("self-evolution state cleared" + map[bool]string{true: " (worktree + all sets)", false: " (worktree)"}[*purge])
}

// selfCore builds the minimal Core a CLI verb needs — propose parks a row on
// the shared store; the daemon settles the review like any other. oneShot
// marks it so a merge approval never re-execs this CLI process.
func selfCore(cfg *config.Config, db *sql.DB, oneShot bool) *core.Core {
	var card ledger.Card
	if p := defaultCardPath(); p != "" {
		if c, err := ledger.LoadCard(p); err == nil {
			card = c
		}
	}
	nodeID := core.RuntimeNodeID(cfg.Node.Name, cfg.Node.Kind, cfg.Node.EffectiveIdentity())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := core.NewCore(db, nodeID, card, schedulerTier(cfg.Node.ResourceClass), logger, cfg.Model)
	c.SetSelfEvolution(self.New(cfg.SelfEvolutionDir(), cfg.SelfEvolution.Repo, logger), oneShot, cfg.SelfEvolution.Producers)
	return c
}

func critNote(s self.Set) string {
	if s.Critical {
		return "  [touches critical paths]"
	}
	return ""
}
