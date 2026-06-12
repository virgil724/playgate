package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/playgate/playgate-host/internal/rtc"
	"github.com/playgate/playgate-host/internal/signaling"
)

// wsAnswerServer serves the host WS route. On accept it writes the given frames
// in order (each a marshalled signaling.Message), then holds the socket open
// until the request ctx is done. Non-WS paths fall through to a 404 so that
// dial-failure tests can omit the route entirely by passing serveWS=false.
func wsAnswerServer(t *testing.T, serveWS bool, frames [][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if serveWS && strings.HasSuffix(r.URL.Path, "/ws") {
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
			return
		}
		http.NotFound(rw, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func answerFrame(t *testing.T, ts, sdp string) []byte {
	t.Helper()
	payload, _ := json.Marshal(signaling.SDPMessage{Type: "answer", SDP: sdp})
	m, _ := json.Marshal(signaling.Message{Seq: 0, Ts: ts, Payload: payload})
	return m
}

// TestPollForAnswerWSReceivesAnswer verifies the WS path: the server replays a
// stale answer (ts before offerTs) which must be skipped, then a fresh answer
// which must be applied. pollForAnswer should return nil without falling back to
// long-poll (the server has no poll route).
func TestPollForAnswerWSReceivesAnswer(t *testing.T) {
	peer, err := rtc.NewPeer(rtc.Config{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	answerSDP := buildRealAnswer(t, peer)

	offerTs := "2020-01-01T00:00:05Z"
	staleTs := "2020-01-01T00:00:00Z" // before offerTs → must be skipped
	freshTs := "2020-01-01T00:00:10Z" // after offerTs → must be applied

	stale := answerFrame(t, staleTs, answerSDP)
	fresh := answerFrame(t, freshTs, answerSDP)
	srv := wsAnswerServer(t, true, [][]byte{stale, fresh})

	sc, err := signaling.New(signaling.Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	cm := &connManager{log: discardLogger(), client: sc, poll: 10 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The server has no HTTP poll route, so pollForAnswer can only succeed via the
	// WS path. A nil return proves the fresh answer was applied (and, since the
	// stale frame arrived first and a stale apply does not count as gotAnswer, that
	// the stale answer was correctly skipped rather than ending the loop early).
	if err := cm.pollForAnswer(ctx, peer, offerTs); err != nil {
		t.Fatalf("pollForAnswer (WS) returned error: %v", err)
	}
}

// TestPollForAnswerWSDialFailFallsBackToLongPoll verifies that when the server
// does not speak WS (no /ws route), pollForAnswer falls back to the HTTP
// long-poll loop and still applies the answer.
func TestPollForAnswerWSDialFailFallsBackToLongPoll(t *testing.T) {
	peer, err := rtc.NewPeer(rtc.Config{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	answerSDP := buildRealAnswer(t, peer)
	payload, _ := json.Marshal(signaling.SDPMessage{Type: "answer", SDP: answerSDP})

	// Server: /ws → 404 (dial fails), poll route → returns the answer once.
	var firstPoll = true
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws") {
			http.NotFound(rw, r)
			return
		}
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		type pollResp struct {
			Messages  []signaling.Message `json:"messages"`
			NextSince int                 `json:"nextSince"`
		}
		var msgs []signaling.Message
		if firstPoll {
			firstPoll = false
			msgs = []signaling.Message{{Seq: 0, Ts: ts, Payload: json.RawMessage(payload)}}
		}
		_ = json.NewEncoder(rw).Encode(pollResp{Messages: msgs, NextSince: -1})
	}))
	t.Cleanup(srv.Close)

	sc, err := signaling.New(signaling.Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	cm := &connManager{log: discardLogger(), client: sc, poll: 10 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cm.pollForAnswer(ctx, peer, ""); err != nil {
		t.Fatalf("pollForAnswer (fallback) returned error: %v", err)
	}
}

// TestPollForAnswerWSCtxCancelDuringReceive verifies that cancelling ctx while a
// WS Receive is parked (server accepted but sends nothing) returns promptly.
func TestPollForAnswerWSCtxCancelDuringReceive(t *testing.T) {
	peer, err := rtc.NewPeer(rtc.Config{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	srv := wsAnswerServer(t, true, nil) // accepts, sends nothing, holds open
	sc, err := signaling.New(signaling.Config{BaseURL: srv.URL, RoomID: "r", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	cm := &connManager{log: discardLogger(), client: sc, poll: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cm.pollForAnswer(ctx, peer, "") }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pollForAnswer returned non-nil error after ctx cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pollForAnswer did not return within 1s after ctx cancel")
	}
}
