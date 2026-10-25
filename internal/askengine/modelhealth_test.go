// SPDX-License-Identifier: AGPL-3.0-or-later

package askengine

import (
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/entry"
)

// modelTestClient builds a client for breaker tests. No request is ever made;
// NewClient only validates and stores the endpoint.
func modelTestClient(t *testing.T, base, model string) *entry.Client {
	t.Helper()
	c, err := entry.NewClient(config.ModelConfig{BaseURL: base, Model: model, APIKey: "k"})
	if err != nil {
		t.Fatalf("client %s: %v", base, err)
	}
	return c
}

// TestModelBreakerEscalates pins the cooldown arithmetic: the first fatal
// failure cools a client for the base interval, repetition widens it up to
// the ceiling, and one success clears the record.
func TestModelBreakerEscalates(t *testing.T) {
	e := &Engine{}
	e.recordModelFailure("k")
	first := time.Until(e.breaker["k"].until)
	if first > breakerBaseCooldown || first < breakerBaseCooldown-time.Second {
		t.Fatalf("first cooldown = %v, want ~%v", first, breakerBaseCooldown)
	}
	e.recordModelFailure("k")
	second := time.Until(e.breaker["k"].until)
	if second <= first {
		t.Fatalf("second cooldown = %v, want more than %v", second, first)
	}
	for i := 0; i < 10; i++ {
		e.recordModelFailure("k")
	}
	if last := time.Until(e.breaker["k"].until); last > breakerMaxCooldown || last < breakerMaxCooldown-time.Second {
		t.Fatalf("capped cooldown = %v, want ~%v", last, breakerMaxCooldown)
	}
	e.recordModelSuccess("k")
	if !e.isModelHealthy("k") {
		t.Fatal("success must clear the cooldown")
	}
}

// TestModelBreakerKeyedByEndpoint pins the identity rule: two registry aliases
// of the same model id on different endpoints are separate clients, so one
// provider's outage does not cool down the other.
func TestModelBreakerKeyedByEndpoint(t *testing.T) {
	a := modelTestClient(t, "https://a.example.com/v1", "same-model")
	b := modelTestClient(t, "https://b.example.com/v1", "same-model")
	if a.Key() == b.Key() {
		t.Fatalf("keys must differ, both = %q", a.Key())
	}
	e := &Engine{}
	e.recordModelFailure(a.Key())
	if e.isModelHealthy(a.Key()) {
		t.Fatal("failed endpoint should be cooling")
	}
	if !e.isModelHealthy(b.Key()) {
		t.Fatal("a different endpoint must not inherit the cooldown")
	}
}

// TestModelFallbacksHealthyFirst pins the fallback order: the client that just
// failed is skipped, duplicates are tried once, and a cooling candidate moves
// behind every healthy one without being dropped.
func TestModelFallbacksHealthyFirst(t *testing.T) {
	a := modelTestClient(t, "https://a.example.com/v1", "model-a")
	b := modelTestClient(t, "https://b.example.com/v1", "model-b")
	e := &Engine{fallbacks: []*entry.Client{a, b}}

	e.recordModelFailure(a.Key())
	got := e.modelFallbacks("")
	if len(got) != 2 || got[0].Key() != b.Key() || got[1].Key() != a.Key() {
		t.Fatalf("order = %v, want healthy b then cooling a", clientKeys(got))
	}
	if got := e.modelFallbacks(a.Key()); len(got) != 1 || got[0].Key() != b.Key() {
		t.Fatalf("skip-failed order = %v, want only b", clientKeys(got))
	}

	dup := &Engine{fallbacks: []*entry.Client{a, a}}
	if got := dup.modelFallbacks(""); len(got) != 1 {
		t.Fatalf("duplicates = %v, want one entry", clientKeys(got))
	}
}

// TestHealthyClientSkipsCoolingEndpoint pins the ask-start path: a primary in
// cooldown is served by the healthy fallback.
func TestHealthyClientSkipsCoolingEndpoint(t *testing.T) {
	a := modelTestClient(t, "https://a.example.com/v1", "model-a")
	b := modelTestClient(t, "https://b.example.com/v1", "model-b")
	e := &Engine{fallbacks: []*entry.Client{a}}
	e.client.Store(b)
	e.recordModelFailure(b.Key())

	got, name := e.healthyClient()
	if got == nil || got.Key() != a.Key() || name != "model-a" {
		t.Fatalf("healthyClient = %v (%q), want the healthy fallback a", got, name)
	}
}

func clientKeys(cs []*entry.Client) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ModelName()
	}
	return out
}
