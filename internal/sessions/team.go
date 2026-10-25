// SPDX-License-Identifier: AGPL-3.0-or-later

package sessions

import (
	"fmt"
	"strings"
)

// MaxTeam bounds a thread's agent roster. It matches routing.preferred_agents:
// a roster longer than this is not a team anyone picks by hand, and the list
// rides every task's spec_json.
const MaxTeam = 16

// CleanTeam trims, dedupes and validates a roster. Invalid names are rejected
// instead of dropped: a roster that quietly loses an entry becomes a *smaller*
// team, and one that loses every entry becomes "no restriction" — either way
// the thread's work could go to an agent the user never picked.
//
// The same function backs the panel's team picker, the REPL/TUI /team command
// and the bare-conversation roster file, so a name accepted by one surface is
// accepted by all of them.
func CleanTeam(in []string) ([]string, error) {
	var out []string
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, " \t\r\n/\\:") || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid agent name %q", raw)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > MaxTeam {
		return nil, fmt.Errorf("team holds at most %d agents", MaxTeam)
	}
	return out, nil
}
