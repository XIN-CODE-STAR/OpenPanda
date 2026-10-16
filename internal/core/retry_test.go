// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/commander"
)

// TestRetryThenReview verifies a task that keeps failing is retried up to the
// loop detector's budget and then paused into review, not left in failed.
func TestRetryThenReview(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "retry-node")
	c.SetWorkDir(t.TempDir())
	c.sleep = func(time.Duration) {} // no-op backoff in tests

	attempts := 0
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		attempts++
		return commander.AgentResult{OK: false, Result: "flaky", ExitCode: 1}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "flaky",
		Project:     "proj",
		ContextType: "command",
		Intent:      "flaky",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (1 initial + 2 retries)", attempts)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review", task.State)
	}
	if result.OK {
		t.Fatalf("result should be not-ok for a reviewed task")
	}
}

// TestReviewRestoresRetryBudget pins the reset half of the loop detector, which
// had no call site at all (audit P2-11): the per-task failure count is what
// pauses a task into review, and it used to survive that pause forever. A person
// who looked at the parked task and sent it back therefore got a task with zero
// retries left, which parked itself again on its first failure — and every task
// id that ever failed stayed in the detector's map for the daemon's lifetime.
// Asserting a full fresh budget (max=2 ⇒ two allows, then a refusal) covers both.
func TestReviewRestoresRetryBudget(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "retry-reset-node")
	c.SetWorkDir(t.TempDir())
	c.sleep = func(time.Duration) {}

	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		return commander.AgentResult{OK: false, Result: "flaky", ExitCode: 1}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "flaky",
		Project:     "proj",
		ContextType: "command",
		Intent:      "flaky",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review", task.State)
	}
	for i := 1; i <= 2; i++ {
		if !c.loop.Allow(task.TaskID) {
			t.Fatalf("retry %d refused: the parked task did not get its budget back", i)
		}
	}
	if c.loop.Allow(task.TaskID) {
		t.Error("the budget is unbounded after a reset; the loop guard is gone")
	}
}

// TestSuccessClearsFailureCount is the other reset path: a task that fails and
// then succeeds must not carry its consumed count forward.
func TestSuccessClearsFailureCount(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "retry-reset-ok")
	c.SetWorkDir(t.TempDir())
	c.sleep = func(time.Duration) {}

	attempts := 0
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		attempts++
		if attempts == 1 {
			return commander.AgentResult{OK: false, Result: "flaky", ExitCode: 1}
		}
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "flaky then ok",
		Project:     "proj",
		ContextType: "command",
		Intent:      "flaky then ok",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if task.State != StateDone {
		t.Fatalf("state = %s, want done", task.State)
	}
	for i := 1; i <= 2; i++ {
		if !c.loop.Allow(task.TaskID) {
			t.Fatalf("retry %d refused: the completed task kept its spent budget", i)
		}
	}
}

// TestCheckpointRetriesUseSessionBudget verifies the split budget: a run that
// dies on its wall-clock bound WITH a live session (exit 124 + session id) is
// a resumable checkpoint, not a hard failure — it draws the larger checkpoint
// budget (maxCheckpointRetries) instead of parking after the hard-failure
// detector's two allowances, and the backoff stays at the base delay rather
// than doubling toward hour-scale sleeps.
func TestCheckpointRetriesUseSessionBudget(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "checkpoint-node")
	c.SetWorkDir(t.TempDir())

	var delays []time.Duration
	c.sleep = func(d time.Duration) { delays = append(delays, d) }

	attempts := 0
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		attempts++
		return commander.AgentResult{OK: false, Result: "adapter timed out", ExitCode: 124, SessionID: "ses_123"}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "long work",
		Project:     "proj",
		ContextType: "command",
		Intent:      "long work",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if attempts != maxCheckpointRetries+1 {
		t.Fatalf("attempts = %d, want 1 initial + %d checkpoint retries", attempts, maxCheckpointRetries)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review after the checkpoint budget is spent", task.State)
	}
	// Every retry event carries the checkpoint flag; the hard counter stays 0.
	// (CheckpointRetries' since-last-review window is already closed here —
	// the budget-exhaustion park IS a review — so assert with the totals.)
	if n, _ := c.store.RetryCount(ctx, task.TaskID); n != maxCheckpointRetries {
		t.Fatalf("total retries = %d, want %d", n, maxCheckpointRetries)
	}
	if n, _ := c.store.HardRetries(ctx, task.TaskID); n != 0 {
		t.Fatalf("hard retries = %d, want 0 — checkpoints must not draw the failure budget", n)
	}
	for i, d := range delays {
		if d != c.retryBackoff {
			t.Fatalf("delay[%d] = %v, want the base %v — checkpoint resumes pace flat", i, d, c.retryBackoff)
		}
	}
	_ = result
}

// TestCheckpointRetriesThenHardFailure verifies the budgets stay separate:
// checkpoint timeouts do not draw the hard-failure budget, and when the run
// later fails hard (no session), the ordinary detector applies.
func TestCheckpointRetriesThenHardFailure(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "checkpoint-then-hard")
	c.SetWorkDir(t.TempDir())
	c.sleep = func(time.Duration) {}

	attempts := 0
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		attempts++
		if attempts <= 3 {
			// Three timed-out rounds — past the hard budget already — all
			// resumable checkpoints.
			return commander.AgentResult{OK: false, Result: "timed out", ExitCode: 124, SessionID: "ses_x"}
		}
		// Then a genuine failure: the session-less error draws the hard
		// budget, whose two allowances are still unspent.
		return commander.AgentResult{OK: false, Result: "flaky", ExitCode: 1}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "long then broken",
		Project:     "proj",
		ContextType: "command",
		Intent:      "long then broken",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	// 3 checkpoints + 1 initial hard failure + 2 hard retries = 6 runs.
	if attempts != 6 {
		t.Fatalf("attempts = %d, want 3 checkpoints + 1 + 2 hard retries", attempts)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review", task.State)
	}
	// Same review-window caveat as above: totals say 3 checkpoint + 2 hard.
	if n, _ := c.store.RetryCount(ctx, task.TaskID); n != 5 {
		t.Fatalf("total retries = %d, want 5 (3 checkpoint + 2 hard)", n)
	}
	if n, _ := c.store.HardRetries(ctx, task.TaskID); n != 2 {
		t.Fatalf("hard retries = %d, want 2 — the hard budget was untouched by checkpoints", n)
	}
}

// TestRetryThenSuccess verifies a task that fails once then succeeds is retried
// and completes done.
func TestRetryThenSuccess(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithAgent(t, "retry-node-ok")
	c.SetWorkDir(t.TempDir())
	c.sleep = func(time.Duration) {} // no-op backoff in tests

	attempts := 0
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		attempts++
		if attempts == 1 {
			return commander.AgentResult{OK: false, Result: "flaky", ExitCode: 1}
		}
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "flaky then ok",
		Project:     "proj",
		ContextType: "command",
		Intent:      "flaky then ok",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if task.State != StateDone {
		t.Fatalf("state = %s, want done", task.State)
	}
	if !result.OK {
		t.Fatalf("result not ok: %+v", result)
	}
}
