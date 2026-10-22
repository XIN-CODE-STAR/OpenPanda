// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func testDirRow(t *testing.T) (*DirRow, string) {
	t.Helper()
	pub, priv, err := GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &DirRow{
		ID:        "node-a",
		PubKey:    hex.EncodeToString(pub),
		XPub:      hex.EncodeToString(make([]byte, 32)),
		Ver:       "1.2.3",
		Addrs:     []string{"10.0.0.1:7836"},
		Neighbors: []string{"node-b"},
		UDPPort:   7837,
		SeenAt:    time.Now().Unix(),
	}
	SignDirRow(priv, r)
	return r, ""
}

func TestDirRowSignVerifyRoundTrip(t *testing.T) {
	r, _ := testDirRow(t)
	if r.Sig == "" {
		t.Fatal("sign produced empty sig")
	}
	if !VerifyDirRow(r) {
		t.Fatal("signed row failed verify")
	}
	// Canonical bytes must re-parse into an identical-verifying row: this is
	// the property byte-for-byte forwarding depends on.
	canon := CanonicalDirRowBytes(r)
	var back DirRow
	if err := json.Unmarshal(canon, &back); err != nil {
		t.Fatal(err)
	}
	if !VerifyDirRow(&back) {
		t.Fatal("row fails verify after canonical round-trip")
	}
	if got := CanonicalDirRowBytes(&back); string(got) != string(canon) {
		t.Fatal("canonical bytes not stable under re-marshal")
	}
}

func TestDirRowContentHashExcludesFreshness(t *testing.T) {
	r, _ := testDirRow(t)
	h1 := DirRowContentHashHex(r)
	r.SeenAt += 5000
	r.Sig = "deadbeeff00d" // sig churn must not move the hash either
	if h2 := DirRowContentHashHex(r); h1 != h2 {
		t.Fatalf("content hash moved on freshness churn: %s → %s", h1, h2)
	}
	r.Addrs = append(r.Addrs, "10.0.0.2:7836")
	if h3 := DirRowContentHashHex(r); h3 == h1 {
		t.Fatal("content hash ignored a content change")
	}
}

func TestDirRowVerifyRejectsTamper(t *testing.T) {
	r, _ := testDirRow(t)
	r.Neighbors = append(r.Neighbors, "evil-node")
	if VerifyDirRow(r) {
		t.Fatal("tampered row still verifies")
	}
	r2, _ := testDirRow(t)
	r2.Sig = ""
	if VerifyDirRow(r2) {
		t.Fatal("unsigned row 'verifies'")
	}
}

func TestDirRowSigCoversSeenAt(t *testing.T) {
	r, _ := testDirRow(t)
	r.SeenAt += 1 // rewind attack: fresher claim under an old signature
	if VerifyDirRow(r) {
		t.Fatal("seen_at is outside the signature domain")
	}
}

func TestDirDigestDeterministic(t *testing.T) {
	a := map[string]string{"n1": "aa", "n2": "bb", "n3": "cc"}
	b := map[string]string{"n3": "cc", "n1": "aa", "n2": "bb"}
	if DirDigestFromStamps(a) != DirDigestFromStamps(b) {
		t.Fatal("digest is insertion-order dependent")
	}
	c := map[string]string{"n1": "aa", "n2": "bb"}
	if DirDigestFromStamps(a) == DirDigestFromStamps(c) {
		t.Fatal("digest ignores a missing row")
	}
	d := map[string]string{"n1": "aa", "n2": "bb", "n3": "dd"}
	if DirDigestFromStamps(a) == DirDigestFromStamps(d) {
		t.Fatal("digest ignores a content change")
	}
}

func TestDirRowContentHashExcludesLatency(t *testing.T) {
	// The measured RTT moves on every ping. If the content hash followed it,
	// two peers holding the same directory would never agree on the digest
	// and would re-sync forever.
	card := json.RawMessage(`{"device":"n","links":[{"peer":"node-b","rtt_ms":12,"kind":"ws"}]}`)
	r := &DirRow{
		ID:    "node-a",
		Card:  card,
		Links: []LinkMetric{{Peer: "node-b", RTTms: 12, Kind: "ws"}},
	}
	h1 := DirRowContentHashHex(r)
	r.Links[0].RTTms = 987
	r.Card = json.RawMessage(`{"device":"n","links":[{"peer":"node-b","rtt_ms":987,"kind":"ws"}]}`)
	if h2 := DirRowContentHashHex(r); h1 != h2 {
		t.Fatalf("content hash moved with the latency: %s vs %s", h1, h2)
	}
	// A link without a sample must normalize to the same bytes as one with
	// a measured latency.
	r.Links[0].RTTms = 0
	r.Card = json.RawMessage(`{"device":"n","links":[{"peer":"node-b","kind":"ws"}]}`)
	if h3 := DirRowContentHashHex(r); h1 != h3 {
		t.Fatalf("an unmeasured link hashes differently from a measured one: %s vs %s", h1, h3)
	}
	// The signature still covers the latency: the hash domain is narrower
	// than the signature domain on purpose.
	pub, priv, err := GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	s := &DirRow{ID: "node-a", PubKey: hex.EncodeToString(pub), Links: []LinkMetric{{Peer: "node-b", RTTms: 12}}}
	SignDirRow(priv, s)
	if !VerifyDirRow(s) {
		t.Fatal("signed row failed verify")
	}
	s.Links[0].RTTms = 13
	if VerifyDirRow(s) {
		t.Fatal("a tampered latency must break the signature")
	}
}

func TestParseDirRowBounds(t *testing.T) {
	if _, err := ParseDirRow(nil); err == nil {
		t.Fatal("empty row parsed")
	}
	if _, err := ParseDirRow([]byte(`{"id":""}`)); err == nil {
		t.Fatal("id-less row parsed")
	}
	big := make([]byte, DirRowMaxBytes+1)
	if _, err := ParseDirRow(big); err == nil {
		t.Fatal("oversized row parsed")
	}
	r, _ := testDirRow(t)
	raw := CanonicalDirRowBytes(r)
	if _, err := ParseDirRow(raw); err != nil {
		t.Fatalf("canonical row failed parse: %v", err)
	}
}

// TestDirRowContentHashIgnoresOrder pins the second half of content-hash
// stability: the live adjacency and its link metrics are built by ranging
// over maps, so two mints of the same directory state carry the same elements
// in a different order. The hash must not move with that order (or with RTT),
// while a genuine content change must still move it.
func TestDirRowContentHashIgnoresOrder(t *testing.T) {
	cardA := json.RawMessage(`{"device":"n","neighbors":["node-b","node-c"],"links":[{"peer":"node-b","rtt_ms":12,"kind":"ws"},{"peer":"node-c","kind":"udp"}]}`)
	cardB := json.RawMessage(`{"device":"n","neighbors":["node-c","node-b"],"links":[{"peer":"node-c","rtt_ms":99,"kind":"udp"},{"peer":"node-b","rtt_ms":4,"kind":"ws"}]}`)
	a := &DirRow{
		ID:        "node-a",
		Card:      cardA,
		Neighbors: []string{"node-b", "node-c"},
		Links: []LinkMetric{
			{Peer: "node-b", RTTms: 12, Kind: "ws"},
			{Peer: "node-c", Kind: "udp"},
		},
	}
	b := &DirRow{
		ID:        "node-a",
		Card:      cardB,
		Neighbors: []string{"node-c", "node-b"},
		Links: []LinkMetric{
			{Peer: "node-c", RTTms: 99, Kind: "udp"},
			{Peer: "node-b", RTTms: 4, Kind: "ws"},
		},
	}
	if ha, hb := DirRowContentHashHex(a), DirRowContentHashHex(b); ha != hb {
		t.Fatalf("content hash follows array order: %s vs %s", ha, hb)
	}
	// A real change (one neighbor replaced) must still be visible.
	b.Neighbors = []string{"node-c", "node-d"}
	if DirRowContentHashHex(a) == DirRowContentHashHex(b) {
		t.Fatal("content hash ignored a changed neighbor set")
	}
}
