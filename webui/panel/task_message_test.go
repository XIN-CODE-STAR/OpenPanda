// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
