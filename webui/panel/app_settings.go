// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/commander"
	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/memory"
	"github.com/Xustalis/OpenPanda/internal/security"
)

// appSettingsJSON is the wire form of the "app policy" config groups (C1):
// model injection, routing preferences, memory caps, the approval gate, the
// agent tool face, and the subprocess sandbox mode. The sandbox block reports
// the confinement actually in effect; SandboxMode is its single writable
// field, because mode is a policy the operator owns while backend/active are
// facts about the platform.
type appSettingsJSON struct {
	InjectionModel  string           `json:"injection_model"` // auto | always | never
	PreferredAgents []string         `json:"preferred_agents"`
	MemoryLimits    memoryLimitsJSON `json:"memory_limits"`
	ApprovalMode    string           `json:"approval_mode"` // always | on-request | never
	// ToolsPolicy grades the tool face agent adapters run with: minimal keeps each
	// adapter's safe whitelist, extended reaches the agent's own skills, sub-agent
	// tooling and MCP servers. `panda config routing set tools_policy` has edited
	// it since v0.0.7; it was the one policy the console could not reach.
	ToolsPolicy string       `json:"tools_policy"` // minimal | extended
	Sandbox     *sandboxJSON `json:"sandbox,omitempty"`
	// SandboxMode is the write half of that block: off | standard | strict.
	// Empty means "leave the configured mode alone", so a client built before
	// this field existed still saves the rest of the policy.
	SandboxMode string `json:"sandbox_mode,omitempty"`
}

type memoryLimitsJSON struct {
	User    int `json:"user"`
	Memory  int `json:"memory"`
	Project int `json:"project"`
}

// On PATCH each field is optional, including the individual memory limits.
// A supplied empty preferred_agents list clears the routing preference.
type appSettingsPatchJSON struct {
	InjectionModel  *string   `json:"injection_model"`
	PreferredAgents *[]string `json:"preferred_agents"`
	MemoryLimits    struct {
		User    *int `json:"user"`
		Memory  *int `json:"memory"`
		Project *int `json:"project"`
	} `json:"memory_limits"`
	ApprovalMode *string `json:"approval_mode"`
	ToolsPolicy  *string `json:"tools_policy"`
	SandboxMode  *string `json:"sandbox_mode"`
}

// sandboxJSON reports the confinement actually in effect. Mode is writable
// (PUT, sandbox_mode); backend and active are read-only facts — the platform
// provides whatever backend it has (seatbelt/bwrap/""), and the UI must not
// pretend the sandbox is stronger than it is.
type sandboxJSON struct {
	WorkPath string `json:"work_path"`
	Mode     string `json:"mode"`
	Backend  string `json:"backend"`
	// Active is true only when a non-off mode meets a real platform backend.
	// A non-off mode with Backend()=="" blocks task subprocess launches. The
	// UI reports that failure rather than implying unconfined execution.
	Active bool `json:"active"`
}

// getAppSettings serves GET /api/settings/app — the live values of the policy
// groups plus the sandbox block (mode writable, backend/active descriptive).
func (h *handler) getAppSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	var out appSettingsJSON
	h.readCfg(func(c *config.Config) {
		out = appSettingsJSON{
			InjectionModel:  c.Injection.NormalizedModel(),
			PreferredAgents: append([]string{}, c.Routing.PreferredAgents...),
			MemoryLimits: memoryLimitsJSON{
				User:    c.Memory.Limits.User,
				Memory:  c.Memory.Limits.Memory,
				Project: c.Memory.Limits.Project,
			},
			ApprovalMode: c.Approval.NormalizedMode(),
			ToolsPolicy:  c.Routing.NormalizedToolsPolicy(),
			Sandbox: &sandboxJSON{
				WorkPath: c.Storage.WorkPath,
				Mode:     c.Sandbox.NormalizedMode(),
				Backend:  security.Backend(),
				Active:   c.Sandbox.NormalizedMode() != "off" && security.Backend() != "",
			},
		}
	})
	writeJSON(w, out)
}

// putAppSettings keeps PUT's full-form contract for older consoles. PATCH
// applies only the fields the user edited, so a stale form never replaces
// another editor's unrelated changes.
func (h *handler) putAppSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	req, err := decodeAppSettings(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := req.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	edits := req.configEdits()
	if len(edits) == 0 {
		h.getAppSettings(w, r)
		return
	}
	err = h.mutateCfgErr(func(c *config.Config) error {
		if h.configPath != "" {
			if err := config.UpdateSectionFields(h.configPath, edits); err != nil {
				return err
			}
		}
		req.apply(c)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if eng := h.currentEngine(); eng != nil {
		if req.InjectionModel != nil || req.PreferredAgents != nil || req.ToolsPolicy != nil {
			eng.RefreshRouterPolicy()
		}
		if req.SandboxMode != nil {
			// Runtime subprocesses read the process-wide base policy.
			eng.ApplySandboxPolicy()
		}
	} else if req.SandboxMode != nil {
		cardPath := h.cardPath()
		h.readCfg(func(c *config.Config) {
			commander.ApplyFromConfig(c, h.configPath, cardPath)
		})
	}

	h.getAppSettings(w, r)
}

func decodeAppSettings(r *http.Request) (*appSettingsPatchJSON, error) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if r.Method == http.MethodPatch {
		dec.DisallowUnknownFields()
		var patch *appSettingsPatchJSON
		if err := dec.Decode(&patch); err != nil || patch == nil || dec.Decode(new(any)) != io.EOF {
			return nil, errors.New("invalid JSON body")
		}
		return patch, nil
	}
	var full appSettingsJSON
	if err := dec.Decode(&full); err != nil {
		return nil, errors.New("invalid JSON body")
	}
	req := &appSettingsPatchJSON{
		InjectionModel:  &full.InjectionModel,
		PreferredAgents: &full.PreferredAgents,
		ApprovalMode:    &full.ApprovalMode,
	}
	req.MemoryLimits.User = &full.MemoryLimits.User
	req.MemoryLimits.Memory = &full.MemoryLimits.Memory
	req.MemoryLimits.Project = &full.MemoryLimits.Project
	// These were added after PUT shipped. Older forms leave them unchanged.
	if strings.TrimSpace(full.ToolsPolicy) != "" {
		req.ToolsPolicy = &full.ToolsPolicy
	}
	if strings.TrimSpace(full.SandboxMode) != "" {
		req.SandboxMode = &full.SandboxMode
	}
	return req, nil
}

func (req *appSettingsPatchJSON) validate() error {
	if req.InjectionModel != nil {
		*req.InjectionModel = strings.TrimSpace(*req.InjectionModel)
		switch *req.InjectionModel {
		case config.InjectionModelAuto, config.InjectionModelAlways, config.InjectionModelNever:
		default:
			return errors.New("injection_model must be auto, always, or never")
		}
	}
	if req.ApprovalMode != nil {
		*req.ApprovalMode = strings.TrimSpace(*req.ApprovalMode)
		switch *req.ApprovalMode {
		case config.ApprovalModeAlways, config.ApprovalModeOnRequest, config.ApprovalModeNever:
		default:
			return errors.New("approval_mode must be always, on-request, or never")
		}
	}
	if req.ToolsPolicy != nil {
		*req.ToolsPolicy = strings.TrimSpace(*req.ToolsPolicy)
		switch *req.ToolsPolicy {
		case config.ToolsPolicyMinimal, config.ToolsPolicyExtended:
		default:
			return errors.New("tools_policy must be minimal or extended")
		}
	}
	if req.SandboxMode != nil {
		parsed, err := config.ParseSandboxMode(strings.TrimSpace(*req.SandboxMode))
		if err != nil {
			return err
		}
		*req.SandboxMode = parsed
	}
	limits := req.MemoryLimits
	for _, limit := range []*int{limits.User, limits.Memory, limits.Project} {
		if limit != nil && *limit <= 0 {
			return errors.New("memory_limits values must be positive")
		}
	}
	if req.PreferredAgents == nil {
		return nil
	}
	agents := make([]string, 0, len(*req.PreferredAgents))
	seen := map[string]bool{}
	for _, name := range *req.PreferredAgents {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if err := memory.ValidateName(name); err != nil {
			return errors.New("preferred_agents contains an invalid agent name")
		}
		seen[name] = true
		agents = append(agents, name)
	}
	if len(agents) > 16 {
		return errors.New("preferred_agents holds at most 16 entries")
	}
	*req.PreferredAgents = agents
	return nil
}

func (req appSettingsPatchJSON) configEdits() []config.SectionFieldUpdate {
	var edits []config.SectionFieldUpdate
	for _, field := range []struct {
		section string
		key     string
		value   *string
	}{
		{"injection", "model", req.InjectionModel},
		{"approval", "mode", req.ApprovalMode},
		{"routing", "tools_policy", req.ToolsPolicy},
		{"sandbox", "mode", req.SandboxMode},
	} {
		if field.value != nil {
			edits = append(edits, config.SectionFieldUpdate{Section: []string{field.section}, Key: field.key, Value: *field.value})
		}
	}
	if req.PreferredAgents != nil {
		edits = append(edits, config.SectionFieldUpdate{Section: []string{"routing"}, Key: "preferred_agents", Value: *req.PreferredAgents})
	}
	for _, limit := range []struct {
		key   string
		value *int
	}{
		{"user", req.MemoryLimits.User},
		{"memory", req.MemoryLimits.Memory},
		{"project", req.MemoryLimits.Project},
	} {
		if limit.value != nil {
			edits = append(edits, config.SectionFieldUpdate{Section: []string{"memory", "limits"}, Key: limit.key, Value: *limit.value})
		}
	}
	return edits
}

func (req appSettingsPatchJSON) apply(c *config.Config) {
	if req.InjectionModel != nil {
		c.Injection.Model = *req.InjectionModel
	}
	if req.PreferredAgents != nil {
		c.Routing.PreferredAgents = *req.PreferredAgents
	}
	if req.ApprovalMode != nil {
		c.Approval.Mode = *req.ApprovalMode
	}
	if req.ToolsPolicy != nil {
		c.Routing.ToolsPolicy = *req.ToolsPolicy
	}
	if req.SandboxMode != nil {
		c.Sandbox.Mode = *req.SandboxMode
	}
	if req.MemoryLimits.User != nil {
		c.Memory.Limits.User = *req.MemoryLimits.User
	}
	if req.MemoryLimits.Memory != nil {
		c.Memory.Limits.Memory = *req.MemoryLimits.Memory
	}
	if req.MemoryLimits.Project != nil {
		c.Memory.Limits.Project = *req.MemoryLimits.Project
	}
}
