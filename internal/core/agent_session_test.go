// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/Xustalis/OpenPanda/internal/commander"
)

// TestAgentSessionCheckpointResumesAcrossRounds exercises §5.2 breakpoint
// mooring inside one run: the adapter's session handle is captured per round,
// persisted to the task row, and threaded back through WithResume on the next
// round so the agent continues its conversation instead of cold-starting.
func TestAgentSessionCheckpointResumesAcrossRounds(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "sess-checkpoint", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSuperviseRounds(2)
	c.SetSupervisor(newFakeSupervisor(t, func(call int) string {
		return `{"status":"continue","reason":"还没完成","followup":"继续"}`
	}))

	var mu sync.Mutex
	var resumes []string
	c.router.SetAdapterRunner(func(runCtx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		mu.Lock()
		resumes = append(resumes, commander.ResumeID(runCtx))
		n := len(resumes)
		mu.Unlock()
		return commander.AgentResult{
			OK: true, Result: fmt.Sprintf("round-%d", n), ExitCode: 0,
			SessionID: fmt.Sprintf("sess-%d", n),
		}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "long work",
		Project:     "proj",
		ContextType: "command",
		Intent:      "do the thing",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review (round budget spent on continue)", task.State)
	}
	if len(resumes) != 2 || resumes[0] != "" || resumes[1] != "sess-1" {
		t.Fatalf("resumes = %v, want [\"\" \"sess-1\"]", resumes)
	}
	row, err := c.store.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.AgentSessionID != "sess-2" || row.AgentSessionNode != "sess-checkpoint" {
		t.Fatalf("row session = %q@%q, want sess-2@sess-checkpoint",
			row.AgentSessionID, row.AgentSessionNode)
	}
}

// TestAgentSessionSeedOnRerun: a task row carrying a checkpoint — persisted by
// an interrupted earlier run or adopted from a ResumeSessionID hint — seeds
// the next run() with WithResume, and a clean finish clears the handle so a
// stale conversation is never dragged into unrelated work.
func TestAgentSessionSeedOnRerun(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "sess-rerun", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSupervisor(newFakeSupervisor(t, func(call int) string {
		return `{"status":"done","reason":"完成","followup":""}`
	}))

	var gotResume string
	c.router.SetAdapterRunner(func(runCtx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		gotResume = commander.ResumeID(runCtx)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0, SessionID: "sess-final"}
	})

	tk := createTask(t, c.store, "", "resume-me", c.nodeID)
	if err := c.store.SetAgentSession(ctx, tk.TaskID, "sess-xyz", c.nodeID, ""); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := c.run(ctx, tk.TaskID, "intent", []string{"code:modify"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotResume != "sess-xyz" {
		t.Fatalf("runner saw resume %q, want sess-xyz", gotResume)
	}
	row, err := c.store.Get(ctx, tk.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.State != StateDone {
		t.Fatalf("state = %s, want done", row.State)
	}
	if row.AgentSessionID != "" || row.AgentSessionNode != "" {
		t.Fatalf("finished task kept session %q@%q, want cleared",
			row.AgentSessionID, row.AgentSessionNode)
	}
}

// TestResultRecordsRemoteSession: when the executor's result carries
// AgentSessionID, the delegator stores it against the authenticated sender —
// so a later re-dispatch can offer the handle back to the one node whose
// adapter actually owns that conversation.
func TestResultRecordsRemoteSession(t *testing.T) {
	ctx := context.Background()
	c := newCore(t, "sess-origin", "127.0.0.1:18152")
	tk := createTask(t, c.store, "", "delegated", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, "node-b"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	env, err := bus.NewEnvelope(bus.MsgTaskResult, "node-b", "r-sess", bus.TaskResultPayload{
		TaskID: tk.TaskID, AttemptID: tk.AttemptID, State: StateReview, OK: true,
		Chain:          []string{c.nodeID, "node-b"},
		AgentSessionID: "sess-remote-7",
	})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	c.handleResult(ctx, env)

	row, err := c.store.Get(ctx, tk.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.AgentSessionID != "sess-remote-7" || row.AgentSessionNode != "node-b" {
		t.Fatalf("recorded session = %q@%q, want sess-remote-7@node-b",
			row.AgentSessionID, row.AgentSessionNode)
	}
}

// TestDelegateAdoptsResumeHint: a ResumeSessionID hint arriving with a
// delegation is adopted as a session owned by this node — the hint names the
// conversation this node's adapter should continue, so its ownership row
// points at self, not at the delegator that merely carried it.
func TestDelegateAdoptsResumeHint(t *testing.T) {
	ctx := context.Background()
	c := newCore(t, "sess-adopter", "127.0.0.1:18151")

	env, err := bus.NewEnvelope(bus.MsgTaskDelegate, "parent", "d-resume", bus.TaskDelegatePayload{
		TaskID:          "t-resume-hint",
		Title:           "resumed work",
		Intent:          "x",
		Requires:        []string{"ability:nowhere"},
		Chain:           []string{"parent"},
		ResumeSessionID: "sess-hint-1",
	})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	c.handleDelegate(ctx, env)

	row, err := c.store.Get(ctx, "t-resume-hint")
	if err != nil {
		t.Fatalf("delegated task missing: %v", err)
	}
	if row.AgentSessionID != "sess-hint-1" || row.AgentSessionNode != "sess-adopter" {
		t.Fatalf("adopted session = %q@%q, want sess-hint-1@sess-adopter",
			row.AgentSessionID, row.AgentSessionNode)
	}
}

// TestAgentSessionAdapterAffinity: the §5.2 handle is adapter-affine. A row
// whose session was minted by opencode.py must NOT be offered to a round
// landing on claude_code.py — the foreign id is poison (opencode answers a
// non-"ses" handle with HTTP 400, so a bounded retry used to re-burn a whole
// cold-start run per round). The fix records the minting adapter and only
// resumes on a match; an unknown producer (legacy row) still gets the offer.
func TestAgentSessionAdapterAffinity(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "sess-affinity", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSupervisor(newFakeSupervisor(t, func(call int) string {
		return `{"status":"done","reason":"完成","followup":""}`
	}))

	var gotResume string
	c.router.SetAdapterRunner(func(runCtx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		gotResume = commander.ResumeID(runCtx)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0, SessionID: "sess-final"}
	})

	// Foreign-adapter session: must not reach the claude_code.py round.
	tk := createTask(t, c.store, "", "affinity-drop", c.nodeID)
	if err := c.store.SetAgentSession(ctx, tk.TaskID, "bc47c3fd-29e9-4cbf-91b0-e4c84918f54b", c.nodeID, "opencode.py"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := c.run(ctx, tk.TaskID, "intent", []string{"code:modify"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotResume != "" {
		t.Fatalf("runner saw foreign resume %q, want none", gotResume)
	}

	// Same-adapter session: offered normally. The first run's fresh mint is
	// recorded against claude_code.py, so the row's adapter column moves on.
	tk2 := createTask(t, c.store, "", "affinity-match", c.nodeID)
	if err := c.store.SetAgentSession(ctx, tk2.TaskID, "sess-mine", c.nodeID, "claude_code.py"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := c.store.Queue(ctx, tk2.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk2.TaskID, c.nodeID, c.nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := c.run(ctx, tk2.TaskID, "intent", []string{"code:modify"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotResume != "sess-mine" {
		t.Fatalf("runner saw resume %q, want sess-mine", gotResume)
	}
	row, err := c.store.Get(ctx, tk2.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Done clears the handle — and the minting adapter with it, so nothing
	// downstream can resume a dead conversation.
	if row.AgentSessionID != "" || row.AgentSessionAdapter != "" {
		t.Fatalf("finished task kept session %q adapter %q, want cleared",
			row.AgentSessionID, row.AgentSessionAdapter)
	}
}

// TestResultRecordsRemoteSessionAdapter: the producer adapter rides the
// task_result payload so the delegator's stored copy carries the affinity a
// re-dispatch needs — not just the node it belongs to.
func TestResultRecordsRemoteSessionAdapter(t *testing.T) {
	ctx := context.Background()
	c := newCore(t, "sess-origin-adapter", "127.0.0.1:18153")
	tk := createTask(t, c.store, "", "delegated", c.nodeID)
	if err := c.store.Queue(ctx, tk.TaskID, c.nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.Dispatch(ctx, tk.TaskID, c.nodeID, "node-b"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	env, err := bus.NewEnvelope(bus.MsgTaskResult, "node-b", "r-sess-ad", bus.TaskResultPayload{
		TaskID: tk.TaskID, AttemptID: tk.AttemptID, State: StateReview, OK: true,
		Chain:               []string{c.nodeID, "node-b"},
		AgentSessionID:      "ses_remotexyz",
		AgentSessionAdapter: "opencode.py",
	})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	c.handleResult(ctx, env)

	row, err := c.store.Get(ctx, tk.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.AgentSessionID != "ses_remotexyz" || row.AgentSessionNode != "node-b" ||
		row.AgentSessionAdapter != "opencode.py" {
		t.Fatalf("recorded session = %q@%q adapter %q, want ses_remotexyz@node-b opencode.py",
			row.AgentSessionID, row.AgentSessionNode, row.AgentSessionAdapter)
	}
}
