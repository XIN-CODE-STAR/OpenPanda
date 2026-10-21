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
