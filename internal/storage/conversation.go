// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

func migrateV76(tx MigrationExec) error {
	exists, err := tableExistsTx(tx, "delegation_metrics")
	if err != nil {
		return err
	}
	if exists {
		if err = addColumnIfMissingTx(tx, "delegation_metrics", "usage_source", "TEXT NOT NULL DEFAULT 'task'"); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE TABLE conversation_proposals (session_id TEXT NOT NULL, version INTEGER NOT NULL, proposal TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(session_id,version))`,
		`CREATE TABLE conversation_plan_runs (session_id TEXT NOT NULL, version INTEGER NOT NULL, proposal TEXT NOT NULL,
 round_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
 goal_id TEXT NOT NULL DEFAULT '', goal_generation INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(session_id,version))`,
		`CREATE TABLE conversation_controls (
 session_id TEXT PRIMARY KEY, planning INTEGER NOT NULL DEFAULT 0, mode_generation INTEGER NOT NULL DEFAULT 0,
 proposal_version INTEGER NOT NULL DEFAULT 0, proposal TEXT NOT NULL DEFAULT '',
 confirmed_version INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE conversation_goals (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, objective TEXT NOT NULL,
 status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', generation INTEGER NOT NULL DEFAULT 1,
 round_id TEXT NOT NULL DEFAULT '', lease_owner TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0,
 rounds INTEGER NOT NULL DEFAULT 0, tokens INTEGER NOT NULL DEFAULT 0, token_budget INTEGER,
 blocker TEXT NOT NULL DEFAULT '', blocked_streak INTEGER NOT NULL DEFAULT 0,
 empty_streak INTEGER NOT NULL DEFAULT 0, completion_requested INTEGER NOT NULL DEFAULT 0,
 paused_by_plan INTEGER NOT NULL DEFAULT 0, waiting_for TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE UNIQUE INDEX conversation_goal_current ON conversation_goals(session_id) WHERE status!='cleared'`,
		`CREATE TABLE conversation_goal_rounds (
 round_id TEXT PRIMARY KEY REFERENCES ask_rounds(round_id), goal_id TEXT NOT NULL REFERENCES conversation_goals(id),
 generation INTEGER NOT NULL, entry_tokens INTEGER NOT NULL DEFAULT 0, task_tokens INTEGER NOT NULL DEFAULT 0,
 settled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE conversation_events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
 kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE INDEX conversation_events_session ON conversation_events(session_id,id)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func migrateV77(tx MigrationExec) error {
	// Keep the round ledger for settlement and late usage. The cleared goal
	// itself no longer needs to hold the controller's active-round pointer.
	_, err := tx.Exec(`UPDATE conversation_goals SET round_id='',lease_owner='',lease_until=0 WHERE status='cleared' AND round_id!=''`)
	return err
}

func migrateV80(tx MigrationExec) error {
	// Task rows can be cleared from the board after they settle. Keep evidence
	// that this round published work for as long as the round itself exists.
	if _, err := tx.Exec(`ALTER TABLE ask_rounds ADD COLUMN dispatched INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE ask_rounds SET dispatched=1
		WHERE reported_version!=''
		OR EXISTS(SELECT 1 FROM ask_round_tasks WHERE round_id=ask_rounds.round_id)`)
	return err
}

func migrateV81(tx MigrationExec) error {
	for _, q := range []string{
		`ALTER TABLE conversation_goals ADD COLUMN waiting_round TEXT NOT NULL DEFAULT ''`,
		`UPDATE conversation_goals SET waiting_round=COALESCE((
			SELECT r.round_id FROM conversation_goal_rounds r JOIN ask_rounds a ON a.round_id=r.round_id
			WHERE r.goal_id=conversation_goals.id AND r.generation=conversation_goals.generation AND a.status='closed'
			AND NOT EXISTS(SELECT 1 FROM conversation_plan_runs p WHERE p.round_id=r.round_id)
			ORDER BY a.closed_at DESC,a.round_id DESC LIMIT 1),'')
			WHERE waiting_for='input'`,
		`UPDATE conversation_goals SET completion_requested=0
			WHERE waiting_for='input' OR (status IN ('active','paused') AND round_id='')`,
		`UPDATE conversation_goals SET status=CASE WHEN token_budget IS NOT NULL AND tokens>=token_budget THEN 'budget_limited' ELSE 'active' END
			WHERE status='complete' AND waiting_for='input'`,
		`CREATE TABLE ask_round_removed_tasks (
			round_id TEXT NOT NULL REFERENCES ask_rounds(round_id) ON DELETE CASCADE,
			task_id TEXT NOT NULL, parent_id TEXT NOT NULL, state TEXT NOT NULL, withdrawn INTEGER NOT NULL,
			PRIMARY KEY(round_id,task_id))`,
		`CREATE INDEX ask_round_removed_tasks_task ON ask_round_removed_tasks(task_id)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	exists, err := tableExistsTx(tx, "tasks")
	if err != nil || !exists {
		return err
	}
	for _, q := range []string{
		// A task may be removed before its controller sees the result. Keep only
		// the verdict and its round identity. This also preserves attribution for
		// a metric arriving after its task or an ancestor has been removed.
		`CREATE TRIGGER remember_removed_round_task BEFORE DELETE ON tasks BEGIN
			INSERT OR REPLACE INTO ask_round_removed_tasks(round_id,task_id,parent_id,state,withdrawn)
			SELECT round_id,OLD.task_id,COALESCE(OLD.parent_id,''),
				CASE WHEN OLD.state IN ('done','failed','cancelled','expired') THEN OLD.state ELSE 'cancelled' END,
				CASE WHEN OLD.state='cancelled' AND OLD.plan_id!='' AND OLD.remote=0 THEN COALESCE((
					SELECT json_extract(e.data_json,'$.plan_withdrawn')=1 AND json_type(e.data_json,'$.decision_id')='text'
					FROM task_events e WHERE e.task_id=OLD.task_id AND e.type='cancel' ORDER BY e.id DESC LIMIT 1),0) ELSE 0 END
			FROM (
				WITH RECURSIVE ancestors(id) AS (
					SELECT OLD.task_id
					UNION SELECT t.parent_id FROM tasks t JOIN ancestors a ON t.task_id=a.id WHERE t.parent_id!=''
				)
				SELECT m.round_id FROM ask_round_tasks m JOIN ancestors a ON a.id=m.task_id
				UNION
				SELECT m.round_id FROM ask_round_removed_tasks m JOIN ancestors a ON a.id=m.task_id
				WHERE NOT EXISTS(SELECT 1 FROM tasks t WHERE t.task_id=a.id)
			);
		END`,
		// Settled ledgers already contain their task usage. Bring only the open
		// ledgers forward before new metrics start updating them at write time.
		`UPDATE conversation_goal_rounds AS r SET task_tokens=MAX(task_tokens,(
			WITH RECURSIVE members(id) AS (
				SELECT task_id FROM ask_round_tasks WHERE round_id=r.round_id
				UNION SELECT t.task_id FROM tasks t JOIN members m ON t.parent_id=m.id
			)
			SELECT COALESCE(SUM(MAX(0,COALESCE(tokens,0))),0) FROM delegation_metrics
			WHERE task_id IN (SELECT id FROM members) AND usage_source!='ask'))
			WHERE settled=0`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func migrateV82(tx MigrationExec) error {
	for _, q := range []string{
		`CREATE TABLE conversation_input_replies (
			round_id TEXT PRIMARY KEY REFERENCES ask_rounds(round_id) ON DELETE CASCADE,
			owner TEXT NOT NULL, lease_until INTEGER NOT NULL)`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN plan_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN stage_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN plan_final INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN result_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE ask_round_removed_tasks ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	exists, err := tableExistsTx(tx, "tasks")
	if err != nil || !exists {
		return err
	}
	// The v81 trigger inserts the verdict while the task and its events
	// are still available. Retain the final delivery and the bounded task
	// evidence that the next goal round would have read from the board.
	_, err = tx.Exec(`CREATE TRIGGER remember_removed_round_evidence AFTER INSERT ON ask_round_removed_tasks
		WHEN (EXISTS(SELECT 1 FROM conversation_goal_rounds WHERE round_id=NEW.round_id)
			OR EXISTS(SELECT 1 FROM conversation_plan_runs WHERE round_id=NEW.round_id AND status IN ('running','waiting_input')))
			AND NOT EXISTS(SELECT 1 FROM ask_rounds r JOIN conversation_controls c ON c.session_id=r.session_id
				WHERE r.round_id=NEW.round_id AND c.deleted=1)
		BEGIN
			UPDATE ask_round_removed_tasks
			SET (plan_id,stage_id,plan_final,title,result_json,updated_at) = (
				SELECT COALESCE(plan_id,''),COALESCE(stage_id,''),plan_final,substr(title,1,150),
					json_object('stdout',COALESCE(CASE WHEN plan_final THEN json_extract(result,'$.stdout')
						ELSE substr(json_extract(result,'$.stdout'),1,600) END,''),
						'verdict_reason',COALESCE(substr(json_extract(result,'$.verdict_reason'),1,600),''),
						'stderr',COALESCE(NULLIF(json_extract(result,'$.stderr'),''),(
							SELECT COALESCE(NULLIF(json_extract(e.data_json,'$.failed'),''),
								NULLIF(json_extract(e.data_json,'$.stderr'),''),NULLIF(json_extract(e.data_json,'$.reason'),''))
							FROM task_events e WHERE e.task_id=NEW.task_id AND e.type IN ('result','review')
							AND json_valid(e.data_json) AND state IN ('failed','cancelled','expired')
							ORDER BY e.id DESC LIMIT 1),''))
					,updated_at
				FROM (SELECT *, COALESCE(CASE WHEN json_valid(spec_json) THEN json_extract(spec_json,'$.plan_final')=1 END,0) AS plan_final,
					CASE WHEN json_valid(result_json) THEN result_json ELSE '{}' END AS result
					FROM tasks WHERE task_id=NEW.task_id)
			)
			WHERE round_id=NEW.round_id AND task_id=NEW.task_id;
		END`)
	return err
}
