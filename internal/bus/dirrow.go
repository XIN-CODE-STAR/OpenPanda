// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// DirRowMaxBytes bounds one gossiped row. The capability summary inside is
// the variable-sized member; a generous-but-finite ceiling keeps a crafted
// blob from eating the whole dir_rows budget in one entry.
const DirRowMaxBytes = 16 * 1024

// dirRowCanonical marshals a DirRow deterministically for the two crypto
// domains. Go's encoding/json orders struct fields by declaration and map
// keys lexically, RawMessage passes through verbatim — so the bytes a signer
// hashed are the bytes every receiver reproduces after a parse round-trip.
//
//	withSeen=true  → signature domain: everything except Sig
//	withSeen=false → content-hash domain: also excludes SeenAt and the
//	                 measured link latencies
func dirRowCanonical(r *DirRow, withSeen bool) []byte {
	c := *r
	c.Sig = ""
	if !withSeen {
		c.SeenAt = 0
		c.Links = stableLinks(c.Links)
		c.Card = stableCard(c.Card)
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return nil
	}
	return b
}

// stableLinks drops the measured latency from a row's link metrics. The RTT
// moves on every ping, and the content hash is what two peers compare to
// decide whether their directories agree: hashing the latency would make
// the hash move with it, and the two sides would never meet.
func stableLinks(links []LinkMetric) []LinkMetric {
	if len(links) == 0 {
		return links
	}
	out := make([]LinkMetric, len(links))
	copy(out, links)
	for i := range out {
		out[i].RTTms = 0
	}
	return out
}

// stableCard drops the measured latency from a capability card. The card
// embeds the same link metrics (links[].rtt_ms) for consumers that read the
// summary; the key is REMOVED rather than zeroed so a link with a sample and
// a link without one normalize to the same bytes.
func stableCard(card json.RawMessage) json.RawMessage {
	if len(card) == 0 {
		return card
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(card, &m); err != nil {
		return card
	}
	raw, ok := m["links"]
	if !ok {
		return card
	}
	var links []map[string]any
	if err := json.Unmarshal(raw, &links); err != nil {
		return card
	}
	for _, l := range links {
		delete(l, "rtt_ms")
	}
	norm, err := json.Marshal(links)
	if err != nil {
		return card
	}
	m["links"] = norm
	out, err := json.Marshal(m)
	if err != nil {
		return card
	}
	return out
}

// CanonicalDirRowBytes returns the full-field serialization — the byte
// string stored as a row's gossip-layer copy and forwarded in dir_rows,
// signature and seen_at included. The stored bytes are always THIS form so
// the hash a peer recomputes from them equals the hash stamped at mint time.
func CanonicalDirRowBytes(r *DirRow) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// DirRowContentHash is the change-detection digest the stamp table and
// dir_digest compare (M1 §2): SHA-256 over the canonical row minus seen_at
// and sig. Freshness churn (a re-minted seen_at) leaves the hash untouched,
// so two peers holding the same CONTENT agree even when their copies were
// signed at different instants.
func DirRowContentHash(r *DirRow) []byte {
	sum := sha256.Sum256(dirRowCanonical(r, false))
	return sum[:]
}

// DirRowContentHashHex is ContentHash as the lowercase hex the stamp table
// and pull payload carry.
func DirRowContentHashHex(r *DirRow) string {
	return hex.EncodeToString(DirRowContentHash(r))
}

// SignDirRow mints the owner's attestation over the canonical row (seen_at
// included — a gossiped row's freshness is source-asserted, so a forwarder
// cannot rewind or fast-forward it). The signature covers the row exactly as
// DirRowContentHash peers will later re-derive: Sig cleared, struct order
// fixed. Sets r.Sig; r.PubKey must already hold the matching public key.
func SignDirRow(priv ed25519.PrivateKey, r *DirRow) {
	r.Sig = hex.EncodeToString(ed25519.Sign(priv, dirRowCanonical(r, true)))
}

// VerifyDirRow checks r.Sig against the canonical row under r.PubKey. A row
// that fails is not "unsigned" — it is corrupt, and callers must drop rather
// than downgrade it: unsigned rows lose every conflict to signed ones anyway.
func VerifyDirRow(r *DirRow) bool {
	if r == nil || r.Sig == "" || r.PubKey == "" {
		return false
	}
	pub, err := hex.DecodeString(r.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, dirRowCanonical(r, true), sig)
}

// ParseDirRow validates a row blob pulled off the wire: JSON shape, sane id,
// size ceiling enforced by the caller's payload budget. It does NOT check the
// signature — callers branch on Sig=="" vs VerifyDirRow because unsigned rows
// remain legal gossip (a direct observation has no signature to forward).
func ParseDirRow(raw []byte) (*DirRow, error) {
	if len(raw) == 0 || len(raw) > DirRowMaxBytes {
		return nil, fmt.Errorf("dir row size %d out of bounds", len(raw))
	}
	var r DirRow
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("dir row parse: %w", err)
	}
	if r.ID == "" || len(r.ID) > 128 {
		return nil, fmt.Errorf("dir row id %q out of bounds", r.ID)
	}
	return &r, nil
}

// DirDigestFromStamps folds a stamp table (id → content_hash hex) into the
// single heartbeat digest. The input set IS the definition — both sides of a
// compare must build it with the same row filter (gossipable, session rows
// excluded) or the digests never meet.
func DirDigestFromStamps(stamps map[string]string) string {
	ids := make([]string, 0, len(stamps))
	for id := range stamps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte(":"))
		h.Write([]byte(stamps[id]))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
