// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpdateNetworkSectionCreatesMissingDir pins the first-run contract: the
// join path (`panda pair --secret … --peer …`, `panda nodes add …`) writes
// the network section into a config that does not exist yet, on a machine
// where the config directory itself has never been created.
//
// Every sibling writer (UpdateModelSection, UpdateModelsSection,
// UpdateSectionField*) does os.MkdirAll before the atomic write. When
// UpdateNetworkSection did not, the write died with
//
//	create temp file: open …/openpanda/.tmp-config.yaml-…: no such file or directory
//
// — a raw OS error on the exact command the join guide printed to the other
// machine.
func TestUpdateNetworkSectionCreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent directory does not exist at all — the fresh-machine
	// shape. t.TempDir() exists; the config dir under it does not.
	path := filepath.Join(dir, "Application Support", "openpanda", "config.yaml")

	if err := UpdateNetworkSection(path, NetworkConfig{
		ListenAddr:   "127.0.0.1:7836",
		SharedSecret: "secret-material",
		Peers:        []PeerEntry{{Addr: "127.0.0.1:7837"}},
	}); err != nil {
		t.Fatalf("UpdateNetworkSection into a missing dir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("config written empty")
	}
}
