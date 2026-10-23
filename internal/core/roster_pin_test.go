// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/commander"
)

// TestRosterRefusesPinOutsideTeam: a task that pins an agent the thread's roster
// excludes must be refused, not re-staffed.
//
// The roster still outranks a model-emitted pin (TestRankAgentsRosterBeatsModelPin
// in the commander package) — a conversation told which hands may work must not
// be re-opened to everyone. But "the roster wins" was implemented as "rank the
// roster instead", which silently changed WHO the task was for while leaving the
// task's own text intact. Observed live: three "test agent:<name>" tasks pinned
// to claude_code, codex and deepseek_harness ran on a roster of
// [deepseek_harness, opencode]; every one of them ran on deepseek_harness and
// dutifully printed "claude_code 调用成功" / "codex 调用成功", so the supervisor
// reported three green passes for two agents that never started. A refusal names
// the pinned agent and the roster, and nothing executes.
func TestRosterRefusesPinOutsideTeam(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithTwoAgents(t)
	var ran []string
	c.router.SetAdapterRunner(func(_ context.Context, adapter, _, _ string) commander.AgentResult {
		ran = append(ran, adapter)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "test agent:claude_code",
		Project:     "proj",
		ContextType: "file",
		Intent:      "test claude_code",
		SpecJSON:    `{"scope":"x","target":"y","allow_agents":["opencode"]}`,
		Requires:    []string{"agent:claude_code"},
		Complexity:  0.1,
		Risk:        "low",
	})
	if err == nil {
		t.Fatalf("a pin outside the roster must be refused; state=%s ran=%v", task.State, ran)
	}
	if !strings.Contains(err.Error(), "claude_code") {
		t.Fatalf("refusal should name the pinned agent: %v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("no agent may run a refused task, ran=%v", ran)
	}
}

// TestRosterWithNoLocalAgentIsRefused: a team made only of agents this node
// does not have cannot be staffed here. The router's own degradation (an
// unresolvable roster leaves the card unrestricted) is right for a mesh-wide
// pick, but this node has already been chosen to execute — degrading there
// means running on someone the user did not pick, which is how "test grok" can
// come back green from a different CLI.
func TestRosterWithNoLocalAgentIsRefused(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithTwoAgents(t)
	var ran []string
	c.router.SetAdapterRunner(func(_ context.Context, adapter, _, _ string) commander.AgentResult {
		ran = append(ran, adapter)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "test grok",
		Project:     "proj",
		ContextType: "file",
		Intent:      "test grok",
		SpecJSON:    `{"scope":"x","target":"y","allow_agents":["grok_build"]}`,
		Requires:    []string{"agent:grok_build"},
		Complexity:  0.1,
		Risk:        "low",
	})
	if err == nil {
		t.Fatalf("a roster of absent agents must be refused; state=%s ran=%v", task.State, ran)
	}
	if !strings.Contains(err.Error(), "grok_build") {
		t.Fatalf("refusal should name the roster: %v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("no agent may run a task scoped to an absent team, ran=%v", ran)
	}
}

// TestRosterAllowsPinInsideTeam is the control: the same pin is honoured when
// the roster carries that agent, so the refusal above is about the roster and
// not about pins being rejected generally.
func TestRosterAllowsPinInsideTeam(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithTwoAgents(t)
	var ran []string
	c.router.SetAdapterRunner(func(_ context.Context, adapter, _, _ string) commander.AgentResult {
		ran = append(ran, adapter)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "test agent:claude_code",
		Project:     "proj",
		ContextType: "file",
		Intent:      "test claude_code",
		SpecJSON:    `{"scope":"x","target":"y","allow_agents":["claude_code"]}`,
		Requires:    []string{"agent:claude_code"},
		Complexity:  0.1,
		Risk:        "low",
	})
	if err != nil {
		t.Fatalf("submit local: %v", err)
	}
	if task.State != StateDone {
		t.Fatalf("state = %s, want done", task.State)
	}
	if len(ran) != 1 || ran[0] != "claude_code.py" {
		t.Fatalf("adapter runs = %v, want exactly claude_code.py", ran)
	}
	if result.Agent != "claude_code" {
		t.Fatalf("result agent = %q, want claude_code", result.Agent)
	}
}
