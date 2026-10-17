// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/commander"
)

// TestUserInjectInterruptsAndLeadsNextRound is the end-to-end steer contract:
// a user inject arriving while an agent round is live aborts that round
// (never marking the task failed), the aborted round's work is discarded, and
// the next round's prompt leads with the user's directive — ahead of the task
// spec and any supervisor follow-up. The task then finishes normally.
func TestUserInjectInterruptsAndLeadsNextRound(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "steer-e2e", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSuperviseRounds(3)
	c.SetSupervisor(newFakeSupervisor(t, func(call int) string {
		return `{"status":"done","reason":"完成","followup":""}`
	}))

	type run struct {
		prompt string
		ctx    context.Context
	}
	runs := make(chan run, 4)
	var calls atomic.Int32
	c.router.SetAdapterRunner(func(runCtx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		runs <- run{prompt: prompt, ctx: runCtx}
		if calls.Add(1) > 1 {
			return commander.AgentResult{OK: true, Result: "finished", ExitCode: 0}
		}
		select {
		case <-runCtx.Done():
			// The inject's round cancel — the aborted-turn shape.
			return commander.AgentResult{OK: false, Result: "aborted", ExitCode: 1}
		case <-time.After(10 * time.Second):
			return commander.AgentResult{OK: true, Result: "finished", ExitCode: 0}
		}
	})

	tk := createTask(t, c.store, "", "steer target", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, tk.TaskID, "original task intent", []string{"code:modify"}, nil)
		done <- err
	}()

	// Round 1 is live and registered as interruptible.
	first := <-runs
	if !strings.Contains(first.prompt, "original task intent") {
		t.Fatalf("round 1 prompt missing the task intent:\n%s", first.prompt)
	}
	status, err := c.InjectUserMessage(ctx, tk.TaskID, "change course: write the report instead")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if status != "interrupted" {
		t.Fatalf("inject status = %q, want interrupted", status)
	}

	// Round 2: the steer round — its intent must LEAD with the directive
	// (the prompt builder wraps the intent in an execution-context preamble,
	// so assert on ordering within the prompt, not the prompt's first byte).
	second := <-runs
	directive := strings.Index(second.prompt, "[user directive]\nchange course: write the report instead")
	intent := strings.Index(second.prompt, "original task intent")
	if directive < 0 {
		t.Fatalf("round 2 prompt missing the directive:\n%s", second.prompt)
	}
	if intent < 0 {
		t.Fatalf("round 2 prompt dropped the task context:\n%s", second.prompt)
	}
	if directive > intent {
		t.Fatalf("directive must lead the intent (directive@%d intent@%d):\n%s", directive, intent, second.prompt)
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	row, err := c.store.Get(ctx, tk.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.State != StateDone {
		t.Fatalf("state = %s, want done (an inject must not fail the task)", row.State)
	}
	// The audit trail carries both halves: the user message and the abort.
	events, err := c.store.Events(ctx, tk.TaskID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawMsg, sawAbort bool
	for _, ev := range events {
		if ev.Type == EvUserMsg {
			sawMsg = true
		}
		if ev.Type == "user_inject_interrupt" {
			sawAbort = true
		}
	}
	if !sawMsg || !sawAbort {
		t.Fatalf("events: user_msg=%v interrupt=%v, want both", sawMsg, sawAbort)
	}
}

// TestUserQueueMessageReachesNextRound: the non-disruptive mode — a queued
// message rides the sibling board into the running task's next round without
// aborting anything, and the round that was live when it arrived completes.
func TestUserQueueMessageReachesNextRound(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "queue-e2e", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSuperviseRounds(3)
	c.SetSupervisor(newFakeSupervisor(t, func(call int) string {
		if call == 1 {
			return `{"status":"continue","reason":"halfway","followup":"finish the rest"}`
		}
		return `{"status":"done","reason":"完成","followup":""}`
	}))

	prompts := make(chan string, 4)
	c.router.SetAdapterRunner(func(runCtx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		prompts <- prompt
		return commander.AgentResult{OK: true, Result: "round done", ExitCode: 0}
	})

	tk := createTask(t, c.store, "", "queue target", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := c.QueueUserMessage(ctx, tk.TaskID, "also touch the docs"); err != nil {
		t.Fatalf("queue message: %v", err)
	}

	if _, err := c.run(ctx, tk.TaskID, "original intent", []string{"code:modify"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Round 1 read the note as board context (delivered before the agent ran);
	// round 2 carried the supervisor follow-up. Both must be present.
	first := <-prompts
	if !strings.Contains(first, "also touch the docs") {
		t.Fatalf("round 1 prompt missing the queued note:\n%s", first)
	}
	if !strings.Contains(first, "original intent") {
		t.Fatalf("round 1 prompt missing the task intent:\n%s", first)
	}
	second := <-prompts
	if !strings.Contains(second, "finish the rest") {
		t.Fatalf("round 2 prompt missing the supervisor follow-up:\n%s", second)
	}
}
