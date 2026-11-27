// SPDX-License-Identifier: AGPL-3.0-or-later

package askengine

import (
	"context"
	"fmt"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/i18n"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

// RenderPlanAudit keeps event IDs alongside the executor's synthesis. A delivery
// to a mailbox and an acknowledgement by an executor are different evidence.
func RenderPlanAudit(ctx context.Context, loc i18n.Locale, store *core.TaskStore, planID string) (string, error) {
	if planID == "" {
		return "", nil
	}
	audit, err := store.PlanAudit(ctx, planID)
	if err != nil || len(audit.Decisions) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString(i18n.T(loc, "ask.plan.auditTitle"))
	b.WriteByte('\n')
	for _, d := range audit.Decisions {
		fmt.Fprintf(&b, "\n- #%d · %s", d.EventID, d.Source)
		if d.RequestedTarget != "" && d.RequestedTarget != d.Target {
			fmt.Fprintf(&b, " → %s", d.RequestedTarget)
		}
		if d.Target != "" {
			fmt.Fprintf(&b, " → %s", d.Target)
		}
		fmt.Fprintf(&b, " · %s (%s)", d.Kind, d.Outcome)
		if d.ReadEventID > 0 {
			fmt.Fprintf(&b, " · %s #%d", i18n.T(loc, "ask.plan.auditRead"), d.ReadEventID)
		}
		if d.State != "" {
			fmt.Fprintf(&b, " · %s", d.State)
		}
		if d.Verification != "" {
			fmt.Fprintf(&b, " · %s", d.Verification)
		}
		for _, text := range []string{d.Reason, d.Instruction, d.Detail} {
			if text != "" {
				b.WriteString(" · ")
				b.WriteString(strings.Join(strings.Fields(text), " "))
			}
		}
	}
	if audit.Omitted > 0 {
		b.WriteString("\n\n")
		b.WriteString(i18n.Tf(loc, "ask.plan.auditEarlier", "n", fmt.Sprint(audit.Omitted)))
	}
	return b.String(), nil
}

// Prepare the final transcript before taking the run settlement's write lock.
func (s *ConversationService) planDeliveryTurns(ctx context.Context, sid, round string, successful bool, withdrawn map[string]bool) ([]sessions.Turn, error) {
	rows, err := s.store.DB.QueryContext(ctx, `SELECT t.plan_id,t.stage_id,t.task_id,t.state,COALESCE(t.spec_json,''),COALESCE(t.result_json,'')
 FROM ask_round_tasks a JOIN tasks t ON t.task_id=a.task_id
 WHERE a.round_id=?1 AND t.session_id=?2 AND t.plan_id!='' AND t.parent_id=''
 UNION ALL
 SELECT m.plan_id,m.stage_id,m.task_id,m.state,CASE WHEN m.plan_final THEN '{"plan_final":true}' ELSE '{}' END,m.result_json
 FROM ask_round_removed_tasks m WHERE m.round_id=?1 AND m.plan_id!='' AND m.parent_id=''
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.task_id=m.task_id)
 ORDER BY 1,2,3`, round, sid)
	if err != nil {
		return nil, err
	}
	var ids []string
	boards := make(map[string][]core.Task)
	for rows.Next() {
		var task core.Task
		if err := rows.Scan(&task.PlanID, &task.StageID, &task.TaskID, &task.State, &task.SpecJSON, &task.ResultJSON); err != nil {
			rows.Close()
			return nil, err
		}
		if len(boards[task.PlanID]) == 0 {
			ids = append(ids, task.PlanID)
		}
		boards[task.PlanID] = append(boards[task.PlanID], task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	st := s.sessionStore()
	sess, err := st.Get(sid)
	if err != nil {
		return nil, err
	}
	var turns []sessions.Turn
	store := s.engine.TaskStore()
	if withdrawn == nil {
		withdrawn = map[string]bool{}
	}
	for _, id := range ids {
		outcome := &Result{PlanID: id, OK: true}
		if err := s.engine.planBoardVerdictWithWithdrawn(ctx, store, outcome, boards[id], withdrawn); err != nil {
			return nil, err
		}
		// The live board may no longer contain a failed stage whose verdict
		// is still recorded in the round. Don't report its partial result as
		// a successful implementation.
		if outcome.OK && !successful {
			continue
		}
		text := outcome.Answer
		kind := "answer"
		if !outcome.OK {
			kind = "error"
			if text == "" {
				text = outcome.Stderr
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		seen := false
		for _, turn := range sess.Turns {
			if turn.IdempotencyKey == "plan:assistant:"+round && turn.Text == text {
				seen = true
				break
			}
		}
		if !seen {
			turns = append(turns, sessions.Turn{Role: "assistant", Kind: kind, Text: text, OperationID: round, RoundID: round, IdempotencyKey: "plan:delivery:" + round + ":" + id})
		}
	}
	return turns, nil
}
