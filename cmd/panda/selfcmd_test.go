// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/Xustalis/OpenPanda/internal/storage"
)

// TestSchemaFloorFailsClosed pins the rollback guard's input: an unreadable
// store must be an error, never the zero floor that means "nothing to
// protect". Turning a failed read into 0 was a fail-open rollback.
func TestSchemaFloorFailsClosed(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	floor, err := schemaFloor(db)
	if err != nil || floor <= 0 {
		t.Fatalf("schemaFloor = %d, %v; want a positive floor", floor, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := schemaFloor(db); err == nil {
		t.Fatal("a closed store must report an error, not a zero floor")
	}
}
