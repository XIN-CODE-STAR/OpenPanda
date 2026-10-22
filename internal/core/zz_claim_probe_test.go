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

func TestZZClaimShareBypass(t *testing.T) {
	ctx := context.Background()
	c := newCoreOnDB(t, openTestDB(t), "node-a")

	// Attacker's verified hello pinned its session id to its key.
	pub, priv, err := bus.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	seedPeerPubKey(t, c, "attacker", pubHex)

	mkPhantom := func(id string) json.RawMessage {
		r := &bus.DirRow{ID: id, PubKey: pubHex, SeenAt: time.Now().Unix()}
		bus.SignDirRow(priv, r)
		return bus.CanonicalDirRowBytes(r)
	}

	// One frame: 256 fresh non-stable ids signed by the pinned key.
	var rows []json.RawMessage
	for i := 0; i < dirRowsNewPerWindow; i++ {
		rows = append(rows, mkPhantom(fmt.Sprintf("phantom-%04d", i)))
	}
	env, err := bus.NewEnvelope(bus.MsgDirRows, "attacker", "m1", bus.DirRowsPayload{Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	c.handleDirRows(ctx, env)

	var total int
	if err := c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	room, share := c.dirRowBudget(ctx, "attacker")
	t.Logf("after 1 frame: total=%d room=%d share=%d", total, room, share)
	var withKey int
	_ = c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache WHERE id LIKE 'phantom-%' AND COALESCE(pub_key,'')!=''`).Scan(&withKey)
	t.Logf("phantoms with a pub_key pin: %d", withKey)

	// Roll the new-id window repeatedly and keep pushing fresh ids.
	for w := 0; w < 5; w++ {
		var more []json.RawMessage
		for i := 0; i < dirRowsNewPerWindow; i++ {
			more = append(more, mkPhantom(fmt.Sprintf("phantom-%02d-%04d", w+1, i)))
		}
		e, err := bus.NewEnvelope(bus.MsgDirRows, "attacker", fmt.Sprintf("m%d", w+2), bus.DirRowsPayload{Rows: more})
		if err != nil {
			t.Fatal(err)
		}
		c.dir.mu.Lock()
		c.dir.rowsNewAt["attacker"] = time.Time{}
		c.dir.mu.Unlock()
		c.handleDirRows(ctx, e)
		_ = c.db.QueryRow(`SELECT COUNT(1) FROM employee_cache`).Scan(&total)
		room, share = c.dirRowBudget(ctx, "attacker")
		t.Logf("window %d: total=%d room=%d share=%d", w, total, room, share)
	}
	// What can an honest new peer add now?
	c.dir.mu.Lock()
	c.dir.rowsNewAt["honest"] = time.Time{}
	c.dir.mu.Unlock()
	roomH, shareH := c.dirRowBudget(ctx, "honest")
	t.Logf("honest new peer: room=%d share=%d (dirRowsShareMax=%d dirRowsMax=%d)", roomH, shareH, dirRowsShareMax, dirRowsMax)
}