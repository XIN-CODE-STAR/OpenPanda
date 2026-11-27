// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/conversation"
	"github.com/Xustalis/OpenPanda/internal/sessions"
	"github.com/Xustalis/OpenPanda/internal/storage"
)

func TestSessionAskPlanModePersistsUntilExplicitExit(t *testing.T) {
	engine, _ := newApprovalQueueEngine(t)
	var mu sync.Mutex
	var planned []bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		planned = append(planned, strings.Contains(string(body), "Request Mode: PLAN"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Which project should the plan cover?\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	if err := engine.SetModel(config.ModelConfig{APIType: config.APITypeOpenAI, BaseURL: model.URL, Model: "plan-test", NoAuth: true}); err != nil {
		t.Fatal(err)
	}
	store := sessions.NewStore(t.TempDir())
	sess, err := store.Create("plan mode")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPermissionMode(sess.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: engine.TaskStore(), engine: engine, sessions: store, cfgMu: &sync.RWMutex{}, serviceCtx: context.Background(), activeAsks: map[string]sessionOperation{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions/{id}/ask", h.sessionAsk)
	for index, mode := range []string{"plan", "", ""} {
		if index == 2 {
			if err := engine.Conversations(store).SetPlanning(context.Background(), sess.ID, false); err != nil {
				t.Fatal(err)
			}
		}
		rr := httptest.NewRecorder()
		body := fmt.Sprintf(`{"prompt":"Prepare a staged project roadmap; ask which project first.","mode":%q}`, mode)
		mux.ServeHTTP(rr, authedReq(http.MethodPost, "/api/sessions/"+sess.ID+"/ask", strings.NewReader(body)))
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "event: error") {
			t.Fatalf("mode %q: %d %s", mode, rr.Code, rr.Body.String())
		}
		mu.Lock()
		if len(planned) == 0 || planned[len(planned)-1] != (index < 2) {
			t.Errorf("mode %q: model planning lenses %v", mode, planned)
		}
		mu.Unlock()
	}
	current, err := store.Get(sess.ID)
	if err != nil || current.PermissionMode != "manual" {
		t.Fatalf("planning changed the stored permission: %+v, %v", current, err)
	}
}

func TestSessionAskRejectsUnknownRequestMode(t *testing.T) {
	h := &handler{}
	rr := httptest.NewRecorder()
	h.sessionAsk(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"Make a plan","mode":"plna"}`)))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown request mode") {
		t.Fatalf("unknown mode: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSessionAskRoutesReplyToNamedGoalQuestion(t *testing.T) {
	engine, counter := newApprovalQueueEngine(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"The directory is recorded.\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	if err := engine.SetModel(config.ModelConfig{APIType: config.APITypeOpenAI, BaseURL: model.URL, Model: "reply-test", NoAuth: true}); err != nil {
		t.Fatal(err)
	}
	st := sessions.NewStore(t.TempDir())
	sess, err := st.Create("two pending questions")
	if err != nil {
		t.Fatal(err)
	}
	service := engine.Conversations(st)
	service.Close()
	db, err := storage.Open(filepath.Join(filepath.Dir(counter), "panda.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	controls := &conversation.Store{DB: db}
	ctx := context.Background()
	goal, err := controls.Create(ctx, sess.ID, "Check the fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal.RoundID, err = controls.Claim(ctx, goal, "test", "en")
	if err != nil {
		t.Fatal(err)
	}
	if err = controls.PublishGoalQuestion(ctx, goal, nil); err != nil {
		t.Fatal(err)
	}
	tasks := engine.TaskStore()
	if err = tasks.CloseAskRound(ctx, goal.RoundID, "closed"); err != nil {
		t.Fatal(err)
	}
	if err = controls.Settle(ctx, goal, 0); err != nil {
		t.Fatal(err)
	}
	const planRound = "other-question"
	if err = tasks.BeginAskRound(ctx, planRound, sess.ID, "en"); err != nil {
		t.Fatal(err)
	}
	if err = tasks.CloseAskRound(ctx, planRound, "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO conversation_plan_runs(session_id,version,proposal,round_id,status) VALUES(?,1,'Other fixture',?,'waiting_input')", sess.ID, planRound); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE ask_rounds SET closed_at=closed_at+1 WHERE round_id=?", planRound); err != nil {
		t.Fatal(err)
	}
	api := New(Deps{Store: tasks, Engine: engine, Sessions: st, DB: db, Token: testToken, StaticDir: t.TempDir()})
	rr := httptest.NewRecorder()
	body := fmt.Sprintf(`{"prompt":"Use the named test directory.","reply_to":%q}`, goal.RoundID)
	api.ServeHTTP(rr, authedReq(http.MethodPost, "/api/sessions/"+sess.ID+"/ask", strings.NewReader(body)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "The directory is recorded.") || strings.Contains(rr.Body.String(), "event: error") {
		t.Fatalf("reply: %d %s", rr.Code, rr.Body.String())
	}
	state, err := service.State(ctx, sess.ID)
	if err != nil || state.Goal.WaitingFor != "" {
		t.Fatalf("goal question was not resolved: %+v %v", state.Goal, err)
	}
	var status string
	if err = db.QueryRow("SELECT status FROM conversation_plan_runs WHERE round_id=?", planRound).Scan(&status); err != nil || status != "waiting_input" {
		t.Fatalf("reply touched a separate question: %q %v", status, err)
	}

	before, err := st.Get(sess.ID)
	if err != nil || before.Operation == nil {
		t.Fatalf("read accepted operation: %+v %v", before, err)
	}
	h := &handler{store: tasks, engine: engine, sessions: st, cfgMu: &sync.RWMutex{}, serviceCtx: ctx, activeAsks: map[string]sessionOperation{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions/{id}/ask", h.sessionAsk)
	h.carryNotes(sess.ID, []pendingNote{{key: "mem:1", text: "A separate parked note."}})
	rr = httptest.NewRecorder()
	body = fmt.Sprintf(`{"prompt":"A stale reply","reply_to":%q}`, goal.RoundID)
	mux.ServeHTTP(rr, authedReq(http.MethodPost, "/api/sessions/"+sess.ID+"/ask", strings.NewReader(body)))
	if rr.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("stale clarification reached the model or streamed success: http=%d calls=%d %s", rr.Code, calls.Load(), rr.Body.String())
	}
	after, err := st.Get(sess.ID)
	if err != nil || len(after.Turns) != len(before.Turns) || after.Operation == nil || after.Operation.ID != before.Operation.ID {
		t.Fatalf("stale clarification changed the stored operation or transcript: before=%+v after=%+v err=%v", before, after, err)
	}
	if !h.hasCarry(sess.ID) {
		t.Fatal("stale clarification consumed the parked note")
	}
}
