package heartbeat

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
)

// --- fakes ---

// fakeViewers implements ViewerSource for tests.
type fakeViewers struct {
	id string
}

func (f *fakeViewers) CurrentViewerID() string { return f.id }

// --- helpers ---

func buildCfg(serverURL string, intervalSecs int) config.Config {
	cfg := config.Default()
	cfg.Server.URL = serverURL
	cfg.Server.APIKey = "test-key"
	cfg.Server.HeartbeatIntervalSeconds = intervalSecs
	cfg.Signaling.RoomID = "room42"
	return cfg
}

// --- tests ---

// TestHeartbeatPostsCorrectPathAndAuth verifies the runner POSTs to the right
// path and includes the Authorization header.
func TestHeartbeatPostsCorrectPathAndAuth(t *testing.T) {
	var gotPath, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(heartbeatResponse{Status: "ok", KickRequested: false})
	}))
	defer ts.Close()

	cfg := buildCfg(ts.URL, 60)
	r := New(discardLogger(), cfg, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.beat(ctx)

	if gotPath != "/api/rooms/room42/heartbeat" {
		t.Errorf("path = %q, want /api/rooms/room42/heartbeat", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want Bearer test-key", gotAuth)
	}
}

// TestHeartbeatIncludesCurrentViewer verifies that the current_viewer field is
// populated when a viewer holds control, and null when nobody does.
func TestHeartbeatIncludesCurrentViewer(t *testing.T) {
	type reqBody struct {
		Online        bool    `json:"online"`
		CurrentViewer *string `json:"current_viewer"`
	}

	cases := []struct {
		name       string
		viewerID   string
		wantViewer *string
	}{
		{"with viewer", "abc123", strPtr("abc123")},
		{"no viewer", "", nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotBody reqBody
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&gotBody)
				json.NewEncoder(w).Encode(heartbeatResponse{Status: "ok"})
			}))
			defer ts.Close()

			cfg := buildCfg(ts.URL, 60)
			viewers := &fakeViewers{id: tc.viewerID}
			r := New(discardLogger(), cfg, viewers, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r.beat(ctx)

			if tc.wantViewer == nil {
				if gotBody.CurrentViewer != nil {
					t.Errorf("current_viewer = %q, want null", *gotBody.CurrentViewer)
				}
			} else {
				if gotBody.CurrentViewer == nil {
					t.Errorf("current_viewer = null, want %q", *tc.wantViewer)
				} else if *gotBody.CurrentViewer != *tc.wantViewer {
					t.Errorf("current_viewer = %q, want %q", *gotBody.CurrentViewer, *tc.wantViewer)
				}
			}
		})
	}
}

// TestKickRequestedTriggerKickOnce verifies that a kick_requested=true response
// calls the kick function exactly once.
func TestKickRequestedTriggerKickOnce(t *testing.T) {
	var kicks atomic.Int32
	kickFn := KickFunc(func() { kicks.Add(1) })

	// First request returns kick_requested=true; subsequent ones return false.
	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		kick := n == 1
		json.NewEncoder(w).Encode(heartbeatResponse{Status: "ok", KickRequested: kick})
	}))
	defer ts.Close()

	cfg := buildCfg(ts.URL, 60)
	r := New(discardLogger(), cfg, nil, kickFn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Two beats: first one triggers kick, second does not.
	r.beat(ctx)
	r.beat(ctx)

	if kicks.Load() != 1 {
		t.Errorf("kick called %d times, want exactly 1", kicks.Load())
	}
}

// TestLoopExitsOnCancel verifies the Run loop stops promptly when ctx is
// cancelled and does not leak a goroutine.
func TestLoopExitsOnCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(heartbeatResponse{Status: "ok"})
	}))
	defer ts.Close()

	// Use a long interval so the loop blocks on the ticker, not on HTTP.
	cfg := buildCfg(ts.URL, 9999)
	r := New(discardLogger(), cfg, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the initial beat fire.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s after context cancel")
	}
}

// TestConfigDefaultInterval verifies that <= 0 heartbeat_interval_seconds
// defaults to 30 s.
func TestConfigDefaultInterval(t *testing.T) {
	cfg := buildCfg("http://example.com", 0)
	r := New(discardLogger(), cfg, nil, nil)
	if r.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s for zero config value", r.interval)
	}

	cfg2 := buildCfg("http://example.com", -5)
	r2 := New(discardLogger(), cfg2, nil, nil)
	if r2.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s for negative config value", r2.interval)
	}
}

// TestEmptyURLMeansNoLoop verifies that when Server.URL is empty the guard in
// the host does not start the runner (tested indirectly by confirming the
// constructor itself does not panic and Run would work if called).
// The real "not started" invariant is exercised in host_heartbeat_test.go.
func TestEmptyURLMeansNoLoop(t *testing.T) {
	cfg := config.Default() // Server.URL is ""
	if cfg.Server.URL != "" {
		t.Fatal("default config should have empty server URL")
	}
	// Just confirm New doesn't panic with an empty URL.
	r := New(discardLogger(), cfg, nil, nil)
	if r.url != "" {
		t.Errorf("url = %q, want empty string", r.url)
	}
}

// --- utilities ---

func strPtr(s string) *string { return &s }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
