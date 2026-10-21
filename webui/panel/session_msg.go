// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

// User messages into a live session turn — the console's half of the
// queue/inject pair, so the chat composer stops swallowing what the user types
// while the agent runs.
//
// Two delivery paths, picked by what the thread actually is:
//
//	task-backed (a classified task is moving) — the durable board note
//	   (askengine.SendTaskMessage): survives a page reload, reaches the
//	   executor's round whether or not this browser stays open, and carries the
//	   same queue-vs-inject semantics the task detail page and the CLI use.
//
//	plain answer turn — there is no task row to land a note on, so the text
//	   rides the streaming ask's steer channel (askengine.StreamCallbacks
//	   .GetSteer), which the round loop folds into its next prompt. This is
//	   what the TUI has always done; the console simply had no channel.
//
// The distinction is not cosmetic: only the board path can interrupt a round,
// because the cancel handle is process-local (core/steer.go). An answer turn
// therefore reports "queued" even when the caller asked to inject, and the
// response says so instead of letting the user believe the model was cut off.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/i18n"
)

// steerCap bounds one session's steering queue. The same bound the TUI's
// channel uses: a user typing additions faster than the round loop runs is the
// only way to reach it, and refusing loudly beats growing memory behind a
// spinner.
const steerCap = 32

// steerQueue returns the session's queue, creating it on first use. Callers
// must not hold steerMu.
func (h *handler) steerQueue(id string) chan string {
	h.steerMu.Lock()
	defer h.steerMu.Unlock()
	if h.steers == nil {
		h.steers = map[string]chan string{}
	}
	q, ok := h.steers[id]
	if !ok {
		q = make(chan string, steerCap)
		h.steers[id] = q
	}
	return q
}

// pushSteer lands one note on the queue and reports whether it fit. A closed or
// full queue must stay visible to the caller: claiming success on a note no
// round will read is the failure mode this whole path exists to remove.
func (h *handler) pushSteer(id, text string) bool {
	select {
	case h.steerQueue(id) <- text:
		return true
	default:
		return false
	}
}

// drainSteer pops one note for the round loop's GetSteer poll. It deliberately
// does NOT create a queue: GetSteer runs on every round of every turn, and a
// read that allocated would leave a channel behind for each turn the console
// ever served.
func (h *handler) drainSteer(id string) string {
	h.steerMu.Lock()
	q := h.steers[id]
	h.steerMu.Unlock()
	if q == nil {
		return ""
	}
	select {
	case text := <-q:
		return text
	default:
		return ""
	}
}

// dropSteer releases the session's queue when its ask ends. Notes still sitting
// in it are dropped: their turn is over, and the transcript already persisted
// the text as part of the user's side (see sessions.AppendTurn), so re-delivering
// them to the next turn would read as a duplicate.
func (h *handler) dropSteer(id string) {
	h.steerMu.Lock()
	delete(h.steers, id)
	h.steerMu.Unlock()
}

// sessionStreaming reports whether a turn is live for this session — the only
// state in which a steer note can reach anything at all.
func (h *handler) sessionStreaming(id string) bool {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	_, ok := h.activeAsks[id]
	return ok
}

// sendSessionMessage serves POST /api/sessions/{id}/messages: type into the
// thread while the agent is working. Body {text, inject}; the response mirrors
// the task endpoint — {task_id, status, warning} — so both surfaces phrase the
// outcome from the same three fields.
func (h *handler) sendSessionMessage(w http.ResponseWriter, r *http.Request) {
	sessID := r.PathValue("id")
	if h.sessions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sessions are not configured"))
		return
	}
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, errors.New("text must not be empty"))
		return
	}
	if _, err := h.sessions.Get(sessID); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("no such session"))
		return
	}

	// A live task linked to this thread takes the durable path first. Newest
	// row wins (ListBySession is created_at DESC) so a thread that spawned a
	// second task after the first settled still targets the current one.
	if eng := h.currentEngine(); eng != nil && h.store != nil {
		if tasks, err := h.store.ListBySession(r.Context(), sessID); err == nil {
			for _, t := range tasks {
				if taskSettled(t.State) {
					continue
				}
				status, warning, serr := eng.SendTaskMessage(r.Context(), t.TaskID, req.Text, req.Inject)
				if serr != nil {
					writeTaskMessageErr(w, serr)
					return
				}
				writeJSON(w, map[string]string{
					"task_id": t.TaskID, "status": status, "warning": warning,
				})
				return
			}
		}
	}

	// No task row: a plain answer turn. Its only live channel is the steer
	// queue, and it exists only while the turn streams.
	if !h.sessionStreaming(sessID) {
		writeErr(w, http.StatusConflict,
			errors.New("no turn is running in this session — send it as a new prompt"))
		return
	}
	if !h.pushSteer(sessID, req.Text) {
		writeErr(w, http.StatusTooManyRequests,
			errors.New("steering queue is full — wait for the next round"))
		return
	}
	warning := ""
	if req.Inject {
		warning = i18n.T(h.locale(), "web.session.injectNoAbort")
	}
	writeJSON(w, map[string]string{"task_id": "", "status": "queued", "warning": warning})
}

// locale resolves the console's language for the rare response that carries
// prose rather than a structured field. Same source and fallback the task
// endpoints use (see sendTaskMessage's no-consumer warning).
func (h *handler) locale() i18n.Locale {
	loc := i18n.Locale("")
	h.readCfg(func(c *config.Config) { loc = i18n.Parse(c.UI.Locale) })
	return loc
}

// taskSettled reports whether a state ends the row's life — the same set the
// detail page uses to hide its send box.
func taskSettled(state string) bool {
	switch state {
	case core.StateDone, core.StateFailed, core.StateCancelled, core.StateExpired:
		return true
	}
	return false
}
