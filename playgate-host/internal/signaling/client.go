// Package signaling is a stdlib-only HTTP client for the PlayGate signaling
// Worker (playgate-signaling, T7/T8). It speaks the Worker's REST API:
//
//	POST /rooms/{roomId}/{peer}            push a JSON message (peer = host|viewer)
//	GET  /rooms/{roomId}/{peer}?since=n    poll the OTHER peer's messages
//	POST /turn/credentials                 fetch ICE servers (STUN + TURN)
//
// The host is always the "host" peer and the offerer: it posts an SDP offer to
// its own queue, then polls the "viewer" queue for the viewer's answer and ICE
// candidates. (The Worker's GET returns messages from the *other* peer, so the
// host polls /rooms/{room}/host to read what the viewer posted — see PollViewer.)
//
// This package is transport only; it does not import internal/rtc, so it can be
// unit-tested against httptest without any WebRTC machinery. The host wiring
// layer (internal/host) glues a Client to a *rtc.Peer.
package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Peer identifies which side of a room a message belongs to.
type Peer string

const (
	// PeerHost is the streaming host (this process); the offerer.
	PeerHost Peer = "host"
	// PeerViewer is the browser viewer; the answerer.
	PeerViewer Peer = "viewer"
)

// SDPMessage is the JSON shape of an SDP offer/answer payload exchanged through
// the Worker. It matches what the browser posts and what internal/rtc produces
// (webrtc.SessionDescription marshals to {"type":..,"sdp":..}).
type SDPMessage struct {
	Type string `json:"type"` // "offer" | "answer"
	SDP  string `json:"sdp"`
}

// ICEServer mirrors the iceServers entries returned by POST /turn/credentials
// and accepted by the browser / Pion. URLs may be a single string or an array;
// see turnURLs for normalisation.
type ICEServer struct {
	URLs       json.RawMessage `json:"urls"`
	Username   string          `json:"username,omitempty"`
	Credential string          `json:"credential,omitempty"`
}

// turnCredentialsResponse is the body of POST /turn/credentials.
type turnCredentialsResponse struct {
	ICEServers []ICEServer `json:"iceServers"`
	TTL        int         `json:"ttl"`
}

// message is one entry in the GET /rooms poll response.
type message struct {
	Seq     int             `json:"seq"`
	Ts      string          `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// messagesResponse is the body of GET /rooms/{roomId}/{peer}.
type messagesResponse struct {
	Messages  []message `json:"messages"`
	NextSince int       `json:"nextSince"`
}

// Config configures a signaling Client.
type Config struct {
	// BaseURL is the Worker root, e.g. "https://x.workers.dev" or
	// "http://localhost:8787". A trailing slash is tolerated.
	BaseURL string
	// RoomID is the room this host joins.
	RoomID string
	// Token is an optional bearer token sent as Authorization on every request.
	Token string
	// HTTPClient is used for all requests; nil means a default client with a
	// 10s timeout.
	HTTPClient *http.Client
	// Logger receives diagnostics; nil means a discard logger.
	Logger *slog.Logger
}

// Client talks to the signaling Worker. It is safe for concurrent use.
type Client struct {
	base  string
	room  string
	token string
	http  *http.Client
	log   *slog.Logger
}

// New constructs a Client. BaseURL and RoomID are required.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("signaling: BaseURL must not be empty")
	}
	if strings.TrimSpace(cfg.RoomID) == "" {
		return nil, fmt.Errorf("signaling: RoomID must not be empty")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{
		base:  strings.TrimRight(cfg.BaseURL, "/"),
		room:  cfg.RoomID,
		token: cfg.Token,
		http:  hc,
		log:   log.With("module", "signaling", "room", cfg.RoomID),
	}, nil
}

// TURNCredentials fetches ICE servers from the Worker. On any error the caller
// should fall back to a plain STUN configuration.
func (c *Client) TURNCredentials(ctx context.Context) ([]ICEServer, error) {
	body := []byte(`{}`)
	req, err := c.newRequest(ctx, http.MethodPost, c.base+"/turn/credentials", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signaling: turn credentials request: %w", err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signaling: turn credentials: unexpected status %d", resp.StatusCode)
	}
	var out turnCredentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("signaling: decode turn credentials: %w", err)
	}
	return out.ICEServers, nil
}

// PostOffer pushes an SDP offer onto the host queue.
func (c *Client) PostOffer(ctx context.Context, sdp SDPMessage) error {
	return c.post(ctx, PeerHost, sdp)
}

// PostMessage pushes an arbitrary JSON payload onto the host queue (used for ICE
// candidates and any future host→viewer signaling).
func (c *Client) PostMessage(ctx context.Context, payload any) error {
	return c.post(ctx, PeerHost, payload)
}

// post marshals payload and pushes it onto the given peer's queue.
func (c *Client) post(ctx context.Context, peer Peer, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("signaling: marshal payload: %w", err)
	}
	url := fmt.Sprintf("%s/rooms/%s/%s", c.base, c.room, peer)
	req, err := c.newRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("signaling: post to %s: %w", peer, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signaling: post to %s: unexpected status %d", peer, resp.StatusCode)
	}
	return nil
}

// Poll fetches messages posted by the OTHER peer since the given sequence. The
// host calls this with PeerHost (the Worker returns the viewer's messages). It
// returns the raw payloads and the next "since" value to use.
func (c *Client) Poll(ctx context.Context, self Peer, since int) ([]json.RawMessage, int, error) {
	url := fmt.Sprintf("%s/rooms/%s/%s?since=%d", c.base, c.room, self, since)
	req, err := c.newRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, since, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, since, fmt.Errorf("signaling: poll %s: %w", self, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, since, fmt.Errorf("signaling: poll %s: unexpected status %d", self, resp.StatusCode)
	}
	var out messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, since, fmt.Errorf("signaling: decode poll response: %w", err)
	}
	payloads := make([]json.RawMessage, 0, len(out.Messages))
	for _, m := range out.Messages {
		payloads = append(payloads, m.Payload)
	}
	return payloads, out.NextSince, nil
}

// newRequest builds a request with the optional bearer token attached.
func (c *Client) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, fmt.Errorf("signaling: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// drainClose drains and closes a response body so the underlying connection can
// be reused by the keep-alive pool.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
