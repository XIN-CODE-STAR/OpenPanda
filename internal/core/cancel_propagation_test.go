// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// slowNativeAbility is a long-running native capability used to catch a task
// mid-flight: the cancel has to land while the executor is still working.
func slowNativeAbility() ledger.NativeAbility {
	return ledger.NativeAbility{ID: "slow:run", Command: "sleep", Args: []string{"30"}}
}

// TestCancelPropagatesDownstream verifies P2-3: cancelling a delegated task at
// the delegator reaches the executor, whose local copy transitions to
// cancelled instead of running to completion and reporting into a void.
func TestCancelPropagatesDownstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	root := newCore(t, "root")
	leaf := newCoreWithNative(t, "leaf", slowNativeAbility())

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("startup: %v", err)
		}
	}
	must(root.Register(ctx))
	must(leaf.Register(ctx))
	startListen(t, ctx, root, "127.0.0.1:0")
	leafAddr := startListen(t, ctx, leaf, "127.0.0.1:0")
	must(root.DialPeer(ctx, leafAddr))
	waitPeer(t, root, "leaf")
	waitPeer(t, leaf, "root")
	time.Sleep(300 * time.Millisecond)

	// Submit blocks until a result arrives; run it in the background so the
	// test can cancel mid-flight.
	in := TaskInput{Title: "slow", Intent: "slow", Requires: []string{"slow:run"}}
	type outcome struct {
		task Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		tk, _, err := root.Submit(ctx, in)
		done <- outcome{tk, err}
	}()

	// Wait until the leaf is actually running the task, then cancel at root.
	var taskID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := leaf.store.ListByState(ctx, StateRunning)
		if err == nil && len(tasks) > 0 {
			taskID = tasks[0].TaskID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if taskID == "" {
		t.Fatalf("leaf never started the task")
	}

	if _, err := root.CancelTree(ctx, taskID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The cancel must propagate: the leaf's copy leaves running for cancelled.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := leaf.store.Get(ctx, taskID)
		if err == nil && tk.State == StateCancelled {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	tk, _ := leaf.store.Get(ctx, taskID)
	t.Fatalf("leaf task state = %s, want cancelled (cancel did not propagate)", tk.State)
}

func TestOfflineCancelSurvivesTaskRemoval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(context.Context, *TaskStore, string) error
	}{
		{"keep", nil},
		{"delete", func(ctx context.Context, s *TaskStore, id string) error {
			_, err := s.Delete(ctx, id)
			return err
		}},
		{"clear queue", func(ctx context.Context, s *TaskStore, _ string) error {
			_, _, err := s.ClearQueue(ctx)
			return err
		}},
		{"retention", func(ctx context.Context, s *TaskStore, _ string) error {
			_, err := s.DeleteSettledBefore(ctx, time.Now().Unix()+1)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			root := newCore(t, "cancel-owner")
			leaf := newCore(t, "cancel-executor")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(root.Register(ctx))
			must(leaf.Register(ctx))
			chain := []string{root.nodeID, leaf.nodeID}
			task, err := root.store.Create(ctx, "", "", "cancel while offline", root.nodeID, chain[:1])
			must(err)
			must(root.store.Queue(ctx, task.TaskID, root.nodeID))
			must(root.store.Dispatch(ctx, task.TaskID, root.nodeID, leaf.nodeID))
			_, err = leaf.store.CreateFromRemote(ctx, task.TaskID, task.Title, leaf.nodeID, task.AttemptID, chain)
			must(err)
			must(leaf.store.Queue(ctx, task.TaskID, leaf.nodeID))
			must(leaf.store.Dispatch(ctx, task.TaskID, leaf.nodeID, leaf.nodeID))
			must(leaf.store.Accept(ctx, task.TaskID, leaf.nodeID))

			// The owner's dispatch and the executor's running copy are already
			// recorded. Leave them unlinked until after cancellation and board
			// removal so a real hello exercises delivery in place of a mock.
			_, err = root.CancelTree(ctx, task.TaskID)
			must(err)
			pending := func() int {
				t.Helper()
				var count int
				must(root.db.QueryRowContext(ctx, "SELECT count(*) FROM cancel_outbox WHERE task_id=?", task.TaskID).Scan(&count))
				return count
			}
			if count := pending(); count != 1 {
				t.Fatalf("cancel while disconnected parked %d rows, want 1", count)
			}
			if tc.remove != nil {
				must(tc.remove(ctx, root.store, task.TaskID))
				if _, err := root.store.Get(ctx, task.TaskID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("local task remained after removal: %v", err)
				}
			}
			if count := pending(); count != 1 {
				t.Fatalf("cancel custody after removal = %d, want 1", count)
			}
			leafAddr := startListen(t, ctx, leaf, "127.0.0.1:0")
			must(root.DialPeer(ctx, leafAddr))
			waitState(t, ctx, leaf.store, task.TaskID, StateCancelled)
			if count := pending(); count != 0 {
				t.Fatalf("delivered cancel still parked: %d rows", count)
			}
		})
	}
}
