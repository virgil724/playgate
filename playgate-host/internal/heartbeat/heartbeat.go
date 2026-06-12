// Package heartbeat sends periodic heartbeats to playgate-server so the
// streamer console can show the room's online status and current controller,
// and so the host can honour Force kick requests.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/session"
)

// ViewerSource is implemented by anything that can report the viewer_id of the
// current controller. *session.Manager satisfies this interface.
type ViewerSource interface {
	CurrentViewerID() string
}

// KickFunc is called when the server reports kick_requested=true. It must kick
// the current controller using the same path the idle-kick uses.
type KickFunc func()

// heartbeatRequest matches POST /api/rooms/{id}/heartbeat request body.
type heartbeatRequest struct {
	Online        bool    `json:"online"`
	CurrentViewer *string `json:"current_viewer"`
}

// heartbeatResponse matches the 200-OK body returned by the server.
type heartbeatResponse struct {
	Status        string `json:"status"`
	KickRequested bool   `json:"kick_requested"`
}

// Runner sends heartbeats on a fixed interval until its context is cancelled.
type Runner struct {
	log      *slog.Logger
	url      string
	roomID   string
	apiKey   string
	interval time.Duration
	viewers  ViewerSource // nil when session gating is disabled
	kick     KickFunc     // called when kick_requested=true; may be nil
	client   *http.Client
}

// New builds a Runner from cfg. viewers may be nil (session disabled); kick may
// be nil (no kick action). The caller must check cfg.Server.URL != "" before
// calling New — this constructor does not validate the URL.
func New(log *slog.Logger, cfg config.Config, viewers ViewerSource, kick KickFunc) *Runner {
	interval := time.Duration(cfg.Server.HeartbeatIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Runner{
		log:      log.With("module", "heartbeat"),
		url:      cfg.Server.URL,
		roomID:   cfg.Signaling.RoomID,
		apiKey:   cfg.Server.APIKey,
		interval: interval,
		viewers:  viewers,
		kick:     kick,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Run sends a heartbeat immediately, then repeats on each interval tick until
// ctx is cancelled. It never returns a non-nil error (failures are logged and
// the loop continues), making it safe to run in an errgroup.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("heartbeat loop started",
		"url", r.url,
		"room", r.roomID,
		"interval", r.interval)

	// Send one heartbeat right away so the console shows online immediately.
	r.beat(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Debug("heartbeat loop stopped")
			return nil
		case <-ticker.C:
			r.beat(ctx)
		}
	}
}

// beat sends one heartbeat POST and acts on the response.
func (r *Runner) beat(ctx context.Context) {
	// Build request body.
	req := heartbeatRequest{Online: true}
	if r.viewers != nil {
		if vid := r.viewers.CurrentViewerID(); vid != "" {
			req.CurrentViewer = &vid
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		r.log.Warn("heartbeat marshal failed", "err", err)
		return
	}

	endpoint := fmt.Sprintf("%s/api/rooms/%s/heartbeat", r.url, r.roomID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		r.log.Warn("heartbeat request build failed", "err", err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// Context cancelled: don't log as a warning.
			return
		}
		r.log.Warn("heartbeat request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		r.log.Warn("heartbeat non-200 response", "status", resp.StatusCode)
		return
	}

	var hbResp heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		r.log.Warn("heartbeat response decode failed", "err", err)
		return
	}

	r.log.Debug("heartbeat ok", "kick_requested", hbResp.KickRequested)

	if hbResp.KickRequested && r.kick != nil {
		r.log.Info("kick requested by server; kicking current controller")
		r.kick()
	}
}

// MakeKickFunc returns a KickFunc that kicks the current controller through the
// session manager's idle-kick path (endSession with EventIdleKicked). This is
// the same mechanism the idle-timeout uses.
func MakeKickFunc(mgr *session.Manager) KickFunc {
	return func() {
		mgr.KickCurrentController()
	}
}
