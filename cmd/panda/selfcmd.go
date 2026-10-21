// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/cliui"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/i18n"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/security"
	"github.com/Xustalis/OpenPanda/internal/self"
	"github.com/Xustalis/OpenPanda/internal/updater"
	versionpkg "github.com/Xustalis/OpenPanda/internal/version"
)

// runSelf implements `panda self <verb>` — the self-evolution pipeline's
// operator surface (design doc docs/plans/design-self-evolution.md). All
// verbs are local: the queue, worktree and archive live under <data>/self/,
// and the only mutation with cross-process effect is `propose`, which parks
// a review task on the shared store for `panda approve` / the console.
func runSelf(args []string) {
	loc := i18n.Detect()
	if len(args) == 0 {
		fail(2, i18n.T(loc, "cli.self.usage"))
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
		failf(2, i18n.T(loc, "cli.self.unknown")+"\n"+i18n.T(loc, "cli.self.usage")+"\n", args[0])
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
	loc := i18n.Detect()
	p := pal()
	mgr := selfManager(cfg, false)
	fmt.Println(p.Bold(i18n.T(loc, "cli.self.status.title")))
	fmt.Printf("  %-12s %v\n", i18n.T(loc, "cli.self.status.enabled")+":", cfg.SelfEvolution.Enabled)
	fmt.Printf("  %-12s %s\n", i18n.T(loc, "cli.self.status.provenance")+":", versionpkg.ProvenanceClass())
	fmt.Printf("  %-12s %s\n", i18n.T(loc, "cli.self.status.statedir")+":", mgr.Root())
	fmt.Printf("  %-12s %s\n", i18n.T(loc, "cli.self.status.repo")+":", orDash(cfg.SelfEvolution.Repo))
	if fb := updater.ReleaseFallbackPath(); fb != "" {
		mark := i18n.T(loc, "cli.self.status.present")
		if _, err := os.Stat(fb); err != nil {
			mark = i18n.T(loc, "cli.self.status.missing")
		}
		fmt.Printf("  %-12s %s (%s)\n", i18n.T(loc, "cli.self.status.rollback")+":", fb, mark)
	}
	if _, err := os.Stat(filepath.Join(mgr.WorktreePath(), ".git")); err == nil {
		fmt.Printf("  %-12s %s\n", i18n.T(loc, "cli.self.status.worktree")+":", mgr.WorktreePath())
	}
	sets, err := mgr.Queue()
	if err != nil {
		fatal("read queue", err)
	}
	if len(sets) == 0 {
		fmt.Printf("  %-12s %s\n", i18n.T(loc, "cli.self.status.queue")+":", i18n.T(loc, "cli.self.status.empty"))
		return
	}
	fmt.Printf("  %s:\n", i18n.T(loc, "cli.self.status.queue"))
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
		fmt.Println(p.Muted(i18n.T(i18n.Detect(), "cli.self.queue.empty")))
		return
	}
	for _, s := range sets {
		printSetLine(p, s)
	}
}

func printSetLine(p cliui.Palette, s self.Set) {
	crit := ""
	if s.Critical {
		crit = " " + p.Warn(i18n.T(i18n.Detect(), "cli.self.mint.critical"))
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
	loc := i18n.Detect()
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
		fmt.Println(p.Warn(fmt.Sprintf(i18n.T(loc, "cli.self.doctor.missing"), bad)))
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
	loc := i18n.Detect()
	rejectExtraArgs(fs, i18n.T(loc, "cli.self.mint.usage"))
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	if strings.TrimSpace(*title) == "" {
		fatal("mint", fmt.Errorf("%s", i18n.T(loc, "cli.self.mint.title")))
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
	note := ""
	if set.Critical {
		note = i18n.T(loc, "cli.self.mint.critical")
	}
	fmt.Printf(i18n.T(loc, "cli.self.mint.queued")+"\n", set.Manifest.ID, set.Manifest.Version, note)
	fmt.Println(pal().Muted(fmt.Sprintf(i18n.T(loc, "cli.self.mint.hint"), set.Manifest.ID)))
}

func runSelfPropose(args []string) {
	fs, configPath := selfFlags("propose")
	fs.Parse(reorderFlags(fs, args))
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	loc := i18n.Detect()
	if id == "" {
		fail(2, i18n.T(loc, "cli.self.propose.usage"))
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
		fmt.Println(pal().Muted(fmt.Sprintf(i18n.T(loc, "cli.self.propose.imported"), set.Manifest.ID)))
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
	fmt.Printf(i18n.T(loc, "cli.self.propose.parked")+"\n", t.TaskID)
	fmt.Println(pal().Muted(fmt.Sprintf(i18n.T(loc, "cli.self.propose.hint"), t.TaskID, t.TaskID)))
}

func runSelfRebase(args []string) {
	fs, configPath := selfFlags("rebase")
	fs.Parse(reorderFlags(fs, args))
	rest := fs.Args()
	loc := i18n.Detect()
	if len(rest) != 2 {
		fail(2, i18n.T(loc, "cli.self.rebase.usage"))
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
	// A rebase re-signs the set with THIS node's key. Under producers=self
	// that would turn a foreign set — which propose refuses — into a locally
	// signed, mergeable one, so the operator path carries the same admission
	// rule as the boot-time sweep.
	set, err := mgr.Get(rest[0])
	if err != nil {
		fatal("rebase", err)
	}
	if !self.ProducerAdmitted(cfg.SelfEvolution.Producers, set.Manifest.Producer, pub) {
		fatal("rebase", fmt.Errorf("patch set %s is signed by %s, which self_evolution.producers=%q does not admit on this node",
			set.Manifest.ID, set.Manifest.Producer, self.ProducerMode(cfg.SelfEvolution.Producers)))
	}
	if err := mgr.RebaseOnto(context.Background(), rest[0], rest[1], pub, priv); err != nil {
		fatal("rebase", err)
	}
	fmt.Printf(i18n.T(loc, "cli.self.rebase.done")+"\n", rest[0], rest[1])
}

// runSelfRollback restores the retained release binary over the current one —
// the escape hatch when a self build misbehaves. It is the one `self` verb
// that works with the feature disabled: rollback must never be gated behind
// the thing it rescues you from.
func runSelfRollback(args []string) {
	fs, configPath := selfFlags("rollback")
	fs.Parse(reorderFlags(fs, args))
	loc := i18n.Detect()
	rejectExtraArgs(fs, i18n.T(loc, "cli.self.rollback.usage"))
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	db, _, err := panelStore(cfg)
	if err != nil {
		fatal("open store", err)
	}
	defer db.Close()
	floor, err := schemaFloor(db)
	if err != nil {
		// Fail closed: without the live floor the fallback's own reading
		// describes a different data directory, so compatibility cannot be
		// proven. Rolling back blind is exactly how a node gets bricked.
		fatal("read schema floor", err)
	}
	if err := updater.RollbackToRelease(floor); err != nil {
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
	fmt.Println(fmt.Sprintf(i18n.T(loc, "cli.self.rollback.restored"), updater.ReleaseFallbackPath()))
	fmt.Println(pal().Muted(i18n.T(loc, "cli.self.rollback.restart")))
}

func runSelfReset(args []string) {
	fs, configPath := selfFlags("reset")
	purge := fs.Bool("purge", false, "also delete every queued and archived patch set")
	force := fs.Bool("force", false, "clear the self state even if the release binary cannot be restored")
	fs.Parse(reorderFlags(fs, args))
	loc := i18n.Detect()
	rejectExtraArgs(fs, i18n.T(loc, "cli.self.reset.usage"))
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	// reset is the escape hatch back to a pure release node: put the retained
	// release binary back first, then remove the state. It stays available
	// after the feature was disabled — the state may predate that.
	mgr := selfManager(cfg, false)
	if versionpkg.ProvenanceClass() == versionpkg.ProvenanceSelf {
		restoreErr := restoreReleaseForReset(cfg)
		switch {
		case restoreErr == nil:
			fmt.Println(fmt.Sprintf(i18n.T(loc, "cli.self.reset.restored"), updater.ReleaseFallbackPath()))
		case errors.Is(restoreErr, errNoReleaseFallback):
			// Nothing signed to put back. Clearing the state is still the
			// escape hatch back to "no self-evolution state".
			fmt.Println(pal().Warn(fmt.Sprintf(i18n.T(loc, "cli.self.reset.restoreFailed"), restoreErr)))
		case *force:
			fmt.Println(pal().Warn(fmt.Sprintf(i18n.T(loc, "cli.self.reset.restoreFailed"), restoreErr)))
		default:
			// The restore is the point of reset while a self build runs.
			// Clearing the queue/archive now would leave the node on the bad
			// binary with its last recovery material deleted. Stop instead.
			fatal("reset", fmt.Errorf("%w (use --force to clear the self state anyway)", restoreErr))
		}
	}
	if err := mgr.Reset(context.Background(), *purge); err != nil {
		fatal("reset", err)
	}
	suffix := i18n.T(loc, "cli.self.reset.worktree")
	if *purge {
		suffix = i18n.T(loc, "cli.self.reset.purged")
	}
	fmt.Println(fmt.Sprintf(i18n.T(loc, "cli.self.reset.cleared"), suffix))
}

// errNoReleaseFallback marks "there is nothing installed to restore". It is
// the one restore failure that may be ignored: with no signed fallback on
// disk there is no binary to put back, so clearing the self state is still a
// valid escape hatch.
var errNoReleaseFallback = errors.New("no release fallback")

// restoreReleaseForReset puts the retained release binary back before reset
// erases the state. A missing fallback is reported as errNoReleaseFallback;
// every other failure means a fallback exists but could not be proven safe,
// and the caller must not clear the state and strand the node on the self
// build.
func restoreReleaseForReset(cfg *config.Config) error {
	fallback := updater.ReleaseFallbackPath()
	if fallback == "" {
		return errNoReleaseFallback
	}
	if _, err := os.Stat(fallback); err != nil {
		return fmt.Errorf("%w: %v", errNoReleaseFallback, err)
	}
	db, _, err := panelStore(cfg)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()
	floor, err := schemaFloor(db)
	if err != nil {
		return err
	}
	return updater.RollbackToRelease(floor)
}

// schemaFloor reports the live data directory's migration level. The rollback
// guard needs it: the fallback binary is probed in isolation and cannot see
// the store it would be rolled back onto. An unreadable floor is an ERROR, not
// zero: zero means "nothing to protect" to the guard, which would turn a
// failed read into a fail-open rollback.
func schemaFloor(db *sql.DB) (int, error) {
	if db == nil {
		return 0, errors.New("no store to read the schema floor from")
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema floor: %w", err)
	}
	return v, nil
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
