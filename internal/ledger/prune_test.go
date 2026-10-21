// SPDX-License-Identifier: AGPL-3.0-or-later

package ledger

import (
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/storage"
)

// TestPruneOfflineKeepsPins pins the directory GC contract: only long-offline
// rows with NO verified-hello pin are deleted. A pinned row is a TOFU
// commitment and must survive; recent rows and explicitly excluded ids stay.
func TestPruneOfflineKeepsPins(t *testing.T) {
	db := openLedgerDB(t)
	now := storage.Now()
	old := now - int64((31 * 24 * time.Hour).Seconds())
	recent := now - 60
	insert := func(id, status, pub string, lastSeen, seenAt int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO employee_cache (id, status, pub_key, last_seen, seen_at)
			VALUES (?, ?, ?, ?, ?)`, id, status, pub, lastSeen, seenAt); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("gossip-old", "offline", "", old, old)
	insert("gossip-recent", "offline", "", recent, recent)
	insert("pinned-old", "offline", "aabb", old, old)
	insert("excluded-old", "offline", "", old, old)
	insert("online-old", "online", "", old, old)

	n, err := PruneOffline(db, int64((30 * 24 * time.Hour).Seconds()), []string{"excluded-old"})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id='gossip-old'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatal("gossip-only stale row survived")
	}
	for _, id := range []string{"gossip-recent", "pinned-old", "excluded-old", "online-old"} {
		var one int
		if err := db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id=?`, id).Scan(&one); err != nil || one != 1 {
			t.Fatalf("%s was pruned (count=%d err=%v)", id, one, err)
		}
	}
}

// A card that travels to another machine brings commands that machine may not
// have. The pruning has to remove exactly those and leave the rest — including
// the abilities that are still runnable — because dropping too much silently
// downgrades the node and dropping too little makes routing pick a plan that
// dies at exec with 127.
func TestPruneUnavailableNativeDropsOnlyMissingCommands(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) bool { return name == "uname" || name == "ping" }

	c := Card{Device: "d", Native: []NativeAbility{
		{ID: "sys:info", Command: "uname", Args: []string{"-a"}},
		{ID: "disk:usage", Command: "df"},
		{ID: "temp:read", Command: "vcgencmd"},
		{ID: "net:ping", Command: "ping"},
		{ID: "broken", Command: ""}, // no command at all is unrunnable too
	}}

	dropped := c.PruneUnavailableNative()
	want := map[string]bool{"disk:usage": true, "temp:read": true, "broken": true}
	if len(dropped) != len(want) {
		t.Fatalf("dropped %v, want the 3 unavailable ids", dropped)
	}
	for _, id := range dropped {
		if !want[id] {
			t.Errorf("dropped %q, which is runnable here", id)
		}
	}
	if len(c.Native) != 2 || c.Native[0].ID != "sys:info" || c.Native[1].ID != "net:ping" {
		t.Fatalf("kept %+v, want sys:info and net:ping in order", c.Native)
	}
	// The surviving ability keeps its args: pruning filters, it never rewrites.
	if len(c.Native[0].Args) != 1 || c.Native[0].Args[0] != "-a" {
		t.Errorf("args mangled: %+v", c.Native[0].Args)
	}
}

// Nothing to prune must be a no-op that reports nothing, so the caller's log
// line only ever appears when a real ability was removed.
func TestPruneUnavailableNativeIsSilentWhenEverythingResolves(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) bool { return true }

	c := Card{Device: "d", Native: []NativeAbility{{ID: "sys:info", Command: "uname"}}}
	if dropped := c.PruneUnavailableNative(); dropped != nil {
		t.Fatalf("dropped %v from a fully available card", dropped)
	}
	if len(c.Native) != 1 {
		t.Fatalf("native mutated: %+v", c.Native)
	}

	empty := Card{Device: "d"}
	if dropped := empty.PruneUnavailableNative(); dropped != nil {
		t.Fatalf("dropped %v from a card with no native block", dropped)
	}
}
