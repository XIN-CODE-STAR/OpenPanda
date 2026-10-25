// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/ledger"
)

// P1-5 regression tests: a declined task is re-routed to the next-best node
// instead of failing outright, and a node that already declined is never
// offered the task again (loop bound).

// TestDeclineReroutesToAlternateNode wires root → {leafA, leafB}, both
// advertising gpio:read. leafA's single execution slot is occupied, so the
// preferred delegation to leafA is declined (capacity full); the root must
// re-route to leafB, which executes. The root's copy ends done with the real
// output — the decline is absorbed, not propagated.
func TestDeclineReroutesToAlternateNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	root := newCore(t, "root", "127.0.0.1:17931")

	mkLeaf := func(id, addr string, maxConcurrent int) *Core {
		db := openTestDB(t)
		card := ledger.Card{
			Device:        id,
			ResourceClass: "Standard",
			Native: []ledger.NativeAbility{
				{ID: "gpio:read", Command: "echo", Args: []string{"gpio-ok"}},
			},
			Capacity: ledger.Capacity{CPUCores: 2, RAMGB: 4, MaxConcurrent: maxConcurrent},
		}
		c := NewCore(db, id, card, 5, testLogger(), config.ModelConfig{})
		c.SetWorkDir(t.TempDir())
		c.SetSharedSecret(testSharedSecret)
		return c
	}
	leafA := mkLeaf("leaf-a", "127.0.0.1:17932", 1)
	leafB := mkLeaf("leaf-b", "127.0.0.1:17933", 3)

	// Occupy leafA's only slot so it declines on capacity.
	busy, err := leafA.store.Create(ctx, "", "", "busy", "leaf-a", []string{"leaf-a"})
	if err != nil {
		t.Fatalf("create busy: %v", err)
	}
	if err := leafA.store.Queue(ctx, busy.TaskID, "leaf-a"); err != nil {
		t.Fatalf("queue busy: %v", err)
	}
	if err := leafA.store.Dispatch(ctx, busy.TaskID, "leaf-a", "leaf-a"); err != nil {
		t.Fatalf("dispatch busy: %v", err)
	}
	if err := leafA.store.Accept(ctx, busy.TaskID, "leaf-a"); err != nil {
		t.Fatalf("accept busy: %v", err)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("startup: %v", err)
		}
	}
	must(root.Register(ctx))
	must(leafA.Register(ctx))
	must(leafB.Register(ctx))
	startListen(t, ctx, root, "127.0.0.1:17931")
	startListen(t, ctx, leafA, "127.0.0.1:17932")
	startListen(t, ctx, leafB, "127.0.0.1:17933")
	must(root.DialPeer(ctx, "127.0.0.1:17932"))
	must(root.DialPeer(ctx, "127.0.0.1:17933"))
	time.Sleep(300 * time.Millisecond)

	// Preferred node forces the first attempt onto the saturated leafA; without
	// a preference the DCPS score would pick the idle leafB outright and the
	// re-route path would never run.
	in := TaskInput{
		Title: "read gpio", Intent: "read gpio",
		Requires: []string{"gpio:read"}, PreferredNode: "leaf-a",
	}
	task, result, err := root.Submit(ctx, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !result.OK {
		t.Fatalf("result not ok: %+v", result)
	}
	if task.State != StateDone {
		t.Fatalf("root task state = %s, want done (re-routed)", task.State)
	}
	if !strings.Contains(result.Stdout, "gpio-ok") {
		t.Fatalf("result stdout = %q, want gpio-ok", result.Stdout)
	}

	// The audit trail records leafA's decline, which is what excluded it from
	// the second routing pass.
	decliners, err := root.store.DeclinedBy(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("declined-by: %v", err)
	}
	if len(decliners) != 1 || decliners[0] != "leaf-a" {
		t.Fatalf("declined-by = %v, want [leaf-a]", decliners)
	}

	// leafA's copy was terminalized (declined), leafB's copy completed.
	if tk, err := leafA.store.Get(ctx, task.TaskID); err != nil || tk.State != StateCancelled {
		t.Fatalf("leafA task = %v (err %v), want cancelled", tk, err)
	}
	if tk, err := leafB.store.Get(ctx, task.TaskID); err != nil || tk.State != StateDone {
		t.Fatalf("leafB task = %v (err %v), want done", tk, err)
	}
}

// TestDeclineRerouteExhaustedPropagates verifies the failure path: when every
// capable node has declined, the root gives up — the waiting Submit sees a
// failed result instead of hanging.
func TestDeclineRerouteExhaustedPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	root := newCore(t, "root", "127.0.0.1:17941")

	db := openTestDB(t)
	card := ledger.Card{
		Device:        "only-leaf",
		ResourceClass: "Standard",
		Native: []ledger.NativeAbility{
			{ID: "gpio:read", Command: "echo", Args: []string{"gpio-ok"}},
		},
		Capacity: ledger.Capacity{CPUCores: 2, RAMGB: 4, MaxConcurrent: 1},
	}
	leaf := NewCore(db, "only-leaf", card, 5, testLogger(), config.ModelConfig{})
	leaf.SetWorkDir(t.TempDir())
	leaf.SetSharedSecret(testSharedSecret)

	// Saturate the only capable node.
	busy, err := leaf.store.Create(ctx, "", "", "busy", "only-leaf", []string{"only-leaf"})
	if err != nil {
		t.Fatalf("create busy: %v", err)
	}
	if err := leaf.store.Queue(ctx, busy.TaskID, "only-leaf"); err != nil {
		t.Fatalf("queue busy: %v", err)
	}
	if err := leaf.store.Dispatch(ctx, busy.TaskID, "only-leaf", "only-leaf"); err != nil {
		t.Fatalf("dispatch busy: %v", err)
	}
	if err := leaf.store.Accept(ctx, busy.TaskID, "only-leaf"); err != nil {
		t.Fatalf("accept busy: %v", err)
	}

	if err := root.Register(ctx); err != nil {
		t.Fatalf("register root: %v", err)
	}
	if err := leaf.Register(ctx); err != nil {
		t.Fatalf("register leaf: %v", err)
	}
	startListen(t, ctx, root, "127.0.0.1:17941")
	startListen(t, ctx, leaf, "127.0.0.1:17942")
	if err := root.DialPeer(ctx, "127.0.0.1:17942"); err != nil {
		t.Fatalf("dial: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	in := TaskInput{
		Title: "read gpio", Intent: "read gpio",
		Requires: []string{"gpio:read"}, PreferredNode: "only-leaf",
	}
	_, result, err := root.Submit(ctx, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.OK {
		t.Fatalf("result ok, want failure after exhaustion")
	}
	if !strings.Contains(result.Stderr, "declined") {
		t.Fatalf("stderr = %q, want decline reason", result.Stderr)
	}
}

// TestDeclineReroutesToLocalNode: when the preferred peer declines and this
// node can run the task itself, the local copy must execute. The old path
// only accepted ActionForward, so a capable origin reported failure (and a
// synchronous Submit waited on a row nothing would ever run).
func TestDeclineReroutesToLocalNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mk := func(id, addr string, maxConcurrent int) *Core {
		db := openTestDB(t)
		card := ledger.Card{
			Device:        id,
			ResourceClass: "Standard",
			Native: []ledger.NativeAbility{
				{ID: "gpio:read", Command: "echo", Args: []string{"gpio-local"}},
			},
			Capacity: ledger.Capacity{CPUCores: 2, RAMGB: 4, MaxConcurrent: maxConcurrent},
		}
		c := NewCore(db, id, card, 5, testLogger(), config.ModelConfig{})
		c.SetWorkDir(t.TempDir())
		c.SetSharedSecret(testSharedSecret)
		return c
	}
	root := mk("root-local", "127.0.0.1:17951", 2)
	leaf := mk("leaf-busy", "127.0.0.1:17952", 1)

	// Saturate the leaf so it declines on capacity.
	busy, err := leaf.store.Create(ctx, "", "", "busy", "leaf-busy", []string{"leaf-busy"})
	if err != nil {
		t.Fatalf("create busy: %v", err)
	}
	for _, step := range []func() error{
		func() error { return leaf.store.Queue(ctx, busy.TaskID, "leaf-busy") },
		func() error { return leaf.store.Dispatch(ctx, busy.TaskID, "leaf-busy", "leaf-busy") },
		func() error { return leaf.store.Accept(ctx, busy.TaskID, "leaf-busy") },
	} {
		if err := step(); err != nil {
			t.Fatalf("saturate leaf: %v", err)
		}
	}

	for _, c := range []*Core{root, leaf} {
		if err := c.Register(ctx); err != nil {
			t.Fatalf("register %s: %v", c.nodeID, err)
		}
	}
	startListen(t, ctx, root, "127.0.0.1:17951")
	startListen(t, ctx, leaf, "127.0.0.1:17952")
	if err := root.DialPeer(ctx, "127.0.0.1:17952"); err != nil {
		t.Fatalf("dial: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	task, result, err := root.Submit(ctx, TaskInput{
		Title: "read gpio locally", Intent: "read gpio",
		Requires: []string{"gpio:read"}, PreferredNode: "leaf-busy",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !result.OK {
		t.Fatalf("result not ok after decline: %+v", result)
	}
	if task.State != StateDone {
		t.Fatalf("task state = %s, want done (ran locally after decline)", task.State)
	}
	if !strings.Contains(result.Stdout, "gpio-local") {
		t.Fatalf("stdout = %q, want the local executor's output", result.Stdout)
	}
}
