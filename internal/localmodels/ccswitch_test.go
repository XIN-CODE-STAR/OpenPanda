// SPDX-License-Identifier: AGPL-3.0-or-later

package localmodels

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/ccswitch"
	_ "modernc.org/sqlite"
)

// seedCCSwitch writes a minimal store shaped like CC Switch's: one row per
// provider, with the Claude settings body under settings_config.
func seedCCSwitch(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL,
		app_type TEXT NOT NULL,
		name TEXT NOT NULL,
		settings_config TEXT NOT NULL,
		is_current BOOLEAN NOT NULL DEFAULT 0,
		sort_index INTEGER)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	insert := func(id, app, name, base, token string, withModel, current bool, sortIndex int) {
		cfg := fmt.Sprintf(`{"env":{"ANTHROPIC_BASE_URL":%q,"ANTHROPIC_AUTH_TOKEN":%q}}`, base, token)
		if withModel {
			cfg = fmt.Sprintf(
				`{"env":{"ANTHROPIC_BASE_URL":%q,"ANTHROPIC_AUTH_TOKEN":%q,"ANTHROPIC_MODEL":"glm-5.3-flash","ANTHROPIC_DEFAULT_OPUS_MODEL":"glm-5.3-flashx"}}`,
				base, token)
		}
		if _, err := db.Exec(
			`INSERT INTO providers (id, app_type, name, settings_config, is_current, sort_index) VALUES (?,?,?,?,?,?)`,
			id, app, name, cfg, current, sortIndex); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// The same relay saved twice: the second one is the live entry, and its
	// trailing slash must not split the group.
	insert("id-a", "claude", "Zhipu GLM", "https://open.bigmodel.cn/api/anthropic", "tok-a", true, false, 1)
	insert("id-b", "claude", "Zhipu 备用", "https://open.bigmodel.cn/api/anthropic/", "tok-b", true, true, 2)
	// A second relay, alphabetically first but listed after the Zhipu pair:
	// the sheet keeps CC Switch's own order instead of sorting endpoints.
	insert("id-d", "claude", "Alpha Relay", "https://aaa.example/v1", "tok-d", true, false, 4)
	// A Claude Desktop row with no model: nothing to import.
	insert("id-c", "claude-desktop", "Desktop only", "https://api.xiaomimimo.com/anthropic", "tok-c", false, false, 3)
	// The same model pinned twice, once with Claude Code's 1M marker: the
	// marker is split off the id and becomes the row's window.
	oneM := `{"env":{"ANTHROPIC_BASE_URL":"https://onem.example/v1","ANTHROPIC_AUTH_TOKEN":"tok-e","ANTHROPIC_MODEL":"claude-opus-5[1M]","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-5"}}`
	if _, err := db.Exec(
		`INSERT INTO providers (id, app_type, name, settings_config, is_current, sort_index) VALUES (?,?,?,?,?,?)`,
		"id-e", "claude", "OneM", oneM, false, 5); err != nil {
		t.Fatalf("insert id-e: %v", err)
	}
	return path
}

func TestCCSwitchGroupsMergesByEndpoint(t *testing.T) {
	path := seedCCSwitch(t)
	groups := CCSwitchGroups(context.Background(), Options{
		Getenv: func(key string) string {
			if key == "CC_SWITCH_DB" {
				return path
			}
			return ""
		},
	})
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want one per app type", len(groups))
	}
	src := groups[0]
	if src.ID != "ccswitch:claude" {
		t.Fatalf("first group = %q, want the Claude Code roster", src.ID)
	}
	if src.Status != StatusOK {
		t.Fatalf("status = %s (%s), want ok", src.Status, src.Detail)
	}
	// Two models per endpoint, one more on the 1M relay; the model-less
	// Desktop row is skipped.
	if len(src.Candidates) != 5 {
		t.Fatalf("candidates = %d, want 5: %+v", len(src.Candidates), src.Candidates)
	}
	// The console draws one heading per endpoint, in the order CC Switch shows
	// them — not alphabetised by URL.
	wantGroups := []struct{ key, label string }{
		{"https://open.bigmodel.cn/api/anthropic|anthropic", "Zhipu 备用"},
		{"https://aaa.example/v1|anthropic", "Alpha Relay"},
		{"https://onem.example/v1|anthropic", "OneM"},
	}
	gotGroups := make([]struct{ key, label string }, 0, 3)
	for _, c := range src.Candidates {
		if n := len(gotGroups); n == 0 || gotGroups[n-1].key != c.Group {
			gotGroups = append(gotGroups, struct{ key, label string }{c.Group, c.GroupLabel})
		}
	}
	if len(gotGroups) != len(wantGroups) {
		t.Fatalf("groups = %+v, want %+v", gotGroups, wantGroups)
	}
	for i, want := range wantGroups {
		if gotGroups[i] != want {
			t.Fatalf("group %d = %+v, want %+v", i, gotGroups[i], want)
		}
	}
	for _, c := range src.Candidates {
		if c.Provider != "custom" {
			t.Errorf("%s provider = %q, want custom (the anthropic endpoint is not the catalogue's OpenAI one)", c.Model, c.Provider)
		}
		if c.APIType != "anthropic" {
			t.Errorf("%s api_type = %q, want anthropic", c.Model, c.APIType)
		}
		if c.Group != strings.ToLower(strings.TrimRight(c.BaseURL, "/"))+"|"+c.APIType {
			t.Errorf("%s group = %q, want its endpoint", c.Model, c.Group)
		}
		if strings.Contains(c.GroupLabel, "+1") {
			t.Errorf("%s group label = %q, want the provider name without the merge count", c.Model, c.GroupLabel)
		}
		if c.BaseURL != "https://open.bigmodel.cn/api/anthropic" {
			continue
		}
		// The live entry's id, not the first row's: its token is the one that
		// works right now.
		if c.KeyRef != "ccswitch:claude:id-b" {
			t.Errorf("%s key_ref = %q, want the current row's reference", c.Model, c.KeyRef)
		}
		if !c.Bearer {
			t.Errorf("%s should keep the Bearer scheme (AUTH_TOKEN)", c.Model)
		}
		if !c.Suggested {
			t.Errorf("%s should be suggested: its provider is the live one", c.Model)
		}
		if !strings.Contains(c.Note, "Zhipu 备用 +1") {
			t.Errorf("%s note = %q, want the live name and the merge count", c.Model, c.Note)
		}
	}
	// The marker is Claude Code's own syntax, not part of the id a relay
	// serves: it is split off, the two entries collapse into one row, and the
	// window it asked for lands on that row.
	var oneMRows int
	for _, c := range src.Candidates {
		if c.BaseURL != "https://onem.example/v1" {
			continue
		}
		oneMRows++
		if c.Model != "claude-opus-5" {
			t.Errorf("model = %q, want the id without the marker", c.Model)
		}
		if c.ContextWindow != 1_000_000 {
			t.Errorf("context_window = %d, want 1000000", c.ContextWindow)
		}
	}
	if oneMRows != 1 {
		t.Fatalf("the 1M relay produced %d candidates, want the two entries merged into 1", oneMRows)
	}
	// The console keeps one tick set keyed by ID across all four rosters, so an
	// empty or repeated ID ticks every row at once.
	seen := map[string]bool{}
	for _, g := range groups {
		for _, c := range g.Candidates {
			if c.ID == "" {
				t.Fatalf("%s has no id: the console cannot tell the rows apart", c.Model)
			}
			if seen[c.ID] {
				t.Fatalf("%s reuses id %q", c.Model, c.ID)
			}
			seen[c.ID] = true
		}
	}
}

func TestCCSwitchGroupsMissingStore(t *testing.T) {
	groups := CCSwitchGroups(context.Background(), Options{
		Getenv: func(key string) string {
			if key == "CC_SWITCH_DB" {
				return filepath.Join(t.TempDir(), "nope.db")
			}
			return ""
		},
	})
	if len(groups) != 1 || groups[0].Status != StatusMissing {
		t.Fatalf("groups = %+v, want a single missing source", groups)
	}
}

func TestCCSwitchMergedModelsKeepTheirOwnCredentials(t *testing.T) {
	rows, _ := ccSwitchCandidates([]ccswitch.Provider{
		{ID: "active", AppType: "claude", Name: "Active", Current: true, HasToken: true,
			BaseURL: "https://relay.invalid/Tenant", APIType: "anthropic", Models: []string{"shared", "active-only"}},
		{ID: "other", AppType: "claude", Name: "Other", HasToken: true, Bearer: true,
			BaseURL: "https://RELAY.invalid/Tenant", APIType: "anthropic", Models: []string{"shared[1M]", "other-only"}},
		{ID: "case", AppType: "claude", HasToken: true,
			BaseURL: "https://relay.invalid/tenant", APIType: "anthropic", Models: []string{"shared"}},
	})
	if len(rows) != 4 || rows[0].KeyRef != "ccswitch:claude:active" || rows[0].ContextWindow != 1_000_000 ||
		rows[2].KeyRef != "ccswitch:claude:other" || !rows[2].Bearer || rows[2].Suggested ||
		rows[0].Group != rows[2].Group || rows[0].Group == rows[3].Group {
		t.Fatalf("credential or endpoint pairing changed: %+v", rows)
	}
}

// A cc-switch row stores the reference, not the secret: config.yaml must not
// gain a copy of a token CC Switch already owns, and the row must not read as
// "no credential" just because APIKey is empty.
func TestRowKeepsCCSwitchReference(t *testing.T) {
	c := Candidate{
		Source:     SourceCCSwitch,
		Provider:   "custom",
		Model:      "glm-5.3-flash",
		BaseURL:    "https://open.bigmodel.cn/api/anthropic",
		APIType:    "anthropic",
		KeyRef:     "ccswitch:claude:p-1",
		Bearer:     true,
		Importable: true,
	}
	mc, err := c.Row(Options{})
	if err != nil {
		t.Fatalf("Row: %v", err)
	}
	if mc.KeyRef != "ccswitch:claude:p-1" {
		t.Fatalf("key_ref = %q, want the reference preserved", mc.KeyRef)
	}
	if mc.APIKey != "" || mc.KeyEnv != "" {
		t.Fatalf("row copied a credential: api_key=%q key_env=%q", mc.APIKey, mc.KeyEnv)
	}
	if mc.NoAuth {
		t.Fatalf("row with a credential reference must not be no_auth")
	}
	if mc.KeyScheme != "bearer" {
		t.Fatalf("key_scheme = %q, want bearer", mc.KeyScheme)
	}
}
