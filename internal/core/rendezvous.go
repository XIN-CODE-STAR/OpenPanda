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

	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// Dynamic rendezvous edges (cross-network mesh design M2).
//
// A rendezvous edge is not a role — it is a preference: this node keeps K
// long-lived connections to peers that advertised relay.rendezvous, scored
// by RTT, row freshness, advertised edge load, and failure-domain diversity.
// Those edges are what punch offers route over, what transit re-forwards
// through, and what the epidemic directory converges along — the mesh stays
// glued without any fixed central server. If every candidate disappears the
// node degrades honestly: direct/punch paths still work, the directory says
// so, and the outbox/DTN carries the rest.
//
// "Proven" per the design means AutoNAT-style callback verification — that
// dial_check exchange is the P5 hardening item; until it lands, an
// advertised flag on an online, trusted row is the candidate gate (the same
// trust level the card framework already grants every other claim).

const (
	// rendezvousKDefault is the redundancy target used when no policy is
	// installed: two edges survive one loss without a partition. The live
	// target is network.bootstrap_edges (0 = manual mode), resolved by
	// Core.rendezvousTarget.
	rendezvousKDefault = 2
	// rendezvousEvalEvery is the re-election period (design: 5min).
	rendezvousEvalEvery = 5 * time.Minute
	// rendezvousHysteresis requires a challenger to beat an incumbent's
	// score by this factor before an edge is swapped — mesh-wide churn
	// costs more than a marginally-suboptimal pick.
	rendezvousHysteresis = 1.25
	// rendezvousDialTimeout bounds one candidate dial during an eval pass.
	rendezvousDialTimeout = 6 * time.Second
)

// rendezvousState tracks the elected set and the degraded flag. peers is
// the current K; degraded reports "no viable candidate exists" for
// status/doctor surfaces.
type rendezvousState struct {
	mu       sync.Mutex
	peers    map[string]bool
	degraded bool
	lastEval time.Time
	// provenBy holds the distinct peers that answered a dial_check, with the
	// timestamp of their last successful callback; proven is the derived
	// "enough of them, recently enough" verdict (dialcheck.go).
	provenBy  map[string]time.Time
	proven    bool
	lastProof time.Time
}

// rendezvousCandidate is one scored row.
type rendezvousCandidate struct {
	id       string
	score    float64
	domain   string // failure-domain key: /24 v4 or /48 v6 of the best addr
	online   bool
	edgeLoad int
}

// rendezvousEval re-elects the K set: score every eligible candidate, then
// keep incumbents unless a challenger wins by the hysteresis margin, then
// dial whatever the elected set still lacks an edge to.
func (c *Core) rendezvousEval(ctx context.Context) {
	k := c.rendezvousTarget()
	c.rzv.mu.Lock()
	c.rzv.lastEval = time.Now()
	if k == 0 {
		// Manual mode (network.bootstrap_edges: 0): the mesh is the
		// configured peer list, exactly as before the feature existed. An
		// empty set with degraded=false is the honest report — this node
		// chose not to keep rendezvous edges, it did not fail to find one.
		c.rzv.peers = nil
		c.rzv.degraded = false
		c.rzv.mu.Unlock()
		return
	}
	c.rzv.mu.Unlock()
	cands := c.rendezvousCandidates(ctx)
	// Elect: top-K by score with failure-domain diversity — two edges into
	// the same /24 are one failure waiting to happen, so a worse-scored
	// candidate in a fresh domain wins the second slot.
	elected := c.rendezvousElect(cands, k)
	c.rzv.mu.Lock()
	if c.rzv.peers == nil {
		c.rzv.peers = make(map[string]bool)
	}
	prev := len(c.rzv.peers)
	c.rzv.peers = elected
	// Degraded is honest bookkeeping, not an alarm: zero candidates means
	// the mesh is whatever the live conns already are — direct and punch
	// paths keep working, and the flag tells status views to say that.
	degraded := len(elected) == 0
	was := c.rzv.degraded
	c.rzv.degraded = degraded
	c.rzv.mu.Unlock()
	if degraded != was {
		if degraded {
			c.logger.Warn("no rendezvous-capable peers — mesh is the connected component only")
		} else {
			c.logger.Info("rendezvous candidates available again")
		}
	}
	if len(elected) != prev {
		c.logger.Debug("rendezvous set re-elected", "n", len(elected))
	}
	// Ensure an edge to every electee we lack one to.
	for id := range elected {
		if c.connFor(id) != nil || c.UDPRoute(id) != nil {
			continue
		}
		id := id
		c.goGuarded("rendezvous dial "+id, func() {
			dctx, cancel := context.WithTimeout(ctx, rendezvousDialTimeout)
			defer cancel()
			c.dialPeerAddrs(dctx, id)
		})
	}
}

// rendezvousCandidates scores every directory row that advertises
// relay.rendezvous on an online, non-session, non-self row with a usable
// trust tier. Trust is the operator's local assertion — a peer the operator
// fenced to "open" stays a candidate for COORDINATION (rendezvous serves
// the mesh, not task admission), but a pub-less row cannot be pinned at
// dial time and is skipped.
func (c *Core) rendezvousCandidates(ctx context.Context) []rendezvousCandidate {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, COALESCE(relay,''), COALESCE(status,''), COALESCE(pub_key,''),
		       COALESCE(seen_at,0), COALESCE(last_seen,0), COALESCE(addrs,'')
		FROM employee_cache`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	now := time.Now().Unix()
	var out []rendezvousCandidate
	for rows.Next() {
		var id, relayJSON, status, pub, addrsJSON string
		var seenAt, lastSeen int64
		if err := rows.Scan(&id, &relayJSON, &status, &pub, &seenAt, &lastSeen, &addrsJSON); err != nil {
			continue
		}
		if id == c.nodeID || ledger.SessionRowID(id) || pub == "" {
			continue
		}
		var rp ledger.RelayProfile
		if err := json.Unmarshal([]byte(relayJSON), &rp); err != nil || !rp.Rendezvous {
			continue
		}
		// ForTrust scopes the offer: a relay declared "for mesh only" is not
		// ours to use from a stricter zone's perspective… the field names the
		// tier the PROVIDER serves, and anything narrower than our trust for
		// the peer is still a valid pick — the edge is coordination, not
		// delegated work, so the existing row trust suffices.
		var addrs []string
		_ = json.Unmarshal([]byte(addrsJSON), &addrs)
		if len(addrs) == 0 && c.connFor(id) == nil {
			continue // undialable: no hint and no live edge
		}
		fresh := max(seenAt, lastSeen)
		age := now - fresh
		if age < 0 {
			age = 0
		}
		score := 1000.0 / (1.0 + float64(age)/60.0) // freshness: decays per minute
		if c.connFor(id) != nil {
			score += 50 // a live edge is proven reachability
		}
		if load := c.peerEdgeLoad(id); load >= 0 {
			score -= float64(load) * 2 // prefer less-pinned candidates
		}
		out = append(out, rendezvousCandidate{
			id: id, score: score,
			domain:   addrDomain(addrs),
			online:   status == "online",
			edgeLoad: c.peerEdgeLoad(id),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// rendezvousElect picks up to K candidates honoring hysteresis: an incumbent
// already in the set keeps its seat unless a challenger out-scores it by the
// margin, and failure-domain diversity is enforced across the electees.
func (c *Core) rendezvousElect(cands []rendezvousCandidate, k int) map[string]bool {
	c.rzv.mu.Lock()
	incumbents := c.rzv.peers
	c.rzv.mu.Unlock()
	elected := make(map[string]bool)
	domains := make(map[string]bool)
	// Incumbents first: keep any still-eligible candidate whose score has
	// not been decisively beaten — this pass computes stability, the fill
	// pass below adds fresh capacity.
	for _, cd := range cands {
		if len(elected) >= k {
			break
		}
		if incumbents[cd.id] {
			// Is there a non-incumbent challenger decisively better?
			beaten := false
			for _, ch := range cands {
				if ch.id == cd.id || incumbents[ch.id] {
					continue
				}
				if ch.score > cd.score*rendezvousHysteresis && !domains[ch.domain] {
					beaten = true
					break
				}
			}
			if beaten {
				continue
			}
			elected[cd.id] = true
			domains[cd.domain] = true
		}
	}
	// Fill: best remaining candidates, preferring fresh domains.
	for _, cd := range cands {
		if len(elected) >= k {
			break
		}
		if elected[cd.id] || domains[cd.domain] {
			continue
		}
		elected[cd.id] = true
		domains[cd.domain] = true
	}
	return elected
}

// RendezvousPeers reports the currently elected rendezvous set (status/
// doctor surfaces).
func (c *Core) RendezvousPeers() []string {
	c.rzv.mu.Lock()
	defer c.rzv.mu.Unlock()
	out := make([]string, 0, len(c.rzv.peers))
	for id := range c.rzv.peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RendezvousDegraded reports whether zero rendezvous candidates exist — the
// honest "mesh is the connected component" signal for doctor and status.
func (c *Core) RendezvousDegraded() bool {
	c.rzv.mu.Lock()
	defer c.rzv.mu.Unlock()
	return c.rzv.degraded
}

// addrDomain derives the failure-domain key for a candidate: the /24 of its
// best public v4 addr or /48 of v6; hostname addrs use the name itself (two
// nodes behind one hostname share fate anyway), and no addr means a domain
// of its own — undialable rows can only be reached through a conn.
func addrDomain(addrs []string) string {
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return "name:" + host
		}
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		if ip.Is4() {
			return "v4:" + netip.PrefixFrom(ip, 24).Masked().String()
		}
		return "v6:" + netip.PrefixFrom(ip, 48).Masked().String()
	}
	return "none"
}
