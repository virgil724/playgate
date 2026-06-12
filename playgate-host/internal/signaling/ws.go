package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// wsReadLimit caps a single WebSocket frame. SDP offers/answers are a few KB;
// 1 MB is generous headroom while still bounding memory against a hostile peer.
const wsReadLimit = 1 << 20 // 1 MiB

// WSConn is a receive-only WebSocket to the signaling Worker. The Worker replays
// the other peer's backlog on accept (one JSON frame per message) and then pushes
// new messages live; each frame has the same shape as a Message from the HTTP
// poll. The host never needs to send over this socket — offers still go through
// HTTP PostOffer — so only Receive and Close are exposed.
type WSConn struct {
	conn   *websocket.Conn
	log    interface{ Debug(string, ...any) }
	closed bool
}

// DialWS opens a WebSocket to {wsbase}/rooms/{room}/{self}/ws, deriving ws/wss
// from the Client's http/https base URL. When the Client carries a token it is
// appended as a ?token= query parameter (the canonical WS auth path) and also
// sent as an Authorization header for good measure. On accept the Worker begins
// replaying the other peer's backlog; read frames with Receive.
func (c *Client) DialWS(ctx context.Context, self Peer) (*WSConn, error) {
	wsURL, err := wsURLFor(c.base, c.room, self, c.token)
	if err != nil {
		return nil, err
	}

	opts := &websocket.DialOptions{}
	// Reuse the package's HTTP client where sensible, but never inherit its
	// Timeout: coder/websocket applies HTTPClient.Timeout as a deadline on the
	// whole connection lifetime, which would kill a long-lived idle read. Strip
	// it by shallow-copying the client.
	if c.http != nil {
		hc := *c.http
		hc.Timeout = 0
		opts.HTTPClient = &hc
	}
	if c.token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + c.token}}
	}

	conn, resp, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return nil, fmt.Errorf("signaling: ws dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	conn.SetReadLimit(wsReadLimit)
	return &WSConn{conn: conn, log: c.log}, nil
}

// wsURLFor builds the ws/wss endpoint URL for a room and peer, attaching the
// token as a query parameter when non-empty.
func wsURLFor(base, room string, self Peer, token string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("signaling: parse base url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
		// already a websocket scheme; leave as-is.
	default:
		return "", fmt.Errorf("signaling: unsupported base url scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + fmt.Sprintf("/rooms/%s/%s/ws", room, self)
	if token != "" {
		q := u.Query()
		q.Set("token", token)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Receive reads one frame and unmarshals it into a Message. Malformed frames are
// skipped (logged at Debug) and the next frame is read. It returns an error when
// the socket closes or ctx is done; the ctx bounds the blocking read, so a
// cancelled ctx aborts a parked Receive promptly.
func (w *WSConn) Receive(ctx context.Context) (Message, error) {
	for {
		typ, data, err := w.conn.Read(ctx)
		if err != nil {
			return Message{}, err
		}
		if typ != websocket.MessageText && typ != websocket.MessageBinary {
			continue
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			if w.log != nil {
				w.log.Debug("ws: skipping malformed frame", "err", err)
			}
			continue
		}
		return m, nil
	}
}

// Close closes the WebSocket. It is idempotent.
func (w *WSConn) Close() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	_ = w.conn.Close(websocket.StatusNormalClosure, "")
}
