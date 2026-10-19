// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/updater"
)

func TestUpdateApplyCommands(t *testing.T) {
	cases := []struct {
		includePre bool
		apply      string
		upgrade    string
	}{
		{false, "panda update apply", "panda upgrade"},
		{true, "panda update apply --pre", "panda upgrade --pre"},
	}
	for _, c := range cases {
		apply, upgrade := updateApplyCommands(c.includePre)
		if apply != c.apply || upgrade != c.upgrade {
			t.Errorf("updateApplyCommands(%v) = (%q, %q), want (%q, %q)",
				c.includePre, apply, upgrade, c.apply, c.upgrade)
		}
	}
}

func TestNewerPrerelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct {
			TagName    string `json:"tag_name"`
			Body       string `json:"body"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}{
			{TagName: "v0.0.8", Body: "Release 0.0.8", Prerelease: false},
			{TagName: "v0.0.9-preview", Body: "Preview 0.0.9", Prerelease: true},
		})
	}))
	defer srv.Close()

	cleanup := updater.SetAPIBaseForTest(srv.URL)
	defer cleanup()
	t.Setenv("OPENPANDA_UPDATE_REPO", "")

	rel, ok := newerPrerelease(context.Background(), "0.0.8")
	if !ok || rel.Version != "0.0.9-preview" {
		t.Fatalf("newerPrerelease from 0.0.8 = (%+v, %v), want 0.0.9-preview", rel, ok)
	}
	if _, ok := newerPrerelease(context.Background(), "0.0.9-preview"); ok {
		t.Fatal("newerPrerelease should not report a preview when already running one")
	}
}

func TestNewerPrereleaseNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct {
			TagName    string `json:"tag_name"`
			Body       string `json:"body"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}{
			{TagName: "v0.0.8", Body: "Release 0.0.8", Prerelease: false},
		})
	}))
	defer srv.Close()

	cleanup := updater.SetAPIBaseForTest(srv.URL)
	defer cleanup()
	t.Setenv("OPENPANDA_UPDATE_REPO", "")

	if rel, ok := newerPrerelease(context.Background(), "0.0.8"); ok {
		t.Fatalf("newerPrerelease with only stable releases = (%+v, true), want false", rel)
	}
}

func TestUpdateCheckFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct {
			TagName    string `json:"tag_name"`
			Body       string `json:"body"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}{
			{TagName: "v0.0.8", Body: "Release 0.0.8", Prerelease: false},
			{TagName: "v0.0.8-preview", Body: "Preview 0.0.8", Prerelease: true},
		})
	}))
	defer srv.Close()

	cleanup := updater.SetAPIBaseForTest(srv.URL)
	defer cleanup()

	// Direct test of updater Manager with current = "0.0.8-preview"
	m := updater.New(updater.Options{
		Current: "0.0.8-preview",
	})
	if err := m.Check(context.Background()); err != nil {
		t.Fatalf("m.Check failed: %v", err)
	}
	st := m.Status()
	if !st.Available {
		t.Fatalf("expected update to be available, got %+v", st)
	}
	if st.Latest != "0.0.8" {
		t.Fatalf("expected latest version 0.0.8, got %q", st.Latest)
	}
}
