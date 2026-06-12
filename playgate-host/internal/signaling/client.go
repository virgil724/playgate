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

// Message is one entry in the GET /rooms poll response: the Worker's envelope
// (seq + ISO-8601 timestamp from the Worker's clock) around the raw payload.
type Message struct {
	Seq     int             `json:"seq"`
	Ts      string          `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// messagesResponse is the body of GET /rooms/{roomId}/{peer}.
type messagesResponse struct {
	Messages  []Message `json:"messages"`
	NextSince int       `json:"nextSince"`
}

// postResponse is the 201 body of POST /rooms/{roomId}/{peer}:
// { "seq": <int>, "ts": "<iso>" }. Older workers may send nothing.
type postResponse struct {
	Seq int    `json:"seq"`
	Ts  string `json:"ts"`
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

// PostOffer pushes an SDP offer onto the host queue and returns the
// Worker-assigned timestamp (ISO 8601, from the Worker's clock). An empty or
// unparseable response body (older Worker) yields a zero-value ts, not an
// error — callers must treat ts=="" as "unknown" and skip ts comparisons.
func (c *Client) PostOffer(ctx context.Context, sdp SDPMessage) (ts string, err error) {
	r, err := c.post(ctx, PeerHost, sdp)
	return r.Ts, err
}

// PostMessage pushes an arbitrary JSON payload onto the host queue (used for ICE
// candidates and any future host→viewer signaling).
func (c *Client) PostMessage(ctx context.Context, payload any) error {
	_, err := c.post(ctx, PeerHost, payload)
	return err
}

// post marshals payload and pushes it onto the given peer's queue, returning
// the Worker's seq/ts assignment (zero values when the body is absent).
func (c *Client) post(ctx context.Context, peer Peer, payload any) (postResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return postResponse{}, fmt.Errorf("signaling: marshal payload: %w", err)
	}
	url := fmt.Sprintf("%s/rooms/%s/%s", c.base, c.room, peer)
	req, err := c.newRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return postResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return postResponse{}, fmt.Errorf("signaling: post to %s: %w", peer, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return postResponse{}, fmt.Errorf("signaling: post to %s: unexpected status %d", peer, resp.StatusCode)
	}
	// Best-effort decode of {"seq":..,"ts":..}; older workers may answer with an
	// empty or different body, which is tolerated (zero values, no error).
	var out postResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return postResponse{}, nil
	}
	return out, nil
}

// Poll fetches messages posted by the OTHER peer since the given sequence. The
// host calls this with PeerHost (the Worker returns the viewer's messages). It
// returns the messages (envelope incl. seq/ts plus raw payload) and the next
// "since" value to use.
func (c *Client) Poll(ctx context.Context, self Peer, since int) ([]Message, int, error) {
	return c.PollWait(ctx, self, since, 0)
}

// PollWait is like Poll but appends &wait=<seconds> when wait > 0, asking a
// supporting Durable Object Worker to hold the request until a message arrives
// or the server-side timeout elapses. Old Workers that do not understand the
// parameter return immediately — callers should detect that case (response time
// < 1 second) and throttle themselves to avoid a hot-spin.
//
// When wait > 0 a per-request context deadline of wait+10s is applied so a
// stalled server cannot hold the connection longer than that. The shared
// http.Client timeout (typically 10s) is intentionally left unchanged; adding a
// per-request deadline via context is the correct mechanism for long-polls.
func (c *Client) PollWait(ctx context.Context, self Peer, since int, wait time.Duration) ([]Message, int, error) {
	rawURL := fmt.Sprintf("%s/rooms/%s/%s?since=%d", c.base, c.room, self, since)
	reqCtx := ctx
	httpClient := c.http
	if wait > 0 {
		waitSec := int(wait.Seconds())
		rawURL = fmt.Sprintf("%s&wait=%d", rawURL, waitSec)
		// http.Client.Timeout is an absolute cap that a context deadline can
		// only shorten, never extend — the shared client (typically 10s) would
		// kill a held 25s request. Use a copy with the cap removed and bound
		// the request with a wait+10s context deadline instead.
		lp := *c.http
		lp.Timeout = 0
		httpClient = &lp
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, wait+10*time.Second)
		defer cancel()
	}
	req, err := c.newRequest(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, since, err
	}
	resp, err := httpClient.Do(req)
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
	return out.Messages, out.NextSince, nil
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
