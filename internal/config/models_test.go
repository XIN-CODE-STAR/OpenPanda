// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"strings"
	"testing"
)

func TestUpdateModelsSectionRoundTrip(t *testing.T) {
	p := writeTemp(t, "node:\n  name: \"n\"\n")
	models := []ModelConfig{
		{Name: "deepseek", Provider: "deepseek", APIType: "anthropic", BaseURL: "https://api.deepseek.com/anthropic", APIKey: "sk-1", Model: "deepseek-v4-flash", MaxTokens: 4096},
		{Name: "claude", Provider: "claude", APIType: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-2", Model: "claude-sonnet-4-5", MaxTokens: 8192},
	}
	if err := UpdateModelsSection(p, models); err != nil {
		t.Fatalf("write models: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}
	if cfg.Models[0].Name != "deepseek" || cfg.Models[0].APIKey != "sk-1" {
		t.Fatalf("models[0] = %+v", cfg.Models[0])
	}
	if cfg.Models[1].Model != "claude-sonnet-4-5" {
		t.Fatalf("models[1] = %+v", cfg.Models[1])
	}
}

func TestUpdateModelsSectionEmptyRemovesKey(t *testing.T) {
	p := writeTemp(t, "node:\n  name: \"n\"\n")
	models := []ModelConfig{{Name: "a", Model: "m"}}
	if err := UpdateModelsSection(p, models); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := UpdateModelsSection(p, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Models) != 0 {
		t.Fatalf("expected empty registry, got %+v", cfg.Models)
	}
}

func TestUpdateModelsSectionPreservesOtherSections(t *testing.T) {
	p := writeTemp(t, "node:\n  name: \"keep-me\"\n  kind: \"physical\"\n")
	if err := UpdateModelsSection(p, []ModelConfig{{Name: "a", Model: "m"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Node.Name != "keep-me" {
		t.Fatalf("node section clobbered: %+v", cfg.Node)
	}
}

// TestUpdateModelSectionMaxRetries verifies the retry budget round-trips:
// a positive value lands in the file, a negative value (retries disabled)
// survives instead of being mistaken for unset, and writing 0 removes the key
// so the built-in default applies on reload.
func TestUpdateModelSectionMaxRetries(t *testing.T) {
	p := writeTemp(t, "node:\n  name: \"n\"\n")
	mc := ModelConfig{BaseURL: "https://api.anthropic.com", Model: "m", MaxRetries: 50}
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.MaxRetries != 50 {
		t.Errorf("model.max_retries = %d, want 50", cfg.Model.MaxRetries)
	}

	// A negative budget is a real setting (retries disabled), not "unset".
	mc.MaxRetries = -1
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection negative: %v", err)
	}
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load after negative: %v", err)
	}
	if cfg.Model.MaxRetries != -1 {
		t.Errorf("model.max_retries = %d, want -1", cfg.Model.MaxRetries)
	}

	mc.MaxRetries = 0
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection clear: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "max_retries") {
		t.Errorf("max_retries=0 should remove the key:\n%s", data)
	}
}

func TestModelConfigAlias(t *testing.T) {
	if got := (ModelConfig{Name: "x", Model: "m"}).Alias(); got != "x" {
		t.Fatalf("alias = %q, want x", got)
	}
	if got := (ModelConfig{Model: "m"}).Alias(); got != "m" {
		t.Fatalf("alias = %q, want m", got)
	}
}

func TestUpdateModelSectionPersistsProviderAndName(t *testing.T) {
	p := writeTemp(t, "node:\n  name: \"n\"\n")
	mc := ModelConfig{
		Name:          "claude-primary",
		Provider:      "claude",
		APIType:       "anthropic",
		BaseURL:       "https://api.anthropic.com",
		APIKey:        "sk-ant",
		Model:         "claude-sonnet-4-5",
		MaxTokens:     8192,
		ContextWindow: 200000,
	}
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.Name != "claude-primary" {
		t.Errorf("model.name = %q, want claude-primary", cfg.Model.Name)
	}
	if cfg.Model.Provider != "claude" {
		t.Errorf("model.provider = %q, want claude", cfg.Model.Provider)
	}
	if cfg.Model.ContextWindow != 200000 {
		t.Errorf("model.context_window = %d, want 200000", cfg.Model.ContextWindow)
	}
	if cfg.Model.MaxTokens != 8192 {
		t.Errorf("model.max_tokens = %d, want 8192", cfg.Model.MaxTokens)
	}
}

// TestUpdateModelSectionCarriesCredentialReferences: the live section is the
// row the daemon dials, so a switch has to write the credential's form — and
// clear the form it left behind. Writing only api_key kept A's key_env in the
// file after switching to B (the YAML still named the account just left), and
// dropped key_scheme from a Bearer row (the token came back on the dialect
// default header and 401'd).
func TestUpdateModelSectionCarriesCredentialReferences(t *testing.T) {
	p := writeTemp(t, `node:
  name: "n"
`)
	off := false
	env := ModelConfig{
		Name: "b", Provider: "custom", APIType: "anthropic",
		BaseURL: "https://relay.example/v1", Model: "m", KeyEnv: "B_KEY",
		KeyScheme: "bearer", AllowPlaintext: true, OfflineWait: &off,
	}
	if err := UpdateModelSection(p, env); err != nil {
		t.Fatalf("UpdateModelSection: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.KeyEnv != "B_KEY" {
		t.Errorf("model.key_env = %q, want B_KEY", cfg.Model.KeyEnv)
	}
	if cfg.Model.KeyScheme != "bearer" {
		t.Errorf("model.key_scheme = %q, want bearer", cfg.Model.KeyScheme)
	}
	if !cfg.Model.AllowPlaintext {
		t.Error("model.allow_plaintext was dropped")
	}
	if cfg.Model.OfflineWait == nil || *cfg.Model.OfflineWait {
		t.Errorf("model.offline_wait = %v, want an explicit false", cfg.Model.OfflineWait)
	}

	// A literal-key row replaces the reference entirely: leaving key_env
	// behind would keep the old account's variable in the live section.
	inline := ModelConfig{
		Name: "b", Provider: "custom", APIType: "anthropic",
		BaseURL: "https://relay.example/v1", Model: "m", APIKey: "sk-b",
		KeyScheme: "bearer",
	}
	if err := UpdateModelSection(p, inline); err != nil {
		t.Fatalf("UpdateModelSection inline: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"key_env", "key_ref", "allow_plaintext", "offline_wait"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("%s survived a switch to a literal key:/n%s", gone, data)
		}
	}
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load inline: %v", err)
	}
	if cfg.Model.APIKey != "sk-b" || cfg.Model.KeyScheme != "bearer" {
		t.Errorf("inline row = key %q scheme %q", cfg.Model.APIKey, cfg.Model.KeyScheme)
	}
}

// TestUpdateModelSectionCarriesConnectionName: the console's name for a
// connection is written by the same field-by-field rewrite as the rest of the
// live section, so it has to be in that list — otherwise renaming a row that
// only exists under `model:` survives until the next restart and then falls
// back to the vendor name.
func TestUpdateModelSectionCarriesConnectionName(t *testing.T) {
	p := writeTemp(t, `node:
  name: "n"
`)
	mc := ModelConfig{
		Name: "b", Provider: "custom", APIType: "anthropic",
		BaseURL: "https://relay.example/v1", Model: "m", APIKey: "sk-b",
		ConnName: "公司中转",
	}
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.ConnName != "公司中转" {
		t.Errorf("model.conn_name = %q, want 公司中转", cfg.Model.ConnName)
	}

	// Clearing the name removes the key rather than leaving an empty string.
	mc.ConnName = ""
	if err := UpdateModelSection(p, mc); err != nil {
		t.Fatalf("UpdateModelSection clear: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "conn_name") {
		t.Errorf("conn_name survived a cleared name:\n%s", data)
	}
}
