// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

// The registry's "what can this machine already serve" pair: one read that
// asks the four collectors, one write that turns the ticked candidates into
// rows. Both are thin — the collection lives in internal/localmodels, the row
// shape in config — so this file is only the boundary where a candidate meets
// the live registry: what is already there, what alias is free, and what the
// entry client refuses to dial.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ccswitch"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/entry"
	"github.com/Xustalis/OpenPanda/internal/localmodels"
	"github.com/Xustalis/OpenPanda/internal/security"
)

// discoverTimeout bounds the whole scan. The OpenCode CLI alone can take ten
// seconds on a cold cache, and the console shows a spinner for the duration, so
// the ceiling is generous but finite.
const discoverTimeout = 45 * time.Second

// importVerifyBudget bounds the whole loopback gate of one import. Individual
// checks are still capped at 12s, but they run concurrently: twenty ticked
// local rows must finish inside a browser's patience, not twenty times it.
const importVerifyBudget = 20 * time.Second

// discoverModels serves GET /api/models/discover.
func (h *handler) discoverModels(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	sources := localmodels.Discover(ctx, localmodels.Options{})
	h.markRegistered(sources)
	writeJSON(w, map[string]any{"sources": sources})
}

// ccswitchModels serves GET /api/models/ccswitch: CC Switch's four app rosters
// as independent groups. It is a separate endpoint from discovery because the
// console shows it behind its own button and one group at a time — a single
// mixed list of thirty rows is what this split exists to avoid.
func (h *handler) ccswitchModels(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	groups := localmodels.CCSwitchGroups(ctx, localmodels.Options{})
	h.markRegistered(groups)
	writeJSON(w, map[string]any{"groups": groups})
}

// markRegistered fills Candidate.Existing.
//
// Discovery and import use the same registry identity for each candidate.
func (h *handler) markRegistered(sources []localmodels.Source) {
	h.readCfg(func(c *config.Config) {
		registered := make(map[string]bool, len(c.Models)+1)
		note := func(m config.ModelConfig) {
			if m.Model != "" {
				registered[modelIdentity(m)] = true
			}
		}
		for _, m := range c.Models {
			note(m)
		}
		note(c.Model)
		for si := range sources {
			for ci := range sources[si].Candidates {
				cand := &sources[si].Candidates[ci]
				cand.Existing = cand.Model != "" && registered[modelIdentity(cand.RegistryTarget())]
			}
		}
	})
}

// modelIdentity is the triple entry.Client keys a row on: two rows that agree on
// it are one row, whatever their aliases say.
func modelIdentity(mc config.ModelConfig) string {
	return strings.Join([]string{mc.NormalizedAPIType(), localmodels.EndpointIdentity(mc.BaseURL), strings.TrimSpace(mc.Model)}, "\x00")
}

// modelImportItem is one candidate the console ticked. It carries the
// credential *reference* the discovery JSON reported, never a secret: the
// server resolves it while writing, so a panel session cannot be replayed to
// read a key.
type modelImportItem struct {
	CandidateID   string `json:"candidate_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	BaseURL       string `json:"base_url,omitempty"`
	APIType       string `json:"api_type,omitempty"`
	KeyRef        string `json:"key_ref,omitempty"`
	NoAuth        bool   `json:"no_auth,omitempty"`
	Auth          string `json:"auth,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Alias         string `json:"alias,omitempty"`
}

type modelImportResult struct {
	Added            int      `json:"added"`
	Labeled          int      `json:"labeled"`
	Aliases          []string `json:"aliases"`
	CompletedIDs     []string `json:"completed_ids"`
	Skipped          []string `json:"skipped"`
	ActivationFailed bool     `json:"activation_failed,omitempty"`
}

// importModels serves POST /api/models/import — the batch form of POST
// /api/models, used by the discover sheet.
//
// One config write for the whole batch: the sheet can tick twenty rows, and
// twenty rewrites of the models block would leave twenty snapshots behind for
// one user action.
func (h *handler) importModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []modelImportItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	if h.cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no models to import"))
		return
	}
	if len(req.Items) > 500 {
		writeErr(w, http.StatusBadRequest, errors.New("too many models in one import"))
		return
	}

	opt := localmodels.Options{SkipVerification: true}
	var needsDiscovery, needsCCSwitch bool
	for _, item := range req.Items {
		if item.fromCCSwitch() {
			needsCCSwitch = true
		} else if item.needsDiscovery() {
			needsDiscovery = true
		}
	}

	// Verify references against the collector that issued them. The CC Switch
	// sheet reads a separate store and imports the candidate from that store.
	refCtx, refCancel := context.WithTimeout(r.Context(), discoverTimeout)
	var localRows, ccSwitchRows map[importTarget]localmodels.Candidate
	if needsCCSwitch {
		ccSwitchRows = importCandidates(localmodels.CCSwitchGroups(refCtx, opt))
	}
	if needsDiscovery {
		localRows = importCandidates(localmodels.Discover(refCtx, opt))
	}
	refCancel()
	result := modelImportResult{Aliases: []string{}, CompletedIDs: []string{}, Skipped: []string{}}
	skip := func(item modelImportItem, why string) {
		result.Skipped = append(result.Skipped, security.Redact(fmt.Sprintf("%s: %s", item.Model, why)))
	}
	type pendingImport struct {
		mc   config.ModelConfig
		item modelImportItem
	}
	var pending []pendingImport
	for _, item := range req.Items {
		var mc config.ModelConfig
		var err error
		if item.fromCCSwitch() {
			candidate, ok := ccSwitchRows[item.target()]
			if !ok {
				skip(item, "selection no longer matches the CC Switch store; refresh the list")
				continue
			}
			mc, err = rowFromImportCandidate(candidate, item.Alias, opt)
		} else if item.needsDiscovery() {
			candidate, ok := localRows[item.target()]
			if !ok {
				skip(item, "selection no longer matches local discovery; refresh the list")
				continue
			}
			mc, err = rowFromImportCandidate(candidate, item.Alias, opt)
		} else {
			mc, err = rowFromImport(item, opt)
		}
		if err != nil {
			skip(item, err.Error())
			continue
		}
		pending = append(pending, pendingImport{mc, item})
	}
	// A loopback row is cheap to check and expensive to get wrong: the proxy in
	// front of it is the thing that decides which model ids exist, and "the
	// config names it" is not the same answer. Remote rows are not checked —
	// fifty catalogue entries would make one import a minute of network — and
	// the loopback checks run together under one budget, because a server that
	// hangs must not spend twelve seconds per ticked row.
	//
	// A cancelled request imports nothing it could not confirm: the browser
	// going away is not a verdict, and the user would otherwise see "import
	// failed" after every row was written.
	checkCtx, cancel := context.WithTimeout(r.Context(), importVerifyBudget)
	defer cancel()
	verdicts := make([]error, len(pending))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range pending {
		if !localmodels.IsLoopback(pending[i].mc.BaseURL) {
			continue
		}
		select {
		case <-checkCtx.Done():
			verdicts[i] = checkCtx.Err()
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			verdicts[i] = localmodels.Verify(checkCtx, pending[i].mc, 12*time.Second)
		}(i)
	}
	wg.Wait()
	var added []pendingImport
	for i, p := range pending {
		if err := verdicts[i]; err != nil {
			skip(p.item, err.Error())
			continue
		}
		added = append(added, p)
	}
	if len(added) == 0 {
		writeJSON(w, result)
		return
	}

	var activate config.ModelConfig
	err := h.mutateCfgErr(func(c *config.Config) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		next := *c
		next.Models = append([]config.ModelConfig(nil), c.Models...)
		// Identity, not alias: an import that lands on an endpoint+dialect+model
		// the registry already holds is the same row, and suffixing its alias
		// with -2 would create a duplicate that the health check cannot tell
		// apart (entry.Client keys on exactly this triple).
		seen := make(map[string]int, len(next.Models)+len(added))
		for i, m := range next.Models {
			seen[modelIdentity(m)] = i
		}
		for _, row := range added {
			mc := row.mc
			id := modelIdentity(mc)
			if row.item.CandidateID != "" {
				result.CompletedIDs = append(result.CompletedIDs, row.item.CandidateID)
			}
			if idx, ok := seen[id]; ok {
				// The row is already here, but this is the only moment the
				// machine tells us where it came from: backfill the provenance
				// so a connection imported before the field existed is still
				// named after the tool that reported it. Not a duplicate —
				// nothing is added, only labelled.
				if mc.Source != "" && next.Models[idx].Source == "" {
					next.Models[idx].Source = mc.Source
					result.Labeled++
				}
				if row.item.CandidateID == "" {
					skip(row.item, "already registered")
				}
				continue
			}
			if next.Model.Model != "" && id == modelIdentity(next.Model) {
				if row.item.CandidateID == "" {
					skip(row.item, "already registered")
				}
				continue
			}
			seen[id] = len(next.Models)
			mc.Name = freeAliasIn(&next, firstNonEmpty(mc.Name, defaultAlias(mc)), config.ModelConfig{})
			// Append inside the loop: freeAliasIn reads the config, so the next
			// row in this batch has to be visible to it or two candidates with
			// one model id would both claim the same alias.
			next.Models = append(next.Models, mc)
			result.Aliases = append(result.Aliases, mc.Alias())
			result.Added++
			if next.Model.BaseURL == "" && next.Model.Provider == "" && next.Model.Model == "" {
				next.Model = mc
				activate = mc
			}
		}
		// Nothing new is not a reason to rewrite (and snapshot) the file — but
		// a backfilled label is a change, and it is the only one this pass made.
		if result.Added == 0 && result.Labeled == 0 {
			return nil
		}
		if h.configPath != "" {
			var err error
			if activate.Model != "" {
				err = config.UpdateModelAndModelsSections(h.configPath, next.Model, next.Models)
			} else {
				err = config.UpdateModelsSection(h.configPath, next.Models)
			}
			if err != nil {
				return err
			}
		}
		c.Models = next.Models
		c.Model = next.Model
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if activate.Model != "" {
		result.ActivationFailed = h.activateModelConfig(activate) != nil
	}
	writeJSON(w, result)
}

// Match the selection to a current server candidate before resolving credentials
// and use that candidate's auth scheme, context window and provenance.
type importTarget struct {
	id, ref, provider, baseURL, model, apiType, auth string
}

func (item modelImportItem) fromCCSwitch() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.KeyRef)), ccswitch.RefPrefix)
}

func (item modelImportItem) needsDiscovery() bool {
	return item.CandidateID != "" || strings.TrimSpace(item.KeyRef) != "" || strings.TrimSpace(item.Auth) != ""
}

func (item modelImportItem) target() importTarget {
	return importTarget{
		id:       item.CandidateID,
		ref:      strings.TrimSpace(item.KeyRef),
		provider: strings.TrimSpace(item.Provider),
		baseURL:  localmodels.EndpointIdentity(item.BaseURL),
		model:    strings.TrimSpace(item.Model),
		apiType:  strings.TrimSpace(item.APIType),
		auth:     strings.TrimSpace(item.Auth),
	}
}

func importCandidates(groups []localmodels.Source) map[importTarget]localmodels.Candidate {
	rows := make(map[importTarget]localmodels.Candidate)
	for _, group := range groups {
		for _, c := range group.Candidates {
			if !c.Importable {
				continue
			}
			item := modelImportItem{KeyRef: c.KeyRef, Provider: c.Provider, BaseURL: c.BaseURL,
				Model: c.Model, APIType: c.APIType, Auth: c.Auth}
			// Older clients submit the same fields without the candidate ID.
			rows[item.target()] = c
			item.CandidateID = c.ID
			rows[item.target()] = c
		}
	}
	return rows
}

// rowFromImport validates one item and builds the config row.
//
// The address check is the same one the add sheet's custom endpoint goes
// through: the import body names a URL the daemon will dial later, so a caller
// who holds the panel token must not be able to point it at a metadata service.
func rowFromImport(item modelImportItem, opt localmodels.Options) (config.ModelConfig, error) {
	model := strings.TrimSpace(item.Model)
	if model == "" {
		return config.ModelConfig{}, errors.New("no model id")
	}
	apiType := strings.TrimSpace(item.APIType)
	if apiType != "" {
		apiType = normalizeAPIType(apiType)
	}
	cand := localmodels.Candidate{
		Provider:      strings.TrimSpace(item.Provider),
		Model:         model,
		BaseURL:       strings.TrimSpace(item.BaseURL),
		APIType:       apiType,
		KeyRef:        strings.TrimSpace(item.KeyRef),
		NoAuth:        item.NoAuth,
		Auth:          strings.TrimSpace(item.Auth),
		ContextWindow: item.ContextWindow,
		Importable:    true,
	}
	return rowFromImportCandidate(cand, item.Alias, opt)
}

func rowFromImportCandidate(cand localmodels.Candidate, alias string, opt localmodels.Options) (config.ModelConfig, error) {
	mc, err := cand.Row(opt)
	if err != nil {
		return config.ModelConfig{}, err
	}
	if mc.Provider == "custom" || cand.BaseURL != "" {
		if err := checkCustomBaseURL(mc.BaseURL); err != nil {
			return config.ModelConfig{}, err
		}
	}
	// The client constructor is the last word on whether this row can exist: it
	// resolves the dialect, the key and the endpoint guard, so a candidate that
	// would fail on its first turn is refused here instead of being written.
	if _, err := entry.NewClient(mc); err != nil {
		return config.ModelConfig{}, err
	}
	if alias != "" {
		mc.Name = strings.TrimSpace(alias)
	}
	return mc, nil
}
