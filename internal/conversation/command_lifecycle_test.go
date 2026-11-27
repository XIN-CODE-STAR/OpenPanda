// SPDX-License-Identifier: AGPL-3.0-or-later

package conversation

import (
	"context"
	"errors"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/testdb"
)

func TestGoalPauseAndResumePreserveUnansweredQuestion(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: testdb.Open(t)}
	const sid = "goal-question"
	g, err := s.Create(ctx, sid, "Verify the fixture with the required input", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.RoundID, err = s.Claim(ctx, g, "owner", "en")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PublishGoalQuestion(ctx, g, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, g, 0); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "resume"} {
		if err := s.Control(ctx, sid, g.ID, action, "", nil); err != nil {
			t.Fatal(err)
		}
		state, err := s.Get(ctx, sid)
		if err != nil || state.Goal == nil || state.Goal.WaitingRound != g.RoundID || state.Goal.WaitingFor != "input" {
			t.Fatalf("%s discarded the unanswered question: %+v, %v", action, state.Goal, err)
		}
	}
	if _, err := s.Claim(ctx, g, "owner", "en"); !errors.Is(err, ErrConflict) {
		t.Fatalf("claimed a round before the user answered: %v", err)
	}
	wait, err := s.ReserveInput(ctx, sid, g.RoundID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveInput(ctx, sid, wait); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, g, "owner", "en"); err != nil {
		t.Fatalf("could not continue after the user answered: %v", err)
	}
}

func TestConfirmKeepsEarlierImplementationAndNewProposal(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []string{"pending", "running", "waiting_input"} {
		t.Run(phase, func(t *testing.T) {
			s := &Store{DB: testdb.Open(t)}
			const sid = "plan-confirm"
			save := func(text string) int64 {
				t.Helper()
				if err := s.SetPlanning(ctx, sid, true); err != nil {
					t.Fatal(err)
				}
				version, err := s.SaveProposal(ctx, sid, text)
				if err != nil {
					t.Fatal(err)
				}
				return version
			}
			first := save("First plan")
			if _, err := s.Confirm(ctx, sid, first); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec("UPDATE conversation_plan_runs SET status=? WHERE session_id=? AND version=?", phase, sid, first); err != nil {
				t.Fatal(err)
			}
			second := save("Second plan")
			if _, err := s.Confirm(ctx, sid, second); !errors.Is(err, ErrWaiting) {
				t.Fatalf("confirmed a second live run during %s: %v", phase, err)
			}
			state, err := s.Get(ctx, sid)
			if err != nil || !state.Planning || state.ConfirmedVersion != first || state.ProposalVersion != second || state.Proposal != "Second plan" {
				t.Fatalf("failed confirmation changed saved state: %+v, %v", state, err)
			}
			if _, err := s.DB.Exec("UPDATE conversation_plan_runs SET status='done' WHERE session_id=? AND version=?", sid, first); err != nil {
				t.Fatal(err)
			}
			if text, err := s.Confirm(ctx, sid, second); err != nil || text != "Second plan" {
				t.Fatalf("cannot confirm after the first plan settled: %q, %v", text, err)
			}
		})
	}
}

func TestBeginPlanCommitsModeDraftAndGoalTogether(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: testdb.Open(t)}
	const sid = "plan-admission"
	if err := s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	version, err := s.SaveProposal(ctx, sid, "Existing proposal")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.Create(ctx, sid, "Active goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Control(ctx, sid, g.ID, "resume", "", nil); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_plan_draft BEFORE INSERT ON conversation_events
		WHEN NEW.kind='plan_invalidated' BEGIN SELECT RAISE(ABORT, 'fixture: record failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPlan(ctx, sid, true, 0); err == nil {
		t.Fatal("planning admission passed despite the failed write")
	}
	after, err := s.Get(ctx, sid)
	if err != nil || after.Planning || after.ModeGeneration != before.ModeGeneration || after.Proposal != before.Proposal ||
		after.Goal == nil || after.Goal.Status != "active" {
		t.Fatalf("failed admission changed the proposal or goal: %+v, %v", after, err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_plan_draft"); err != nil {
		t.Fatal(err)
	}
	draft, err := s.BeginPlan(ctx, sid, true, 0)
	if err != nil || draft.Version != version || draft.Generation != before.ModeGeneration+1 || !draft.ContinuesGoal {
		t.Fatalf("admitted draft did not capture its goal/version: %+v, %v", draft, err)
	}
	after, err = s.Get(ctx, sid)
	if err != nil || !after.Planning || after.Proposal != "" || after.Goal == nil || !after.Goal.PausedByPlan {
		t.Fatalf("admitted draft did not retire the proposal and pause the goal: %+v, %v", after, err)
	}
}

func TestGoalControlSealsExactRoundAndRejectsStaleStop(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: testdb.Open(t)}
	const sid = "goal-control"
	g, err := s.Create(ctx, sid, "Work on the fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(ctx, g, "owner", "en")
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.ControlGoal(ctx, sid, g.ID, "pause", "", nil, "")
	if err != nil || target.RoundID != first {
		t.Fatalf("pause did not capture the claimed round: %+v, %v", target, err)
	}
	if err := s.Renew(ctx, target, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("runner renewed a sealed round: %v", err)
	}
	if err := s.Settle(ctx, target, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Control(ctx, sid, g.ID, "resume", "", nil); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, g, "owner", "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlGoal(ctx, sid, g.ID, "stop", "", nil, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("stop for the old round reached the new one: %v", err)
	}
	replaced, previous, err := s.ReplaceGoal(ctx, sid, "Next objective", nil)
	if err != nil || replaced.ID == g.ID || previous.RoundID != second {
		t.Fatalf("replacement did not capture the exact current round: %+v, %+v, %v", replaced, previous, err)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM ask_rounds WHERE round_id=?", second).Scan(&status); err != nil || status != "stopped" {
		t.Fatalf("replacement did not seal the old round: %s, %v", status, err)
	}
}
