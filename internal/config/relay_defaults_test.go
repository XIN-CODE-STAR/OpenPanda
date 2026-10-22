// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"testing"

	"github.com/Xustalis/OpenPanda/internal/trust"
)

// TestRelayDefaultsForMesh pins the relay posture a three-node deployment
// relies on: a mesh-tier node hands frames to the relay and routes its own
// frames through it, with no trust table written. The scoped opt-ins exist
// for the sub-mesh tiers, not for the baseline.
func TestRelayDefaultsForMesh(t *testing.T) {
	tr := TrustConfig{}
	if !tr.RelayAcceptFor(trust.Mesh) {
		t.Fatal("mesh tier must be allowed to hand frames to the relay by default")
	}
	if !tr.RelayServeFor(trust.Mesh) {
		t.Fatal("mesh tier must be allowed to route its own frames through a relay by default")
	}
}
