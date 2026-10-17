// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/core"
)

// TestSendTaskMessageWithoutEngine pins the scheduler-less path: queue mode
// still lands the durable note (the store is enough), inject honestly refuses
// with 503 — a console without the engine can never abort a live round.
func TestSendTaskMessageWithoutEngine(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	task, err := store.Create(ctx, "", "proj", "message target", "node", []string{"node"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Queue(ctx, task.TaskID, "node"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	h := New(Deps{Store: store, StaticDir: t.TempDir(), Token: testToken})

	body := `{"text":"also update the docs"}`
	rr := httptest.NewRecorder()
	req := authedReq(http.MethodPost, "/api/tasks/"+task.TaskID+"/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("queue send: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "queued" || out.TaskID != task.TaskID {
		t.Fatalf("out = %+v, want queued for the task", out)
	}
	msgs, err := store.TaskMessages(ctx, "task:"+task.TaskID)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "also update the docs" || msgs[0].Grade != core.MsgGradeStatus {
		t.Fatalf("board after send: %v msgs=%+v", err, msgs)
	}

	rr = httptest.NewRecorder()
	req = authedReq(http.MethodPost, "/api/tasks/"+task.TaskID+"/messages", strings.NewReader(`{"text":"stop","inject":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("inject without engine: status = %d, want 503", rr.Code)
	}
}

// TestSendTaskMessageGuards pins the input and state guards: empty text is a
// 400, an unknown task a 404, a finished task a 409 (state conflict, not a
// server fault).
func TestSendTaskMessageGuards(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	h := New(Deps{Store: store, StaticDir: t.TempDir(), Token: testToken})

	post := func(path, body string) int {
		rr := httptest.NewRecorder()
		req := authedReq(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := post("/api/tasks/nope/messages", `{"text":"hi"}`); got != http.StatusNotFound {
		t.Fatalf("unknown task: status = %d, want 404", got)
	}

	task, err := store.Create(ctx, "", "proj", "guarded", "node", []string{"node"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := post("/api/tasks/"+task.TaskID+"/messages", `{"text":"   "}`); got != http.StatusBadRequest {
		t.Fatalf("empty text: status = %d, want 400", got)
	}
	if _, err := store.CancelCascade(ctx, task.TaskID); err != nil {
		t.Fatalf("finish task: %v", err)
	}
	if got := post("/api/tasks/"+task.TaskID+"/messages", `{"text":"late"}`); got != http.StatusConflict {
		t.Fatalf("finished task: status = %d, want 409", got)
	}
}

// parkFollowupTask drives a fresh task through the real transitions into the
// exact row a supervised follow-up park leaves: running -> review under
// accept_work with needs_followup inside the stored result.
func parkFollowupTask(t *testing.T, store *core.TaskStore, nodeID, title string, followup bool) string {
	t.Helper()
	ctx := context.Background()
	task, err := store.Create(ctx, "", "proj", title, nodeID, []string{nodeID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Queue(ctx, task.TaskID, nodeID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := store.Dispatch(ctx, task.TaskID, nodeID, nodeID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := store.Accept(ctx, task.TaskID, nodeID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	result := map[string]any{"ok": false, "stderr": "round budget exhausted"}
	if followup {
		result["needs_followup"] = true
	}
	if err := store.PauseWithResult(ctx, task.TaskID, nodeID, result); err != nil {
		t.Fatalf("park: %v", err)
	}
	return task.TaskID
}

// TestContinueTaskEndpoint pins the endpoint's honest gating: a follow-up
// park requeues (200 + queued row + the note landed as a steer message), a
// plain review is a 409, and without an engine the answer is 503 — never a
// silent state change.
func TestContinueTaskEndpoint(t *testing.T) {
	engine := newQueueEngine(t)
	store := engine.TaskStore()
	h := New(Deps{Store: store, Engine: engine, StaticDir: t.TempDir(), Token: testToken})
	ctx := context.Background()
	// The queue engine's stable runtime id (vm kind → name@vm-<identity>):
	// owner and dispatch target must resolve to it or the continue's
	// locality guards (correctly) refuse the requeue.
	nodeID := core.RuntimeNodeID("panel-test", "vm", config.PhysicalIdentity())

	id := parkFollowupTask(t, store, nodeID, "long work", true)
	rr := httptest.NewRecorder()
	req := authedReq(http.MethodPost, "/api/tasks/"+id+"/continue", strings.NewReader(`{"note":"finish part two"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.State != core.StateQueued {
		t.Fatalf("out = %d %+v, want queued", rr.Code, out)
	}
	cur, err := store.Get(ctx, id)
	if err != nil || cur.State != core.StateQueued {
		t.Fatalf("row after continue: %+v %v", cur, err)
	}
	msgs, err := store.TaskMessages(ctx, "task:"+id)
	if err != nil {
		t.Fatalf("board read: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Grade == core.MsgGradeSteer && strings.Contains(m.Body, "finish part two") {
			found = true
		}
	}
	if !found {
		t.Fatalf("operator note must land as a steer message, got %+v", msgs)
	}

	plain := parkFollowupTask(t, store, nodeID, "plain park", false)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq(http.MethodPost, "/api/tasks/"+plain+"/continue", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("non-followup continue: %d, want 409", rr.Code)
	}

	// Without an engine the same parked task is a 503 — the console cannot
	// validate ownership for the requeue.
	h2 := New(Deps{Store: store, StaticDir: t.TempDir(), Token: testToken})
	id2 := parkFollowupTask(t, store, nodeID, "engineless", true)
	rr = httptest.NewRecorder()
	h2.ServeHTTP(rr, authedReq(http.MethodPost, "/api/tasks/"+id2+"/continue", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("engineless continue: %d, want 503", rr.Code)
	}
}
