// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !lite

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Xustalis/OpenPanda/internal/askengine"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/i18n"
	"github.com/Xustalis/OpenPanda/internal/storage"
)

// newNoteTUI builds a model whose repl owns a real board store and, unless
// withEngine is false, a real ask engine — the two dependencies /queue and
// /inject read. The engine is scheduler-less (no capability card), which is the
// honest state of a seat that has not loaded one: queue notes still reach the
// row, inject must refuse rather than pretend.
func newNoteTUI(t *testing.T, withEngine bool) (tuiModel, *core.TaskStore, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		// Kind vm, not the physical default: the engine probes the node
		// identity lock to decide whether it owns the row, and a physical test
		// node collides with a live daemon on this machine.
		Node: config.NodeConfig{Name: "note-test", Kind: "vm", ResourceClass: "Standard"},
		Storage: config.StorageConfig{
			DBPath:       filepath.Join(dir, "panda.db"),
			MemoryPath:   filepath.Join(dir, "memory"),
			ProjectsPath: filepath.Join(dir, "projects"),
			SkillsPath:   filepath.Join(dir, "skills"),
			WorkPath:     dir,
		},
		Model: config.ModelConfig{BaseURL: "http://127.0.0.1:9", Model: "test-model"},
		UI:    config.UIConfig{Locale: "en"},
	}
	db, err := storage.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := core.NewTaskStore(db, nil)

	r := &repl{loc: i18n.Locale("en"), cfg: cfg, store: st, interactive: true}
	if withEngine {
		eng, err := askengine.New(context.Background(), cfg, askengine.Options{})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		t.Cleanup(eng.Close)
		r.engine.Store(eng)
	}
	m := newTUIModel(r)
	m.mode = modeIdle
	return m, st, core.RuntimeNodeID(cfg.Node.Name, cfg.Node.Kind, cfg.Node.EffectiveIdentity())
}

// queueTestTask creates one task owned by this node and puts it on the queue,
// the shape a note needs: PostUserMessage refuses terminal work and refuses
// another node's row, and Create alone leaves the row out of the queue.
func queueTestTask(t *testing.T, st *core.TaskStore, nodeID, title string) core.Task {
	t.Helper()
	task, err := st.Create(context.Background(), "", "", title, nodeID, []string{nodeID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Queue(context.Background(), task.TaskID, nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	return task
}

// userNotes returns the board's user messages for one task.
func userNotes(t *testing.T, st *core.TaskStore, taskID string) []core.TaskMessage {
	t.Helper()
	msgs, err := st.TaskMessages(context.Background(), "task:"+taskID)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	var out []core.TaskMessage
	for _, m := range msgs {
		if strings.HasPrefix(m.Sender, "user:") {
			out = append(out, m)
		}
	}
	return out
}

// TestTUIQueueWritesBoardInsteadOfDispatchingExec is the regression this pair of
// commands exists for: /queue used to fall through submitSlash's default branch
// into the exec pump, which spawns `panda queue …` — a verb that does not
// exist. The note must land on the task's board, as a status grade, with the
// confirmation in the transcript and the model back in idle.
func TestTUIQueueWritesBoardInsteadOfDispatchingExec(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "queue target")

	next, cmd := m.submit("/queue " + task.TaskID + " read the README before the next edit")
	got := next.(tuiModel)

	if got.mode != modeIdle {
		t.Fatalf("/queue is a store write, not an exec — mode should stay idle, got %v", got.mode)
	}
	printed := collectPrinted(cmd)
	if want := i18n.Tf(i18n.English, "cli.task.send.queued", "id", shortID(task.TaskID)); !strings.Contains(printed, want) {
		t.Fatalf("/queue should confirm the delivery, got %q (want %q)", printed, want)
	}
	// A task still sitting in queued with no consumer will never reach a round:
	// the confirmation has to carry that warning, not stop at "sent".
	if want := i18n.T(i18n.English, "cli.queue.noConsumer"); !strings.Contains(printed, want) {
		t.Fatalf("a note to an unconsumed queue should warn, got %q (want %q)", printed, want)
	}

	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 1 {
		t.Fatalf("one /queue should leave exactly one user note, got %d (%+v)", len(notes), notes)
	}
	if notes[0].Body != "read the README before the next edit" {
		t.Fatalf("the note should carry the whole text, got %q", notes[0].Body)
	}
	if notes[0].Grade != core.MsgGradeStatus {
		t.Fatalf("/queue must post a status note, got grade %q", notes[0].Grade)
	}
	if notes[0].ConsumedAt != 0 {
		t.Fatalf("nothing has read the note yet — consumed_at should stay 0, got %d", notes[0].ConsumedAt)
	}
}

// TestTUIQueueDefaultsToFocusedTask covers the gesture the bare form exists for:
// open a running task, then type /queue without pasting a 36-character id. The
// fallback must be the task last opened, and a first word that happens to name a
// real task must still win over the fallback.
func TestTUIQueueDefaultsToFocusedTask(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	focused := queueTestTask(t, st, nodeID, "focused task")
	other := queueTestTask(t, st, nodeID, "other task")

	m.focusTask = focused.TaskID
	next, _ := m.submit("/queue fix the flaky test")
	got := next.(tuiModel)
	if got.mode != modeIdle {
		t.Fatalf("/queue should stay inline, got mode %v", got.mode)
	}
	notes := userNotes(t, st, focused.TaskID)
	if len(notes) != 1 || notes[0].Body != "fix the flaky test" {
		t.Fatalf("the bare form should target the focused task with the whole line, got %+v", notes)
	}
	if n := userNotes(t, st, other.TaskID); len(n) != 0 {
		t.Fatalf("the note must not leak to another task: %+v", n)
	}

	// An explicit leading id wins, and it becomes the new focus.
	next, _ = got.submit("/queue " + other.TaskID + " and add a regression test")
	got = next.(tuiModel)
	if notes := userNotes(t, st, other.TaskID); len(notes) != 1 ||
		notes[0].Body != "and add a regression test" {
		t.Fatalf("an explicit id should retarget the note, got %+v", notes)
	}
	if got.focusTask != other.TaskID {
		t.Fatalf("an explicit send should move the focus, got %q", got.focusTask)
	}
}

// TestTUIInjectQueuesHonestlyWhenAnotherProcessRunsIt is the semantic line
// between the two modes. The abort handle lives in memory, so an inject can only
// cut a round THIS process is running; when the row is running elsewhere the
// note still lands (as a steer, so it leads that executor's next round) but the
// confirmation must not claim an interrupt that never happened.
func TestTUIInjectQueuesHonestlyWhenAnotherProcessRunsIt(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "inject target")
	ctx := context.Background()
	// Take the row to running the way the queue consumer does, without ever
	// registering a cancel handle — exactly a task this process did not start.
	if err := st.ClaimLocal(ctx, task.TaskID, nodeID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.Accept(ctx, task.TaskID, nodeID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	next, cmd := m.submit("/inject " + task.TaskID + " stop and re-read")
	if got := next.(tuiModel); got.mode != modeIdle {
		t.Fatalf("/inject should stay inline, got mode %v", got.mode)
	}
	printed := collectPrinted(cmd)
	short := shortID(task.TaskID)
	if want := i18n.Tf(i18n.English, "cli.task.send.queued", "id", short); !strings.Contains(printed, want) {
		t.Fatalf("/inject on a foreign round should report queued, got %q (want %q)", printed, want)
	}
	if want := i18n.T(i18n.English, "cli.task.send.injectNoInterrupt"); !strings.Contains(printed, want) {
		t.Fatalf("a foreign round cannot be aborted — the note must say so, got %q (want %q)", printed, want)
	}
	if lie := i18n.Tf(i18n.English, "cli.task.send.interrupted", "id", short); strings.Contains(printed, lie) {
		t.Fatalf("nothing was interrupted, so the interrupted confirmation is a lie: %q", printed)
	}

	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 1 || notes[0].Grade != core.MsgGradeSteer {
		t.Fatalf("/inject must post a steer note (it leads the next round), got %+v", notes)
	}
}

// TestTUISendNoteWithoutEngineNamesTheGap keeps the refusal loud: a seat with no
// engine cannot write a task note at all, and inventing a success would be worse
// than naming the missing piece.
func TestTUISendNoteWithoutEngineNamesTheGap(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, false)
	task := queueTestTask(t, st, nodeID, "no engine")

	for _, line := range []string{"/queue ", "/inject "} {
		next, cmd := m.submit(line + task.TaskID + " anything")
		if got := next.(tuiModel); got.mode != modeIdle {
			t.Fatalf("%s without an engine should stay in idle, got %v", line, got.mode)
		}
		if printed := collectPrinted(cmd); !strings.Contains(printed, i18n.T(i18n.English, "tui.error.noModel")) {
			t.Fatalf("%s without an engine should name the gap, got %q", line, printed)
		}
	}
	if notes := userNotes(t, st, task.TaskID); len(notes) != 0 {
		t.Fatalf("a refused send must not reach the board: %+v", notes)
	}
}

// TestTUIMidTurnQueueDoesNotBecomeSteeringText pins the routing split while a
// turn is live: plain text steers the model, a /command is an ask of its own.
// A task note must take the command path — pushing it into the steer channel
// would feed "queue <id> text" to the model as prose.
func TestTUIMidTurnQueueDoesNotBecomeSteeringText(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "mid-turn target")

	cancelled := false
	m.mode = modeAsking
	m.stream = newTestStream(&cancelled)

	next, _ := m.sendTurnInput("/queue " + task.TaskID + " switch to the v2 endpoint")
	if got := next.(tuiModel); got.mode != modeAsking {
		t.Fatalf("a mid-turn note must not end the turn, got mode %v", got.mode)
	}
	if len(m.stream.steer) != 0 {
		t.Fatalf("a /command must not be injected as steering text, steer queue holds %d", len(m.stream.steer))
	}
	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 1 || notes[0].Body != "switch to the v2 endpoint" {
		t.Fatalf("the mid-turn note should land on the board, got %+v", notes)
	}
}

// TestTUITaskPickerMarksUnreadNotes covers the board's missing read receipt: a
// note waits invisibly until the next round folds it, so the picker has to say
// how many are waiting — and stop saying it once the round consumed them.
func TestTUITaskPickerMarksUnreadNotes(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "unread target")

	items := m.buildTaskItems("")
	badge := i18n.Tf(i18n.English, "tui.task.pendingN", "n", "1")
	for _, it := range items {
		if it.ID == task.TaskID && strings.Contains(it.Snippet, badge) {
			t.Fatalf("a task with no user note must not claim one: %q", it.Snippet)
		}
	}

	if err := st.PostUserMessage(context.Background(), task.TaskID, "waiting note",
		core.MsgGradeStatus, nodeID); err != nil {
		t.Fatalf("post note: %v", err)
	}
	items = m.buildTaskItems("")
	found := false
	for _, it := range items {
		if it.ID != task.TaskID {
			continue
		}
		found = true
		if !strings.Contains(it.Snippet, badge) {
			t.Fatalf("the picker should mark %q on the row, got %q", badge, it.Snippet)
		}
	}
	if !found {
		t.Fatal("the task should be listed")
	}

	// The round consumed it: the badge is the half that has to disappear.
	if err := st.MarkTaskMessageConsumed(context.Background(), "task:"+task.TaskID, "user:1", 1); err != nil {
		t.Fatalf("mark consumed: %v", err)
	}
	for _, it := range m.buildTaskItems("") {
		if it.ID == task.TaskID && strings.Contains(it.Snippet, badge) {
			t.Fatalf("a consumed note must drop its badge, got %q", it.Snippet)
		}
	}
}

// TestTUISteerAckReportsDelivery is the ack half of steering: enqueueing into a
// buffered channel proves nothing (the turn can end before the next poll), so
// only the round loop's own callback can turn "queued" into "the model has it".
// An ack from a superseded stream must be dropped, not painted over the new turn.
func TestTUISteerAckReportsDelivery(t *testing.T) {
	m := newTestTUI(t)
	cancelled := false
	m.mode = modeAsking
	m.stream = newTestStream(&cancelled)

	// The command is a batch with the pump re-arm, which blocks by design — the
	// committed block is what carries the text, so inspect that instead.
	first, _ := m.Update(steerAckMsg{stream: m.stream, text: "use tabs", round: 3})
	if got := first.(tuiModel); got.mode != modeAsking {
		t.Fatalf("an ack is transcript noise, not a state change, got mode %v", got.mode)
	}
	if body := lastBlock(first.(tuiModel)).body; !strings.Contains(body, "round 3") ||
		!strings.Contains(body, "use tabs") {
		t.Fatalf("the ack should name the round and the text, got %q", body)
	}

	// A stale stream (the previous ask) must not print over the live one.
	before := len(m.chatHistory.blocks)
	dropped, cmd := m.Update(steerAckMsg{stream: newTestStream(&cancelled), text: "stale", round: 9})
	if got := len(dropped.(tuiModel).chatHistory.blocks); got != before {
		t.Fatalf("an ack from another stream must be dropped, block count %d → %d", before, got)
	}
	if cmd != nil {
		t.Fatalf("a dropped ack should schedule nothing, got %v", cmd)
	}

	// A detached stream is equally stale: its turn was released.
	m.stream.detached = true
	before = len(m.chatHistory.blocks)
	released, _ := m.Update(steerAckMsg{stream: m.stream, text: "late", round: 4})
	if got := len(released.(tuiModel).chatHistory.blocks); got != before {
		t.Fatalf("an ack from a released stream must be dropped, block count %d → %d", before, got)
	}
}

// TestTUIEnterQueuesThenPromotes is the two-step gesture the footer advertises:
// Enter while a task runs queues the note, and Enter again on the same draft
// escalates that same note instead of posting a second one. One board row
// before and after — the round must never read the words twice.
func TestTUIEnterQueuesThenPromotes(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "steer target")
	m.mode = modeAsking
	m.stream = &askStream{steer: make(chan string, 4)}
	m.streamTask = task.TaskID

	next, cmd := m.sendTurnInput("use tabs, not spaces")
	got := next.(tuiModel)
	if got.staged == nil {
		t.Fatal("the first Enter should stage a note so the second can escalate it")
	}
	if got.staged.taskID != task.TaskID || got.staged.key == "" {
		t.Fatalf("staged note = %+v, want a board slot on %s", got.staged, task.TaskID)
	}
	if want := i18n.Tf(i18n.English, "tui.note.queued", "id", shortID(task.TaskID)); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("queue confirmation = %q, want %q", collectPrinted(cmd), want)
	}
	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 1 || notes[0].Grade != core.MsgGradeStatus {
		t.Fatalf("board after Enter = %+v, want one status note", notes)
	}
	// The steer channel is untouched: this path went to the durable board.
	if len(m.stream.steer) != 0 {
		t.Fatalf("a task-backed note should not also hit the steer channel")
	}

	next, cmd = got.sendTurnInput("use tabs, not spaces")
	got = next.(tuiModel)
	if got.staged != nil {
		t.Fatalf("the promotion should close the staged note, still %+v", got.staged)
	}
	if want := i18n.Tf(i18n.English, "tui.note.promoted", "id", shortID(task.TaskID)); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("promote confirmation = %q, want %q", collectPrinted(cmd), want)
	}
	notes = userNotes(t, st, task.TaskID)
	if len(notes) != 1 {
		t.Fatalf("the second Enter posted a duplicate: %+v", notes)
	}
	if notes[0].Grade != core.MsgGradeSteer {
		t.Fatalf("grade after promotion = %q, want steer", notes[0].Grade)
	}
	// The turn persists the user side once, not once per Enter.
	if n := strings.Count(got.pendingPrompt, "use tabs, not spaces"); n != 1 {
		t.Fatalf("pendingPrompt carries the note %d times, want 1: %q", n, got.pendingPrompt)
	}
}

// TestTUIEnterTwiceWithoutTaskStaysHonest covers the task-less turn: its only
// channel delivers at the next round boundary, so the second Enter must say it
// cannot interrupt rather than faking an inject — and must not queue the text
// again.
func TestTUIEnterTwiceWithoutTaskStaysHonest(t *testing.T) {
	m, _, _ := newNoteTUI(t, true)
	m.mode = modeAsking
	m.stream = &askStream{steer: make(chan string, 4)}

	next, _ := m.sendTurnInput("go faster")
	got := next.(tuiModel)
	if got.staged == nil || got.staged.key != "" {
		t.Fatalf("staged note = %+v, want a keyless (steer-channel) note", got.staged)
	}
	if len(m.stream.steer) != 1 {
		t.Fatalf("steer channel holds %d notes, want 1", len(m.stream.steer))
	}

	next, cmd := got.sendTurnInput("go faster")
	if len(m.stream.steer) != 1 {
		t.Fatalf("the second Enter queued the text again (%d notes)", len(m.stream.steer))
	}
	if want := i18n.T(i18n.English, "tui.note.noAbort"); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("second Enter = %q, want the cannot-interrupt note %q", collectPrinted(cmd), want)
	}
	if next.(tuiModel).staged == nil {
		t.Fatal("the note stays staged: nothing was delivered early")
	}
}

// TestTUITaskRowBindsMidTurnNotes pins the id plumbing: the engine reports the
// row before it runs, and that id — not whatever the picker last opened — is
// what a mid-turn note targets.
func TestTUITaskRowBindsMidTurnNotes(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	stream := &askStream{steer: make(chan string, 4)}
	m.mode = modeAsking
	m.stream = stream
	m.focusTask = queueTestTask(t, st, nodeID, "stale picker target").TaskID
	running := queueTestTask(t, st, nodeID, "the running one")

	next, _ := m.Update(taskRowMsg{stream: stream, taskID: running.TaskID})
	got := next.(tuiModel)
	if got.streamTask != running.TaskID {
		t.Fatalf("streamTask = %q, want %q", got.streamTask, running.TaskID)
	}
	next, _ = got.sendTurnInput("look at the failing test")
	got = next.(tuiModel)
	if got.staged == nil || got.staged.taskID != running.TaskID {
		t.Fatalf("note targeted %+v, want the running task", got.staged)
	}
	if notes := userNotes(t, st, m.focusTask); len(notes) != 0 {
		t.Fatalf("the stale picker target got the note: %+v", notes)
	}

	// A row announced by an ask the user already released must not retarget
	// anything: its work is over.
	stale := &askStream{steer: make(chan string, 4), detached: true}
	next, _ = m.Update(taskRowMsg{stream: stale, taskID: "some-other-task"})
	if got := next.(tuiModel); got.streamTask != "" {
		t.Fatalf("a detached stream retargeted the turn: %q", got.streamTask)
	}
}

// TestTUICtrlDRetractsQueuedNote is the delete half of the gesture: the note the
// last Enter queued can be taken back before the round reads it. The board has
// to lose the row (not just the highlight) and the turn's pending prompt has to
// lose the line too, or the transcript would keep claiming a note nobody can
// see — the same drift the stale-write guard exists to prevent.
func TestTUICtrlDRetractsQueuedNote(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "retract target")
	m.mode = modeAsking
	m.stream = &askStream{steer: make(chan string, 4)}
	m.streamTask = task.TaskID

	next, _ := m.sendTurnInput("use tabs, not spaces")
	got := next.(tuiModel)
	if got.staged == nil {
		t.Fatal("precondition: the first Enter should stage a note")
	}
	if n := len(userNotes(t, st, task.TaskID)); n != 1 {
		t.Fatalf("board before retract = %d notes, want 1", n)
	}

	next, cmd := got.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	got = next.(tuiModel)
	if got.staged != nil {
		t.Fatalf("ctrl+d left the note staged: %+v", got.staged)
	}
	if notes := userNotes(t, st, task.TaskID); len(notes) != 0 {
		t.Fatalf("the retracted note is still on the board: %+v", notes)
	}
	if strings.Contains(got.pendingPrompt, "use tabs, not spaces") {
		t.Fatalf("pendingPrompt kept the retracted line: %q", got.pendingPrompt)
	}
	if want := i18n.Tf(i18n.English, "tui.note.dropped", "id", shortID(task.TaskID)); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("retract confirmation = %q, want %q", collectPrinted(cmd), want)
	}

	// A second ctrl+d has nothing to take back. Saying so keeps the key from
	// reading as a silent success on a note that was already read by the round.
	before := len(got.chatHistory.blocks)
	again, cmd := got.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if n := len(again.(tuiModel).chatHistory.blocks); n <= before {
		t.Fatal("a no-op retract should still say there is nothing to retract")
	}
	if want := i18n.T(i18n.English, "tui.note.nothing"); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("second ctrl+d = %q, want %q", collectPrinted(cmd), want)
	}
}

// TestTUIReEnterEditsStagedNote: a changed draft aimed at the same task rewrites
// the queued note in place instead of posting beside it. Two rows would let the
// round read both the wrong text and its correction, and the user only ever
// meant one of them.
func TestTUIReEnterEditsStagedNote(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "edit target")
	m.mode = modeAsking
	m.stream = &askStream{steer: make(chan string, 4)}
	m.streamTask = task.TaskID

	next, _ := m.sendTurnInput("use tabs")
	got := next.(tuiModel)
	if got.staged == nil || got.staged.key == "" {
		t.Fatalf("precondition: staged note = %+v, want a board slot", got.staged)
	}
	key := got.staged.key

	next, cmd := got.sendTurnInput("use spaces")
	got = next.(tuiModel)
	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 1 {
		t.Fatalf("re-Enter posted a second note: %+v", notes)
	}
	if notes[0].Body != "use spaces" {
		t.Fatalf("board body = %q, want the corrected text", notes[0].Body)
	}
	if notes[0].Grade != core.MsgGradeStatus {
		t.Fatalf("editing a queued note must not escalate it: grade %q", notes[0].Grade)
	}
	if got.staged == nil || got.staged.key != key || got.staged.text != "use spaces" {
		t.Fatalf("staged after edit = %+v, want the same slot with the new text", got.staged)
	}
	if strings.Contains(got.pendingPrompt, "use tabs") || !strings.Contains(got.pendingPrompt, "use spaces") {
		t.Fatalf("pendingPrompt kept the old wording: %q", got.pendingPrompt)
	}
	if want := i18n.Tf(i18n.English, "tui.note.updated", "id", shortID(task.TaskID)); !strings.Contains(collectPrinted(cmd), want) {
		t.Fatalf("edit confirmation = %q, want %q", collectPrinted(cmd), want)
	}
}

// TestTUIClearedDraftStartsANewNote: once the draft that was queued is gone
// (Esc, or emptied by hand), the next thing typed is a new note. Without that
// rule stageNote's "a changed draft on the same slot is a correction" would
// quietly overwrite the note the user had already confirmed — the round would
// then read only the second message.
func TestTUIClearedDraftStartsANewNote(t *testing.T) {
	m, st, nodeID := newNoteTUI(t, true)
	task := queueTestTask(t, st, nodeID, "second note target")
	m.mode = modeAsking
	m.stream = &askStream{steer: make(chan string, 4)}
	m.streamTask = task.TaskID

	m.ta.SetValue("use tabs")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(tuiModel)
	if got.staged == nil {
		t.Fatal("precondition: Enter should queue the first note")
	}

	// Esc clears the box: that draft is finished, so its note stops being the
	// one the next Enter would escalate or rewrite.
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got = next.(tuiModel)
	if got.staged != nil {
		t.Fatalf("clearing the draft left the note staged: %+v", got.staged)
	}

	got.ta.SetValue("also add the changelog line")
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got = next.(tuiModel)
	notes := userNotes(t, st, task.TaskID)
	if len(notes) != 2 {
		t.Fatalf("board has %d notes, want the second one added: %+v", len(notes), notes)
	}
	if notes[0].Body != "use tabs" {
		t.Fatalf("the first note was rewritten instead of kept: %q", notes[0].Body)
	}
	if got.staged == nil || got.staged.text != "also add the changelog line" {
		t.Fatalf("staged after the second Enter = %+v", got.staged)
	}
}
