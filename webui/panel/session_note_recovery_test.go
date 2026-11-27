// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/storage"
)

func TestSessionQueuedResumeRequiresIdleAndConsumesOnce(t *testing.T) {
	f := newSummaryDeliveryFixture(t, false)
	f.h.engine, _ = newApprovalQueueEngine(t)
	f.h.store = f.h.engine.TaskStore()
	f.h.cfgMu = &sync.RWMutex{}
	ctx := context.Background()
	note, ok := f.h.pushSteer(f.sessionID, "canonical queued text", false)
	if !ok {
		t.Fatal("note not queued")
	}
	ask := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"stale client preview","resume_queued":true}`))
		r.SetPathValue("id", f.sessionID)
		w := httptest.NewRecorder()
		f.h.sessionAsk(w, r)
		return w
	}
	wantConflict := func() {
		t.Helper()
		if w := ask(); w.Code != http.StatusConflict {
			t.Fatalf("automatic resume = %d, want conflict: %s", w.Code, w.Body.String())
		}
	}

	oldCtx, oldCancel := context.WithCancel(ctx)
	defer oldCancel()
	op, err := f.h.registerSessionAsk(f.sessionID, oldCancel)
	if err != nil {
		t.Fatal(err)
	}
	wantConflict()
	if oldCtx.Err() != nil || !f.h.hasActiveAsk(f.sessionID) {
		t.Fatal("automatic resume canceled or replaced the live ask")
	}
	f.h.unregisterSessionAsk(f.sessionID, op)

	// The operation map is per handler. A main round published by another
	// handler has the same durable exclusion, even before the session write.
	if err := f.h.store.BeginAskRound(ctx, "other-panel-main", f.sessionID, "en"); err != nil {
		t.Fatal(err)
	}
	wantConflict()
	if got := f.h.listSteer(f.sessionID); len(got) != 1 || got[0] != note {
		t.Fatalf("failed admission changed the queue: %+v", got)
	}
	f.assertTurns(t, 0)
	if err := f.h.store.CloseAskRound(ctx, "other-panel-main", "closed"); err != nil {
		t.Fatal(err)
	}

	if err := f.h.store.BeginSideAskRound(ctx, "parallel-side", f.sessionID, "en"); err != nil {
		t.Fatal(err)
	}
	if w := ask(); w.Code != http.StatusOK {
		t.Fatalf("side round blocked main continuation: %d %s", w.Code, w.Body.String())
	}
	wantConflict()
	sess, err := f.h.sessions.Get(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, turn := range sess.Turns {
		if turn.Role == "user" {
			users++
			if turn.Text != note.text {
				t.Errorf("persisted %q, want queued text %q", turn.Text, note.text)
			}
		}
	}
	side, err := f.h.store.AskRoundProgress(ctx, f.sessionID, "parallel-side")
	if users != 1 || len(f.h.listSteer(f.sessionID)) != 0 || err != nil || side.Status != "open" {
		t.Fatalf("users=%d, notes=%+v, side round=%+v (%v)", users, f.h.listSteer(f.sessionID), side, err)
	}
}

func TestSessionAskRestoresNotesAfterRoundFailure(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "carry"
		if live {
			name = "live queue"
		}
		t.Run(name, func(t *testing.T) {
			f := newSummaryDeliveryFixture(t, false)
			engine, counter := newApprovalQueueEngine(t)
			roundDB, err := storage.Open(filepath.Join(filepath.Dir(counter), "panda.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { roundDB.Close() })
			f.h.store = engine.TaskStore()
			f.h.engine = engine
			f.h.cfgMu = &sync.RWMutex{}
			f.h.serviceCtx = context.Background()
			note, ok := f.h.pushSteer(f.sessionID, "keep this queued message", true)
			if !ok {
				t.Fatal("note was not accepted")
			}
			if !live {
				f.h.parkLeftovers(f.sessionID, nil)
			}
			if _, err := roundDB.Exec(`CREATE TRIGGER fail_note_round BEFORE INSERT ON ask_rounds
				BEGIN SELECT RAISE(ABORT, 'injected storage failure'); END`); err != nil {
				t.Fatal(err)
			}
			ask := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"","resume_queued":true}`))
				r.SetPathValue("id", f.sessionID)
				w := httptest.NewRecorder()
				f.h.sessionAsk(w, r)
				return w
			}
			failed := ask()
			if !strings.Contains(failed.Body.String(), "create round failed") {
				t.Fatalf("did not reach injected failure: %s", failed.Body.String())
			}
			if _, err := roundDB.Exec(`DROP TRIGGER fail_note_round`); err != nil {
				t.Fatal(err)
			}
			pending := f.h.listSteer(f.sessionID)
			if len(pending) != 1 || pending[0] != note {
				t.Fatalf("failed start lost or changed note: %+v, want %+v", pending, note)
			}
			resumed := ask()
			if resumed.Code != http.StatusOK {
				t.Fatalf("retry failed: %d %s", resumed.Code, resumed.Body.String())
			}
			// The entry-model fixture may reject its stub answer. Once the
			// user turn was saved, even a later model error must not replay it.
			sess, err := f.h.sessions.Get(f.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, turn := range sess.Turns {
				if turn.Role == "user" && turn.Text == note.text {
					count++
				}
			}
			if count != 1 || len(f.h.listSteer(f.sessionID)) != 0 {
				t.Fatalf("recovered note must be persisted once and consumed: turns=%d pending=%+v", count, f.h.listSteer(f.sessionID))
			}
		})
	}
}

type noteFailureWriter struct {
	*httptest.ResponseRecorder
	// onFlush runs once, when the stream's headers are flushed. That is the
	// last moment before the handler persists the operation and the user turn;
	// making the session file unavailable here exercises the write failure
	// without depending on which frame happens to be written first.
	onFlush func()
}

func (w *noteFailureWriter) Flush() {
	if w.onFlush != nil {
		w.onFlush()
		w.onFlush = nil
	}
	w.ResponseRecorder.Flush()
}

func TestSessionAskRestoresNotesAfterOperationFailure(t *testing.T) {
	f := newSummaryDeliveryFixture(t, false)
	engine, _ := newApprovalQueueEngine(t)
	f.h.engine = engine
	f.h.cfgMu = &sync.RWMutex{}
	note, _ := f.h.pushSteer(f.sessionID, "preserve original note metadata", false)
	f.h.parkLeftovers(f.sessionID, nil)
	path := filepath.Join(f.root, f.sessionID+".json")
	backup := path + ".unavailable"
	w := &noteFailureWriter{ResponseRecorder: httptest.NewRecorder(), onFlush: func() {
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
	}}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"resume_queued":true}`))
	r.SetPathValue("id", f.sessionID)
	f.h.sessionAsk(w, r)
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "save operation failed") {
		t.Fatalf("operation failure not reached: %s", w.Body.String())
	}
	if got := f.h.listSteer(f.sessionID); len(got) != 1 || got[0] != note {
		t.Fatalf("operation failure lost the note: %+v", got)
	}
}

func TestPendingNotesRollbackKeepsOrderAndDeletion(t *testing.T) {
	const id = "session"
	h := &handler{}
	first, _ := h.pushSteer(id, "first", false)
	claim1 := h.claimPendingNotes(id, true)
	second, _ := h.pushSteer(id, "second", true)
	claim2 := h.claimPendingNotes(id, true)
	h.settlePendingNotes(claim1, false)
	h.settlePendingNotes(claim2, false) // later claim rolls back last
	h.settlePendingNotes(claim1, false) // duplicate cleanup is harmless
	got := h.listSteer(id)
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("rollback reordered or duplicated input: %+v", got)
	}
	committed := h.claimPendingNotes(id, true)
	h.settlePendingNotes(committed, true)
	h.settlePendingNotes(committed, false)
	if len(h.listSteer(id)) != 0 || len(h.steerClaims) != 0 {
		t.Fatal("committed notes were restored or claim leaked")
	}
	h.pushSteer(id, "deleted", false)
	deleted := h.claimPendingNotes(id, true)
	h.dropSteer(id)
	h.settlePendingNotes(deleted, false)
	if len(h.listSteer(id)) != 0 || len(h.steerClaims) != 0 {
		t.Fatal("failed start resurrected deleted session notes")
	}
}
