// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// Reachability ladder (cross-network mesh M3): when sendTo finds no live
// track to a peer it kicks this path in the background — directory addrs
// first, then automatic hole punching — so the next send can ride a direct
// edge instead of a relay. Transit and the outbox remain the deeper
// fallbacks; the ladder exists to converge the mesh toward directness, not
// to make the current send wait on a dial.
//
// The addr source is the directory row's self-advertised addrs, gated hard:
// only signed rows (gossip credentials) or directly-learned rows feed the
// dial list, and private/loopback candidates are never dialed on an indirect
// row — a remote gossiper must not be able to aim our dials at our own LAN.
// Per-address cooldowns and a global concurrency cap keep a poisoned row or
// a flapping peer from spending the dial budget.

const (
	// reachKickEvery dedupes ladder runs per peer: one in-flight or recent
	// attempt satisfies every sender that fails in the same window.
	reachKickEvery = 30 * time.Second
	// addrDialCooldown silences a failed (peer,addr) pair — the address is
	// a hint, not a promise, and redialing a dead one every send is exactly
	// the spray the budget exists to stop.
	addrDialCooldown = 5 * time.Minute
	// addrDialMax bounds simultaneous outbound dial attempts.
	addrDialMax = 4
	// addrDialTimeout bounds one candidate's connect+hello before the next
	// is tried.
	addrDialTimeout = 4 * time.Second
	// autoPunchEvery is the minimum offer interval per peer — punching is a
	// UDP spray, so the budget is deliberately coarser than a WS dial's.
	autoPunchEvery = 60 * time.Second
	// punchWantEvery rate-limits how often we ASK the elected offerer to
	// punch us — a want is cheap but unthrottled it is a forgeable spray
	// trigger on the small side.
	punchWantEvery = 60 * time.Second
	// natSuspectWindow is how long a peer with two failed punch windows is
	// considered un-punchable: NATs that refused two full handshake rounds
	// almost always refuse the third, and the quiet period is what makes
	// outbox/DTN the honest path instead of endless spray.
	natSuspectWindow = 10 * time.Minute
	// natSuspectAfter is the failed-window count that raises suspicion.
	natSuspectAfter = 2
)

// pathState holds the reachability ladder's bookkeeping.
type pathState struct {
	mu         sync.Mutex
	kickAt     map[string]time.Time
	addrCool   map[string]time.Time
	dialSem    chan struct{}
	punchAt    map[string]time.Time
	punchTries map[string]int
	natSuspect map[string]time.Time
	wantAt     map[string]time.Time
	// edgeLoad caches each peer's heartbeat-advertised live edge count for
	// rendezvous scoring — volatile by nature, so it stays in memory.
	edgeLoad map[string]int
}

func (p *pathState) init() {
	if p.kickAt == nil {
		p.kickAt = make(map[string]time.Time)
		p.addrCool = make(map[string]time.Time)
		p.dialSem = make(chan struct{}, addrDialMax)
		p.punchAt = make(map[string]time.Time)
		p.punchTries = make(map[string]int)
		p.natSuspect = make(map[string]time.Time)
		p.wantAt = make(map[string]time.Time)
		p.edgeLoad = make(map[string]int)
	}
}

// kickReachable starts one background ladder run for id, deduped. Called by
// sendTo whenever a send had to fall back below a live track.
func (c *Core) kickReachable(id string) {
	if id == "" || id == c.nodeID || ledger.SessionRowID(id) {
		return
	}
	c.path.mu.Lock()
	c.path.init()
	if last, ok := c.path.kickAt[id]; ok && time.Since(last) < reachKickEvery {
		c.path.mu.Unlock()
		return
	}
	c.path.kickAt[id] = time.Now()
	c.path.mu.Unlock()
	c.goGuarded("reach "+id, func() { c.reachPeer(context.Background(), id) })
}

// reachPeer walks the ladder: direct addrs first, auto-punch after. Transit
// is not a rung here — sendTo already tried it; this function's job is to
// manufacture the cheaper tracks transit stands in for.
func (c *Core) reachPeer(ctx context.Context, id string) {
	if c.connFor(id) != nil || c.UDPRoute(id) != nil {
		return
	}
	if c.dialPeerAddrs(ctx, id) {
		return
	}
	if c.connFor(id) != nil || c.UDPRoute(id) != nil {
		return
	}
	c.autoPunch(ctx, id)
}

// dialPeerAddrs dials the peer's advertised addrs best-first. Returns true
// when a conn to id exists by the end (a wrong-node answer doesn't count —
// the dedup machinery keeps whichever edge the hello proved).
func (c *Core) dialPeerAddrs(ctx context.Context, id string) bool {
	var addrsJSON, learnedVia, dirSig string
	if err := c.db.QueryRowContext(ctx, `
		SELECT COALESCE(addrs,''), COALESCE(learned_via,''), COALESCE(dir_sig,'')
		FROM employee_cache WHERE id=?`, id).Scan(&addrsJSON, &learnedVia, &dirSig); err != nil {
		return false
	}
	var addrs []string
	if err := json.Unmarshal([]byte(addrsJSON), &addrs); err != nil || len(addrs) == 0 {
		return false
	}
	// Addr provenance gate: unsigned gossip rows (learned_via set, no sig)
	// contribute nothing — an indirect unverified claim must never aim a
	// dial. Directly-learned and signed rows both qualify, but only the
	// direct row's private/loopback hints are dialable: a forwarded row's
	// LAN addresses mean nothing on our network.
	indirect := learnedVia != ""
	if indirect && dirSig == "" {
		return false
	}
	sort.SliceStable(addrs, func(i, j int) bool { return addrRank(addrs[i]) < addrRank(addrs[j]) })
	tried := 0
	for _, addr := range addrs {
		if tried >= addrDialMax*2 { // scan cap: skip evaluating a huge list
			break
		}
		if indirect && !addrPublic(addr) {
			continue
		}
		key := id + "|" + addr
		c.path.mu.Lock()
		c.path.init()
		if cool, ok := c.path.addrCool[key]; ok && time.Now().Before(cool) {
			c.path.mu.Unlock()
			continue
		}
		select {
		case c.path.dialSem <- struct{}{}:
		default:
			c.path.mu.Unlock()
			return false // global dial budget spent — punch rung decides now
		}
		c.path.mu.Unlock()
		tried++
		go func(addr string) {
			defer func() { <-c.path.dialSem }()
			dctx, cancel := context.WithTimeout(ctx, addrDialTimeout)
			defer cancel()
			// DialPeer registers whatever answers under ITS hello id; the
			// addr is a hint about id, not a claim we enforce pre-hello.
			if err := c.DialPeer(dctx, addr); err != nil {
				c.path.mu.Lock()
				c.path.addrCool[key] = time.Now().Add(addrDialCooldown)
				c.path.mu.Unlock()
			}
		}(addr)
	}
	if tried == 0 {
		return false
	}
	// Give the fastest dial a moment to prove itself, then check.
	deadline := time.Now().Add(addrDialTimeout + time.Second)
	for time.Now().Before(deadline) {
		if c.connFor(id) != nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return c.connFor(id) != nil
}

// addrRank orders dial candidates: global v6 first (a working v6 path skips
// NAT entirely), then public v4, then everything else.
func addrRank(addr string) int {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return 9
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Hostname: treat as public-v4-class — resolution happens at dial.
		return 5
	}
	if ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsPrivate() {
		return 0
	}
	if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() {
		return 4
	}
	return 8
}

// addrPublic reports whether an addr names a globally-routable endpoint —
// the gate indirect (gossip-learned) rows must pass before we dial them.
func addrPublic(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return true // hostnames resolve to whatever they resolve to
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// autoPunch runs the punching rung: the lexicographically-smaller side is
// the elected offerer (one spray instead of two); the larger side sends a
// punch_want so its perception of the failure isn't wasted waiting. Repeated
// failed windows raise nat_suspect, after which the honest answer is the
// outbox, not more spray.
func (c *Core) autoPunch(ctx context.Context, id string) {
	c.udpMu.Lock()
	hasUDP := c.udp != nil
	port := c.udpPeerPort[id]
	c.udpMu.Unlock()
	if !hasUDP || port <= 0 {
		return
	}
	// A gossip-only peer learned its port at dir-apply time but may carry
	// no spray candidates yet — synthesize them from the directory addrs:
	// the WS hint's IP at the advertised UDP port is the most likely
	// reflexive endpoint. Same provenance gate as the dial rung.
	c.seedPunchCandidates(ctx, id, port)
	c.path.mu.Lock()
	c.path.init()
	if until, ok := c.path.natSuspect[id]; ok && time.Now().Before(until) {
		c.path.mu.Unlock()
		return
	}
	if last, ok := c.path.punchAt[id]; ok {
		if time.Since(last) < autoPunchEvery {
			c.path.mu.Unlock()
			return
		}
	}
	c.path.mu.Unlock()

	if c.nodeID < id {
		// We are the elected offerer. The attempt is marked BEFORE the call:
		// PunchPeer starts the spray session even when the offer envelope
		// has no route yet, so an unroutable offer still counts against the
		// interval and the failure window — the spray itself is the cost.
		c.path.mu.Lock()
		c.path.punchAt[id] = time.Now()
		c.path.mu.Unlock()
		if err := c.PunchPeer(ctx, id); err != nil {
			c.logger.Debug("auto-punch offer", "peer", id, "err", err)
		}
		c.notePunchAttempt(id)
		return
	}
	// The blind end: we saw the failure but the smaller side owns the
	// offerer role. Ask it to punch us — a single routed envelope, no spray.
	c.sendPunchWant(ctx, id)
	c.notePunchAttempt(id)
}

// seedPunchCandidates synthesizes spray targets for a peer we have never
// connected to: each directory addr's IP at the peer's advertised UDP port.
// Indirect (gossip) rows contribute only their public hints — a forwarded
// row's LAN addr means nothing off its own segment, and the unsigned-
// indirect case is refused outright, same as the dial gate.
func (c *Core) seedPunchCandidates(ctx context.Context, id string, udpPort int) {
	var addrsJSON, learnedVia, dirSig string
	if err := c.db.QueryRowContext(ctx, `
		SELECT COALESCE(addrs,''), COALESCE(learned_via,''), COALESCE(dir_sig,'')
		FROM employee_cache WHERE id=?`, id).Scan(&addrsJSON, &learnedVia, &dirSig); err != nil {
		return
	}
	indirect := learnedVia != ""
	if indirect && dirSig == "" {
		return
	}
	var addrs []string
	if err := json.Unmarshal([]byte(addrsJSON), &addrs); err != nil {
		return
	}
	var cands []string
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil || host == "" {
			continue
		}
		if indirect && !addrPublic(a) {
			continue
		}
		cands = append(cands, net.JoinHostPort(host, itoa(udpPort)))
	}
	if len(cands) > 0 {
		c.learnPeerUDP(id, udpPort, cands, "")
	}
}

// notePunchAttempt ages auto-punch bookkeeping: once a full window has
// passed since the last attempt with still no route, a failure is counted;
// natSuspectAfter of those park the peer as suspected-unreachable-by-punch.
func (c *Core) notePunchAttempt(id string) {
	c.path.mu.Lock()
	c.path.init()
	defer c.path.mu.Unlock()
	if c.UDPRoute(id) != nil {
		c.path.punchTries[id] = 0
		delete(c.path.natSuspect, id)
		return
	}
	c.path.punchTries[id]++
	if c.path.punchTries[id] >= natSuspectAfter {
		c.path.natSuspect[id] = time.Now().Add(natSuspectWindow)
		c.path.punchTries[id] = 0
		c.logger.Info("peer suspected unpunchable — falling back to relay/outbox",
			"peer", id, "retry_after", natSuspectWindow)
	}
}

// sendPunchWant routes a punch_want hint to the elected offerer.
func (c *Core) sendPunchWant(ctx context.Context, id string) {
	c.path.mu.Lock()
	c.path.init()
	if last, ok := c.path.wantAt[id]; ok && time.Since(last) < punchWantEvery {
		c.path.mu.Unlock()
		return
	}
	c.path.wantAt[id] = time.Now()
	c.path.mu.Unlock()
	msgID, err := newUUID()
	if err != nil {
		return
	}
	env, err := bus.NewEnvelope(bus.MsgPunchWant, c.nodeID, msgID, bus.PunchWantPayload{TTL: bus.PunchMaxTTL})
	if err != nil {
		return
	}
	env.To = id
	if err := c.routeTo(ctx, id, env); err != nil {
		c.logger.Debug("punch_want unroutable", "to", id, "err", err)
	}
}

// handlePunchWant honors a punch request: the sender (env.From) is the
// lexicographically-larger side telling us — the elected offerer — to punch
// it. TTL'd relay frames arrive here after the env.To check in dispatch.
func (c *Core) handlePunchWant(ctx context.Context, env bus.Envelope) {
	var p bus.PunchWantPayload
	if err := env.PayloadInto(&p); err != nil {
		return
	}
	if env.To != "" && env.To != c.nodeID {
		if p.TTL <= 1 {
			return
		}
		p.TTL--
		raw, err := json.Marshal(p)
		if err != nil {
			return
		}
		env.Payload = raw
		env.From = c.nodeID
		if err := c.routeTo(ctx, env.To, env); err != nil {
			c.logger.Debug("punch_want relay unroutable", "to", env.To, "err", err)
		}
		return
	}
	// Only the elected side acts — a want from the smaller side is nonsense
	// (it IS the offerer), and ignoring it also kills a bounce loop between
	// two nodes each sure the other is smaller.
	if c.nodeID > env.From || env.From == "" || env.From == c.nodeID {
		return
	}
	c.path.mu.Lock()
	c.path.init()
	if last, ok := c.path.punchAt[env.From]; ok && time.Since(last) < autoPunchEvery {
		c.path.mu.Unlock()
		return
	}
	c.path.punchAt[env.From] = time.Now()
	c.path.mu.Unlock()
	if err := c.PunchPeer(ctx, env.From); err != nil {
		c.logger.Debug("punch_want honored, offer failed", "peer", env.From, "err", err)
	}
}

// notePeerEdgeLoad records a heartbeat's advertised edge count for the
// rendezvous scorer.
func (c *Core) notePeerEdgeLoad(id string, n int) {
	c.path.mu.Lock()
	c.path.init()
	c.path.edgeLoad[id] = n
	c.path.mu.Unlock()
}

// peerEdgeLoad reads the cached count; -1 means never reported.
func (c *Core) peerEdgeLoad(id string) int {
	c.path.mu.Lock()
	defer c.path.mu.Unlock()
	if c.path.edgeLoad == nil {
		return -1
	}
	if n, ok := c.path.edgeLoad[id]; ok {
		return n
	}
	return -1
}
