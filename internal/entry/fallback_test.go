// SPDX-License-Identifier: AGPL-3.0-or-later

package entry

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestWrapAPIError400CarriesProviderDetail: the stock 400 hint tells the user
// to check model/api_type — which is wrong advice when the provider rejected
// something else (a retired model name, an oversized context). The provider's
// own rejection reason must ride along so the user can act on the real cause
// instead of guessing.
func TestWrapAPIError400CarriesProviderDetail(t *testing.T) {
	err := WrapAPIError(&statusError{
		status: 400,
		body:   `{"error":{"message":"Model Not Found: deepseek-chat had been deprecated","type":"invalid_request_error"}}`,
	})
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Fatalf("user message lost the status: %q", msg)
	}
	if !strings.Contains(msg, "Model Not Found") {
		t.Fatalf("user message missing the provider's reason: %q", msg)
	}
}

// TestWrapAPIError400SurvivesOddBodies: a non-JSON or empty body must degrade
// gracefully — the hint still reads cleanly, never a dangling separator.
func TestWrapAPIError400SurvivesOddBodies(t *testing.T) {
	for _, body := range []string{"", "   ", "plain text reason\nsecond line"} {
		msg := WrapAPIError(&statusError{status: 400, body: body}).Error()
		if strings.HasSuffix(msg, "：") || strings.HasSuffix(msg, ":") {
			t.Fatalf("dangling separator for body %q: %q", body, msg)
		}
	}
	msg := WrapAPIError(&statusError{status: 400, body: "plain text reason\nsecond line"}).Error()
	if !strings.Contains(msg, "plain text reason") {
		t.Fatalf("plain-text body should surface its first line: %q", msg)
	}
	if strings.Contains(msg, "second line") {
		t.Fatalf("only the first line of the body should surface: %q", msg)
	}
}

// TestWrapAPIError400TruncatesLongBodies: a provider verbose enough to fill a
// page must not flood the chat bubble — the excerpt stays bounded.
func TestWrapAPIError400TruncatesLongBodies(t *testing.T) {
	body := strings.Repeat("x", 5000)
	msg := WrapAPIError(&statusError{status: 400, body: body}).Error()
	if len(msg) > 500 {
		t.Fatalf("user message not bounded: %d chars", len(msg))
	}
}

// TestIsModelConfigError: the web console hangs a fix-it CTA on this verdict,
// so the classification must line up with the failures that actually live in
// the model settings (absent/rejected key, bad endpoint, retired model id)
// and exclude everything the user can only wait out or retry (rate limits,
// outages, unreachable endpoints, cancellations). Wrapped ClassifyErrors
// must classify the same as their causes — the panel sees the wrapped form.
func TestIsModelConfigError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no key", ErrNoKey, true},
		{"no model", ErrNoModel, true},
		{"401", &statusError{status: 401, body: "bad key"}, true},
		{"403", &statusError{status: 403}, true},
		{"404", &statusError{status: 404}, true},
		{"400", &statusError{status: 400, body: "model retired"}, true},
		{"429", &statusError{status: 429}, false},
		{"500", &statusError{status: 500}, false},
		{"canceled", context.Canceled, false},
		{"generic", errors.New("boom"), false},
		{"wrapped 401", WrapAPIError(&statusError{status: 401, body: "bad key"}), true},
		{"wrapped no-key", &ClassifyError{UserMsg: "x", Err: ErrNoKey}, true},
		{"wrapped canceled", &ClassifyError{UserMsg: "x", Err: context.Canceled}, false},
	}
	for _, tc := range cases {
		if got := IsModelConfigError(tc.err); got != tc.want {
			t.Fatalf("%s: IsModelConfigError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
