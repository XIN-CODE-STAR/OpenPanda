// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
	"github.com/gorilla/websocket"
)

// TestPairOverTransitThroughContactPoint walks the whole P4b path in-process:
// a joiner that can reach neither the introducer nor the mesh dials a contact
// point, names the introducer it wants, and completes the ceremony through
// it. Three real sockets, two cores, no stubs.
//
//	node "contact" (127.0.0.1:17982) ──authenticated ws── node "intro" (:17981)
//	        ▲                                              │
//	        │ unauthenticated pair_hello(target=k:intro)    │ transit, seal=none
//	   raw joiner ws ───────────────────────────────────────┘
func TestPairOverTransitThroughContactPoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	intro := newCore(t, "intro", "127.0.0.1:17981")
	contact := newCore(t, "contact", "127.0.0.1:17982")
	if err := intro.Register(ctx); err != nil {
		t.Fatalf("register intro: %v", err)
	}
	if err := contact.Register(ctx); err != nil {
		t.Fatalf("register contact: %v", err)
	}
	go func() { _ = intro.Listen(ctx, "127.0.0.1:17981") }()
	go func() { _ = contact.Listen(ctx, "127.0.0.1:17982") }()
	time.Sleep(200 * time.Millisecond)

	// The contact point must be a real member of the introducer's mesh —
	// that authenticated edge is what carries the proxied handshake.
	contact.SyncPeers(ctx, []string{"127.0.0.1:17981"})
	waitPeer(t, contact, "intro")
	waitPeer(t, intro, "contact")

	introPub, _, ok := intro.nodeKeyPair()
	if !ok {
		t.Fatal("introducer has no identity key")
	}
	introStable := stableIDPrefix + hex.EncodeToString(introPub)

	// The joiner: an ordinary websocket, no shared secret, no hello.
	joinerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("joiner key: %v", err)
	}
	joinerX, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("joiner x key: %v", err)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	session := "sess-transit-" + t.Name()

	ws, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:17982/ws", nil)
	if err != nil {
		t.Fatalf("dial contact: %v", err)
	}
	defer ws.Close()
	hello, err := bus.NewEnvelope(bus.MsgPairHello, "pair-init", "m-"+session, bus.PairHelloPayload{
		Session: session,
		X:       hex.EncodeToString(joinerX.PublicKey().Bytes()),
		Pub:     hex.EncodeToString(joinerPub),
		Nonce:   hex.EncodeToString(nonce),
		Addr:    "0.0.0.0:19999",
		Name:    "pair-init",
		NodeID:  "pair-init",
		Target:  introStable,
	})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatalf("pair_hello: %v", err)
	}

	// pair_ready comes back through the contact point and must name the
	// INTRODUCER's identity — that is the k= check a join URI relies on.
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var env bus.Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("read pair_ready: %v", err)
	}
	if env.Type != bus.MsgPairReady {
		t.Fatalf("expected pair_ready, got %s", env.Type)
	}
	var ready bus.PairReadyPayload
	if err := env.PayloadInto(&ready); err != nil || ready.Session != session {
		t.Fatalf("bad pair_ready: %v", err)
	}
	if ready.Pub != hex.EncodeToString(introPub) {
		t.Fatalf("pair_ready pub = %s, want the introducer's identity", ready.Pub)
	}
	respX, _ := hex.DecodeString(ready.X)
	respN, _ := hex.DecodeString(ready.Nonce)
	respXKey, err := ecdh.X25519().NewPublicKey(respX)
	if err != nil {
		t.Fatalf("responder x key: %v", err)
	}
	dh, err := joinerX.ECDH(respXKey)
	if err != nil {
		t.Fatalf("dh: %v", err)
	}

	// The introducer's operator confirms, and the sealed secret follows the
	// same route back.
	if err := AnswerPairSession(intro.db, session, PairStateConfirmed); err != nil {
		t.Fatalf("confirm session: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var reply bus.Envelope
	if err := ws.ReadJSON(&reply); err != nil {
		t.Fatalf("read pair_secret: %v", err)
	}
	if reply.Type != bus.MsgPairSecret {
		t.Fatalf("expected pair_secret, got %s", reply.Type)
	}
	var ps bus.PairSecretPayload
	if err := reply.PayloadInto(&ps); err != nil {
		t.Fatalf("pair_secret: %v", err)
	}
	box, err := base64.StdEncoding.DecodeString(ps.Box)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	key, err := pairBoxKey(dh, nonce, respN)
	if err != nil {
		t.Fatalf("box key: %v", err)
	}
	plain, err := PairOpen(key, box)
	if err != nil {
		t.Fatalf("open box: %v", err)
	}
	if string(plain) != testSharedSecret {
		t.Fatalf("secret = %q, want the mesh secret", string(plain))
	}

	// The binding served its purpose and must not outlive the handshake.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if contact.attClientFor(stableIDPrefix+hex.EncodeToString(joinerPub)) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("attached-client binding survived the completed handshake")
}
