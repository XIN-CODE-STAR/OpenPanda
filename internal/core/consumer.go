// SPDX-License-Identifier: AGPL-3.0-or-later

package core

// Queue-consumer liveness probe. `panda task add`, `panda plan run`, the
// REPL's `/task add` and the classified-plan path all persist rows only;
// the queue is drained by a daemon's queue scheduler or by an embedded web
// console (QueueTasks). With neither running, an enqueued task sits forever
// — and until this probe existed the enqueue output gave no hint of that.
// The check is best-effort: a false negative costs one advisory line, never
// behavior.

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"time"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/guard"
	"github.com/Xustalis/OpenPanda/internal/nodeidentity"
)

// QueueConsumerAlive reports whether some local process can drain this node's
// task queue: the daemon holds the node identity lock (the same probe
// `panda status` uses to mark the local row running), and `panda web` (or the
// standalone sidecar) accepts TCP connections on the panel address.
func QueueConsumerAlive(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if held, err := nodeidentity.Held(cfg.Node.Kind, cfg.Node.EffectiveIdentity()); err == nil && held {
		return true
	}
	return PanelReachable(cfg.Network.PanelAddr)
}

// PanelReachable dials the panel bind address. A wildcard host probes
// loopback (the listener is local by definition); the fallback range mirrors
// listenPanel, which slips forward a few ports when the configured one is
// taken — a console on 7841 is still a live consumer.
func PanelReachable(addr string) bool {
	host, base := "127.0.0.1", 7840
	if addr != "" {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return false
		}
		host = h
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		base = n
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	const span = 6
	hits := make(chan bool, span)
	for i := 0; i < span; i++ {
		guard.Go(nil, "core: port probe", nil, func() {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(base+i)), 200*time.Millisecond)
			if conn != nil {
				_ = conn.Close()
			}
			hits <- err == nil
		})
	}
	for i := 0; i < span; i++ {
		if <-hits {
			return true
		}
	}
	return false
}

// storeConsumerMaxAge bounds how fresh the node row's last_seen must be to
// prove an owner is heartbeating THIS store: the self row rewrites at least
// every selfRowRefresh (30s) and the stale-peer sweep ages a row out at 90s,
// so a row inside the window is being written live.
const storeConsumerMaxAge = 90 * time.Second

// NodeRowLastSeen reads the directory row's heartbeat stamp — 0 when the row
// is absent. Snapshot it BEFORE the engine's own Register: that upsert always
// rewrites last_seen, so a post-register read cannot tell our write from a
// real owner's heartbeat without the pre-register value to compare against.
func NodeRowLastSeen(ctx context.Context, db *sql.DB, nodeID string) int64 {
	if db == nil {
		return 0
	}
	var lastSeen int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(last_seen,0) FROM employee_cache WHERE id=?`, nodeID,
	).Scan(&lastSeen); err != nil {
		return 0
	}
	return lastSeen
}

// StoreConsumerFresh reports whether an owner heartbeats nodeID's row in db.
// QueueConsumerAlive answers "is a consumer process running on this machine"
// — the identity lock and the panel port cannot see which database that
// process actually drains. This probe is store-bound: a daemon that holds the
// lock but opened a different db_path never refreshes this store's row, so
// enqueues here would dead-letter even though the process check reports a
// consumer alive.
//
// lastForeign/selfWriteAt separate the caller's own Register writes from a
// real consumer's beats: selfWriteAt is the unix time of our most recent
// register (0 if the engine never wrote); lastForeign is the newest last_seen
// observed strictly above it — i.e. a stamp nobody in this process wrote.
// Alive means the row moved past our write, or the last foreign stamp is
// still inside the heartbeat window. Our own stamp alone proves nothing, and
// a re-register must not let it age into "fresh foreign evidence".
func StoreConsumerFresh(ctx context.Context, db *sql.DB, nodeID string, lastForeign, selfWriteAt int64) bool {
	cur := NodeRowLastSeen(ctx, db, nodeID)
	if cur == 0 {
		return false
	}
	if selfWriteAt == 0 {
		// The caller never wrote the row — plain freshness applies.
		return time.Since(time.Unix(cur, 0)) < storeConsumerMaxAge
	}
	if cur > selfWriteAt {
		return true // a heartbeat after our last register — a live foreign owner
	}
	return lastForeign > 0 && time.Since(time.Unix(lastForeign, 0)) < storeConsumerMaxAge
}
