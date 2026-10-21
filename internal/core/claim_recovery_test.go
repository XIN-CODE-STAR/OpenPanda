// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"testing"
)

// TestClaimSurvivesTaskRecovery pins the queue claim against task recovery.
//
// The queue scheduler's claim→run window is one goroutine (ClaimLocal →
// runScheduled → Accept). Task recovery can land inside that window: its
// dispatched → queued branch moves the row back under the runner, and the
// runner's run() refuses a row that is not in a runnable state — "cannot run
// task in state queued" — so the task dies instead of running.
//
// It was not a theoretical interleaving: the daemon started the queue
// scheduler before it ran recovery, so the scheduler's first pass could
// claim a queued row microseconds before recovery requeued it, and every
// task that had been queued while the daemon was down was lost on the next
// start.
//
// The runner now reclaims its own claim (the row is still owned by this node
// and the scheduler's registry still holds its resource keys), so the task
// runs. This test drives the exact interleaving deterministically.
func TestClaimSurvivesTaskRecovery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := createTask(t, s, "", "queued before the restart", "node")
	if err := s.Queue(ctx, task.TaskID, "node"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	// The queue scheduler claims it.
	if err := s.ClaimLocal(ctx, task.TaskID, "node"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Task recovery runs inside the claim→run window and requeues the row.
	if _, err := s.Recover(ctx, "node"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	after, err := s.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateQueued {
		t.Fatalf("precondition: state = %s, want %s (recovery must have requeued it)",
			after.State, StateQueued)
	}

	// The runner reclaims its own claim before running.
	if err := s.ReclaimClaimed(ctx, task.TaskID, "node"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	reclaimed, err := s.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.State != StateDispatched {
		t.Fatalf("state after reclaim = %s, want %s — the runner would refuse the row",
			reclaimed.State, StateDispatched)
	}
}

// TestReclaimClaimedLeavesOtherOwnersAlone pins the reclaim's guard: a row
// this node does not own must never be moved back to dispatched, or the
// reclaim would steal a row another node is running.
func TestReclaimClaimedLeavesOtherOwnersAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := createTask(t, s, "", "owned by another node", "other-node")
	if err := s.Queue(ctx, task.TaskID, "other-node"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := s.ClaimLocal(ctx, task.TaskID, "other-node"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Recover(ctx, "other-node"); err != nil {
		t.Fatalf("recover: %v", err)
	}

	err := s.ReclaimClaimed(ctx, task.TaskID, "this-node")
	if err == nil {
		t.Fatal("reclaim succeeded on a row owned by another node")
	}
	if !strings.Contains(err.Error(), "conflict") && !strings.Contains(err.Error(), "not queued") {
		t.Fatalf("unexpected error: %v", err)
	}
	after, err := s.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State == StateDispatched {
		t.Fatalf("the row was reclaimed by a node that does not own it (owner=%s)", after.OwnerNode)
	}
}
