// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"bytes"
	"testing"
)

func TestX25519RoundTrip(t *testing.T) {
	priv, err := GenerateX25519()
	if err != nil {
		t.Fatalf("GenerateX25519: %v", err)
	}
	// Persisted form must reload to the same identity — the settings-table
	// round trip every restart takes.
	reloaded, err := LoadX25519(priv.Bytes())
	if err != nil {
		t.Fatalf("LoadX25519: %v", err)
	}
	if !priv.PublicKey().Equal(reloaded.PublicKey()) {
		t.Fatal("reloaded X25519 key yields a different public key")
	}
	if _, err := LoadX25519([]byte{1, 2, 3}); err == nil {
		t.Fatal("LoadX25519 accepted a malformed scalar")
	}
	if _, err := LoadX25519Pub([]byte{0x42}); err == nil {
		t.Fatal("LoadX25519Pub accepted a malformed point")
	}
}

func TestTransitSealOpenX25519(t *testing.T) {
	dest, err := GenerateX25519()
	if err != nil {
		t.Fatalf("GenerateX25519: %v", err)
	}
	plain := []byte(`{"env":{"type":"heartbeat","from":"a"},"pub":"x","sig":"y"}`)

	blob, err := TransitSealX25519(dest.PublicKey(), plain)
	if err != nil {
		t.Fatalf("TransitSealX25519: %v", err)
	}
	// Blob = eph_pub(32) ‖ nonce(12) ‖ ciphertext.
	if len(blob) != x25519PubLen+transitNonceLen+len(plain)+16 {
		t.Fatalf("blob length %d, want %d", len(blob), x25519PubLen+transitNonceLen+len(plain)+16)
	}
	got, err := TransitOpenX25519(dest, blob)
	if err != nil {
		t.Fatalf("TransitOpenX25519: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("opened %q, want %q", got, plain)
	}

	// Two seals of the same plaintext must differ — ephemeral key + nonce.
	blob2, err := TransitSealX25519(dest.PublicKey(), plain)
	if err != nil {
		t.Fatalf("TransitSealX25519 #2: %v", err)
	}
	if bytes.Equal(blob, blob2) {
		t.Fatal("two seals produced identical blobs — ephemeral key reused")
	}

	// A different destination cannot open it (wrong-key rejection).
	other, _ := GenerateX25519()
	if _, err := TransitOpenX25519(other, blob); err == nil {
		t.Fatal("blob opened under a foreign key")
	}
	// Tampering anywhere in the sealed region fails closed.
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := TransitOpenX25519(dest, tampered); err == nil {
		t.Fatal("tampered blob opened")
	}
	// And so does a swapped ephemeral prefix.
	tampered = append([]byte(nil), blob...)
	tampered[0] ^= 0x01
	if _, err := TransitOpenX25519(dest, tampered); err == nil {
		t.Fatal("blob with tampered ephemeral key opened")
	}
	if _, err := TransitOpenX25519(dest, blob[:10]); err == nil {
		t.Fatal("truncated blob opened")
	}
}

func TestTransitMeshSeal(t *testing.T) {
	const secret, dest = "mesh-secret", "k:abc123"
	plain := []byte("hello over transit")

	blob, err := TransitSealMesh(secret, dest, plain)
	if err != nil {
		t.Fatalf("TransitSealMesh: %v", err)
	}
	got, err := TransitOpenMesh(secret, dest, blob)
	if err != nil {
		t.Fatalf("TransitOpenMesh: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("opened %q, want %q", got, plain)
	}
	// The dest claim is bound into the key: a blob re-addressed to another
	// peer does not open under that claim.
	if _, err := TransitOpenMesh(secret, "k:other", blob); err == nil {
		t.Fatal("mesh blob opened under a different dest claim")
	}
	if _, err := TransitOpenMesh("wrong-secret", dest, blob); err == nil {
		t.Fatal("mesh blob opened under a foreign secret")
	}
	if _, err := TransitSealMesh("", dest, plain); err == nil {
		t.Fatal("mesh seal minted with no secret")
	}
}

func TestTransitInnerAttestation(t *testing.T) {
	pub, priv, err := GenerateNodeKey()
	if err != nil {
		t.Fatalf("GenerateNodeKey: %v", err)
	}
	envJSON := []byte(`{"v":2,"type":"heartbeat","msg_id":"m1","from":"a"}`)

	sig := SignTransitInner(priv, envJSON)
	if !VerifyTransitInner(pub, envJSON, sig) {
		t.Fatal("valid transit inner attestation rejected")
	}
	// One byte of the attested envelope rewritten — a relay editing From or
	// the payload — must break the signature.
	mutated := []byte(`{"v":2,"type":"heartbeat","msg_id":"m1","from":"b"}`)
	if VerifyTransitInner(pub, mutated, sig) {
		t.Fatal("attestation verified over a rewritten envelope")
	}
	if VerifyTransitInner(pub, envJSON, sig[:len(sig)-2]+"ff") {
		t.Fatal("corrupt signature verified")
	}
	if VerifyTransitInner(pub, envJSON, "zz") {
		t.Fatal("non-hex signature verified")
	}
	_, priv2, _ := GenerateNodeKey()
	if VerifyTransitInner(pub, envJSON, SignTransitInner(priv2, envJSON)) {
		t.Fatal("foreign signer's attestation verified")
	}
}
