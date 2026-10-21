// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !lite

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
