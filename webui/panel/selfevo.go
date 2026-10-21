// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Xustalis/OpenPanda/internal/self"
	"github.com/Xustalis/OpenPanda/internal/updater"
	versionpkg "github.com/Xustalis/OpenPanda/internal/version"
)

// selfevoJSON is the wire form of GET /api/selfevo — the self-evolution
// pipeline's operator surface for the console: enabled flag, where its state
// lives, the running binary's provenance, the rollback binary's presence, and
// the patch-set queue. Everything is read-only here; the merge decision
// itself rides the existing task review endpoints (the proposal IS a task).
type selfevoJSON struct {
	Enabled    bool             `json:"enabled"`
	Repo       string           `json:"repo,omitempty"`
	StateDir   string           `json:"state_dir"`
	Provenance string           `json:"provenance"`
	Fallback   string           `json:"fallback,omitempty"`
	Worktree   string           `json:"worktree,omitempty"`
	Sets       []selfevoSetJSON `json:"sets"`
}

// selfevoSetJSON is one patch set as the console lists it: manifest fields
// plus the merge-side recomputed critical flag — never the producer's claim.
type selfevoSetJSON struct {
	ID            string   `json:"id"`
	State         string   `json:"state"`
	Title         string   `json:"title"`
	Base          string   `json:"base"`
	Version       string   `json:"version"`
	Producer      string   `json:"producer"`
	Files         []string `json:"files"`
	Critical      bool     `json:"critical"`
	CriticalPaths []string `json:"critical_paths,omitempty"`
	GateOK        bool     `json:"gate_ok"`
}

func toSelfevoSet(s self.Set) selfevoSetJSON {
	return selfevoSetJSON{
		ID: s.Manifest.ID, State: string(s.State), Title: s.Manifest.Title,
		Base: s.Manifest.Base, Version: s.Manifest.Version, Producer: s.Manifest.Producer,
		Files: s.Manifest.Files, Critical: s.Critical, CriticalPaths: s.CriticalPaths,
		GateOK: s.Manifest.Gate.OK,
	}
}

// selfManager builds a read-scoped manager from the panel's config. Works
// while disabled — status must answer on a node that never opted in.
func (h *handler) selfManager() *self.Manager {
	if h.cfg == nil {
		return nil
	}
	return self.New(h.cfg.SelfEvolutionDir(), h.cfg.SelfEvolution.Repo, nil)
}

// getSelfEvo serves GET /api/selfevo.
func (h *handler) getSelfEvo(w http.ResponseWriter, r *http.Request) {
	out := selfevoJSON{StateDir: "", Provenance: versionpkg.ProvenanceClass(), Sets: []selfevoSetJSON{}}
	mgr := h.selfManager()
	if h.cfg != nil {
		out.Enabled = h.cfg.SelfEvolution.Enabled
		out.Repo = h.cfg.SelfEvolution.Repo
		out.StateDir = h.cfg.SelfEvolutionDir()
	}
	if fb := updater.ReleaseFallbackPath(); fb != "" {
		if _, err := os.Stat(fb); err == nil {
			out.Fallback = fb
		}
	}
	if mgr != nil {
		if _, err := os.Stat(filepath.Join(mgr.WorktreePath(), ".git")); err == nil {
			out.Worktree = mgr.WorktreePath()
		}
		if sets, err := mgr.Queue(); err == nil {
			for _, s := range sets {
				out.Sets = append(out.Sets, toSelfevoSet(s))
			}
		}
	}
	writeJSON(w, out)
}

// getSelfEvoSet serves GET /api/selfevo/{id} — one set's manifest view.
func (h *handler) getSelfEvoSet(w http.ResponseWriter, r *http.Request) {
	mgr := h.selfManager()
	if mgr == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("no config"))
		return
	}
	set, err := mgr.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, toSelfevoSet(set))
}

// getSelfEvoFile serves GET /api/selfevo/{id}/file?n=<patch-name> — the raw
// unified diff for the review view. The name MUST be one the manifest lists:
// the endpoint never reads outside the set's own directory.
func (h *handler) getSelfEvoFile(w http.ResponseWriter, r *http.Request) {
	mgr := h.selfManager()
	if mgr == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("no config"))
		return
	}
	set, err := mgr.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	name := r.URL.Query().Get("n")
	listed := false
	for _, f := range set.Manifest.Files {
		if f == name {
			listed = true
			break
		}
	}
	if !listed {
		writeErr(w, http.StatusBadRequest, errors.New("file not in manifest"))
		return
	}
	body, err := os.ReadFile(filepath.Join(set.Dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(body)
}
