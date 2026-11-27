// SPDX-License-Identifier: AGPL-3.0-or-later

package ccswitch

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// seed writes a store at <home>/.cc-switch/cc-switch.db with one provider.
func seed(t *testing.T) (home, id string) {
	t.Helper()
	t.Setenv("CC_SWITCH_DB", "")
	home = t.TempDir()
	dir := filepath.Join(home, ".cc-switch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "cc-switch.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, is_current BOOLEAN NOT NULL DEFAULT 0,
		sort_index INTEGER)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	id = "p-1"
	if _, err := db.Exec(
		`INSERT INTO providers (id, app_type, name, settings_config, is_current, sort_index) VALUES (?,?,?,?,?,?)`,
		id, "claude", "Zhipu GLM",
		`{"env":{"ANTHROPIC_BASE_URL":"https://open.bigmodel.cn/api/anthropic","ANTHROPIC_AUTH_TOKEN":"tok-123","ANTHROPIC_MODEL":"glm-5.3-flash"}}`,
		true, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return home, id
}

func TestReadFlattensClaudeSettings(t *testing.T) {
	home, id := seed(t)
	providers, err := Read(Path(home))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.ID != id || p.AppType != "claude" || p.Name != "Zhipu GLM" {
		t.Fatalf("identity = %+v", p)
	}
	if p.BaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Fatalf("base = %q", p.BaseURL)
	}
	if len(p.Models) != 1 || p.Models[0] != "glm-5.3-flash" {
		t.Fatalf("models = %v", p.Models)
	}
	if !p.HasToken || !p.Bearer || !p.Current {
		t.Fatalf("flags = hasToken:%v bearer:%v current:%v", p.HasToken, p.Bearer, p.Current)
	}
}

func TestResolveKeyReadsTheReferencedProvider(t *testing.T) {
	home, id := seed(t)
	if got := ResolveKey(MakeRef("claude", id), home); got != "tok-123" {
		t.Fatalf("ResolveKey = %q, want the stored token", got)
	}
	// A reference to a row that is not there is an empty key, not a panic.
	if got := ResolveKey(MakeRef("claude", "missing"), home); got != "" {
		t.Fatalf("ResolveKey(missing) = %q, want empty", got)
	}
	if got := ResolveKey("env:NOT_A_REF", home); got != "" {
		t.Fatalf("ResolveKey(env:) = %q, want empty", got)
	}
}

func TestReadMissingStoreIsNotAnError(t *testing.T) {
	providers, err := Read(filepath.Join(t.TempDir(), "nope.db"))
	if err != nil {
		t.Fatalf("Read missing: %v", err)
	}
	if providers != nil {
		t.Fatalf("providers = %+v, want nil", providers)
	}
}

func TestReadAndResolveGeminiFromTheOverriddenStore(t *testing.T) {
	home, _ := seed(t)
	override := filepath.Join(t.TempDir(), "CC Switch #1.db")
	if err := os.Rename(Path(home), override); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", override)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM providers"); err != nil {
		t.Fatal(err)
	}
	providers, err := Read(override)
	if err != nil || providers == nil || len(providers) != 0 {
		t.Fatalf("empty store: %d entries, present=%v, error=%v", len(providers), providers != nil, err)
	}
	for id, settings := range map[string]string{
		"api":    `{"env":{"GEMINI_MODEL":"models/gemini-test","GEMINI_API_KEY":"gemini-fixture"}}`,
		"vertex": `{"env":{"GEMINI_MODEL":"gemini-test","GEMINI_API_KEY":"vertex-fixture","GOOGLE_GENAI_USE_VERTEXAI":"true"}}`,
	} {
		if _, err := db.Exec("INSERT INTO providers VALUES (?, ?, ?, ?, 0, 1)", id, AppGemini, id, settings); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CC_SWITCH_DB", override)
	providers, err = Read(override)
	if err != nil || len(providers) != 2 {
		t.Fatalf("Gemini store: %d entries, error=%v", len(providers), err)
	}
	google := providers[0]
	if google.APIType != "google" || google.BaseURL != "https://generativelanguage.googleapis.com" ||
		len(google.Models) != 1 || google.Models[0] != "gemini-test" || !google.HasToken || providers[1].HasToken {
		t.Fatal("Gemini model, default endpoint, or auth type was parsed incorrectly")
	}
	if ResolveKey(MakeRef(AppGemini, "api"), home) != "gemini-fixture" ||
		ResolveKey(MakeRef(AppGemini, "vertex"), home) != "" {
		t.Fatal("Gemini credential resolution does not match discovery")
	}
}

func TestClaudeFormatsFromCurrentAndLegacyCCSwitchSettings(t *testing.T) {
	for _, tc := range []struct {
		name, meta, fields, protocol string
		bearer, hasToken             bool
	}{
		{"metadata prefers Chat", `{"apiFormat":"openai_chat"}`, `,"api_format":"openai_responses"`, "openai", true, true},
		{"legacy Responses", "", `,"api_format":"openai_responses"`, "responses", true, true},
		{"legacy compat mode", "", `,"openrouter_compat_mode":true`, "openai", true, true},
		{"native Gemini API key", `{"apiFormat":"gemini_native"}`, "", "google", false, true},
		{"managed OAuth", `{"apiFormat":"openai_responses","providerType":"codex_oauth"}`, "", "responses", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := Provider{AppType: AppClaudeDesktop}
			applySettings(&provider, `{"env":{"ANTHROPIC_AUTH_TOKEN":"fixture"}`+tc.fields+`}`, tc.meta)
			if provider.APIType != tc.protocol || provider.Bearer != tc.bearer || provider.HasToken != tc.hasToken {
				t.Fatalf("got protocol=%s bearer=%v hasToken=%v", provider.APIType, provider.Bearer, provider.HasToken)
			}
		})
	}
}

// seedCodex writes a store with one Codex provider: auth.json under "auth"
// and config.toml as a string under "config", which is the shape CC Switch
// stores for that app.
func seedCodex(t *testing.T) (home, id string) {
	t.Helper()
	t.Setenv("CC_SWITCH_DB", "")
	home = t.TempDir()
	dir := filepath.Join(home, ".cc-switch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "cc-switch.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, is_current BOOLEAN NOT NULL DEFAULT 0,
		sort_index INTEGER)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	id = "c-1"
	cfg := `{"auth":{"OPENAI_API_KEY":"sk-test"},"config":"model_provider = \"custom\"\nmodel = \"deepseek-v4-flash\"\n\n[model_providers.custom]\nname = \"deepseek\"\nbase_url = \"https://api.deepseek.com\"\nwire_api = \"responses\"\n"}`
	if _, err := db.Exec(
		`INSERT INTO providers (id, app_type, name, settings_config, is_current, sort_index) VALUES (?,?,?,?,?,?)`,
		id, "codex", "DeepSeek", cfg, true, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return home, id
}

func TestReadCodexRow(t *testing.T) {
	home, id := seedCodex(t)
	providers, err := Read(Path(home))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.AppType != AppCodex || p.Name != "DeepSeek" {
		t.Fatalf("identity = %+v", p)
	}
	if p.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base = %q", p.BaseURL)
	}
	if p.APIType != "responses" {
		t.Fatalf("api_type = %q, want responses (wire_api)", p.APIType)
	}
	if len(p.Models) != 1 || p.Models[0] != "deepseek-v4-flash" {
		t.Fatalf("models = %v", p.Models)
	}
	if !p.HasToken || p.Bearer {
		t.Fatalf("flags = hasToken:%v bearer:%v, want a plain API key", p.HasToken, p.Bearer)
	}
	if got := ResolveKey(MakeRef(AppCodex, id), home); got != "sk-test" {
		t.Fatalf("ResolveKey = %q, want the Codex API key", got)
	}
}

// A Codex row that signs in with ChatGPT keeps no OPENAI_API_KEY; there is no
// credential OpenPanda can reuse, so the collector must skip it.
func TestCodexChatGPTRowHasNoToken(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".cc-switch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "cc-switch.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, is_current BOOLEAN NOT NULL DEFAULT 0,
		sort_index INTEGER)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	cfg := `{"auth":{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"x"}},"config":"model = \"gpt-5.4\"\n"}`
	if _, err := db.Exec(
		`INSERT INTO providers (id, app_type, name, settings_config, is_current, sort_index) VALUES (?,?,?,?,?,?)`,
		"c-2", "codex", "ChatGPT", cfg, true, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	providers, err := Read(Path(home))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(providers) != 1 || providers[0].HasToken {
		t.Fatalf("providers = %+v, want a tokenless ChatGPT row", providers)
	}
}
