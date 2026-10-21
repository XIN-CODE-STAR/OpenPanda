// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/Xustalis/OpenPanda/internal/scheduler"
)

// The inter-task message layer (design: sibling-board). Tasks that share a
// scope — the stages of one plan, or a parent and its delegate children — may
// post short notes a sibling reads at its next supervision round instead of
// waiting for a terminal result. The board keeps the per-sender slot semantic
// of a temporal message buffer: one slot per (sender, recipient) per scope,
// newest wins, so a reader's prompt shows every member's latest state rather
// than whichever lines a FIFO happened to keep.
//
// Topology: the scope's OWNER node holds the authoritative member roster
// (plan stages for plan:<id>, the child set plus the parent itself for
// task:<id>) and fans each incoming message out to every member's executor
// node, which keeps a full local replica. Members send toward the owner —
// their delegation predecessor for a task scope, the plan origin for a plan
// scope — so a deeply nested member needs no global view. Fan-out copies are
// addressed per recipient task, so a directed note only ever lands on the
// board replica of the member it names; broadcast (rcpt="") lands on all.

// Message grades are the payload-size contract a sender picks from — the
// board's analogue of choosing a compression level: a status line is cheap
// and always lands, a report is a structured summary, an artifact_ref is a
// pointer the consumer pulls through the data plane only if it needs the
// tree behind it.
const (
	MsgGradeStatus      = "status"
	MsgGradeReport      = "report"
	MsgGradeArtifactRef = "artifact_ref"
	// MsgGradeSteer marks a user inject (steer.go): the note is delivered by
	// the round loop as the next round's leading directive, not folded as a
	// board line — the drain skips it so the text never lands twice.
	MsgGradeSteer = "steer"
)

const (
	// maxMsgStatusBody/maxMsgReportBody bound one message's wire and prompt
	// size. A body beyond its grade's cap is truncated, not refused — a
	// message that fails to send teaches the sender nothing, while a trimmed
	// line still carries the signal.
	maxMsgStatusBody = 400
	maxMsgReportBody = 4096
	// msgBoardTTL is how long a slot stays readable after landing. Boards are
	// scoped to live work; a message that outlives its scope's relevance —
	// or an abandoned run that never drains — must not resurface the next
	// time a same-named scope exists.
	msgBoardTTL = 24 * time.Hour
	// defaultMsgBudget bounds how many messages one task episode may send.
	// The cap meters rather than forbids (the soft-penalty analogue): the
	// sender is told the budget is spent and carries on with local work.
	defaultMsgBudget = 8
	// minMsgRefPrefix is the shortest task-id prefix a recipient address may
	// use. Board lines render a sender's id shortened; the floor keeps an
	// accidental one- or two-character address from resolving by luck.
	minMsgRefPrefix = 8
	// maxMsgRelayTTL bounds the re-envelope hops one wire copy may take —
	// the same bound the delegation chain itself enforces, since a board
	// post never legitimately travels further than the chain that spawned
	// its scope.
	maxMsgRelayTTL = scheduler.MaxChainDepth
)

// clipRunes bounds s to n runes — a plain byte slice can split a multi-byte
// rune mid-sequence, which renders as mojibake inside a prompt line.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s // byte len ≤ n already bounds the rune count
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// noteAttachHeader opens the block noteBody appends. Kept as one constant so
// the writer and NoteProse cannot drift: a console that failed to strip it
// would show the agent's instruction text as if the user had typed it.
const noteAttachHeader = "Attached files (read them from the workspace):"

// NoteProse returns the user's own words from a note body — everything before
// the attachment block. The block itself stays in the stored body because the
// agent reads bodies, never chip lists; surfaces that show the note to its
// author want the prose back.
func NoteProse(body string) string {
	if i := strings.Index(body, "\n\n"+noteAttachHeader); i >= 0 {
		return strings.TrimRight(body[:i], "\n")
	}
	if strings.HasPrefix(body, noteAttachHeader) {
		return ""
	}
	return body
}

// TaskMessage is one board slot: a sender's latest note toward one recipient
// (Rcpt=="" = broadcast to the scope) within a scope.
type TaskMessage struct {
	Scope       string `json:"scope"`
	Sender      string `json:"sender"` // sender task_id
	Rcpt        string `json:"rcpt"`   // recipient task_id, "" = broadcast
	SenderSeq   int64  `json:"sender_seq"`
	SenderLabel string `json:"sender_label"` // stage id or title, resolved by the owner
	Grade       string `json:"grade"`
	Body        string `json:"body"`
	SentAt      int64  `json:"sent_at"`
	RecvAt      int64  `json:"recv_at"`
	// ConsumedAt is when the recipient's round loop folded this slot into its
	// prompt; 0 = still waiting. RecvAt cannot answer that: it is stamped on
	// arrival, so a just-posted note already reads "received". Surfaces use
	// this one to draw "waiting for the next round" apart from "already read".
	ConsumedAt int64 `json:"consumed_at"`
	// Attachments are the files a user note rode along with, as
	// workspace-relative paths. The body already names them (`@path`); this
	// keeps the prose separable so a console can show chips and edit the text
	// without touching the attachment block. Empty on agent-authored slots.
	Attachments []string `json:"attachments,omitempty"`
	ExpiresAt   int64    `json:"expires_at"`
}

// msgLabel is the human-readable name a board line shows for a sender: its
// stage id when it has one (the natural address within a plan), else its
// title, else "". Bounded — a label rides every fan-out copy.
func msgLabel(t Task) string {
	label := t.StageID
	if label == "" {
		label = t.Title
	}
	return clipRunes(label, 40)
}

// msgRequest is the parsed form of one send intent from an agent — the
// structured-schema field and the PANDA_MSG marker both decode into it.
type msgRequest struct {
	To    string `json:"to"`
	Grade string `json:"grade"`
	Body  string `json:"body"`
}

// messageScope returns the board this task posts to and reads from, or "" for
// a standalone task with no siblings. A plan stage shares its board with every
// stage of the plan; a delegate child shares the board of its parent's child
// set — which is also how a parent hears its own children: the parent's second
// readable scope is task:<its own id> (see readableScopes).
func messageScope(t Task) string {
	if t.PlanID != "" {
		return "plan:" + t.PlanID
	}
	if t.ParentID != "" {
		return "task:" + t.ParentID
	}
	return ""
}

// BoardScopes lists the boards a task drains at round boundaries: its own
// scope plus the board of the child set it spawns (a parent reads its
// children's task:<self> board without being a sibling of it). Exported for
// the `panda task board` inspector.
func BoardScopes(t Task) []string {
	var out []string
	if s := messageScope(t); s != "" {
		out = append(out, s)
	}
	out = append(out, "task:"+t.TaskID)
	return out
}

// PutTaskMessage upserts one slot on a scope board. Stale arrivals (a fan-out
// copy landing after the sender already posted newer) lose to the seq guard.
func (s *TaskStore) PutTaskMessage(ctx context.Context, m TaskMessage) error {
	if m.Scope == "" || m.Sender == "" {
		return fmt.Errorf("task message needs scope and sender")
	}
	now := time.Now().Unix()
	recv := m.RecvAt
	if recv == 0 {
		recv = now
	}
	expires := m.ExpiresAt
	if expires == 0 {
		expires = now + int64(msgBoardTTL/time.Second)
	}
	// Stored as a JSON array, '' for none. A malformed list can only come from
	// a hand-edited row; persist '' rather than refusing the message — the
	// body still names the files, so the agent loses nothing.
	attach := ""
	if len(m.Attachments) > 0 {
		if raw, err := json.Marshal(m.Attachments); err == nil {
			attach = string(raw)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_messages
		(scope, sender, rcpt, sender_seq, sender_label, grade, body, attachments, sent_at, recv_at, consumed_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,0,?)
		ON CONFLICT(scope, sender, rcpt) DO UPDATE SET
			sender_seq=excluded.sender_seq, sender_label=excluded.sender_label,
			grade=excluded.grade, body=excluded.body, attachments=excluded.attachments,
			sent_at=excluded.sent_at,
			recv_at=excluded.recv_at, consumed_at=0, expires_at=excluded.expires_at
		WHERE excluded.sender_seq > task_messages.sender_seq`,
		m.Scope, m.Sender, m.Rcpt, m.SenderSeq, m.SenderLabel, m.Grade, m.Body,
		attach, m.SentAt, recv, expires)
	return err
}

// MarkTaskMessageConsumed records that the recipient folded this slot into a
// prompt. Best-effort: the round has already read the note, so a failed write
// costs one stale "waiting" badge, never a lost message — callers log at debug
// and carry on. Guarded by (scope, sender, sender_seq) so a slow write can
// never mark a newer post as read.
func (s *TaskStore) MarkTaskMessageConsumed(ctx context.Context, scope, sender string, seq int64) error {
	if s == nil || scope == "" || sender == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE task_messages SET consumed_at=?
		WHERE scope=? AND sender=? AND sender_seq=? AND consumed_at=0`,
		time.Now().Unix(), scope, sender, seq)
	return err
}

// PendingNoteCounts reports, per task id, how many user notes on that task's
// own board are still waiting for a round. One grouped query over live boards
// rather than a probe per row: the queue picker and the board card both need
// this for every row they paint, and a per-row round trip is the N+1 that
// makes a 50-row list feel slow. Grades are not filtered — a status note and a
// steer both sit there unread until a round folds them, and both are what the
// user typed.
func (s *TaskStore) PendingNoteCounts(ctx context.Context) (map[string]int, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope, COUNT(*) FROM task_messages
		WHERE consumed_at=0 AND expires_at > ? AND (sender=? OR sender LIKE ?)
		GROUP BY scope`, time.Now().Unix(), "user", userSenderPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var scope string
		var n int
		if err := rows.Scan(&scope, &n); err != nil {
			return out, err
		}
		if id, ok := strings.CutPrefix(scope, "task:"); ok && id != "" {
			out[id] += n
		}
	}
	return out, rows.Err()
}

// NextMsgSeq returns the seq a sender's next post on this scope should carry:
// one past the highest current slot, or 1 for a first post. The local read is
// authoritative for the sender's own slots — only the sender increments them.
func (s *TaskStore) NextMsgSeq(ctx context.Context, scope, sender string) int64 {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sender_seq) FROM task_messages WHERE scope=? AND sender=?`,
		scope, sender).Scan(&seq)
	if err != nil {
		return 1 // empty board or row-less sender: start at 1
	}
	return seq + 1
}

// TaskMessages returns every live slot on a scope board, oldest post first so
// a rendered board reads chronologically.
func (s *TaskStore) TaskMessages(ctx context.Context, scope string) ([]TaskMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, sender, rcpt, sender_seq,
		sender_label, grade, body, attachments, sent_at, recv_at, consumed_at FROM task_messages
		WHERE scope=? AND expires_at > ? ORDER BY sent_at ASC, sender ASC`,
		scope, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskMessage
	for rows.Next() {
		var m TaskMessage
		var attach sql.NullString
		if err := rows.Scan(&m.Scope, &m.Sender, &m.Rcpt, &m.SenderSeq,
			&m.SenderLabel, &m.Grade, &m.Body, &attach, &m.SentAt, &m.RecvAt, &m.ConsumedAt); err != nil {
			return out, err
		}
		m.Attachments = decodeAttachments(attach.String)
		out = append(out, m)
	}
	return out, rows.Err()
}

// decodeAttachments reads the JSON array PutTaskMessage wrote. A row from a
// pre-v60 build, or one whose bytes were touched outside the store, yields
// nil rather than an error: the note itself is still readable, and refusing
// the whole board for a malformed chip list would hide the message too.
func decodeAttachments(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// msgSeen tracks, per board slot, the sender_seq a run episode already folded
// into the prompt — so each round reports only what changed. In-memory per
// episode: a resumed run re-folds the whole live board once, which is the
// correct catch-up for a run that was away.
type msgSeen map[string]int64 // scope+"|"+sender+"|"+rcpt -> last seq folded

func msgSlotKey(m TaskMessage) string {
	return m.Scope + "|" + m.Sender + "|" + m.Rcpt
}

// drainTaskMessages reads the boards this task may read and returns the lines
// to fold into the next round's intent. Directed slots land only when they
// name this task; the sender's own posts are never read back to it.
func (c *Core) drainTaskMessages(ctx context.Context, task Task, seen msgSeen) []string {
	var lines []string
	for _, scope := range BoardScopes(task) {
		msgs, err := c.store.TaskMessages(ctx, scope)
		if err != nil {
			c.logger.Debug("read message board", "scope", scope, "err", err)
			continue
		}
		for _, m := range msgs {
			if m.Sender == task.TaskID {
				continue // never echo a task its own words
			}
			if m.Rcpt != "" && m.Rcpt != task.TaskID {
				continue // addressed elsewhere; replica noise
			}
			if m.Grade == MsgGradeSteer {
				continue // a user inject: delivered as the round's directive, not folded context
			}
			k := msgSlotKey(m)
			if seen[k] >= m.SenderSeq {
				continue
			}
			seen[k] = m.SenderSeq
			// Stamp the delivery on the row so a console in another process can
			// stop drawing this note as unread (see MarkTaskMessageConsumed).
			if err := c.store.MarkTaskMessageConsumed(ctx, m.Scope, m.Sender, m.SenderSeq); err != nil && c.logger != nil {
				c.logger.Debug("mark board note consumed", "task", task.TaskID, "err", err)
			}
			lines = append(lines, c.formatBoardLine(ctx, m))
		}
	}
	return lines
}

// formatBoardLine renders one slot for the prompt: the sender resolved to its
// stage id or title when the roster knows it, and an age tag so the reader
// can weigh freshness itself (the delay-discount analogue — surfaced rather
// than computed away).
func (c *Core) formatBoardLine(ctx context.Context, m TaskMessage) string {
	// The label the owner resolved rides the slot, so a replica that never
	// held the sender's row still names it; a local row refines it; the raw
	// task id is the last resort — long, but always addressable exactly.
	from := m.SenderLabel
	if from == "" {
		if st, err := c.store.Get(ctx, m.Sender); err == nil {
			from = msgLabel(st)
		}
	}
	if from == "" {
		from = m.Sender
	}
	age := time.Since(time.Unix(m.SentAt, 0)).Round(time.Second)
	line := fmt.Sprintf("from=%s grade=%s ago=%s: %s", from, m.Grade, age, m.Body)
	return clipRunes(line, 600)
}

// msgBoardOwner resolves the scope-owner node for a post on a given board.
// resolved=false means the answer is only the arrival direction the roster
// lives behind, not a verdict — usable for routing onward, not for the
// replica-side owner authentication.
//
// A plan board's owner is the member's chain root — the node that ran
// StartPlan holds every stage row. A children board's owner is the parent's
// EXECUTOR — the node that minted the members' task rows and holds the
// Children(pid) roster. That is NOT necessarily the chain root: a parent
// that was itself delegated spawns children from mid-chain, and its rows
// never propagate upstream, so the root has no roster to resolve against.
// Wherever the parent row is held its owner_node resolves correctly: the
// executor's own copy names itself (Accept stamps the executor as owner),
// and upstream copies name the next hop toward it (handleAccept stamps
// each relay's accept-sender), so routing to the resolved node re-resolves
// hop-wise until it lands on the executor. A node holding no parent row —
// a member's own executor, say — cannot name the owner; its chain
// predecessor is the direction the roster lives behind.
func (c *Core) msgBoardOwner(ctx context.Context, scope string, t Task) (owner string, resolved bool) {
	if scope == "task:"+t.TaskID {
		return c.nodeID, true
	}
	if pid, ok := strings.CutPrefix(scope, "task:"); ok {
		if parent, err := c.store.Get(ctx, pid); err == nil && parent.OwnerNode != "" {
			return parent.OwnerNode, true
		}
		if pred := scheduler.Predecessor(t.Chain, c.nodeID); pred != "" {
			return pred, false
		}
		return c.nodeID, false
	}
	if len(t.Chain) > 0 {
		return t.Chain[0], true
	}
	return c.nodeID, false
}

// msgSrcInChain reports whether src names a node on the member's own
// delegation chain upstream of this node — the corridor a fanout copy
// legitimately arrives through when the member cannot resolve the owner
// locally (no parent row). The tail element is the member's own executor
// (this node), which can never be its own board owner.
func msgSrcInChain(chain []string, src, self string) bool {
	for i, hop := range chain {
		if i == len(chain)-1 && scheduler.SameRuntimeIdentity(hop, self) {
			break // the tail is this node — the owner sits strictly before it
		}
		if scheduler.SameRuntimeIdentity(hop, src) {
			return true
		}
	}
	return false
}

// messageScopeFor picks the board a post goes to: the sender's own sibling
// scope, or — when a parent addresses one of its children by task id, or
// "children" — the children board the parent itself hosts.
func (c *Core) messageScopeFor(ctx context.Context, sender Task, to string) (string, error) {
	to = strings.TrimSpace(to)
	if to == "children" {
		return "task:" + sender.TaskID, nil
	}
	if to != "" && to != "parent" && to != "siblings" && to != "all" && !strings.HasPrefix(to, "stage:") {
		id := strings.TrimPrefix(to, "task:")
		if kids, err := c.store.Children(ctx, sender.TaskID); err == nil {
			for _, k := range kids {
				if k.TaskID == id {
					return "task:" + sender.TaskID, nil
				}
			}
		}
	}
	own := messageScope(sender)
	if own == "" {
		return "", fmt.Errorf("task has no sibling scope to post to")
	}
	return own, nil
}

// deliverTaskMessage posts a message for the scope: written to the sender's
// local replica immediately, then sent to the scope owner (or resolved
// directly when this node is the owner) for member fan-out. Delivery is
// best-effort by design — the slot model makes a dropped note silently
// correctable by the next post, so there is no outbox here.
func (c *Core) deliverTaskMessage(ctx context.Context, sender Task, req msgRequest) error {
	scope, err := c.messageScopeFor(ctx, sender, req.To)
	if err != nil {
		return err
	}
	grade := req.Grade
	if grade == "" {
		grade = MsgGradeStatus
	}
	body := strings.TrimSpace(req.Body)
	switch grade {
	case MsgGradeStatus:
		body = clipRunes(body, maxMsgStatusBody)
	case MsgGradeReport, MsgGradeArtifactRef:
		body = clipRunes(body, maxMsgReportBody)
	default:
		return fmt.Errorf("unknown message grade %q", grade)
	}
	if body == "" {
		return fmt.Errorf("empty message body")
	}
	m := TaskMessage{
		Scope:       scope,
		Sender:      sender.TaskID,
		SenderSeq:   c.store.NextMsgSeq(ctx, scope, sender.TaskID),
		SenderLabel: msgLabel(sender),
		Grade:       grade,
		Body:        body,
		SentAt:      time.Now().Unix(),
	}
	owner, _ := c.msgBoardOwner(ctx, scope, sender)
	if owner == c.nodeID {
		// We are the scope owner: resolve recipients and fan out directly.
		return c.fanoutTaskMessage(ctx, m, req.To, sender)
	}
	// Post to the local replica first — a task's own board keeps its words —
	// then hand the envelope to the owner for roster resolution. The local
	// copy's recipient is what can be resolved without the roster: "parent"
	// names the scope's parent, a concrete task id is already the answer.
	// Only a stage address stays unresolved locally, and the owner's
	// authoritative fan-out copy is what carries the true rcpt anyway.
	if m.Rcpt = localMsgRcpt(scope, req.To); m.Rcpt == sender.TaskID {
		return fmt.Errorf("a task cannot message itself")
	}
	if err := c.store.PutTaskMessage(ctx, m); err != nil {
		c.logger.Warn("post local board slot", "task", sender.TaskID, "err", err)
	}
	p := bus.TaskMessagePayload{
		Scope:       m.Scope,
		Sender:      m.Sender,
		To:          req.To,
		Src:         c.nodeID,
		SenderLabel: m.SenderLabel,
		Grade:       m.Grade,
		Body:        m.Body,
		Seq:         m.SenderSeq,
		SentAt:      m.SentAt,
		TTL:         maxMsgRelayTTL,
	}
	msgID, err := newUUID()
	if err != nil {
		return err
	}
	env, err := bus.NewEnvelope(bus.MsgTaskMessage, c.nodeID, msgID, p)
	if err != nil {
		return err
	}
	env.To = owner
	if err := c.sendTo(owner, env); err != nil {
		// The owner is off-link: prefer the chain walk home — the delegation
		// predecessor both holds the row and is the arrival direction the
		// owner's executor check expects — before the cheapest-path fallback.
		if hop := scheduler.Predecessor(sender.Chain, c.nodeID); hop != "" && hop != owner {
			if herr := c.sendTo(hop, env); herr == nil {
				return nil
			}
		}
		if rerr := c.routeTo(ctx, owner, env); rerr != nil {
			return fmt.Errorf("message undeliverable to scope owner %s: %w", owner, rerr)
		}
	}
	return nil
}

// localMsgRcpt resolves a post's recipient as far as it can be known without
// the scope roster: "" (broadcast) for the group addresses, the scope's
// parent for "parent", the address itself for a concrete task id. A
// "stage:<id>" address stays unresolved (""), as does any prefix — the
// owner's fan-out copy carries the authoritative resolution.
func localMsgRcpt(scope, to string) string {
	to = strings.TrimSpace(to)
	switch {
	case to == "", to == "siblings", to == "all", to == "children":
		return ""
	case to == "parent":
		pid, _ := strings.CutPrefix(scope, "task:")
		return pid
	case strings.HasPrefix(to, "stage:"):
		return ""
	default:
		return strings.TrimPrefix(to, "task:")
	}
}

// fanoutTaskMessage runs on the scope owner: resolve the `to` address, write
// the slot on the owner's board (which serves every member executing locally),
// and post copies to every remote member node the message addresses. A
// broadcast reaches all member nodes; a directed post reaches only the
// recipient's.
func (c *Core) fanoutTaskMessage(ctx context.Context, m TaskMessage, to string, sender Task) error {
	members, err := c.scopeMembers(ctx, m.Scope)
	if err != nil {
		return err
	}
	// The sender must itself be on this board's roster — the member→owner
	// wire leg authenticated the sending node, not the task id it claims,
	// so without this check any reachable node could write slots under a
	// task that is not part of the scope.
	inScope := false
	for _, mm := range members {
		if mm.taskID == sender.TaskID {
			inScope = true
			break
		}
	}
	if !inScope {
		return fmt.Errorf("sender %s is not a member of scope %s", sender.TaskID, m.Scope)
	}
	rcpt, err := resolveMsgRcpt(m, to, members, sender.TaskID)
	if err != nil {
		return err
	}
	m.Rcpt = rcpt
	// The owner re-stamps the authoritative sequence: a member's local
	// replica is the wrong source of truth because a task re-delegated to
	// a node whose replica is empty restarts its own counter at 1 — and
	// the seq guard would then drop every post it ever sends as stale.
	// The owner's board is the shared sequence space; whatever it says
	// next is what the slot records and every replica converges to.
	if seq := c.store.NextMsgSeq(ctx, m.Scope, m.Sender); seq > m.SenderSeq {
		m.SenderSeq = seq
	}
	// The owner's roster is the best source for the sender's display name:
	// stage id on the plan node, title on the spawner. A replica that never
	// held the sender's row renders this label as-is.
	if l := msgLabel(sender); l != "" {
		m.SenderLabel = l
	}
	if err := c.store.PutTaskMessage(ctx, m); err != nil {
		c.logger.Warn("store board slot", "scope", m.Scope, "err", err)
	}
	// Collect the distinct remote member nodes this post must reach. The
	// sender's own node already holds the slot (written at send time), so a
	// remote sender is skipped — its replica was the authoritative seq.
	nodes := map[string]string{}
	for _, mm := range members {
		if mm.taskID == sender.TaskID {
			continue
		}
		if rcpt != "" && mm.taskID != rcpt {
			continue // directed post: only the named member's node
		}
		if mm.node == "" || mm.node == c.nodeID {
			continue // local members read the owner's board just written
		}
		if _, seen := nodes[mm.node]; !seen {
			nodes[mm.node] = mm.taskID // one copy per node; its member verifies scope
		}
	}
	var firstErr error
	for node, memberID := range nodes {
		p := bus.TaskMessagePayload{
			Scope: m.Scope, Sender: m.Sender, Rcpt: m.Rcpt, Member: memberID,
			Src: c.nodeID, SenderLabel: m.SenderLabel, Grade: m.Grade, Body: m.Body,
			Seq: m.SenderSeq, SentAt: m.SentAt, Fanout: true,
			TTL: maxMsgRelayTTL,
		}
		msgID, err := newUUID()
		if err != nil {
			return err
		}
		env, err := bus.NewEnvelope(bus.MsgTaskMessage, c.nodeID, msgID, p)
		if err != nil {
			return err
		}
		env.To = node
		if err := c.sendTo(node, env); err != nil {
			if rerr := c.routeTo(ctx, node, env); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
		}
	}
	return firstErr
}

// scopeMember is the roster entry resolution needs: which task, on which node.
type scopeMember struct {
	taskID string
	stage  string // "" on non-stage members
	node   string // executor node; "" = local/unknown
}

// scopeMembers lists a scope's member set. Only meaningful on the owner node —
// a member node holds its own row and nothing to resolve against.
func (c *Core) scopeMembers(ctx context.Context, scope string) ([]scopeMember, error) {
	var members []scopeMember
	if pid, ok := strings.CutPrefix(scope, "plan:"); ok {
		stages, err := c.store.PlanStages(ctx, pid)
		if err != nil {
			return nil, err
		}
		for _, st := range stages {
			node, _ := c.store.DispatchTarget(ctx, st.TaskID)
			members = append(members, scopeMember{taskID: st.TaskID, stage: st.StageID, node: node})
		}
		return members, nil
	}
	pid, ok := strings.CutPrefix(scope, "task:")
	if !ok {
		return nil, fmt.Errorf("malformed message scope %q", scope)
	}
	// A children board's members are the child set plus the parent itself —
	// "to: parent" addresses a member like any other.
	rows, err := c.store.Children(ctx, pid)
	if err != nil {
		return nil, err
	}
	for _, ch := range rows {
		node, _ := c.store.DispatchTarget(ctx, ch.TaskID)
		members = append(members, scopeMember{taskID: ch.TaskID, node: node})
	}
	if parent, err := c.store.Get(ctx, pid); err == nil {
		members = append(members, scopeMember{taskID: parent.TaskID, node: parent.OwnerNode})
	}
	return members, nil
}

// resolveMsgRcpt maps the `to` address onto one recipient task id, "" for a
// broadcast (every member but the sender). "parent" names the children
// board's parent member (task: scopes only); "stage:<id>" or a bare member
// task id pick one member. An address naming nobody in the scope is a
// scope-drift attempt and is refused outright.
func resolveMsgRcpt(m TaskMessage, to string, members []scopeMember, senderID string) (string, error) {
	to = strings.TrimSpace(to)
	// one resolves a member by exact task id or unique id prefix — board
	// lines render senders shortened, so a reply copies the prefix it read
	// (the same exact-then-prefix rule ResolveTaskID gives the CLI).
	one := func(id string) (string, error) {
		match := ""
		for _, mm := range members {
			if mm.taskID == id {
				match = mm.taskID
				break
			}
		}
		if match == "" && len(id) >= minMsgRefPrefix {
			for _, mm := range members {
				if !strings.HasPrefix(mm.taskID, id) {
					continue
				}
				if match != "" {
					return "", fmt.Errorf("recipient %q matches multiple members of %s", to, m.Scope)
				}
				match = mm.taskID
			}
		}
		if match == "" {
			return "", fmt.Errorf("recipient %q is not a member of scope %s", to, m.Scope)
		}
		if match == senderID {
			return "", fmt.Errorf("a task cannot message itself")
		}
		return match, nil
	}
	switch {
	case to == "", to == "siblings", to == "all":
		return "", nil // broadcast slot; drain filters out the sender
	case to == "children":
		if m.Scope != "task:"+senderID {
			return "", fmt.Errorf("only a parent may address its children board")
		}
		return "", nil // broadcast to the child set
	case to == "parent":
		pid, _ := strings.CutPrefix(m.Scope, "task:")
		if pid == "" {
			return "", fmt.Errorf("only a children board has a parent member")
		}
		return one(pid)
	case strings.HasPrefix(to, "stage:"):
		want := strings.TrimPrefix(to, "stage:")
		for _, mm := range members {
			if mm.stage == want {
				return one(mm.taskID)
			}
		}
		return "", fmt.Errorf("no stage %q in scope %s", want, m.Scope)
	default:
		id := strings.TrimPrefix(to, "task:")
		return one(id)
	}
}

// sendAgentMessages is the run-loop entry for parsed message intents: budget
// accounting first (the spend is what stops a marker-happy agent), then one
// delivery per request. Returns per-request notes the caller folds into the
// next prompt so the agent learns what landed and what was refused.
func (c *Core) sendAgentMessages(ctx context.Context, task Task, reqs []msgRequest, sent *int, budget int) []string {
	var notes []string
	for _, req := range reqs {
		if *sent >= budget {
			notes = append(notes, fmt.Sprintf("message to %q not sent: budget spent (%d)", req.To, budget))
			continue
		}
		*sent++
		if err := c.deliverTaskMessage(ctx, task, req); err != nil {
			notes = append(notes, fmt.Sprintf("message to %q failed: %s", req.To, err))
			continue
		}
		notes = append(notes, fmt.Sprintf("message to %q posted", req.To))
		c.EvTrace(ctx, task.TaskID, "task_message", map[string]any{
			"to": req.To, "grade": req.Grade,
		})
	}
	return notes
}

// handleTaskMessage is the wire side (bus.MsgTaskMessage). A fanout=false
// envelope is a member addressing its scope owner; fanout=true is the owner's
// resolved copy for a member replica. Both legs relay hop-by-hop: an envelope
// addressed past this node forwards toward env.To — re-minted as ours (the
// bound-peer conn check kills a carried From), TTL spent one hop — so a scope
// stretched over a delegation chain works where every node sees only its own
// edge. Neither leg trusts the transport for identity: the owner admits a
// member post only when it arrived via that member's own dispatch path, and
// a replica admits a fan-out copy only when its Src names the scope owner —
// resolved from the parent row where held, else bounded to the member's own
// delegation chain.
func (c *Core) handleTaskMessage(ctx context.Context, env bus.Envelope) {
	var p bus.TaskMessagePayload
	if err := env.PayloadInto(&p); err != nil {
		c.logger.Warn("task_message: bad payload", "from", env.From, "err", err)
		return
	}
	// Transit first, for both legs: an envelope whose destination is another
	// node forwards without needing the row — a shortcut relay may hold none.
	// The row, when held, only picks the better next hop: chain predecessor
	// on the way up to the owner, next dispatch target on the way down to a
	// member.
	if env.To != "" && env.To != c.nodeID {
		hop := ""
		rowID := p.Sender
		if p.Fanout {
			rowID = p.Member
			if rowID == "" {
				rowID = p.Rcpt
			}
		}
		if rowID != "" {
			if t, err := c.store.Get(ctx, rowID); err == nil {
				if p.Fanout {
					hop, _ = c.store.DispatchTarget(ctx, rowID)
				} else {
					hop = scheduler.Predecessor(t.Chain, c.nodeID)
				}
			}
		}
		c.forwardTaskMessage(ctx, env, p, hop)
		return
	}
	if p.Fanout {
		// Owner-resolved copy for a member replica on this node. The copy
		// names the local member it is for (Member, falling back to Rcpt for
		// directed posts) — a task this node must actually hold, or the copy
		// is a misroute or a forgery.
		memberID := p.Member
		if memberID == "" {
			memberID = p.Rcpt
		}
		if memberID == "" {
			return
		}
		t, err := c.store.Get(ctx, memberID)
		if err != nil || messageScope(t) != p.Scope {
			c.logger.Warn("task_message fanout for non-member", "member", memberID, "scope", p.Scope, "from", env.From)
			return
		}
		// The member executes further down the chain, not here: re-address
		// the copy to the next dispatch hop and relay it onward.
		if tgt, _ := c.store.DispatchTarget(ctx, memberID); tgt != "" && !scheduler.SameRuntimeIdentity(tgt, c.nodeID) {
			env.To = tgt
			c.forwardTaskMessage(ctx, env, p, tgt)
			return
		}
		// The copy's claimed origin must be the scope's owner. When this node
		// can resolve the owner (it holds the parent row, or it IS the owner)
		// the check is exact; when it cannot — a member's own executor holds
		// no parent row — the copy must at least arrive from a node on the
		// member's own delegation chain, the corridor every real fanout copy
		// travels. Src carries the origin across re-minted hops; a pre-Src
		// peer's direct send still authenticates through env.From.
		expected, resolved := c.msgBoardOwner(ctx, p.Scope, t)
		src := msgSenderNode(p, env)
		forged := expected == c.nodeID ||
			(resolved && !scheduler.SameRuntimeIdentity(expected, src)) ||
			(!resolved && !msgSrcInChain(t.Chain, src, c.nodeID))
		if forged {
			c.logger.Warn("task_message fanout from non-owner", "member", memberID,
				"scope", p.Scope, "src", src, "from", env.From)
			return
		}
		m := TaskMessage{Scope: p.Scope, Sender: p.Sender, Rcpt: p.Rcpt,
			SenderSeq: p.Seq, SenderLabel: p.SenderLabel,
			Grade: p.Grade, Body: p.Body,
			SentAt: p.SentAt, RecvAt: time.Now().Unix(),
			ExpiresAt: time.Now().Add(msgBoardTTL).Unix()}
		if err := c.store.PutTaskMessage(ctx, m); err != nil {
			c.logger.Warn("store fanout slot", "scope", p.Scope, "err", err)
		}
		return
	}
	// Member → owner leg. Resolve on the node holding the roster: if this
	// node is not the owner, relay along the delegation path toward it.
	sender, err := c.store.Get(ctx, p.Sender)
	if err != nil {
		c.logger.Warn("task_message from unknown sender", "sender", p.Sender, "from", env.From)
		return
	}
	if owner, _ := c.msgBoardOwner(ctx, p.Scope, sender); owner != c.nodeID {
		env.To = owner
		c.forwardTaskMessage(ctx, env, p, scheduler.Predecessor(sender.Chain, c.nodeID))
		return
	}
	// A member post must arrive along its own dispatch path: env.From is the
	// authenticated last hop, and the member's first dispatch hop is the only
	// direction its subtree can speak through. Without the binding, any
	// reachable node could write slots — and squat the sender's seq — under a
	// member of a scope it merely knows.
	if !c.msgOnDispatchPath(ctx, p.Sender, env.From) {
		c.logger.Warn("task_message off-path member post", "sender", p.Sender,
			"from", env.From, "scope", p.Scope)
		return
	}
	m := TaskMessage{Scope: p.Scope, Sender: p.Sender,
		SenderSeq: p.Seq, Grade: p.Grade, Body: p.Body,
		SentAt: p.SentAt, RecvAt: time.Now().Unix()}
	if err := c.fanoutTaskMessage(ctx, m, p.To, sender); err != nil {
		c.logger.Warn("task_message fanout", "scope", p.Scope, "from", env.From, "err", err)
	}
}

// forwardTaskMessage relays a board envelope one hop toward its destination.
// The punch-relay rules apply verbatim (udp.go): the conn authenticates
// envelopes by their bound peer id, so the copy leaving here is re-minted as
// ours (the origin node survives in payload Src); TTL spends down per hop;
// the shared msg_id lets receiver dedup kill a residual loop. hop is the
// preferred next hop when the caller's rows can name one — a chain
// predecessor or a dispatch target — else the cheapest advertised path wins.
func (c *Core) forwardTaskMessage(ctx context.Context, env bus.Envelope, p bus.TaskMessagePayload, hop string) {
	if p.TTL == 0 {
		p.TTL = maxMsgRelayTTL // pre-TTL peer: grant the budget a fresh mint carries
	}
	if p.TTL <= 1 {
		c.logger.Debug("task_message ttl spent", "to", env.To, "scope", p.Scope)
		return
	}
	p.TTL--
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	env.Payload = raw
	env.From = c.nodeID
	if hop != "" && hop != c.nodeID {
		if err := c.sendTo(hop, env); err == nil {
			return
		}
	}
	if err := c.routeTo(ctx, env.To, env); err != nil {
		c.logger.Debug("task_message relay unroutable", "to", env.To, "err", err)
	}
}

// msgSenderNode is the node a board post claims to originate from: payload
// Src across re-minted relays, the envelope sender on a direct send — the
// only spelling a pre-Src peer produces.
func msgSenderNode(p bus.TaskMessagePayload, env bus.Envelope) string {
	if p.Src != "" {
		return p.Src
	}
	return env.From
}

// msgOnDispatchPath reports whether the node a member→owner frame physically
// arrived from is the node's own first dispatch hop for that member — the
// subtree the member's execution lives behind. "" target means the member
// never left this node, so a wire post under it is inconsistent outright
// (an owner-local member reaches the board through the in-process fanout).
func (c *Core) msgOnDispatchPath(ctx context.Context, senderTaskID, from string) bool {
	tgt, err := c.store.DispatchTarget(ctx, senderTaskID)
	if err != nil || tgt == "" {
		return false
	}
	return scheduler.SameRuntimeIdentity(tgt, from)
}
