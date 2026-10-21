// SPDX-License-Identifier: AGPL-3.0-or-later

package core

// AutoNAT-style reachability proof (cross-network mesh design M2 / P5).
//
// A rendezvous role is the one capability a node could claim and not have:
// "dial me here" is worthless if nobody can dial it, and a mesh that
// believes the claim routes first contact through a hole. So the claim is
// proven the way the design says — by asking peers to try:
//
//	claimant ──dial_check{addr, nonce}──▶ peer
//	peer     ──dial_check_ack{nonce, ok}──▶ claimant
//
// The claimant counts DISTINCT acks; two are required before `auto` mode
// advertises the role, and the proof expires so a node that loses its port
// mapping stops claiming. The probing side dials under the same source gate
// the reachability ladder uses: only a globally routable address is ever
// touched, so a member cannot turn this into a port scanner aimed at our
// LAN.

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
)

const (
	// dialCheckEvery paces the proof refresh. Twelve minutes is comfortably
	// shorter than the proof's own lifetime, so the claim never lapses while
	// the node is healthy.
	dialCheckEvery = 12 * time.Minute
	// dialCheckProofTTL bounds how long an ack keeps the claim alive. A NAT
	// mapping that silently dies is exactly the failure this catches.
	dialCheckProofTTL = 30 * time.Minute
	// dialCheckProbes is how many peers are asked per round: the design's
	// "≥2 independent confirmations", plus one spare for a peer that is
	// itself about to vanish.
	dialCheckProbes = 3
	// dialCheckNeed is the distinct-ack count that upgrades the claim.
	dialCheckNeed = 2
	// dialCheckTimeout bounds one outbound probe. It is a TCP connect to a
	// public address, nothing more.
	dialCheckTimeout = 4 * time.Second
)

// handleDialCheck serves a peer's proof request: dial the address it
// advertised, once, and say what happened. The address is untrusted input —
// a member could name anything — so the gate is the same one the ladder
// applies to directory addrs: globally routable targets only, one probe per
// source per window.
func (c *Core) handleDialCheck(ctx context.Context, env bus.Envelope) {
	var p bus.DialCheckPayload
	if err := env.PayloadInto(&p); err != nil || p.Addr == "" || p.Nonce == "" {
		return
	}
	host, port, err := net.SplitHostPort(p.Addr)
	if err != nil || port == "" {
		c.sendDialAck(ctx, env.From, p.Nonce, false)
		return
	}
	if !publicDialTarget(host) {
		// Private, loopback and overlay addresses are never probed. The
		// probe is a favour between members, not a scanning service.
		c.sendDialAck(ctx, env.From, p.Nonce, false)
		return
	}
	if !c.dialCheckAllow(env.From) {
		c.logger.Debug("dial_check: rate limited", "peer", env.From)
		return
	}
	d := net.Dialer{Timeout: dialCheckTimeout}
	conn, derr := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if derr == nil {
		_ = conn.Close()
	}
	c.sendDialAck(ctx, env.From, p.Nonce, derr == nil)
}

// sendDialAck answers a probe. ok=false is a real answer, not a failure to
// send: "I could not reach you" is what lets the claimant stop claiming.
func (c *Core) sendDialAck(ctx context.Context, to, nonce string, ok bool) {
	if to == "" {
		return
	}
	msgID, err := newUUID()
	if err != nil {
		return
	}
	env, err := bus.NewEnvelope(bus.MsgDialCheckAck, c.nodeID, msgID,
		bus.DialCheckAckPayload{Nonce: nonce, OK: ok, Proto: "ws"})
	if err != nil {
		return
	}
	env.To = to
	if err := c.sendTo(to, env); err != nil {
		c.logger.Debug("dial_check: ack unroutable", "peer", to, "err", err)
	}
}

// dialCheckAllow bounds probes per probing peer: each one costs an outbound
// connection to an address the PEER supplied.
func (c *Core) dialCheckAllow(peer string) bool {
	c.path.mu.Lock()
	defer c.path.mu.Unlock()
	c.path.init()
	if c.path.dialProbeAt == nil {
		c.path.dialProbeAt = make(map[string]time.Time)
	}
	if last, ok := c.path.dialProbeAt[peer]; ok && time.Since(last) < time.Minute {
		return false
	}
	c.path.dialProbeAt[peer] = time.Now()
	return true
}

// handleDialCheckAck records a confirmation. Distinct peers are what count —
// one friendly node answering five times proves one path, not five. The ack
// must also answer a probe THIS node sent: a nonce nobody issued is not
// evidence that anyone could dial us, so it never reaches provenBy.
func (c *Core) handleDialCheckAck(ctx context.Context, env bus.Envelope) {
	var p bus.DialCheckAckPayload
	if err := env.PayloadInto(&p); err != nil || p.Nonce == "" || env.From == "" {
		return
	}
	key := dialProbeKey(env.From, p.Nonce)
	c.rzv.mu.Lock()
	sentAt, ok := c.rzv.probes[key]
	if ok {
		delete(c.rzv.probes, key)
	}
	// Bound the pending table even when acks never arrive (a peer that
	// vanished mid-probe).
	for k, at := range c.rzv.probes {
		if time.Since(at) > dialCheckProofTTL {
			delete(c.rzv.probes, k)
		}
	}
	c.rzv.mu.Unlock()
	if !ok {
		c.logger.Debug("dial_check_ack for an unissued probe", "peer", env.From, "nonce", p.Nonce)
		return
	}
	if time.Since(sentAt) > dialCheckProofTTL {
		c.logger.Debug("dial_check_ack arrived after the probe lease", "peer", env.From)
		return
	}
	if !p.OK {
		c.mesh.dialCheckFail.Add(1)
		return
	}
	c.mesh.dialCheckOK.Add(1)
	c.rzv.mu.Lock()
	if c.rzv.provenBy == nil {
		c.rzv.provenBy = make(map[string]time.Time)
	}
	c.rzv.provenBy[env.From] = time.Now()
	// Prune, then judge.
	for peer, at := range c.rzv.provenBy {
		if time.Since(at) > dialCheckProofTTL {
			delete(c.rzv.provenBy, peer)
		}
	}
	n := len(c.rzv.provenBy)
	was := c.rzv.proven
	c.rzv.proven = n >= dialCheckNeed
	nowProven := c.rzv.proven
	c.rzv.mu.Unlock()
	if nowProven != was {
		if nowProven {
			c.logger.Info("rendezvous role proven by callbacks", "peers", n)
		} else {
			c.logger.Warn("rendezvous proof lost — the claim is withdrawn")
		}
	}
}

// dialProbeKey identifies one outstanding probe. NUL cannot appear in a
// peer id or a UUID, so the concatenation is unambiguous.
func dialProbeKey(peer, nonce string) string { return peer + "\x00" + nonce }

// noteDialProbe records an issued probe before it goes on the wire, so an ack
// can be matched to a request this node actually made.
func (c *Core) noteDialProbe(peer, nonce string) {
	c.rzv.mu.Lock()
	if c.rzv.probes == nil {
		c.rzv.probes = make(map[string]time.Time)
	}
	c.rzv.probes[dialProbeKey(peer, nonce)] = time.Now()
	c.rzv.mu.Unlock()
}

// dialCheckDue reports whether a proof round should run now, and marks it.
func (c *Core) dialCheckDue() bool {
	c.rzv.mu.Lock()
	defer c.rzv.mu.Unlock()
	if time.Since(c.rzv.lastProof) < dialCheckEvery {
		return false
	}
	c.rzv.lastProof = time.Now()
	return true
}

// rendezvousProofValid reports whether the callback proof currently holds.
func (c *Core) rendezvousProofValid() bool {
	c.rzv.mu.Lock()
	defer c.rzv.mu.Unlock()
	for peer, at := range c.rzv.provenBy {
		if time.Since(at) > dialCheckProofTTL {
			delete(c.rzv.provenBy, peer)
		}
	}
	c.rzv.proven = len(c.rzv.provenBy) >= dialCheckNeed
	return c.rzv.proven
}

// runDialCheck asks a few peers to dial this node's advertised address. Only
// the address the capability summary publishes is probed — proving an
// address we do not advertise would prove nothing.
func (c *Core) runDialCheck(ctx context.Context) {
	addr := c.publicListenAddr()
	if addr == "" {
		return
	}
	c.mu.RLock()
	peers := make([]string, 0, dialCheckProbes)
	for id, p := range c.peers {
		if p.conn == nil || id == c.nodeID {
			continue
		}
		peers = append(peers, id)
		if len(peers) >= dialCheckProbes {
			break
		}
	}
	c.mu.RUnlock()
	for _, peer := range peers {
		nonce, err := newUUID()
		if err != nil {
			continue
		}
		env, err := bus.NewEnvelope(bus.MsgDialCheck, c.nodeID, nonce,
			bus.DialCheckPayload{Addr: addr, UDP: c.localUDPPort(), Nonce: nonce})
		if err != nil {
			continue
		}
		env.To = peer
		c.noteDialProbe(peer, nonce)
		if err := c.sendTo(peer, env); err != nil {
			c.logger.Debug("dial_check: probe unroutable", "peer", peer, "err", err)
		}
	}
}

// publicListenAddr returns the dialable ws address this node advertises, or
// "" when the listener is bound to a wildcard/loopback host — the operator
// has to name a routable address before the claim can be proven.
func (c *Core) publicListenAddr() string {
	for _, a := range c.selfAddrs() {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if publicDialTarget(host) {
			return a
		}
	}
	return ""
}

// localUDPPort reports the datagram-plane port for the probe payload, or 0.
func (c *Core) localUDPPort() int {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if c.udp == nil {
		return 0
	}
	_, portStr, err := net.SplitHostPort(c.udp.LocalAddr().String())
	if err != nil {
		return 0
	}
	port := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}

// publicDialTarget is the probe gate: a host a stranger could legitimately
// reach. Names pass (the operator's promise); routed addresses must be
// global unicast and outside every private, loopback and overlay band.
func publicDialTarget(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // a hostname, not an address literal
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !isSharedAddr(ip) && !ip.IsLoopback()
}
