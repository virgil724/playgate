// Package api wires together HTTP handlers for the PlayGate server.
package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/playgate/playgate-server/internal/auth"
	"github.com/playgate/playgate-server/internal/db"
	"github.com/playgate/playgate-server/internal/queue"
)

// Server holds shared dependencies for all handlers.
type Server struct {
	db      *db.DB
	keys    *auth.KeyPair
	queue   *queue.Manager
	mux     *http.ServeMux
}

// New creates a Server and registers all routes.
func New(database *db.DB, keys *auth.KeyPair) *Server {
	s := &Server{
		db:    database,
		keys:  keys,
		queue: queue.New(),
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler. It applies permissive CORS so the
// browser frontend (playgate-web) can call the API cross-origin, and answers
// preflight OPTIONS requests directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Public
	s.mux.HandleFunc("GET /api/public-key", s.handleGetPublicKey)

	// Host management
	s.mux.HandleFunc("POST /api/hosts/register", s.handleRegisterHost)

	// Room management (host auth required)
	s.mux.HandleFunc("GET /api/rooms", s.handleListRooms)
	s.mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}", s.handleGetRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("POST /api/rooms/{id}/kick", s.handleKick)
	s.mux.HandleFunc("POST /api/rooms/{id}/tokens", s.handleIssueTokens)
	s.mux.HandleFunc("GET /api/rooms/{id}/tokens", s.handleListTokens)

	// Token management
	s.mux.HandleFunc("DELETE /api/tokens/{code}", s.handleRevokeToken)

	// Viewer redeem (no auth — only token code required)
	s.mux.HandleFunc("POST /api/tokens/{code}/redeem", s.handleRedeemToken)
}

// ---- /api/public-key ----

func (s *Server) handleGetPublicKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"algorithm":  "EdDSA",
		"public_key": s.keys.PublicKeyBase64(),
	})
}

// ---- /api/hosts/register ----

type registerHostRequest struct {
	Name string `json:"name"`
}

type registerHostResponse struct {
	HostID string `json:"host_id"`
	APIKey string `json:"api_key"`
}

func (s *Server) handleRegisterHost(w http.ResponseWriter, r *http.Request) {
	var req registerHostRequest
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	hostID := newID()
	apiKey := newSecret()

	h := &db.Host{
		ID:        hostID,
		APIKey:    apiKey,
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.CreateHost(h); err != nil {
		writeError(w, http.StatusInternalServerError, "create host failed")
		return
	}
	writeJSON(w, http.StatusCreated, registerHostResponse{HostID: hostID, APIKey: apiKey})
}

// ---- /api/rooms ----

type createRoomRequest struct {
	Name           string `json:"name"`
	SessionSeconds int    `json:"session_seconds"`
}

type roomResponse struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	SessionSeconds int     `json:"session_seconds"`
	Online         bool    `json:"online"`
	CurrentViewer  *string `json:"current_viewer"`
	QueueDepth     int     `json:"queue_depth"`
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}

	var req createRoomRequest
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.SessionSeconds <= 0 {
		req.SessionSeconds = 60
	}

	room := &db.Room{
		ID:             newID(),
		HostID:         host.ID,
		Name:           req.Name,
		SessionSeconds: req.SessionSeconds,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := s.db.CreateRoom(room); err != nil {
		writeError(w, http.StatusInternalServerError, "create room failed")
		return
	}
	writeJSON(w, http.StatusCreated, roomResponse{
		ID:             room.ID,
		Name:           room.Name,
		SessionSeconds: room.SessionSeconds,
	})
}

// ---- GET /api/rooms?host=me ----

func (s *Server) handleListRooms(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}

	rooms, err := s.db.ListRoomsByHostID(host.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := make([]roomResponse, 0, len(rooms))
	for _, room := range rooms {
		depth, _ := s.db.CountActiveSessionsInRoom(room.ID)
		out = append(out, roomResponse{
			ID:             room.ID,
			Name:           room.Name,
			SessionSeconds: room.SessionSeconds,
			Online:         room.Online,
			CurrentViewer:  room.CurrentViewer,
			QueueDepth:     depth,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// ---- GET /api/rooms/{id} ----

func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	room, err := s.db.GetRoom(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	depth, _ := s.db.CountActiveSessionsInRoom(id)
	writeJSON(w, http.StatusOK, roomResponse{
		ID:             room.ID,
		Name:           room.Name,
		SessionSeconds: room.SessionSeconds,
		Online:         room.Online,
		CurrentViewer:  room.CurrentViewer,
		QueueDepth:     depth,
	})
}

// ---- POST /api/rooms/{id}/heartbeat ----

type heartbeatRequest struct {
	Online        bool    `json:"online"`
	CurrentViewer *string `json:"current_viewer"`
}

type heartbeatResponse struct {
	Status        string `json:"status"`
	KickRequested bool   `json:"kick_requested"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	// Verify room belongs to this host.
	room, err := s.db.GetRoomByIDAndHostID(id, host.ID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.db.UpdateRoomHeartbeat(id, req.Online, req.CurrentViewer); err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}

	// Report any pending kick request to the host, then clear it (consume-once).
	kick := room.KickRequested
	if kick {
		if err := s.db.SetRoomKickRequested(id, false); err != nil {
			writeError(w, http.StatusInternalServerError, "clear kick failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, heartbeatResponse{Status: "ok", KickRequested: kick})
}

// ---- POST /api/rooms/{id}/kick ----

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	_, err := s.db.GetRoomByIDAndHostID(id, host.ID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	if err := s.db.SetRoomKickRequested(id, true); err != nil {
		writeError(w, http.StatusInternalServerError, "set kick failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "kick_requested"})
}

// ---- POST /api/rooms/{id}/tokens ----

type issueTokensRequest struct {
	Count int `json:"count"`
}

type issueTokensResponse struct {
	Codes []string `json:"codes"`
}

func (s *Server) handleIssueTokens(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	_, err := s.db.GetRoomByIDAndHostID(id, host.ID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	var req issueTokensRequest
	if err := decodeJSON(r, &req); err != nil || req.Count <= 0 {
		writeError(w, http.StatusBadRequest, "count must be > 0")
		return
	}
	if req.Count > 100 {
		writeError(w, http.StatusBadRequest, "count must be <= 100")
		return
	}

	codes := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code := newTokenCode()
		tok := &db.Token{
			Code:      code,
			RoomID:    id,
			HostID:    host.ID,
			CreatedAt: time.Now().UTC(),
		}
		if err := s.db.CreateToken(tok); err != nil {
			writeError(w, http.StatusInternalServerError, "create token failed")
			return
		}
		codes = append(codes, code)
	}

	writeJSON(w, http.StatusCreated, issueTokensResponse{Codes: codes})
}

// ---- GET /api/rooms/{id}/tokens ----

type tokenInfo struct {
	Code     string `json:"code"`
	Status   string `json:"status"` // "issued" | "redeemed" | "revoked"
	Redeemed bool   `json:"redeemed"`
	Revoked  bool   `json:"revoked"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	_, err := s.db.GetRoomByIDAndHostID(id, host.ID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	tokens, err := s.db.ListTokensByRoomID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := make([]tokenInfo, 0, len(tokens))
	for _, t := range tokens {
		status := "issued"
		switch {
		case t.Revoked:
			status = "revoked"
		case t.Redeemed:
			status = "redeemed"
		}
		out = append(out, tokenInfo{
			Code:     t.Code,
			Status:   status,
			Redeemed: t.Redeemed,
			Revoked:  t.Revoked,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// ---- DELETE /api/tokens/{code} ----

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireHost(w, r)
	if !ok {
		return
	}
	code := r.PathValue("code")

	tok, err := s.db.GetTokenByCodeAndHostID(code, host.ID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if tok.Redeemed {
		writeError(w, http.StatusConflict, "token already redeemed")
		return
	}
	if tok.Revoked {
		writeError(w, http.StatusConflict, "token already revoked")
		return
	}
	if err := s.db.RevokeToken(code); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- POST /api/tokens/{code}/redeem ----

type redeemResponse struct {
	SessionToken string `json:"session_token"`
	QueuePos     int    `json:"queue_position"`
	ExpiresAt    int64  `json:"expires_at"`
	RoomID       string `json:"room_id"`
	ViewerID     string `json:"viewer_id"`
}

func (s *Server) handleRedeemToken(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	tok, err := s.db.GetToken(code)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if tok.Revoked {
		writeError(w, http.StatusGone, "token revoked")
		return
	}
	if tok.Redeemed {
		writeError(w, http.StatusConflict, "token already redeemed")
		return
	}

	room, err := s.db.GetRoom(tok.RoomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "room not found")
		return
	}

	viewerID := newID()
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(room.SessionSeconds) * time.Second)

	claims := auth.Claims{
		Issuer:         "playgate-server",
		IssuedAt:       now.Unix(),
		ExpiresAt:      expiresAt.Unix(),
		RoomID:         tok.RoomID,
		ViewerID:       viewerID,
		SessionSeconds: room.SessionSeconds,
	}
	jwt, err := s.keys.IssueJWT(claims)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign token failed")
		return
	}

	if err := s.db.RedeemToken(code, viewerID); err != nil {
		writeError(w, http.StatusInternalServerError, "redeem failed")
		return
	}

	queuePos := s.queue.Next(tok.RoomID)

	sess := &db.Session{
		ID:        newID(),
		RoomID:    tok.RoomID,
		ViewerID:  viewerID,
		TokenCode: code,
		JWT:       jwt,
		QueuePos:  queuePos,
		Active:    true,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := s.db.CreateSession(sess); err != nil {
		writeError(w, http.StatusInternalServerError, "create session failed")
		return
	}

	writeJSON(w, http.StatusOK, redeemResponse{
		SessionToken: jwt,
		QueuePos:     queuePos,
		ExpiresAt:    expiresAt.Unix(),
		RoomID:       tok.RoomID,
		ViewerID:     viewerID,
	})
}

// ---- Auth helper ----

func (s *Server) requireHost(w http.ResponseWriter, r *http.Request) (*db.Host, bool) {
	key := bearerToken(r)
	if key == "" {
		writeError(w, http.StatusUnauthorized, "missing API key")
		return nil, false
	}
	host, err := s.db.GetHostByAPIKey(key)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return nil, false
	}
	return host, true
}

// ---- ID / secret generators ----

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newTokenCode() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
