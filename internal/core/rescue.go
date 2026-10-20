// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/Xustalis/OpenPanda/internal/ledger"
	"github.com/Xustalis/OpenPanda/internal/scheduler"
)

// orphanedForwardGrace is how long an orphaned delegation may stay unroutable
// before the rescue sweep gives up and fails it (S1-1). A task stuck in queued
// after a restart is re-route-retried on every monitor tick; the moment a peer
// reconnects or another capable node appears, it is rescued immediately. Only
// a task unroutable for this whole window is force-failed, so a transient
// outage does not kill work that was already forwarded. The sighting
// timestamps live in memory, so a restart resets the grace window — an
// accepted trade-off for keeping the rescue state-free in the database.
const orphanedForwardGrace = 2 * time.Minute

// staleRowAfter is how long a lifecycle row may sit untouched before the
// sweep treats it as stranded. Every live path rewrites its row constantly —
// submits route within seconds, claims accept in the same goroutine, running
// work renews its lease (which bumps updated_at) every lease period — so a
// row this old belongs to a process that died mid-flight. Two minutes
// matches the orphaned-forward grace: far beyond every live window, far
// short of the "parked forever" failures it exists to clear.
const staleRowAfter = 2 * time.Minute

// sweepStrandedWork is the running-process counterpart of Recover: rows whose
// owner died mid-flight strand in states no other pass touches — submitted
// (scheduled=0 hides them from ListReady), self-claimed dispatched rows that
// never accepted (no lease for ExpireTasks to reap), and self-owned
// running/waiting_context rows whose lease is NULL (a dead supervisor renews
// nothing). The age cutoff is the guard that keeps live work out: state-
// guarded writes then arbitrate against a healthy concurrent transition, so
// any process sharing the store may run the sweep.
func (c *Core) sweepStrandedWork(ctx context.Context) {
	cutoff := time.Now().Add(-staleRowAfter).Unix()
	if n, err := c.store.RequeueStaleSubmitted(ctx, cutoff); err != nil {
		c.logger.Warn("stranded submit sweep", "err", err)
	} else if n > 0 {
		c.logger.Info("stranded submits requeued", "count", n)
	}
	// Remote submits get the terminal verdict instead: requeueing an
	// un-admitted delegate would slip it past the trust-zone review gate.
	if n, err := c.store.CancelStaleRemoteSubmits(ctx, cutoff); err != nil {
		c.logger.Warn("stranded remote submit sweep", "err", err)
	} else if n > 0 {
		c.logger.Info("stranded remote submits cancelled", "count", n)
	}
	if n, err := c.store.RequeueStaleClaims(ctx, c.nodeID, cutoff); err != nil {
		c.logger.Warn("stranded claim sweep", "err", err)
	} else if n > 0 {
		c.logger.Info("orphaned claims requeued", "count", n)
	}
	if n, err := c.store.FailStaleLeaseless(ctx, c.nodeID, cutoff); err != nil {
		c.logger.Warn("leaseless execution sweep", "err", err)
	} else if n > 0 {
		c.logger.Info("leaseless executions failed", "count", n)
	}
}

// stalePeerAfter is how long a directory row may go unseen before the stale
// sweep marks it offline (S1-4). Without it a node that died silently — no
// clean disconnect, no heartbeat — keeps an online row forever and routing
// keeps handing work to a ghost. Peers with a live connection are excluded,
// and rows with last_seen=0 (never seen) are left alone.
const stalePeerAfter = 90 * time.Second

// peerFreshnessSec bounds how stale an unchanged inbound heartbeat may leave
// last_seen before the next beat rewrites the row (ledger.HeartbeatIfChanged).
// It must stay comfortably under stalePeerAfter — and under the panel's 45s
// "running" window — so a peer that stops beating still flips offline on
// schedule; 30s keeps a healthy peer's row at most ~30s old while halving the
// write traffic a steady beat used to produce.
const peerFreshnessSec = 30

// rescueOrphanedForwards recovers tasks this node forwarded to a remote
// executor before a restart and that Recover left queued (S1-1): the local
// in-flight delegation state (waiter, lease, connection) died with the
// process, and nothing else touches a queued-remotely row — ExpireTasks
// ignores it (no lease), and the queue scheduler never claims it (it is not
// scheduled). Each scan first attempts a re-route; when none exists the task
// is given orphanedForwardGrace before being failed and reported upstream.
func (c *Core) rescueOrphanedForwards(ctx context.Context) {
	tasks, err := c.store.QueuedDelegatedRemotely(ctx, c.nodeID)
	if err != nil {
		c.logger.Warn("orphan sweep: list", "err", err)
		return
	}
	active := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		active[t.TaskID] = true
		if c.taskOutboxPending(ctx, t.TaskID) {
			// The task is parked in this node's custody awaiting the peer's
			// next contact: delivery is already owed by the outbox flush, so
			// it is not orphaned no matter what state the restart left the
			// row in. Failing it would let custody deliver work the local
			// copy already declared dead.
			c.dropOrphan(t.TaskID)
			continue
		}
		if c.rerouteDeclined(ctx, t.TaskID) {
			c.dropOrphan(t.TaskID)
			c.logger.Info("orphaned forward rescued by re-route", "task", t.TaskID)
			continue
		}
		c.orphanExpired(ctx, t.TaskID, t.AttemptID)
	}
	// Drop sightings for tasks that are no longer orphaned (rescued by the
	// queue scheduler, completed, cancelled) so the map cannot leak.
	c.mu.Lock()
	for id := range c.orphanSeen {
		if !active[id] {
			delete(c.orphanSeen, id)
		}
	}
	c.mu.Unlock()
}

// orphanExpired records when a task was first seen orphaned and, once the
// grace window has passed, fails it and propagates the failure upstream. The
// verdict binds the sighted attempt: a delegate that landed mid-grace rotated
// the row into fresh custody, and that copy is not ours to kill.
func (c *Core) orphanExpired(ctx context.Context, taskID, attemptID string) {
	c.mu.Lock()
	first, seen := c.orphanSeen[taskID]
	if !seen {
		c.orphanSeen[taskID] = time.Now()
	}
	c.mu.Unlock()
	if seen && time.Since(first) >= orphanedForwardGrace {
		c.failOrphanedForward(ctx, taskID, attemptID)
	}
}

// dropOrphan forgets a task's orphan sighting once it is no longer stranded.
func (c *Core) dropOrphan(taskID string) {
	c.mu.Lock()
	delete(c.orphanSeen, taskID)
	c.mu.Unlock()
}

// failOrphanedForward force-fails an orphaned delegation and reports the
// failure up the chain, so a root scheduler blocked in Submit (or a parent
// relay) learns the forwarded work is dead instead of waiting on a lease
// that nothing renews.
func (c *Core) failOrphanedForward(ctx context.Context, taskID, attemptID string) {
	const reason = "relay restarted: no route for re-forward"
	if err := c.store.ForceFailAttempt(ctx, taskID, attemptID, reason); err != nil {
		c.logger.Warn("orphan: force fail", "task", taskID, "err", err)
		return
	}
	c.dropOrphan(taskID)
	tk, err := c.store.Get(ctx, taskID)
	if err != nil {
		return
	}
	// ForceFailAttempt no-ops when the row rotated past the sighted attempt —
	// the fresh custody is alive, so there is no failure to report.
	if tk.State != StateFailed || (attemptID != "" && tk.AttemptID != attemptID) {
		return
	}
	res := bus.TaskResultPayload{
		TaskID: taskID, AttemptID: tk.AttemptID, State: StateFailed, OK: false, ExitCode: 1,
		Stderr: reason, Chain: tk.Chain, Executor: c.nodeID,
	}
	c.relayToParent(ctx, bus.MsgTaskResult, tk.Chain, res)
	c.signalResult(taskID, res)
	c.logger.Info("orphaned forward failed out", "task", taskID)
}

// sweepStalePeers marks directory rows that have gone silent offline (S1-4).
// Connected peers are excluded: their row may lag a heartbeat tick behind,
// and a live conn is the stronger liveness signal.
//
// The self row is excluded too — and not just restored after the fact:
// ExpireStale's flip goes through MarkOffline, which STAMPS last_seen, so a
// swept self row gets its freshness refreshed by the very sweep that found
// it stale. For an owner the heartbeat would overwrite it seconds later;
// for a borrowed engine — a web console or CLI whose row no process
// heartbeats — the 5s monitor tick turned that into a refresh loop that
// kept the row perpetually "fresh", masking the dead queue from the
// store-bound consumer probe (StoreConsumerFresh). The row's freshness is
// the heartbeat's alone: no heartbeat, no freshness — which is exactly
// what the probe must see.
func (c *Core) sweepStalePeers(ctx context.Context) {
	c.mu.RLock()
	exclude := make([]string, 0, len(c.peers)+1)
	for id := range c.peers {
		exclude = append(exclude, id)
	}
	c.mu.RUnlock()
	if c.nodeID != "" {
		exclude = append(exclude, c.nodeID)
		// An ephemeral node id's directory row is its stable base
		// (refreshSelfNeighbors writes there): the same reasoning protects
		// both spellings.
		if base, ok := scheduler.EphemeralBase(c.nodeID); ok && base != c.nodeID {
			exclude = append(exclude, base)
		}
	}

	expired, err := ledger.ExpireStale(c.db, int64(stalePeerAfter.Seconds()), exclude)
	if err != nil {
		c.logger.Warn("sweep stale peers", "err", err)
		return
	}
	for _, id := range expired {
		if scheduler.IsSelfRow(id, c.nodeID) {
			if err := ledger.MarkOnline(c.db, id); err != nil {
				c.logger.Warn("restore self directory row", "err", err)
			}
			continue
		}
		c.logger.Info("stale peer marked offline", "peer", id)
	}

	// Session-ghost GC: rows keyed by an ephemeral "-8hex" id can never host
	// a fleet member again once their session ends — delete them outright
	// instead of letting them pile up as offline routing candidates.
	ghosts, err := ledger.ReapGhosts(c.db, int64(ghostRowAfter.Seconds()), exclude)
	if err != nil {
		c.logger.Warn("reap session ghosts", "err", err)
	} else {
		for _, id := range ghosts {
			c.logger.Info("reaped session-ghost row", "node", id)
		}
	}
}

// ghostRowAfter is the grace before a session-keyed row is deleted: long
// enough that a session reconnecting mid-sweep keeps its row, short enough
// that yesterday's `panda ask` does not linger as a routing candidate.
const ghostRowAfter = 15 * time.Minute
