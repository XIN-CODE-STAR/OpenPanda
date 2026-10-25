// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !lite

package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Xustalis/OpenPanda/internal/agents"
)

// TestTeamPickerTogglesAndApplies: space builds a working set, Enter writes it
// to the bare conversation's roster, and the status-row copy follows.
func TestTeamPickerTogglesAndApplies(t *testing.T) {
	m := newIsolatedTUI(t)
	m, _ = m.openTeamPicker()
	if m.mode != modeList || m.listKind != listTeam {
		t.Fatalf("picker did not open: mode=%v kind=%v", m.mode, m.listKind)
	}
	if len(m.selectionList.Items) != 1+len(agents.Registry()) {
		t.Fatalf("picker rows = %d, want automatic + registry", len(m.selectionList.Items))
	}

	// Move onto codex and toggle it.
	idx := -1
	for i, it := range m.selectionList.Items {
		if it.ID == "codex" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("codex missing from the registry-backed picker")
	}
	m.selectionList.Cursor = idx
	m.toggleTeamMark()
	if !m.teamMarks["codex"] {
		t.Fatal("space did not mark codex")
	}

	next, _ := m.applyTeamPicker()
	m = next.(tuiModel)
	if m.mode != modeIdle {
		t.Fatalf("apply left mode = %v", m.mode)
	}
	if len(m.team) != 1 || m.team[0] != "codex" {
		t.Fatalf("applied team = %v, want [codex]", m.team)
	}
	if got := loadBareTeam(); len(got) != 1 || got[0] != "codex" {
		t.Fatalf("persisted team = %v, want [codex]", got)
	}

	// Reopening marks the saved pick.
	m, _ = m.openTeamPicker()
	if !m.teamMarks["codex"] {
		t.Fatal("reopened picker lost the saved pick")
	}

	// Enter on the automatic row clears the roster.
	m.selectionList.Cursor = 0
	next, _ = m.applyTeamPicker()
	m = next.(tuiModel)
	if len(m.team) != 0 {
		t.Fatalf("automatic row did not clear the team: %v", m.team)
	}
	if got := loadBareTeam(); len(got) != 0 {
		t.Fatalf("persisted roster after automatic = %v", got)
	}
}

// TestTeamPickerSpaceThroughUpdate exercises the key path, not just the
// helper: both the KeySpace and rune-space spellings must toggle.
func TestTeamPickerSpaceThroughUpdate(t *testing.T) {
	m := newIsolatedTUI(t)
	m, _ = m.openTeamPicker()
	m.selectionList.Cursor = 1 // first registry agent

	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune(" ")},
	} {
		next, _ := m.Update(msg)
		m = next.(tuiModel)
	}
	if len(m.teamMarks) != 0 {
		t.Fatalf("two toggles should cancel out, marks = %v", m.teamMarks)
	}

	// Esc discards the working set without writing.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	m = next.(tuiModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(tuiModel)
	if m.mode != modeIdle || m.teamMarks != nil {
		t.Fatalf("esc left mode=%v marks=%v", m.mode, m.teamMarks)
	}
	if got := loadBareTeam(); len(got) != 0 {
		t.Fatalf("esc wrote a roster: %v", got)
	}
}

// TestTeamPickerKeepsUnregisteredAgent: a roster entry the registry cannot
// name — a capability card's agent — gets a row of its own and survives an
// apply. The picker used to draw registry agents only, so a card agent's
// roster rendered as "automatic" and one Enter wrote that empty set back.
func TestTeamPickerKeepsUnregisteredAgent(t *testing.T) {
	m := newIsolatedTUI(t)
	if err := m.r.setTeam([]string{"meta-wrapper"}); err != nil {
		t.Fatalf("seed roster: %v", err)
	}
	m, _ = m.openTeamPicker()

	idx := -1
	for i, it := range m.selectionList.Items {
		if it.ID == "meta-wrapper" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("picker has no row for an unregistered agent: %+v", m.selectionList.Items)
	}
	if m.selectionList.Items[idx].Badge == "" {
		t.Fatal("unregistered agent row is not marked as selected")
	}
	if m.selectionList.Cursor != idx {
		t.Fatalf("cursor = %d, want the selected row %d", m.selectionList.Cursor, idx)
	}

	next, _ := m.applyTeamPicker()
	m = next.(tuiModel)
	if len(m.team) != 1 || m.team[0] != "meta-wrapper" {
		t.Fatalf("applied team = %v, want [meta-wrapper]", m.team)
	}

	// A mixed roster keeps both names when the picker is opened and applied
	// without touching a row.
	if err := m.r.setTeam([]string{"codex", "meta-wrapper"}); err != nil {
		t.Fatalf("seed mixed roster: %v", err)
	}
	m, _ = m.openTeamPicker()
	next, _ = m.applyTeamPicker()
	m = next.(tuiModel)
	if got := strings.Join(m.team, ","); got != "codex,meta-wrapper" {
		t.Fatalf("mixed roster after apply = %q, want codex,meta-wrapper", got)
	}
}
