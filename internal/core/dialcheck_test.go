// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
)

// TestRendezvousProofNeedsDistinctPeers: one friendly node answering twice
// proves one path, not two. The role upgrades only at the distinct-peer
// threshold.
func TestRendezvousProofNeedsDistinctPeers(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "claimant")
	if c.rendezvousProofValid() {
		t.Fatal("a fresh node must not start proven")
	}
	ack := func(from string) bus.Envelope {
		raw, _ := json.Marshal(bus.DialCheckAckPayload{Nonce: "n", OK: true, Proto: "ws"})
		return bus.Envelope{V: 2, Type: bus.MsgDialCheckAck, From: from, MsgID: "m-" + from, Payload: raw}
	}
	c.handleDialCheckAck(ctx, ack("peer-a"))
	c.handleDialCheckAck(ctx, ack("peer-a"))
	if c.rendezvousProofValid() {
		t.Fatal("repeat acks from one peer must not prove the claim")
	}
	c.handleDialCheckAck(ctx, ack("peer-b"))
	if !c.rendezvousProofValid() {
		t.Fatal("two distinct acks must prove the claim")
	}
}

// TestRendezvousProofExpires: a NAT mapping that dies takes the claim with
// it — the proof is a lease, not a trophy.
func TestRendezvousProofExpires(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "claimant")
	for _, from := range []string{"peer-a", "peer-b"} {
		raw, _ := json.Marshal(bus.DialCheckAckPayload{Nonce: "n", OK: true})
		c.handleDialCheckAck(ctx, bus.Envelope{V: 2, Type: bus.MsgDialCheckAck, From: from, Payload: raw})
	}
	if !c.rendezvousProofValid() {
		t.Fatal("setup: proof should hold")
	}
	c.rzv.mu.Lock()
	for peer := range c.rzv.provenBy {
		c.rzv.provenBy[peer] = time.Now().Add(-dialCheckProofTTL - time.Second)
	}
	c.rzv.mu.Unlock()
	if c.rendezvousProofValid() {
		t.Fatal("expired proof must not hold")
	}
}

// TestDialCheckAckRejectsFailure: a "could not reach you" answer never
// counts toward the claim.
func TestDialCheckAckRejectsFailure(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "claimant")
	for _, from := range []string{"peer-a", "peer-b"} {
		raw, _ := json.Marshal(bus.DialCheckAckPayload{Nonce: "n", OK: false})
		c.handleDialCheckAck(ctx, bus.Envelope{V: 2, Type: bus.MsgDialCheckAck, From: from, Payload: raw})
	}
	if c.rendezvousProofValid() {
		t.Fatal("negative acks must not prove anything")
	}
}

// TestPublicDialTarget: the probe gate refuses everything that is not a
// stranger-reachable endpoint, so a member cannot aim it at our LAN.
func TestPublicDialTarget(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"203.0.113.5", true},
		{"node.example.com", true},
		{"2001:db8::5", true},
		{"192.168.1.20", false},
		{"10.0.0.4", false},
		{"127.0.0.1", false},
		{"100.101.102.103", false},
		{"fd7a:115e:a214::1", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := publicDialTarget(tc.host); got != tc.want {
			t.Fatalf("publicDialTarget(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestDialCheckProbeRateLimit: one probe per source per minute, so the
// favour cannot be spent as a port scanner.
func TestDialCheckProbeRateLimit(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "prober")
	if !c.dialCheckAllow("peer-a") {
		t.Fatal("first probe must be allowed")
	}
	if c.dialCheckAllow("peer-a") {
		t.Fatal("second probe inside the window must be refused")
	}
	if !c.dialCheckAllow("peer-b") {
		t.Fatal("a different peer has its own budget")
	}
}

// TestDialCheckDuePacing: the proof round runs on its own clock, not on the
// monitor tick.
func TestDialCheckDuePacing(t *testing.T) {
	c := newCoreOnDB(t, openTestDB(t), "claimant")
	if !c.dialCheckDue() {
		t.Fatal("first round must be due")
	}
	if c.dialCheckDue() {
		t.Fatal("a second round inside the interval must not be due")
	}
	c.rzv.mu.Lock()
	c.rzv.lastProof = time.Now().Add(-dialCheckEvery - time.Second)
	c.rzv.mu.Unlock()
	if !c.dialCheckDue() {
		t.Fatal("round must come due again after the interval")
	}
}
