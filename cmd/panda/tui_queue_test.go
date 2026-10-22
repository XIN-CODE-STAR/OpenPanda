// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !lite

package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTUIExecModeQueuesInput covers the busy-mode contract: while an exec owns
// the foreground a submitted line does not die — it lands on pendingInputs and
// drains as the next submission the moment the exec ends.
func TestTUIExecModeQueuesInput(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeExec
	m.execGen = 1
	m.exec = newCommandExec(m.execGen)

	// Typing while the exec runs must reach the textarea — the input box is
	// no longer frozen mid-command.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!echo hi")})
	if m.ta.Value() != "!echo hi" {
		t.Fatalf("typed input during exec lost: ta=%q", m.ta.Value())
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.pendingInputs) != 1 || m.pendingInputs[0] != "!echo hi" {
		t.Fatalf("Enter during exec should queue the line: pending=%v", m.pendingInputs)
	}
	if m.ta.Value() != "" {
		t.Fatalf("queued line must clear the draft, got %q", m.ta.Value())
	}

	// The exec's terminal event drains the queue: "!echo hi" resubmits into a
	// new exec instead of vanishing.
	m = step(m, execDoneMsg{exec: m.exec, generation: 1, text: "!old", output: "old output"})
	if len(m.pendingInputs) != 0 {
		t.Fatalf("queue should drain after exec end: %v", m.pendingInputs)
	}
	if m.mode != modeExec || m.exec == nil {
		t.Fatalf("drained '!echo hi' should start a new exec: mode=%v exec=%v", m.mode, m.exec != nil)
	}
}

// TestTUIExecModeEscClearsDraftBeforeCancel keeps the two-step Esc: a draft in
// the box clears first, and only an empty box lets Esc reach the cancel.
func TestTUIExecModeEscClearsDraftBeforeCancel(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeExec
	m.execGen = 1
	m.exec = newCommandExec(m.execGen)
	exec := m.exec

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.ta.Value() != "" {
		t.Fatalf("Esc with a draft must clear it first, got %q", m.ta.Value())
	}
	select {
	case <-exec.ctx.Done():
		t.Fatal("Esc cleared the draft but cancelled the exec too")
	case <-time.After(50 * time.Millisecond):
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case <-exec.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Esc on an empty draft must cancel the exec")
	}
}

// TestTUIAskingSlashRunsBackgroundExec is the "commands don't queue" half:
// /cmd typed mid-turn dispatches immediately as a background exec — the turn
// keeps modeAsking, the exec slot stays free, and the output lands in the
// transcript on completion.
func TestTUIAskingSlashRunsBackgroundExec(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeAsking
	cancelled := false
	m.stream = newTestStream(&cancelled)

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/version")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeAsking {
		t.Fatalf("a mid-turn /cmd must not leave asking mode: mode=%v", m.mode)
	}
	if m.exec != nil {
		t.Fatal("a mid-turn /cmd must not take the foreground exec slot")
	}
	if len(m.pendingInputs) != 0 {
		t.Fatalf("/version prints text — it must not queue: %v", m.pendingInputs)
	}
	if cancelled {
		t.Fatal("a background exec must not cancel the running turn")
	}
}

// TestTUIAskingForegroundCommandQueues is the other half: a command that
// spawns a turn or owns a mode (/ask, pickers) cannot run mid-turn — it joins
// the pending queue and echoes the queued note.
func TestTUIAskingForegroundCommandQueues(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeAsking
	m.chatHistory = &chatHistory{}
	m.stream = newTestStream(new(bool))

	for _, line := range []string{"/ask next prompt", "/tasks", "/new"} {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(line)})
		m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	if len(m.pendingInputs) != 3 {
		t.Fatalf("foreground-needing commands should queue: %v", m.pendingInputs)
	}
	if m.mode != modeAsking {
		t.Fatalf("queued commands must not flip the mode: %v", m.mode)
	}
	// Each enqueue echoes a visible note — the user must see it queued.
	notes := 0
	for _, b := range m.chatHistory.blocks {
		if b.kind == blockNote && strings.Contains(b.body, "queued") {
			notes++
		}
	}
	if notes != 3 {
		t.Fatalf("each queued line must echo a note, got %d", notes)
	}
}

// TestTUIPlainTextQueuesAsNextTurn guards the split's third way: text without a
// command prefix is neither an exec nor a command — it is the user's next turn,
// parked in the pending queue. It must NOT reach the running turn: a mid-stream
// "steer" made the model read the same sentence twice (once inline in the user
// turn, once folded back from the task board) and rewrote the persisted
// transcript to include words typed after sending.
func TestTUIPlainTextQueuesAsNextTurn(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeAsking
	m.chatHistory = &chatHistory{}
	m.stream = newTestStream(new(bool))
	m.pendingPrompt = "the original request"

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("focus the tests")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.pendingInputs) != 1 || m.pendingInputs[0] != "focus the tests" {
		t.Fatalf("plain text mid-turn must queue as the next turn: %v", m.pendingInputs)
	}
	if m.pendingPrompt != "the original request" {
		t.Fatalf("a queued line must not join the in-flight prompt: %q", m.pendingPrompt)
	}
	if m.mode != modeAsking || m.stream == nil {
		t.Fatalf("queueing must leave the running turn alone: mode=%v stream=%v", m.mode, m.stream != nil)
	}
}

// TestTUIPendingIndicator shows the queue on the status row — the visible
// "N queued" tag that tells the user their lines are parked, not lost.
func TestTUIPendingIndicator(t *testing.T) {
	m := newTestTUI(t)
	m.pendingInputs = []string{"/a", "/b"}
	line := m.contextLine()
	if !strings.Contains(line, "2") || !strings.Contains(ansiStrip(line), "queued") {
		t.Fatalf("pending queue must be visible in context line: %q", line)
	}
}

// TestTUIBackgroundDoneLeavesModeAlone: a background execDoneMsg commits the
// echo+output into the transcript without touching the running turn's mode.
func TestTUIBackgroundDoneLeavesModeAlone(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeAsking
	m.chatHistory = &chatHistory{}
	m.stream = newTestStream(new(bool))

	bg := newCommandExec(m.execGen)
	bg.background = true
	m = step(m, execDoneMsg{exec: bg, generation: m.execGen, text: "/version", output: "v1.2.3"})
	if m.mode != modeAsking || m.stream == nil {
		t.Fatalf("background done must not disturb the turn: mode=%v", m.mode)
	}
	var saw bool
	for _, b := range m.chatHistory.blocks {
		if b.kind == blockInfo && strings.Contains(b.body, "v1.2.3") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("background exec output should commit into the transcript")
	}
}

// TestTUIDrainOrderIsFIFO: queued lines run in submit order — the queue is a
// queue, not a stack of surprises.
func TestTUIDrainOrderIsFIFO(t *testing.T) {
	m := newTestTUI(t)
	m.pendingInputs = []string{"!first", "!second"}
	next, _ := m.drainPending()
	if len(next.pendingInputs) != 1 || next.pendingInputs[0] != "!second" {
		t.Fatalf("drain must pop FIFO: %v", next.pendingInputs)
	}
	if next.mode != modeExec || next.exec == nil {
		t.Fatalf("drained '!first' should run as an exec: mode=%v", next.mode)
	}
}

// TestTUIAskingBangBangQueues pins the "!!" rule: a repeat mid-turn replays a
// whole turn, so it cannot run as a background shell exec — it joins the
// pending queue like every other foreground-needing line.
func TestTUIAskingBangBangQueues(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeAsking
	m.chatHistory = &chatHistory{}
	m.stream = newTestStream(new(bool))

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!!")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.pendingInputs) != 1 || m.pendingInputs[0] != "!!" {
		t.Fatalf("'!!' must queue — a background replay could spawn a rogue turn: %v", m.pendingInputs)
	}
}

// TestTUIPickerExitDrainsQueue pins the drain gap: a queued command that opens
// a picker ("/tasks") holds the rest of the queue until the picker closes, and
// that close — not another turn ending — is what drains the next line.
func TestTUIPickerExitDrainsQueue(t *testing.T) {
	m := newTestTUI(t)
	m.mode = modeList
	m.pendingInputs = []string{"/clear"}

	nm, _ := m.drainAfter(m.onKey(tea.KeyMsg{Type: tea.KeyEsc}))
	tm := nm.(tuiModel)
	if tm.mode != modeIdle {
		t.Fatalf("Esc should land in idle: mode=%v", tm.mode)
	}
	if len(tm.pendingInputs) != 0 {
		t.Fatalf("picker exit must drain the queue: %v", tm.pendingInputs)
	}
}

// TestTUIDrainChainsIdleCommands pins the drain loop: a queued line that
// resolves without taking the foreground ("/clear") must not strand the rest
// of the queue — the next line runs immediately, in the same drain.
func TestTUIDrainChainsIdleCommands(t *testing.T) {
	m := newTestTUI(t)
	m.chatHistory = &chatHistory{}
	m.pendingInputs = []string{"/clear", "!echo hi"}

	next, _ := m.drainPending()
	if len(next.pendingInputs) != 0 {
		t.Fatalf("queue should be empty after a chained drain: %v", next.pendingInputs)
	}
	if next.mode != modeExec || next.exec == nil {
		t.Fatalf("'!echo hi' should run after /clear: mode=%v exec=%v", next.mode, next.exec != nil)
	}
}

// TestTUIDrainKeepsOrderAcrossForeground pins the full contract: a foreground
// submission stops the drain mid-queue, the remainder waits, and nothing is
// reordered or lost.
func TestTUIDrainKeepsOrderAcrossForeground(t *testing.T) {
	m := newTestTUI(t)
	m.chatHistory = &chatHistory{}
	m.pendingInputs = []string{"/clear", "!first", "/clear"}

	next, _ := m.drainPending()
	if next.mode != modeExec {
		t.Fatalf("'!first' must claim the exec slot: mode=%v", next.mode)
	}
	if len(next.pendingInputs) != 1 || next.pendingInputs[0] != "/clear" {
		t.Fatalf("the line after the exec must stay queued: %v", next.pendingInputs)
	}
}

// TestTUIQueueFullKeepsDraft pins the bound's contract: a refused line is not
// swallowed — it goes back into the input box with a loud error note.
func TestTUIQueueFullKeepsDraft(t *testing.T) {
	m := newTestTUI(t)
	m.chatHistory = &chatHistory{}
	m.pendingInputs = make([]string, pendingMax)

	nm, _ := m.enqueuePending("/status")
	tm := nm.(tuiModel)
	if len(tm.pendingInputs) != pendingMax {
		t.Fatalf("a full queue must refuse, got %d", len(tm.pendingInputs))
	}
	if tm.ta.Value() != "/status" {
		t.Fatalf("refused line must be restored to the input, got %q", tm.ta.Value())
	}
	var saw bool
	for _, b := range m.chatHistory.blocks {
		if b.kind == blockError {
			saw = true
		}
	}
	if !saw {
		t.Fatal("a refused enqueue must surface an error note")
	}
}

// ansiStrip is the test-local name for dropping escape bytes out of a rendered
// line before substring checks — the status row renders ANSI in asking mode.
func ansiStrip(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case !inEsc:
			b.WriteRune(r)
		}
	}
	return b.String()
}
