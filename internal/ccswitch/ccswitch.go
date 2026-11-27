// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ccswitch reads CC Switch's provider store.
//
// CC Switch is a desktop switcher for Claude Code / Codex / Gemini endpoints:
// the person keeps a list of providers there (name, endpoint, token, model
// tiers) and the app rewrites the client's own config to whichever one is
// current. That list is the interesting thing for discovery — it is every
// relay the person already configured, not just the one that happens to be
// live — but it lives in a SQLite file the app owns, so this package only ever
// opens it read-only and only ever answers two questions: "which providers are
// configured" and "what is this provider's token".
//
// Two deliberate limits:
//
//   - Read-only, always. The file is opened with mode=ro and a busy timeout:
//     CC Switch may be writing while discovery runs, and a scan must never be
//     able to corrupt or block the app that owns the store.
//   - Nothing secret leaves the process. Read returns the endpoint, the model
//     ids and *whether* a token exists; ResolveKey is the only function that
//     returns a secret, and its only caller is the model client at call time.
package ccswitch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// The app types CC Switch stores. Each one keeps its settings in a different
// shape under settings_config, which is why the parser branches on this.
const (
	AppClaude        = "claude"
	AppClaudeDesktop = "claude-desktop"
	AppCodex         = "codex"
	AppGemini        = "gemini"
)

// Provider is one configured endpoint, flattened out of the store's
// settings_config JSON.
type Provider struct {
	// ID is CC Switch's own row id (a UUID). KeyRef names it.
	ID string
	// AppType is "claude", "claude-desktop", "codex" or "gemini".
	AppType string
	// Name is the label the person gave the provider ("Zhipu GLM", "本地").
	// It is the most recognisable thing a discovered row can show.
	Name string
	// BaseURL is the endpoint the provider dials, empty for a vendor-official
	// row that leaves the endpoint to the client's default.
	BaseURL string
	// Models are the model ids the provider pins, in tier order and deduped.
	// Claude Desktop rows usually carry none — the client picks the model.
	Models []string
	// APIType is the wire dialect this row speaks: "anthropic" for the Claude
	// apps, "responses" or "openai" for Codex (its wire_api).
	APIType string
	// HasToken says a credential is stored for this provider. The value never
	// leaves ResolveKey.
	HasToken bool
	// Bearer marks a credential stored as ANTHROPIC_AUTH_TOKEN rather than
	// ANTHROPIC_API_KEY: the two headers are not interchangeable on a relay,
	// so the imported row has to keep the scheme.
	Bearer bool
	// Current marks the provider CC Switch has written into the live config.
	Current bool
	// SortIndex is CC Switch's own ordering; it keeps discovery output stable.
	SortIndex int
}

// Path returns the store's default location under home.
func Path(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cc-switch", "cc-switch.db")
}

// StorePath keeps discovery and credential resolution on the same store.
func StorePath(home, override string) string {
	if path := strings.TrimSpace(override); path != "" {
		return path
	}
	return Path(home)
}

// open opens the store read-only. A missing file is not an error the caller
// has to distinguish: it means CC Switch is not installed here.
func open(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Path: filepath.ToSlash(abs)}
	dsn := "file:" + u.EscapedPath() + "?mode=ro&_pragma=busy_timeout(1500)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: the file is read a handful of rows at a time, and a pool
	// would open the same read-only handle several times for no gain.
	db.SetMaxOpenConns(1)
	return db, nil
}

// Read returns every provider in the store, ordered the way CC Switch orders
// them. A store that does not exist yields (nil, nil): absence is a status,
// not a failure.
func Read(path string) ([]Provider, error) {
	return ReadContext(context.Background(), path)
}

// ReadContext is Read with a caller-supplied scan deadline.
func ReadContext(ctx context.Context, path string) ([]Provider, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	const selectProviders = "SELECT id, app_type, name, settings_config, is_current, sort_index, "
	const orderProviders = " FROM providers ORDER BY app_type, sort_index, name, id"
	rows, err := db.QueryContext(ctx, selectProviders+"meta"+orderProviders)
	if err != nil && strings.Contains(err.Error(), "no such column: meta") {
		rows, err = db.QueryContext(ctx, selectProviders+"NULL"+orderProviders)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Provider{}
	for rows.Next() {
		var (
			id, appType, name, cfg string
			current                bool
			sortIndex              sql.NullInt64
			meta                   sql.NullString
		)
		if err := rows.Scan(&id, &appType, &name, &cfg, &current, &sortIndex, &meta); err != nil {
			return nil, err
		}
		p := Provider{
			ID:        strings.TrimSpace(id),
			AppType:   strings.TrimSpace(appType),
			Name:      strings.TrimSpace(name),
			Current:   current,
			SortIndex: int(sortIndex.Int64),
		}
		applySettings(&p, cfg, meta.String)
		out = append(out, p)
	}
	return out, rows.Err()
}

// settingsConfig is the union of the shapes CC Switch stores. Claude rows put
// the settings.json body under "env"; Codex rows put auth.json under "auth"
// and config.toml (as a string) under "config"; Gemini stores its .env fields
// under "env".
type settingsConfig struct {
	Env              map[string]string `json:"env"`
	Auth             map[string]any    `json:"auth"`
	Config           any               `json:"config"`
	AuthMode         string            `json:"auth_mode"`
	LegacyAPIFormat  string            `json:"api_format"`
	OpenRouterCompat json.RawMessage   `json:"openrouter_compat_mode"`
}

type providerMeta struct {
	APIFormat    string `json:"apiFormat"`
	ProviderType string `json:"providerType"`
}

// applySettings flattens one settings_config row into the provider.
func applySettings(p *Provider, raw, rawMeta string) {
	var doc settingsConfig
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return
	}
	var meta providerMeta
	if rawMeta != "" {
		_ = json.Unmarshal([]byte(rawMeta), &meta)
	}
	switch p.AppType {
	case AppClaude, AppClaudeDesktop:
		applyClaude(p, doc.Env)
		p.APIType = doc.claudeAPIType(meta)
	case AppCodex:
		applyCodex(p, doc.Config)
	case AppGemini:
		applyGemini(p, doc.Env)
	}
	key, bearer := doc.credential(p.AppType)
	switch meta.ProviderType {
	case "github_copilot", "codex_oauth", "xai_oauth":
		// These providers obtain their credentials from the CC Switch proxy.
		return
	}
	if p.APIType == "google" && (p.AppType == AppClaude || p.AppType == AppClaudeDesktop) {
		if strings.HasPrefix(key, "ya29.") || strings.HasPrefix(key, "{") {
			return // CC Switch's Gemini CLI OAuth flow
		}
		bearer = false // Native Gemini keys are sent as x-goog-api-key.
	}
	p.HasToken, p.Bearer = key != "", bearer
}

func (doc settingsConfig) claudeAPIType(meta providerMeta) string {
	format := strings.TrimSpace(meta.APIFormat)
	if format == "" {
		format = strings.TrimSpace(doc.LegacyAPIFormat)
	}
	switch format {
	case "openai_chat":
		return "openai"
	case "openai_responses":
		return "responses"
	case "gemini_native":
		return "google"
	case "":
		switch strings.ToLower(strings.Trim(string(doc.OpenRouterCompat), `" `)) {
		case "true", "1":
			return "openai"
		}
	}
	return "anthropic"
}

// applyClaude reads the Claude Code settings.json body: the keys here are the
// ones Claude Code itself reads.
func applyClaude(p *Provider, env map[string]string) {
	p.BaseURL = strings.TrimRight(strings.TrimSpace(env["ANTHROPIC_BASE_URL"]), "/")
	seen := make(map[string]bool, len(claudeTierKeys))
	for _, key := range claudeTierKeys {
		id := strings.TrimSpace(env[key])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		p.Models = append(p.Models, id)
	}
}

// applyCodex reads the Codex pair: auth.json under "auth" and config.toml as a
// string under "config". A row whose auth carries no OPENAI_API_KEY relies on
// the ChatGPT login, which OpenPanda cannot reuse — HasToken stays false and
// the collector skips it.
func applyCodex(p *Provider, config any) {
	text, _ := config.(string)
	model, _, base, wire := parseCodexConfig(text)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(base), "/")
	if model != "" {
		p.Models = []string{model}
	}
	switch strings.ToLower(strings.TrimSpace(wire)) {
	case "responses":
		p.APIType = "responses"
	default:
		// "chat" is the OpenAI-compatible surface; an empty wire_api is the
		// CLI's default, which is also chat completions.
		p.APIType = "openai"
	}
}

func applyGemini(p *Provider, env map[string]string) {
	p.APIType = "google"
	p.BaseURL = strings.TrimRight(strings.TrimSpace(env["GOOGLE_GEMINI_BASE_URL"]), "/")
	if p.BaseURL == "" {
		p.BaseURL = "https://generativelanguage.googleapis.com"
	}
	if model := strings.TrimPrefix(strings.TrimSpace(env["GEMINI_MODEL"]), "models/"); model != "" {
		p.Models = []string{model}
	}
}

// parseCodexConfig pulls the four fields an import needs out of a config.toml
// body: the pinned model, the provider id, and that provider's base_url and
// wire_api. It is a line reader, not a TOML parser, because those four keys
// are flat strings and a full parser would be a dependency for nothing.
func parseCodexConfig(text string) (model, providerID, baseURL, wireAPI string) {
	type endpoint struct{ base, wire string }
	providers := make(map[string]endpoint, 4)
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if id, isProvider := strings.CutPrefix(section, "model_providers."); isProvider {
			if id == "" {
				continue
			}
			cur := providers[id]
			switch key {
			case "base_url":
				cur.base = value
			case "wire_api":
				cur.wire = value
			}
			providers[id] = cur
			continue
		}
		if section != "" {
			continue
		}
		switch key {
		case "model":
			model = value
		case "model_provider":
			providerID = value
		}
	}
	if p, ok := providers[providerID]; ok {
		baseURL, wireAPI = p.base, p.wire
	}
	return model, providerID, baseURL, wireAPI
}

// claudeTierKeys mirrors the env names Claude Code reads a model id from, in
// the same order the settings collector uses. The `_NAME` siblings are display
// aliases, not model ids, and CLAUDE_CODE_SUBAGENT_MODEL names a model the
// endpoint need not serve — both are deliberately absent.
var claudeTierKeys = []string{
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL",
}

// RefPrefix names a credential that lives in the CC Switch store:
// "ccswitch:<app_type>:<provider id>". It is stored in config as a reference,
// so the secret stays in the file that already holds it and a key rotation in
// CC Switch is picked up without re-importing.
const RefPrefix = "ccswitch:"

// MakeRef builds the reference for one provider.
func MakeRef(appType, id string) string {
	return RefPrefix + appType + ":" + id
}

// ResolveKey returns the token a reference names. It is the only function in
// this package that returns a secret, and the model client is its only caller.
// An unreadable store, an unknown id or a provider without a token all answer
// "" — the client then reports the row as having no key, which is the truth.
func ResolveKey(ref, home string) string {
	appType, id, ok := splitRef(ref)
	if !ok {
		return ""
	}
	path := StorePath(home, os.Getenv("CC_SWITCH_DB"))
	if path == "" {
		return ""
	}
	db, err := open(path)
	if err != nil {
		return ""
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow(`SELECT settings_config FROM providers WHERE app_type = ? AND id = ?`, appType, id).Scan(&raw); err != nil {
		return ""
	}
	var doc settingsConfig
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return ""
	}
	key, _ := doc.credential(appType)
	return key
}

func (doc settingsConfig) credential(appType string) (key string, bearer bool) {
	switch appType {
	case AppCodex:
		key, _ = doc.Auth["OPENAI_API_KEY"].(string)
	case AppGemini:
		switch strings.ToLower(strings.TrimSpace(doc.Env["GOOGLE_GENAI_USE_VERTEXAI"])) {
		case "true", "1":
			return "", false
		}
		key = doc.Env["GEMINI_API_KEY"]
	case AppClaude, AppClaudeDesktop:
		// AUTH_TOKEN wins because it is the Bearer credential the relay expects.
		if token := strings.TrimSpace(doc.Env["ANTHROPIC_AUTH_TOKEN"]); token != "" {
			return token, true
		}
		bearer = doc.AuthMode == "bearer_only" || doc.Env["AUTH_MODE"] == "bearer_only"
		for _, env := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
			if key = strings.TrimSpace(doc.Env[env]); key != "" {
				return key, bearer
			}
		}
	}
	return strings.TrimSpace(key), false
}

func splitRef(ref string) (appType, id string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(ref), RefPrefix)
	if !found {
		return "", "", false
	}
	appType, id, found = strings.Cut(rest, ":")
	if !found || appType == "" || id == "" {
		return "", "", false
	}
	return appType, id, true
}

// Describe is the source's one-line status detail, kept here so the collector
// does not have to duplicate the counting rules. "Importable" means the row has
// an endpoint, a model and a token — the three things a registry row cannot
// invent.
func Describe(providers []Provider) string {
	importable := 0
	for _, p := range providers {
		if p.BaseURL != "" && len(p.Models) > 0 && p.HasToken {
			importable++
		}
	}
	if len(providers) == 0 {
		return "no providers configured"
	}
	return fmt.Sprintf("%d provider(s), %d importable", len(providers), importable)
}
