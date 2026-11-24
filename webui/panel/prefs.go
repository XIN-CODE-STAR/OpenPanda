// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

// The console's node-wide preferences: today, the directories starred in the
// working-directory picker. They live here rather than inside /api/projects
// because a star belongs to a *path*, not to a project — a folder can be
// starred before it ever becomes one, and the star must survive a rename of the
// project that owns it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
)

// prefsView is the whole bag. A struct rather than a map so the next
// preference can be added without changing the endpoint's shape.
type prefsView struct {
	Favorites []string `json:"favorites"`
}

// getPrefs serves GET /api/prefs. A node without a project store answers with
// an empty list instead of failing: the picker then behaves as it did before
// favourites existed.
func (h *handler) getPrefs(w http.ResponseWriter, _ *http.Request) {
	favs := []string{}
	if h.projectStore != nil {
		list, err := h.projectStore.Favorites()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		favs = list
	}
	writeJSON(w, prefsView{Favorites: favs})
}

// postFavorite serves POST /api/prefs/favorite — one path, starred or not. The
// answer is the whole list, so the card never has to guess what the store now
// holds after a toggle it did not see.
func (h *handler) postFavorite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		Favorite bool   `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	if !filepath.IsAbs(path) {
		writeErr(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}
	if h.projectStore == nil {
		writeJSON(w, prefsView{Favorites: []string{}})
		return
	}
	list, err := h.projectStore.SetFavorite(path, req.Favorite)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, prefsView{Favorites: list})
}
