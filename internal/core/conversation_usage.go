// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Record usage on the ledger independently of the board row. Settlement
// charges accumulated usage, and metrics arriving afterward charge the goal
// in the same transaction as the ledger increment.
func chargeGoalMetric(ctx context.Context, tx *sql.Tx, taskID string, tokens int64) error {
	rows, err := tx.QueryContext(ctx, askRoundAncestrySQL+`
 UPDATE conversation_goal_rounds SET task_tokens=task_tokens+?
 WHERE round_id IN (SELECT id FROM rounds)
 RETURNING goal_id,settled`, taskID, tokens)
	if err != nil {
		return err
	}
	deltas := map[string]int64{}
	for rows.Next() {
		var id string
		var settled bool
		if err = rows.Scan(&id, &settled); err != nil {
			rows.Close()
			return err
		}
		if settled {
			deltas[id] += tokens
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for id, delta := range deltas {
		var sid string
		err = tx.QueryRowContext(ctx, `UPDATE conversation_goals SET tokens=tokens+?,
 status=CASE WHEN status='active' AND token_budget IS NOT NULL AND tokens+?>=token_budget THEN 'budget_limited' ELSE status END,updated_at=?
 WHERE id=? RETURNING session_id`, delta, delta, time.Now().UnixMilli(), id).Scan(&sid)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"id": id, "tokens": delta})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_events(session_id,kind,payload,created_at) VALUES(?,'goal_settled',?,?)", sid, string(payload), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}
