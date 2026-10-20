// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// TestReconcileStopsOutOfBandTerminal is the zombie-execution half of "no
// silent stall": a task this process is executing can be cancelled or
// force-failed by another process sharing the store — the row goes terminal
// while the agent subprocess here keeps running. reconcileLocalWork is the
// pass that notices and kills it; without it the execution outlives its own
// verdict until the agent hard timeout.
func TestReconcileStopsOutOfBandTerminal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := NewCore(db, "worker", ledger.Card{}, 5, testLogger(), config.ModelConfig{})
	c.SetWorkDir(t.TempDir())

	// A task this process is "executing": registered like a real run.
	tk, err := c.store.Create(ctx, "", "", "orphaned exec", "worker", []string{"worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.store.Queue(ctx, tk.TaskID, "worker"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, "worker", "worker"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := c.store.Accept(ctx, tk.TaskID, "worker"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	c.registerRunning(tk.TaskID, execCancel)
	c.pendingCtx.Store(tk.TaskID, &pendingContext{})

	// Another process cancels the row out of band.
	if err := c.store.Cancel(ctx, tk.TaskID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.reconcileLocalWork(ctx)

	select {
	case <-execCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("a task terminalized out of band must stop its local execution")
	}
	if _, ok := c.running.Load(tk.TaskID); ok {
		t.Fatal("cancelled execution should have been unregistered")
	}
	if _, ok := c.pendingCtx.Load(tk.TaskID); ok {
		t.Fatal("the parked-context entry must be dropped with its reservation")
	}
}

// TestReconcileLeavesLiveExecutions guards the other direction: a task whose
// row is still active is this process's own work — reconcile must not touch
// it no matter how stale it looks here.
func TestReconcileLeavesLiveExecutions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := NewCore(db, "worker", ledger.Card{}, 5, testLogger(), config.ModelConfig{})
	c.SetWorkDir(t.TempDir())

	tk, err := c.store.Create(ctx, "", "", "live exec", "worker", []string{"worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.store.Queue(ctx, tk.TaskID, "worker"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, "worker", "worker"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := c.store.Accept(ctx, tk.TaskID, "worker"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	c.registerRunning(tk.TaskID, execCancel)
	c.pendingCtx.Store(tk.TaskID, &pendingContext{})

	c.reconcileLocalWork(ctx)

	select {
	case <-execCtx.Done():
		t.Fatal("a live task must never be reconciled away")
	default:
	}
	if _, ok := c.running.Load(tk.TaskID); !ok {
		t.Fatal("the live execution's registration must stay")
	}
}

// TestSettleExpiredKillGate pins the monitor's post-settle kill rule: a
// settled expiry kills the snapshotted run (re-route or terminal alike), but
// a skipped settle means a concurrent transition owns the row — an ACTIVE
// row must never have its execution murdered out from under it, while a row
// that closed concurrently still needs its zombie reaped.
func TestSettleExpiredKillGate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := NewCore(db, "worker", ledger.Card{}, 5, testLogger(), config.ModelConfig{})
	c.SetWorkDir(t.TempDir())

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	runningTask := func(title string) Task {
		tk, err := c.store.Create(ctx, "", "", title, "worker", []string{"worker"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		must(c.store.Queue(ctx, tk.TaskID, "worker"))
		must(c.store.Dispatch(ctx, tk.TaskID, "worker", "worker"))
		must(c.store.Accept(ctx, tk.TaskID, "worker"))
		return tk
	}

	// Case 1: the scan view is stale — the row was re-claimed to running
	// between scan and settle. The settle skips; the live run survives.
	live := runningTask("reclaimed")
	liveCtx, liveCancel := context.WithCancel(ctx)
	defer liveCancel()
	c.registerRunning(live.TaskID, liveCancel)
	out := c.settleExpired(ctx, ExpiredRow{
		TaskID: live.TaskID, State: StateDispatched, Owner: "worker",
	})
	if out != ExpirySkipped {
		t.Fatalf("outcome = %s, want skipped (row moved dispatched->running)", out)
	}
	select {
	case <-liveCtx.Done():
		t.Fatal("a skipped settle murdered a live reclaimed run")
	default:
	}
	if _, ok := c.running.Load(live.TaskID); !ok {
		t.Fatal("the live run's registration was dropped")
	}

	// Case 2: skipped because the row already closed — the zombie still dies.
	closed := runningTask("closed-concurrent")
	closedCtx, closedCancel := context.WithCancel(ctx)
	defer closedCancel()
	c.registerRunning(closed.TaskID, closedCancel)
	must(c.store.ForceFail(ctx, closed.TaskID, "lost"))
	out = c.settleExpired(ctx, ExpiredRow{
		TaskID: closed.TaskID, State: StateRunning, Owner: "worker",
	})
	if out != ExpirySkipped {
		t.Fatalf("outcome = %s, want skipped (row already failed)", out)
	}
	select {
	case <-closedCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("a concurrently-closed row must still reap its zombie run")
	}

	// Case 3: a genuine lease expiry — the row settles to re-queue and the
	// registered run is the kill target.
	exp := runningTask("expired")
	expCtx, expCancel := context.WithCancel(ctx)
	defer expCancel()
	c.registerRunning(exp.TaskID, expCancel)
	if _, err := c.store.db.ExecContext(ctx,
		`UPDATE tasks SET lease_expires_at=? WHERE task_id=?`, time.Now().Unix()-1, exp.TaskID); err != nil {
		t.Fatalf("stamp lease: %v", err)
	}
	out = c.settleExpired(ctx, ExpiredRow{
		TaskID: exp.TaskID, State: StateRunning, Owner: "worker",
	})
	if out != ExpiryRerouted {
		t.Fatalf("outcome = %s, want rerouted", out)
	}
	select {
	case <-expCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("the expired run must be cancelled after the re-queue settles")
	}
	if _, ok := c.running.Load(exp.TaskID); ok {
		t.Fatal("the expired run's handle must leave the registry")
	}
}
