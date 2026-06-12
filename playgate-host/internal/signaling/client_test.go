package signaling

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWorker is an in-memory stand-in for the signaling Worker that implements
// the subset of its API the host uses: per-peer message queues plus TURN creds.
type fakeWorker struct {
	mu     sync.Mutex
	queues map[string][]Message // key: roomID/peer (the POSTER's peer)
	srv    *httptest.Server
}

// fakeTs mimics the Worker's ISO-8601 timestamps.
func fakeTs() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	w := &fakeWorker{queues: map[string][]Message{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/turn/credentials", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			rw.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(rw).Encode(turnCredentialsResponse{
			ICEServers: []ICEServer{
				{URLs: json.RawMessage(`"stun:stun.cloudflare.com:3478"`)},
				{URLs: json.RawMessage(`["turn:turn.cloudflare.com:3478?transport=udp"]`), Username: "u", Credential: "c"},
			},
			TTL: 3600,
		})
	})

	mux.HandleFunc("/rooms/", func(rw http.ResponseWriter, r *http.Request) {
		// path: /rooms/{room}/{peer}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rooms/"), "/")
		if len(parts) != 2 {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		room, peer := parts[0], parts[1]
		key := room + "/" + peer

		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			seq := len(w.queues[key])
			ts := fakeTs()
			w.queues[key] = append(w.queues[key], Message{Seq: seq, Ts: ts, Payload: json.RawMessage(body)})
			w.mu.Unlock()
			rw.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(rw).Encode(map[string]any{"seq": seq, "ts": ts})
		case http.MethodGet:
			// GET returns the OTHER peer's messages.
			other := "viewer"
			if peer == "viewer" {
				other = "host"
			}
			otherKey := room + "/" + other
			since := -1
			if s := r.URL.Query().Get("since"); s != "" {
				if v, err := strconv.Atoi(s); err == nil {
					since = v
				}
			}
			w.mu.Lock()
			all := w.queues[otherKey]
			var out []Message
			next := since
			for _, m := range all {
				if m.Seq > since {
					out = append(out, m)
					next = m.Seq
				}
			}
			w.mu.Unlock()
			_ = json.NewEncoder(rw).Encode(messagesResponse{Messages: out, NextSince: next})
		default:
			rw.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	w.srv = httptest.NewServer(mux)
	t.Cleanup(w.srv.Close)
	return w
}

// postAsViewer injects a message into the viewer queue (simulating the browser).
func (w *fakeWorker) postAsViewer(room string, payload any) {
	body, _ := json.Marshal(payload)
	key := room + "/viewer"
	w.mu.Lock()
	seq := len(w.queues[key])
	w.queues[key] = append(w.queues[key], Message{Seq: seq, Ts: fakeTs(), Payload: json.RawMessage(body)})
	w.mu.Unlock()
}

func TestTURNCredentials(t *testing.T) {
	w := newFakeWorker(t)
	c, err := New(Config{BaseURL: w.srv.URL, RoomID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := c.TURNCredentials(context.Background())
	if err != nil {
		t.Fatalf("TURNCredentials: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 ice servers, got %d", len(servers))
	}
	if got := servers[0].URLList(); len(got) != 1 || got[0] != "stun:stun.cloudflare.com:3478" {
		t.Errorf("server0 urls = %v", got)
	}
	if got := servers[1].URLList(); len(got) != 1 || !strings.HasPrefix(got[0], "turn:") {
		t.Errorf("server1 urls = %v", got)
	}
	if servers[1].Username != "u" || servers[1].Credential != "c" {
		t.Errorf("server1 creds = %q/%q", servers[1].Username, servers[1].Credential)
	}
}

func TestPostOfferAndPollAnswer(t *testing.T) {
	w := newFakeWorker(t)
	c, err := New(Config{BaseURL: w.srv.URL, RoomID: "room42", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	offerTs, err := c.PostOffer(ctx, SDPMessage{Type: "offer", SDP: "v=0..."})
	if err != nil {
		t.Fatalf("PostOffer: %v", err)
	}
	if offerTs == "" {
		t.Error("PostOffer returned empty ts from a worker that sends one")
	}

	// Viewer posts an answer.
	w.postAsViewer("room42", SDPMessage{Type: "answer", SDP: "v=0-answer"})

	msgs, next, err := c.Poll(ctx, PeerHost, -1)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0].Ts == "" {
		t.Error("Poll dropped the message ts")
	}
	var sdp SDPMessage
	if err := json.Unmarshal(msgs[0].Payload, &sdp); err != nil {
		t.Fatal(err)
	}
	if sdp.Type != "answer" || sdp.SDP != "v=0-answer" {
		t.Errorf("unexpected answer: %+v", sdp)
	}
	if next != 0 {
		t.Errorf("nextSince = %d, want 0", next)
	}

	// Polling again from next yields nothing new.
	msgs2, _, err := c.Poll(ctx, PeerHost, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 0 {
		t.Errorf("expected no new messages, got %d", len(msgs2))
	}
}

// TestPostOfferToleratesEmptyBody ensures an older Worker that answers 201 with
// no body yields zero values, not an error.
func TestPostOfferToleratesEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusCreated) // no body
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := c.PostOffer(context.Background(), SDPMessage{Type: "offer", SDP: "v=0"})
	if err != nil {
		t.Fatalf("PostOffer with empty body: %v", err)
	}
	if ts != "" {
		t.Errorf("ts = %q, want empty", ts)
	}
}

func TestAuthorizationHeaderSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		rw.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PostMessage(context.Background(), map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{RoomID: "r"}); err == nil {
		t.Error("expected error for empty BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Error("expected error for empty RoomID")
	}
}

// TestPollWaitOutlivesClientTimeout pins the long-poll timeout fix:
// http.Client.Timeout is an absolute cap that a context deadline can only
// shorten, so PollWait must bypass the shared client's cap or a held request
// dies early (the production default is 10s vs a 25s wait).
func TestPollWaitOutlivesClientTimeout(t *testing.T) {
	const hold = 500 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		time.Sleep(hold)
		_ = json.NewEncoder(rw).Encode(messagesResponse{
			Messages:  []Message{{Seq: 1, Ts: fakeTs(), Payload: json.RawMessage(`{"type":"answer","sdp":"v=0"}`)}},
			NextSince: 1,
		})
	}))
	defer srv.Close()

	// Shared client timeout far below the server hold; wait must still win.
	c, err := New(Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}

	msgs, _, err := c.PollWait(context.Background(), PeerHost, -1, 2*time.Second)
	if err != nil {
		t.Fatalf("PollWait died before the server replied (client cap not bypassed): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}

	// And the short-poll path must still honour the shared client timeout.
	if _, _, err := c.Poll(context.Background(), PeerHost, -1); err == nil {
		t.Fatal("Poll with 100ms client timeout against a 500ms hold should fail")
	}
}

// TestPollWaitSendsWaitParam verifies that PollWait appends &wait=<seconds> to
// the URL and that a server-side delay is tolerated — the response is returned
// intact once the server replies.
func TestPollWaitSendsWaitParam(t *testing.T) {
	const holdDuration = 100 * time.Millisecond // fast for CI

	var gotWait string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotWait = r.URL.Query().Get("wait")
		// Simulate a server hold before replying.
		time.Sleep(holdDuration)
		_ = json.NewEncoder(rw).Encode(messagesResponse{
			Messages:  []Message{{Seq: 7, Ts: fakeTs(), Payload: json.RawMessage(`{"type":"answer","sdp":"v=0"}`)}},
			NextSince: 7,
		})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	msgs, next, err := c.PollWait(context.Background(), PeerHost, -1, 2*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("PollWait: %v", err)
	}
	if gotWait != "2" {
		t.Errorf("wait query param = %q, want %q", gotWait, "2")
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if next != 7 {
		t.Errorf("nextSince = %d, want 7", next)
	}
	if elapsed < holdDuration {
		t.Errorf("elapsed %v < hold %v — server hold was not respected", elapsed, holdDuration)
	}
}

// TestPollWaitCtxCancellation verifies that cancelling the context during a
// held long-poll returns promptly (no hang).
func TestPollWaitCtxCancellation(t *testing.T) {
	// Server holds indefinitely until the client disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // released when client aborts
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.PollWait(ctx, PeerHost, -1, 25*time.Second)
		done <- err
	}()

	// Give the goroutine time to reach the server, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error after ctx cancellation, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("PollWait did not return within 1s after ctx cancellation")
	}
}

// TestPollWaitZeroWait verifies that Poll (which delegates with wait=0) does NOT
// append a wait parameter — keeping backward compatibility with old Workers.
func TestPollWaitZeroWait(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotRaw = r.URL.RawQuery
		_ = json.NewEncoder(rw).Encode(messagesResponse{})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, RoomID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Poll(context.Background(), PeerHost, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotRaw, "wait") {
		t.Errorf("Poll appended wait to query; raw=%q", gotRaw)
	}
}
