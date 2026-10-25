// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/i18n"
)

// TestParseTeamCommand pins the /team grammar: a bare list is a set, verbs
// mutate, and missing operands are a usage error rather than a silent clear.
func TestParseTeamCommand(t *testing.T) {
	cases := []struct {
		arg   string
		op    teamOp
		names []string
		err   bool
	}{
		{"", teamShow, nil, false},
		{"claude_code", teamSet, []string{"claude_code"}, false},
		{"claude_code, codex", teamSet, []string{"claude_code", "codex"}, false},
		{"set a b", teamSet, []string{"a", "b"}, false},
		{"add codex", teamAdd, []string{"codex"}, false},
		{"+ codex", teamAdd, []string{"codex"}, false},
		{"rm codex", teamRemove, []string{"codex"}, false},
		{"remove codex", teamRemove, []string{"codex"}, false},
		{"- codex", teamRemove, []string{"codex"}, false},
		{"clear", teamClear, nil, false},
		{"auto", teamClear, nil, false},
		{"set", teamSet, nil, true},
		{"add", teamAdd, nil, true},
		{"rm", teamRemove, nil, true},
	}
	for _, c := range cases {
		op, names, err := parseTeamCommand(c.arg)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected an error", c.arg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.arg, err)
			continue
		}
		if op != c.op || !reflect.DeepEqual(names, c.names) {
			t.Errorf("%q = (%v, %v), want (%v, %v)", c.arg, op, names, c.op, c.names)
		}
	}
}

// TestNormalizeTeamCanonicalises: a typed name maps onto the registry's
// spelling, and duplicates created by that mapping collapse.
func TestNormalizeTeamCanonicalises(t *testing.T) {
	got, err := normalizeTeam([]string{"Claude_Code", "claude_code", " Codex "})
	if err != nil {
		t.Fatalf("normalizeTeam: %v", err)
	}
	want := []string{"claude_code", "codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeTeam = %v, want %v", got, want)
	}
	if _, err := normalizeTeam([]string{"bad/name"}); err == nil {
		t.Fatal("a name with a path separator must be refused")
	}
}

// TestBareTeamRoundTrip: the bare conversation's roster is written, read back,
// and removed by an empty set — the "no restriction" state is the absence of a
// file, not an empty pick.
func TestBareTeamRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := cliConfigPath
	cliConfigPath = filepath.Join(t.TempDir(), "missing.yaml") // no ambient project / DB
	t.Cleanup(func() { cliConfigPath = old })

	r := &repl{loc: i18n.English}
	if err := r.setTeam([]string{"codex", "claude_code"}); err != nil {
		t.Fatalf("setTeam: %v", err)
	}
	if got := loadBareTeam(); !reflect.DeepEqual(got, []string{"codex", "claude_code"}) {
		t.Fatalf("loadBareTeam = %v", got)
	}
	if got := r.teamNow(); !reflect.DeepEqual(got, []string{"codex", "claude_code"}) {
		t.Fatalf("teamNow = %v", got)
	}

	// The cached display copy follows an external write once invalidated.
	if err := saveBareTeam([]string{"opencode"}); err != nil {
		t.Fatalf("saveBareTeam: %v", err)
	}
	r.invalidateTeam()
	if got := r.teamNow(); !reflect.DeepEqual(got, []string{"opencode"}) {
		t.Fatalf("teamNow after invalidate = %v", got)
	}

	if err := r.setTeam(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := loadBareTeam(); len(got) != 0 {
		t.Fatalf("loadBareTeam after clear = %v", got)
	}
}

// TestTeamCommandAppliesDelta covers the user-facing /team verbs against a
// real bare roster file.
func TestTeamCommandAppliesDelta(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := cliConfigPath
	cliConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	t.Cleanup(func() { cliConfigPath = old })

	r := &repl{loc: i18n.English}
	if _, err := r.teamCommand("add codex, opencode"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := r.teamCommand("rm opencode"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if got := loadBareTeam(); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("roster = %v, want [codex]", got)
	}
	if _, err := r.teamCommand("clear"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := loadBareTeam(); len(got) != 0 {
		t.Fatalf("roster after clear = %v", got)
	}
	if _, err := r.teamCommand("rm"); err == nil {
		t.Fatal("a verb without names must be a usage error")
	}
}

// TestResolveAskTeamInheritsBarePick: `panda ask --continue` inherits the
// conversation's roster; a one-shot ask does not, and an explicit --team always
// wins.
func TestResolveAskTeamInheritsBarePick(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := cliConfigPath
	cliConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	t.Cleanup(func() { cliConfigPath = old })

	if err := saveBareTeam([]string{"codex"}); err != nil {
		t.Fatalf("saveBareTeam: %v", err)
	}
	got, err := resolveAskTeam("", true)
	if err != nil || !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("continue inherit = %v, %v", got, err)
	}
	got, err = resolveAskTeam("", false)
	if err != nil || len(got) != 0 {
		t.Fatalf("one-shot without --team = %v, %v", got, err)
	}
	got, err = resolveAskTeam("opencode", true)
	if err != nil || !reflect.DeepEqual(got, []string{"opencode"}) {
		t.Fatalf("explicit --team = %v, %v", got, err)
	}
	if _, err := resolveAskTeam("bad/name", false); err == nil {
		t.Fatal("invalid --team name must be refused")
	}
}

// TestNodeCommandOneShot: /node pins exactly the next ask and then clears.
func TestNodeCommandOneShot(t *testing.T) {
	r := &repl{loc: i18n.English}
	if _, err := r.nodeCommand("gpu-box"); err != nil {
		t.Fatalf("nodeCommand: %v", err)
	}
	if got := r.takeNextNode(); got != "gpu-box" {
		t.Fatalf("takeNextNode = %q", got)
	}
	if got := r.takeNextNode(); got != "" {
		t.Fatalf("the pin must be consumed once, got %q", got)
	}
	if _, err := r.nodeCommand("two words"); err == nil {
		t.Fatal("a node id may not contain spaces")
	}
}

// TestUnknownAgentNamesHonoursCard: a card-defined agent is a known roster
// name. The one-shot CLI used to compare against the registry alone, so every
// `panda ask --team <card agent>` printed a bogus "unknown agent" warning
// about a name the user had correctly picked.
func TestUnknownAgentNamesHonoursCard(t *testing.T) {
	cardPath := filepath.Join(t.TempDir(), "capabilities.yaml")
	card := "device: test-node\nagents:\n  meta-wrapper:\n    adapter: generic\n"
	if err := os.WriteFile(cardPath, []byte(card), 0o600); err != nil {
		t.Fatalf("write card: %v", err)
	}
	got := unknownAgentNames([]string{"meta-wrapper", "definitely-not-an-agent"}, cardPath)
	if !reflect.DeepEqual(got, []string{"definitely-not-an-agent"}) {
		t.Fatalf("unknown = %v, want only the typo", got)
	}
	// A missing card still leaves the registry in charge.
	if got := unknownAgentNames([]string{"codex"}, ""); len(got) != 0 {
		t.Fatalf("registry agent flagged unknown: %v", got)
	}
}
