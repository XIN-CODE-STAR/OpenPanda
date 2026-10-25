// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// startTestServer boots a Server on an OS-chosen loopback port and waits
// until it is accepting. It returns a cancel func, the ws:// URL carrying the
// port the kernel actually chose, and Listen's done channel.
//
// Tests must not pin a port: packages run in parallel, and several worktrees
// of this repository can share one machine, so a fixed port is a collision
// waiting for a busy day. Readiness is signalled by Ready(), never a guessed
// sleep — a 150ms sleep was the reason TestPingLoopSendsPings failed about
// once in ten runs under load.
func startTestServer(t *testing.T, handler func(*Conn)) (context.CancelFunc, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer("127.0.0.1:0", testLogger(), handler)
	done := make(chan error, 1)
	go func() { done <- srv.Listen(ctx) }()
	waitServerReady(t, srv, done)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server on %s did not stop", srv.Addr())
		}
	})
	return cancel, "ws://" + srv.Addr() + "/ws"
}

// waitServerReady blocks until srv is accepting, Listen returned an error, or
// the deadline expires. A failed bind never closes Ready, so the error channel
// is part of the wait rather than an afterthought.
func waitServerReady(t *testing.T, srv *Server, done <-chan error) {
	t.Helper()
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("server on %s never became ready", srv.Addr())
	}
}

// waitActiveConns polls until the server's live-connection count settles on
// want. The handler decrements asynchronously when it returns, so a fixed
// sleep after releasing a handler is the same race as a fixed sleep before
// dialing.
func waitActiveConns(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		got := srv.active
		srv.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active connections = %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServerAcceptsAndEchoes verifies an envelope sent by a raw client is
// received by the server and echoed back.
func TestServerAcceptsAndEchoes(t *testing.T) {
	got := make(chan Envelope, 1)
	cancel, url := startTestServer(t, func(conn *Conn) {
		var env Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		got <- env
		_ = conn.Send(env)
	})
	defer cancel()

	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	env, _ := NewEnvelope(MsgHello, "client-a", "m-1", HelloPayload{NodeID: "client-a", Ver: "t"})
	if err := ws.WriteJSON(env); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case gotEnv := <-got:
		if gotEnv.Type != MsgHello || gotEnv.From != "client-a" {
			t.Fatalf("server got unexpected envelope: %+v", gotEnv)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not receive envelope")
	}

	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var echoed Envelope
	if err := ws.ReadJSON(&echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if echoed.MsgID != "m-1" {
		t.Fatalf("echo mismatch: %+v", echoed)
	}
}

// TestServerEphemeralPortContract pins the API tests rely on: Addr() reports
// the configured address before the bind, the bound one afterwards, Ready()
// closes exactly then, and OnBound carries the chosen port to a caller that
// advertises itself (the Core's hello/discovery address).
func TestServerEphemeralPortContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer("127.0.0.1:0", testLogger(), func(*Conn) {})
	if got := srv.Addr(); got != "127.0.0.1:0" {
		t.Fatalf("Addr before bind = %q, want the configured address", got)
	}
	var onBound string
	srv.OnBound(func(addr string) { onBound = addr })

	done := make(chan error, 1)
	go func() { done <- srv.Listen(ctx) }()
	waitServerReady(t, srv, done)

	// Ready closed after the address was published and, because the kernel
	// already accepts on the bound socket, the dial below cannot race it.
	bound := srv.Addr()
	if bound == "127.0.0.1:0" || !strings.HasPrefix(bound, "127.0.0.1:") {
		t.Fatalf("Addr after bind = %q, want the chosen loopback port", bound)
	}
	if onBound != bound {
		t.Fatalf("OnBound saw %q, Addr reports %q", onBound, bound)
	}
	conn, err := net.DialTimeout("tcp", bound, time.Second)
	if err != nil {
		t.Fatalf("bound address is not accepting: %v", err)
	}
	_ = conn.Close()
}

// TestClientDialConnects verifies the Client dials a server and survives a
// hello round trip.
func TestClientDialConnects(t *testing.T) {
	got := make(chan Envelope, 1)
	cancel, url := startTestServer(t, func(conn *Conn) {
		var env Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		got <- env
	})
	defer cancel()

	client := NewClient(url, testLogger())
	conn, err := client.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	env, _ := NewEnvelope(MsgHello, "client-b", "m-2", HelloPayload{NodeID: "client-b"})
	if err := conn.Send(env); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case gotEnv := <-got:
		if gotEnv.From != "client-b" {
			t.Fatalf("client id mismatch: %+v", gotEnv)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not receive client envelope")
	}
}

// TestPingLoopSendsPings verifies StartPingLoop issues pings at the given
// cadence; the gorilla client auto-answers pongs, so the loop is the thing
// under test.
func TestPingLoopSendsPings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer("127.0.0.1:0", testLogger(), func(conn *Conn) {
		go conn.StartPingLoop(ctx, 20*time.Millisecond)
		var env Envelope
		for {
			if err := conn.ReadJSON(&env); err != nil {
				return
			}
		}
	})
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Listen(ctx) }()
	waitServerReady(t, srv, srvDone)

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr()+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	// Keep the connection alive for ~10 ping periods (~200ms) by sending
	// data; the ping loop must keep the read path functional throughout.
	for i := 0; i < 5; i++ {
		if err := ws.WriteJSON(Envelope{Type: MsgHello, MsgID: "ping-test"}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(40 * time.Millisecond)
	}

	// Final read: a clean timeout (no frame) is fine; a closed connection
	// means the ping loop failed to keep the peer alive.
	_ = ws.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err = ws.ReadMessage()
	if err != nil && !isReadTimeout(err) {
		t.Fatalf("connection closed during ping loop: %v", err)
	}
}

func isReadTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// TestServerDropsSlowHello verifies an inbound WebSocket connection that never
// sends a hello is closed within helloTimeout.
func TestServerDropsSlowHello(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer("127.0.0.1:0", testLogger(), func(conn *Conn) {
		var env Envelope
		for {
			if err := conn.ReadJSON(&env); err != nil {
				return
			}
		}
	})
	srv.helloTimeout = 500 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- srv.Listen(ctx) }()
	waitServerReady(t, srv, done)

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr()+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	start := time.Now()
	// Read without sending anything; the server should close us once
	// srv.helloTimeout expires.
	_ = ws.SetReadDeadline(time.Now().Add(srv.helloTimeout + 500*time.Millisecond))
	_, _, err = ws.ReadMessage()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected slow hello to be closed")
	}
	if elapsed < srv.helloTimeout-100*time.Millisecond {
		t.Fatalf("closed too fast: %v", elapsed)
	}
}

// TestServerEnforcesConnectionLimits verifies global and per-IP connection
// limits are applied before the WebSocket upgrade.
func TestServerEnforcesConnectionLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conns := make(chan *Conn, 4)
	// Use two separate channels instead of reassigning one variable: the handler
	// closure captures the variable by reference, so reassignment races with the
	// goroutine still reading the first channel.
	block1 := make(chan struct{})
	block2 := make(chan struct{})
	var block atomic.Value
	block.Store(block1)
	srv := NewServer("127.0.0.1:0", testLogger(), func(conn *Conn) {
		conns <- conn
		// Block until the test releases so connections count as active.
		<-block.Load().(chan struct{})
	})

	done := make(chan error, 1)
	go func() { done <- srv.Listen(ctx) }()
	waitServerReady(t, srv, done)
	url := "ws://" + srv.Addr() + "/ws"

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 2 * time.Second

	// Per-IP limit: allow only one connection from the same IP.
	srv.SetLimits(10, 1)

	ws1, resp, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("first dial: %v (status %d)", err, statusOf(resp))
	}
	defer ws1.Close()

	_, resp, err = dialer.Dial(url, nil)
	if err == nil {
		t.Fatalf("second same-IP dial should have been rejected")
	}
	if statusOf(resp) != http.StatusServiceUnavailable {
		t.Fatalf("second same-IP status = %d, want 503", statusOf(resp))
	}

	// Release the first connection and reset limits for the global-limit check.
	// With global=2 and per-IP=10, three same-IP connections should hit the
	// global limit on the third.
	close(block1)
	// Wait for the handler goroutine to unblock and decrement counters.
	waitActiveConns(t, srv, 0)

	block.Store(block2)
	srv.SetLimits(2, 10)

	ws2, resp, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("global-limit first dial: %v (status %d)", err, statusOf(resp))
	}
	defer ws2.Close()

	ws3, resp, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("global-limit second dial: %v (status %d)", err, statusOf(resp))
	}
	defer ws3.Close()

	_, resp, err = dialer.Dial(url, nil)
	if err == nil {
		t.Fatalf("third dial should have been rejected by global limit")
	}
	if statusOf(resp) != http.StatusServiceUnavailable {
		t.Fatalf("third dial status = %d, want 503", statusOf(resp))
	}
	close(block2)
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// TestDataFrameRoundTrip covers the CapBinaryData wire form end to end: a
// chunk written by SendData must arrive with its header fields intact and
// its body in Envelope.BinaryPayload, at byte-exact size (no base64).
func TestDataFrameRoundTrip(t *testing.T) {
	got := make(chan Envelope, 1)
	cancel, url := startTestServer(t, func(conn *Conn) {
		var env Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		got <- env
	})
	defer cancel()

	client, err := NewClient(url, testLogger()).Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	body := make([]byte, ArtifactChunkBytes)
	for i := range body {
		body[i] = byte(i)
	}
	head := ArtifactChunkPayload{TaskID: "t-1", Hash: "abc", Offset: 7 << 20, Total: 9 << 20, OK: true}
	env, err := NewEnvelope(MsgArtifactChunk, "node-a", "m-data", head)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if err := client.SendData(env, body); err != nil {
		t.Fatalf("send data frame: %v", err)
	}

	select {
	case got := <-got:
		if got.Type != MsgArtifactChunk || got.From != "node-a" {
			t.Fatalf("header = %s from %s", got.Type, got.From)
		}
		var cp ArtifactChunkPayload
		if err := got.PayloadInto(&cp); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if cp.Offset != 7<<20 || cp.Total != 9<<20 || !cp.OK {
			t.Fatalf("chunk header fields = %+v", cp)
		}
		if len(cp.Data) != 0 {
			t.Fatalf("json data field should be empty on a data frame, got %d bytes", len(cp.Data))
		}
		if len(got.BinaryPayload) != len(body) || got.BinaryPayload[123] != body[123] {
			t.Fatalf("binary body = %d bytes", len(got.BinaryPayload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("data frame never arrived")
	}
}
