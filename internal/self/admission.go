// SPDX-License-Identifier: AGPL-3.0-or-later

package self

import (
	"crypto/ed25519"
	"encoding/hex"
)

// ProducersSelf and ProducersMesh are the two admission policies a node can
// run under (config: self_evolution.producers).
const (
	// ProducersSelf admits only sets signed by this node's own key.
	ProducersSelf = "self"
	// ProducersMesh admits any signer — the mesh-wide policy.
	ProducersMesh = "mesh"
)

// ProducerID is the stable "k:<pub>" identity a producer's sets carry.
func ProducerID(pub ed25519.PublicKey) string {
	return "k:" + hex.EncodeToString(pub)
}

// ProducerMode normalizes the configured producers value: the empty string
// means "self" (the documented default).
func ProducerMode(mode string) string {
	if mode == "" {
		return ProducersSelf
	}
	return mode
}

// ProducerAdmitted reports whether a set signed by producer may enter this
// node's merge queue under the given producers mode.
//
// The rule lives here, not in its callers: the boot-time rebase sweep, the
// propose path and the operator's `panda self rebase` must agree. Any path
// that re-signs a set with the node's own key would otherwise turn a foreign
// set — which propose refuses — into a locally signed, mergeable one.
func ProducerAdmitted(mode, producer string, pub ed25519.PublicKey) bool {
	if ProducerMode(mode) == ProducersMesh {
		return true
	}
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return producer == ProducerID(pub)
}
