// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/commander"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/defense"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/pyexec"
	"github.com/Xustalis/OpenPanda/internal/storage"
	"github.com/Xustalis/OpenPanda/internal/testdb"
)

// steerTestTask enqueues one task without a scheduler running, so it sits
// queued — the "message a parked task" case both send modes must support.
func steerTestTask(t *testing.T, c *Core) Task {
	t.Helper()
	tk, err := c.Enqueue(context.Background(), TaskInput{
		Title: "steer target", Intent: "work", Requires: []string{"sys:info"}, Authorized: true,
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return tk
}

// TestUserMessageModes pins the two send modes end to end at the kernel
// boundary: queue lands a plain board note the round drain folds as context;
// inject lands a steer-grade note the drain skips and the round's takeSteer
// delivers exactly once as the leading directive; a live round's cancel
// fires on inject and is reported as "interrupted".
func TestUserMessageModes(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)

	// queue: durable note, grade status, no interrupt semantics.
	if err := c.QueueUserMessage(ctx, tk.TaskID, "  also update the README  "); err != nil {
		t.Fatalf("queue message: %v", err)
	}
	// inject with no live round: reported as queued-for-boundary.
	status, err := c.InjectUserMessage(ctx, tk.TaskID, "stop editing; check the logs first")
	if err != nil || status != "queued" {
		t.Fatalf("inject status = %q err=%v, want queued", status, err)
	}
	// Both messages SURVIVE on the board: the per-sender slot would have
	// made the second overwrite the first, so user sends use one slot each.
	msgs, err := c.store.TaskMessages(ctx, "task:"+tk.TaskID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("board after two sends: %v msgs=%d want 2", err, len(msgs))
	}
	byBody := map[string]TaskMessage{}
	for _, m := range msgs {
		byBody[m.Body] = m
	}
	if q := byBody["also update the README"]; q.Grade != MsgGradeStatus || q.SenderLabel != "user" {
		t.Fatalf("queue note = %+v", q)
	}
	if s := byBody["stop editing; check the logs first"]; s.Grade != MsgGradeSteer || s.SenderLabel != "user" {
		t.Fatalf("steer note = %+v", s)
	}
	// User notes stay pending until the prompt carrying each one is handed
	// off. The sibling-board drain cannot declare a received user note.
	task, _ := c.store.Get(ctx, tk.TaskID)
	seen := msgSeen{}
	if lines := c.drainTaskMessages(ctx, task, seen); len(lines) != 0 {
		t.Fatalf("sibling drain folded user notes: %v", lines)
	}
	queued := c.pendingQueuedNotes(ctx, tk.TaskID)
	if len(queued) != 1 || queued[0].Body != "also update the README" {
		t.Fatalf("queued notes = %v", queued)
	}
	if got := consumedAt(t, c, tk.TaskID, queued[0].Body); got != 0 {
		t.Fatal("preparing a queue note marked it consumed")
	}
	c.markPromptNoteDelivered(ctx, tk.TaskID, queued[0], "")
	if got := consumedAt(t, c, tk.TaskID, "stop editing; check the logs first"); got != 0 {
		t.Fatalf("steer note must stay unread until takeSteer, consumed_at=%d", got)
	}
	if again := c.drainTaskMessages(ctx, task, seen); len(again) != 0 {
		t.Fatalf("drain replayed seen note: %v", again)
	}
	if lines := c.drainTaskMessages(ctx, task, seen); len(lines) != 0 {
		t.Fatalf("drain folded a steer note: %v", lines)
	}
	if !c.steerUnseen(ctx, tk.TaskID, seen) {
		t.Fatal("steer note must read unseen before takeSteer")
	}
	// takeSteer delivers it once, as the leading directive.
	if got := c.takeSteer(ctx, tk.TaskID, seen); got != "stop editing; check the logs first" {
		t.Fatalf("takeSteer = %q", got)
	}
	if got := c.takeSteer(ctx, tk.TaskID, seen); got != "" {
		t.Fatalf("takeSteer replayed: %q", got)
	}
	if c.steerUnseen(ctx, tk.TaskID, seen) {
		t.Fatal("steer must read seen after takeSteer")
	}
	if got := consumedAt(t, c, tk.TaskID, "stop editing; check the logs first"); got == 0 {
		t.Fatal("taken steer note must be stamped consumed")
	}

	// inject with a live round: the registered cancel fires and the caller
	// is told the turn was aborted.
	fired := false
	c.registerInterrupt(tk.TaskID, func() { fired = true })
	status, err = c.InjectUserMessage(ctx, tk.TaskID, "abort and redo")
	if err != nil || status != "interrupted" || !fired {
		t.Fatalf("live inject: status=%q fired=%v err=%v", status, fired, err)
	}
	c.clearInterrupt(tk.TaskID)

	// Every posted note carries a real timestamp — a zero SentAt rendered
	// "~57 years ago" on `panda task board` and broke ordering by time.
	// Re-fetch first: the live inject above posted the third note, and the
	// freshness check (not just > 0) is what catches a wrong unit — a
	// millisecond stamp is positive too.
	msgs, err = c.store.TaskMessages(ctx, "task:"+tk.TaskID)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("board after three sends: %v msgs=%d want 3", err, len(msgs))
	}
	now := time.Now().Unix()
	for _, m := range msgs {
		if age := now - m.SentAt; age < 0 || age > 60 {
			t.Fatalf("user message %q has SentAt=%d (age %ds), want a fresh unix ts", m.Body, m.SentAt, age)
		}
	}

	// A terminal task refuses both modes.
	if _, err := c.store.db.ExecContext(ctx,
		`UPDATE tasks SET state='failed' WHERE task_id=?`, tk.TaskID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := c.QueueUserMessage(ctx, tk.TaskID, "late"); err == nil {
		t.Fatal("queue must refuse a terminal task")
	}
	if _, err := c.InjectUserMessage(ctx, tk.TaskID, "late"); err == nil {
		t.Fatal("inject must refuse a terminal task")
	}
}

// A steer note is stamped "read" only when an adapter actually took the
// prompt. The callback that used to stamp it fired before the adapter was
// called, so a round whose adapter never launched still consumed the user's
// instruction: the console stopped offering it to edit, and no agent had seen
// it. Both directions are pinned — the gate must not turn stamping off.
func TestSteerNoteDeliveryFollowsTheAdapterLaunch(t *testing.T) {
	run := func(t *testing.T, announce bool) int64 {
		t.Helper()
		ctx := context.Background()
		c := newSuperviseCore(t, "steer-launch", defense.TierReversible)
		c.SetSuperviseRounds(1)
		c.router.SetAdapterRunner(func(runCtx context.Context, _, _, _ string) commander.AgentResult {
			if announce {
				commander.PromptDelivered(runCtx)
			}
			return commander.AgentResult{OK: false, ExitCode: 127, Result: "adapter script not found"}
		})
		task, err := c.Enqueue(ctx, TaskInput{
			Title: "steer", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
		}, DefaultQueueSpec())
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := c.InjectUserMessage(ctx, task.TaskID, "stop and read the log first"); err != nil {
			t.Fatalf("inject: %v", err)
		}
		if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		c.runScheduled(ctx, task.TaskID)
		return consumedAt(t, c, task.TaskID, "stop and read the log first")
	}
	// A launch refusal: nothing read the prompt, so the note stays pending and
	// a later round (or the user) can still deliver it.
	if got := run(t, false); got != 0 {
		t.Fatalf("a launch failure stamped the note read: consumed_at=%d", got)
	}
	// A delivered run: the row must say so, or the console keeps offering to
	// edit an instruction the agent already has.
	if got := run(t, true); got == 0 {
		t.Fatal("a delivered note must be stamped read")
	}
}

// A round can be aborted between the peek and the hand-off: the directive is
// already folded into the intent, the user injects a second one, and the
// cancelled round never reaches an adapter. The next round's prompt carries
// both notes, so delivery must stamp both — stamping only the note that round
// peeked left the first one pending forever while the agent was reading it.
func TestSteerNotesAbortedBeforeDeliveryAreStampedNextRound(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "steer-abort", defense.TierReversible)
	c.SetSuperviseRounds(1)
	task, err := c.Enqueue(ctx, TaskInput{
		Title: "steer abort", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := c.InjectUserMessage(ctx, task.TaskID, "directive A"); err != nil {
		t.Fatalf("inject A: %v", err)
	}
	entered := make(chan struct{})
	var calls atomic.Int32
	c.router.SetAdapterRunner(func(runCtx context.Context, _, prompt, _ string) commander.AgentResult {
		if calls.Add(1) == 1 {
			// The round is live but the prompt has not been delivered: wait
			// for the inject that aborts it.
			close(entered)
			<-runCtx.Done()
			return commander.AgentResult{ExitCode: 1, Result: "interrupted"}
		}
		if !strings.Contains(prompt, "directive A") || !strings.Contains(prompt, "directive B") {
			t.Errorf("second round prompt lost a directive: %q", prompt)
		}
		commander.PromptDelivered(runCtx)
		return commander.AgentResult{OK: true, Result: "done"}
	})
	if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runScheduled(ctx, task.TaskID)
	}()
	<-entered
	if status, err := c.InjectUserMessage(ctx, task.TaskID, "directive B"); err != nil || status != "interrupted" {
		close(done)
		t.Fatalf("inject B: status=%q err=%v", status, err)
	}
	<-done
	for _, body := range []string{"directive A", "directive B"} {
		if got := consumedAt(t, c, task.TaskID, body); got == 0 {
			t.Fatalf("%q rode the delivered prompt but is still unread", body)
		}
	}
}

func TestPendingSteerEditsAndDeletionReachNextPrompt(t *testing.T) {
	const original = "keep the first draft"
	const revised = "replace the first draft"
	const additional = "include a summary"
	for _, tc := range []struct {
		name   string
		edit   bool
		inject bool
	}{
		{name: "edit interrupts via watcher", edit: true},
		{name: "edit with another directive", edit: true, inject: true},
		{name: "delete interrupts via watcher"},
		{name: "delete before another directive", inject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c := newSuperviseCore(t, "steer-revision", defense.TierReversible)
			c.SetSuperviseRounds(1)
			task, err := c.Enqueue(ctx, TaskInput{
				Title: "edit notes", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
			}, DefaultQueueSpec())
			if err != nil {
				t.Fatal(err)
			}
			old, _, err := c.InjectUserNote(ctx, task.TaskID, original, nil)
			if err != nil {
				t.Fatal(err)
			}
			var prompts []string
			c.router.SetAdapterRunner(func(runCtx context.Context, _, prompt, _ string) commander.AgentResult {
				prompts = append(prompts, prompt)
				if len(prompts) > 1 {
					commander.PromptDelivered(runCtx)
					return commander.AgentResult{OK: true, Result: "done"}
				}
				if tc.edit {
					if _, err := c.store.EditUserNote(ctx, task.TaskID, old.Sender, revised, nil, c.nodeID); err != nil {
						t.Fatal(err)
					}
				} else if err := c.store.DeleteUserNote(ctx, task.TaskID, old.Sender); err != nil {
					t.Fatal(err)
				}
				if tc.inject {
					if status, err := c.InjectUserMessage(ctx, task.TaskID, additional); err != nil || status != "interrupted" {
						t.Fatalf("second directive = %q, %v", status, err)
					}
				}
				<-runCtx.Done()
				return commander.AgentResult{ExitCode: 1, Result: "interrupted before delivery"}
			})
			if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
				t.Fatal(err)
			}
			c.runScheduled(ctx, task.TaskID)
			if len(prompts) != 2 || !strings.Contains(prompts[0], original) {
				t.Fatalf("expected two attempts and the original in the first, got %q", prompts)
			}
			second := prompts[1]
			if strings.Contains(second, original) {
				t.Errorf("the stale directive reached the next adapter: %q", second)
			}
			if got := strings.Contains(second, revised); got != tc.edit {
				t.Errorf("revised directive present = %t, want %t", got, tc.edit)
			}
			if tc.edit && consumedAt(t, c, task.TaskID, revised) == 0 {
				t.Error("the edited directive was not marked delivered")
			}
			if !tc.edit {
				events, err := c.store.Events(ctx, task.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type == EvUserMsgConsumed && strings.Contains(event.DataJSON, original) {
						t.Fatalf("withdrawn text recorded as delivered: %s", event.DataJSON)
					}
				}
			}
			if tc.inject {
				other := strings.Index(second, additional)
				if other < 0 || (tc.edit && other > strings.Index(second, revised)) {
					t.Errorf("new directive should lead the carried directive: %q", second)
				}
				if consumedAt(t, c, task.TaskID, additional) == 0 {
					t.Error("the new directive was not marked delivered")
				}
			}
		})
	}
}

func TestPrunedReadSteerDoesNotCancelTheAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c := newSuperviseCore(t, "prune-read-steer", defense.TierReversible)
	c.SetSuperviseRounds(1)
	task, err := c.Enqueue(ctx, TaskInput{
		Title: "steer while running", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := c.InjectUserNote(ctx, task.TaskID, "read this instruction", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.router.SetAdapterRunner(func(runCtx context.Context, _, prompt, _ string) commander.AgentResult {
		calls++
		if calls != 1 || !strings.Contains(prompt, note.Body) {
			t.Fatal("read steer failed to stay in the original round")
		}
		commander.PromptDelivered(runCtx)
		if _, err := c.store.db.ExecContext(ctx, `DELETE FROM task_messages WHERE scope=? AND sender=? AND consumed_at>0`,
			note.Scope, note.Sender); err != nil {
			t.Fatal(err)
		}
		select {
		case <-runCtx.Done():
			t.Fatal("message cleanup cancelled a round that already acknowledged it")
		case <-time.After(1250 * time.Millisecond):
		}
		return commander.AgentResult{OK: true, Result: "done"}
	})
	if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
		t.Fatal(err)
	}
	c.runScheduled(ctx, task.TaskID)
}

func TestNativeObjectiveReviewDoesNotReplayForPendingSteer(t *testing.T) {
	const directive = "do another operation"
	for _, tc := range []struct {
		name   string
		inject bool
	}{
		{name: "no directive"},
		{name: "pending directive", inject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			work := t.TempDir()
			marker := filepath.Join(work, "executions.log")
			card := ledger.Card{
				Device: "native-objective",
				Native: []ledger.NativeAbility{queueFixtureAbility(t, "sys:info", "append", marker)},
			}
			c := NewCore(openTestDB(t), card.Device, card, 5, testLogger(), config.ModelConfig{})
			c.SetWorkDir(work)
			var reviews atomic.Int32
			c.SetSupervisor(newFakeSupervisor(t, func(int) string {
				reviews.Add(1)
				return `{"status":"done","objective_assessments":[{"dimension":"visual","criterion":0,"status":"passed","evidence":"verified output"}]}`
			}))
			task, err := c.Enqueue(ctx, TaskInput{
				Title: "append once", Intent: "record execution", SpecJSON: objectiveSpec,
				Requires: []string{"sys:info"}, Authorized: true,
			}, DefaultQueueSpec())
			if err != nil {
				t.Fatal(err)
			}
			if tc.inject {
				if _, err := c.InjectUserMessage(ctx, task.TaskID, directive); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
				t.Fatal(err)
			}
			c.runScheduled(ctx, task.TaskID)
			got, err := c.store.Get(ctx, task.TaskID)
			if err != nil || got.State != StateDone {
				t.Fatalf("native task state = %q, %v (result %q)", got.State, err, got.ResultJSON)
			}
			recorded, err := os.ReadFile(marker)
			if err != nil || string(recorded) != "executed\n" || reviews.Load() != 1 {
				t.Fatalf("execution log = %q (%v), reviews = %d, want one execution and review", recorded, err, reviews.Load())
			}
			if tc.inject && consumedAt(t, c, task.TaskID, directive) != 0 {
				t.Error("a directive for an agent was marked read by native execution")
			}
		})
	}
}

func TestQueueNoteDeliveryFollowsPromptReceipt(t *testing.T) {
	const noteText = "Include the handoff detail"
	for _, tc := range []struct {
		name, ability     string
		native, delivered bool
	}{
		{name: "launch failed", ability: "code:modify"},
		{name: "agent acknowledged", ability: "code:modify", delivered: true},
		{name: "native command completed", ability: "sys:info", native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var c *Core
			if tc.native {
				work := t.TempDir()
				card := ledger.Card{Device: "queue-native", Native: []ledger.NativeAbility{
					queueFixtureAbility(t, tc.ability, "append", filepath.Join(work, "runs.log")),
				}}
				c = NewCore(openTestDB(t), card.Device, card, 5, testLogger(), config.ModelConfig{})
				c.SetWorkDir(work)
			} else {
				c = newSuperviseCore(t, "queue-delivery", defense.TierReversible)
				c.router.SetAdapterRunner(func(runCtx context.Context, _, prompt, _ string) commander.AgentResult {
					if !strings.Contains(prompt, noteText) {
						t.Fatal("the prepared prompt omitted the queued note")
					}
					if !tc.delivered {
						return commander.AgentResult{ExitCode: 127, Result: "fixture agent unavailable"}
					}
					commander.PromptDelivered(runCtx)
					return commander.AgentResult{OK: true, Result: "done"}
				})
			}
			c.SetSuperviseRounds(1)
			c.sleep = func(time.Duration) {}
			task, err := c.Enqueue(ctx, TaskInput{
				Title: "queue delivery", Intent: "work", Requires: []string{tc.ability}, Authorized: true,
			}, DefaultQueueSpec())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.QueueUserMessage(ctx, task.TaskID, noteText); err != nil {
				t.Fatal(err)
			}
			if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
				t.Fatal(err)
			}
			c.runScheduled(ctx, task.TaskID)
			if got := consumedAt(t, c, task.TaskID, noteText) != 0; got != tc.delivered {
				t.Errorf("note marked delivered = %t, want %t", got, tc.delivered)
			}
			pending, err := c.store.UserNotes(ctx, task.TaskID)
			if err != nil || (len(pending) == 0) != tc.delivered {
				t.Errorf("pending notes = %v, err=%v", pending, err)
			}
			if tc.native {
				got, err := c.store.Get(ctx, task.TaskID)
				if err != nil || got.State != StateDone {
					t.Fatalf("native fixture state = %q, err=%v", got.State, err)
				}
			}
		})
	}
}

func TestFailedChildLaunchLeavesTaskNotesPending(t *testing.T) {
	if !pyexec.Available() {
		t.Skip("adapter transport requires Python")
	}
	const adapter = `
import os
import _harness as harness

req = harness.read_request()
try:
    harness.run_plain([os.path.join(req.cwd, "absent-panda-fixture.exe"), req.prompt], cwd=req.cwd)
except FileNotFoundError:
    harness.emit(False, "fixture child binary not found", 127)
`
	ctx := context.Background()
	dir := t.TempDir()
	harness, err := os.ReadFile(filepath.Join("..", "..", "adapters", "_harness.py"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"claude_code.py": []byte(adapter), "_harness.py": harness} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENPANDA_ADAPTER_DIR", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	c := newSuperviseCore(t, "missing-child", defense.TierReversible)
	c.SetSuperviseRounds(1)
	c.sleep = func(time.Duration) {}
	task, err := c.Enqueue(ctx, TaskInput{
		Title: "missing fixture subprocess", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.QueueUserMessage(ctx, task.TaskID, "queue for the child"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InjectUserMessage(ctx, task.TaskID, "steer the child"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
		t.Fatal(err)
	}
	c.runScheduled(ctx, task.TaskID)
	got, err := c.store.Get(ctx, task.TaskID)
	if err != nil || !strings.Contains(got.ResultJSON, "fixture child binary not found") {
		t.Fatalf("task did not reach the downstream spawn refusal: %s, %v", got.ResultJSON, err)
	}
	notes, err := c.store.UserNotes(ctx, task.TaskID)
	if err != nil || len(notes) != 2 {
		t.Fatalf("downstream launch failure consumed user notes: %v, %v", notes, err)
	}
}

func TestPendingNoteDeliveryFencesConcurrentMutations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "tasks.db")
	db := testdb.OpenFile(t, path)
	card := ledger.Card{Device: "delivery-fixture"}
	c := NewCore(db, card.Device, card, 5, testLogger(), config.ModelConfig{})
	otherDB, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other := NewCore(otherDB, card.Device, card, 5, testLogger(), config.ModelConfig{})
	task := steerTestTask(t, c)
	if err := c.store.ClaimLocal(ctx, task.TaskID, card.Device); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE task_id=?`, task.TaskID); err != nil {
		t.Fatal(err)
	}
	task, err = c.store.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}

	note, err := c.QueueUserNote(ctx, task.TaskID, "original", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.store.DeleteUserNote(ctx, task.TaskID, note.Sender); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.reservePromptNotes(ctx, task.TaskID, task.AttemptID, []TaskMessage{note}); !errors.Is(err, errPromptNotesChanged) {
		t.Fatalf("stale prompt handoff = %v", err)
	}
	note, err = c.QueueUserNote(ctx, task.TaskID, "queued after withdrawal", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldHandoff, err := c.store.reservePromptNotes(ctx, task.TaskID, task.AttemptID, []TaskMessage{note})
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := c.store.reservePromptNotes(ctx, task.TaskID, task.AttemptID, []TaskMessage{note})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.releasePromptNotes(ctx, task.TaskID, oldHandoff); err != nil {
		t.Fatal(err)
	}
	if c.markPromptNoteDelivered(ctx, task.TaskID, note, oldHandoff) {
		t.Fatal("stale receipt acknowledged a replacement handoff")
	}
	if err := other.store.DeleteUserNote(ctx, task.TaskID, note.Sender); !errors.Is(err, ErrConflict) {
		t.Fatalf("withdraw during handoff = %v", err)
	}
	if _, err := other.PromoteUserNote(ctx, task.TaskID, note.Sender); !errors.Is(err, ErrConflict) {
		t.Fatalf("promote during handoff = %v", err)
	}
	if err := c.store.releasePromptNotes(ctx, task.TaskID, handoff); err != nil {
		t.Fatal(err)
	}
	edited, err := other.store.EditUserNote(ctx, task.TaskID, note.Sender, "revised after failed delivery", nil, card.Device)
	if err != nil || edited.Body != "revised after failed delivery" {
		t.Fatalf("edit after released handoff = %+v, %v", edited, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	finished := make(chan error, 1)
	go func() {
		_, err := other.store.EditUserNote(ctx, task.TaskID, edited.Sender, "should not overwrite read content", nil, card.Device)
		finished <- err
	}()
	for otherDB.Stats().InUse == 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("edit never reached the SQLite write lock")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_messages SET consumed_at=? WHERE scope=? AND sender=? AND sender_seq=?`,
		time.Now().Unix(), edited.Scope, edited.Sender, edited.SenderSeq); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "already read") {
			t.Fatalf("concurrent edit result = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("edit remained blocked")
	}
	actual, err := c.store.userNote(ctx, task.TaskID, edited.Sender)
	if err != nil || actual.Body != edited.Body || actual.SenderSeq != edited.SenderSeq || actual.ConsumedAt == 0 {
		t.Fatalf("read note mutated: %+v, %v", actual, err)
	}
}

// consumedAt reads one note's delivery stamp off the board, failing when the
// body is absent — a silent 0 would make the "must be stamped" assertions pass
// for the wrong reason.
func consumedAt(t *testing.T, c *Core, taskID, body string) int64 {
	t.Helper()
	msgs, err := c.store.TaskMessages(context.Background(), "task:"+taskID)
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	for _, m := range msgs {
		if m.Body == body {
			return m.ConsumedAt
		}
	}
	t.Fatalf("board has no note %q", body)
	return 0
}

// TestPendingNoteCounts pins the badge source both pickers read: unread user
// notes counted per task, and the count dropping to zero once a round folds
// them. A stale count is worse than no count — it tells the user their note is
// still queued when the agent already read it.
func TestPendingNoteCounts(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)
	if err := c.QueueUserMessage(ctx, tk.TaskID, "first"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.QueueUserMessage(ctx, tk.TaskID, "second"); err != nil {
		t.Fatalf("queue 2: %v", err)
	}
	counts, err := c.store.PendingNoteCounts(ctx)
	if err != nil {
		t.Fatalf("pending counts: %v", err)
	}
	if got := counts[tk.TaskID]; got != 2 {
		t.Fatalf("pending notes = %d, want 2 (map=%v)", got, counts)
	}
	for _, note := range c.pendingQueuedNotes(ctx, tk.TaskID) {
		c.markPromptNoteDelivered(ctx, tk.TaskID, note, "")
	}
	counts, err = c.store.PendingNoteCounts(ctx)
	if err != nil {
		t.Fatalf("pending counts after delivery: %v", err)
	}
	if got := counts[tk.TaskID]; got != 0 {
		t.Fatalf("pending notes after delivery = %d, want 0", got)
	}
}

// TestUserMessageEmptyAndMissing pins the guards: empty text and unknown ids
// are refused before any board write.
func TestUserMessageEmptyAndMissing(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	if err := c.QueueUserMessage(ctx, "no-such-task", "hi"); err == nil {
		t.Fatal("unknown task must be refused")
	}
	tk := steerTestTask(t, c)
	if err := c.QueueUserMessage(ctx, tk.TaskID, "   "); err == nil {
		t.Fatal("empty text must be refused")
	}
	if _, err := c.InjectUserMessage(ctx, tk.TaskID, ""); err == nil {
		t.Fatal("empty inject must be refused")
	}
}

// TestPostUserMessageOwnerGuard pins the store-level owner check every
// delivery path now shares — including the scheduler-less fallbacks (panel
// without engine, REPL without engine, scheduler-less ask engine). A task
// owned by another node reads that node's board: a note landed here would
// silently never deliver, so the store refuses it.
func TestPostUserMessageOwnerGuard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mine := createTask(t, s, "", "local work", "local-node")
	remote := createTask(t, s, "", "remote work", "local-node")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET owner_node=? WHERE task_id=?`, "peer-node", remote.TaskID); err != nil {
		t.Fatalf("reassign owner: %v", err)
	}
	if err := s.PostUserMessage(ctx, remote.TaskID, "hi", MsgGradeStatus, "local-node"); err == nil ||
		!strings.Contains(err.Error(), "runs on peer-node") {
		t.Fatalf("remote-owned task must refuse with executor hint, got %v", err)
	}
	if err := s.PostUserMessage(ctx, mine.TaskID, "hi", MsgGradeStatus, "local-node"); err != nil {
		t.Fatalf("local task must accept: %v", err)
	}
	// An ephemeral sibling of the owning node is the same runtime identity.
	if err := s.PostUserMessage(ctx, mine.TaskID, "hi", MsgGradeStatus, "local-node-1a2b3c4d"); err != nil {
		t.Fatalf("ephemeral sibling must accept: %v", err)
	}
	// A caller that cannot name the store owner degrades to no owner check.
	if err := s.PostUserMessage(ctx, remote.TaskID, "hi", MsgGradeStatus, ""); err != nil {
		t.Fatalf("empty local id skips the guard: %v", err)
	}
}

// TestUserNoteLifecycle pins the store-side lifecycle the console's pending
// strip drives: list → edit → drop, all on the same slot, none of it posting a
// second row.
func TestUserNoteLifecycle(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)

	msg, err := c.QueueUserNote(ctx, tk.TaskID, "check the tests", []string{"uploads/shot.png"})
	if err != nil {
		t.Fatalf("queue note: %v", err)
	}
	if !strings.HasPrefix(msg.Sender, userSenderPrefix) {
		t.Fatalf("note sender = %q, want a user:<seq> slot", msg.Sender)
	}
	// The agent reads the body; the console reads the prose. Both must be
	// right from one row: a chip list alone would leave the prompt without the
	// path, and the raw body alone would show the attachment header as prose.
	if !strings.Contains(msg.Body, "- @uploads/shot.png") {
		t.Fatalf("body should carry the attachment ref: %q", msg.Body)
	}
	if got := NoteProse(msg.Body); got != "check the tests" {
		t.Fatalf("NoteProse = %q", got)
	}

	notes, err := c.store.UserNotes(ctx, tk.TaskID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("UserNotes = %v err=%v, want 1", notes, err)
	}
	if notes[0].Attachments == nil || notes[0].Attachments[0] != "uploads/shot.png" {
		t.Fatalf("attachments did not round-trip: %+v", notes[0].Attachments)
	}

	// Edit keeps the slot and the grade, bumps the seq (the store's stale-write
	// guard needs it) and drops the attachment the user removed.
	edited, err := c.store.EditUserNote(ctx, tk.TaskID, msg.Sender, "check the tests twice", nil, c.nodeID)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Sender != msg.Sender || edited.Grade != MsgGradeStatus {
		t.Fatalf("edit changed identity: %+v", edited)
	}
	if edited.SenderSeq <= msg.SenderSeq {
		t.Fatalf("edit kept seq %d (was %d) — the store would drop it as stale", edited.SenderSeq, msg.SenderSeq)
	}
	if got := NoteProse(edited.Body); got != "check the tests twice" {
		t.Fatalf("edited prose = %q", got)
	}

	if err := c.store.DeleteUserNote(ctx, tk.TaskID, msg.Sender); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if notes, _ := c.store.UserNotes(ctx, tk.TaskID); len(notes) != 0 {
		t.Fatalf("note survived the drop: %+v", notes)
	}
}

// TestPromoteKeepsOneRow is the second-Enter contract: escalation rewrites the
// queued slot as a steer instead of posting a duplicate, so the round reads the
// user's words exactly once.
func TestPromoteKeepsOneRow(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)

	msg, err := c.QueueUserNote(ctx, tk.TaskID, "use tabs", nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	status, err := c.PromoteUserNote(ctx, tk.TaskID, msg.Sender)
	if err != nil || status != "queued" {
		t.Fatalf("promote = %q err=%v, want queued (no running round here)", status, err)
	}
	msgs, err := c.store.TaskMessages(ctx, "task:"+tk.TaskID)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	var user []TaskMessage
	for _, m := range msgs {
		if isUserSender(m.Sender) {
			user = append(user, m)
		}
	}
	if len(user) != 1 {
		t.Fatalf("promote posted a second row: %+v", user)
	}
	if user[0].Grade != MsgGradeSteer {
		t.Fatalf("promoted grade = %q, want steer", user[0].Grade)
	}
	if user[0].SenderSeq <= msg.SenderSeq {
		t.Fatalf("promoted seq %d did not advance past %d", user[0].SenderSeq, msg.SenderSeq)
	}

	// A live round's cancel is what turns the same call into "interrupted".
	cctx, cancel := context.WithCancel(context.Background())
	c.registerInterrupt(tk.TaskID, cancel)
	msg2, err := c.QueueUserNote(ctx, tk.TaskID, "and run gofmt", nil)
	if err != nil {
		t.Fatalf("queue 2: %v", err)
	}
	status, err = c.PromoteUserNote(ctx, tk.TaskID, msg2.Sender)
	if err != nil || status != "interrupted" {
		t.Fatalf("promote with a live round = %q err=%v, want interrupted", status, err)
	}
	if cctx.Err() == nil {
		t.Fatal("promote did not abort the live round")
	}
	c.clearInterrupt(tk.TaskID)
}

// TestConsumedNoteRefusesEdits keeps the board honest: once a round folded a
// note into its prompt, rewriting or deleting the row would leave the board
// disagreeing with what the model was actually told.
func TestConsumedNoteRefusesEdits(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)
	msg, err := c.QueueUserNote(ctx, tk.TaskID, "already read", nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := c.store.MarkTaskMessageConsumed(ctx, msg.Scope, msg.Sender, msg.SenderSeq); err != nil {
		t.Fatalf("mark consumed: %v", err)
	}
	if _, err := c.store.EditUserNote(ctx, tk.TaskID, msg.Sender, "rewritten", nil, c.nodeID); err == nil ||
		!strings.Contains(err.Error(), "already read") {
		t.Fatalf("edit of a consumed note = %v, want an already-read refusal", err)
	}
	if err := c.store.DeleteUserNote(ctx, tk.TaskID, msg.Sender); err == nil ||
		!strings.Contains(err.Error(), "already read") {
		t.Fatalf("drop of a consumed note = %v, want an already-read refusal", err)
	}
	if _, err := c.store.EditUserNote(ctx, tk.TaskID, "user:999", "ghost", nil, c.nodeID); err == nil ||
		!strings.Contains(err.Error(), "no such note") {
		t.Fatalf("edit of an unknown key = %v, want no-such-note", err)
	}
}

// Reading a steer into a round must not claim the executor received it. The
// durable stamp is what a restart consults (and what the console draws as
// "read"); a crash between the drain and the adapter hand-off has to leave the
// note deliverable.
func TestPeekRoundSteerLeavesDeliveryUnstamped(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	tk := steerTestTask(t, c)
	if _, err := c.InjectUserMessage(ctx, tk.TaskID, "keep the API stable"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	seen := msgSeen{}
	note := c.peekRoundSteer(ctx, tk.TaskID, seen)
	if note == nil || note.body != "keep the API stable" || note.label != "[user directive]" {
		t.Fatalf("peek = %+v", note)
	}
	if got := consumedAt(t, c, tk.TaskID, "keep the API stable"); got != 0 {
		t.Fatalf("peek stamped the note consumed_at=%d, want pending", got)
	}
	c.markSteerDelivered(ctx, tk.TaskID, note, "")
	if got := consumedAt(t, c, tk.TaskID, "keep the API stable"); got == 0 {
		t.Fatal("delivered note must be stamped consumed")
	}
}

func TestSteerReceiptDoesNotConsumeReplacement(t *testing.T) {
	c := queueTestCore(t)
	ctx := context.Background()
	task := steerTestTask(t, c)
	old, _, err := c.InjectUserNote(ctx, task.TaskID, "old directive", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := msgSeen{}
	receipt := c.peekRoundSteer(ctx, task.TaskID, seen)
	if receipt == nil || receipt.body != old.Body {
		t.Fatalf("first peek = %+v", receipt)
	}
	if err := c.store.DeleteUserNote(ctx, task.TaskID, old.Sender); err != nil {
		t.Fatal(err)
	}
	otherStore := NewTaskStore(c.store.db, c.logger)
	replacement, err := otherStore.PostUserNote(ctx, task.TaskID, "replacement", nil, MsgGradeSteer, c.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	c.markSteerDelivered(ctx, task.TaskID, receipt, "")
	if got := consumedAt(t, c, task.TaskID, replacement.Body); got != 0 {
		t.Errorf("old receipt stamped replacement consumed_at=%d", got)
	}
	if replacement.Sender == old.Sender || replacement.SenderSeq <= old.SenderSeq {
		t.Errorf("replacement reused the deleted identity: old=%+v replacement=%+v", old, replacement)
	}
	next := c.peekRoundSteer(ctx, task.TaskID, seen)
	if next == nil || next.body != replacement.Body {
		t.Fatalf("replacement missing from the same round cursor: %+v", next)
	}
	c.markSteerDelivered(ctx, task.TaskID, next, "")
	if got := consumedAt(t, c, task.TaskID, replacement.Body); got == 0 {
		t.Fatal("replacement remained unread after its own delivery")
	}
}

func TestAdapterReceiptDoesNotConsumeDirectiveSentAfterItsPrompt(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "steer-replacement", defense.TierReversible)
	c.SetSuperviseRounds(1)
	task, err := c.Enqueue(ctx, TaskInput{
		Title: "steer", Intent: "work", Requires: []string{"code:modify"}, Authorized: true,
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := c.InjectUserNote(ctx, task.TaskID, "old directive", nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := &Core{store: NewTaskStore(c.store.db, c.logger), nodeID: c.nodeID}
	var observed bool
	c.router.SetAdapterRunner(func(runCtx context.Context, _, prompt, _ string) commander.AgentResult {
		if !observed {
			observed = true
			if !strings.Contains(prompt, old.Body) || strings.Contains(prompt, "replacement directive") {
				t.Errorf("unexpected first adapter prompt: %q", prompt)
			}
			if err := sender.store.DeleteUserNote(ctx, task.TaskID, old.Sender); err != nil {
				t.Error(err)
				return commander.AgentResult{ExitCode: 1}
			}
			if _, _, err := sender.InjectUserNote(ctx, task.TaskID, "replacement directive", nil); err != nil {
				t.Error(err)
				return commander.AgentResult{ExitCode: 1}
			}
			commander.PromptDelivered(runCtx)
			if got := consumedAt(t, c, task.TaskID, "replacement directive"); got != 0 {
				t.Errorf("first adapter receipt marked an unseen directive consumed_at=%d", got)
			}
		}
		return commander.AgentResult{OK: false, ExitCode: 127, Result: "fixture stopped"}
	})
	if err := c.store.ClaimLocal(ctx, task.TaskID, c.nodeID); err != nil {
		t.Fatal(err)
	}
	c.runScheduled(ctx, task.TaskID)
	if !observed {
		t.Fatal("the task never reached the adapter")
	}
}

// The durable stamp must land when the adapter receives the prompt, not when
// its process exits: an agent can run for minutes, and a note still marked
// pending stays editable in the console while the instruction is executing.
func TestSteerStampedWhenPromptReachesAdapter(t *testing.T) {
	c := newCoreWithTwoAgents(t)
	ctx := context.Background()
	tk, err := c.Enqueue(ctx, TaskInput{
		Title: "steer target", ContextType: "file", Intent: "edit a file",
		Requires: []string{"code:modify"},
	}, DefaultQueueSpec())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := c.InjectUserMessage(ctx, tk.TaskID, "keep the API stable"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	c.router.SetAdapterRunner(func(runCtx context.Context, _, _, _ string) commander.AgentResult {
		// What a real adapter does at the moment it takes the prompt.
		commander.PromptDelivered(runCtx)
		close(entered)
		<-release
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = c.runLocal(ctx, tk, TaskInput{Intent: "edit a file", Requires: []string{"code:modify"}})
	}()
	<-entered
	if got := consumedAt(t, c, tk.TaskID, "keep the API stable"); got == 0 {
		close(release)
		<-done
		t.Fatal("the adapter is running but the steer note is still unconsumed")
	}
	close(release)
	<-done
}

func TestWatchRoundSteerInterruptsForAnotherProcess(t *testing.T) {
	c := queueTestCore(t)
	tk := steerTestTask(t, c)
	roundCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go c.watchRoundSteer(roundCtx, tk.TaskID, msgSeen{}, cancel, done, nil)

	_, status, err := c.InjectUserNote(context.Background(), tk.TaskID, "inspect the screenshot first", nil)
	if err != nil || status != "queued" {
		t.Fatalf("remote inject = %q, %v", status, err)
	}
	select {
	case <-roundCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("running round did not observe the durable inject")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("steer watcher did not stop after cancelling the round")
	}
}
