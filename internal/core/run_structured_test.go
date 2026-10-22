// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"

	"github.com/Xustalis/OpenPanda/internal/commander"
)

// TestApplyStructuredFailure pins the one thing the adapter's own ok flag
// cannot see: a clean CLI turn whose model said it failed. That run used to
// settle as done with the failure buried in a trace event.
func TestApplyStructuredFailure(t *testing.T) {
	tests := []struct {
		name       string
		res        commander.Result
		sr         *structuredAgentResult
		wantOK     bool
		wantStderr string
	}{
		{
			name:       "failed status flips ok and carries the answer",
			res:        commander.Result{OK: true},
			sr:         &structuredAgentResult{Status: "failed", Answer: "could not reach the device"},
			wantOK:     false,
			wantStderr: "could not reach the device",
		},
		{
			name:       "failed status keeps a real stderr",
			res:        commander.Result{OK: true, Stderr: "exit 1"},
			sr:         &structuredAgentResult{Status: "failed", Answer: "could not reach the device"},
			wantOK:     false,
			wantStderr: "exit 1",
		},
		{
			name:       "failed status without an answer still fails",
			res:        commander.Result{OK: true},
			sr:         &structuredAgentResult{Status: "failed"},
			wantOK:     false,
			wantStderr: "agent reported status: failed",
		},
		{
			name:   "done status leaves the result alone",
			res:    commander.Result{OK: true, Stderr: ""},
			sr:     &structuredAgentResult{Status: "done"},
			wantOK: true,
		},
		{
			name:   "question status is not a failure",
			res:    commander.Result{OK: true},
			sr:     &structuredAgentResult{Status: "question", Question: "which port?"},
			wantOK: true,
		},
		{
			name:   "absent structured output is a no-op",
			res:    commander.Result{OK: true},
			sr:     nil,
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			applyStructuredFailure(&tc.res, tc.sr)
			if tc.res.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v", tc.res.OK, tc.wantOK)
			}
			if tc.res.Stderr != tc.wantStderr {
				t.Fatalf("Stderr = %q, want %q", tc.res.Stderr, tc.wantStderr)
			}
		})
	}
}
