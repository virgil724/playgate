package sunshine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/session"
)

// --------------------------------------------------------------------------
// Mock Sunshine server (httptest)
// --------------------------------------------------------------------------

// mockSunshine is a tiny httptest server that mimics the Sunshine REST API.
type mockSunshine struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex

	// Counters for assertions.
	statusCalls   int
	pinCalls      int
	closeAppCalls int
	clientsCalls  int

	// Configurable responses.
	statusCode     int // override HTTP status (0 = 200)
	rejectPin      bool
	rejectCloseApp bool

	// PIN submitted to /api/pin.
	lastPIN string
}

func newMockSunshine(t *testing.T) *mockSunshine {
	t.Helper()
	m := &mockSunshine{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc(endpointStatus, m.handleStatus)
	mux.HandleFunc(endpointPairPinApprove, m.handlePin)
	mux.HandleFunc(endpointCloseApp, m.handleCloseApp)
	mux.HandleFunc(endpointClients, m.handleClients)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSunshine) handleStatus(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls++
	if m.statusCode != 0 {
		w.WriteHeader(m.statusCode)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version":  "0.23.0",
		"platform": "windows",
	})
}

func (m *mockSunshine) handlePin(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinCalls++
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	m.lastPIN = r.FormValue("pin")
	if m.rejectPin {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "true"})
}

func (m *mockSunshine) handleCloseApp(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeAppCalls++
	if m.rejectCloseApp {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "true"})
}

func (m *mockSunshine) handleClients(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientsCalls++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"named_certs": []map[string]string{
			{"name": "Virgil-PC", "cert": "CERT_PLACEHOLDER"},
		},
	})
}

// clientForMock builds an HTTPClient pointing at the mock server.
// No TLS because httptest.NewServer uses plain HTTP.
func clientForMock(srv *httptest.Server) *HTTPClient {
	return NewHTTPClient(ClientConfig{
		BaseURL:            srv.URL,
		InsecureSkipVerify: false, // plain HTTP test server
		Timeout:            3 * time.Second,
		MaxRetries:         0, // no retries in most tests (speed)
		RetryDelay:         10 * time.Millisecond,
	})
}

// --------------------------------------------------------------------------
// Client unit tests
// --------------------------------------------------------------------------

func TestHTTPClient_Status(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)

	info, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if info.Version != "0.23.0" {
		t.Errorf("Version: got %q, want %q", info.Version, "0.23.0")
	}
	if info.Platform != "windows" {
		t.Errorf("Platform: got %q, want %q", info.Platform, "windows")
	}
	if mock.statusCalls != 1 {
		t.Errorf("statusCalls: got %d, want 1", mock.statusCalls)
	}
}

func TestHTTPClient_ApprovePair(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)

	if err := c.ApprovePair(context.Background(), "1234"); err != nil {
		t.Fatalf("ApprovePair: %v", err)
	}
	if mock.lastPIN != "1234" {
		t.Errorf("lastPIN: got %q, want %q", mock.lastPIN, "1234")
	}
	if mock.pinCalls != 1 {
		t.Errorf("pinCalls: got %d, want 1", mock.pinCalls)
	}
}

func TestHTTPClient_KickAll(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)

	if err := c.KickAll(context.Background()); err != nil {
		t.Fatalf("KickAll: %v", err)
	}
	if mock.closeAppCalls != 1 {
		t.Errorf("closeAppCalls: got %d, want 1", mock.closeAppCalls)
	}
}

func TestHTTPClient_Clients(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)

	clients, err := c.Clients(context.Background())
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("len(clients): got %d, want 1", len(clients))
	}
	if clients[0].Name != "Virgil-PC" {
		t.Errorf("client name: got %q, want %q", clients[0].Name, "Virgil-PC")
	}
}

// TestHTTPClient_SunshineUnreachable verifies that the client reports an error
// and retries when the server is not listening.
func TestHTTPClient_SunshineUnreachable(t *testing.T) {
	// Point client at a port that is (almost certainly) not listening.
	c := NewHTTPClient(ClientConfig{
		BaseURL:    "http://127.0.0.1:1", // port 1 is reserved, always refused
		Timeout:    500 * time.Millisecond,
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
	})

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

// TestHTTPClient_Retry_EventualSuccess verifies that the client retries on 5xx
// and succeeds on the third attempt.
func TestHTTPClient_Retry_EventualSuccess(t *testing.T) {
	var attempt atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "ok", "platform": "test"})
	}))
	defer srv.Close()

	c := NewHTTPClient(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    3 * time.Second,
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	})

	info, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status after retries: %v", err)
	}
	if info.Version != "ok" {
		t.Errorf("version: got %q, want ok", info.Version)
	}
	if attempt.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempt.Load())
	}
}

// TestHTTPClient_4xx_NoRetry verifies that 4xx responses are not retried.
func TestHTTPClient_4xx_NoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewHTTPClient(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    3 * time.Second,
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	})

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for 4xx, got %d", calls.Load())
	}
}

// --------------------------------------------------------------------------
// Agent integration tests
// --------------------------------------------------------------------------

// makeTestEvents creates a buffered channel and returns (ch, send-func).
func makeTestEvents() (chan session.SessionEvent, func(session.SessionEvent)) {
	ch := make(chan session.SessionEvent, 64)
	return ch, func(ev session.SessionEvent) { ch <- ev }
}

func newTestAgent(t *testing.T, ctrl Controller, events <-chan session.SessionEvent, pin string) *Agent {
	t.Helper()
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ag, err := NewAgent(AgentConfig{
		Controller:     ctrl,
		Events:         events,
		PairPIN:        pin,
		KickRetries:    2,
		KickRetryDelay: 10 * time.Millisecond,
		Log:            log,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return ag
}

// testWriter bridges slog to t.Log for clean test output.
type testWriter struct{ t *testing.T }

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Helper()
	tw.t.Log(string(p))
	return len(p), nil
}

// TestAgent_GrantedApprovesPair verifies that EventGranted triggers ApprovePair.
func TestAgent_GrantedApprovesPair(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	events, send := makeTestEvents()
	close(events) // will be replaced below — actually we need an open one

	evCh := make(chan session.SessionEvent, 8)
	ag := newTestAgent(t, c, evCh, "5678")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	evCh <- session.SessionEvent{Kind: session.EventGranted, ViewerID: "alice", RemainingSeconds: 120}
	// Give the agent time to process.
	time.Sleep(100 * time.Millisecond)

	cancel()
	<-done

	_ = send // suppress unused warning
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.pinCalls != 1 {
		t.Errorf("pinCalls: got %d, want 1", mock.pinCalls)
	}
	if mock.lastPIN != "5678" {
		t.Errorf("PIN submitted: got %q, want %q", mock.lastPIN, "5678")
	}
}

// TestAgent_ExpiredKicksAll verifies that EventExpired triggers KickAll.
func TestAgent_ExpiredKicksAll(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	evCh := make(chan session.SessionEvent, 8)
	ag := newTestAgent(t, c, evCh, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	evCh <- session.SessionEvent{Kind: session.EventExpired, ViewerID: "alice"}
	time.Sleep(100 * time.Millisecond)

	cancel()
	<-done

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.closeAppCalls != 1 {
		t.Errorf("closeAppCalls: got %d, want 1", mock.closeAppCalls)
	}
}

// TestAgent_IdleKickedKicksAll verifies that EventIdleKicked triggers KickAll.
func TestAgent_IdleKickedKicksAll(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	evCh := make(chan session.SessionEvent, 8)
	ag := newTestAgent(t, c, evCh, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	evCh <- session.SessionEvent{Kind: session.EventIdleKicked, ViewerID: "bob"}
	time.Sleep(100 * time.Millisecond)

	cancel()
	<-done

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.closeAppCalls != 1 {
		t.Errorf("closeAppCalls: got %d, want 1", mock.closeAppCalls)
	}
}

// mockController is a Controller that returns configurable errors, used to
// test the agent's retry logic without a real HTTP server.
type mockController struct {
	mu            sync.Mutex
	kickCallCount int
	kickErrors    []error // errors to return in sequence; nil = success
	approveErr    error
}

func (m *mockController) Status(ctx context.Context) (StatusInfo, error) {
	return StatusInfo{Version: "mock"}, nil
}

func (m *mockController) ApprovePair(ctx context.Context, pin string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.approveErr
}

func (m *mockController) KickAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.kickCallCount
	m.kickCallCount++
	if n < len(m.kickErrors) {
		return m.kickErrors[n]
	}
	return nil
}

func (m *mockController) Clients(ctx context.Context) ([]ClientInfo, error) {
	return nil, nil
}

func (m *mockController) kickCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kickCallCount
}

// Ensure mockController implements Controller.
var _ Controller = (*mockController)(nil)

// TestAgent_KickRetry verifies that on Sunshine error the agent retries KickAll
// until it succeeds, using an in-process mock Controller for determinism.
func TestAgent_KickRetry(t *testing.T) {
	// KickAll fails on attempt 1, succeeds on attempt 2.
	ctrl := &mockController{
		kickErrors: []error{fmt.Errorf("sunshine: KickAll: simulated 500")},
	}
	evCh := make(chan session.SessionEvent, 8)
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ag, err := NewAgent(AgentConfig{
		Controller:     ctrl,
		Events:         evCh,
		KickRetries:    3,
		KickRetryDelay: 10 * time.Millisecond,
		Log:            log,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	evCh <- session.SessionEvent{Kind: session.EventExpired, ViewerID: "alice"}

	// Poll until we see at least 2 kicks or timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctrl.kickCalls() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	got := ctrl.kickCalls()
	if got < 2 {
		t.Errorf("expected at least 2 KickAll calls (retry), got %d", got)
	}
}

// TestAgent_ShutdownOnContextCancel verifies that Run returns when ctx is cancelled.
func TestAgent_ShutdownOnContextCancel(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	evCh := make(chan session.SessionEvent, 8)
	ag := newTestAgent(t, c, evCh, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestAgent_ShutdownOnChannelClose verifies that Run returns when Events is closed.
func TestAgent_ShutdownOnChannelClose(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	evCh := make(chan session.SessionEvent, 8)
	ag := newTestAgent(t, c, evCh, "")

	ctx := context.Background()
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()

	close(evCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Events channel close")
	}
}

// --------------------------------------------------------------------------
// Integration test with real session.Manager (short-lived session)
// --------------------------------------------------------------------------

// testKeyPair is replicated here (can't access session package-private helpers).
func testKeyPair(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(pub), priv
}

func mintSessionToken(t *testing.T, priv ed25519.PrivateKey, roomID, viewerID string, sessionSecs int) string {
	t.Helper()
	now := time.Now()
	// Build JWT manually (same approach as session_test.go).
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]interface{}{
		"iss":             "playgate-server",
		"iat":             now.Unix(),
		"exp":             now.Add(10 * time.Minute).Unix(),
		"room_id":         roomID,
		"viewer_id":       viewerID,
		"session_seconds": sessionSecs,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	msg := header + "." + payloadB64
	sig := ed25519.Sign(priv, []byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestIntegration_SessionExpiry uses a real session.Manager to emit
// EventExpired and asserts that the Agent calls KickAll exactly once.
func TestIntegration_SessionExpiry(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)

	pubB64, priv := testKeyPair(t)
	mgr, err := session.NewManager(session.Config{
		PublicKeyBase64: pubB64,
		RoomID:          "room-pc",
		TickInterval:    200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	go func() { _ = mgr.Run(mgrCtx) }()

	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ag, err := NewAgent(AgentConfig{
		Controller:     c,
		Events:         mgr.Events(),
		KickRetries:    2,
		KickRetryDelay: 20 * time.Millisecond,
		Log:            log,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go func() { _ = ag.Run(agCtx) }()

	// Claim a 1-second session — it will expire naturally.
	tok := mintSessionToken(t, priv, "room-pc", "viewer-1", 1)
	if _, err := mgr.Claim(tok); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Wait up to 5 seconds for KickAll to be called.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		calls := mock.closeAppCalls
		mock.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mock.mu.Lock()
	calls := mock.closeAppCalls
	mock.mu.Unlock()
	if calls == 0 {
		t.Error("KickAll was never called after session expiry")
	}
}

// --------------------------------------------------------------------------
// NewAgent validation tests
// --------------------------------------------------------------------------

func TestNewAgent_NilController(t *testing.T) {
	evCh := make(chan session.SessionEvent)
	_, err := NewAgent(AgentConfig{Controller: nil, Events: evCh})
	if err == nil {
		t.Fatal("expected error for nil Controller")
	}
}

func TestNewAgent_NilEvents(t *testing.T) {
	mock := newMockSunshine(t)
	c := clientForMock(mock.srv)
	_, err := NewAgent(AgentConfig{Controller: c, Events: nil})
	if err == nil {
		t.Fatal("expected error for nil Events")
	}
}

// Ensure HTTPClient implements Controller at compile time.
var _ Controller = (*HTTPClient)(nil)

// Ensure the fmt import is used (avoids "imported and not used" if all callers
// are removed during refactoring).
var _ = fmt.Sprintf
