// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"testing"
)

// TestCLIErrorLineIsParseableJSON pins the --json failure contract: one line on
// stderr, one object, the documented keys. Before this existed a `--json`
// consumer had to parse two formats — JSON on success, prose on failure — and
// the prose was not even on stdout, so `panda queue --json | jq` reported a
// syntax error instead of the reason.
func TestCLIErrorLineIsParseableJSON(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		step     string
		msg      string
		wantMsg  string
		wantStep string
	}{
		{
			name:    "rejected argument",
			code:    2,
			msg:     "task add requires --title",
			wantMsg: "task add requires --title",
		},
		{
			name:     "operation failed carries its step",
			code:     1,
			step:     "open store",
			msg:      "unable to open database file",
			wantMsg:  "unable to open database file",
			wantStep: "open store",
		},
		{
			name:    "text decorations are not part of the message",
			code:    2,
			msg:     "panda: error: --config nope.yaml: no such file",
			wantMsg: "--config nope.yaml: no such file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := cliErrorLine(tc.code, tc.step, tc.msg)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var got struct {
				Error struct {
					Code    int    `json:"code"`
					Step    string `json:"step"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatalf("not JSON (%v): %s", err, line)
			}
			if got.Error.Code != tc.code {
				t.Errorf("code = %d, want %d", got.Error.Code, tc.code)
			}
			if got.Error.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", got.Error.Message, tc.wantMsg)
			}
			if got.Error.Step != tc.wantStep {
				t.Errorf("step = %q, want %q", got.Error.Step, tc.wantStep)
			}
			if n := len(line); n > 0 && line[n-1] == '\n' {
				t.Errorf("the line carries its own newline; the writer adds one")
			}
		})
	}
}
