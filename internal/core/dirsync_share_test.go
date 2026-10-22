// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/bus"
)

// TestDirShareStopsPhantomRowFlood pins the directory's share cap against a
// member that signs rows for ids it does not own.
//
// The cap is the only thing that stops one member from filling the table
// with rows no honest peer could add. It counts how much of the directory a
// sender occupies — and it used to count by pub_key, the key an id is PINNED
// to. A non-stable id is never pinned (pinning one would let a member bind
// another node's id to its own key), so the attacker's phantom rows counted
// against nobody: its share stayed at 0 while the table grew to the 1024
// cap, and an honest new peer was left with room=0 — unable to publish its
// own row at all. One member could silence the whole fleet's directory.
//
// The signature covers the row's own key, so who SIGNED a row is proof, not a
// claim. The share cap counts that.
func TestDirShareStopsPhantomRowFlood(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "node-a")

	pub, priv, err := bus.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	seedPeerPubKey(t, c, "attacker", pubHex)

	// A frame of fresh ids, every one signed by the attacker's key.
	mkRows := func(prefix string, n int) []json.RawMessage {
		rows := make([]json.RawMessage, 0, n)
		for i := 0; i < n; i++ {
			r := &bus.DirRow{
				ID:     fmt.Sprintf("%s-%04d", prefix, i),
				PubKey: pubHex,
				SeenAt: time.Now().Unix(),
			}
			bus.SignDirRow(priv, r)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, raw)
		}
		return rows
	}

	env, err := bus.NewEnvelope(bus.MsgDirRows, "attacker", "m1",
		bus.DirRowsPayload{Rows: mkRows("phantom", dirRowsNewPerWindow)})
	if err != nil {
		t.Fatal(err)
	}
	c.handleDirRows(ctx, env)

	// The attacker's share must be attributed to it, and the cap must bite.
	_, share := c.dirRowBudget(ctx, "attacker")
	if share == 0 {
		t.Fatal("the attacker's rows counted against nobody — its share cap cannot bind")
	}
	if share > dirRowsShareMax {
		t.Fatalf("attacker share = %d, over the cap %d", share, dirRowsShareMax)
	}

	// A second window of fresh ids must add nothing: the share is full.
	var afterFirst int
	if err := c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache`).Scan(&afterFirst); err != nil {
		t.Fatal(err)
	}
	c.dir.mu.Lock()
	c.dir.rowsNewAt["attacker"] = time.Time{}
	c.dir.mu.Unlock()
	env2, err := bus.NewEnvelope(bus.MsgDirRows, "attacker", "m2",
		bus.DirRowsPayload{Rows: mkRows("phantom2", dirRowsNewPerWindow)})
	if err != nil {
		t.Fatal(err)
	}
	c.handleDirRows(ctx, env2)

	var total int
	if err := c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != afterFirst {
		t.Fatalf("directory grew from %d to %d rows on a second flood frame — the share cap did not bind",
			afterFirst, total)
	}

	// The point of the cap: an honest peer can still publish.
	room, _ := c.dirRowBudget(ctx, "honest")
	if room <= 0 {
		t.Fatalf("an honest peer has room=%d — the flood starved the directory", room)
	}
}
