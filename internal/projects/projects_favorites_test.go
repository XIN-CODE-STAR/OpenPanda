// SPDX-License-Identifier: AGPL-3.0-or-later

package projects

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// A star is a preference of the node, not of a project: it is stored as one
// settings row and read back in the order it was set, newest first.
func TestFavoritesRoundTrip(t *testing.T) {
	s := newTestStore(t)

	empty, err := s.Favorites()
	if err != nil {
		t.Fatalf("favorites on a fresh store: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("fresh favorites = %v, want empty", empty)
	}

	if _, err := s.SetFavorite(`D:\X\project`, true); err != nil {
		t.Fatalf("star first: %v", err)
	}
	got, err := s.SetFavorite(`D:\X\other`, true)
	if err != nil {
		t.Fatalf("star second: %v", err)
	}
	want := []string{`D:\X\other`, `D:\X\project`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("favorites = %v, want %v", got, want)
	}

	// Same folder, different spelling: Windows does not have two of these, so
	// starring it again must move it to the front rather than add a row.
	got, err = s.SetFavorite(`d:\x\PROJECT\`, true)
	if err != nil {
		t.Fatalf("restar with different case: %v", err)
	}
	want = []string{`d:\x\PROJECT\`, `D:\X\other`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after restar = %v, want %v", got, want)
	}

	got, err = s.SetFavorite(`D:\X\other`, false)
	if err != nil {
		t.Fatalf("unstar: %v", err)
	}
	want = []string{`d:\x\PROJECT\`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after unstar = %v, want %v", got, want)
	}

	// The list survives a reload of the store, which is the whole point of
	// keeping it on disk.
	if _, err := s.SetFavorite(`d:\x\PROJECT\`, false); err != nil {
		t.Fatalf("clear last: %v", err)
	}
	got, err = s.Favorites()
	if err != nil {
		t.Fatalf("favorites after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("favorites after clear = %v, want empty", got)
	}
}

func TestSetFavoriteRejectsEmptyPath(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetFavorite("   ", true); err == nil {
		t.Fatal("starring an empty path should fail")
	}
}

// Concurrent stars must all survive. Reading the whole list and overwriting it
// in a second statement lost every write but the last one — 24 simultaneous
// stars left five rows — and a star the next read cannot see is a star the
// picker never shows.
func TestConcurrentFavoritesKeepEveryStar(t *testing.T) {
	s := newTestStore(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.SetFavorite(fmt.Sprintf(`D:\X\f%02d`, i), true); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent star: %v", err)
	}
	got, err := s.Favorites()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != n {
		t.Fatalf("favorites = %d rows, want %d", len(got), n)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[favoriteKey(d)] = true
	}
	for i := 0; i < n; i++ {
		if key := favoriteKey(fmt.Sprintf(`D:\X\f%02d`, i)); !seen[key] {
			t.Fatalf("star %q lost in the concurrent write", key)
		}
	}
}
