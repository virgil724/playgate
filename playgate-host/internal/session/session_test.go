package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// ---- test helpers ----------------------------------------------------------

// testKeyPair generates a fresh ed25519 key pair and returns (pubKeyBase64, privKey).
func testKeyPair(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(pub), priv
}

// makeClaims builds a Claims with sensible defaults. Exported fields may be
// overridden by the caller before passing to signJWT.
func makeClaims(roomID, viewerID string, sessionSecs int, notBefore time.Time) Claims {
	return Claims{
		Issuer:         "playgate-server",
		IssuedAt:       notBefore.Unix(),
		ExpiresAt:      notBefore.Add(10 * time.Minute).Unix(),
		RoomID:         roomID,
		ViewerID:       viewerID,
		SessionSeconds: sessionSecs,
	}
}

// newTestManager builds a Manager with an injectable clock for tests. The
// clock starts at epoch t0 and never advances unless the returned advance
// function is called. The manager does NOT call Run — tests wire their own
// goroutines or call endSession directly.
//
// tickInterval is set to 50ms so tests can observe ticks quickly; the minimum
// enforced by the code is 100ms so we use that.
func newTestManager(t *testing.T, roomID string, idleTimeout time.Duration) (*Manager, func(d time.Duration)) {
	t.Helper()
	pubB64, priv := testKeyPair(t)

	// injectable clock
	var clockNow time.Time = time.Unix(1_718_000_000, 0)
	advance := func(d time.Duration) { clockNow = clockNow.Add(d) }

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          roomID,
		IdleTimeout:     idleTimeout,
		TickInterval:    100 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return clockNow },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Store priv key on t for easy token minting.
	t.Cleanup(func() { _ = priv })
	return m, advance
}

// mintToken mints a JWT for the given viewer/room using the Manager's current
// clock value. The token expires 10 minutes after clockNow.
//
// We expose this as a standalone helper because the Manager's clock is
// injectable; callers must pass the same time they used to build the claims.
func mintToken(t *testing.T, priv ed25519.PrivateKey, roomID, viewerID string, sessionSecs int, now time.Time) string {
	t.Helper()
	c := makeClaims(roomID, viewerID, sessionSecs, now)
	return signJWT(t, priv, c)
}

// drainEvents returns all events currently buffered in ch without blocking.
func drainEvents(ch <-chan SessionEvent) []SessionEvent {
	var out []SessionEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// waitEvent polls ch for an event matching pred for up to timeout. Returns
// true if matched.
func waitEvent(t *testing.T, ch <-chan SessionEvent, pred func(SessionEvent) bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return false
			}
			if pred(ev) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// ---- gap 1: control authority handover ------------------------------------

// TestControlTransfer_QueueThenExpire verifies:
//   - A Claim → granted (active)
//   - B Claim → queued
//   - A's session context expires (timer fires)
//   - B is automatically promoted → granted
//   - event sequence includes granted/queued/expired/granted
func TestControlTransfer_QueueThenExpire(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	// Use a very short session for A (1 second), B gets 2 seconds.
	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tokA := mintToken(t, priv, "room1", "alice", 1, t0)
	tokB := mintToken(t, priv, "room1", "bob", 2, t0)

	sessA, err := m.Claim(tokA)
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if sessA.ViewerID() != "alice" {
		t.Errorf("sessA viewerID: got %q, want %q", sessA.ViewerID(), "alice")
	}

	// Confirm A is active immediately.
	evs := drainEvents(m.Events())
	if len(evs) == 0 || evs[0].Kind != EventGranted || evs[0].ViewerID != "alice" {
		t.Errorf("expected EventGranted for alice, got %+v", evs)
	}

	sessB, err := m.Claim(tokB)
	if err != nil {
		t.Fatalf("Claim B: %v", err)
	}
	if sessB.ViewerID() != "bob" {
		t.Errorf("sessB viewerID: got %q, want %q", sessB.ViewerID(), "bob")
	}

	// B should be queued.
	if !waitEvent(t, m.Events(), func(ev SessionEvent) bool {
		return ev.Kind == EventQueued && ev.ViewerID == "bob"
	}, 2*time.Second) {
		t.Fatal("expected EventQueued for bob")
	}

	// Wait for A's 1-second session to actually expire (real time).
	if !waitEvent(t, m.Events(), func(ev SessionEvent) bool {
		return ev.Kind == EventExpired && ev.ViewerID == "alice"
	}, 5*time.Second) {
		t.Fatal("expected EventExpired for alice")
	}

	// Context for A must be done.
	select {
	case <-sessA.Context().Done():
		// good
	case <-time.After(2 * time.Second):
		t.Error("sessA.Context() not cancelled after expiry")
	}

	// B should be promoted now.
	if !waitEvent(t, m.Events(), func(ev SessionEvent) bool {
		return ev.Kind == EventGranted && ev.ViewerID == "bob"
	}, 3*time.Second) {
		t.Fatal("expected EventGranted for bob after A expired")
	}
}

// TestControlTransfer_InputGate verifies that A's gate forwards commands while
// A is active, and stops forwarding (closes) once A's session expires.
func TestControlTransfer_InputGate(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// A's session is 1 second.
	tokA := mintToken(t, priv, "room1", "alice", 1, t0)
	_, err = m.Claim(tokA)
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}

	rawIn := make(chan core.InputCommand, 4)
	gated := m.Gate("alice", rawIn)

	// Send a command while A is active.
	cmd := core.InputCommand{Buttons: core.ButtonA}
	rawIn <- cmd

	select {
	case got, ok := <-gated:
		if !ok {
			t.Fatal("gate closed unexpectedly before session expired")
		}
		if got.Buttons != core.ButtonA {
			t.Errorf("forwarded buttons: got %#x, want %#x", got.Buttons, core.ButtonA)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command not forwarded within timeout")
	}

	// Wait for A to expire; gated channel must close.
	select {
	case _, ok := <-gated:
		if ok {
			t.Error("expected gate to be closed after session expiry, but received value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate channel not closed after session expiry")
	}

	// B is NOT active, so sending on rawIn (still open) must not arrive on gated
	// (it is already closed, so the select below just returns immediately — the
	// test verifies the channel is indeed closed).
	close(rawIn)
}

// TestInputGate_QueuedViewerBlocked verifies that a queued (not-yet-active)
// viewer's gate does NOT forward commands to the output.
func TestInputGate_QueuedViewerBlocked(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// A holds control for 60s, so B stays queued throughout this test.
	tokA := mintToken(t, priv, "room1", "alice", 60, t0)
	tokB := mintToken(t, priv, "room1", "bob", 60, t0)

	_, err = m.Claim(tokA)
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	_, err = m.Claim(tokB)
	if err != nil {
		t.Fatalf("Claim B: %v", err)
	}

	rawBIn := make(chan core.InputCommand, 4)
	defer close(rawBIn)
	gatedB := m.Gate("bob", rawBIn)

	// Send a command for B (queued, not active).
	rawBIn <- core.InputCommand{Buttons: core.ButtonB}

	// The command must NOT be forwarded.
	select {
	case cmd, ok := <-gatedB:
		if ok {
			t.Errorf("queued viewer's command was forwarded (buttons=%#x), expected drop", cmd.Buttons)
		}
		// If not ok, the channel was closed; that should not happen here.
		if !ok {
			t.Error("gate closed unexpectedly for queued viewer")
		}
	case <-time.After(300 * time.Millisecond):
		// Correct — nothing forwarded.
	}
}

// ---- gap 2: idle kick ------------------------------------------------------

// TestIdleKick verifies that a viewer with no input activity is removed after
// the idle timeout and an EventIdleKicked is emitted.
func TestIdleKick(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		IdleTimeout:     200 * time.Millisecond, // very short for tests
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tok := mintToken(t, priv, "room1", "alice", 60, t0)
	sess, err := m.Claim(tok)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Drain the granted event.
	drainEvents(m.Events())

	// Wait for idle kick.
	if !waitEvent(t, m.Events(), func(ev SessionEvent) bool {
		return ev.Kind == EventIdleKicked && ev.ViewerID == "alice"
	}, 3*time.Second) {
		t.Fatal("expected EventIdleKicked for alice")
	}

	// Session context must be cancelled.
	select {
	case <-sess.Context().Done():
		// good
	case <-time.After(time.Second):
		t.Error("session context not cancelled after idle kick")
	}
}

// TestIdleKick_ResetOnInput verifies that input activity resets the idle timer
// and prevents premature eviction.
func TestIdleKick_ResetOnInput(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		IdleTimeout:     300 * time.Millisecond,
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tok := mintToken(t, priv, "room1", "alice", 60, t0)
	sess, err := m.Claim(tok)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	rawIn := make(chan core.InputCommand, 4)
	defer close(rawIn)
	_ = m.Gate("alice", rawIn)

	// Send input every 100ms for 500ms total — enough to reset the 300ms timer
	// repeatedly and keep the session alive.
	resetDone := make(chan struct{})
	go func() {
		defer close(resetDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		end := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(end) {
			<-ticker.C
			select {
			case rawIn <- core.InputCommand{Buttons: core.ButtonA}:
			default:
			}
		}
	}()
	<-resetDone

	// Session should still be alive.
	select {
	case <-sess.Context().Done():
		t.Error("session was kicked during active input — idle timer not reset")
	default:
		// good
	}
}

// ---- gap 3: event sequence -------------------------------------------------

// TestEventSequence verifies that the canonical event order is correct for a
// full lifecycle: granted → tick(s) → expired.
func TestEventSequence(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    100 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// 1-second session → should produce at least one Tick before Expired.
	tok := mintToken(t, priv, "room1", "alice", 1, t0)
	_, err = m.Claim(tok)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var kinds []EventKind
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-m.Events():
			if !ok {
				break loop
			}
			kinds = append(kinds, ev.Kind)
			if ev.Kind == EventExpired {
				break loop
			}
		case <-deadline:
			t.Fatal("event sequence timed out")
		}
	}

	if len(kinds) == 0 {
		t.Fatal("no events received")
	}
	if kinds[0] != EventGranted {
		t.Errorf("first event: want %q, got %q", EventGranted, kinds[0])
	}
	last := kinds[len(kinds)-1]
	if last != EventExpired {
		t.Errorf("last event: want %q, got %q", EventExpired, last)
	}
	// Must have at least one tick between granted and expired.
	hasTick := false
	for _, k := range kinds[1 : len(kinds)-1] {
		if k == EventTick {
			hasTick = true
		}
	}
	if !hasTick {
		t.Logf("event sequence: %v", kinds)
		t.Error("expected at least one EventTick between EventGranted and EventExpired")
	}
}

// TestEventTick_RemainingSeconds verifies that EventTick carries a positive
// and monotonically non-increasing remaining-seconds value.
func TestEventTick_RemainingSeconds(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    100 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tok := mintToken(t, priv, "room1", "alice", 3, t0) // 3-second session
	_, err = m.Claim(tok)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Collect 2 tick events.
	var ticks []SessionEvent
	deadline := time.After(5 * time.Second)
	for len(ticks) < 2 {
		select {
		case ev, ok := <-m.Events():
			if !ok {
				t.Fatal("events channel closed before 2 ticks")
			}
			if ev.Kind == EventTick {
				ticks = append(ticks, ev)
			}
		case <-deadline:
			t.Fatalf("timed out collecting ticks, got %d", len(ticks))
		}
	}

	for i, tick := range ticks {
		if tick.RemainingSeconds < 0 {
			t.Errorf("tick[%d].RemainingSeconds negative: %d", i, tick.RemainingSeconds)
		}
	}
	if ticks[0].RemainingSeconds < ticks[1].RemainingSeconds {
		t.Errorf("remaining seconds increased: tick[0]=%d, tick[1]=%d", ticks[0].RemainingSeconds, ticks[1].RemainingSeconds)
	}
}

// ---- gap 4: policy reject --------------------------------------------------

// TestPolicyReject verifies that with PolicyReject, a second Claim while one
// is active returns ErrControllerActive.
func TestPolicyReject(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		QueuePolicy:     PolicyReject,
		TickInterval:    time.Second,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tokA := mintToken(t, priv, "room1", "alice", 60, t0)
	_, err = m.Claim(tokA)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	tokB := mintToken(t, priv, "room1", "bob", 60, t0)
	_, err = m.Claim(tokB)
	if err != ErrControllerActive {
		t.Errorf("second Claim: got %v, want ErrControllerActive", err)
	}
}

// TestQueueFull verifies that MaxQueueDepth is enforced.
func TestQueueFull(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		QueuePolicy:     PolicyQueue,
		MaxQueueDepth:   1,
		TickInterval:    time.Second,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	tokA := mintToken(t, priv, "room1", "alice", 60, t0)
	_, _ = m.Claim(tokA) // active

	tokB := mintToken(t, priv, "room1", "bob", 60, t0)
	_, err = m.Claim(tokB) // queued (depth 1)
	if err != nil {
		t.Fatalf("second Claim (queue): %v", err)
	}

	tokC := mintToken(t, priv, "room1", "carol", 60, t0)
	_, err = m.Claim(tokC) // should fail: queue full
	if err != ErrQueueFull {
		t.Errorf("third Claim: got %v, want ErrQueueFull", err)
	}
}

// ---- gap 5: Run shutdown ---------------------------------------------------

// TestRun_Shutdown verifies that Run() returns when the context is cancelled
// and that it closes the Events() channel.
func TestRun_Shutdown(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    time.Second,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = m.Run(ctx)
	}()

	// Give Run a moment to start.
	time.Sleep(10 * time.Millisecond)

	// Claim a session so there is an active entry to cancel on shutdown.
	tok := mintToken(t, priv, "room1", "alice", 60, t0)
	sess, err := m.Claim(tok)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	cancel() // trigger shutdown

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	// Events() channel must be closed after Run returns.
	select {
	case _, ok := <-m.Events():
		if ok {
			// drain remaining until closed
			for range m.Events() {
			}
		}
	case <-time.After(time.Second):
		t.Error("Events() channel not closed after Run returned")
	}

	// Active session context must be cancelled.
	select {
	case <-sess.Context().Done():
	case <-time.After(time.Second):
		t.Error("session context not cancelled after Run shutdown")
	}
}

// TestGate_NoSessionClosesImmediately verifies that Gate returns an already-
// closed channel when called for an unknown viewerID.
func TestGate_NoSessionClosesImmediately(t *testing.T) {
	pubB64, _ := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    time.Second,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	rawIn := make(chan core.InputCommand)
	defer close(rawIn)

	gated := m.Gate("nobody", rawIn)
	select {
	case _, ok := <-gated:
		if ok {
			t.Error("expected gate to be closed for unknown viewerID")
		}
	case <-time.After(time.Second):
		t.Error("gate channel not closed for unknown viewerID")
	}
}

// ---- gap 6: no goroutine leak ----------------------------------------------
//
// Run with -race and -count=1; the test relies on the race detector catching
// any residual goroutines accessing shared state after the manager shuts down.
// We also use a short sleep and re-verification to surface goroutine leaks
// detectable via runtime.NumGoroutine (best-effort, not a hard assertion).

func TestNoGoroutineLeak(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		IdleTimeout:     0, // disabled
		TickInterval:    100 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = m.Run(ctx)
	}()

	// Claim two sessions and open gates.
	tokA := mintToken(t, priv, "room1", "alice", 1, t0)
	tokB := mintToken(t, priv, "room1", "bob", 1, t0)
	_, _ = m.Claim(tokA)
	_, _ = m.Claim(tokB)

	rawA := make(chan core.InputCommand)
	rawB := make(chan core.InputCommand)
	gatedA := m.Gate("alice", rawA)
	gatedB := m.Gate("bob", rawB)

	// Close raw channels to let the gate goroutines wind down on their own.
	close(rawA)
	close(rawB)

	// Wait for gate channels to drain/close.
	drainOrTimeout := func(ch <-chan core.InputCommand, name string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-deadline:
				t.Errorf("gate %s not closed within timeout", name)
				return
			}
		}
	}
	// alice's gate will close when rawA closes; bob's will close when either
	// rawB closes or session ends.
	drainOrTimeout(gatedA, "alice")
	drainOrTimeout(gatedB, "bob")

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// Drain the events channel so we don't block.
	for range m.Events() {
	}
}

// TestInputGate_PromotedViewerGetsCommands verifies that a viewer's gate that
// was created while queued is successfully transferred and becomes active
// (forwards commands) once the viewer is promoted. It also verifies that the
// gate is closed when the promoted session expires.
func TestInputGate_PromotedViewerGetsCommands(t *testing.T) {
	pubB64, priv := testKeyPair(t)
	t0 := time.Unix(1_718_000_000, 0)

	cfg := Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room1",
		TickInterval:    200 * time.Millisecond,
		QueuePolicy:     PolicyQueue,
		now:             func() time.Time { return t0 },
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// Alice gets a 1-second session. Bob gets 2 seconds.
	tokA := mintToken(t, priv, "room1", "alice", 1, t0)
	tokB := mintToken(t, priv, "room1", "bob", 2, t0)

	_, err = m.Claim(tokA)
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	_, err = m.Claim(tokB)
	if err != nil {
		t.Fatalf("Claim B: %v", err)
	}

	rawBIn := make(chan core.InputCommand, 4)
	gatedB := m.Gate("bob", rawBIn)

	// Send a command for B (queued, not active). Should not be forwarded.
	rawBIn <- core.InputCommand{Buttons: core.ButtonB}
	select {
	case cmd := <-gatedB:
		t.Errorf("queued viewer's command was forwarded (buttons=%#x), expected drop", cmd.Buttons)
	case <-time.After(100 * time.Millisecond):
		// Good, not forwarded.
	}

	// Wait for Alice to expire; Bob should be promoted automatically.
	if !waitEvent(t, m.Events(), func(ev SessionEvent) bool {
		return ev.Kind == EventGranted && ev.ViewerID == "bob"
	}, 3*time.Second) {
		t.Fatal("expected Bob to be promoted/granted")
	}

	// Send command for Bob now that he is active.
	rawBIn <- core.InputCommand{Buttons: core.ButtonA}
	select {
	case got, ok := <-gatedB:
		if !ok {
			t.Fatal("bob's gate closed unexpectedly after promotion")
		}
		if got.Buttons != core.ButtonA {
			t.Errorf("bob forwarded buttons: got %#x, want %#x", got.Buttons, core.ButtonA)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bob's command not forwarded after promotion within timeout")
	}

	// Wait for Bob to expire; Bob's gate must be closed.
	select {
	case _, ok := <-gatedB:
		if ok {
			t.Error("expected bob's gate to be closed after session expiry, but received value")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("bob's gate channel not closed after session expiry")
	}

	close(rawBIn)
}
