package signaling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsTestServer upgrades /rooms/{room}/{peer}/ws and, on accept, writes each of
// the supplied frames in order, then holds the socket open until ctx is done (so
// tests can exercise blocked reads). It records the last request URL seen.
func wsTestServer(t *testing.T, frames [][]byte) (*httptest.Server, *string) {
	t.Helper()
	var lastURL string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		lastURL = r.URL.String()
		conn, err := websocket.Accept(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "")
		ctx := r.Context()
		for _, f := range frames {
			if err := conn.Write(ctx, websocket.MessageText, f); err != nil {
				return
			}
		}
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &lastURL
}

// TestDialWSReceivesFrames verifies DialWS connects and Receive unmarshals
// replayed frames in order.
func TestDialWSReceivesFrames(t *testing.T) {
	f1, _ := json.Marshal(Message{Seq: 0, Ts: "t0", Payload: json.RawMessage(`{"type":"x"}`)})
	f2, _ := json.Marshal(Message{Seq: 1, Ts: "t1", Payload: json.RawMessage(`{"type":"answer"}`)})
	srv, _ := wsTestServer(t, [][]byte{f1, f2})

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := c.DialWS(ctx, PeerHost)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()

	m1, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive 1: %v", err)
	}
	if m1.Seq != 0 || m1.Ts != "t0" {
		t.Fatalf("frame 1 mismatch: %+v", m1)
	}
	m2, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive 2: %v", err)
	}
	if m2.Seq != 1 || m2.Ts != "t1" {
		t.Fatalf("frame 2 mismatch: %+v", m2)
	}
}

// TestDialWSSkipsMalformedFrame verifies a malformed JSON frame is skipped and
// the following valid frame is returned.
func TestDialWSSkipsMalformedFrame(t *testing.T) {
	bad := []byte(`{not json`)
	good, _ := json.Marshal(Message{Seq: 7, Ts: "t7"})
	srv, _ := wsTestServer(t, [][]byte{bad, good})

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := c.DialWS(ctx, PeerHost)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()

	m, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if m.Seq != 7 {
		t.Fatalf("expected to skip malformed frame and read seq 7, got %+v", m)
	}
}

// TestDialWSTokenInQuery verifies the dialed URL carries ?token= when set.
func TestDialWSTokenInQuery(t *testing.T) {
	srv, lastURL := wsTestServer(t, nil)
	c, err := New(Config{BaseURL: srv.URL, RoomID: "room9", Token: "secret-tok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := c.DialWS(ctx, PeerHost)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()

	if got := *lastURL; got != "/rooms/room9/host/ws?token=secret-tok" {
		t.Fatalf("dialed URL = %q, want token query param", got)
	}
}

// TestDialWSCtxCancelAbortsReceive verifies a parked Receive returns promptly
// when ctx is cancelled.
func TestDialWSCtxCancelAbortsReceive(t *testing.T) {
	srv, _ := wsTestServer(t, nil) // accepts, sends nothing, holds open
	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	conn, err := c.DialWS(dialCtx, PeerHost)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.Receive(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Receive to error on ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("Receive did not return within 1s after ctx cancel")
	}
}

// TestWSConnCloseIdempotent verifies Close can be called multiple times safely.
func TestWSConnCloseIdempotent(t *testing.T) {
	srv, _ := wsTestServer(t, nil)
	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := c.DialWS(ctx, PeerHost)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	conn.Close()
	conn.Close() // must not panic
}

// TestWSURLForScheme verifies http→ws and https→wss derivation.
func TestWSURLForScheme(t *testing.T) {
	cases := []struct{ base, want string }{
		{"http://localhost:8787", "ws://localhost:8787/rooms/r/host/ws"},
		{"https://x.workers.dev", "wss://x.workers.dev/rooms/r/host/ws"},
		{"https://x.workers.dev/", "wss://x.workers.dev/rooms/r/host/ws"},
	}
	for _, tc := range cases {
		got, err := wsURLFor(tc.base, "r", PeerHost, "")
		if err != nil {
			t.Fatalf("wsURLFor(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Errorf("wsURLFor(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}
