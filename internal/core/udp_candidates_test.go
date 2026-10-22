// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"net"
	"strings"
	"testing"
)

// TestUDPCandidatesCarryTheNATPort pins the endpoint a NAT-bound node
// advertises to a punching peer.
//
// The candidate list is what the other side sprays at. A NAT rewrites the
// external port of the UDP mapping (measured on a real home uplink: local
// 17837 became public 8690), so the public endpoint of the mapping is
// public-host:public-PORT — and the port is part of the address, not a
// detail. A candidate that pairs the public host with the LOCAL port names
// a port the mapping does not live on, so the punch is sprayed at a socket
// that does not exist and the hole never opens.
//
// The reflexive address STUN hands back already carries the right port and
// must be advertised verbatim.
func TestUDPCandidatesCarryTheNATPort(t *testing.T) {
	c := newCore(t, "cand-node", "127.0.0.1:17999")
	c.udpMu.Lock()
	c.udpPort = 17837
	// What a STUN answer looks like: the public host with the port the NAT
	// actually assigned to the mapping.
	c.udpReflexive = "203.0.113.5:41234"
	c.observedIP = "203.0.113.9"
	c.udpMu.Unlock()

	got := c.udpCandidates()
	joined := strings.Join(got, ",")

	if !strings.Contains(joined, "203.0.113.5:41234") {
		t.Fatalf("candidates %v do not carry the reflexive endpoint 203.0.113.5:41234 — "+
			"the NAT-mapped port is dropped, so a peer would spray a port that does not exist", got)
	}
	// The observed host still belongs in the list, but it is the observed
	// HOST — it carries no port information of its own.
	if !strings.Contains(joined, "203.0.113.9:") {
		t.Fatalf("candidates %v lost the peer-observed host 203.0.113.9", got)
	}
	// And no candidate may pair the public host with the LOCAL port: that
	// endpoint is unreachable through the NAT.
	for _, cand := range got {
		host, port, err := net.SplitHostPort(cand)
		if err != nil {
			t.Fatalf("candidate %q is not host:port: %v", cand, err)
		}
		if host == "203.0.113.5" && port == "17837" {
			t.Fatalf("candidate %q pairs the public host with the local port — the NAT "+
				"does not map it there", cand)
		}
	}
}
