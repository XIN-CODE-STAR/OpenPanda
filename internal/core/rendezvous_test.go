// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// seedRendezvousRow plants a directory row with the relay/addrs/status/pub
// fields the candidate scorer reads.
func seedRendezvousRow(t *testing.T, c *Core, id string, rp ledger.RelayProfile, addrs []string, status, pub string, seenAt int64) {
	t.Helper()
	rj, _ := json.Marshal(rp)
	aj, _ := json.Marshal(addrs)
	if _, err := c.db.Exec(`INSERT INTO employee_cache
		(id, relay, addrs, status, pub_key, seen_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET relay=excluded.relay, addrs=excluded.addrs,
			status=excluded.status, pub_key=excluded.pub_key,
			seen_at=excluded.seen_at, last_seen=excluded.last_seen`,
		id, string(rj), string(aj), status, pub, seenAt, seenAt); err != nil {
		t.Fatalf("seed rendezvous row: %v", err)
	}
}

func TestRendezvousCandidatesFilters(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "aa")
	now := time.Now().Unix()

	// Eligible: rendezvous flag, pub key, addrs, online.
	seedRendezvousRow(t, c, "rv-ok", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.1:7836"}, "online", "pk", now)
	// No rendezvous flag — never a candidate.
	seedRendezvousRow(t, c, "rv-no-flag", ledger.RelayProfile{Transit: true},
		[]string{"203.0.113.2:7836"}, "online", "pk", now)
	// No pub key — cannot be pinned at dial time.
	seedRendezvousRow(t, c, "rv-no-pub", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.3:7836"}, "online", "", now)
	// No addrs and no conn — undialable.
	seedRendezvousRow(t, c, "rv-no-addr", ledger.RelayProfile{Rendezvous: true},
		nil, "online", "pk", now)
	// Session row — never a fleet member.
	seedRendezvousRow(t, c, "laptop-1a2b3c4d", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.4:7836"}, "online", "pk", now)
	// Self — never our own rendezvous.
	seedRendezvousRow(t, c, "aa", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.5:7836"}, "online", "pk", now)

	cands := c.rendezvousCandidates(ctx)
	if len(cands) != 1 || cands[0].id != "rv-ok" {
		ids := make([]string, 0, len(cands))
		for _, cd := range cands {
			ids = append(ids, cd.id)
		}
		t.Fatalf("candidates = %v, want [rv-ok]", ids)
	}
}

func TestRendezvousScoring(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "aa")
	now := time.Now().Unix()
	seedRendezvousRow(t, c, "rv-fresh", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.1:7836"}, "online", "pk", now)
	seedRendezvousRow(t, c, "rv-stale", ledger.RelayProfile{Rendezvous: true},
		[]string{"203.0.113.2:7836"}, "online", "pk", now-3600)
	// A high edge-load report must drag the fresher candidate down past the
	// stale one (freshness base 1000 vs. the stale row's ~16 after an hour).
	c.notePeerEdgeLoad("rv-fresh", 600)
	cands := c.rendezvousCandidates(ctx)
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].id != "rv-stale" {
		t.Fatalf("edge load did not reorder: first = %s", cands[0].id)
	}
	// Clearing the load report restores the freshness ordering.
	c.notePeerEdgeLoad("rv-fresh", 0)
	cands = c.rendezvousCandidates(ctx)
	if cands[0].id != "rv-fresh" {
		t.Fatalf("fresh candidate lost its lead without load: first = %s", cands[0].id)
	}
}

func TestRendezvousElect(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "aa")
	mk := func(id string, score float64, domain string) rendezvousCandidate {
		return rendezvousCandidate{id: id, score: score, domain: domain}
	}

	t.Run("K cap", func(t *testing.T) {
		got := c.rendezvousElect([]rendezvousCandidate{
			mk("a", 100, "d1"), mk("b", 90, "d2"), mk("c", 80, "d3"),
		}, rendezvousKDefault)
		if len(got) != rendezvousKDefault || !got["a"] || !got["b"] {
			t.Fatalf("elected %v", got)
		}
	})

	t.Run("domain diversity", func(t *testing.T) {
		got := c.rendezvousElect([]rendezvousCandidate{
			mk("a", 100, "same"), mk("b", 99, "same"), mk("c", 10, "other"),
		}, rendezvousKDefault)
		if !got["a"] || !got["c"] || got["b"] {
			t.Fatalf("same-domain runner-up beat the diverse pick: %v", got)
		}
	})

	t.Run("hysteresis keeps incumbent", func(t *testing.T) {
		c.rzv.mu.Lock()
		c.rzv.peers = map[string]bool{"inc": true}
		c.rzv.mu.Unlock()
		// Challenger scores better but not by the hysteresis margin.
		got := c.rendezvousElect([]rendezvousCandidate{
			mk("ch", 120, "d2"), mk("inc", 100, "d1"),
		}, rendezvousKDefault)
		if !got["inc"] {
			t.Fatalf("incumbent lost to a marginal challenger: %v", got)
		}
	})

	t.Run("decisive challenger swaps", func(t *testing.T) {
		c.rzv.mu.Lock()
		c.rzv.peers = map[string]bool{"inc": true}
		c.rzv.mu.Unlock()
		// score 100 * 1.25 = 125; 200 clears it decisively.
		got := c.rendezvousElect([]rendezvousCandidate{
			mk("ch", 200, "d2"), mk("inc", 100, "d1"),
		}, rendezvousKDefault)
		if !got["ch"] {
			t.Fatalf("decisive challenger not elected: %v", got)
		}
	})
}

func TestAddrDomain(t *testing.T) {
	same1 := addrDomain([]string{"203.0.113.9:7836", "192.168.1.1:7836"})
	same2 := addrDomain([]string{"203.0.113.44:9000"})
	diff := addrDomain([]string{"203.0.114.9:7836"})
	if same1 != same2 {
		t.Fatalf("same /24 scored as different domains: %q vs %q", same1, same2)
	}
	if same1 == diff {
		t.Fatal("different /24s collapsed to one domain")
	}
	if got := addrDomain([]string{"node.example.com:7836"}); got != "name:node.example.com" {
		t.Fatalf("hostname domain = %q", got)
	}
	if got := addrDomain(nil); got != "none" {
		t.Fatalf("addrless domain = %q", got)
	}
	// v6 /48 grouping.
	v6a := addrDomain([]string{"[2001:db8:1:2::9]:7836"})
	v6b := addrDomain([]string{"[2001:db8:1:9::1]:7836"})
	if v6a != v6b {
		t.Fatalf("same /48 scored as different domains: %q vs %q", v6a, v6b)
	}
}
