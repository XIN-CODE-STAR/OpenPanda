// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Xustalis/OpenPanda/internal/config"
)

func TestAppSettingsPatchMergesStaleForms(t *testing.T) {
	h, _, path := newEnginePanel(t)
	const route = "/api/settings/app"
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("# keep this comment\n"), original...), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both forms were loaded before either edit. Their nested updates must
	// each apply against the node's current policy.
	firstStatus, first := doJSON(t, h, authedReq(http.MethodGet, route, nil))
	secondStatus, second := doJSON(t, h, authedReq(http.MethodGet, route, nil))
	if firstStatus != http.StatusOK || secondStatus != http.StatusOK ||
		first["injection_model"] != second["injection_model"] {
		t.Fatalf("load both forms: %d %v, %d %v", firstStatus, first, secondStatus, second)
	}
	patch := func(body string) map[string]any {
		t.Helper()
		reply := httptest.NewRecorder()
		h.ServeHTTP(reply, jsonReq(http.MethodPatch, route, body))
		var result map[string]any
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &result) != nil {
			t.Fatalf("PATCH %s: HTTP %d, %s", body, reply.Code, reply.Body.String())
		}
		return result
	}
	patch(`{"memory_limits":{"user":2400},"preferred_agents":["codex"],"tools_policy":"extended"}`)
	out := patch(`{"memory_limits":{"memory":3400},"approval_mode":"always"}`)
	limits, ok := out["memory_limits"].(map[string]any)
	if !ok || limits["user"] != 2400.0 || limits["memory"] != 3400.0 ||
		limits["project"] != first["memory_limits"].(map[string]any)["project"] ||
		out["tools_policy"] != "extended" || out["approval_mode"] != "always" {
		t.Fatalf("second PATCH did not return the merged policy: %v", out)
	}
	disk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Memory.Limits.User != 2400 || disk.Memory.Limits.Memory != 3400 ||
		disk.Routing.ToolsPolicy != "extended" || disk.Approval.Mode != "always" ||
		len(disk.Routing.PreferredAgents) != 1 || disk.Routing.PreferredAgents[0] != "codex" {
		t.Fatalf("disk policy: limits=%+v, routing=%+v, approval=%+v", disk.Memory.Limits, disk.Routing, disk.Approval)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "# keep this comment") {
		t.Fatalf("config comment was lost, read error %v", err)
	}

	for _, body := range []string{
		`{"memory_limits":{"user":1200,"project":0}}`,
		`{"injection_model":"never","sandbox_mode":"standrad"}`,
		`{"memory_limits":{"unknown":10}}`,
	} {
		if code, out := doJSON(t, h, jsonReq(http.MethodPatch, route, body)); code != http.StatusBadRequest {
			t.Fatalf("invalid PATCH accepted: %d %v (%s)", code, out, body)
		}
	}
	out = patch(`{"preferred_agents":[]}`)
	limits = out["memory_limits"].(map[string]any)
	if limits["user"] != 2400.0 || out["injection_model"] != first["injection_model"] ||
		len(out["preferred_agents"].([]any)) != 0 {
		t.Fatalf("invalid PATCH mutated settings or empty list did not clear: %v", out)
	}
}

func TestAppSettingsPatchDiskFailureIsAtomic(t *testing.T) {
	h, _, path := newEnginePanel(t)
	const route = "/api/settings/app"
	const invalidConfig = "injection:\n  model: auto\nmemory: broken\n"
	if err := os.WriteFile(path, []byte(invalidConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _ := doJSON(t, h, jsonReq(http.MethodPatch, route,
		`{"injection_model":"always","memory_limits":{"user":2400}}`))
	if code != http.StatusInternalServerError {
		t.Fatalf("PATCH with invalid disk shape: %d", code)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != invalidConfig {
		t.Fatalf("failed PATCH partially wrote config: %q, error %v", raw, err)
	}
	code, out := doJSON(t, h, authedReq(http.MethodGet, route, nil))
	if code != http.StatusOK || out["injection_model"] != "auto" {
		t.Fatalf("failed PATCH changed live policy: %d, %v", code, out)
	}
}
