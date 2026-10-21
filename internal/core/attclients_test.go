// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
)

// TestAttClientBindAndResolve: the contact point remembers which conn a
// joiner's stable identity arrived on, and resolves transit frames to it.
func TestAttClientBindAndResolve(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "contact")
	conn := &bus.Conn{}
	c.registerAttClient("k:joiner", conn)
	if got := c.attClientFor("k:joiner"); got != conn {
		t.Fatalf("resolve = %v, want the bound conn", got)
	}
	if got := c.attClientFor("k:stranger"); got != nil {
		t.Fatal("unbound identity resolved to a conn")
	}
	c.dropAttClient("k:joiner", nil)
	if got := c.attClientFor("k:joiner"); got != nil {
		t.Fatal("dropped binding still resolves")
	}
}

// TestAttClientExpiry: the binding lives no longer than the pairing window.
func TestAttClientExpiry(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "contact")
	conn := &bus.Conn{}
	c.registerAttClient("k:joiner", conn)
	c.att.mu.Lock()
	ac := c.att.byStable["k:joiner"]
	ac.expires = time.Now().Add(-time.Second)
	c.att.byStable["k:joiner"] = ac
	c.att.mu.Unlock()
	if got := c.attClientFor("k:joiner"); got != nil {
		t.Fatal("expired binding must not resolve")
	}
	c.att.mu.Lock()
	_, still := c.att.byStable["k:joiner"]
	c.att.mu.Unlock()
	if still {
		t.Fatal("expired binding should be swept on lookup")
	}
}

// TestAttClientCap: a hostile LAN cannot pin unlimited conns in a contact
// point's memory; an EXISTING binding is still refreshed at the cap.
func TestAttClientCap(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "contact")
	for i := 0; i < attMaxClients; i++ {
		c.registerAttClient(stableIDPrefix+string(rune('a'+i)), &bus.Conn{})
	}
	extra := &bus.Conn{}
	c.registerAttClient("k:overflow", extra)
	if got := c.attClientFor("k:overflow"); got != nil {
		t.Fatal("binding past the cap must be refused")
	}
	// Refreshing an incumbent is always allowed.
	first := c.attClientFor(stableIDPrefix + "a")
	c.registerAttClient(stableIDPrefix+"a", first)
	if got := c.attClientFor(stableIDPrefix + "a"); got != first {
		t.Fatal("incumbent refresh refused at the cap")
	}
}

// TestDropAttClientForConn: when the joiner's conn dies every binding it held
// goes with it, so a dead socket cannot capture a future join's replies.
func TestDropAttClientForConn(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "contact")
	dead := &bus.Conn{}
	live := &bus.Conn{}
	c.registerAttClient("k:a", dead)
	c.registerAttClient("k:b", dead)
	c.registerAttClient("k:c", live)
	c.dropAttClientForConn(dead)
	if c.attClientFor("k:a") != nil || c.attClientFor("k:b") != nil {
		t.Fatal("bindings on a dead conn survived")
	}
	if c.attClientFor("k:c") != live {
		t.Fatal("unrelated binding was dropped")
	}
}

// TestPairSessionCloseConnGuard: a transit session owns no conn, so closing
// it must not panic.
func TestPairSessionCloseConnGuard(t *testing.T) {
	s := &pairSession{}
	s.closeConn() // nil conn: no-op
}
