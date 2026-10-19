// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/commander"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/entry"
)

// TestContextOverflowCompactRetry verifies the in-round recovery: a context
// overflow on the first attempt re-runs the round once on a compressed
// prompt (fresh session, tight budget) instead of parking — the shrunken
// run fits and the task completes.
func TestContextOverflowCompactRetry(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "ovf-retry", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSupervisor(newFakeSupervisor(t, func(int) string {
		return `{"status":"done","reason":"ok","followup":""}`
	}))

	var calls atomic.Int32
	var retryPrompt string
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		if calls.Add(1) == 1 {
			return commander.AgentResult{OK: false, ExitCode: 1,
				Stderr: "API Error: context_length_exceeded — prompt is too long"}
		}
		retryPrompt = prompt
		return commander.AgentResult{OK: true, Result: "done", ExitCode: 0}
	})

	task, result, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "overflow",
		Project:     "proj",
		ContextType: "command",
		Intent:      "do the work",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.State != StateDone {
		t.Fatalf("state = %s, want done (compressed retry should have recovered)", task.State)
	}
	if calls.Load() != 2 {
		t.Fatalf("agent ran %d times, want 2 (overflow + one compressed retry)", calls.Load())
	}
	if !result.OK {
		t.Fatalf("result OK = false, want true")
	}
	if !strings.Contains(retryPrompt, "do the work") {
		t.Fatalf("compressed retry lost the task intent: %q", retryPrompt)
	}
}

// TestContextOverflowParksAfterRetryExhausted verifies the bound: when even
// the compressed retry overflows, the task parks in review for a human — the
// retry must not loop, so exactly two adapter calls happen.
func TestContextOverflowParksAfterRetryExhausted(t *testing.T) {
	ctx := context.Background()
	c := newSuperviseCore(t, "ovf-park", 1)
	c.SetWorkDir(t.TempDir())
	c.SetSupervisor(newFakeSupervisor(t, func(int) string {
		return `{"status":"done","reason":"ok","followup":""}`
	}))

	var calls atomic.Int32
	c.router.SetAdapterRunner(func(ctx context.Context, adapter, prompt, cwd string) commander.AgentResult {
		calls.Add(1)
		return commander.AgentResult{OK: false, ExitCode: 1,
			Stderr: "This model's maximum context length is 8000 tokens"}
	})

	task, _, err := c.SubmitLocal(ctx, TaskInput{
		Title:       "always overflows",
		Project:     "proj",
		ContextType: "command",
		Intent:      "do the work",
		Requires:    []string{"code:modify"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.State != StateReview {
		t.Fatalf("state = %s, want review (parked for a human)", task.State)
	}
	if calls.Load() != 2 {
		t.Fatalf("agent ran %d times, want exactly 2 (overflow + one retry)", calls.Load())
	}
}

// TestCompactRunNotesTruncatesWithoutSupervisor covers the no-model degrade
// path: the accumulated notes are hard-capped keeping the newest entries —
// detail is lost, the cap holds.
func TestCompactRunNotesTruncatesWithoutSupervisor(t *testing.T) {
	c := &Core{logger: testLogger()}
	var notes []string
	for i := 0; i < 10; i++ {
		notes = append(notes, strings.Repeat("x", 4000))
	}
	got := c.compactRunNotes(context.Background(), notes)
	total := 0
	for _, n := range got {
		total += len(n)
	}
	if total > 12000 {
		t.Fatalf("compressed notes = %d bytes, want <= 12000", total)
	}
	if got[len(got)-1] != notes[len(notes)-1] {
		t.Fatal("newest note must survive the truncation")
	}
}

// TestCompactRunNotesCutOnRuneBoundary: the 96000-byte tail cut can land
// inside a multi-byte rune — a dangling continuation byte would reach the
// supervisor as mojibake. The cut must advance to a rune boundary first.
func TestCompactRunNotesCutOnRuneBoundary(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		resp := map[string]any{"content": []map[string]string{{"type": "text", "text": "digest"}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	client, err := entry.NewClient(config.ModelConfig{BaseURL: srv.URL, APIKey: "sk-test", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("supervisor client: %v", err)
	}
	c := &Core{logger: testLogger()}
	c.SetSupervisor(client)

	// "é" straddles the 96000-byte cut: without the boundary walk the tail
	// begins on its continuation byte.
	note := strings.Repeat("x", 999) + "é" + strings.Repeat("y", 95999)
	got := c.compactRunNotes(context.Background(), []string{note})
	if len(got) != 1 || !strings.Contains(got[0], "digest") {
		t.Fatalf("digest path lost: %v", got)
	}
	if bytes.Contains(captured, []byte("�")) || bytes.Contains(captured, []byte(`�`)) {
		t.Fatal("mojibake reached the supervisor prompt")
	}
}

// TestCompactRunNotesDigests covers the supervisor path: the notes fold into
// a single model-written brief.
func TestCompactRunNotesDigests(t *testing.T) {
	c := &Core{logger: testLogger()}
	c.SetSupervisor(newFakeSupervisor(t, func(int) string {
		return "child A built the parser; child B's result is still pending"
	}))
	got := c.compactRunNotes(context.Background(), []string{
		"[delegated child result]\nA: parser done",
		"[delegated child result arrived]\nB: still running",
	})
	if len(got) != 1 {
		t.Fatalf("compressed notes = %d entries, want 1 digest", len(got))
	}
	if !strings.Contains(got[0], "child A built the parser") {
		t.Fatalf("digest missing supervisor text: %q", got[0])
	}
	if !strings.Contains(got[0], "earlier run context") {
		t.Fatalf("digest missing its marker: %q", got[0])
	}
}
