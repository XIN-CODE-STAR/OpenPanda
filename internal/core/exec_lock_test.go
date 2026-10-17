// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/scheduler/queue"
	"github.com/Xustalis/OpenPanda/internal/storage"
)

// TestExecLockKeys pins the key derivation: the queue registry's set (explicit
// keys, else the project, else the shared anonymous lock) plus the resolved
// physical actuator — a delegated actuator task carries no queue keys at all,
// and the device must still exclude every other driver.
func TestExecLockKeys(t *testing.T) {
	if got := execLockKeys(Task{Project: "p"}, "hardware:servo"); !slices.Equal(got, []string{"project:p", "hardware:servo"}) {
		t.Fatalf("project+actuator keys = %v", got)
	}
	if got := execLockKeys(Task{ResourceKeys: []string{"plan:1:a", "hardware:servo"}}, "hardware:servo"); !slices.Equal(got, []string{"plan:1:a", "hardware:servo"}) {
		t.Fatalf("explicit keys must not duplicate the actuator: %v", got)
	}
	if got := execLockKeys(Task{}, ""); !slices.Equal(got, []string{"agent:*"}) {
		t.Fatalf("anonymous keys = %v", got)
	}
}

// TestExecLocksStore pins the persistence semantics on the shared table: a
// live holder blocks foreign acquirers, a lapsed lease / terminal row makes
// the holder stale and its keys stealable, and the pre-accept window
// (dispatched, no lease yet) counts as live. Two stores on one file stand in
// for two core processes on one host.
func TestExecLocksStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exec.db")
	openShared := func() *sql.DB {
		t.Helper()
		db, err := storage.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		t.Cleanup(func() { db.Close() })
		if err := storage.Migrate(db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return db
	}
	s1 := NewTaskStore(openShared(), testLogger())
	s2 := NewTaskStore(openShared(), testLogger())
	keys := []string{"project:proj"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// (1) A live running holder blocks a foreign acquirer.
	h := createTask(t, s1, "", "holder", "n1")
	must(s1.Queue(ctx, h.TaskID, "n1"))
	must(s1.Dispatch(ctx, h.TaskID, "n1", "n1"))
	must(s1.Accept(ctx, h.TaskID, "n1"))
	must(s1.SetLease(ctx, h.TaskID, 600_000))
	ok, err := s1.TryAcquireExecLocks(ctx, h.TaskID, "n1", "run-h", keys)
	if err != nil || !ok {
		t.Fatalf("live hold: ok=%v err=%v", ok, err)
	}
	if ok, err = s2.TryAcquireExecLocks(ctx, "task-2", "n2", "run-2", keys); err != nil || ok {
		t.Fatalf("foreign acquire beside a live holder: ok=%v err=%v, want blocked", ok, err)
	}

	// (2) A lapsed lease marks the holder a crashed run; the next acquirer
	// steals the keys instead of waiting on a dead process forever.
	if _, err := s1.db.ExecContext(ctx, `UPDATE tasks SET lease_expires_at=1 WHERE task_id=?`, h.TaskID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if ok, err = s2.TryAcquireExecLocks(ctx, "task-2", "n2", "run-2", keys); err != nil || !ok {
		t.Fatalf("steal over a lapsed lease: ok=%v err=%v", ok, err)
	}
	must(s2.ReleaseExecLocks(ctx, "task-2", "run-2"))

	// (3) The dead holder's own task can also take its rows back (a restarted
	// predecessor), and once that row is terminal any acquirer may.
	if ok, err = s1.TryAcquireExecLocks(ctx, h.TaskID, "n1", "run-h", keys); err != nil || !ok {
		t.Fatalf("self takeover over its own leftovers: ok=%v err=%v", ok, err)
	}
	must(s1.Fail(ctx, h.TaskID, "n1", "crashed"))
	if ok, err = s2.TryAcquireExecLocks(ctx, "task-2", "n2", "run-2", keys); err != nil || !ok {
		t.Fatalf("steal over a terminal holder: ok=%v err=%v", ok, err)
	}
	must(s2.ReleaseExecLocks(ctx, "task-2", "run-2"))

	// (4) A dispatched row with no lease yet is the pre-accept window of a
	// live run, not a leftover: foreign acquirers wait.
	h2 := createTask(t, s1, "", "preaccept", "n1")
	must(s1.Queue(ctx, h2.TaskID, "n1"))
	must(s1.Dispatch(ctx, h2.TaskID, "n1", "n1"))
	if ok, err = s1.TryAcquireExecLocks(ctx, h2.TaskID, "n1", "run-h2", keys); err != nil || !ok {
		t.Fatalf("pre-accept hold: ok=%v err=%v", ok, err)
	}
	if ok, err = s2.TryAcquireExecLocks(ctx, "task-2b", "n2", "run-2b", keys); err != nil || ok {
		t.Fatalf("foreign acquire in the pre-accept window: ok=%v err=%v, want blocked", ok, err)
	}
	must(s1.ReleaseExecLocks(ctx, h2.TaskID, "run-h2"))
}

// TestExecDanceReleaseReacquire pins the delegate-wait mirror of the queue
// registry dance: the run drops its worktree exec_locks rows while the
// fan-out runs (children inherit the parent's project key), a child could
// take them, and the run re-takes them before the round resumes. The
// actuator row stays held across the wait — releasing it would let a task
// that needs the device take the worktree key and then block on the
// in-process actuator mutex the parent holds, while the parent blocks on that
// task's worktree key: a cycle. The actor of the device is the parent for the
// whole round, exactly as the mutex makes it.
func TestExecDanceReleaseReacquire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := newCoreWithNative(t, "exec-dance", "", ledger.NativeAbility{ID: "sys:echo", Command: "echo"})

	tk := createTask(t, c.store, "", "dance", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := c.store.Accept(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	full := execLockKeys(tk, "hardware:servo")
	dance := queue.DefaultKeys(tk.ResourceKeys, tk.Project)
	if !slices.Equal(full, []string{"project:proj", "hardware:servo"}) || !slices.Equal(dance, []string{"project:proj"}) {
		t.Fatalf("setup keys: full=%v dance=%v", full, dance)
	}

	if _, ok := c.execReleaseFor(tk.TaskID); ok {
		t.Fatal("execReleaseFor reported a hold before any acquire")
	}
	if err := c.execLockAcquire(ctx, tk.TaskID, "run-1", full, dance, true); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	const foreign = "foreign-task"
	for _, keys := range [][]string{full, {"project:proj"}, {"hardware:servo"}} {
		if ok, err := c.store.TryAcquireExecLocks(ctx, foreign, "n2", "run-foreign", keys); err != nil || ok {
			t.Fatalf("foreign acquire %v during the hold: ok=%v err=%v, want blocked", keys, ok, err)
		}
	}

	got, ok := c.execReleaseFor(tk.TaskID)
	if !ok || !slices.Equal(got, dance) {
		t.Fatalf("release for the wait: ok=%v keys=%v, want %v", ok, got, dance)
	}
	// The worktree key is handed over (a child could take it) ...
	if ok, err := c.store.TryAcquireExecLocks(ctx, foreign, "n2", "run-foreign", []string{"project:proj"}); err != nil || !ok {
		t.Fatalf("worktree key not freed during the wait: ok=%v err=%v", ok, err)
	}
	// ... while the device stays with the parent for the whole round.
	if ok, err := c.store.TryAcquireExecLocks(ctx, foreign, "n2", "run-foreign", []string{"hardware:servo"}); err != nil || ok {
		t.Fatalf("actuator key must stay held across the wait: ok=%v err=%v", ok, err)
	}
	if err := c.store.ReleaseExecLocks(ctx, foreign, "run-foreign"); err != nil {
		t.Fatalf("child release: %v", err)
	}

	if err := c.execAcquireFor(ctx, tk.TaskID, got); err != nil {
		t.Fatalf("re-acquire after the wait: %v", err)
	}
	if ok, err := c.store.TryAcquireExecLocks(ctx, foreign, "n2", "run-foreign", []string{"project:proj"}); err != nil || ok {
		t.Fatalf("key not re-held for the resumed round: ok=%v err=%v", ok, err)
	}

	c.execLockUnlock(tk.TaskID)
	if ok, err := c.store.TryAcquireExecLocks(ctx, foreign, "n2", "run-foreign", full); err != nil || !ok {
		t.Fatalf("keys not released at run end: ok=%v err=%v", ok, err)
	}
	if err := c.store.ReleaseExecLocks(ctx, foreign, "run-foreign"); err != nil {
		t.Fatalf("cleanup release: %v", err)
	}
}

// TestDuplicateRunYieldsToInFlight: a second invocation of a task whose run
// is in flight in this process must stand down with ErrAlreadyRunning — not
// steal the live run's keys, which would leave the working run unprotected —
// and must not touch the row.
func TestDuplicateRunYieldsToInFlight(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithNative(t, "lock-dup", "", ledger.NativeAbility{ID: "sys:echo", Command: "echo", Args: []string{"dup"}})
	tk := createTask(t, c.store, "", "dup", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Stand in for the still-running first invocation holding the task.
	c.execHold.Store(tk.TaskID, execHoldEntry{keys: []string{"project:proj"}, runID: "run-x"})
	defer c.execHold.Delete(tk.TaskID)

	_, err := c.run(ctx, tk.TaskID, "echo", []string{"sys:echo"}, nil)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("duplicate run = %v, want ErrAlreadyRunning", err)
	}
	fresh, gerr := c.store.Get(ctx, tk.TaskID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if fresh.State != StateDispatched {
		t.Fatalf("duplicate touched the row: state = %s, want %s", fresh.State, StateDispatched)
	}
}

// TestExecLocksGCTrigger pins the trigger half of lock cleanup (migration
// 48): a task row leaving the executing states drops its exec_locks rows in
// the same UPDATE — no sweeper pass, no acquirer poll, no unlock call needed —
// while a move between live states (dispatched -> waiting_context) keeps
// them. Without it a lock row can outlive its run under a live-looking row
// and wedge the key until a sweep happened to cycle the task back through
// queued.
func TestExecLocksGCTrigger(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "gc.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := NewTaskStore(db, testLogger())
	keys := []string{"project:gc"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tk := createTask(t, s, "", "gc", "n1")
	held := func() int {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM exec_locks WHERE task_id=?`, tk.TaskID).Scan(&n); err != nil {
			t.Fatalf("count locks: %v", err)
		}
		return n
	}
	must(s.Queue(ctx, tk.TaskID, "n1"))
	must(s.Dispatch(ctx, tk.TaskID, "n1", "n1"))
	if ok, err := s.TryAcquireExecLocks(ctx, tk.TaskID, "n1", "run-tk", keys); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if held() == 0 {
		t.Fatal("acquire left no rows")
	}

	// dispatched -> waiting_context is a live->live move: the context wait is
	// still the same run, so the rows stay.
	must(s.SetWaitingContext(ctx, tk.TaskID, "n1"))
	if held() == 0 {
		t.Fatal("live->live transition dropped the lock rows")
	}

	// waiting_context -> cancelled leaves the executing states: the rows die
	// with the transition, before any sweeper or acquirer could see them.
	must(s.Cancel(ctx, tk.TaskID))
	if n := held(); n != 0 {
		t.Fatalf("cancel left %d lock rows", n)
	}
	if ok, err := s.TryAcquireExecLocks(ctx, "foreign", "n2", "run-f", keys); err != nil || !ok {
		t.Fatalf("foreign acquire after the transition: ok=%v err=%v", ok, err)
	}

	// The trigger is path-blind: a bulk sweep's raw UPDATE releases the rows
	// exactly like a guarded transition — requeue the task and drop it again.
	if err := s.ReleaseExecLocks(ctx, "foreign", "run-f"); err != nil {
		t.Fatalf("release foreign: %v", err)
	}
	if ok, err := s.TryAcquireExecLocks(ctx, tk.TaskID, "n1", "run-tk", keys); err != nil || !ok {
		t.Fatalf("re-acquire: ok=%v err=%v", ok, err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET state='queued', updated_at=0 WHERE task_id=?`, tk.TaskID); err != nil {
		t.Fatalf("raw requeue update: %v", err)
	}
	if n := held(); n != 0 {
		t.Fatalf("a raw sweep-style UPDATE left %d lock rows", n)
	}
}

// TestStaleReleaseKeepsReacquiredRows pins the run-nonce release predicate:
// a deferred unlock that fires AFTER its task's terminal transition (the
// trigger already freed the old rows) must not delete the fresh rows a
// re-execution of the same task just took under the same task_id. Without
// run_id the stale release is indistinguishable and the new run continues
// believing it holds locks it no longer does — two writers, one worktree.
func TestStaleReleaseKeepsReacquiredRows(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "stale.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := NewTaskStore(db, testLogger())
	keys := []string{"project:stale"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tk := createTask(t, s, "", "stale", "n1")
	must(s.Queue(ctx, tk.TaskID, "n1"))
	must(s.Dispatch(ctx, tk.TaskID, "n1", "n1"))

	// Run 1 acquires, then its run ends: the terminal transition fires the
	// gc trigger, which is what frees run 1's rows — simulate that with the
	// same DELETE the trigger runs.
	if ok, err := s.TryAcquireExecLocks(ctx, tk.TaskID, "n1", "run-1", keys); err != nil || !ok {
		t.Fatalf("run-1 acquire: ok=%v err=%v", ok, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM exec_locks WHERE task_id=?`, tk.TaskID); err != nil {
		t.Fatalf("trigger-equivalent delete: %v", err)
	}
	// The task re-executes and holds the keys under a new run nonce.
	if ok, err := s.TryAcquireExecLocks(ctx, tk.TaskID, "n1", "run-2", keys); err != nil || !ok {
		t.Fatalf("run-2 acquire: ok=%v err=%v", ok, err)
	}
	// Run 1's straggler unlock lands: keyed on its nonce it must match
	// nothing, not drop run 2's rows.
	must(s.ReleaseExecLocks(ctx, tk.TaskID, "run-1"))
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exec_locks WHERE task_id=?`, tk.TaskID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(keys) {
		t.Fatalf("stale release dropped %d of run 2's rows", len(keys)-n)
	}
	// And the current run's release still works on its own nonce.
	must(s.ReleaseExecLocks(ctx, tk.TaskID, "run-2"))
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exec_locks WHERE task_id=?`, tk.TaskID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("keyed release left %d rows", n)
	}
}

// TestRunWaitsOnForeignExecLock is the P0 regression: a task whose project key
// is held by a run in another core process (same database) waits in dispatched
// and runs only after the holder releases — instead of writing the same
// worktree concurrently with it.
func TestRunWaitsOnForeignExecLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := newCoreWithNative(t, "lock-wait", "", ledger.NativeAbility{ID: "sys:echo", Command: "echo", Args: []string{"locked"}})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	blocked := createTask(t, c.store, "", "blocked", c.nodeID)
	must(c.store.Queue(ctx, blocked.TaskID, c.nodeID))
	must(c.store.Dispatch(ctx, blocked.TaskID, c.nodeID, c.nodeID))

	// Another core process' in-flight run for the same project: a live row
	// holding the project key under its own task id.
	holder := createTask(t, c.store, "", "holder", c.nodeID)
	must(c.store.Queue(ctx, holder.TaskID, c.nodeID))
	must(c.store.Dispatch(ctx, holder.TaskID, c.nodeID, c.nodeID))
	must(c.store.Accept(ctx, holder.TaskID, c.nodeID))
	must(c.store.SetLease(ctx, holder.TaskID, 600_000))
	held, err := c.store.TryAcquireExecLocks(ctx, holder.TaskID, "other-process", "run-holder", []string{"project:proj"})
	if err != nil || !held {
		t.Fatalf("foreign hold: ok=%v err=%v", held, err)
	}

	done := make(chan error, 1)
	go func() {
		_, rerr := c.run(ctx, blocked.TaskID, "echo", []string{"sys:echo"}, nil)
		done <- rerr
	}()

	// Well past a poll interval: the run must sit in dispatched, not execute
	// beside the holder.
	time.Sleep(700 * time.Millisecond)
	if tk, gerr := c.store.Get(ctx, blocked.TaskID); gerr != nil || tk.State != StateDispatched {
		t.Fatalf("blocked task = %q err=%v, want %s while the key is held", tk.State, gerr, StateDispatched)
	}
	select {
	case rerr := <-done:
		t.Fatalf("run finished while the key was held (err=%v)", rerr)
	default:
	}

	must(c.store.ReleaseExecLocks(ctx, holder.TaskID, "run-holder"))
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("run after the release: %v", rerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run never proceeded after the exec lock was released")
	}
	tk, gerr := c.store.Get(ctx, blocked.TaskID)
	if gerr != nil || tk.State != StateDone {
		t.Fatalf("blocked task after the release = %q err=%v, want %s", tk.State, gerr, StateDone)
	}
}
