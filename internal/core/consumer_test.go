// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// TestStoreConsumerFresh pins the store-bound consumer probe: only a heartbeat
// the caller did not write counts. The lastForeign/selfWriteAt pair brackets
// the caller's own Register stamps — a borrowed engine or one-shot CLI
// rewrites last_seen at init (and again on every card reload), so a naive
// freshness check reads its own write as a live consumer and the split
// db_path dead-letter stays invisible.
func TestStoreConsumerFresh(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if StoreConsumerFresh(ctx, db, "ghost", 0, 0) {
		t.Fatal("absent row must not count as a live consumer")
	}
	if StoreConsumerFresh(ctx, nil, "n", 0, 0) {
		t.Fatal("nil db must not count as a live consumer")
	}

	// The caller never wrote the row: plain freshness — a foreign heartbeat.
	card := ledger.Card{Device: "node-a"}
	if err := ledger.Register(db, card, "node-a", 1); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !StoreConsumerFresh(ctx, db, "node-a", 0, 0) {
		t.Fatal("fresh foreign row must count as live")
	}

	// Stale row, no write of ours: dead.
	stale := time.Now().Add(-2 * storeConsumerMaxAge).Unix()
	if _, err := db.Exec(`UPDATE employee_cache SET last_seen=? WHERE id=?`, stale, "node-a"); err != nil {
		t.Fatalf("age row: %v", err)
	}
	if StoreConsumerFresh(ctx, db, "node-a", 0, 0) {
		t.Fatal("stale row must not count as a live consumer")
	}

	// The split-store case: our own Register stamped last_seen=now (cur ==
	// selfWriteAt) with no foreign beat ever observed — must read dead even
	// though the row LOOKS fresh.
	now := time.Now().Unix()
	if _, err := db.Exec(`UPDATE employee_cache SET last_seen=? WHERE id=?`, now, "node-a"); err != nil {
		t.Fatalf("self stamp: %v", err)
	}
	if StoreConsumerFresh(ctx, db, "node-a", 0, now) {
		t.Fatal("our own register stamp must not count as a foreign heartbeat")
	}

	// Re-register self-poisoning guard: a foreign stamp observed once long ago
	// stays stale — freshness is measured from the stamp's age, not from the
	// time of our latest write.
	if StoreConsumerFresh(ctx, db, "node-a", stale, now) {
		t.Fatal("an aged foreign stamp must not count as live")
	}

	// A beat landing after our write — a real owner arriving — counts live
	// even when nothing foreign was seen before.
	if _, err := db.Exec(`UPDATE employee_cache SET last_seen=? WHERE id=?`, now+1, "node-a"); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if !StoreConsumerFresh(ctx, db, "node-a", 0, now) {
		t.Fatal("a heartbeat newer than our register must count as live")
	}

	// The healthy shared-store case (`task add` while the daemon owns this
	// db): a foreign stamp inside the window counts even when the row still
	// carries our own latest write.
	if _, err := db.Exec(`UPDATE employee_cache SET last_seen=? WHERE id=?`, now, "node-a"); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	if !StoreConsumerFresh(ctx, db, "node-a", now-10, now) {
		t.Fatal("a fresh foreign stamp must count as live")
	}
}
