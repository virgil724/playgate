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
)

// fakeWorker is an in-memory stand-in for the signaling Worker that implements
// the subset of its API the host uses: per-peer message queues plus TURN creds.
type fakeWorker struct {
	mu     sync.Mutex
	queues map[string][]message // key: roomID/peer (the POSTER's peer)
	srv    *httptest.Server
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	w := &fakeWorker{queues: map[string][]message{}}
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
			w.queues[key] = append(w.queues[key], message{Seq: seq, Payload: json.RawMessage(body)})
			w.mu.Unlock()
			rw.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(rw).Encode(map[string]any{"seq": seq})
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
			var out []message
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
	w.queues[key] = append(w.queues[key], message{Seq: seq, Payload: json.RawMessage(body)})
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

	if err := c.PostOffer(ctx, SDPMessage{Type: "offer", SDP: "v=0..."}); err != nil {
		t.Fatalf("PostOffer: %v", err)
	}

	// Viewer posts an answer.
	w.postAsViewer("room42", SDPMessage{Type: "answer", SDP: "v=0-answer"})

	payloads, next, err := c.Poll(ctx, PeerHost, -1)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("want 1 payload, got %d", len(payloads))
	}
	var sdp SDPMessage
	if err := json.Unmarshal(payloads[0], &sdp); err != nil {
		t.Fatal(err)
	}
	if sdp.Type != "answer" || sdp.SDP != "v=0-answer" {
		t.Errorf("unexpected answer: %+v", sdp)
	}
	if next != 0 {
		t.Errorf("nextSince = %d, want 0", next)
	}

	// Polling again from next yields nothing new.
	payloads2, _, err := c.Poll(ctx, PeerHost, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads2) != 0 {
		t.Errorf("expected no new messages, got %d", len(payloads2))
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
