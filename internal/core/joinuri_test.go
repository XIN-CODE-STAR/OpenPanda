// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"errors"
	"strings"
	"testing"
)

func TestParseJoinURI(t *testing.T) {
	key := strings.Repeat("ab", 32)
	spec, err := ParseJoinURI("panda://join?b=203.0.113.5:7836&b=node.example.com:7836&k=k:" + key + "&c=482913")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.Bootstraps) != 2 || spec.Bootstraps[0] != "203.0.113.5:7836" || spec.Bootstraps[1] != "node.example.com:7836" {
		t.Fatalf("bootstraps = %v", spec.Bootstraps)
	}
	if spec.Introducer != key {
		t.Fatalf("introducer = %q", spec.Introducer)
	}
	if spec.Code != "482913" {
		t.Fatalf("code = %q", spec.Code)
	}
}

func TestParseJoinURIForms(t *testing.T) {
	key := strings.Repeat("cd", 32)
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"openpanda scheme", "openpanda://join?b=host:7836", false},
		{"bare key", "panda://join?b=host:7836&k=" + key, false},
		{"dashed code", "panda://join?b=host:7836&c=482-913", false},
		{"ws url bootstrap", "panda://join?b=wss%3A%2F%2Fhost%3A7836", false},
		{"no bootstrap", "panda://join?k=k:" + key, true},
		{"wrong verb", "panda://invite?b=host:7836", true},
		{"wrong scheme", "https://join?b=host:7836", true},
		{"short key", "panda://join?b=host:7836&k=k:abcd", true},
		{"bad code", "panda://join?b=host:7836&c=12ab56", true},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJoinURI(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestJoinURIRoundTrip: what `nodes invite --uri` prints is what `panda
// join` parses — the two halves must not drift.
func TestJoinURIRoundTrip(t *testing.T) {
	key := strings.Repeat("01", 32)
	orig := &JoinSpec{
		Bootstraps: []string{"203.0.113.5:7836", "node.example.com:7836"},
		Introducer: key,
		Code:       "482913",
	}
	got, err := ParseJoinURI(orig.URI())
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(got.Bootstraps) != len(orig.Bootstraps) || got.Bootstraps[0] != orig.Bootstraps[0] || got.Bootstraps[1] != orig.Bootstraps[1] {
		t.Fatalf("bootstraps = %v", got.Bootstraps)
	}
	if got.Introducer != orig.Introducer || got.Code != orig.Code {
		t.Fatalf("round trip = %+v", got)
	}
}

// TestJoinSpecVerifyPeer: the two pins fail independently and unpinned means
// "no opinion".
func TestJoinSpecVerifyPeer(t *testing.T) {
	key := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	spec := &JoinSpec{Introducer: key, Code: "482913"}

	if err := spec.VerifyPeer(key, "482-913"); err != nil {
		t.Fatalf("matching peer rejected: %v", err)
	}
	if err := spec.VerifyPeer(other, "482-913"); !errors.Is(err, ErrJoinWrongIntroducer) {
		t.Fatalf("wrong introducer = %v", err)
	}
	if err := spec.VerifyPeer(key, "111-222"); !errors.Is(err, ErrJoinCodeMismatch) {
		t.Fatalf("wrong code = %v", err)
	}
	open := &JoinSpec{Bootstraps: []string{"host:7836"}}
	if err := open.VerifyPeer(other, "any-code"); err != nil {
		t.Fatalf("unpinned spec rejected a peer: %v", err)
	}
}
