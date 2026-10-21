// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/commander"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// newCoreWithTwoAgents builds a Core whose card carries a cheap and an
// expensive agent that both declare code:modify — so unrestricted routing has
// a clear winner (the cheap one) and a roster has something to override.
func newCoreWithTwoAgents(t *testing.T) *Core {
	t.Helper()
	db := openTestDB(t)
	card := ledger.Card{
		Device:        "roster-node",
		ResourceClass: "Standard",
		Agents: map[string]ledger.Agent{
			"claude_code": {Adapter: "claude_code.py", Capabilities: []string{"code:modify"}, CostTier: "high", Tier: 1},
			"opencode":    {Adapter: "opencode.py", Capabilities: []string{"code:modify"}, CostTier: "low", Tier: 1},
		},
		Capacity: ledger.Capacity{CPUCores: 8, RAMGB: 16, MaxConcurrent: 3},
	}
	c := NewCore(db, "roster-node", card, 5, testLogger(), config.ModelConfig{})
	c.router.SetAgentProber(func(string, ledger.Agent) bool { return true })
	c.SetWorkDir(t.TempDir())
	c.SetSharedSecret(testSharedSecret)
	return c
}

// TestRosterNarrowsAgentChoice: a task whose spec carries allow_agents runs on
// one of the listed agents even when another agent on the card scores better.
// This is the whole point of the composer's team pick — the restriction has to
// survive from the chat turn down to the executor's routing table, which is
// what spec_json carries.
func TestRosterNarrowsAgentChoice(t *testing.T) {
	ctx := context.Background()
	c := newCoreWithTwoAgents(t)
	var ran []string
	c.router.SetAdapterRunner(func(_ context.Context, adapter, _, _ string) commander.AgentResult {
		ran = append(ran, adapter)
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "roster work",
		Project:     "proj",
		ContextType: "file",
		Intent:      "edit a file",
		SpecJSON:    `{"scope":"x","target":"y","allow_agents":["claude_code"]}`,
		Requires:    []string{"code:modify"},
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

	// Control: the same task with no roster takes the cheap agent, so the
	// assertion above is about the roster and not about a card that only had
	// one usable agent.
	ran = nil
	if _, result, err = c.SubmitLocal(ctx, TaskInput{
		Title:       "no roster",
		Project:     "proj",
		ContextType: "file",
		Intent:      "edit a file",
		SpecJSON:    `{"scope":"x","target":"y"}`,
		Requires:    []string{"code:modify"},
		Complexity:  0.1,
		Risk:        "low",
	}); err != nil {
		t.Fatalf("submit local (no roster): %v", err)
	}
	if len(ran) != 1 || ran[0] != "opencode.py" {
		t.Fatalf("unrestricted runs = %v, want opencode.py (cheapest match)", ran)
	}
	if result.Agent != "opencode" {
		t.Fatalf("unrestricted result agent = %q, want opencode", result.Agent)
	}
}
