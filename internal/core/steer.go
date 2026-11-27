// SPDX-License-Identifier: AGPL-3.0-or-later

package core

// User-message delivery into live tasks — the two send modes users know from
// Claude Code and Codex, mapped onto this kernel's execution model:
//
//	queue  — a durable note on the task's own board scope ("task:<id>"), the
//	         same channel sibling tasks post on. The running agent finishes
//	         its current round and reads the note as context at the next one;
//	         a parked or queued task reads it at its first round. Nothing is
//	         interrupted.
//
//	inject — the same durable note, marked with the steer grade, plus the
//	         abort of the in-flight agent round when one is live. The steer
//	         text LEADS the next round's intent, ahead of the supervisor's
//	         follow-up: the user's words become the round's primary
//	         instruction, not context.
//
// Both modes are board-backed and therefore restart-safe: the note is the
// delivery mechanism, and the round loop's seen-set makes each message land
// exactly once. The only in-memory state is the running round's cancel — the
// handle an inject needs to abort work in flight.
//
// Both modes refuse terminal tasks.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Xustalis/OpenPanda/internal/scheduler"
)

// QueueUserMessage posts a durable advisory note to the task's own board
// scope. The next round (or the first, for a task that has not started)
// folds it into the agent's intent; a finished task refuses.
func (c *Core) QueueUserMessage(ctx context.Context, taskID, text string) error {
	_, err := c.userMessage(ctx, taskID, text, nil, MsgGradeStatus, "queue")
	return err
}

// QueueUserNote is QueueUserMessage carrying files, returning the slot it
// landed so the caller can edit or promote that exact note afterwards.
func (c *Core) QueueUserNote(ctx context.Context, taskID, text string, attachments []string) (TaskMessage, error) {
	return c.userMessage(ctx, taskID, text, attachments, MsgGradeStatus, "queue")
}

// InjectUserMessage posts the steer note AND aborts the running agent round
// when one is live. Returns "interrupted" when a running turn was aborted,
// "queued" when the steer waits for the next round boundary (or the task's
// first run — the note is durable either way).
func (c *Core) InjectUserMessage(ctx context.Context, taskID, text string) (string, error) {
	_, status, err := c.InjectUserNote(ctx, taskID, text, nil)
	return status, err
}

// InjectUserNote is InjectUserMessage carrying files. The slot is returned so
// a surface can keep addressing the note it just sent.
func (c *Core) InjectUserNote(ctx context.Context, taskID, text string, attachments []string) (TaskMessage, string, error) {
	msg, err := c.userMessage(ctx, taskID, text, attachments, MsgGradeSteer, "inject")
	if err != nil {
		return TaskMessage{}, "", err
	}
	return msg, c.interrupt(taskID), nil
}

// PromoteUserNote turns an already-queued note into an inject without posting
// a second row: the same slot is re-written with the steer grade, so the next
// round carries the user's words exactly once, as a directive instead of
// context. Returns "interrupted" when a live round was aborted here,
// "queued" when the steer waits for the round boundary.
//
// This is the second-Enter escalation: a surface that queued a note on the
// first Enter must not send a duplicate on the second.
func (c *Core) PromoteUserNote(ctx context.Context, taskID, sender string) (string, error) {
	if c == nil || c.store == nil {
		return "", fmt.Errorf("task messages require a task store")
	}
	msg, err := c.store.rewriteUserNote(ctx, taskID, sender, c.nodeID, func(msg *TaskMessage) {
		msg.Grade = MsgGradeSteer
	})
	if err != nil {
		return "", err
	}
	c.EvTrace(ctx, taskID, EvUserMsg, map[string]any{"mode": "inject", "text": msg.Body})
	return c.interrupt(taskID), nil
}

// interrupt aborts the round this process is running for the task, when one
// is live, and reports which promise the caller may make: "interrupted" or
// "queued". The cancel handle is process-local (see registerInterrupt), so a
// task executing in another process always answers "queued" — which is the
// truth, not a downgrade.
func (c *Core) interrupt(taskID string) string {
	c.steerMu.Lock()
	cancel := c.interrupts[taskID]
	c.steerMu.Unlock()
	if cancel == nil {
		return "queued"
	}
	cancel()
	return "interrupted"
}

// userMessage is the shared body: land the durable board note (the store
// applies the state and owner guards) and trace the event.
func (c *Core) userMessage(ctx context.Context, taskID, text string, attachments []string, grade, mode string) (TaskMessage, error) {
	if c == nil || c.store == nil {
		return TaskMessage{}, fmt.Errorf("task messages require a task store")
	}
	msg, err := c.store.PostUserNote(ctx, taskID, text, attachments, grade, c.nodeID)
	if err != nil {
		return TaskMessage{}, err
	}
	c.EvTrace(ctx, taskID, EvUserMsg, map[string]any{"mode": mode, "text": msg.Body, "seq": msg.SenderSeq})
	return msg, nil
}

// PostUserMessage lands a user message on the task's own board scope. The
// state guard refuses terminal tasks. Each user note has its own slot.
// This is also the scheduler-less delivery path: a note needs the store,
// not the execution core.
//
// localNodeID is the runtime identity of the node this store belongs to; it
// backs the owner guard — a task dispatched to another node reads ITS
// store's board, not ours, so a note landed here would never reach the
// agent. The guard refuses honestly instead of accepting a message nobody
// will read. The comparison is runtime identity, not literal equality: a
// CLI engine's ephemeral id and the daemon's base id resolve to the same
// store-owning node. An empty localNodeID skips the check (callers that
// cannot name the store's owner degrade to the state guard alone).
func (s *TaskStore) PostUserMessage(ctx context.Context, taskID, text, grade, localNodeID string) error {
	_, err := s.PostUserNote(ctx, taskID, text, nil, grade, localNodeID)
	return err
}

// PostUserNote is PostUserMessage returning the slot it landed, so a sender
// can address that exact note afterwards (edit / delete / promote). The
// returned Sender is the row's board id (`user:<seq>`), not "user".
func (s *TaskStore) PostUserNote(ctx context.Context, taskID, text string, attachments []string, grade, localNodeID string) (TaskMessage, error) {
	if s == nil {
		return TaskMessage{}, fmt.Errorf("task messages require a task store")
	}
	body := noteBody(text, attachments)
	if body == "" {
		return TaskMessage{}, fmt.Errorf("message text must not be empty")
	}
	var msg TaskMessage
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		t, err := s.scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=?`, taskID))
		if err != nil {
			return err
		}
		if terminalState(t.State) {
			return fmt.Errorf("task %s is %s — messages reach running or queued work only", taskID, t.State)
		}
		if localNodeID != "" && t.OwnerNode != "" &&
			!scheduler.SameRuntimeIdentity(t.OwnerNode, localNodeID) {
			return fmt.Errorf("task %s runs on %s — messages deliver to the executor's own store", taskID, t.OwnerNode)
		}
		msg, err = postUserNoteTx(ctx, tx, taskID, body, attachments, grade)
		return err
	})
	if err != nil {
		return TaskMessage{}, err
	}
	return msg, nil
}

// postUserNoteTx assigns a board slot under the same write transaction that
// inserts the note. Keep the high-water mark after deletion so an in-flight
// delivery receipt can never match a different note.
func postUserNoteTx(ctx context.Context, tx *sql.Tx, taskID, body string, attachments []string, grade string) (TaskMessage, error) {
	// The board's slot key is (scope, sender, rcpt) with newest-wins per
	// slot — right for status notes, wrong for user messages, where every
	// send must survive. A per-message sender id (`user:<seq>`) gives each
	// its own slot; SenderLabel keeps the rendered line reading "user". The
	// high-water mark also accounts for notes written before this counter
	// existed, including edits which bump an individual slot's sender_seq.
	scope := "task:" + taskID
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE tasks SET user_note_seq = MAX(user_note_seq, (
			SELECT COALESCE(MAX(sender_seq), 0) FROM task_messages
			WHERE scope=? AND (sender=? OR sender LIKE ?)
		)) + 1 WHERE task_id=? RETURNING user_note_seq`,
		scope, "user", userSenderPrefix+"%", taskID).Scan(&seq); err != nil {
		return TaskMessage{}, fmt.Errorf("user message seq: %w", err)
	}
	msg := TaskMessage{
		Scope:       scope,
		Sender:      fmt.Sprintf("%s%d", userSenderPrefix, seq),
		SenderLabel: "user",
		Rcpt:        taskID,
		SenderSeq:   seq,
		Grade:       grade,
		Body:        body,
		Attachments: attachments,
		// SentAt went unset once and every user note rendered as "~57 years
		// ago" on `panda task board` — the store writes m.SentAt verbatim.
		SentAt: time.Now().Unix(),
	}
	if err := putTaskMessage(ctx, tx, msg); err != nil {
		return TaskMessage{}, fmt.Errorf("post user message: %w", err)
	}
	return msg, nil
}

// noteBody is the text the agent reads: the user's prose plus one `@path`
// line per attachment, in the same shape the web composer's attachment block
// uses. Keeping the list in the body (as well as the Attachments column) is
// what makes the note self-contained — the round folds bodies, never chips.
func noteBody(text string, attachments []string) string {
	text = strings.TrimSpace(text)
	kept := make([]string, 0, len(attachments))
	for _, a := range attachments {
		if a = strings.TrimSpace(a); a != "" {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	b.WriteString("Attached files (read them from the workspace):\n")
	for _, a := range kept {
		fmt.Fprintf(&b, "- @%s\n", a)
	}
	return strings.TrimRight(b.String(), "\n")
}

// userNote loads one live user slot. A row that expired with its board, or
// that names an agent sender, is "no such note" — editing another sender's
// line is not a user affordance.
func (s *TaskStore) userNote(ctx context.Context, taskID, sender string) (TaskMessage, error) {
	if s == nil {
		return TaskMessage{}, fmt.Errorf("task messages require a task store")
	}
	if !isUserSender(sender) || sender == "user" {
		return TaskMessage{}, fmt.Errorf("no such note %q", sender)
	}
	msgs, err := s.TaskMessages(ctx, "task:"+taskID)
	if err != nil {
		return TaskMessage{}, err
	}
	for _, m := range msgs {
		if m.Sender == sender {
			return m, nil
		}
	}
	return TaskMessage{}, fmt.Errorf("no such note %q", sender)
}

// UserNotes lists the user's unconsumed notes on a task board, oldest first —
// what a console draws as the pending strip above its composer. Consumed
// notes are excluded: the round already folded them into a prompt, so there
// is nothing left to edit, drop, or promote.
func (s *TaskStore) UserNotes(ctx context.Context, taskID string) ([]TaskMessage, error) {
	msgs, err := s.TaskMessages(ctx, "task:"+taskID)
	if err != nil {
		return nil, err
	}
	out := make([]TaskMessage, 0, len(msgs))
	for _, m := range msgs {
		if isUserSender(m.Sender) && m.ConsumedAt == 0 && m.Rcpt == taskID {
			out = append(out, m)
		}
	}
	return out, nil
}

// EditUserNote rewrites a pending note in place, keeping its slot and grade.
// The seq is bumped so the store's stale-write guard accepts the update, and
// SentAt moves to now so the edited note re-sorts to the tail of the board —
// an edit is the user saying "this is what I mean now".
//
// A note the round already read is refused: rewriting history would leave the
// agent's prompt and the board disagreeing about what was said.
func (s *TaskStore) EditUserNote(ctx context.Context, taskID, sender, text string, attachments []string, localNodeID string) (TaskMessage, error) {
	body := noteBody(text, attachments)
	if body == "" {
		return TaskMessage{}, fmt.Errorf("message text must not be empty")
	}
	return s.rewriteUserNote(ctx, taskID, sender, localNodeID, func(msg *TaskMessage) {
		msg.Body = body
		msg.Attachments = attachments
	})
}

func (s *TaskStore) editableUserNote(ctx context.Context, tx *sql.Tx, taskID, sender string) (TaskMessage, error) {
	msg := TaskMessage{Scope: "task:" + taskID, Sender: sender, SenderLabel: "user", Rcpt: taskID}
	if !isUserSender(sender) || sender == "user" {
		return msg, fmt.Errorf("no such note %q", sender)
	}
	var attached sql.NullString
	var sending bool
	err := tx.QueryRowContext(ctx, `SELECT sender_seq,grade,body,attachments,consumed_at,
		handoff_id<>'' AND EXISTS (SELECT 1 FROM tasks WHERE task_id=?
			AND substr(task_messages.handoff_id,1,length(attempt_id)+1)=attempt_id||'/'
			AND state IN ('dispatched','running','waiting_context'))
		FROM task_messages WHERE scope=? AND sender=? AND rcpt=? AND expires_at>?`,
		taskID, msg.Scope, sender, taskID, time.Now().Unix()).
		Scan(&msg.SenderSeq, &msg.Grade, &msg.Body, &attached, &msg.ConsumedAt, &sending)
	if errors.Is(err, sql.ErrNoRows) {
		return msg, fmt.Errorf("no such note %q", sender)
	}
	if err != nil {
		return msg, err
	}
	if msg.ConsumedAt != 0 {
		return msg, fmt.Errorf("note %q was already read by the round — send a new one", sender)
	}
	if sending {
		return msg, fmt.Errorf("%w: note %q is being delivered", ErrConflict, sender)
	}
	msg.Attachments = decodeAttachments(attached.String)
	return msg, nil
}

func (s *TaskStore) rewriteUserNote(ctx context.Context, taskID, sender, localNodeID string, rewrite func(*TaskMessage)) (TaskMessage, error) {
	var msg TaskMessage
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		task, err := s.scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=?`, taskID))
		if err != nil {
			return err
		}
		if terminalState(task.State) {
			return fmt.Errorf("task %s is %s — messages reach running or queued work only", taskID, task.State)
		}
		if localNodeID != "" && task.OwnerNode != "" && !scheduler.SameRuntimeIdentity(task.OwnerNode, localNodeID) {
			return fmt.Errorf("task %s runs on %s — messages deliver to the executor's own store", taskID, task.OwnerNode)
		}
		msg, err = s.editableUserNote(ctx, tx, taskID, sender)
		if err != nil {
			return err
		}
		rewrite(&msg)
		msg.SenderSeq++
		msg.SentAt = time.Now().Unix()
		return putTaskMessage(ctx, tx, msg)
	})
	return msg, err
}

// DeleteUserNote drops a pending note from the board. Guarded by the same
// "already read" rule as Edit: a note the round consumed is part of the
// transcript's history, and removing the row would only make the board lie.
func (s *TaskStore) DeleteUserNote(ctx context.Context, taskID, sender string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		note, err := s.editableUserNote(ctx, tx, taskID, sender)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM task_messages WHERE scope=? AND sender=? AND rcpt=?`,
			note.Scope, note.Sender, note.Rcpt)
		return err
	})
}

// errPromptNotesChanged asks the local round loop to rebuild before it sends.
var errPromptNotesChanged = errors.New("pending user notes changed before prompt delivery")

func (s *TaskStore) reservePromptNotes(ctx context.Context, taskID, attemptID string, notes []TaskMessage) (string, error) {
	nonce, err := newUUID()
	if err != nil {
		return "", err
	}
	// A recovered run can keep the same task attempt. Receipt and cleanup
	// still belong to one specific prompt, not every handoff in that attempt.
	handoffID := attemptID + "/" + nonce
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var current bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM tasks WHERE task_id=? AND attempt_id=? AND state='running')`,
			taskID, attemptID).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrCancelled
		}
		for _, note := range notes {
			res, err := tx.ExecContext(ctx, `UPDATE task_messages SET handoff_id=?
				WHERE scope=? AND sender=? AND rcpt=? AND sender_seq=? AND consumed_at=0 AND expires_at>?`,
				handoffID, note.Scope, note.Sender, note.Rcpt, note.SenderSeq, time.Now().Unix())
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errPromptNotesChanged
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return handoffID, nil
}

func (s *TaskStore) releasePromptNotes(ctx context.Context, taskID, handoffID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_messages SET handoff_id=''
		WHERE scope=? AND rcpt=? AND handoff_id=?`, "task:"+taskID, taskID, handoffID)
	return err
}

// userSenderPrefix marks the sender ids of user messages (steer.go): each
// message gets its own slot via `user:<seq>`, and the prefix is what the
// round loop's steer/drain matching keys on.
const userSenderPrefix = "user:"

// isUserSender reports whether a board sender id is a user message.
func isUserSender(sender string) bool {
	return sender == "user" || strings.HasPrefix(sender, userSenderPrefix)
}

// takeSteer consumes the pending steer note, if any — called once at the top
// of each round so the directive leads that round's intent exactly once. The
// seen-set is the round loop's board cursor: a note the loop already folded
// (or delivered as a directive) never re-fires.
func (c *Core) takeSteer(ctx context.Context, taskID string, seen msgSeen) string {
	body, _ := c.takeRoundSteer(ctx, taskID, seen)
	return body
}

// steerNote is one message the round is about to fold into its prompt. The
// delivery stamp is deliberately separate from the read: peeking marks the
// in-memory cursor (so this round folds the note once), while the durable
// consumed_at and the audit event wait until the executor has actually been
// handed the prompt.
type steerNote struct {
	msg          TaskMessage
	slot         string
	body         string
	label        string
	planDecision string
}

func (c *Core) takeRoundSteer(ctx context.Context, taskID string, seen msgSeen) (string, string) {
	note := c.peekRoundSteer(ctx, taskID, seen)
	if note == nil {
		return "", ""
	}
	c.markSteerDelivered(ctx, taskID, note, "")
	return note.body, note.label
}

// peekRoundSteer selects the next pending steer note and advances the round
// cursor, without recording delivery. The caller stamps it once the prompt
// carrying it has left for the adapter (see markSteerDelivered); a restart in
// that window must still find the note, especially a plan adjustment, whose
// pending test is consumed_at==0.
func (c *Core) peekRoundSteer(ctx context.Context, taskID string, seen msgSeen) *steerNote {
	if notes := c.refreshRoundSteers(ctx, taskID, seen, nil); len(notes) > 0 {
		return notes[0]
	}
	return nil
}

// refreshRoundSteers keeps the still-current directives from aborted attempts
// and prepends the next unseen directive. An edit or deletion can invalidate
// a snapshot while it is waiting for the adapter's delivery receipt.
func (c *Core) refreshRoundSteers(ctx context.Context, taskID string, seen msgSeen, pending []*steerNote) []*steerNote {
	if c == nil || c.store == nil {
		return pending
	}
	msgs, err := c.store.TaskMessages(ctx, "task:"+taskID)
	if err != nil {
		return pending
	}
	if len(pending) > 0 {
		current := make(map[string]TaskMessage, len(msgs))
		for _, m := range msgs {
			if m.Grade == MsgGradeSteer && m.ConsumedAt == 0 {
				current[msgSlotKey(m)] = m
			}
		}
		pending = slices.DeleteFunc(pending, func(note *steerNote) bool {
			_, exists := current[note.slot]
			return !exists
		})
		for i, note := range pending {
			if m := current[note.slot]; m.SenderSeq > note.msg.SenderSeq {
				pending[i] = newSteerNote(m)
				seen[note.slot] = m.SenderSeq
			}
		}
	}
	for _, m := range msgs {
		if pendingRoundSteer(m, taskID, seen) {
			next := newSteerNote(m)
			seen[next.slot] = next.msg.SenderSeq
			return slices.Insert(pending, 0, next)
		}
	}
	return pending
}

func newSteerNote(m TaskMessage) *steerNote {
	note := &steerNote{
		msg: m, slot: msgSlotKey(m),
		body: strings.TrimSpace(m.Body), label: "[user directive]",
	}
	if strings.HasPrefix(m.Sender, planSenderPrefix) {
		note.label = "[Panda plan adjustment]"
		note.planDecision = strings.TrimPrefix(m.Sender, planSenderPrefix)
	}
	return note
}

// markSteerDelivered records that the executor received the note: the row is
// stamped consumed (the console's "read at round N" badge) and the audit event
// lands. Called after the prompt was handed to the adapter, never at drain
// time — a crash before the hand-off must leave the note deliverable.
func (c *Core) markSteerDelivered(ctx context.Context, taskID string, note *steerNote, handoffID string) {
	if c == nil || c.store == nil || note == nil {
		return
	}
	if note.planDecision != "" {
		if c.markPromptNoteDelivered(ctx, taskID, note.msg, "") {
			c.EvTrace(ctx, taskID, EvPlanAdjustmentRead, map[string]any{"decision_id": note.planDecision, "instruction": note.body})
		}
		return
	}
	c.markPromptNoteDelivered(ctx, taskID, note.msg, handoffID)
}

func (c *Core) markPromptNoteDelivered(ctx context.Context, taskID string, note TaskMessage, handoffID string) bool {
	updated, err := c.store.markTaskMessageConsumed(ctx, note.Scope, note.Sender, note.SenderSeq, handoffID)
	if err != nil && c.logger != nil {
		c.logger.Debug("stamp note delivery", "task", taskID, "err", err)
	}
	if updated && isUserSender(note.Sender) {
		mode := "queue"
		if note.Grade == MsgGradeSteer {
			mode = "inject"
		}
		c.EvTrace(ctx, taskID, EvUserMsgConsumed, map[string]any{"mode": mode, "seq": note.SenderSeq, "text": note.Body})
	}
	return updated
}

func (c *Core) pendingQueuedNotes(ctx context.Context, taskID string) []TaskMessage {
	notes, err := c.store.UserNotes(ctx, taskID)
	if err != nil {
		c.logger.Debug("read queued user notes", "task", taskID, "err", err)
		return nil
	}
	return slices.DeleteFunc(notes, func(note TaskMessage) bool { return note.Grade == MsgGradeSteer })
}

func (c *Core) promptSteerWithdrawn(ctx context.Context, taskID string, picked []TaskMessage) bool {
	var current []TaskMessage
	loaded := false
	for _, note := range picked {
		if note.Grade != MsgGradeSteer {
			continue
		}
		if !loaded {
			var err error
			current, err = c.store.TaskMessages(ctx, "task:"+taskID)
			if err != nil {
				return false
			}
			loaded = true
		}
		if !slices.ContainsFunc(current, func(m TaskMessage) bool {
			return m.Sender == note.Sender && m.Rcpt == note.Rcpt && m.SenderSeq == note.SenderSeq
		}) {
			return true
		}
	}
	return false
}

func pendingRoundSteer(m TaskMessage, taskID string, seen msgSeen) bool {
	if m.Grade != MsgGradeSteer || m.ConsumedAt != 0 || seen[msgSlotKey(m)] >= m.SenderSeq {
		return false
	}
	return isUserSender(m.Sender) || (m.Rcpt == taskID && strings.HasPrefix(m.Sender, planSenderPrefix) && m.ConsumedAt == 0)
}

// steerUnseen reports whether an undelivered steer note is waiting — the
// round loop's interrupt and done-verdict guards key on this so the message
// is never closed over unread.
func (c *Core) steerUnseen(ctx context.Context, taskID string, seen msgSeen) bool {
	if c == nil || c.store == nil {
		return false
	}
	msgs, err := c.store.TaskMessages(ctx, "task:"+taskID)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if pendingRoundSteer(m, taskID, seen) {
			return true
		}
	}
	return false
}

// watchRoundSteer wakes the executor when another local process posts a directive.
func (c *Core) watchRoundSteer(ctx context.Context, taskID string, seen msgSeen, cancel context.CancelFunc, done chan<- struct{}, changed func() bool) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if (changed != nil && changed()) || c.steerUnseen(ctx, taskID, seen) {
				cancel()
				return
			}
		}
	}
}

// registerInterrupt publishes the running round's cancel so an inject can
// abort the in-flight agent turn. Paired with clearInterrupt on every exit
// path of the round.
func (c *Core) registerInterrupt(taskID string, cancel context.CancelFunc) {
	c.steerMu.Lock()
	if c.interrupts == nil {
		c.interrupts = map[string]context.CancelFunc{}
	}
	c.interrupts[taskID] = cancel
	c.steerMu.Unlock()
}

func (c *Core) clearInterrupt(taskID string) {
	c.steerMu.Lock()
	delete(c.interrupts, taskID)
	c.steerMu.Unlock()
}

// terminalState reports whether a task state ends the row's life.
func terminalState(state string) bool {
	switch state {
	case StateDone, StateFailed, StateCancelled, StateExpired:
		return true
	}
	return false
}
