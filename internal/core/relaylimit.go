// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"sync"
	"time"
)

// relayBudget bounds what serving other members' transit costs this node
// (network.trust.relay.max_kbps / max_sessions). It is self-harm protection,
// not fairness: a metered uplink or a small VPS should be able to say "relay
// for me, but no more than 512 kbit/s and 32 frames at a time" and have the
// excess dropped rather than queued. Transit is best-effort by design — the
// sender's outbox is the reliability layer — so dropping here is honest.
//
// The two knobs answer different questions. Sessions bound concurrency (how
// many frames can be mid-forward at once — bounded memory and file
// descriptors), bandwidth bounds rate. A zero for either disables that
// limit, which is the default: an unlimited relay is what the pre-policy
// behaviour was.
type relayBudget struct {
	mu          sync.Mutex
	maxSessions int     // 0 = unlimited
	sessions    int     // frames currently being forwarded
	bytesPerSec float64 // 0 = unlimited
	tokens      float64 // current bucket fill, in bytes
	last        time.Time
}

// relayBurstFloor is the minimum burst a bandwidth-capped relay may spend
// inside one second. A control-plane transit frame is small (delegates and
// results, not artifacts), so flooring the burst keeps a conservative cap
// from rejecting every frame outright — the bucket would otherwise never
// hold enough for even one send at dial-up-era rates.
const relayBurstFloor = 16 << 10

// reconfigure installs the limits. Called from SetNetworkPolicy on boot and
// on every hot reload; safe to call concurrently with acquire/allow.
func (b *relayBudget) reconfigure(maxKbps, maxSessions int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maxKbps < 0 {
		maxKbps = 0
	}
	if maxSessions < 0 {
		maxSessions = 0
	}
	// Lowering the cap never kills in-flight forwards and never rewrites the
	// live count: acquire refuses new slots until the tally drains below the
	// new line, and every release still decrements exactly once.
	b.maxSessions = maxSessions
	b.bytesPerSec = float64(maxKbps) * 125 // kbit/s → byte/s
	burst := b.burst()
	b.last = time.Now()
	if b.tokens > burst || b.tokens == 0 {
		b.tokens = burst
	}
}

// burst is one second of budget, floored so a single control frame fits.
func (b *relayBudget) burst() float64 {
	if b.bytesPerSec <= 0 {
		return 0
	}
	if b.bytesPerSec < relayBurstFloor {
		return relayBurstFloor
	}
	return b.bytesPerSec
}

// refillLocked tops the bucket up for elapsed time. Caller holds b.mu.
func (b *relayBudget) refillLocked() {
	if b.bytesPerSec <= 0 {
		return
	}
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
		b.tokens = b.burst()
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.last = now
	b.tokens += elapsed * b.bytesPerSec
	if burst := b.burst(); b.tokens > burst {
		b.tokens = burst
	}
}

// acquire reserves one concurrent relay slot. It returns a no-op release
// when the session cap is unlimited or a slot was taken.
func (b *relayBudget) acquire() (release func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxSessions > 0 && b.sessions >= b.maxSessions {
		return nil, false
	}
	b.sessions++
	return b.release, true
}

// release frees a slot taken by acquire.
func (b *relayBudget) release() {
	b.mu.Lock()
	if b.sessions > 0 {
		b.sessions--
	}
	b.mu.Unlock()
}

// allow spends n bytes from the bucket. Unlimited when no cap is set; when
// a frame cannot fit even in a full bucket it is refused immediately rather
// than banked against a future that will never hold it.
func (b *relayBudget) allow(n int) bool {
	if n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bytesPerSec <= 0 {
		return true
	}
	b.refillLocked()
	cost := float64(n)
	if cost > b.burst() {
		return false
	}
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// sessionsActive reports the current concurrent-forward count (tests and
// status surfaces).
func (b *relayBudget) sessionsActive() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions
}
