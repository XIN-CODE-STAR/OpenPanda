// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// mintDirRow builds and signs a directory row for id, returning the
// canonical wire bytes and the signer's pub hex. mut customizes fields
// before signing.
func mintDirRow(t *testing.T, id string, seenAt int64, mut func(*bus.DirRow)) (json.RawMessage, string) {
	t.Helper()
	pub, priv, err := bus.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &bus.DirRow{ID: id, PubKey: hex.EncodeToString(pub), SeenAt: seenAt}
	if mut != nil {
		mut(r)
	}
	bus.SignDirRow(priv, r)
	return bus.CanonicalDirRowBytes(r), hex.EncodeToString(pub)
}

// stripSig post-processes a minted row: parse, mutate (typically clearing or
// corrupting the signature), and re-marshal. Needed because mintDirRow signs
// after `mut` — post-sign edits belong here.
func stripSig(t *testing.T, raw json.RawMessage, mut func(*bus.DirRow)) json.RawMessage {
	t.Helper()
	var r bus.DirRow
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	mut(&r)
	return bus.CanonicalDirRowBytes(&r)
}

// dirField reads one column for assertions.
func dirField(t *testing.T, c *Core, id, col string) string {
	t.Helper()
	var v string
	if err := c.db.QueryRow(`SELECT COALESCE(`+col+`,'') FROM employee_cache WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatalf("read %s for %s: %v", col, id, err)
	}
	return v
}

func TestSelfDirRowSignedAndStored(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "node-a")
	ctx := context.Background()
	row, err := c.selfDirRow(time.Now())
	if err != nil {
		t.Fatalf("selfDirRow: %v", err)
	}
	if row.ID != "node-a" || row.Sig == "" || row.PubKey == "" || row.XPub == "" {
		t.Fatalf("self row incomplete: %+v", row)
	}
	if !bus.VerifyDirRow(row) {
		t.Fatal("self row fails its own signature")
	}
	c.refreshSelfDirRow(ctx)
	if got := dirField(t, c, "node-a", "dir_sig"); got == "" {
		t.Fatal("self row not stored in gossip layer")
	}
	var r bus.DirRow
	if err := json.Unmarshal([]byte(dirField(t, c, "node-a", "dir_row_json")), &r); err != nil {
		t.Fatalf("stored row unparsable: %v", err)
	}
	if !bus.VerifyDirRow(&r) {
		t.Fatal("stored self row lost its signature")
	}
}

func TestApplyDirRowSignedLands(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "node-a")
	ctx := context.Background()
	card, _ := json.Marshal(ledger.CapabilitySummary{
		Device: "ghost", NodeKind: "physical",
		NativeIDs: []string{"sys:info"}, Addrs: []string{"10.9.9.9:7836"},
	})
	raw, pub := mintDirRow(t, "ghost", time.Now().Unix(), func(r *bus.DirRow) {
		r.Card = card
		r.XPub = hex.EncodeToString(make([]byte, 32))
		r.Addrs = []string{"10.9.9.9:7836"}
	})
	if !c.applyDirRow(ctx, raw, "via-peer") {
		t.Fatal("signed fresh row rejected")
	}
	if got := dirField(t, c, "ghost", "pub_key"); got != pub {
		t.Fatalf("pub_key = %q, want %q", got, pub)
	}
	if got := dirField(t, c, "ghost", "dir_sig"); got == "" {
		t.Fatal("dir_sig not stored")
	}
	if got := dirField(t, c, "ghost", "learned_via"); got != "via-peer" {
		t.Fatalf("learned_via = %q", got)
	}
	if got := dirField(t, c, "ghost", "status"); got != "online" {
		t.Fatalf("fresh signed row status = %q, want online", got)
	}
	if got := dirField(t, c, "ghost", "name"); got != "ghost" {
		t.Fatalf("card not applied: name = %q", got)
	}
}

func TestApplyDirRowConflicts(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Unix()
	mkCore := func() *Core { return newCoreOnDB(t, openTestDB(t), "node-a") }

	t.Run("signed beats unsigned", func(t *testing.T) {
		c := mkCore()
		// unsigned first
		raw, _ := mintDirRow(t, "ghost", now-10, nil)
		raw = stripSig(t, raw, func(r *bus.DirRow) { r.Sig = "" })
		if !c.applyDirRow(ctx, raw, "p") {
			t.Fatal("unsigned row not applied")
		}
		// older signed still wins over newer unsigned
		signed, _ := mintDirRow(t, "ghost", now-20, nil)
		if !c.applyDirRow(ctx, signed, "p") {
			t.Fatal("signed row lost to unsigned incumbent")
		}
		if got := dirField(t, c, "ghost", "dir_sig"); got == "" {
			t.Fatal("unsigned incumbent survived")
		}
		// newer unsigned cannot displace the signed row
		raw2, _ := mintDirRow(t, "ghost", now+50, nil)
		raw2 = stripSig(t, raw2, func(r *bus.DirRow) { r.Sig = "" })
		if c.applyDirRow(ctx, raw2, "p") {
			t.Fatal("unsigned challenger displaced signed row")
		}
	})

	t.Run("newer seen_at wins same signedness", func(t *testing.T) {
		c := mkCore()
		old, _ := mintDirRow(t, "ghost", now-100, func(r *bus.DirRow) { r.Ver = "0.0.1" })
		if !c.applyDirRow(ctx, old, "p") {
			t.Fatal("first signed row rejected")
		}
		stale, _ := mintDirRow(t, "ghost", now-200, func(r *bus.DirRow) { r.Ver = "0.0.0" })
		if c.applyDirRow(ctx, stale, "p") {
			t.Fatal("stale row displaced newer incumbent")
		}
	})

	t.Run("forged sig drops", func(t *testing.T) {
		c := mkCore()
		raw, _ := mintDirRow(t, "ghost", now, nil)
		raw = stripSig(t, raw, func(r *bus.DirRow) {
			r.Sig = hex.EncodeToString(make([]byte, ed25519.SignatureSize))
		})
		if c.applyDirRow(ctx, raw, "p") {
			t.Fatal("row with garbage signature applied")
		}
		var n int
		if err := c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id='ghost'`).Scan(&n); err != nil || n != 0 {
			t.Fatal("forged row stored")
		}
	})

	t.Run("pub contradicts pinned drops", func(t *testing.T) {
		c := mkCore()
		// A verified hello pinned 'ghost' to pub A.
		_, pubA, _ := bus.GenerateNodeKey()
		c.recordPeerPubKey(ctx, "ghost", hex.EncodeToString(pubA), "")
		// A signed row claiming pub B for the same id is a rekey claim
		// gossip cannot adjudicate — it must drop, not rekey.
		raw, _ := mintDirRow(t, "ghost", now, nil)
		if c.applyDirRow(ctx, raw, "p") {
			t.Fatal("contradicting-pub row applied")
		}
		if got := dirField(t, c, "ghost", "pub_key"); got != hex.EncodeToString(pubA) {
			t.Fatal("pinned pub_key was rotated by gossip")
		}
	})

	t.Run("self and session ids drop", func(t *testing.T) {
		c := mkCore()
		self, _ := mintDirRow(t, "node-a", now, nil)
		if c.applyDirRow(ctx, self, "p") {
			t.Fatal("row about ourselves applied")
		}
		sess, _ := mintDirRow(t, "laptop-1a2b3c4d", now, nil)
		if c.applyDirRow(ctx, sess, "p") {
			t.Fatal("session row applied")
		}
	})

	t.Run("unsigned never writes identity", func(t *testing.T) {
		c := mkCore()
		raw, pub := mintDirRow(t, "ghost", now, nil)
		raw = stripSig(t, raw, func(r *bus.DirRow) { r.Sig = "" })
		if !c.applyDirRow(ctx, raw, "p") {
			t.Fatal("unsigned row not applied")
		}
		if got := dirField(t, c, "ghost", "pub_key"); got != "" {
			t.Fatalf("unsigned row wrote pub_key %q (claimed %q)", got, pub)
		}
	})

	t.Run("future seen_at clamps", func(t *testing.T) {
		c := mkCore()
		raw, _ := mintDirRow(t, "ghost", now+3600, nil)
		if !c.applyDirRow(ctx, raw, "p") {
			t.Fatal("future row rejected instead of clamped")
		}
		var seen int64
		if err := c.db.QueryRow(`SELECT seen_at FROM employee_cache WHERE id='ghost'`).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		if seen > now+int64(dirFutureSkew.Seconds())+2 {
			t.Fatalf("seen_at %d exceeded the clamp", seen)
		}
	})
}

func TestDirStampsAndServedRows(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "node-a")
	ctx := context.Background()
	// A gossip-layer row (signed, stored by apply) hashes from its verbatim
	// bytes; a column-only row synthesizes unsigned; a session row never
	// enters the table at all.
	raw, _ := mintDirRow(t, "signed-node", time.Now().Unix(), nil)
	c.applyDirRow(ctx, raw, "p")
	if _, err := c.db.Exec(`INSERT INTO employee_cache (id, name, status, last_seen, neighbors_json)
		VALUES ('plain-node', 'plain', 'online', ?, '["x"]')`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO employee_cache (id, status) VALUES ('laptop-1a2b3c4d','online')`); err != nil {
		t.Fatal(err)
	}
	stamps, err := c.dirStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stamps["signed-node"]; !ok {
		t.Fatal("signed row missing from stamps")
	}
	if _, ok := stamps["plain-node"]; !ok {
		t.Fatal("plain row missing from stamps")
	}
	if _, ok := stamps["laptop-1a2b3c4d"]; ok {
		t.Fatal("session row leaked into stamps")
	}
	// dirRowBytes serves verbatim for the signed row (sig survives).
	b, ok := c.dirRowBytes(ctx, "signed-node")
	if !ok {
		t.Fatal("signed row not served")
	}
	var r bus.DirRow
	if err := json.Unmarshal(b, &r); err != nil || !bus.VerifyDirRow(&r) {
		t.Fatal("served signed row does not verify")
	}
	// …and unsigned synthesis for the plain row.
	b, ok = c.dirRowBytes(ctx, "plain-node")
	if !ok {
		t.Fatal("plain row not served")
	}
	var r2 bus.DirRow
	if err := json.Unmarshal(b, &r2); err != nil || r2.Sig != "" {
		t.Fatal("synthesized row should be unsigned")
	}
	// Serving a session row refuses.
	if _, ok := c.dirRowBytes(ctx, "laptop-1a2b3c4d"); ok {
		t.Fatal("session row served")
	}
}

func TestDirPullRateLimits(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "node-a")
	// Outbound: one fresh pull allowed, second inside 30s blocked,
	// continuations bounded by page cap.
	if !c.dirPullAllowed("peer", "") {
		t.Fatal("first fresh pull denied")
	}
	if c.dirPullAllowed("peer", "") {
		t.Fatal("second fresh pull inside 30s allowed")
	}
	for i := 0; i < dirPullMaxPages; i++ {
		if !c.dirPullAllowed("peer", "cursor") {
			t.Fatalf("continuation %d denied", i)
		}
	}
	if c.dirPullAllowed("peer", "cursor") {
		t.Fatal("continuation beyond page cap allowed")
	}
	// Responder side mirrors the same budget.
	c2 := newCoreOnDB(t, openTestDB(t), "node-b")
	if !c2.dirAnswerAllowed("peer", "") {
		t.Fatal("first inbound pull denied")
	}
	if c2.dirAnswerAllowed("peer", "") {
		t.Fatal("second inbound pull inside 30s allowed")
	}
}

// TestDirSyncEndToEnd: A holds a signed row for a third node; B's pull must
// learn it — signature verified on arrival — and the want direction must
// carry B's rows back to A, all over the live WS edge.
func TestDirSyncEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a := newCore(t, "da", "127.0.0.1:18140")
	b := newCore(t, "db", "127.0.0.1:18141")
	ad := registerAndListen(t, ctx, a, "127.0.0.1:18140")
	linkPair(t, ctx, b, a, "127.0.0.1:18140", ad)

	// Seed a signed third-party row into A's directory.
	card, _ := json.Marshal(ledger.CapabilitySummary{Device: "ghost", NodeKind: "physical", NativeIDs: []string{"sys:info"}})
	raw, gpub := mintDirRow(t, "ghost", time.Now().Unix(), func(r *bus.DirRow) {
		r.Card = card
		r.XPub = hex.EncodeToString(make([]byte, 32))
	})
	if !a.applyDirRow(ctx, raw, "seed") {
		t.Fatal("seed row rejected")
	}

	// B pulls; the exchange runs over the live conn.
	b.sendDirPull("da", "")
	pollPub := func() string {
		var v string
		_ = b.db.QueryRow(`SELECT COALESCE(pub_key,'') FROM employee_cache WHERE id='ghost'`).Scan(&v)
		return v
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pollPub() != gpub {
		time.Sleep(30 * time.Millisecond)
	}
	if got := pollPub(); got != gpub {
		t.Fatalf("B never learned ghost's key: %q", got)
	}
	if got := dirField(t, b, "ghost", "learned_via"); got != "da" {
		t.Fatalf("learned_via = %q, want da", got)
	}
	if got := dirField(t, b, "ghost", "dir_sig"); got == "" {
		t.Fatal("row lost its signature in transit")
	}
	// Want direction: A should have learned B's self row back.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := a.db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id='db' AND dir_sig != ''`).Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id='db' AND dir_sig != ''`).Scan(&n)
	if n != 1 {
		t.Fatal("A never received B's signed row via want")
	}
}
