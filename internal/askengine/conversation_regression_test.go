// SPDX-License-Identifier: AGPL-3.0-or-later

package askengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/conversation"
	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/entry"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

const conversationGeneratedGraph = `{"kind":"plan","plan":{"goal":"Plan the independent fixture","objectives":{"version":1,"revision":1,"dimensions":[{"dimension":"result","weight":1,"priority":"NORMAL","requirement":"Verify the independent fixture","reason":"This isolates draft intent","criteria":["The fixture check is recorded"]}]},"stages":[{"id":"verify","intent":"Verify the independent fixture"}]}}`

const conversationReviewedGraph = `{"goal":"Fixture implementation","stages":[{"id":"inspect","intent":"Inspect the fixture"},{"id":"verify","intent":"Verify the fixture","needs":["inspect"]}]}`

func manualConversation(t *testing.T, e *Engine) (*ConversationService, string, context.Context) {
	t.Helper()
	st := sessions.NewStore(t.TempDir())
	sess, err := st.Create("fixture")
	if err != nil {
		t.Fatal(err)
	}
	service := e.Conversations(st)
	service.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := &ConversationService{engine: e, store: service.store, sessions: st, owner: "audit-owner", ctx: ctx, cancel: cancel, running: map[string]context.CancelFunc{}}
	t.Cleanup(s.Close)
	return s, sess.ID, ctx
}

func confirmTestPlan(t *testing.T, s *ConversationService, ctx context.Context, sid, proposal string) int64 {
	t.Helper()
	if err := s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	version, err := s.store.SaveProposal(ctx, sid, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPlan(ctx, sid, version); err != nil {
		t.Fatal(err)
	}
	return version
}

func claimTestPlan(t *testing.T, s *ConversationService, ctx context.Context, sid, round string) {
	t.Helper()
	if err := s.engine.TaskStore().BeginAskRound(ctx, round, sid, "en"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DB.ExecContext(ctx, "UPDATE conversation_plan_runs SET status='running',round_id=? WHERE session_id=? AND status='pending'", round, sid); err != nil {
		t.Fatal(err)
	}
}

func testPlanStatus(t *testing.T, s *ConversationService, sid string, version int64) string {
	t.Helper()
	var status string
	if err := s.store.DB.QueryRow("SELECT status FROM conversation_plan_runs WHERE session_id=? AND version=?", sid, version).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPlanDraftLeavesOlderClarificationPending(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		response := conversationGeneratedGraph
		switch calls.Add(1) {
		case 1:
			response = clarifyJSON
		case 3:
			response = "Previous implementation continued without its answer."
		}
		_ = json.NewEncoder(w).Encode(anthropicText(response))
	})
	s, sid, ctx := manualConversation(t, e)
	v := confirmTestPlan(t, s, ctx, sid, "Implement the first fixture after the user supplies its directory")
	claimTestPlan(t, s, ctx, sid, "first-implementation")
	s.runPlan(sid, "first-implementation", "Implement the first fixture after the user supplies its directory", v)
	if status := testPlanStatus(t, s, sid, v); status != "waiting_input" {
		t.Fatalf("fixture: first plan did not ask for input: %s", status)
	}
	out, err := e.AskTurnsScoped(ctx, nil, "Draft a separate plan for the second fixture", AskScope{SessionID: sid, Mode: "plan"}, false, StreamCallbacks{})
	if err != nil || out == nil || !out.PlanProposal {
		t.Fatalf("fixture: new draft = %+v, %v", out, err)
	}
	if err = s.SetPlanning(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	s.sweepPlans(ctx)
	s.wg.Wait()
	s.sweepPlans(ctx)
	if status := testPlanStatus(t, s, sid, v); status != "waiting_input" || calls.Load() != 2 {
		t.Fatalf("unanswered prior implementation advanced: status=%s model_calls=%d; want waiting_input and 2 calls", status, calls.Load())
	}
	state, err := s.State(ctx, sid)
	if err != nil || state.Implementation == nil || state.Implementation.Phase != "waiting_input" || state.ProposalVersion != out.ProposalVersion {
		t.Fatalf("new draft hid the unanswered implementation: %+v, %v", state, err)
	}
	if _, err := s.ConfirmPlan(ctx, sid, out.ProposalVersion); !errors.Is(err, conversation.ErrWaiting) {
		t.Fatalf("new draft was confirmed before the earlier plan settled: %v", err)
	}
	if stopped, err := s.StopOperation(ctx, sid, state.Implementation.Round); err != nil || !stopped {
		t.Fatalf("could not stop the unanswered implementation: stopped=%t, %v", stopped, err)
	}
	if _, err := s.ConfirmPlan(ctx, sid, out.ProposalVersion); err != nil {
		t.Fatalf("could not confirm the new draft after stopping the earlier plan: %v", err)
	}
}

func TestGoalQuestionSurvivesNewPlanDraft(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		response := conversationGeneratedGraph
		if calls.Add(1) == 1 {
			response = clarifyJSON
		}
		_ = json.NewEncoder(w).Encode(anthropicText(response))
	})
	s, sid, ctx := manualConversation(t, e)
	g, err := s.Create(ctx, sid, "Verify the fixture in the directory that I will supply", nil)
	if err != nil {
		t.Fatal(err)
	}
	round, err := s.store.Claim(ctx, g, s.owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	g.RoundID = round
	s.run(g)
	s.reconcile(ctx, g)
	state, err := s.State(ctx, sid)
	if err != nil || state.Goal.WaitingFor != "input" || state.Goal.RoundID != "" {
		t.Fatalf("fixture: goal=%+v err=%v", state.Goal, err)
	}
	out, err := e.AskTurnsScoped(ctx, nil, "Draft a roadmap for an unrelated fixture", AskScope{SessionID: sid, Mode: "plan"}, false, StreamCallbacks{})
	if err != nil || out == nil || !out.PlanProposal {
		t.Fatalf("fixture: draft = %+v, %v", out, err)
	}
	state, err = s.State(ctx, sid)
	if err != nil || state.Goal.WaitingFor != "input" {
		t.Fatalf("draft erased a goal question: goal=%+v err=%v", state.Goal, err)
	}
}

func TestConversationReplyResolvesOneQuestion(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "fixture rejected the reply", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicText("The user's answer is recorded."))
	})
	s, sid, ctx := manualConversation(t, e)
	goal, err := s.Create(ctx, sid, "Check the named fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal.RoundID, err = s.store.Claim(ctx, goal, s.owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.PublishGoalQuestion(ctx, goal, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.TaskStore().CloseAskRound(ctx, goal.RoundID, "closed"); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx, goal)
	version := confirmTestPlan(t, s, ctx, sid, "Check a separate fixture")
	const planRound = "separate-question"
	claimTestPlan(t, s, ctx, sid, planRound)
	if _, err = e.db.Exec("UPDATE conversation_plan_runs SET status='waiting_input' WHERE round_id=?", planRound); err != nil {
		t.Fatal(err)
	}
	if err = e.TaskStore().CloseAskRound(ctx, planRound, "stopped"); err != nil {
		t.Fatal(err)
	}
	reply := func(round string) error {
		_, err := e.AskTurnsScoped(ctx, nil, "Use the fixture directory.", AskScope{SessionID: sid, ReplyTo: round}, false, StreamCallbacks{})
		return err
	}
	state := func() conversation.State {
		t.Helper()
		value, err := s.State(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if err := reply(goal.RoundID); err == nil {
		t.Fatal("fixture failed to reject the answer")
	}
	if state().Goal.WaitingRound != goal.RoundID {
		t.Fatal("failed reply released a question")
	}
	if err := reply(goal.RoundID); err != nil {
		t.Fatal(err)
	}
	if state().Goal.WaitingFor != "" || testPlanStatus(t, s, sid, version) != "waiting_input" {
		t.Fatal("a reply to the goal also released the separate implementation")
	}
	if err := reply(goal.RoundID); !errors.Is(err, conversation.ErrConflict) || calls.Load() != 2 {
		t.Fatalf("stale reply reached the model: calls=%d err=%v", calls.Load(), err)
	}
	if err := reply(""); err != nil || testPlanStatus(t, s, sid, version) != "pending" {
		t.Fatalf("reply to the remaining implementation: %v", err)
	}
}

func TestConcurrentRepliesReserveOneQuestion(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(fmt.Sprint(implicit), func(t *testing.T) {
			firstStarted := make(chan struct{})
			firstUnblocked := make(chan struct{})
			var calls atomic.Int32
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.Contains(lastUserMessage(t, r), "first device") {
					close(firstStarted)
					<-firstUnblocked
				}
				_ = json.NewEncoder(w).Encode(anthropicText("Answer recorded."))
			})
			s, sid, ctx := manualConversation(t, e)
			goal, err := s.Create(ctx, sid, "Wait for the user's answer", nil)
			if err != nil {
				t.Fatal(err)
			}
			goal.RoundID, err = s.store.Claim(ctx, goal, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.store.PublishGoalQuestion(ctx, goal, nil); err != nil {
				t.Fatal(err)
			}
			s.reconcile(ctx, goal)
			reply := func(text, round string) error {
				_, err := e.AskTurnsScoped(ctx, nil, text, AskScope{SessionID: sid, ReplyTo: round}, false, StreamCallbacks{})
				return err
			}
			firstErr := make(chan error, 1)
			go func() { firstErr <- reply("The first device answered.", goal.RoundID) }()
			defer func() {
				close(firstUnblocked)
				if err := <-firstErr; err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-firstStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("first model call did not start")
			}
			round := goal.RoundID
			if implicit {
				round = ""
			}
			if err := reply("The second device answered.", round); !errors.Is(err, conversation.ErrConflict) || calls.Load() != 1 {
				t.Fatalf("second reply: %v, model calls=%d", err, calls.Load())
			}
		})
	}
}

func TestPublishedQuestionsCanBeAnsweredBeforePublisherReturns(t *testing.T) {
	for _, kind := range []string{"plan", "goal"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(anthropicText("I recorded the answer."))
			})
			s, sid, ctx := manualConversation(t, e)
			visible := make(chan struct{})
			release := make(chan struct{})
			published := make(chan error, 1)
			round := "publishing-question"
			persist := func() error {
				_, _, err := s.sessions.PersistTurns(sid, sessions.Turn{
					Role: "assistant", Kind: "clarify", RoundID: round, Text: "Which fixture directory should I use?",
				})
				close(visible)
				<-release
				return err
			}
			if kind == "plan" {
				version := confirmTestPlan(t, s, ctx, sid, "Implement the fixture")
				claimTestPlan(t, s, ctx, sid, round)
				go func() { published <- s.store.PublishPlanQuestion(ctx, sid, round, version, persist) }()
			} else {
				goal, err := s.Create(ctx, sid, "Implement the fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				goal.RoundID, err = s.store.Claim(ctx, goal, s.owner, "en")
				if err != nil {
					t.Fatal(err)
				}
				round = goal.RoundID
				go func() { published <- s.store.PublishGoalQuestion(ctx, goal, persist) }()
			}
			select {
			case <-visible:
			case err := <-published:
				t.Fatalf("publisher exited before writing question: %v", err)
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("question did not become visible")
			}
			replies := make(chan error, 1)
			go func() {
				_, err := e.AskTurnsScoped(ctx, nil, "Use the fixture directory.", AskScope{SessionID: sid, ReplyTo: round}, false, StreamCallbacks{})
				replies <- err
			}()
			select {
			case err := <-replies:
				close(release)
				t.Fatalf("reply rejected while its question was being published: %v", err)
			case <-time.After(50 * time.Millisecond):
				close(release)
			}
			if err := <-published; err != nil {
				t.Fatal(err)
			}
			if err := <-replies; err != nil || calls.Load() != 1 {
				t.Fatalf("reply did not reach the model: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestConfirmedPlanKeepsClaimedGraphAfterReenteringPlanning(t *testing.T) {
	var modelCalls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		_ = json.NewEncoder(w).Encode(anthropicText(conversationGeneratedGraph))
	})
	s, sid, ctx := manualConversation(t, e)
	v := confirmTestPlan(t, s, ctx, sid, conversationReviewedGraph)
	if err := s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	s.sweepPlans(ctx)
	if status := testPlanStatus(t, s, sid, v); status != "pending" {
		t.Fatalf("unclaimed implementation while planning = %s", status)
	}
	if err := s.SetPlanning(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	claimTestPlan(t, s, ctx, sid, "confirmed-before-toggle")
	if err := s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	s.runPlan(sid, "confirmed-before-toggle", conversationReviewedGraph, v)
	s.sweepPlans(ctx)
	var tasks int
	if err := s.store.DB.QueryRow("SELECT count(*) FROM ask_round_tasks WHERE round_id='confirmed-before-toggle'").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	status := testPlanStatus(t, s, sid, v)
	if status != "running" || tasks != 3 || modelCalls.Load() != 0 || state.ProposalVersion != v {
		t.Fatalf("confirmed work converted into a draft: status=%s tasks=%d model_calls=%d proposal_version=%d confirmed_version=%d", status, tasks, modelCalls.Load(), state.ProposalVersion, v)
	}
	s.sweepPlans(ctx)
	if got := testPlanStatus(t, s, sid, v); got != "running" {
		t.Fatalf("implementation detached from its live stages: %s", got)
	}
}

func TestGoalClarificationSupersedesCompletionAudit(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "tool_use", "id": "audit", "name": "update_goal", "input": map[string]any{"status": "complete", "reason": "All parts looked complete at the previous checkpoint."}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicText(clarifyJSON))
	})
	s, sid, ctx := manualConversation(t, e)
	g, err := s.Create(ctx, sid, "Verify the full fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	round, err := s.store.Claim(ctx, g, s.owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	g.RoundID = round
	s.run(g)
	before, err := s.State(ctx, sid)
	if err != nil || before.Goal.WaitingFor != "input" || calls.Load() != 2 {
		t.Fatalf("fixture: goal=%+v calls=%d err=%v", before.Goal, calls.Load(), err)
	}
	s.reconcile(ctx, g)
	state, err := s.State(ctx, sid)
	if err != nil || state.Goal.Status != "active" || state.Goal.WaitingFor != "input" {
		t.Fatalf("goal that requires input was completed: goal=%+v err=%v", state.Goal, err)
	}
}

func TestGoalPlanContinuesAfterDiscardingPausedAudit(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(anthropicText("Performed checks on the fixture."))
			})
			s, sid, ctx := manualConversation(t, e)
			g, err := s.Create(ctx, sid, "Verify the fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			round, err := s.store.Claim(ctx, g, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			g.RoundID = round
			if err := s.store.Report(ctx, g, "complete", "Verified the first goal execution."); err != nil {
				t.Fatal(err)
			}
			if err := s.SetPlanning(ctx, sid, true); err != nil {
				t.Fatal(err)
			}
			if err := e.TaskStore().CloseAskRound(ctx, round, "closed"); err != nil {
				t.Fatal(err)
			}
			s.reconcile(ctx, g)
			proposal := "Implement the added fixture checks"
			if structured {
				proposal = conversationReviewedGraph
			}
			v := confirmTestPlan(t, s, ctx, sid, proposal)
			s.sweepPlans(ctx)
			s.wg.Wait()
			s.sweepPlans(ctx)
			state, err := s.State(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			status := testPlanStatus(t, s, sid, v)
			if structured {
				var tasks int
				if err = s.store.DB.QueryRow("SELECT count(*) FROM ask_round_tasks").Scan(&tasks); err != nil {
					t.Fatal(err)
				}
				if tasks == 0 {
					t.Fatalf("linked reviewed graph published zero tasks: plan=%s old_audit=%t goal=%s", status, state.Goal.CompletionRequested, state.Goal.Status)
				}
				return
			}
			if status != "done" {
				t.Fatalf("fixture: implementation status=%s", status)
			}
			if _, err = s.store.Claim(ctx, state.Goal, s.owner, "en"); err != nil {
				t.Fatalf("auto-resumed goal cannot claim again: goal=%+v err=%v", state.Goal, err)
			}
		})
	}
}

func TestGoalRecoveryWaitsForConcurrentDispatch(t *testing.T) {
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("unexpected provider call") })
	s, sid, ctx := manualConversation(t, e)
	g, err := s.Create(ctx, sid, "Run the documented fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	round, err := s.store.Claim(ctx, g, s.owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	g.RoundID = round
	task, err := e.TaskStore().Create(ctx, "", "", "Run fixture", "fixture", []string{"fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.DB.Exec("UPDATE ask_rounds SET lease_until=0 WHERE round_id=?", round); err != nil {
		t.Fatal(err)
	}
	// Materialize the dispatch that wins the writer lock between progress read
	// and round sealing. A second progress read must see this membership.
	q := fmt.Sprintf("CREATE TRIGGER test_dispatch_at_seal BEFORE UPDATE OF status ON ask_rounds WHEN NEW.round_id='%s' AND NEW.status='interrupted' BEGIN INSERT INTO ask_round_tasks(round_id,task_id) VALUES('%s','%s'); END", round, round, task.TaskID)
	if _, err = s.store.DB.Exec(q); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx, g)
	progress, err := e.TaskStore().AskRoundProgress(ctx, sid, round)
	if err != nil || len(progress.Tasks) != 1 || core.Terminal(progress.Tasks[0].State) {
		t.Fatalf("fixture: %+v %v", progress, err)
	}
	var settled bool
	if err = s.store.DB.QueryRow("SELECT settled FROM conversation_goal_rounds WHERE round_id=?", round).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled {
		t.Fatalf("round settled before the committed member: state=%s", progress.Tasks[0].State)
	}
}

func TestConfirmedPlanReplaysAnswerAfterLegacyReceipt(t *testing.T) {
	var sawAnswer atomic.Bool
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		var raw strings.Builder
		_, _ = io.Copy(&raw, r.Body)
		sawAnswer.Store(strings.Contains(raw.String(), "legacy-user-answer-742"))
		_ = json.NewEncoder(w).Encode(anthropicText("Finished the fixture."))
	})
	s, sid, ctx := manualConversation(t, e)
	v := confirmTestPlan(t, s, ctx, sid, "Use the fixture directory provided by the user")
	old := sessions.PlanConfirmationTurn("previous-round", v, fmt.Sprintf("/plan confirm %d", v))
	if _, err := s.sessions.AppendTurn(sid, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessions.AppendTurn(sid, sessions.Turn{Role: "assistant", Text: "Which fixture directory?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessions.AppendTurn(sid, sessions.Turn{Role: "user", Text: "legacy-user-answer-742"}); err != nil {
		t.Fatal(err)
	}
	s.sweepPlans(ctx)
	s.wg.Wait()
	if !sawAnswer.Load() {
		t.Fatal("resumed implementation lost the answer after a legacy confirmation receipt")
	}
}

func TestConfirmedPlanRetainsFailureAfterTaskDeletion(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("unexpected provider call") })
			s, sid, ctx := manualConversation(t, e)
			if err := s.SetPlanning(ctx, sid, true); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Create(ctx, sid, "Only continue goal once the implementation succeeds", nil); err != nil {
				t.Fatal(err)
			}
			version := confirmTestPlan(t, s, ctx, sid, "Implement this fixture")
			const round = "closed-with-failed-task"
			claimTestPlan(t, s, ctx, sid, round)
			tasks := e.TaskStore()
			task, err := tasks.Create(core.WithAskRound(ctx, round), "", "", "Failed fixture task", "fixture", []string{"fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err = tasks.CloseAskRound(ctx, round, "closed"); err != nil {
				t.Fatal(err)
			}
			if err = tasks.ForceFail(ctx, task.TaskID, "fixture failed"); err != nil {
				t.Fatal(err)
			}
			if remove {
				if _, err = tasks.Delete(ctx, task.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			s.sweepPlans(ctx)
			plan := testPlanStatus(t, s, sid, version)
			state, err := s.State(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if plan != "failed" || state.Goal.Status != "paused" {
				t.Fatalf("failed run resumed goal: deleted=%t plan=%s goal=%s", remove, plan, state.Goal.Status)
			}
		})
	}
}

func TestGoalClaimKeepsItsModeWhenPlanningStarts(t *testing.T) {
	var modelCalls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		_ = json.NewEncoder(w).Encode(anthropicText(conversationGeneratedGraph))
	})
	s, sid, ctx := manualConversation(t, e)
	g, err := s.Create(ctx, sid, "Verify the independent fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	round, err := s.store.Claim(ctx, g, s.owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	g.RoundID = round
	if err = s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	s.run(g)
	state, err := s.State(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.ProposalVersion > 0 || state.Proposal != "" {
		t.Fatalf("claimed background goal wrote a plan proposal the user did not request: calls=%d proposal_version=%d", modelCalls.Load(), state.ProposalVersion)
	}
}

func TestConfirmedPlanToolDigestDoesNotCompleteImplementation(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "tool_use", "id": "read", "name": "audit_read", "input": map[string]any{}}}})
		default:
			_ = json.NewEncoder(w).Encode(anthropicText(""))
		}
	})
	e.registry.Register(entry.Tool{Name: "audit_read", Tier: 1, ReadOnly: true, Run: func(context.Context, map[string]any) (string, error) {
		return "Read a fixture, no implementation was made.", nil
	}})
	s, sid, ctx := manualConversation(t, e)
	if err := s.SetPlanning(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, sid, "The goal may only resume after implementation", nil); err != nil {
		t.Fatal(err)
	}
	v := confirmTestPlan(t, s, ctx, sid, "Implement the fixture")
	s.sweepPlans(ctx)
	s.wg.Wait()
	s.sweepPlans(ctx)
	state, err := s.State(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	status := testPlanStatus(t, s, sid, v)
	if status == "done" || state.Goal.Status == "active" {
		t.Fatalf("unavailable implementation went through as success: plan=%s goal=%s calls=%d", status, state.Goal.Status, calls.Load())
	}
}

func TestGoalPausesAfterIncompleteToolAnswer(t *testing.T) {
	var calls atomic.Int32
	e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1)%3 == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]any{{"type": "tool_use", "id": "read", "name": "audit_read", "input": map[string]any{}}},
				"usage":   map[string]int{"input_tokens": 7, "output_tokens": 3},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicText(""))
	})
	e.registry.Register(entry.Tool{Name: "audit_read", Tier: 1, ReadOnly: true, Run: func(context.Context, map[string]any) (string, error) {
		return "The fixture was read and still needs implementation.", nil
	}})
	s, sid, ctx := manualConversation(t, e)
	if _, err := s.Create(ctx, sid, "Implement the fixture", nil); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		s.sweep()
		s.wg.Wait()
	}
	state, err := s.State(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal.Status != "paused" || state.Goal.Rounds != 1 || calls.Load() != 3 || state.Goal.Tokens == 0 {
		t.Fatalf("incomplete answer allowed another goal round or lost its usage: goal=%+v calls=%d", state.Goal, calls.Load())
	}
	session, err := s.sessions.Get(sid)
	if err != nil || session.Operation == nil || session.Operation.Status != "error" {
		t.Fatalf("interrupted goal operation was reported as successful: session=%+v err=%v", session, err)
	}
}

func TestPlanDeliverySurvivesTaskDeletion(t *testing.T) {
	for _, method := range []string{"retain", "delete", "clear", "retention"} {
		t.Run(method, func(t *testing.T) {
			e, store, _ := newStageTestEngine(t)
			e.queueTasks = true
			s, sid, ctx := manualConversation(t, e)
			version := confirmTestPlan(t, s, ctx, sid, conversationReviewedGraph)
			const round = "plan-delivery-with-deletion"
			claimTestPlan(t, s, ctx, sid, round)
			s.runPlan(sid, round, conversationReviewedGraph, version)
			var planID string
			if err := e.db.QueryRow("SELECT plan_id FROM tasks WHERE session_id=? AND plan_id!='' LIMIT 1", sid).Scan(&planID); err != nil {
				t.Fatal(err)
			}
			stages, err := store.PlanStages(ctx, planID)
			if err != nil {
				t.Fatal(err)
			}
			delivery := strings.Repeat("The fixture passed this check.\n", 32) + "This is the conclusion."
			var finalTask string
			for _, stage := range stages {
				var spec entry.TaskSpecDetail
				if err := json.Unmarshal([]byte(stage.SpecJSON), &spec); err != nil {
					t.Fatal(err)
				}
				answer := "Intermediate result."
				if spec.PlanFinal {
					finalTask, answer = stage.TaskID, delivery
				}
				payload, err := json.Marshal(map[string]string{"stdout": answer})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := e.db.Exec("UPDATE tasks SET state='done',result_json=? WHERE task_id=?", string(payload), stage.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			if finalTask == "" {
				t.Fatal("fixture did not dispatch a final stage")
			}
			switch method {
			case "delete":
				_, err = store.Delete(ctx, finalTask)
			case "clear":
				_, _, err = store.ClearQueue(ctx)
			case "retention":
				_, err = store.DeleteSettledBefore(ctx, time.Now().Add(time.Minute).Unix())
			}
			if err != nil {
				t.Fatal(err)
			}
			s.sweepPlans(ctx)
			s.sweepPlans(ctx)
			session, err := s.sessions.Get(sid)
			if err != nil {
				t.Fatal(err)
			}
			deliveries := 0
			for _, turn := range session.Turns {
				if turn.IdempotencyKey != "plan:delivery:"+round+":"+planID {
					continue
				}
				deliveries++
				if turn.Text != delivery {
					t.Fatalf("incorrect final answer: %q", turn.Text)
				}
			}
			if status := testPlanStatus(t, s, sid, version); status != "done" || deliveries != 1 {
				t.Fatalf("plan status=%s, deliveries=%d", status, deliveries)
			}
		})
	}
}

func TestGoalReceivesEvidenceAfterTaskDeletion(t *testing.T) {
	const receipt = "The completed task verified the expected fixture result."
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			var sawReceipt atomic.Bool
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
				sawReceipt.Store(strings.Contains(lastUserMessage(t, r), receipt))
				_ = json.NewEncoder(w).Encode(anthropicText("I checked the prior task result."))
			})
			s, sid, ctx := manualConversation(t, e)
			goal, err := s.Create(ctx, sid, "Verify the fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			goal.RoundID, err = s.store.Claim(ctx, goal, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			store := e.TaskStore()
			task, err := store.Create(core.WithAskRound(ctx, goal.RoundID), "", "", "Verify fixture", "fixture", []string{"fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.db.Exec("UPDATE tasks SET state='done',result_json=? WHERE task_id=?", fmt.Sprintf(`{"stdout":%q}`, receipt), task.TaskID); err != nil {
				t.Fatal(err)
			}
			if err = store.CloseAskRound(ctx, goal.RoundID, "closed"); err != nil {
				t.Fatal(err)
			}
			if remove {
				if _, err = store.Delete(ctx, task.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			s.reconcile(ctx, goal)
			state, err := s.State(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			goal = state.Goal
			goal.RoundID, err = s.store.Claim(ctx, goal, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			s.run(goal)
			if !sawReceipt.Load() {
				t.Fatal("the next Goal round did not receive the completed task result")
			}
		})
	}
}

func TestGoalDeletionKeepsUsageBeforeAndAfterSettlement(t *testing.T) {
	for _, method := range []string{"delete", "clear", "retention"} {
		t.Run(method, func(t *testing.T) {
			e, _ := newBoundaryTestEngine(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected provider call") })
			s, sid, ctx := manualConversation(t, e)
			budget := int64(80)
			goal, err := s.Create(ctx, sid, "A budgeted fixture task", &budget)
			if err != nil {
				t.Fatal(err)
			}
			round, err := s.store.Claim(ctx, goal, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			goal.RoundID = round
			tasks := e.TaskStore()
			root, err := tasks.Create(core.WithAskRound(ctx, round), "", "", "Fixture task", "fixture", []string{"fixture"})
			if err != nil {
				t.Fatal(err)
			}
			child, err := tasks.Create(ctx, root.TaskID, "", "A helper", "fixture", []string{"fixture"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{root.TaskID, child.TaskID} {
				if err = tasks.ForceFail(ctx, id, "fixture ended after using tokens"); err != nil {
					t.Fatal(err)
				}
			}
			if err = tasks.RecordDelegationMetric(ctx, child.TaskID, "controller", "fixture", nil, false, 20, 80); err != nil {
				t.Fatal(err)
			}
			if err = tasks.CloseAskRound(ctx, round, "closed"); err != nil {
				t.Fatal(err)
			}
			switch method {
			case "delete":
				_, err = tasks.Delete(ctx, root.TaskID)
			case "clear":
				_, _, err = tasks.ClearQueue(ctx)
			case "retention":
				_, err = tasks.DeleteSettledBefore(ctx, time.Now().Unix()+60)
			}
			if err != nil {
				t.Fatal(err)
			}
			s.reconcile(ctx, goal)
			state, err := s.State(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if state.Goal.Tokens != 80 || state.Goal.Status != "budget_limited" {
				t.Fatalf("settled after %s: usage=%d status=%s", method, state.Goal.Tokens, state.Goal.Status)
			}
			if err = tasks.RecordDelegationMetric(ctx, child.TaskID, "controller", "fixture", nil, false, 20, 13); err != nil {
				t.Fatal(err)
			}
			s.reconcile(ctx, goal)
			state, err = s.State(ctx, sid)
			if err != nil || state.Goal.Tokens != 93 {
				t.Fatalf("late usage after %s: goal=%+v err=%v", method, state.Goal, err)
			}
		})
	}
}

func TestGoalCreationOnlyAcceptsForegroundDirective(t *testing.T) {
	const tail = "Verify fixture contents exactly"
	const objective = "create goal: " + tail
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint(background), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := newBoundaryTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "tool_use", "id": "create", "name": "create_goal", "input": map[string]any{"objective": tail}}}})
					return
				}
				_ = json.NewEncoder(w).Encode(anthropicText("I checked the current goal."))
			})
			s, sid, ctx := manualConversation(t, e)
			if !background {
				if _, err := e.AskTurnsScoped(ctx, nil, objective, AskScope{SessionID: sid}, false, StreamCallbacks{}); err != nil {
					t.Fatal(err)
				}
				state, err := s.State(ctx, sid)
				if err != nil || state.Goal == nil || state.Goal.Objective != tail {
					t.Fatalf("explicit request failed: %+v, %v", state.Goal, err)
				}
				return
			}
			budget := int64(100)
			original, err := s.Create(ctx, sid, objective, &budget)
			if err != nil {
				t.Fatal(err)
			}
			round, err := s.store.Claim(ctx, original, s.owner, "en")
			if err != nil {
				t.Fatal(err)
			}
			original.RoundID = round
			s.run(original)
			state, err := s.State(ctx, sid)
			if err != nil || state.Goal == nil || state.Goal.ID != original.ID || state.Goal.Objective != objective ||
				state.Goal.Generation != original.Generation || state.Goal.TokenBudget == nil || *state.Goal.TokenBudget != budget {
				t.Fatalf("background text granted goal replacement: %+v, %v", state.Goal, err)
			}
		})
	}
}
