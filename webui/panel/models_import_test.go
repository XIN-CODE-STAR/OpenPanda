// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/localmodels"
)

func TestLocalModelImportKeepsMetadataAndFailedSelections(t *testing.T) {
	isolateUserDirs(t)
	home := t.TempDir()
	for _, key := range []string{"USERPROFILE", "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PATH"} {
		t.Setenv(key, home)
	}
	for _, key := range []string{"CODEX_CLI", "OPENCODE_CLI", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
		"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "UNSLOTH_STUDIO_AUTH_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "*")
	var completions atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		completions.Add(1)
		if r.URL.Path != "/Tenant/v1/messages" || r.Header.Get("Authorization") != "Bearer fixture-token" ||
			r.Header.Get("X-Api-Key") != "" {
			http.Error(w, "wrong route or credential scheme", http.StatusUnauthorized)
			return
		}
		var req struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "ready" {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))
	}))
	t.Cleanup(relay.Close)
	base := relay.URL + "/Tenant"
	settings, err := json.Marshal(map[string]any{"env": map[string]string{
		"ANTHROPIC_BASE_URL": base, "ANTHROPIC_MODEL": "ready", "ANTHROPIC_DEFAULT_OPUS_MODEL": "unavailable",
		"ANTHROPIC_AUTH_TOKEN": "fixture-token",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	h, path := newModelPanel(t, func(c *config.Config) { c.Model = config.ModelConfig{} })
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq(http.MethodGet, "/api/models/discover", nil))
	var discovered struct{ Sources []localmodels.Source }
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &discovered) != nil {
		t.Fatalf("discover: %d %s", rr.Code, rr.Body.String())
	}
	items := make(map[string]modelImportItem)
	for _, source := range discovered.Sources {
		for _, row := range source.Candidates {
			if row.Source == localmodels.SourceClaude {
				items[row.Model] = modelImportItem{CandidateID: row.ID, Provider: row.Provider, Model: row.Model,
					BaseURL: row.BaseURL, APIType: row.APIType, KeyRef: row.KeyRef, NoAuth: true, Alias: "imported-route"}
			}
		}
	}
	if len(items) != 2 || completions.Load() != 2 {
		t.Fatalf("discovered %d fixture models with %d completions", len(items), completions.Load())
	}
	stale := items["ready"]
	stale.BaseURL = relay.URL + "/tenant"
	payload, err := json.Marshal(map[string]any{"items": []modelImportItem{items["ready"], items["unavailable"], stale}})
	if err != nil {
		t.Fatal(err)
	}
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/import", string(payload)))
	ids, _ := out["completed_ids"].([]any)
	failures, _ := out["skipped"].([]any)
	if code != http.StatusOK || out["added"] != 1.0 || len(ids) != 1 || ids[0] != items["ready"].CandidateID ||
		len(failures) != 2 || !strings.Contains(failures[0].(string), "no longer matches") || completions.Load() != 4 {
		t.Fatalf("unexpected batch or redundant verification: %d %v, calls %d", code, out, completions.Load())
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].Source != localmodels.SourceClaude ||
		cfg.Models[0].NoAuth || cfg.Models[0].KeyScheme != "bearer" ||
		cfg.Model.Alias() != "imported-route" || cfg.Model.Model != "ready" {
		t.Fatal("import did not preserve server metadata and persist active model")
	}
}

func TestConcurrentModelImportsKeepFirstActivation(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) { c.Model = config.ModelConfig{} })
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, model := range []string{"model-a", "model-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := jsonReq(http.MethodPost, "/api/models/import",
				`{"items":[{"provider":"custom","model":"`+model+`","base_url":"https://relay.invalid/v1","api_type":"openai","no_auth":true}]}`)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			codes <- rr.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("import status %d", code)
		}
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 2 || cfg.Model.Model != cfg.Models[0].Model {
		t.Fatalf("active model %q differs from first persisted registry row", cfg.Model.Model)
	}
}
