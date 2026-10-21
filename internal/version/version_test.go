// SPDX-License-Identifier: AGPL-3.0-or-later

package version

import "testing"

// TestProvenanceClass pins the normalizer every reporting surface shares:
// defined classes pass through; the unset default and any unrecognized
// injected stamp fold to "unsigned" — never "" — so a dev build stays
// distinguishable from a pre-provenance peer that sends nothing.
func TestProvenanceClass(t *testing.T) {
	orig := Provenance
	t.Cleanup(func() { Provenance = orig })

	cases := []struct {
		stamp string
		want  string
	}{
		{"", ProvenanceUnsigned},
		{ProvenanceRelease, ProvenanceRelease},
		{ProvenanceSelf, ProvenanceSelf},
		{ProvenanceUnsigned, ProvenanceUnsigned},
		{"banana", ProvenanceUnsigned},
		{"SELF", ProvenanceUnsigned},
	}
	for _, tc := range cases {
		Provenance = tc.stamp
		if got := ProvenanceClass(); got != tc.want {
			t.Errorf("ProvenanceClass(%q) = %q, want %q", tc.stamp, got, tc.want)
		}
	}
}
