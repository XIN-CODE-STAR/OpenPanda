// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

// The registry's write semantics: adding never overwrites, editing rewrites one
// row, deleting one row is always possible (including the live one, which
// promotes a successor), and an endpoint that belongs to another plan of the
// same vendor is refused instead of persisted.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/localmodels"
	"gopkg.in/yaml.v3"
)

// newModelPanel builds a panel over a config the test owns, with no engine.
// The registry endpoints never dial, so the engineless path covers them — and
// it keeps the assertions against config.yaml honest (what was written, not
// what memory happens to hold).
func newModelPanel(t *testing.T, mutate func(*config.Config)) (http.Handler, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.Storage.WorkPath = tmp
	cfg.Storage.DBPath = filepath.Join(tmp, "models.db")
	if mutate != nil {
		mutate(cfg)
	}
	configPath := filepath.Join(tmp, "config.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	h := New(Deps{
		Store:      newTestStore(t),
		Cfg:        cfg,
		ConfigPath: configPath,
		StaticDir:  t.TempDir(),
		Token:      testToken,
	})
	return h, configPath
}

// readConfig parses the file the panel wrote.
func readConfig(t *testing.T, path string) config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func entryURL(alias, model, base string) string {
	q := url.Values{}
	if model != "" {
		q.Set("model", model)
	}
	if base != "" {
		q.Set("base_url", base)
	}
	if len(q) == 0 {
		return "/api/models/" + url.PathEscape(alias)
	}
	return "/api/models/" + url.PathEscape(alias) + "?" + q.Encode()
}

// A second account for the same provider must become a second row. The old
// handler replaced whatever carried the same alias, so the first account's key
// silently vanished — and there was no way to keep two at once.
func TestAddModelKeepsASecondAccountSideBySide(t *testing.T) {
	h, path := newModelPanel(t, nil)

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","api_key":"sk-first","alias":"work","model":"deepseek-flash"}`))
	if code != http.StatusOK {
		t.Fatalf("first add = %d %v", code, out)
	}
	if out["alias"] != "work" {
		t.Fatalf("first alias = %v, want work", out["alias"])
	}

	code, out = doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","api_key":"sk-second","alias":"work","model":"deepseek-flash"}`))
	if code != http.StatusOK {
		t.Fatalf("second add = %d %v", code, out)
	}
	if out["alias"] != "work-2" {
		t.Fatalf("second alias = %v, want the suffixed free name", out["alias"])
	}

	cfg := readConfig(t, path)
	if len(cfg.Models) != 2 {
		t.Fatalf("models on disk = %d, want 2 (both accounts)", len(cfg.Models))
	}
	keys := cfg.Models[0].APIKey + " " + cfg.Models[1].APIKey
	if !strings.Contains(keys, "sk-first") || !strings.Contains(keys, "sk-second") {
		t.Fatalf("one account overwrote the other: keys = %q", keys)
	}
}

func TestModelNoAuthClearsCredentialsAndAllowsKnownProviderRelay(t *testing.T) {
	for _, storage := range []string{"env", "ref", "subscription", "inline"} {
		t.Run(storage, func(t *testing.T) {
			t.Setenv("PANDA_PANEL_TEST_KEY", "synthetic-key")
			h, path := newModelPanel(t, func(c *config.Config) {
				m := config.ModelConfig{Name: "relay", Provider: "custom", BaseURL: "http://127.0.0.1:18701/v1", Model: "demo", APIType: config.APITypeOpenAI}
				switch storage {
				case "env":
					m.KeyEnv = "PANDA_PANEL_TEST_KEY"
				case "ref":
					m.KeyRef = "ccswitch:claude:fixture"
					m.KeyScheme = "bearer"
				case "subscription":
					m.Auth = "openai-codex"
				case "inline":
					m.APIKey = "synthetic-key"
				}
				c.Model = m
				c.Models = []config.ModelConfig{m}
			})
			code, out := doJSON(t, h, jsonReq(http.MethodPut, "/api/models/relay",
				`{"model":"demo","no_auth":true}`))
			if code != http.StatusOK || out["no_auth"] != true {
				t.Fatalf("no-auth update = %d %v", code, out)
			}
			cfg := readConfig(t, path)
			for _, m := range append(cfg.Models, cfg.Model) {
				if !m.NoAuth || m.APIKey != "" || m.KeyEnv != "" || m.KeyRef != "" || m.KeyScheme != "" || m.Auth != "" {
					t.Fatalf("credential survived no-auth save for %s", storage)
				}
			}
		})
	}
	h, path := newModelPanel(t, nil)
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","model":"deepseek-chat","base_url":"http://127.0.0.1:18701/v1","no_auth":true}`))
	if code != http.StatusOK || out["no_auth"] != true {
		t.Fatalf("known provider relay = %d %v", code, out)
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 1 || !cfg.Models[0].NoAuth || !cfg.Model.NoAuth {
		t.Fatal("new no-auth relay is missing from the registry or active model")
	}
}

func TestImportModelsSkipsLegacyActiveIdentity(t *testing.T) {
	for _, registered := range []bool{false, true} {
		h, path := newModelPanel(t, func(c *config.Config) {
			c.Model = config.ModelConfig{Name: "legacy", Provider: "custom", BaseURL: "https://example.invalid/v1", Model: "demo", APIType: config.APITypeOpenAI, NoAuth: true}
			if registered {
				c.Models = []config.ModelConfig{c.Model}
			}
		})
		code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/import",
			`{"items":[{"provider":"custom","model":"demo","base_url":"https://example.invalid/v1","api_type":"openai","no_auth":true}]}`))
		if code != http.StatusOK || out["added"] != 0.0 {
			t.Fatalf("import existing identity = %d %v", code, out)
		}
		cfg := readConfig(t, path)
		want := 0
		if registered {
			want = 1
		}
		if len(cfg.Models) != want || cfg.Model.Name != "legacy" {
			t.Fatalf("import changed existing configuration: registry size %d, active alias %q", len(cfg.Models), cfg.Model.Name)
		}
	}
}

// The completion cap used to be dropped for every catalogue provider (only the
// custom branch kept it), so a typed number came back as the auto value.
func TestAddModelAppliesTheLimitsTheUserTyped(t *testing.T) {
	h, _ := newModelPanel(t, nil)

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","api_key":"sk-a","alias":"cap","model":"deepseek-flash","max_tokens":8192,"context_window":123456}`))
	if code != http.StatusOK {
		t.Fatalf("add = %d %v", code, out)
	}
	if out["max_tokens"] != 8192.0 {
		t.Fatalf("max_tokens = %v, want 8192", out["max_tokens"])
	}
	if out["context_window"] != 123456.0 {
		t.Fatalf("context_window = %v, want 123456", out["context_window"])
	}
	if out["max_tokens_auto"] != nil || out["context_auto"] != nil {
		t.Fatalf("an explicit value must not read as auto: %v", out)
	}
}

// Untouched limits stay auto, and the panel reports the numbers the resolver
// picked so the form can show them as placeholders.
func TestAddModelLeavesUntouchedLimitsOnAuto(t *testing.T) {
	h, _ := newModelPanel(t, nil)

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","api_key":"sk-a","alias":"auto","model":"deepseek-flash"}`))
	if code != http.StatusOK {
		t.Fatalf("add = %d %v", code, out)
	}
	if out["max_tokens_auto"] != true || out["context_auto"] != true {
		t.Fatalf("unset limits should read as auto: %v", out)
	}
	if out["auto_max_tokens"] == nil || out["auto_context_window"] == nil {
		t.Fatalf("auto values missing: %v", out)
	}
}

// Zen and Go are sibling rosters on one host; a base pasted from the other
// plan's docs must be refused at write time, because every later call would
// fail with nothing on screen to explain it.
func TestAddModelRefusesTheOtherPlansEndpoint(t *testing.T) {
	h, path := newModelPanel(t, nil)

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"opencode","api_key":"sk-zen","base_url":"https://opencode.ai/zen/go/v1","model":"deepseek-v4.1-flash","alias":"mixed"}`))
	if code != http.StatusBadRequest {
		t.Fatalf("mismatched plan add = %d %v, want 400", code, out)
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 0 {
		t.Fatalf("a refused config reached the file: %+v", cfg.Models)
	}

	// The same base under the row that owns it is fine.
	code, out = doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"opencode-go","api_key":"sk-go","base_url":"https://opencode.ai/zen/go/v1","model":"deepseek-v4.1-flash","alias":"go"}`))
	if code != http.StatusOK {
		t.Fatalf("go add = %d %v", code, out)
	}
	if out["provider"] != "opencode-go" {
		t.Fatalf("provider = %v", out["provider"])
	}
}

// A config written earlier with the mismatch still loads; it is flagged so the
// console can say what is wrong instead of leaving the user to guess.
func TestListModelsFlagsAForeignPlanEndpoint(t *testing.T) {
	h, _ := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name:     "opencode",
			Provider: "opencode",
			APIType:  config.APITypeOpenAI,
			BaseURL:  "https://opencode.ai/zen/go",
			Model:    "deepseek-v4.1-flash",
			APIKey:   "sk-x",
		}
	})

	code, out := doJSON(t, h, authedReq(http.MethodGet, "/api/models", nil))
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	active, _ := out["active"].(map[string]any)
	if active["warning_code"] != "endpoint-owner" || active["warning_arg"] != "opencode-go" {
		t.Fatalf("active warning = %v / %v, want endpoint-owner / opencode-go",
			active["warning_code"], active["warning_arg"])
	}
}

// The live model is deletable too — refusing outright stranded the user. When
// another row exists it takes over; when it does not, the refusal says why.
func TestRemoveModelPromotesASuccessorWhenItIsLive(t *testing.T) {
	live := config.ModelConfig{
		Name: "live", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://live.example/v1", Model: "live", APIKey: "k1",
	}
	spare := config.ModelConfig{
		Name: "spare", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://spare.example/v1", Model: "spare", APIKey: "k2",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = live
		c.Models = []config.ModelConfig{live, spare}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("live", "live", "https://live.example/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete live = %d %v", code, out)
	}
	if out["switched_to"] != "spare" {
		t.Fatalf("switched_to = %v, want spare", out["switched_to"])
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].Alias() != "spare" {
		t.Fatalf("registry after delete = %+v", cfg.Models)
	}
	// The live switch is persisted, not just held in memory.
	if cfg.Model.Alias() != "spare" {
		t.Fatalf("active model = %q, want spare", cfg.Model.Alias())
	}
}

// A model written only to `model:` — an older single-model config, or one a
// user edited by hand — is the row the page paints as "in use". It had no
// delete path on any surface: the console skipped the button for an
// unregistered row and the handler only searched the registry. Removing it now
// clears the section's owner and promotes a registered row.
func TestRemoveModelClearsTheLiveRowOutsideTheRegistry(t *testing.T) {
	stale := config.ModelConfig{
		Name: "stale", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://stale.example/v1", Model: "m", APIKey: "k1",
	}
	spare := config.ModelConfig{
		Name: "spare", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://spare.example/v1", Model: "m", APIKey: "k2",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = stale
		c.Models = []config.ModelConfig{spare}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("stale", "m", "https://stale.example/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete the unregistered live row = %d %v", code, out)
	}
	if out["switched_to"] != "spare" {
		t.Fatalf("switched_to = %v, want spare", out["switched_to"])
	}
	cfg := readConfig(t, path)
	if cfg.Model.Alias() != "spare" {
		t.Fatalf("live model = %q, want the promoted spare", cfg.Model.Alias())
	}
	if len(cfg.Models) != 1 || cfg.Models[0].Alias() != "spare" {
		t.Fatalf("registry after the delete = %+v, want spare untouched", cfg.Models)
	}
}

// Removing the last model is the request a user makes when the only model
// they configured is wrong. It used to be refused with "add another one
// first" — which is the very complaint the report started from, just moved one
// step later. The section is cleared deliberately instead, the same way
// `/model remove` and the TUI panel already did it.
func TestRemoveModelClearsTheLastModel(t *testing.T) {
	only := config.ModelConfig{
		Name: "only", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://only.example/v1", Model: "only", APIKey: "k",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = only
		c.Models = []config.ModelConfig{only}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("only", "only", "https://only.example/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete last = %d %v, want the row gone", code, out)
	}
	if out["cleared"] != true {
		t.Fatalf("response = %v, want cleared:true so the console can say why it is empty", out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 0 {
		t.Fatalf("registry after deleting the last model = %+v", cfg.Models)
	}
	if cfg.Model.Model != "" || cfg.Model.Provider != "" || cfg.Model.BaseURL != "" {
		t.Fatalf("model: still names the deleted entry: %+v", cfg.Model)
	}
	// The in-memory config agrees with the file, so the list does not paint a
	// row that no longer exists.
	code, list := doJSON(t, h, authedReq(http.MethodGet, "/api/models", nil))
	if code != http.StatusOK {
		t.Fatalf("list after the clear = %d", code)
	}
	if rows, _ := list["models"].([]any); len(rows) != 0 {
		t.Fatalf("rows after the clear = %v", rows)
	}
	if active, _ := list["active"].(map[string]any); active["alias"] != "" {
		t.Fatalf("a cleared config still reports an active model: %v", active)
	}
}

// The reported bug: a registry row that merely shares the live row's alias —
// a different endpoint, a second account — was treated as the live row and
// could never be removed.
func TestRemoveModelDropsASameNamedRowOnAnotherEndpoint(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "custom", APIType: config.APITypeOpenAI,
			BaseURL: "https://live.example/v1", Model: "m", APIKey: "k1",
		}
		c.Models = []config.ModelConfig{{
			Name: "opencode", Provider: "custom", APIType: config.APITypeOpenAI,
			BaseURL: "https://other.example/v1", Model: "m", APIKey: "k2",
		}}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("opencode", "m", "https://other.example/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete same-named row = %d %v", code, out)
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 0 {
		t.Fatalf("row still on disk: %+v", cfg.Models)
	}
	if cfg := readConfig(t, path); cfg.Model.BaseURL != "https://live.example/v1" {
		t.Fatalf("the live endpoint moved: %+v", cfg.Model)
	}
}

// Editing in place, including the live entry that was never registered (the
// row the console paints at the top of the page).
func TestUpdateModelRewritesOneRowInPlace(t *testing.T) {
	live := config.ModelConfig{
		Name: "live", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://live.example/v1", Model: "live", APIKey: "k1",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = live
		c.Models = []config.ModelConfig{live}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut, entryURL("live", "live", "https://live.example/v1"),
		`{"model":"live-v2","max_tokens":4096,"alias":"renamed"}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	if out["alias"] != "renamed" || out["model"] != "live-v2" {
		t.Fatalf("edited row = %v", out)
	}
	if out["max_tokens"] != 4096.0 {
		t.Fatalf("max_tokens = %v, want 4096", out["max_tokens"])
	}
	if out["base_url"] != "https://live.example/v1" {
		t.Fatalf("an edit must keep the endpoint it did not restate: %v", out["base_url"])
	}
	cfg := readConfig(t, path)
	if cfg.Model.Alias() != "renamed" || cfg.Model.Model != "live-v2" {
		t.Fatalf("live entry after edit = %+v", cfg.Model)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].Alias() != "renamed" {
		t.Fatalf("registry after edit = %+v", cfg.Models)
	}
	// The stored key survives an edit that did not re-type it.
	if cfg.Model.APIKey != "k1" {
		t.Fatalf("key lost on edit: %q", cfg.Model.APIKey)
	}
}

func TestUpdateModelLeavesRegistryUnchangedWhenConfigWriteFails(t *testing.T) {
	for _, scenario := range []string{"inactive row", "active row"} {
		t.Run(scenario, func(t *testing.T) {
			const oldContext = 8192
			row := config.ModelConfig{Name: "original", Provider: "custom", APIType: config.APITypeOpenAI,
				BaseURL: "https://custom.example/v1", Model: "example", APIKey: "fixture-key", ContextWindow: oldContext}
			active := scenario == "active row"
			h, path := newModelPanel(t, func(c *config.Config) {
				c.Models = []config.ModelConfig{row}
				if active {
					c.Model = row
				}
			})
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			invalid := "models: ["
			if active {
				// A valid document with an invalid active mapping used to allow
				// the registry persist, then fail during the active model persist.
				invalid = "model: invalid\n"
			}
			if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			save := func() (int, map[string]any) {
				return doJSON(t, h, jsonReq(http.MethodPut, entryURL(row.Name, row.Model, row.BaseURL),
					`{"alias":"renamed","context_window":16384}`))
			}
			if code, out := save(); code != http.StatusInternalServerError {
				t.Fatalf("failed save: %d %v", code, out)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != invalid {
				t.Errorf("failed save changed the file: %v", err)
			}
			_, out := doJSON(t, h, authedReq(http.MethodGet, "/api/models", nil))
			models := out["models"].([]any)
			if len(models) != 1 {
				t.Fatalf("live registry after failed save: %v", models)
			}
			listed := models[0].(map[string]any)
			if listed["alias"] != row.Name || listed["context_window"] != float64(oldContext) {
				t.Errorf("failed save changed the live registry: %v", models)
			}
			if active && out["active"].(map[string]any)["alias"] != row.Name {
				t.Errorf("failed save changed the active model: %v", out["active"])
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if code, out := save(); code != http.StatusOK {
				t.Fatalf("retry: %d %v", code, out)
			}
			cfg := readConfig(t, path)
			if len(cfg.Models) != 1 || cfg.Models[0].Alias() != "renamed" || cfg.Models[0].ContextWindow != 16384 ||
				(active && (cfg.Model.Alias() != "renamed" || cfg.Model.ContextWindow != 16384)) {
				t.Fatalf("retry did not persist updated models: %+v, active=%+v", cfg.Models, cfg.Model)
			}
		})
	}
}

// The edit form paints the model, not the row's provenance or the shape of its
// credential. Editing a connection rewrites every kept row, so a field the form
// cannot echo must survive that rewrite: an isolated edit that only fixed the
// context window came back with `source` and `key_scheme` cleared, and a row
// whose token rides `Authorization: Bearer` then 401'd on every later call.
func TestUpdateModelKeepsFieldsTheFormNeverShows(t *testing.T) {
	noWait := false
	row := config.ModelConfig{
		Name: "claude", Provider: "custom", Source: "claude",
		APIType: config.APITypeOpenAI, BaseURL: "https://relay.example/v1",
		Model: "claude-sonnet", APIKey: "sk-x", KeyScheme: "bearer",
		ContextWindow: 200000, OfflineWait: &noWait,
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = row
		c.Models = []config.ModelConfig{row}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("claude", "claude-sonnet", "https://relay.example/v1"),
		`{"context_window":123456}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 {
		t.Fatalf("registry after edit = %+v", cfg.Models)
	}
	got := cfg.Models[0]
	if got.Source != "claude" {
		t.Fatalf("source lost on edit: %q", got.Source)
	}
	if got.KeyScheme != "bearer" {
		t.Fatalf("key_scheme lost on edit: %q", got.KeyScheme)
	}
	// The console only restates api_type when the user changed it, so an edit
	// that keeps the model must keep the dialect: rebuilding without it sent
	// an Anthropic body to an OpenAI-compatible relay.
	if got.APIType != config.APITypeOpenAI {
		t.Fatalf("api_type flipped on edit: %q", got.APIType)
	}
	if got.OfflineWait == nil || *got.OfflineWait {
		t.Fatalf("offline_wait lost on edit: %v", got.OfflineWait)
	}
	if got.ContextWindow != 123456 {
		t.Fatalf("edited limit did not land: %d", got.ContextWindow)
	}
}

// A connection has no record of its own — it is the rows sharing a provider,
// an endpoint, a dialect and a key — so the name the console paints on the card
// rides on every one of them. Three ways it has to move: out through the list,
// through an edit that never mentions it, and back to empty on purpose. Without
// the middle one a rename would survive only until the next unrelated save.
func TestConnectionNameRidesTheRows(t *testing.T) {
	row := config.ModelConfig{
		Name: "relay", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-1", APIKey: "sk-x",
		ConnName: "公司中转",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = row
		c.Models = []config.ModelConfig{row}
	})

	code, out := doJSON(t, h, authedReq(http.MethodGet, "/api/models", nil))
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	models, _ := out["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v", out["models"])
	}
	first, _ := models[0].(map[string]any)
	if first["conn_name"] != "公司中转" {
		t.Fatalf("conn_name on the wire = %v", first["conn_name"])
	}

	code, out = doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("relay", "m-1", "https://relay.example/v1"), `{"max_tokens":4096}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	if got := readConfig(t, path).Models[0].ConnName; got != "公司中转" {
		t.Fatalf("conn_name lost on an edit that did not mention it: %q", got)
	}

	// "" is the deliberate "name the card after the vendor again", which is a
	// different instruction from the omitted field above.
	code, out = doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("relay", "m-1", "https://relay.example/v1"), `{"conn_name":""}`))
	if code != http.StatusOK {
		t.Fatalf("clear = %d %v", code, out)
	}
	if got := readConfig(t, path).Models[0].ConnName; got != "" {
		t.Fatalf("conn_name not cleared: %q", got)
	}

	// A duplicate shares the source's endpoint and key, so it joins the same
	// card and inherits the name instead of arriving nameless.
	code, out = doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("relay", "m-1", "https://relay.example/v1"), `{"conn_name":"公司中转"}`))
	if code != http.StatusOK {
		t.Fatalf("rename = %d %v", code, out)
	}
	code, out = doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"custom","model":"m-1","base_url":"https://relay.example/v1","api_type":"openai","alias":"relay-copy","copy_from":"relay"}`))
	if code != http.StatusOK {
		t.Fatalf("copy = %d %v", code, out)
	}
	if out["conn_name"] != "公司中转" {
		t.Fatalf("a copy of a named connection arrived nameless: %v", out["conn_name"])
	}
}

// A row whose credential another application owns (an env reference or a
// subscription login) carries no api_key, so the rebuilt entry would come back
// keyless unless the reference rides the edit. A typed key replaces it.
func TestUpdateModelKeepsACredentialReference(t *testing.T) {
	row := config.ModelConfig{
		Name: "work", Provider: "custom", APIType: config.APITypeAnthropic,
		BaseURL: "https://relay.example/v1", Model: "claude-sonnet",
		KeyRef: "ccswitch:claude:work", Auth: "anthropic",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = row
		c.Models = []config.ModelConfig{row}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("work", "claude-sonnet", "https://relay.example/v1"),
		`{"max_tokens":4096}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if got := cfg.Models[0]; got.KeyRef != "ccswitch:claude:work" || got.Auth != "anthropic" {
		t.Fatalf("credential reference lost on edit: %+v", got)
	}
	if cfg.Models[0].NoAuth {
		t.Fatal("a row with a credential reference must not read as no-auth")
	}

	// A key typed into the form is the row's credential from then on.
	code, out = doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("work", "claude-sonnet", "https://relay.example/v1"),
		`{"api_key":"sk-new"}`))
	if code != http.StatusOK {
		t.Fatalf("update with a key = %d %v", code, out)
	}
	cfg = readConfig(t, path)
	if got := cfg.Models[0]; got.KeyRef != "" || got.Auth != "" || got.APIKey != "sk-new" {
		t.Fatalf("typed key did not replace the reference: %+v", got)
	}
}

// A row whose credential is a reference — an env var, another app's store, a
// subscription login — has no api_key to echo. An edit that types none must
// keep the reference: the required-key check used to answer 400, and when a
// second account of the same vendor existed it borrowed that account's static
// key instead, which then outranked the reference the row was configured with.
func TestUpdateModelKeepsAReferenceCredential(t *testing.T) {
	row := config.ModelConfig{
		Name: "work", Provider: "deepseek", APIType: config.APITypeAnthropic,
		BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-flash",
		KeyEnv: "OPENPANDA_TEST_DEEPSEEK_KEY", ContextWindow: 64000,
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = row
		c.Models = []config.ModelConfig{row}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("work", "deepseek-flash", "https://api.deepseek.com/anthropic"),
		`{"max_tokens":4096}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	got := readConfig(t, path).Models[0]
	if got.KeyEnv != "OPENPANDA_TEST_DEEPSEEK_KEY" {
		t.Fatalf("key_env lost on edit: %q", got.KeyEnv)
	}
	if got.APIKey != "" {
		t.Fatalf("a reference row picked up a literal key: %q", got.APIKey)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("edited limit did not land: %d", got.MaxTokens)
	}
}

// The provider-wide lookup must not hand a reference row the key of a second
// account under the same vendor: that key would be written into the row and
// sent instead of the credential it was configured with.
func TestUpdateModelDoesNotBorrowAnotherAccountsKeyForAReferenceRow(t *testing.T) {
	ref := config.ModelConfig{
		Name: "ref", Provider: "deepseek", APIType: config.APITypeAnthropic,
		BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-flash",
		KeyRef: "ccswitch:claude:work",
	}
	other := config.ModelConfig{
		Name: "other", Provider: "deepseek", APIType: config.APITypeAnthropic,
		BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-pro",
		APIKey: "sk-other-account",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = ref
		c.Models = []config.ModelConfig{ref, other}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("ref", "deepseek-flash", "https://api.deepseek.com/anthropic"),
		`{"max_tokens":2048}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	got := readConfig(t, path).Models[0]
	if got.KeyRef != "ccswitch:claude:work" || got.APIKey != "" {
		t.Fatalf("reference row borrowed another account: key_ref=%q api_key=%q", got.KeyRef, got.APIKey)
	}
}

// A custom relay keeps the dialect it was configured with even when the model
// id changes: the id is the vendor's, the wire shape is the endpoint's, and
// the console never restates api_type for an untouched switch.
func TestUpdateModelKeepsTheDialectWhenOnlyTheModelChanges(t *testing.T) {
	row := config.ModelConfig{
		Name: "relay", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "gpt-4o", APIKey: "sk-x",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = row
		c.Models = []config.ModelConfig{row}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("relay", "gpt-4o", "https://relay.example/v1"), `{"model":"gpt-5"}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	got := readConfig(t, path).Models[0]
	if got.Model != "gpt-5" {
		t.Fatalf("model id did not change: %q", got.Model)
	}
	if got.APIType != config.APITypeOpenAI {
		t.Fatalf("dialect flipped with the model id: %q", got.APIType)
	}
}

// Re-setting a limit to auto is a distinct instruction from "leave it alone" —
// without the reset flag a value could be typed once and never cleared.
func TestUpdateModelCanReturnALimitToAuto(t *testing.T) {
	live := config.ModelConfig{
		Name: "live", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://live.example/v1", Model: "live", APIKey: "k",
		MaxTokens: 4096, ContextWindow: 32768,
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = live
		c.Models = []config.ModelConfig{live}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut, entryURL("live", "live", "https://live.example/v1"),
		`{"max_tokens_reset":true,"context_window_reset":true}`))
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, out)
	}
	if out["max_tokens_auto"] != true || out["context_auto"] != true {
		t.Fatalf("limits not returned to auto: %v", out)
	}
	cfg := readConfig(t, path)
	if cfg.Model.MaxTokens != 0 || cfg.Model.ContextWindow != 0 {
		t.Fatalf("reset did not reach the file: %+v", cfg.Model)
	}
}

// Two accounts of one vendor share a provider id and an endpoint, so "reuse
// the key on file for this provider" cannot say which account a new row
// belongs to. Naming the row is the only unambiguous answer.
func TestAddModelBorrowsTheKeyOfTheNamedConnection(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Models = []config.ModelConfig{
			{Name: "work", Provider: "deepseek", APIType: config.APITypeAnthropic,
				BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-flash", APIKey: "sk-work"},
			{Name: "personal", Provider: "deepseek", APIType: config.APITypeAnthropic,
				BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-flash", APIKey: "sk-personal"},
		}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","model":"deepseek-chat","alias":"chat-personal","reuse_from":"personal"}`))
	if code != http.StatusOK {
		t.Fatalf("add = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	var got string
	for _, m := range cfg.Models {
		if m.Alias() == "chat-personal" {
			got = m.APIKey
		}
	}
	if got != "sk-personal" {
		t.Fatalf("new row key = %q, want the named connection's key", got)
	}
}

// The repair the console offers for a plan/endpoint mismatch: retarget the
// live entry at the plan that actually owns the endpoint it dials.
func TestUpdateModelRepairsAPlanMismatch(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
			BaseURL: "https://opencode.ai/zen/go", Model: "deepseek-v4.1-flash", APIKey: "k",
		}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut, entryURL("opencode", "deepseek-v4.1-flash", "https://opencode.ai/zen/go"),
		`{"provider":"opencode-go","base_url":"https://opencode.ai/zen/go/v1","api_type":"openai"}`))
	if code != http.StatusOK {
		t.Fatalf("repair = %d %v", code, out)
	}
	if out["warning_code"] != nil {
		t.Fatalf("still flagged after the repair: %v", out)
	}
	cfg := readConfig(t, path)
	if cfg.Model.Provider != "opencode-go" {
		t.Fatalf("live provider = %q, want the plan that owns the endpoint", cfg.Model.Provider)
	}
	if cfg.Model.APIKey != "k" {
		t.Fatalf("key lost on repair: %q", cfg.Model.APIKey)
	}
}

// Repairing a plan mismatch (the OpenCode Zen/Go mix-up) must not ask the user
// to paste the key again: both plans are one account on one host, so the stored
// credential is still the right one.
func TestUpdateModelKeepsTheKeyWhenOnlyTheSiblingPlanChanges(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
			BaseURL: "https://opencode.ai/zen/v1", Model: "deepseek-v4.1-flash", APIKey: "sk-opencode",
		}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("opencode", "deepseek-v4.1-flash", "https://opencode.ai/zen/v1"),
		`{"provider":"opencode-go","base_url":"https://opencode.ai/zen/go/v1","api_type":"openai"}`))
	if code != http.StatusOK {
		t.Fatalf("sibling-plan switch = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if cfg.Model.APIKey != "sk-opencode" {
		t.Fatalf("key across a same-host plan switch = %q, want the stored one", cfg.Model.APIKey)
	}
	if cfg.Model.Provider != "opencode-go" {
		t.Fatalf("provider = %q, want opencode-go", cfg.Model.Provider)
	}
}

// A vendor switch is not a plan switch: carrying the old key over means dialing
// a stranger's API with the previous account's token, which fails with a 401
// nothing on screen can explain. The edit must ask for a key instead.
func TestUpdateModelRefusesAVendorSwitchWithoutAKey(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
			BaseURL: "https://opencode.ai/zen/v1", Model: "deepseek-v4.1-flash", APIKey: "sk-opencode",
		}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("opencode", "deepseek-v4.1-flash", "https://opencode.ai/zen/v1"),
		`{"provider":"deepseek","base_url":"https://api.deepseek.com","api_type":"openai","model":"deepseek-flash"}`))
	if code != http.StatusBadRequest {
		t.Fatalf("vendor switch without a key = %d %v, want 400", code, out)
	}
	if cfg := readConfig(t, path); cfg.Model.Provider != "opencode" {
		t.Fatalf("a refused edit still moved the live entry: %+v", cfg.Model)
	}
}

// A vendor switch that lands on a vendor already configured elsewhere reuses
// that vendor's own key — the one written for the account being dialed.
func TestUpdateModelAdoptsTheNewVendorsStoredKey(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
			BaseURL: "https://opencode.ai/zen/v1", Model: "kimi-k3", APIKey: "sk-opencode",
		}
		c.Models = []config.ModelConfig{{
			Name: "ds", Provider: "deepseek", APIType: config.APITypeOpenAI,
			BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", APIKey: "sk-deepseek",
		}}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("opencode", "kimi-k3", "https://opencode.ai/zen/v1"),
		`{"provider":"deepseek","base_url":"https://api.deepseek.com","api_type":"openai","model":"deepseek-flash","alias":"ds-live"}`))
	if code != http.StatusOK {
		t.Fatalf("vendor switch = %d %v", code, out)
	}
	if cfg := readConfig(t, path); cfg.Model.APIKey != "sk-deepseek" {
		t.Fatalf("key after the switch = %q, want the new vendor's own", cfg.Model.APIKey)
	}
}

// A config written before aliases were de-duplicated can hold two rows under
// one name. Testing "work" has to test the row the console pointed at, not
// whichever one the scan reached first.
func TestResolveModelRefTestsTheAddressedRow(t *testing.T) {
	cfg := config.Default()
	cfg.Models = []config.ModelConfig{
		{Name: "work", Provider: "deepseek", APIType: config.APITypeOpenAI,
			BaseURL: "https://first.example/v1", Model: "deepseek-flash", APIKey: "sk-first"},
		{Name: "work", Provider: "deepseek", APIType: config.APITypeOpenAI,
			BaseURL: "https://second.example/v1", Model: "deepseek-flash", APIKey: "sk-second"},
	}
	// No engine: the resolver only reads config, and a static handler keeps the
	// assertion on lookup order rather than on anything an engine might resolve.
	panel := &handler{cfg: cfg, cfgMu: &sync.RWMutex{}}

	got, err := panel.resolveModelRefErr(modelRefRequest{
		Alias: "work", Model: "deepseek-flash", BaseURL: "https://second.example/v1",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-second" {
		t.Fatalf("resolved key = %q, want the row at the named endpoint", got.APIKey)
	}

	// A caller that names only the alias still gets an answer — the first row,
	// which is what the field meant before the endpoint was available.
	got, err = panel.resolveModelRefErr(modelRefRequest{Alias: "work"})
	if err != nil {
		t.Fatalf("resolve loose: %v", err)
	}
	if got.BaseURL != "https://first.example/v1" {
		t.Fatalf("loose resolve = %q, want the first row", got.BaseURL)
	}
}

// The console addresses a row with the endpoint the list reported, and the list
// rebases an alternate form onto the catalogue's own (`…/zen/go` becomes
// `…/zen/go/v1`). An exact match on the stored base then finds nothing — and
// the row the page is offering to repair answers "no such model" to its own
// fix. A name that is unique still resolves.
func TestUpdateModelAcceptsTheNormalizedAddress(t *testing.T) {
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{
			Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
			BaseURL: "https://opencode.ai/zen/go", Model: "deepseek-v4.1-flash", APIKey: "k",
		}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("opencode", "deepseek-v4.1-flash", "https://opencode.ai/zen/go/v1"),
		`{"provider":"opencode-go","base_url":"https://opencode.ai/zen/go/v1","api_type":"openai"}`))
	if code != http.StatusOK {
		t.Fatalf("repair through a normalized address = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if cfg.Model.Provider != "opencode-go" || cfg.Model.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("live entry after repair = %+v", cfg.Model)
	}
}

// Deleting through the same normalized address must work too, or a row the user
// can see becomes a row they cannot remove — the original complaint.
func TestRemoveModelAcceptsTheNormalizedAddress(t *testing.T) {
	live := config.ModelConfig{
		Name: "opencode", Provider: "opencode", APIType: config.APITypeOpenAI,
		BaseURL: "https://opencode.ai/zen/go", Model: "deepseek-v4.1-flash", APIKey: "k",
	}
	spare := config.ModelConfig{
		Name: "spare", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://spare.example/v1", Model: "spare", APIKey: "k2",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = live
		c.Models = []config.ModelConfig{live, spare}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("opencode", "deepseek-v4.1-flash", "https://opencode.ai/zen/go/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete through a normalized address = %d %v", code, out)
	}
	if out["switched_to"] != "spare" {
		t.Fatalf("switched_to = %v, want spare", out["switched_to"])
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 1 || cfg.Models[0].Alias() != "spare" {
		t.Fatalf("registry after delete = %+v", cfg.Models)
	}
}

// Two rows under one name stay ambiguous on purpose: guessing which account to
// delete is worse than asking for the endpoint again.
func TestRemoveModelKeepsADuplicateNameAmbiguous(t *testing.T) {
	a := config.ModelConfig{Name: "work", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://a.example/v1", Model: "m", APIKey: "ka"}
	b := config.ModelConfig{Name: "work", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://b.example/v1", Model: "m", APIKey: "kb"}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = a
		c.Models = []config.ModelConfig{a, b}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("work", "m", "https://nowhere.example/v1"), nil))
	if code != http.StatusNotFound {
		t.Fatalf("ambiguous delete = %d %v, want 404", code, out)
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 2 {
		t.Fatalf("an ambiguous delete still removed a row: %+v", cfg.Models)
	}
}

// Two accounts of one vendor are told apart in the list by the hint, so the
// hint has to carry more than the last four characters.
// "Copy this account" is a per-row operation, so it needs the row. Resolved by
// alias alone it duplicated whichever account came first — and the copy then
// carried a credential that belongs to the other one, which never
// authenticates and looks exactly like a bad key.
func TestAddModelCopiesTheAddressedAccount(t *testing.T) {
	first := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-first"}
	second := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-second"}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = first
		c.Models = []config.ModelConfig{first, second}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models",
		`{"provider":"deepseek","model":"deepseek-chat","alias":"work","copy_from":"ds","copy_from_key":"`+keyFingerprint("sk-second")+`"}`))
	if code != http.StatusOK {
		t.Fatalf("copy the second account = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 3 {
		t.Fatalf("rows = %d, want the source pair plus the copy", len(cfg.Models))
	}
	if cfg.Models[2].APIKey != "sk-second" {
		t.Fatalf("the copy borrowed the other account's key: %+v", cfg.Models[2])
	}
}

func TestMaskKeyDistinguishesTwoAccounts(t *testing.T) {
	a, b := maskKey("sk-work-aaaaac992"), maskKey("sk-home-aaaaac992")
	if a == b {
		t.Fatalf("two keys share a hint: %q", a)
	}
	if strings.Contains(a, "aaaaa") {
		t.Fatalf("hint leaks the middle of the key: %q", a)
	}
	if !strings.HasSuffix(a, "c992") || !strings.HasPrefix(a, "sk-w") {
		t.Fatalf("hint = %q, want head and tail", a)
	}
	if maskKey("short") != "••••" {
		t.Fatalf("a short key must not be reconstructable: %q", maskKey("short"))
	}
}

// ---- addressing one account when two share every visible field ----

// A config written before names were kept unique can hold two rows under one
// alias with the same model id and endpoint; the credential is the only thing
// telling them apart, and key_fingerprint is how the console learns which one
// a click meant. Nothing consumed it, so the delete addressed the first row
// named "ds" — the other account, and the one still in use.
func TestRemoveModelAddressesTheAccountByKeyDigest(t *testing.T) {
	first := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-first"}
	second := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-second"}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = first
		c.Models = []config.ModelConfig{first, second}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("ds", "deepseek-chat", "")+"&key="+keyFingerprint("sk-second"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete the second account = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].APIKey != "sk-first" {
		t.Fatalf("deleted the wrong account: %+v", cfg.Models)
	}
	if cfg.Model.APIKey != "sk-first" {
		t.Fatalf("the delete moved the live model: %+v", cfg.Model)
	}
}

// The same address is what an edit needs: rewriting "the first row named ds"
// rotated the other account's credential and left the addressed one alone.
func TestUpdateModelAddressesTheAccountByKeyDigest(t *testing.T) {
	first := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-first"}
	second := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-second"}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = first
		c.Models = []config.ModelConfig{first, second}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPut,
		entryURL("ds", "deepseek-chat", "")+"&key="+keyFingerprint("sk-second"),
		`{"max_tokens":1234}`))
	if code != http.StatusOK {
		t.Fatalf("update the second account = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 2 {
		t.Fatalf("rows = %d, want both kept", len(cfg.Models))
	}
	if cfg.Models[0].MaxTokens == 1234 {
		t.Fatalf("the edit landed on the first account: %+v", cfg.Models)
	}
	if cfg.Models[1].MaxTokens != 1234 {
		t.Fatalf("the addressed account was not edited: %+v", cfg.Models)
	}
	if cfg.Model.APIKey != "sk-first" {
		t.Fatalf("editing a spare account took the live slot: %+v", cfg.Model)
	}
}

// Only the row the engine is actually dialing is "in use". Identity that
// stopped at alias+model+endpoint lit both accounts, so the page claimed two
// live models and its one-click switch refused both tiles as already current.
func TestListModelsMarksOnlyTheLiveAccount(t *testing.T) {
	first := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-first"}
	second := config.ModelConfig{Name: "ds", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-second"}
	h, _ := newModelPanel(t, func(c *config.Config) {
		c.Model = second
		c.Models = []config.ModelConfig{first, second}
	})

	code, out := doJSON(t, h, authedReq(http.MethodGet, "/api/models", nil))
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	rows, _ := out["models"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	live := 0
	for _, raw := range rows {
		m, _ := raw.(map[string]any)
		if m["active"] == true {
			live++
			if m["key_fingerprint"] != keyFingerprint("sk-second") {
				t.Fatalf("wrong account marked live: %v", m)
			}
		}
	}
	if live != 1 {
		t.Fatalf("live rows = %d, want exactly the one in use", live)
	}
}

// A row's endpoint is one address even when the config spells it two ways. The
// console gets its base_url from the renderer, which reports the form the
// client will dial, so a stored "https://x/v1/" and an addressed
// "https://x/v1" are the same row — strict equality answered "no such model"
// and left the operator with a row the page could not delete.
func TestSameEntryTreatsOneEndpointSpelledTwoWaysAsOneRow(t *testing.T) {
	a := config.ModelConfig{Name: "x", BaseURL: "https://api.example/v1/", Model: "m", APIKey: "k"}
	b := config.ModelConfig{Name: "x", BaseURL: "https://api.example/v1", Model: "m", APIKey: "k"}
	if !sameEntry(a, b) {
		t.Fatal("an endpoint with and without a trailing slash is one row")
	}
	// The credential is what keeps two accounts apart, and it must still be
	// part of the identity — tolerating the endpoint must not merge them.
	c := b
	c.APIKey = "other"
	if sameEntry(a, c) {
		t.Fatal("two accounts that share the endpoint are still two rows")
	}
}

func TestRemoveModelAddressesAnEndpointSpelledWithATrailingSlash(t *testing.T) {
	stale := config.ModelConfig{
		Name: "stale", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://stale.example/v1/", Model: "m", APIKey: "k1",
	}
	spare := config.ModelConfig{
		Name: "spare", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://spare.example/v1", Model: "m", APIKey: "k2",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = stale
		c.Models = []config.ModelConfig{spare}
	})

	code, out := doJSON(t, h, authedReq(http.MethodDelete,
		entryURL("stale", "m", "https://stale.example/v1"), nil))
	if code != http.StatusOK {
		t.Fatalf("delete a live row whose endpoint carries a trailing slash = %d %v", code, out)
	}
	if out["switched_to"] != "spare" {
		t.Fatalf("switched_to = %v, want the promoted spare", out["switched_to"])
	}
	cfg := readConfig(t, path)
	if cfg.Model.Alias() != "spare" {
		t.Fatalf("live model = %q, want spare", cfg.Model.Alias())
	}
}

// Clearing a connection is one request and one config write. The page paints a
// card per account, and "this account is wrong" is a fact about all of its rows
// at once: doing it row by row meant a rewrite of the models block and a
// hot-swap of the live model per click, with the slot passing through siblings
// that the next click would delete.
func TestRemoveModelsClearsOneConnectionAndYieldsToTheNext(t *testing.T) {
	sol := config.ModelConfig{
		Name: "sol", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay-a.example/v1", Model: "gpt-5.6-sol", APIKey: "ka",
	}
	luna := config.ModelConfig{
		Name: "luna", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay-a.example/v1", Model: "gpt-5.6-luna", APIKey: "ka",
	}
	spare := config.ModelConfig{
		Name: "spare", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay-b.example/v1", Model: "spare", APIKey: "kb",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = sol
		c.Models = []config.ModelConfig{sol, luna, spare}
	})

	body := `{"refs":[
		{"alias":"sol","model":"gpt-5.6-sol","base_url":"https://relay-a.example/v1"},
		{"alias":"luna","model":"gpt-5.6-luna","base_url":"https://relay-a.example/v1"}]}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/remove", body))
	if code != http.StatusOK {
		t.Fatalf("clear the connection = %d %v", code, out)
	}
	if out["removed"] != 2.0 {
		t.Fatalf("removed = %v, want both rows of the account", out["removed"])
	}
	if out["switched_to"] != "spare" {
		t.Fatalf("switched_to = %v, want the surviving account", out["switched_to"])
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].Alias() != "spare" {
		t.Fatalf("registry after the clear = %+v, want only spare", cfg.Models)
	}
	if cfg.Model.Alias() != "spare" {
		t.Fatalf("live model = %q, want the promoted spare", cfg.Model.Alias())
	}
	// The other account is untouched on every field, not just its name: a purge
	// that rewrote rows it was not asked about would have taken its key with it.
	if cfg.Models[0].APIKey != "kb" || cfg.Models[0].BaseURL != spare.BaseURL {
		t.Fatalf("the survivor was rewritten: %+v", cfg.Models[0])
	}
}

// A stale address is what a tab left open through another surface's edit sends.
// Skipping it keeps the call useful; reporting the count that actually went is
// what keeps the toast honest.
func TestRemoveModelsSkipsAddressesThatNoLongerExist(t *testing.T) {
	keep := config.ModelConfig{
		Name: "keep", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "keep", APIKey: "k",
	}
	gone := config.ModelConfig{
		Name: "gone", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "gone", APIKey: "k",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = keep
		c.Models = []config.ModelConfig{keep, gone}
	})

	body := `{"refs":[
		{"alias":"gone","model":"gone","base_url":"https://relay.example/v1"},
		{"alias":"already-deleted","model":"ghost","base_url":"https://relay.example/v1"}]}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/remove", body))
	if code != http.StatusOK {
		t.Fatalf("clear with a stale ref = %d %v", code, out)
	}
	if out["removed"] != 1.0 {
		t.Fatalf("removed = %v, want exactly the row that existed", out["removed"])
	}
	if cfg := readConfig(t, path); len(cfg.Models) != 1 || cfg.Models[0].Alias() != "keep" {
		t.Fatalf("registry = %+v, want keep untouched", cfg.Models)
	}
	// Nothing matched at all is a different answer from nothing left to do.
	code, _ = doJSON(t, h, jsonReq(http.MethodPost, "/api/models/remove",
		`{"refs":[{"alias":"already-deleted","model":"ghost"}]}`))
	if code != http.StatusNotFound {
		t.Fatalf("a batch that matches nothing = %d, want 404", code)
	}
}

// Clearing every card is allowed and says so: the node is left with no model,
// which is a supported state (asks degrade with ErrNoModel) and the only
// honest end for a config whose models were all wrong.
func TestRemoveModelsClearsTheLastConnection(t *testing.T) {
	only := config.ModelConfig{
		Name: "only", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://only.example/v1", Model: "m", APIKey: "k",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = only
		c.Models = []config.ModelConfig{only}
	})

	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/remove",
		`{"refs":[{"alias":"only","model":"m","base_url":"https://only.example/v1"}]}`))
	if code != http.StatusOK {
		t.Fatalf("clear the only connection = %d %v", code, out)
	}
	if out["cleared"] != true || out["removed"] != 1.0 {
		t.Fatalf("response = %v, want removed:1 and cleared:true", out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 0 {
		t.Fatalf("registry after the clear = %+v", cfg.Models)
	}
	if cfg.Model.Model != "" || cfg.Model.Provider != "" || cfg.Model.BaseURL != "" {
		t.Fatalf("model: still names a purged row: %+v", cfg.Model)
	}
}

// The survivor is verified before anything is removed. Draining the account
// first and then discovering the only remaining row cannot be dialed would
// leave the node failing every ask with no way back but the file.
func TestRemoveModelsRefusesWhenTheSurvivorCannotTakeOver(t *testing.T) {
	live := config.ModelConfig{
		Name: "live", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "live", APIKey: "k",
	}
	second := config.ModelConfig{
		Name: "second", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "second", APIKey: "k",
	}
	// A plain-http endpoint that never gave the waiver: buildable only by
	// breaking the guard, which is exactly what the check is for.
	broken := config.ModelConfig{
		Name: "broken", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "http://relay.example/v1", Model: "broken", APIKey: "k",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = live
		c.Models = []config.ModelConfig{live, second, broken}
	})

	body := `{"refs":[
		{"alias":"live","model":"live","base_url":"https://relay.example/v1"},
		{"alias":"second","model":"second","base_url":"https://relay.example/v1"}]}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/remove", body))
	if code != http.StatusConflict {
		t.Fatalf("clear with an unbuildable survivor = %d %v, want 409", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 3 {
		t.Fatalf("a refused batch still removed rows: %+v", cfg.Models)
	}
	if cfg.Model.Alias() != "live" {
		t.Fatalf("live model = %q, want it left alone", cfg.Model.Alias())
	}
}

// A row imported from another tool carries its provenance to the wire, and a
// hand-written row carries none. The console names a connection after it: an
// imported Claude Code tier must not read as "custom base_url" plus a loopback
// port, while a row the person typed keeps the generic label.
func TestModelEntryCarriesTheImportSource(t *testing.T) {
	imported := modelEntryOf(config.ModelConfig{
		Name:     "claude-opus-5[1M]",
		Provider: "custom",
		Source:   "claude",
		APIType:  "anthropic",
		BaseURL:  "http://127.0.0.1:7896",
		APIKey:   "PROXY_MANAGED",
		Model:    "claude-opus-5[1M]",
	}, config.ModelConfig{})
	if imported.Source != "claude" {
		t.Fatalf("source = %q, want claude", imported.Source)
	}

	handWritten := modelEntryOf(config.ModelConfig{
		Name:     "relay",
		Provider: "custom",
		APIType:  "openai",
		BaseURL:  "http://127.0.0.1:3065/v1",
		APIKey:   "sk-local",
		Model:    "m-1",
	}, config.ModelConfig{})
	if handWritten.Source != "" {
		t.Fatalf("hand-written row got source %q", handWritten.Source)
	}
}

// TestRemoveModelRetryDoesNotFallBackToTheSiblingAccount: a delete retried
// after its response was lost carries a key digest that no longer matches
// anything — the row it named is gone. The alias alone must not pick the
// surviving account of the same vendor; the second click used to remove the
// sibling the request was never pointed at.
func TestRemoveModelRetryDoesNotFallBackToTheSiblingAccount(t *testing.T) {
	accountA := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-a", APIKey: "ka",
	}
	accountB := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-b", APIKey: "kb",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = accountA
		c.Models = []config.ModelConfig{accountA, accountB}
	})

	q := url.Values{}
	q.Set("model", "m-b")
	q.Set("base_url", "https://relay.example/v1")
	q.Set("key", keyFingerprint("kb"))
	target := "/api/models/shared?" + q.Encode()

	code, out := doJSON(t, h, authedReq(http.MethodDelete, target, nil))
	if code != http.StatusOK {
		t.Fatalf("first delete = %d %v", code, out)
	}
	// The retry is the same request: B is already gone, so its digest matches
	// nothing. A is the only row left under the alias and must survive.
	code, out = doJSON(t, h, authedReq(http.MethodDelete, target, nil))
	if code != http.StatusNotFound {
		t.Fatalf("retry with a stale digest = %d %v, want 404", code, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].APIKey != "ka" {
		t.Fatalf("registry after the retry = %+v, want the other account untouched", cfg.Models)
	}
}

// TestAddModelBorrowsTheKeyOfTheAccountItNames: two rows of one vendor share
// the alias, so "extend this connection" has to say which account it means.
// An alias-only lookup handed the new row the first row's credential — the
// other account's — and the replacement then failed to authenticate.
func TestAddModelBorrowsTheKeyOfTheAccountItNames(t *testing.T) {
	accountA := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-a", APIKey: "ka",
	}
	accountB := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-b", APIKey: "kb",
		KeyScheme: "bearer",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = accountA
		c.Models = []config.ModelConfig{accountA, accountB}
	})

	body := `{"provider":"custom","base_url":"https://relay.example/v1","api_type":"openai",` +
		`"model":"m-new","alias":"shared","reuse_from":"shared","reuse_from_key":"` + keyFingerprint("kb") + `"}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models", body))
	if code != http.StatusOK {
		t.Fatalf("add borrowing B's key = %d %v", code, out)
	}
	var added config.ModelConfig
	for _, m := range readConfig(t, path).Models {
		if m.Model == "m-new" {
			added = m
		}
	}
	if added.APIKey != "kb" {
		t.Fatalf("new row key = %q, want B's key", added.APIKey)
	}
	if added.KeyScheme != "bearer" {
		t.Fatalf("new row key_scheme = %q, want B's bearer", added.KeyScheme)
	}

	// A digest that names no row must refuse rather than fall through to
	// "whatever key this provider has" — that is how A's credential was
	// silently bound to a replacement for B.
	body = `{"provider":"custom","base_url":"https://relay.example/v1","api_type":"openai",` +
		`"model":"m-lost","alias":"shared","reuse_from":"shared","reuse_from_key":"deadbeef"}`
	code, out = doJSON(t, h, jsonReq(http.MethodPost, "/api/models", body))
	if code != http.StatusBadRequest {
		t.Fatalf("add naming a vanished account = %d %v, want 400", code, out)
	}
	if n := len(readConfig(t, path).Models); n != 3 {
		t.Fatalf("a refused add wrote a row: %d rows", n)
	}
}

// TestKeyReferenceFingerprintsAddressTheirRows: the list signs whatever the
// row's credential is — a literal key, a CC Switch reference, or an env var
// name — but the addressing paths digested APIKey alone. Every valid
// fingerprint from a key_ref/key_env row therefore matched nothing: a PUT
// answered 404 and a borrow answered 400.
func TestKeyReferenceFingerprintsAddressTheirRows(t *testing.T) {
	ref := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-ref", KeyRef: "ccswitch:claude:p-1",
	}
	env := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-env", KeyEnv: "OPENPANDA_TEST_KEY",
	}
	t.Setenv("OPENPANDA_TEST_KEY", "sk-from-env")
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = ref
		c.Models = []config.ModelConfig{ref, env}
	})

	// A PUT addressed by the reference's own digest rewrites that row.
	q := url.Values{}
	q.Set("model", "m-ref")
	q.Set("base_url", "https://relay.example/v1")
	q.Set("key", keyFingerprint(fingerprintSource(ref)))
	body := `{"provider":"custom","base_url":"https://relay.example/v1","api_type":"openai",` +
		`"model":"m-ref","alias":"shared","context_window":64000}`
	code, out := doJSON(t, h, jsonReq(http.MethodPut, "/api/models/shared?"+q.Encode(), body))
	if code != http.StatusOK {
		t.Fatalf("update a key_ref row by its fingerprint = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	var gotRef config.ModelConfig
	for _, m := range cfg.Models {
		if m.Model == "m-ref" {
			gotRef = m
		}
	}
	if gotRef.KeyRef != ref.KeyRef {
		t.Fatalf("key_ref after update = %q, want %q", gotRef.KeyRef, ref.KeyRef)
	}
	if gotRef.ContextWindow != 64000 {
		t.Fatalf("the update did not land: context_window = %d", gotRef.ContextWindow)
	}

	// The same address works for a key_env row.
	q.Set("model", "m-env")
	q.Set("key", keyFingerprint(fingerprintSource(env)))
	body = `{"provider":"custom","base_url":"https://relay.example/v1","api_type":"openai",` +
		`"model":"m-env","alias":"shared","context_window":32000}`
	code, out = doJSON(t, h, jsonReq(http.MethodPut, "/api/models/shared?"+q.Encode(), body))
	if code != http.StatusOK {
		t.Fatalf("update a key_env row by its fingerprint = %d %v", code, out)
	}
	cfg = readConfig(t, path)
	var gotEnv config.ModelConfig
	for _, m := range cfg.Models {
		if m.Model == "m-env" {
			gotEnv = m
		}
	}
	if gotEnv.KeyEnv != env.KeyEnv {
		t.Fatalf("key_env after update = %q, want %q", gotEnv.KeyEnv, env.KeyEnv)
	}

	// The same digest addresses a delete: the row carrying the reference goes,
	// its sibling stays.
	code, out = doJSON(t, h, authedReq(http.MethodDelete, "/api/models/shared?"+q.Encode(), nil))
	if code != http.StatusOK {
		t.Fatalf("delete a key_env row by its fingerprint = %d %v", code, out)
	}
	cfg = readConfig(t, path)
	if len(cfg.Models) != 1 || cfg.Models[0].Model != "m-ref" {
		t.Fatalf("registry after the delete = %+v, want only the key_ref row", cfg.Models)
	}
}

// TestAddModelBorrowsAReferenceCredential: "extend this connection" on a row
// that stores no secret has to carry the reference. The provider-wide lookup
// must not stand in for it — with two accounts under one alias, that is how a
// replacement row was saved with the other account's literal key.
func TestAddModelBorrowsAReferenceCredential(t *testing.T) {
	inline := config.ModelConfig{
		Name: "shared", Provider: "deepseek", APIType: config.APITypeOpenAI,
		BaseURL: "https://api.deepseek.com", Model: "deepseek-chat", APIKey: "sk-a",
	}
	ref := config.ModelConfig{
		Name: "shared", Provider: "deepseek", APIType: config.APITypeOpenAI,
		BaseURL: "https://api.deepseek.com", Model: "deepseek-reasoner",
		KeyRef: "ccswitch:claude:p-1", KeyScheme: "bearer",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = inline
		c.Models = []config.ModelConfig{inline, ref}
	})

	body := `{"provider":"deepseek","model":"deepseek-chat","alias":"shared",` +
		`"reuse_from":"shared","reuse_from_key":"` + keyFingerprint(fingerprintSource(ref)) + `"}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models", body))
	if code != http.StatusOK {
		t.Fatalf("add borrowing a key_ref credential = %d %v", code, out)
	}
	var added config.ModelConfig
	for _, m := range readConfig(t, path).Models {
		if m.Model == "deepseek-chat" && m.Name != inline.Name {
			added = m
		}
	}
	if added.KeyRef != ref.KeyRef {
		t.Fatalf("new row key_ref = %q, want %q", added.KeyRef, ref.KeyRef)
	}
	if added.APIKey != "" {
		t.Fatalf("new row borrowed the other account's literal key %q", added.APIKey)
	}
	if added.KeyScheme != "bearer" {
		t.Fatalf("new row key_scheme = %q, want the lender's bearer", added.KeyScheme)
	}
}

// TestCopyModelCarriesAReferenceCredential: a copy is the same rule — the
// panel never sees the secret, so the reference is what travels.
func TestCopyModelCarriesAReferenceCredential(t *testing.T) {
	src := config.ModelConfig{
		Name: "ref", Provider: "custom", APIType: config.APITypeOpenAI,
		BaseURL: "https://relay.example/v1", Model: "m-1", KeyRef: "ccswitch:claude:p-1",
		KeyScheme: "bearer",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = src
		c.Models = []config.ModelConfig{src}
	})

	body := `{"provider":"custom","base_url":"https://relay.example/v1","api_type":"openai",` +
		`"model":"m-1","alias":"ref-copy","copy_from":"ref","copy_from_key":"` +
		keyFingerprint(fingerprintSource(src)) + `"}`
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models", body))
	if code != http.StatusOK {
		t.Fatalf("copy a key_ref row = %d %v", code, out)
	}
	var copied config.ModelConfig
	for _, m := range readConfig(t, path).Models {
		if m.Alias() == "ref-copy" {
			copied = m
		}
	}
	if copied.KeyRef != src.KeyRef || copied.APIKey != "" {
		t.Fatalf("copy credential = ref %q key %q, want ref %q and no literal key", copied.KeyRef, copied.APIKey, src.KeyRef)
	}
	if copied.KeyScheme != "bearer" {
		t.Fatalf("copy key_scheme = %q, want the source's bearer", copied.KeyScheme)
	}
}

// TestUseModelWritesTheCredentialForm: switching the live account rewrites the
// whole credential, not just api_key. Writing only the literal key left A's
// key_env in `model:` after switching to B — the interface said B, the file
// still named A — and dropped B's key_scheme, so a Bearer token came back on
// the dialect's default header.
func TestUseModelWritesTheCredentialForm(t *testing.T) {
	t.Setenv("A_KEY", "sk-a")
	a := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeAnthropic,
		BaseURL: "https://relay.example/v1", Model: "m-a", KeyEnv: "A_KEY",
	}
	b := config.ModelConfig{
		Name: "shared", Provider: "custom", APIType: config.APITypeAnthropic,
		BaseURL: "https://relay.example/v1", Model: "m-b",
		KeyRef: "ccswitch:claude:p-1", KeyScheme: "bearer",
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = a
		c.Models = []config.ModelConfig{a, b}
	})

	q := url.Values{}
	q.Set("model", "m-b")
	q.Set("base_url", "https://relay.example/v1")
	q.Set("key", keyFingerprint(fingerprintSource(b)))
	code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/models/shared/use?"+q.Encode(), ""))
	if code != http.StatusOK {
		t.Fatalf("switch to the key_ref account = %d %v", code, out)
	}
	cfg := readConfig(t, path)
	if cfg.Model.KeyEnv != "" {
		t.Fatalf("model.key_env = %q, want it cleared", cfg.Model.KeyEnv)
	}
	if cfg.Model.KeyRef != b.KeyRef {
		t.Fatalf("model.key_ref = %q, want %q", cfg.Model.KeyRef, b.KeyRef)
	}
	if cfg.Model.KeyScheme != "bearer" {
		t.Fatalf("model.key_scheme = %q, want bearer", cfg.Model.KeyScheme)
	}
}

func TestCCSwitchRegistrationMatchesProtocol(t *testing.T) {
	const base = "https://relay.invalid/v1"
	const model = "shared-model"
	dbPath := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, is_current BOOLEAN, sort_index INTEGER)"); err != nil {
		t.Fatal(err)
	}
	documents := map[string]map[string]any{
		"claude": {"env": map[string]string{"ANTHROPIC_BASE_URL": base, "ANTHROPIC_MODEL": model, "ANTHROPIC_API_KEY": "fixture"}},
		"codex": {
			"auth":   map[string]string{"OPENAI_API_KEY": "fixture"},
			"config": "model_provider = \"fixture\"\nmodel = \"shared-model\"\n[model_providers.fixture]\nbase_url = \"https://relay.invalid/v1\"\nwire_api = \"chat\"\n",
		},
	}
	for app, doc := range documents {
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO providers VALUES (?,?,?,?,1,1)", app, app, app, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	// A second Codex entry uses the same URL and model via Responses.
	responses := documents["codex"]
	responses["config"] = strings.Replace(responses["config"].(string), "\"chat\"", "\"responses\"", 1)
	raw, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO providers VALUES (?,?,?,?,0,2)", "codex-responses", "codex", "Responses", string(raw)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CC_SWITCH_DB", dbPath)
	h, _ := newModelPanel(t, func(c *config.Config) {
		c.Models = []config.ModelConfig{{BaseURL: base, Model: model, APIType: "openai"}}
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq(http.MethodGet, "/api/models/ccswitch", nil))
	var payload struct {
		Groups []localmodels.Source `json:"groups"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &payload) != nil {
		t.Fatalf("discover CC Switch: HTTP %d, %s", rr.Code, rr.Body.String())
	}
	rows := make(map[string]localmodels.Candidate)
	for _, group := range payload.Groups {
		for _, row := range group.Candidates {
			rows[row.APIType] = row
		}
	}
	if !rows["openai"].Existing || !rows["anthropic"].Importable || rows["anthropic"].Existing ||
		!rows["responses"].Importable || rows["responses"].Existing {
		t.Fatalf("expected only the registered OpenAI row to be marked existing: %+v", rows)
	}
	if rows["openai"].ID == rows["responses"].ID || rows["openai"].Group == rows["responses"].Group {
		t.Fatal("CC Switch merged the two Codex protocols into one selectable row or group")
	}
}

func TestCCSwitchDiscoveredCandidatesImportToDisk(t *testing.T) {
	isolateUserDirs(t)
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("PATH", home)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("UNSLOTH_STUDIO_AUTH_TOKEN", "")
	const base = "https://relay.invalid/v1"
	const model = "shared-model"
	dbPath := filepath.Join(home, "cc-switch.db")
	t.Setenv("CC_SWITCH_DB", dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, is_current BOOLEAN, sort_index INTEGER)"); err != nil {
		t.Fatal(err)
	}
	claude := `{"env":{"ANTHROPIC_BASE_URL":"https://relay.invalid/v1","ANTHROPIC_MODEL":"shared-model[1M]","ANTHROPIC_AUTH_TOKEN":"synthetic-claude"}}`
	codex := `{"auth":{"OPENAI_API_KEY":"synthetic-codex"},"config":"model_provider = \"fixture\"\nmodel = \"shared-model\"\n[model_providers.fixture]\nbase_url = \"https://relay.invalid/v1\"\nwire_api = \"responses\"\n"}`
	gemini := `{"env":{"GOOGLE_GEMINI_BASE_URL":"https://relay.invalid/v1","GEMINI_MODEL":"models/shared-model","GEMINI_API_KEY":"synthetic-gemini"}}`
	for app, settings := range map[string]string{"claude": claude, "codex": codex, "gemini": gemini} {
		if _, err := db.Exec("INSERT INTO providers VALUES (?,?,?,?,1,1)", app, app, app, settings); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("ALTER TABLE providers ADD COLUMN meta TEXT"); err != nil {
		t.Fatal(err)
	}
	desktop := `{"env":{"ANTHROPIC_BASE_URL":"https://relay.invalid/desktop","ANTHROPIC_MODEL":"shared-model","ANTHROPIC_API_KEY":"synthetic-desktop"}}`
	if _, err := db.Exec("INSERT INTO providers VALUES (?,?,?,?,1,1,?)", "desktop", "claude-desktop", "Desktop", desktop,
		`{"apiFormat":"openai_chat"}`); err != nil {
		t.Fatal(err)
	}
	h, path := newModelPanel(t, func(c *config.Config) {
		c.Model = config.ModelConfig{BaseURL: base, APIType: "openai", Model: model}
		c.Models = []config.ModelConfig{c.Model}
	})
	list := func() []localmodels.Candidate {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq(http.MethodGet, "/api/models/ccswitch", nil))
		var payload struct{ Groups []localmodels.Source }
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &payload) != nil {
			t.Fatalf("discover CC Switch: HTTP %d, %s", rr.Code, rr.Body.String())
		}
		var candidates []localmodels.Candidate
		for _, group := range payload.Groups {
			candidates = append(candidates, group.Candidates...)
		}
		return candidates
	}
	var items []modelImportItem
	for _, c := range list() {
		if c.Existing || !c.Importable || c.Model != model || c.KeyRef == "" {
			t.Fatalf("expected separate selectable row: %+v", c)
		}
		items = append(items, modelImportItem{CandidateID: c.ID, Provider: c.Provider, Model: c.Model, BaseURL: c.BaseURL,
			APIType: c.APIType, KeyRef: c.KeyRef, NoAuth: c.NoAuth, Auth: c.Auth, ContextWindow: c.ContextWindow})
	}
	if len(items) != 4 {
		t.Fatalf("discovered %d candidates, want 4", len(items))
	}
	submitItems := func(selected []modelImportItem) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"items": selected})
		if err != nil {
			t.Fatal(err)
		}
		return doJSON(t, h, jsonReq(http.MethodPost, "/api/models/import", string(raw)))
	}
	importItems := func(selected []modelImportItem) map[string]any {
		t.Helper()
		code, result := submitItems(selected)
		if code != http.StatusOK {
			t.Fatalf("import: HTTP %d %v", code, result)
		}
		return result
	}
	validConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("models: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := submitItems(items); code != http.StatusInternalServerError {
		t.Fatalf("malformed config should prevent import, got HTTP %d", code)
	}
	for _, candidate := range list() {
		if candidate.Existing {
			t.Fatalf("failed import changed the registry in memory: %s", candidate.APIType)
		}
	}
	if err := os.WriteFile(path, validConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := importItems(items); result["added"] != float64(4) {
		t.Fatalf("expected four imported model rows, got %v", result)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]config.ModelConfig)
	for _, row := range cfg.Models {
		rows[row.APIType] = row
	}
	if len(cfg.Models) != 5 || rows["anthropic"].KeyRef != "ccswitch:claude:claude" ||
		rows["anthropic"].KeyScheme != "bearer" || rows["anthropic"].ContextWindow != 1_000_000 ||
		rows["responses"].KeyRef != "ccswitch:codex:codex" || rows["google"].KeyRef != "ccswitch:gemini:gemini" ||
		rows["openai"].KeyRef != "ccswitch:claude-desktop:desktop" || rows["openai"].ConnName != "Desktop" {
		t.Fatalf("persisted protocols and credential metadata differ: api types found=%d", len(rows))
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "synthetic-") {
		t.Fatal("import wrote a credential value instead of its reference")
	}
	for _, c := range list() {
		if !c.Existing {
			t.Fatalf("imported protocol %s is still selectable", c.APIType)
		}
	}

	// A row selected before CC Switch changes its endpoint is stale. A later
	// import must still match the provider currently stored in CC Switch.
	moved := strings.ReplaceAll(claude, "relay.invalid", "other.invalid")
	if _, err := db.Exec("UPDATE providers SET settings_config=? WHERE app_type='claude'", moved); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.APIType == "anthropic" {
			result := importItems([]modelImportItem{item})
			skipped, _ := result["skipped"].([]any)
			if result["added"] != float64(0) || len(skipped) != 1 ||
				!strings.Contains(skipped[0].(string), "no longer matches") {
				t.Fatalf("stale CC Switch candidate was not rejected: %v", result)
			}
		}
	}
}
