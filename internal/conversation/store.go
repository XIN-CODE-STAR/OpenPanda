// SPDX-License-Identifier: AGPL-3.0-or-later

// Package conversation owns durable interactive modes and autonomous goals.
// All clients and controllers use the same conditional database transitions.
package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/util"
)

var ErrConflict = errors.New("conversation state changed; refresh before retrying")
var ErrWaiting = errors.New("associated work has not settled")
var ErrInvalid = errors.New("invalid conversation control")

type Goal struct {
	ID                  string `json:"id"`
	SessionID           string `json:"session_id"`
	Objective           string `json:"objective"`
	Status              string `json:"status"`
	Reason              string `json:"reason,omitempty"`
	Generation          int64  `json:"generation"`
	RoundID             string `json:"round_id,omitempty"`
	LeaseOwner          string `json:"-"`
	LeaseUntil          int64  `json:"-"`
	Rounds              int64  `json:"rounds"`
	Tokens              int64  `json:"tokens"`
	TokenBudget         *int64 `json:"token_budget,omitempty"`
	Blocker             string `json:"blocker,omitempty"`
	BlockedStreak       int    `json:"blocked_streak"`
	EmptyStreak         int    `json:"empty_streak"`
	CompletionRequested bool   `json:"completion_requested"`
	PausedByPlan        bool   `json:"paused_by_plan"`
	WaitingFor          string `json:"waiting_for,omitempty"`
	WaitingRound        string `json:"waiting_round,omitempty"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

type State struct {
	ModeGeneration   int64  `json:"mode_generation"`
	Planning         bool   `json:"planning"`
	ProposalVersion  int64  `json:"proposal_version"`
	Proposal         string `json:"proposal,omitempty"`
	ConfirmedVersion int64  `json:"confirmed_version"`
	Goal             *Goal  `json:"goal,omitempty"`
	// Implementation tracks an unsettled confirmed proposal. The service
	// enriches a running implementation with progress from the session operation.
	Implementation *Implementation `json:"implementation,omitempty"`
}

// Implementation is the read-only progress report of a plan round. Phase is a
// stable token the console localizes; Detail is the caller's already-readable
// qualifier and is never translated.
type Implementation struct {
	Round  string `json:"round"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type Store struct{ DB *sql.DB }

const goalColumns = "id,session_id,objective,status,reason,generation,round_id,lease_owner,lease_until,rounds,tokens,token_budget,blocker,blocked_streak,empty_streak,completion_requested,paused_by_plan,waiting_for,waiting_round,created_at,updated_at"

func scanGoal(row interface{ Scan(...any) error }) (*Goal, error) {
	g := new(Goal)
	err := row.Scan(&g.ID, &g.SessionID, &g.Objective, &g.Status, &g.Reason, &g.Generation, &g.RoundID, &g.LeaseOwner, &g.LeaseUntil, &g.Rounds, &g.Tokens, &g.TokenBudget, &g.Blocker, &g.BlockedStreak, &g.EmptyStreak, &g.CompletionRequested, &g.PausedByPlan, &g.WaitingFor, &g.WaitingRound, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (s *Store) Get(ctx context.Context, sid string) (State, error) {
	var st State
	// Planning is read but no longer gates the proposal: leaving the lens
	// must not hide the plan the user was reading. Only "already confirmed"
	// hides it, because from then on the transcript owns the plan.
	err := s.DB.QueryRowContext(ctx, "SELECT planning,proposal_version,proposal,confirmed_version,mode_generation FROM conversation_controls WHERE session_id=? AND deleted=0", sid).Scan(&st.Planning, &st.ProposalVersion, &st.Proposal, &st.ConfirmedVersion, &st.ModeGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	if st.ProposalVersion <= st.ConfirmedVersion {
		st.Proposal = ""
	}
	if st.ConfirmedVersion > 0 {
		var run Implementation
		err = s.DB.QueryRowContext(ctx, `SELECT round_id,status FROM conversation_plan_runs
WHERE session_id=? AND status IN ('pending','running','waiting_input')
ORDER BY CASE status WHEN 'running' THEN 0 WHEN 'waiting_input' THEN 1 ELSE 2 END,version LIMIT 1`, sid).Scan(&run.Round, &run.Phase)
		if err == nil {
			st.Implementation = &run
		} else if !errors.Is(err, sql.ErrNoRows) {
			return st, err
		}
	}
	g, err := scanGoal(s.DB.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM conversation_goals WHERE session_id=? AND status!='cleared'", sid))
	if err == nil {
		st.Goal = g
	} else if !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.Proposal = ProposalBody(st.Proposal)
	return st, nil
}

func ensure(ctx context.Context, tx *sql.Tx, sid string) error {
	if sid == "" {
		return errors.New("session identity required")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO conversation_controls(session_id) VALUES(?) ON CONFLICT DO NOTHING", sid)
	if err != nil {
		return err
	}
	var deleted bool
	if err = tx.QueryRowContext(ctx, "SELECT deleted FROM conversation_controls WHERE session_id=?", sid).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return ErrConflict
	}
	return nil
}

func event(ctx context.Context, tx *sql.Tx, sid, kind string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO conversation_events(session_id,kind,payload,created_at) VALUES(?,?,?,?)", sid, kind, string(b), time.Now().UnixMilli())
	return err
}

const retainedConversationEvents = 4096

// PruneEvents bounds the SSE notification backlog. An unsettled round's
// blocker report also deduplicates audits, so keep it until settlement.
func (s *Store) PruneEvents(ctx context.Context) error {
	var cutoff int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM conversation_events ORDER BY id DESC LIMIT 1 OFFSET ?`, retainedConversationEvents).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM conversation_events AS e WHERE e.id<=? AND (
 e.kind!='goal_blocked' OR NOT EXISTS (
 SELECT 1 FROM conversation_goal_rounds r
 WHERE r.round_id=json_extract(e.payload,'$') AND r.settled=0))`, cutoff)
	return err
}

func (s *Store) SetPlanning(ctx context.Context, sid string, enabled bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensure(ctx, tx, sid); err != nil {
		return err
	}
	// The proposal is deliberately not touched here. It used to be blanked
	// whenever planning flipped, which meant one click on the Plan chip
	// deleted the plan the user was about to approve. Leaving the lens keeps
	// the text; entering it again starts a new planning mode and retires the
	// previous mode's proposal (below), so a stale plan can never be
	// confirmed in the mode that replaced it.
	if _, err = tx.ExecContext(ctx, `UPDATE conversation_controls
SET mode_generation=mode_generation+CASE WHEN planning!=? THEN 1 ELSE 0 END,
    proposal=CASE WHEN ? AND planning=0 THEN '' ELSE proposal END,
    planning=?
WHERE session_id=?`, enabled, enabled, enabled, sid); err != nil {
		return err
	}
	if enabled {
		if err = pauseGoalForPlanning(ctx, tx, sid); err != nil {
			return err
		}
	}
	if err = event(ctx, tx, sid, "mode", map[string]bool{"planning": enabled}); err != nil {
		return err
	}
	return tx.Commit()
}

func pauseGoalForPlanning(ctx context.Context, tx *sql.Tx, sid string) error {
	_, err := tx.ExecContext(ctx, "UPDATE conversation_goals SET status='paused',paused_by_plan=1,updated_at=? WHERE session_id=? AND status='active'", time.Now().UnixMilli(), sid)
	return err
}

type PlanDraft struct {
	Version, Generation int64
	ContinuesGoal       bool
}

// BeginPlanningRequest retires the previous proposal as soon as a new turn
// starts. Versions remain monotonic and saved proposals stay in the archive.
//
// A revision may name the version it is revising. When it does, the proposal
// is only retired if that is still the current one, still unconfirmed *and*
// still on file: a tab that missed a newer proposal must not blank it with
// feedback about text nobody is reviewing; because confirming does not bump
// the version, a revision naming a confirmed proposal must not re-open a plan
// that is already being implemented; and a version whose text was already
// retired is not something to revise a second time. All three facts are
// checked in the same statement that retires the text, so the answer cannot go
// stale between the check and the write.
func (s *Store) BeginPlanningRequest(ctx context.Context, sid string, expected ...int64) (version, generation int64, err error) {
	var revision int64
	if len(expected) > 0 {
		revision = expected[0]
	}
	draft, err := s.BeginPlan(ctx, sid, false, revision)
	return draft.Version, draft.Generation, err
}

// BeginRevision enters planning mode and retires the named proposal in one
// transaction. A revision that arrives while the lens is off used to be two
// statements — SetPlanning(true), then BeginPlanningRequest — and a
// confirmation landing between them left the session in planning mode with the
// proposal blanked even though the revision was refused. Here the conditional
// UPDATE is the whole decision: if the proposal is no longer current, has been
// confirmed, or is already retired, nothing changes at all.
func (s *Store) BeginRevision(ctx context.Context, sid string, expected int64) (version, generation int64, err error) {
	if expected <= 0 {
		return 0, 0, ErrInvalid
	}
	draft, err := s.BeginPlan(ctx, sid, true, expected)
	return draft.Version, draft.Generation, err
}

// BeginPlan admits one read-only planning turn. An explicit request may enter
// planning, while a follow-up must still be in planning. A revision must
// name the current, unconfirmed proposal before any state can change.
func (s *Store) BeginPlan(ctx context.Context, sid string, enter bool, expected int64) (PlanDraft, error) {
	var draft PlanDraft
	if expected < 0 {
		return draft, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return draft, err
	}
	defer tx.Rollback()
	if enter {
		if err = ensure(ctx, tx, sid); err != nil {
			return draft, err
		}
	}
	query := `UPDATE conversation_controls
SET planning=1, mode_generation=mode_generation+1, proposal=''
WHERE session_id=? AND deleted=0 AND (planning=1 OR ?)`
	args := []any{sid, enter}
	if expected > 0 {
		query += " AND proposal_version=? AND confirmed_version<? AND proposal!=''"
		args = append(args, expected, expected)
	}
	err = tx.QueryRowContext(ctx, query+" RETURNING proposal_version,mode_generation", args...).Scan(&draft.Version, &draft.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanDraft{}, ErrConflict
	}
	if err != nil {
		return PlanDraft{}, err
	}
	if enter {
		if err = pauseGoalForPlanning(ctx, tx, sid); err != nil {
			return PlanDraft{}, err
		}
		if err = event(ctx, tx, sid, "mode", map[string]bool{"planning": true}); err != nil {
			return PlanDraft{}, err
		}
	}
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_goals WHERE session_id=? AND status='paused' AND paused_by_plan=1)", sid).Scan(&draft.ContinuesGoal); err != nil {
		return PlanDraft{}, err
	}
	if err = event(ctx, tx, sid, "plan_invalidated", draft.Version); err != nil {
		return PlanDraft{}, err
	}
	return draft, tx.Commit()
}

func (s *Store) SaveProposal(ctx context.Context, sid, text string, expected ...int64) (int64, error) {
	text = ProposalBody(text)
	if text == "" || len(text) > 200000 {
		return 0, fmt.Errorf("%w: complete proposal required, at most 200000 bytes", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var version int64
	query := "UPDATE conversation_controls SET proposal_version=proposal_version+1,proposal=? WHERE session_id=? AND planning=1 AND deleted=0"
	args := []any{text, sid}
	if len(expected) > 0 {
		query += " AND proposal_version=?"
		args = append(args, expected[0])
	}
	if len(expected) > 1 {
		query += " AND mode_generation=?"
		args = append(args, expected[1])
	}
	err = tx.QueryRowContext(ctx, query+" RETURNING proposal_version", args...).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_proposals(session_id,version,proposal,created_at) VALUES(?,?,?,?)", sid, version, text, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	if err = event(ctx, tx, sid, "plan", map[string]any{"version": version, "proposal": text}); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

// Confirm consumes exactly the current proposal once and queues it for
// implementation in this session.
//
// It no longer requires planning=1. The proposal is a document the user owns:
// toggling the lens off is a change of view, not a decision to discard it, so
// approving it after leaving Plan is allowed. Entering Plan again retires the
// text (SetPlanning), which is what keeps a superseded proposal from being
// confirmed by the mode that replaced it.
func (s *Store) Confirm(ctx context.Context, sid string, version int64) (string, error) {
	if version <= 0 {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var text string
	err = tx.QueryRowContext(ctx, "UPDATE conversation_controls SET planning=0,confirmed_version=? WHERE session_id=? AND deleted=0 AND proposal_version=? AND confirmed_version<? AND proposal!='' RETURNING proposal", version, sid, version, version).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	text = ProposalBody(text)
	if text == "" {
		return "", ErrConflict
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_plan_runs WHERE session_id=? AND status IN ('pending','running','waiting_input'))", sid).Scan(&busy); err != nil {
		return "", err
	}
	if busy {
		return "", ErrWaiting
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO conversation_plan_runs(session_id,version,proposal,goal_id,goal_generation) VALUES(?,?,?,COALESCE((SELECT id FROM conversation_goals WHERE session_id=? AND status='paused' AND paused_by_plan=1),''),COALESCE((SELECT generation FROM conversation_goals WHERE session_id=? AND status='paused' AND paused_by_plan=1),0))`, sid, version, text, sid, sid); err != nil {
		return "", err
	}
	if err = event(ctx, tx, sid, "plan_confirmed", map[string]int64{"version": version}); err != nil {
		return "", err
	}
	return text, tx.Commit()
}

func (s *Store) Create(ctx context.Context, sid, objective string, budget *int64) (*Goal, error) {
	created, _, err := s.ReplaceGoal(ctx, sid, objective, budget)
	return created, err
}

// ReplaceGoal returns the exact goal it revoked, including any round claimed
// before the transaction took the writer lock.
func (s *Store) ReplaceGoal(ctx context.Context, sid, objective string, budget *int64) (created, previous *Goal, err error) {
	objective = strings.TrimSpace(objective)
	if objective == "" || len(objective) > 200000 {
		return nil, nil, fmt.Errorf("%w: a nonempty objective of at most 200000 bytes is required", ErrInvalid)
	}
	if budget != nil && *budget <= 0 {
		return nil, nil, fmt.Errorf("%w: token budget must be positive", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err = ensure(ctx, tx, sid); err != nil {
		return nil, nil, err
	}
	previous, err = scanGoal(tx.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM conversation_goals WHERE session_id=? AND status!='cleared'", sid))
	if errors.Is(err, sql.ErrNoRows) {
		previous = nil
	} else if err != nil {
		return nil, nil, err
	}
	if err = sealGoalRound(ctx, tx, previous); err != nil {
		return nil, nil, err
	}
	now := time.Now().UnixMilli()
	id, err := util.UUIDv7()
	if err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_plan_runs SET status='cancelled' WHERE session_id=? AND goal_id!='' AND status IN ('pending','running','waiting_input')", sid); err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_goals SET status='cleared',generation=generation+1,round_id='',lease_owner='',lease_until=0,updated_at=? WHERE session_id=? AND status!='cleared'", now, sid); err != nil {
		return nil, nil, err
	}
	status := "active"
	var planning bool
	if err = tx.QueryRowContext(ctx, "SELECT planning FROM conversation_controls WHERE session_id=?", sid).Scan(&planning); err != nil {
		return nil, nil, err
	}
	if planning {
		status = "paused"
	}
	created, err = scanGoal(tx.QueryRowContext(ctx, "INSERT INTO conversation_goals(id,session_id,objective,status,token_budget,paused_by_plan,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) RETURNING "+goalColumns, id, sid, objective, status, budget, planning, now, now))
	if err != nil {
		return nil, nil, err
	}
	if err = event(ctx, tx, sid, "goal", map[string]string{"id": id, "status": status}); err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return created, previous, nil
}

func sealGoalRound(ctx context.Context, tx *sql.Tx, goal *Goal) error {
	if goal == nil || goal.RoundID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE ask_rounds SET status='stopped',closed_at=? WHERE round_id=? AND session_id=? AND status='open'", time.Now().Unix(), goal.RoundID, goal.SessionID)
	return err
}

// Control is a trusted user action. Models only get Report below, so they
// cannot change the objective, resume a paused goal or raise its budget.
func (s *Store) Control(ctx context.Context, sid, id, action, objective string, budget *int64) error {
	_, err := s.ControlGoal(ctx, sid, id, action, objective, budget, "")
	return err
}

// ControlGoal returns the pre-transition goal so the caller can signal its
// runner and cancel the right task tree. An operation-scoped stop also checks
// that the specified round is still the one the user is stopping.
func (s *Store) ControlGoal(ctx context.Context, sid, id, action, objective string, budget *int64, expectedRound string) (*Goal, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = ensure(ctx, tx, sid); err != nil {
		return nil, err
	}
	g, err := scanGoal(tx.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM conversation_goals WHERE session_id=? AND id=? AND status!='cleared'", sid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if expectedRound != "" && g.RoundID != expectedRound {
		var linked bool
		err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_plan_runs WHERE session_id=? AND round_id=? AND goal_id=? AND goal_generation=? AND status IN ('running','waiting_input'))", sid, expectedRound, id, g.Generation).Scan(&linked)
		if err != nil {
			return nil, err
		}
		if !linked {
			return nil, ErrConflict
		}
	}
	previous := *g
	switch action {
	case "pause":
		g.Status = "paused"
		g.PausedByPlan = false
	case "stop":
		g.Status = "paused"
		g.Generation++
		g.PausedByPlan = false
	case "resume":
		resumeBudget := g.TokenBudget
		if budget != nil {
			resumeBudget = budget
		}
		if resumeBudget != nil && g.Tokens >= *resumeBudget {
			return nil, fmt.Errorf("%w: explicit budget increase required", ErrInvalid)
		}
		// Explicitly continuing the goal also leaves planning. Fence any
		// in-flight proposal producer; merely leaving planning never resumes.
		if _, err = tx.ExecContext(ctx, "UPDATE conversation_controls SET mode_generation=mode_generation+CASE WHEN planning=1 THEN 1 ELSE 0 END,planning=0 WHERE session_id=?", sid); err != nil {
			return nil, err
		}
		if err = event(ctx, tx, sid, "mode", map[string]bool{"planning": false}); err != nil {
			return nil, err
		}
		g.Status = "active"
		g.BlockedStreak = 0
		g.Blocker = ""
		g.EmptyStreak = 0
		g.CompletionRequested = false
		g.PausedByPlan = false
	case "clear":
		g.Status = "cleared"
		g.Generation++
		g.RoundID, g.LeaseOwner, g.LeaseUntil = "", "", 0
	case "edit":
		objective = strings.TrimSpace(objective)
		if objective == "" || len(objective) > 200000 {
			return nil, fmt.Errorf("%w: a nonempty objective of at most 200000 bytes is required", ErrInvalid)
		}
		g.Objective = objective
		g.Generation++
		g.CompletionRequested = false
		g.BlockedStreak = 0
		g.Blocker = ""
		g.EmptyStreak = 0
		if g.Status == "complete" {
			// The old audit cannot certify a changed objective. Saving an
			// edit requires an explicit resume before doing more work.
			g.Status = "paused"
		}
	default:
		return nil, fmt.Errorf("%w: unknown goal control", ErrInvalid)
	}
	if budget != nil {
		if *budget <= 0 {
			return nil, fmt.Errorf("%w: token budget must be positive", ErrInvalid)
		}
		g.TokenBudget = budget
	}
	if g.Status == "active" && g.TokenBudget != nil && g.Tokens >= *g.TokenBudget {
		g.Status = "budget_limited"
	}
	// A pause or resume changes execution eligibility. The pending question
	// still belongs to the same objective and generation.
	if g.Generation != previous.Generation {
		g.WaitingFor, g.WaitingRound = "", ""
	}
	if g.WaitingFor == "" {
		g.Reason = ""
	}
	if action != "resume" {
		if err = sealGoalRound(ctx, tx, &previous); err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE conversation_goals SET objective=?,status=?,generation=?,token_budget=?,blocked_streak=?,blocker=?,empty_streak=?,completion_requested=?,paused_by_plan=?,round_id=?,lease_owner=?,lease_until=?,waiting_for=?,waiting_round=?,reason=?,updated_at=? WHERE id=?", g.Objective, g.Status, g.Generation, g.TokenBudget, g.BlockedStreak, g.Blocker, g.EmptyStreak, g.CompletionRequested, g.PausedByPlan, g.RoundID, g.LeaseOwner, g.LeaseUntil, g.WaitingFor, g.WaitingRound, g.Reason, time.Now().UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	if action == "clear" || action == "stop" || action == "edit" {
		if _, err = tx.ExecContext(ctx, "UPDATE conversation_plan_runs SET status='cancelled' WHERE goal_id=? AND status IN ('pending','running','waiting_input')", id); err != nil {
			return nil, err
		}
	}
	if err = event(ctx, tx, sid, "goal", g); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &previous, nil
}

func (s *Store) DeleteSession(ctx context.Context, sid string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_plan_runs SET status='cancelled' WHERE session_id=? AND status IN ('pending','running','waiting_input')", sid); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_controls(session_id,deleted) VALUES(?,1) ON CONFLICT(session_id) DO UPDATE SET deleted=1", sid); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_goals SET status='cleared',generation=generation+1,round_id='',lease_owner='',lease_until=0 WHERE session_id=?", sid); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ask_round_removed_tasks
		SET title='',result_json='{}',plan_id='',stage_id='',plan_final=0,updated_at=0
		WHERE round_id IN (SELECT round_id FROM ask_rounds WHERE session_id=?)
		AND (title!='' OR result_json!='{}' OR plan_id!='' OR stage_id!='' OR updated_at!=0)`, sid); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Runnable(ctx context.Context) ([]*Goal, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+goalColumns+" FROM conversation_goals WHERE status='active' OR round_id!=''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Claim creates the round and its fencing token atomically. A lease expiry
// never alone authorizes replay: a previous round must first be reconciled.
func (s *Store) Claim(ctx context.Context, g *Goal, owner, locale string) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	round, err := util.UUIDv7()
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversation_goals SET round_id=?,lease_owner=?,lease_until=?,rounds=rounds+1,updated_at=? WHERE id=? AND generation=? AND status='active' AND round_id='' AND lease_until<? AND completion_requested=0 AND waiting_for='' AND (token_budget IS NULL OR tokens<token_budget) AND NOT EXISTS(SELECT 1 FROM ask_rounds a WHERE a.session_id=conversation_goals.session_id AND a.status='open' AND a.lease_until>=CAST(strftime('%s','now') AS INTEGER)) AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.session_id=conversation_goals.session_id AND t.state NOT IN ('done','failed','cancelled','expired')) AND NOT EXISTS(SELECT 1 FROM conversation_plan_runs p WHERE p.session_id=conversation_goals.session_id AND p.status IN ('pending','running','waiting_input')) AND EXISTS(SELECT 1 FROM conversation_controls WHERE session_id=conversation_goals.session_id AND planning=0 AND deleted=0)`, round, owner, now+60000, now, g.ID, g.Generation, now)
	if err != nil {
		return "", err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO ask_rounds(round_id,session_id,locale,lease_until,created_at) VALUES(?,?,?,?,?)", round, g.SessionID, locale, time.Now().Unix()+60, time.Now().Unix()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_goal_rounds(round_id,goal_id,generation) VALUES(?,?,?)", round, g.ID, g.Generation); err != nil {
		return "", err
	}
	if err = event(ctx, tx, g.SessionID, "goal_round", map[string]string{"goal_id": g.ID, "round_id": round}); err != nil {
		return "", err
	}
	return round, tx.Commit()
}

func (s *Store) Renew(ctx context.Context, g *Goal, owner string) error {
	result, err := s.DB.ExecContext(ctx, "UPDATE conversation_goals SET lease_until=? WHERE id=? AND generation=? AND round_id=? AND lease_owner=? AND status IN ('active','paused') AND EXISTS(SELECT 1 FROM ask_rounds WHERE round_id=conversation_goals.round_id AND status='open')", time.Now().UnixMilli()+60000, g.ID, g.Generation, g.RoundID, owner)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

type InputWait struct {
	RoundID    string
	owner      string
	plan       int64
	goal       string
	generation int64
}

func (s *Store) PublishGoalQuestion(ctx context.Context, g *Goal, persist func() error) error {
	return s.publishQuestion(ctx, g.RoundID, "closed", persist, `UPDATE conversation_goals
		SET waiting_for='input',waiting_round=?,reason='waiting for user input',completion_requested=0,updated_at=?
		WHERE id=? AND generation=? AND round_id=? AND status IN ('active','paused','budget_limited')`,
		g.RoundID, time.Now().UnixMilli(), g.ID, g.Generation, g.RoundID)
}

func (s *Store) PublishPlanQuestion(ctx context.Context, sid, round string, version int64, persist func() error) error {
	return s.publishQuestion(ctx, round, "stopped", persist, `UPDATE conversation_plan_runs SET status='waiting_input'
		WHERE session_id=? AND version=? AND round_id=? AND status='running'`, sid, version, round)
}

// Holding the writer transaction across transcript publication makes the
// dispatch seal and the waiting question available together to a replying ask.
// persist must only write the session file, not a database checkpoint.
func (s *Store) publishQuestion(ctx context.Context, round, status string, persist func() error, update string, args ...any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, update, args...)
	if err = oneInputRow(result, err); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE ask_rounds SET status=?,closed_at=? WHERE round_id=? AND status='open'`,
		status, time.Now().Unix(), round)
	if err = oneInputRow(result, err); err != nil {
		return err
	}
	if persist != nil {
		if err = persist(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReserveInput assigns the latest question, or the named question, to one
// replying ask. Selection and ownership serialize on the SQLite writer.
func (s *Store) ReserveInput(ctx context.Context, sid, replyTo string) (InputWait, error) {
	var wait InputWait
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return wait, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `
		SELECT w.round_id,w.version,w.goal_id,w.generation
		FROM (
			SELECT round_id,version,'' AS goal_id,0 AS generation FROM conversation_plan_runs
			WHERE session_id=? AND status='waiting_input'
			UNION ALL
			SELECT waiting_round,0,id,generation FROM conversation_goals
			WHERE session_id=? AND waiting_for='input' AND status IN ('active','paused','budget_limited')
		) w JOIN ask_rounds a ON a.round_id=w.round_id
		WHERE a.session_id=? AND a.status!='open' AND (?='' OR w.round_id=?)
		AND NOT EXISTS(SELECT 1 FROM conversation_controls WHERE session_id=a.session_id AND (planning=1 OR deleted=1))
		ORDER BY a.closed_at DESC,w.round_id DESC LIMIT 1`, sid, sid, sid, replyTo, replyTo).
		Scan(&wait.RoundID, &wait.plan, &wait.goal, &wait.generation)
	if errors.Is(err, sql.ErrNoRows) {
		if replyTo != "" {
			return wait, ErrConflict
		}
		return wait, nil
	}
	if err != nil {
		return wait, err
	}
	wait.owner, err = util.UUIDv7()
	if err != nil {
		return wait, err
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_input_replies(round_id,owner,lease_until) VALUES(?,?,?)
		ON CONFLICT(round_id) DO UPDATE SET owner=excluded.owner,lease_until=excluded.lease_until
		WHERE conversation_input_replies.lease_until<?`, wait.RoundID, wait.owner, now.Add(core.AskRoundLease).UnixMilli(), now.UnixMilli())
	if err = oneInputRow(result, err); err != nil {
		return wait, err
	}
	return wait, tx.Commit()
}

func (s *Store) RenewInput(ctx context.Context, wait InputWait) error {
	now := time.Now()
	result, err := s.DB.ExecContext(ctx, `UPDATE conversation_input_replies SET lease_until=?
		WHERE round_id=? AND owner=? AND lease_until>=?`, now.Add(core.AskRoundLease).UnixMilli(), wait.RoundID, wait.owner, now.UnixMilli())
	return oneInputRow(result, err)
}

func (s *Store) ReleaseInput(ctx context.Context, wait InputWait) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM conversation_input_replies WHERE round_id=? AND owner=?", wait.RoundID, wait.owner)
	return err
}

func (s *Store) ResolveInput(ctx context.Context, sid string, wait InputWait) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM conversation_input_replies WHERE round_id=? AND owner=? AND lease_until>=?`,
		wait.RoundID, wait.owner, time.Now().UnixMilli())
	if err = oneInputRow(result, err); err != nil {
		return err
	}
	if wait.plan > 0 {
		result, err = tx.ExecContext(ctx, `UPDATE conversation_plan_runs SET status='pending',round_id=''
			WHERE session_id=? AND version=? AND round_id=? AND status='waiting_input'`, sid, wait.plan, wait.RoundID)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE conversation_goals SET waiting_for='',waiting_round='',reason='',updated_at=?
		WHERE session_id=? AND id=? AND generation=? AND waiting_round=? AND waiting_for='input'`,
			time.Now().UnixMilli(), sid, wait.goal, wait.generation, wait.RoundID)
	}
	if err = oneInputRow(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

func oneInputRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrConflict
	}
	return err
}

func (s *Store) Report(ctx context.Context, g *Goal, status, reason string) error {
	if status == "complete" {
		progress, err := core.NewTaskStore(s.DB, nil).AskRoundProgress(ctx, g.SessionID, g.RoundID)
		if err != nil {
			return err
		}
		for _, task := range progress.Tasks {
			if task.State != core.StateDone && task.State != core.StateFailed && task.State != core.StateCancelled && task.State != core.StateExpired || progress.PendingRecovery[task.TaskID] {
				return ErrWaiting
			}
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanGoal(tx.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM conversation_goals WHERE id=? AND generation=? AND round_id=? AND status IN ('active','blocked')", g.ID, g.Generation, g.RoundID))
	if err != nil {
		return ErrConflict
	}
	switch status {
	case "complete":
		if current.Status != "active" {
			return ErrConflict
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, core.AskRoundMembersSQL+` SELECT EXISTS(SELECT 1 FROM tasks JOIN members ON task_id=members.id WHERE state NOT IN ('done','failed','cancelled','expired'))`, g.RoundID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrWaiting
		}
		if strings.TrimSpace(reason) == "" {
			return errors.New("completion audit of the full objective is required")
		}
		current.CompletionRequested = true
	case "blocked":
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return errors.New("a concrete blocker is required")
		}
		// Later evidence of missing work invalidates an earlier completion audit,
		// including when this round already reported the same blocker.
		current.CompletionRequested = false
		// One report per round, regardless of repeated tool calls/notifications.
		var reported bool
		err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_events WHERE session_id=? AND kind='goal_blocked' AND payload=?)", g.SessionID, fmt.Sprintf("%q", g.RoundID)).Scan(&reported)
		if err != nil {
			return err
		}
		if !reported {
			if current.Blocker == reason {
				current.BlockedStreak++
			} else {
				current.Blocker = reason
				current.BlockedStreak = 1
			}
			if current.BlockedStreak >= 3 {
				current.Status = "blocked"
			}
			if err = event(ctx, tx, g.SessionID, "goal_blocked", g.RoundID); err != nil {
				return err
			}
		}
	default:
		return errors.New("model may only report complete or blocked")
	}
	_, err = tx.ExecContext(ctx, "UPDATE conversation_goals SET status=?,reason=?,blocker=?,blocked_streak=?,completion_requested=?,updated_at=? WHERE id=? AND generation=?", current.Status, reason, current.Blocker, current.BlockedStreak, current.CompletionRequested, time.Now().UnixMilli(), g.ID, g.Generation)
	if err != nil {
		return err
	}
	if err = event(ctx, tx, g.SessionID, "goal", current); err != nil {
		return err
	}
	return tx.Commit()
}

// Finish records request-local usage monotonically. A remote controller may
// settle a stopped round before its request finalizer returns, so late usage
// must also charge the settled ledger's immutable goal identity, once.
func (s *Store) Finish(ctx context.Context, round string, tokens int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var goal string
	var previous int64
	var settled bool
	// Acquire the write lock before reading, across independent SQLite handles.
	err = tx.QueryRowContext(ctx, "UPDATE conversation_goal_rounds SET entry_tokens=entry_tokens WHERE round_id=? RETURNING goal_id,entry_tokens,settled", round).Scan(&goal, &previous, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Ordinary interactive asks have no goal ledger.
	}
	if err != nil {
		return err
	}
	if tokens <= previous {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_goal_rounds SET entry_tokens=? WHERE round_id=?", tokens, round); err != nil {
		return err
	}
	if settled {
		delta := tokens - previous
		var sid string
		err = tx.QueryRowContext(ctx, `UPDATE conversation_goals SET tokens=tokens+?,status=CASE WHEN status='active' AND token_budget IS NOT NULL AND tokens+?>=token_budget THEN 'budget_limited' ELSE status END,updated_at=? WHERE id=? RETURNING session_id`, delta, delta, time.Now().UnixMilli(), goal).Scan(&sid)
		if err != nil {
			return err
		}
		if err = event(ctx, tx, sid, "goal_settled", map[string]any{"id": goal, "tokens": delta}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Settle(ctx context.Context, g *Goal, taskTokens int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tokens int64
	// Metrics update this ledger in the same transaction as each record.
	err = tx.QueryRowContext(ctx, `UPDATE conversation_goal_rounds SET settled=1,task_tokens=MAX(?3,task_tokens)
		WHERE round_id=?1 AND goal_id=?2 AND settled=0
		AND NOT EXISTS(`+core.AskRoundMembersSQL+` SELECT 1 FROM tasks JOIN members ON task_id=members.id WHERE state NOT IN ('done','failed','cancelled','expired'))
		RETURNING entry_tokens+task_tokens`, g.RoundID, g.ID, taskTokens).Scan(&tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	// Charge the immutable goal identity even after revocation. Only its current
	// round may clear execution state or finish the goal; implementation rounds
	// share this ledger without taking over the autonomous round pointer.
	_, err = tx.ExecContext(ctx, `UPDATE conversation_goals SET tokens=tokens+?,
		status=CASE WHEN status='active' AND round_id=?2 AND completion_requested=1 AND waiting_for='' THEN 'complete'
			WHEN status='active' AND token_budget IS NOT NULL AND tokens+?1>=token_budget THEN 'budget_limited' ELSE status END,
		completion_requested=CASE WHEN round_id=?2 AND (status!='active' OR waiting_for!='') THEN 0 ELSE completion_requested END,
		blocked_streak=CASE WHEN round_id!=?2 OR EXISTS(SELECT 1 FROM conversation_events WHERE session_id=?3 AND kind='goal_blocked' AND payload=?4) THEN blocked_streak ELSE 0 END,
		lease_owner=CASE WHEN round_id=?2 THEN '' ELSE lease_owner END,
		lease_until=CASE WHEN round_id=?2 THEN 0 ELSE lease_until END,
		round_id=CASE WHEN round_id=?2 THEN '' ELSE round_id END,updated_at=?5 WHERE id=?6`,
		tokens, g.RoundID, g.SessionID, fmt.Sprintf("%q", g.RoundID), time.Now().UnixMilli(), g.ID)
	if err != nil {
		return err
	}
	if err = event(ctx, tx, g.SessionID, "goal_settled", map[string]any{"id": g.ID, "tokens": tokens}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Hold(ctx context.Context, g *Goal, status, reason string, empty bool) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE conversation_goals SET status=?,reason=?,empty_streak=CASE WHEN ? THEN empty_streak+1 ELSE 0 END,updated_at=? WHERE id=? AND generation=? AND round_id=? AND status='active'", status, reason, empty, time.Now().UnixMilli(), g.ID, g.Generation, g.RoundID)
	return err
}
