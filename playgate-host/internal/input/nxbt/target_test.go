package nxbt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// makePipeTarget builds a Target backed by an in-memory net.Pipe() connection.
// It returns the Target, the server-side Conn (simulating the daemon), and a
// cancel function.  The Target's Run goroutine is started automatically; call
// cancel() to shut it down.
func makePipeTarget(t *testing.T, rateHz int) (*Target, net.Conn, context.CancelFunc) {
	t.Helper()
	clientConn, serverConn := net.Pipe()

	dialCalled := false
	dialFn := func(ctx context.Context, _ string) (net.Conn, error) {
		if dialCalled {
			// Block until ctx is done so reconnect loops don't spin.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		dialCalled = true
		return clientConn, nil
	}

	target := New(
		testLogger(t),
		"/tmp/test.sock",
		WithRateHz(rateHz),
		WithDialFunc(dialFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// Drain status channel to unblock Run if needed.
		for range target.Status() {
		}
	})
	go func() {
		_ = target.Run(ctx)
	}()

	return target, serverConn, cancel
}

// testLogger returns a slog.Logger that logs to t.Log.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, nil))
}

// testWriter adapts testing.T to io.Writer so slog can write to it.
type testWriter struct{ t *testing.T }

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

// readLine reads one newline-terminated JSON line from conn with a generous
// deadline.  It fails the test if nothing arrives in time.
func readLine(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("readLine: connection closed or timeout: %v", scanner.Err())
	}
	return scanner.Bytes()
}

// writeLine sends a newline-terminated JSON line to conn.
func writeLine(t *testing.T, conn net.Conn, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("writeLine marshal: %v", err)
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("writeLine write: %v", err)
	}
}

// ---- tests ------------------------------------------------------------------

func TestProtocol_InputMessageShape(t *testing.T) {
	target, serverConn, cancel := makePipeTarget(t, 0 /* unlimited */)
	defer cancel()
	defer serverConn.Close()

	// Give Run a moment to establish the session.
	time.Sleep(50 * time.Millisecond)

	cmd := core.InputCommand{
		Buttons: core.ButtonA | core.ButtonB,
		LX:      0.5,
		LY:      -0.5,
		RX:      0.0,
		RY:      1.0,
	}
	if err := target.Send(cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Poll briefly for the coalescer to flush.
	time.Sleep(20 * time.Millisecond)

	raw := readLine(t, serverConn)
	var got inputMsg
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v — raw: %s", err, raw)
	}
	if got.Type != msgTypeInput {
		t.Errorf("type = %q, want %q", got.Type, msgTypeInput)
	}
	if got.Buttons != cmd.Buttons {
		t.Errorf("buttons = %d, want %d", got.Buttons, cmd.Buttons)
	}
	if got.LX != cmd.LX {
		t.Errorf("lx = %v, want %v", got.LX, cmd.LX)
	}
	if got.LY != -cmd.LY {
		t.Errorf("ly = %v, want %v", got.LY, -cmd.LY)
	}
	if got.RY != -cmd.RY {
		t.Errorf("ry = %v, want %v", got.RY, -cmd.RY)
	}
}

func TestProtocol_PingPong(t *testing.T) {
	_, serverConn, cancel := makePipeTarget(t, 0)
	defer cancel()
	defer serverConn.Close()

	time.Sleep(50 * time.Millisecond)

	// The daemon echoes pong back.
	writeLine(t, serverConn, pongMsg{Type: msgTypePong})

	// Just verifying no panic; a more rigorous check would inspect logs.
	time.Sleep(20 * time.Millisecond)
}

func TestProtocol_StatusForwarding(t *testing.T) {
	target, serverConn, cancel := makePipeTarget(t, 0)
	defer cancel()
	defer serverConn.Close()

	// Drain the initial "connecting" status.
	drainStatus := func() {
		for {
			select {
			case <-target.Status():
			default:
				return
			}
		}
	}

	time.Sleep(50 * time.Millisecond)
	drainStatus()

	// Daemon sends "connected".
	writeLine(t, serverConn, statusMsg{
		Type:  msgTypeStatus,
		State: wireStateConnected,
	})

	// Wait for it to arrive.
	select {
	case s := <-target.Status():
		if s != core.TargetStatusConnected {
			t.Errorf("status = %v, want TargetStatusConnected", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for status update")
	}
}

func TestRateLimit_CoalescesBeyondCap(t *testing.T) {
	// 10 Hz cap: 100 ms between emissions.
	target, serverConn, cancel := makePipeTarget(t, 10)
	defer cancel()
	defer serverConn.Close()

	time.Sleep(50 * time.Millisecond)

	// Send a burst of 20 commands well within 1 ms.
	for i := range 20 {
		_ = target.Send(core.InputCommand{Buttons: uint32(i + 1)})
	}

	// Within the first 100 ms only ~1 command should pass through.
	serverConn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	scanner := bufio.NewScanner(serverConn)
	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		var env envelope
		if json.Unmarshal(line, &env) == nil && env.Type == msgTypeInput {
			count++
		}
	}
	// At 10 Hz cap we expect at most 2 commands in 80 ms (1 initial flush +
	// possibly 1 more at t≈100 ms, but we cut off at 80 ms so likely 1).
	// We allow ≤3 to tolerate timing jitter in CI.
	if count > 3 {
		t.Errorf("rate limiter too permissive: got %d commands in 80ms at 10Hz cap", count)
	}
	if count == 0 {
		t.Error("rate limiter too aggressive: got 0 commands in 80ms at 10Hz cap")
	}
}

func TestRateLimit_NoLimitPassesAll(t *testing.T) {
	// 0 Hz = no rate limit. Send commands spaced further apart than the poll
	// tick so each one gets its own flush cycle and none are coalesced.
	// A concurrent goroutine drains the server side so net.Pipe writes don't
	// block the Run goroutine.
	target, serverConn, cancel := makePipeTarget(t, 0)
	defer cancel()
	defer serverConn.Close()

	time.Sleep(50 * time.Millisecond)

	// Collect received input messages in the background.
	received := make(chan struct{}, 32)
	go func() {
		scanner := bufio.NewScanner(serverConn)
		for scanner.Scan() {
			line := scanner.Bytes()
			var env envelope
			if json.Unmarshal(line, &env) == nil && env.Type == msgTypeInput {
				received <- struct{}{}
			}
		}
	}()

	const N = 5
	// Space sends by 3× the poll interval so each command gets its own tick.
	const spacing = sendPollInterval * 3
	for i := range N {
		_ = target.Send(core.InputCommand{Buttons: uint32(i + 1)})
		time.Sleep(spacing)
	}

	// Wait for all N messages to arrive, with a generous deadline.
	deadline := time.After(3 * time.Second)
	count := 0
	for count < N {
		select {
		case <-received:
			count++
		case <-deadline:
			t.Errorf("no-limit: want ≥%d commands, got %d", N, count)
			return
		}
	}
}

func TestReconnect_ReconnectsAfterDisconnect(t *testing.T) {
	// serverSides receives the server half of each net.Pipe the dialFn creates.
	serverSides := make(chan net.Conn, 4)

	dialFn := func(ctx context.Context, _ string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		serverSides <- serverConn
		return clientConn, nil
	}

	target := New(
		slog.Default(),
		"/tmp/test.sock",
		WithRateHz(0),
		WithDialFunc(dialFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = target.Run(ctx) }()

	// Grab first server-side conn.
	var firstServer net.Conn
	select {
	case firstServer = <-serverSides:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for first dial")
	}

	// Force-close the first connection to trigger reconnect.
	firstServer.Close()

	// Wait for second dial (reconnect back-off starts at 500 ms but we give 5 s).
	select {
	case secondServer := <-serverSides:
		// Success: Target reconnected. Clean up.
		secondServer.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for reconnect after disconnect")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	var _ core.InputTarget = (*Target)(nil)
	var _ core.Module = (*Target)(nil)
}
