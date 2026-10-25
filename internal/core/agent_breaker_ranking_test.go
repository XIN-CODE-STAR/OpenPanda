// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "testing"

// TestBlockedAgentLosesLocalRanking pins the wiring between the node's agent
// circuit breaker and local routing. The breaker already gates spawns and is
// published to peers as blocked_agents; without this link the same node kept
// nominating the agent it had just ruled out as its plan's primary, paying a
// spawn + failover every round. The demotion is a ranking term, not an
// exclusion: the blocked agent stays reachable as an alternate, and closing
// the circuit restores it.
func TestBlockedAgentLosesLocalRanking(t *testing.T) {
	c := newCoreWithTwoAgents(t)

	plan, err := c.router.Route([]string{"code:modify"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if plan.Agent != "opencode" {
		t.Fatalf("primary = %q, want the cheaper opencode", plan.Agent)
	}

	for i := 0; i < 3; i++ {
		c.breaker.RecordFailure("agent:opencode")
	}
	if !c.breaker.Blocked("agent:opencode") {
		t.Fatal("breaker should hold opencode open after the threshold")
	}

	plan, err = c.router.Route([]string{"code:modify"})
	if err != nil {
		t.Fatalf("route with open circuit: %v", err)
	}
	if plan.Agent != "claude_code" {
		t.Fatalf("primary = %q, want claude_code while opencode is blocked", plan.Agent)
	}
	if len(plan.Alternates) != 1 || plan.Alternates[0] != "opencode" {
		t.Fatalf("alternates = %v, want the blocked agent still reachable", plan.Alternates)
	}

	c.breaker.RecordSuccess("agent:opencode")
	plan, err = c.router.Route([]string{"code:modify"})
	if err != nil {
		t.Fatalf("route after recovery: %v", err)
	}
	if plan.Agent != "opencode" {
		t.Fatalf("primary = %q, want opencode after the circuit closes", plan.Agent)
	}
}
