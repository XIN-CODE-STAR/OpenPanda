// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/askengine"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

// newSessionMsgHandler mounts only the endpoint under test, over a real
// ServeMux (the handler reads r.PathValue, so the pattern has to be routed).
// New() returns the fully-routed console as an http.Handler, which hides the
// in-memory steering queue this suite has to inspect — it has no HTTP read
// side by design, since only the round loop's GetSteer poll drains it.
func newSessionMsgHandler(store *core.TaskStore, eng *askengine.Engine, sess *sessions.Store) (*handler, http.Handler) {
	h := &handler{
		store:      store,
		engine:     eng,
		sessions:   sess,
		cfgMu:      &sync.RWMutex{},
		serviceCtx: context.Background(),
		activeAsks: map[string]sessionOperation{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions/{id}/messages", h.sendSessionMessage)
	return h, mux
}

// decodeSessionMessage reads the endpoint's three-field response.
func decodeSessionMessage(t *testing.T, rr *httptest.ResponseRecorder) (taskID, status, warning string) {
	t.Helper()
	var res map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return res["task_id"], res["status"], res["warning"]
}

// postSessionMessage posts one message into a session.
func postSessionMessage(t *testing.T, h http.Handler, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sessionID+"/messages",
		strings.NewReader(body))
	h.ServeHTTP(rr, req)
	return rr
}

// TestSessionMessageSteersLiveTurn covers the task-less path: an answer turn
// has no board to land a note on, so the text rides the streaming ask's steer
// queue — the same channel the TUI uses. The queue only exists while a turn is
// live, which is why the test registers an active ask first.
func TestSessionMessageSteersLiveTurn(t *testing.T) {
	sessStore := sessions.NewStore(t.TempDir())
	sess, err := sessStore.Create("steer me")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h, mux := newSessionMsgHandler(newTestStore(t), nil, sessStore)

	// A live turn: what sessionAsk registers for the duration of the ask.
	h.askMu.Lock()
	h.activeAsks[sess.ID] = sessionOperation{operationID: "op-1", generation: 1}
	h.askMu.Unlock()

	rr := postSessionMessage(t, mux, sess.ID, `{"text":"  also check the tests  "}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rr.Code, rr.Body.String())
	}
	taskID, status, warning := decodeSessionMessage(t, rr)
	if taskID != "" || status != "queued" || warning != "" {
		t.Fatalf("response = task:%q status:%q warn:%q", taskID, status, warning)
	}
	// The round loop's GetSteer poll is what drains it — assert the text landed
	// where that poll reads, trimmed.
	if got := h.drainSteer(sess.ID); got != "also check the tests" {
		t.Fatalf("steer queue delivered %q", got)
	}
	if got := h.drainSteer(sess.ID); got != "" {
		t.Fatalf("steer queue replayed %q", got)
	}

	// inject on an answer turn cannot abort the model call, and says so instead
	// of letting the console claim an interruption that never happened.
	rr = postSessionMessage(t, mux, sess.ID, `{"text":"stop and redo","inject":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("inject: %d %s", rr.Code, rr.Body.String())
	}
	_, status, warning = decodeSessionMessage(t, rr)
	if status != "queued" || warning == "" {
		t.Fatalf("inject on an answer turn = status:%q warn:%q, want a queued warning", status, warning)
	}
}

// TestSessionMessageWithoutTurnRefuses pins the honest refusal: with no live
// turn there is nothing to steer, and accepting the note would park it in a
// queue nobody will ever drain.
func TestSessionMessageWithoutTurnRefuses(t *testing.T) {
	sessStore := sessions.NewStore(t.TempDir())
	sess, err := sessStore.Create("idle thread")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, mux := newSessionMsgHandler(newTestStore(t), nil, sessStore)
	rr := postSessionMessage(t, mux, sess.ID, `{"text":"hello"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("send with no turn: %d %s", rr.Code, rr.Body.String())
	}
}

// TestSessionMessageTargetsLiveTask covers the durable path: a thread that
// spawned a classified task takes the board note instead of the in-memory
// queue, so the message survives a reload and reaches the executor's round.
// The task is parked in review (a live state the executor will resume from), so
// it cannot race the scheduler to a terminal one mid-test.
func TestSessionMessageTargetsLiveTask(t *testing.T) {
	engine, _ := newApprovalQueueEngine(t)
	store := engine.TaskStore()
	sessStore := sessions.NewStore(t.TempDir())
	h := New(Deps{
		Store: store, Engine: engine, Sessions: sessStore,
		StaticDir: t.TempDir(), Token: testToken,
	})
	_, mux := newSessionMsgHandler(store, engine, sessStore)

	// The board-created task needs the full console (it links a session), so
	// create it through New's routes rather than the one-route mux.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq(http.MethodPost, "/api/tasks", strings.NewReader(
		`{"title":"parked task","prompt":"do it","requires":["test:once"],"authorize":false}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create task: %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.TaskID == "" || created.SessionID == "" {
		t.Fatalf("create result = %+v", created)
	}
	// Wait for the row to park: a settled row would send this test down the
	// steer path and prove nothing about the board.
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := store.Get(context.Background(), created.TaskID)
		if err == nil && task.State == core.StateReview {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not reach review: %+v, %v", task, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The thread's live task is the target — not the steer queue.
	rr = postSessionMessage(t, mux, created.SessionID, `{"text":"note for the task"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send to task: %d %s", rr.Code, rr.Body.String())
	}
	taskID, status, _ := decodeSessionMessage(t, rr)
	if taskID != created.TaskID || status != "queued" {
		t.Fatalf("response = task:%q status:%q, want the linked task", taskID, status)
	}
	msgs, err := store.TaskMessages(context.Background(), "task:"+created.TaskID)
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	var got bool
	for _, m := range msgs {
		if m.Body == "note for the task" && m.SenderLabel == "user" {
			got = true
			// Unread on arrival: the round has not folded it yet, and the
			// console draws "waiting for the next round" off exactly this.
			if m.ConsumedAt != 0 {
				t.Fatalf("fresh note already marked consumed: %+v", m)
			}
		}
	}
	if !got {
		t.Fatalf("board has no user note: %+v", msgs)
	}
}
