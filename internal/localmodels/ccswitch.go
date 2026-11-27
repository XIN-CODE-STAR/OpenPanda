// SPDX-License-Identifier: AGPL-3.0-or-later

package localmodels

// CC Switch is its own entry point, not a source inside the discover sheet:
// the store holds four independent app rosters (Claude Code, Claude Desktop,
// Codex, Gemini), and mixing twenty rows from all of them into one list buries
// the two the person actually wants. The console asks for the groups and shows
// one at a time.
//
// The store is somebody else's database, so the reader is deliberately narrow:
// read-only, one candidate per endpoint rather than one per CC Switch entry
// (the same relay is often saved several times under different names), and a
// credential stored as a reference ("ccswitch:<app>:<id>") so CC Switch keeps
// owning the secret.

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Xustalis/OpenPanda/internal/ccswitch"
)

// ccSwitchAppLabels name the groups the console shows as tabs. They match the
// client each roster belongs to, not the store's raw app_type values.
var ccSwitchAppLabels = map[string]string{
	ccswitch.AppClaude:        "Claude Code",
	ccswitch.AppClaudeDesktop: "Claude Desktop",
	ccswitch.AppCodex:         "Codex",
	ccswitch.AppGemini:        "Gemini",
}

// ccSwitchAppOrder fixes the tab order: the two Claude rosters first (they are
// what the store is usually for), then Codex, then Gemini.
var ccSwitchAppOrder = []string{
	ccswitch.AppClaude,
	ccswitch.AppClaudeDesktop,
	ccswitch.AppCodex,
	ccswitch.AppGemini,
}

// CCSwitchGroups reads the store and returns one Source per app type. A group
// with nothing importable comes back as an empty source rather than being
// dropped: the console shows the tab greyed out, which answers "why is Codex
// not here" without a second request.
func CCSwitchGroups(ctx context.Context, opt Options) []Source {
	opt = opt.withDefaults()
	path := ccswitch.StorePath(opt.Home, opt.env("CC_SWITCH_DB"))
	if path == "" {
		return []Source{{ID: SourceCCSwitch, Status: StatusMissing, Detail: "no home directory to look for cc-switch.db in", Candidates: []Candidate{}}}
	}
	providers, err := ccswitch.ReadContext(ctx, path)
	if err != nil {
		return []Source{{ID: SourceCCSwitch, Status: StatusError, Detail: "cc-switch.db: " + firstLine(err.Error()), Candidates: []Candidate{}}}
	}
	if providers == nil {
		return []Source{{ID: SourceCCSwitch, Status: StatusMissing, Detail: path + " not found", Candidates: []Candidate{}}}
	}
	byApp := make(map[string][]ccswitch.Provider, len(ccSwitchAppOrder))
	for _, p := range providers {
		byApp[p.AppType] = append(byApp[p.AppType], p)
	}
	out := make([]Source, 0, len(ccSwitchAppOrder))
	for _, app := range ccSwitchAppOrder {
		out = append(out, ccSwitchGroup(app, byApp[app]))
	}
	return out
}

// ccSwitchGroup turns one app roster into a Source.
func ccSwitchGroup(app string, providers []ccswitch.Provider) Source {
	label := ccSwitchAppLabels[app]
	if label == "" {
		label = app
	}
	src := Source{ID: SourceCCSwitch + ":" + app, Label: label, Status: StatusEmpty, Candidates: []Candidate{}}
	if len(providers) == 0 {
		src.Detail = "not configured in CC Switch"
		return src
	}
	candidates, skipped := ccSwitchCandidates(providers)
	src.Candidates = candidates
	src.Detail = ccswitch.Describe(providers)
	if len(candidates) > 0 {
		src.Status = StatusOK
	}
	if skipped > 0 {
		src.Detail += fmt.Sprintf(" · %d skipped (no model or token)", skipped)
	}
	return src
}

// ccSwitchCandidates merges the rows of one app roster into candidates: one
// group per endpoint and protocol, the union of its models, and the live entry as the
// group's representative. Each model keeps the credential of an entry that lists it.
//
// The group is the second level the console draws: a client roster holds
// several providers, and each provider is a heading with its own models and
// its own tick. Group and GroupLabel carry that split on every row, so the
// console never has to re-derive it from the note text.
func ccSwitchCandidates(providers []ccswitch.Provider) ([]Candidate, int) {
	type group struct {
		rep     ccswitch.Provider
		models  []string
		windows map[string]int
		owners  map[string]ccswitch.Provider
		count   int
	}
	order := make([]string, 0, len(providers))
	groups := make(map[string]*group, len(providers))
	skipped := 0
	for _, p := range providers {
		if p.BaseURL == "" || len(p.Models) == 0 {
			// A Claude Desktop row usually pins no model, and an official row
			// leaves the endpoint to the client: neither can become a registry
			// row on its own.
			skipped++
			continue
		}
		if !p.HasToken {
			// No stored credential means the entry relies on a client login
			// (the Codex-on-ChatGPT rows). OpenPanda cannot reuse that login,
			// so a row would import fine and fail on the first turn.
			skipped++
			continue
		}
		protocol := p.APIType
		if protocol == "" {
			protocol = "anthropic"
		}
		key := EndpointIdentity(p.BaseURL) + "|" + protocol
		g, ok := groups[key]
		if !ok {
			g = &group{
				rep:     p,
				owners:  make(map[string]ccswitch.Provider, len(p.Models)),
				windows: make(map[string]int, len(p.Models)),
			}
			groups[key] = g
			order = append(order, key)
		}
		g.count++
		if p.Current && !g.rep.Current {
			g.rep = p
		}
		for _, m := range p.Models {
			// Claude Code spells a 1M request as `id[1M]`. The marker is not
			// part of the model a relay serves — the same relay lists `id` — and
			// a CC Switch tier can be any gateway's name with the marker
			// appended, so the id is stored without it and the window it asked
			// for becomes the row's context_window. Two entries pinning the same
			// model with and without the marker are one row; the wider window
			// wins.
			id, window := splitWindowMarker(m)
			if !modelID.MatchString(id) {
				continue
			}
			owner, seen := g.owners[id]
			if !seen {
				g.models = append(g.models, id)
			}
			if !seen || (p.Current && !owner.Current) {
				g.owners[id] = p
			}
			g.windows[id] = max(g.windows[id], window)
		}
	}

	// Groups keep the order CC Switch itself shows them in (its sort_index):
	// this sheet is a copy of the person's list, and a relay they just moved to
	// the top should not come back alphabetised.
	out := make([]Candidate, 0, len(order)*2)
	for _, key := range order {
		g := groups[key]
		providerID := "custom"
		if id, ok := providerForBase(g.rep.BaseURL); ok {
			providerID = id
		}
		label := g.rep.Name
		if label == "" {
			// CC Switch lets a provider go unnamed; the host is the next most
			// recognisable thing, and "claude" alone reads like a placeholder.
			if u, err := url.Parse(g.rep.BaseURL); err == nil && u.Host != "" {
				label = u.Host
			} else {
				label = g.rep.AppType
			}
		}
		// The heading is the provider's own name. The merge count stays in the
		// note: "two saved entries, one endpoint" is provenance, and a heading
		// ending in "+1" reads like a quantity of models.
		note := label
		if g.count > 1 {
			note = fmt.Sprintf("%s +%d", label, g.count-1)
		}
		apiType := g.rep.APIType
		if apiType == "" {
			apiType = "anthropic"
		}
		for _, model := range g.models {
			owner := g.owners[model]
			cand := Candidate{
				Source:        SourceCCSwitch,
				Provider:      providerID,
				ProviderLabel: owner.Name,
				Model:         model,
				Label:         model,
				BaseURL:       owner.BaseURL,
				APIType:       apiType,
				KeyRef:        ccswitch.MakeRef(owner.AppType, owner.ID),
				Bearer:        owner.Bearer,
				Suggested:     owner.Current,
				Importable:    true,
				Note:          "cc-switch: " + note,
				Group:         key,
				GroupLabel:    label,
				ContextWindow: g.windows[model],
			}
			// The console keeps one tick set for all four rosters, keyed by
			// ID. The app type is part of it because the same relay is often
			// saved under both Claude Code and Codex: two rows there are two
			// rows to the person, and an ID that ignored the roster would tick
			// both from either tab.
			cand.ID = candidateID(cand) + "|" + owner.AppType
			out = append(out, cand)
		}
	}
	return out, skipped
}
