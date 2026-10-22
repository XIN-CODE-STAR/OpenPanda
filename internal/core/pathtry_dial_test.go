// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// TestLadderDialKeepsItsConnectionAlive pins the reachability ladder's
// "advertised addr" rung.
//
// dialPeerAddrs gives each candidate a dial BUDGET (a timeout so a dead
// address cannot stall the rung) and then hands that same context to
// DialPeer, which starts the connection's read loop under it. The budget's
// cancel fires the moment DialPeer returns — right after the hello went
// out, before the peer's reply was read — so the read loop started already
// cancelled, the conn was closed, and the peer never registered. Every
// gossip-address dial and every rendezvous dial silently degraded to
// relay/punch, and nothing reported it.
//
// The dial budget and the connection lifetime are two different clocks: the
// first bounds the attempt, the second bounds the edge.
func TestLadderDialKeepsItsConnectionAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A probed ephemeral port, not a fixed number: a hardcoded one collides
	// with a concurrent test run and then the ladder legitimately reports
	// "no route" against a listener that is not ours.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := l.Addr().String()
	_ = l.Close()

	server := newCoreWithNative(t, "ladder-server", serverAddr,
		ledger.NativeAbility{ID: "sys:info", Command: "uname"})
	dialer := newCoreWithNative(t, "ladder-dialer", "127.0.0.1:0",
		ledger.NativeAbility{ID: "sys:info", Command: "uname"})
	registerAndListen(t, ctx, server, serverAddr)
	// registerAndListen starts Listen in a goroutine; wait until it actually
	// accepts before dialing. On a slow Windows runner the first dial could
	// land before the listener was up, get refused, and the ladder (which
	// dials each candidate once per pass) would report no route.
	readyBy := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", serverAddr, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(readyBy) {
			t.Fatalf("listener never became reachable at %s: %v", serverAddr, derr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The ladder rung dials the peer's ADVERTISED address, so the directory
	// row has to carry it. A directly-learned row (learned_via empty) is
	// what the provenance gate admits.
	if _, err := dialer.db.Exec(
		`INSERT INTO employee_cache (id, name, addrs, status, last_seen)
		 VALUES (?, ?, ?, 'offline', 0)`,
		server.nodeID, server.nodeID, `["`+serverAddr+`"]`); err != nil {
		t.Fatalf("seed directory row: %v", err)
	}

	if !dialer.dialPeerAddrs(ctx, server.nodeID) {
		t.Fatal("the ladder reported no route to an address that is listening")
	}
	// The conn must SURVIVE the dial budget's expiry — that is the whole
	// point: the edge outlives the attempt.
	time.Sleep(addrDialTimeout + 2*time.Second)
	if conn := dialer.connFor(server.nodeID); conn == nil {
		t.Fatal("the dialed connection was closed by the dial budget's context — " +
			"the read loop ran under a context that was already cancelled")
	}
	if server.connFor(dialer.nodeID) == nil {
		t.Fatal("the peer never registered the dialer — the hello reply was never processed")
	}
}
