package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/playgate/playgate-server/internal/api"
	"github.com/playgate/playgate-server/internal/auth"
	"github.com/playgate/playgate-server/internal/db"
)

// ---- test helpers ----

func setupServer(t *testing.T) http.Handler {
	t.Helper()

	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	keyPath := filepath.Join(dir, "key.pem")
	keys, err := auth.LoadOrGenerate(keyPath)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	return api.New(database, keys)
}

func post(t *testing.T, srv http.Handler, path string, body any, authKey string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, srv http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func del(t *testing.T, srv http.Handler, path string, authKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected status %d, got %d (body=%s)", want, rec.Code, rec.Body.String())
	}
}

// ---- tests ----

func TestFullFlow(t *testing.T) {
	srv := setupServer(t)

	// 1. Register host.
	rec := post(t, srv, "/api/hosts/register", map[string]any{"name": "streamer1"}, "")
	mustStatus(t, rec, http.StatusCreated)
	body := decodeBody(t, rec)
	apiKey, _ := body["api_key"].(string)
	if apiKey == "" {
		t.Fatal("expected api_key in response")
	}

	// 2. Create room.
	rec = post(t, srv, "/api/rooms", map[string]any{"name": "Room A", "session_seconds": 120}, apiKey)
	mustStatus(t, rec, http.StatusCreated)
	body = decodeBody(t, rec)
	roomID, _ := body["id"].(string)
	if roomID == "" {
		t.Fatal("expected room id in response")
	}

	// 3. Issue tokens (batch of 3).
	rec = post(t, srv, "/api/rooms/"+roomID+"/tokens", map[string]any{"count": 3}, apiKey)
	mustStatus(t, rec, http.StatusCreated)
	body = decodeBody(t, rec)
	rawCodes, _ := body["codes"].([]any)
	if len(rawCodes) != 3 {
		t.Fatalf("expected 3 codes, got %d", len(rawCodes))
	}
	code0 := rawCodes[0].(string)
	code1 := rawCodes[1].(string)
	code2 := rawCodes[2].(string)

	// 4. Redeem first token.
	rec = post(t, srv, "/api/tokens/"+code0+"/redeem", map[string]any{}, "")
	mustStatus(t, rec, http.StatusOK)
	body = decodeBody(t, rec)
	sessionToken, _ := body["session_token"].(string)
	if sessionToken == "" {
		t.Fatal("expected session_token")
	}
	queuePos, _ := body["queue_position"].(float64)
	if queuePos != 1 {
		t.Fatalf("expected queue_position=1, got %v", queuePos)
	}

	// 5. Verify JWT signature using public key from /api/public-key.
	rec2 := get(t, srv, "/api/public-key")
	mustStatus(t, rec2, http.StatusOK)
	pkBody := decodeBody(t, rec2)
	pkB64, _ := pkBody["public_key"].(string)
	pkBytes, err := base64.StdEncoding.DecodeString(pkB64)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	pub := ed25519.PublicKey(pkBytes)
	claims, err := auth.VerifyJWT(sessionToken, pub)
	if err != nil {
		t.Fatalf("verify JWT: %v", err)
	}
	if claims.RoomID != roomID {
		t.Fatalf("JWT room_id mismatch: got %s, want %s", claims.RoomID, roomID)
	}
	if claims.SessionSeconds != 120 {
		t.Fatalf("JWT session_seconds mismatch: got %d", claims.SessionSeconds)
	}
	if claims.ViewerID == "" {
		t.Fatal("JWT viewer_id should not be empty")
	}
	if claims.Issuer != "playgate-server" {
		t.Fatalf("JWT iss mismatch: %s", claims.Issuer)
	}

	// 6. Cannot redeem the same token twice.
	rec = post(t, srv, "/api/tokens/"+code0+"/redeem", map[string]any{}, "")
	mustStatus(t, rec, http.StatusConflict)

	// 7. Redeem second token — queue position increments.
	rec = post(t, srv, "/api/tokens/"+code1+"/redeem", map[string]any{}, "")
	mustStatus(t, rec, http.StatusOK)
	body = decodeBody(t, rec)
	queuePos2, _ := body["queue_position"].(float64)
	if queuePos2 != 2 {
		t.Fatalf("expected queue_position=2, got %v", queuePos2)
	}

	// 8. Revoke third token before redeem.
	rec = del(t, srv, "/api/tokens/"+code2, apiKey)
	mustStatus(t, rec, http.StatusNoContent)

	// Redeeming revoked token should fail.
	rec = post(t, srv, "/api/tokens/"+code2+"/redeem", map[string]any{}, "")
	mustStatus(t, rec, http.StatusGone)

	// 9. Heartbeat.
	viewer := "viewer-xyz"
	rec = post(t, srv, "/api/rooms/"+roomID+"/heartbeat",
		map[string]any{"online": true, "current_viewer": viewer}, apiKey)
	mustStatus(t, rec, http.StatusOK)

	// 10. Query room status.
	rec = get(t, srv, "/api/rooms/"+roomID)
	mustStatus(t, rec, http.StatusOK)
	body = decodeBody(t, rec)
	if body["online"] != true {
		t.Fatal("expected room online=true")
	}
	if body["current_viewer"] != viewer {
		t.Fatalf("expected current_viewer=%s, got %v", viewer, body["current_viewer"])
	}
}

func TestRegisterHostMissingName(t *testing.T) {
	srv := setupServer(t)
	rec := post(t, srv, "/api/hosts/register", map[string]any{}, "")
	mustStatus(t, rec, http.StatusBadRequest)
}

func TestUnauthorized(t *testing.T) {
	srv := setupServer(t)
	rec := post(t, srv, "/api/rooms", map[string]any{"name": "x"}, "")
	mustStatus(t, rec, http.StatusUnauthorized)
}

func TestRoomNotFound(t *testing.T) {
	srv := setupServer(t)
	rec := get(t, srv, "/api/rooms/doesnotexist")
	mustStatus(t, rec, http.StatusNotFound)
}

func TestTokenNotFound(t *testing.T) {
	srv := setupServer(t)
	rec := post(t, srv, "/api/tokens/badcode/redeem", map[string]any{}, "")
	mustStatus(t, rec, http.StatusNotFound)
}

func TestPublicKey(t *testing.T) {
	srv := setupServer(t)
	rec := get(t, srv, "/api/public-key")
	mustStatus(t, rec, http.StatusOK)
	body := decodeBody(t, rec)
	alg, _ := body["algorithm"].(string)
	if alg != "EdDSA" {
		t.Fatalf("expected EdDSA, got %s", alg)
	}
	pk, _ := body["public_key"].(string)
	pkBytes, err := base64.StdEncoding.DecodeString(pk)
	if err != nil {
		t.Fatalf("invalid base64: %v", err)
	}
	if len(pkBytes) != ed25519.PublicKeySize {
		t.Fatalf("wrong key length: %d", len(pkBytes))
	}
}

func TestKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")

	kp1, err := auth.LoadOrGenerate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := auth.LoadOrGenerate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if kp1.PublicKeyBase64() != kp2.PublicKeyBase64() {
		t.Fatal("key should persist across calls")
	}
}

func TestIssueTokensCount(t *testing.T) {
	srv := setupServer(t)

	rec := post(t, srv, "/api/hosts/register", map[string]any{"name": "h"}, "")
	mustStatus(t, rec, http.StatusCreated)
	apiKey := decodeBody(t, rec)["api_key"].(string)

	rec = post(t, srv, "/api/rooms", map[string]any{"name": "R"}, apiKey)
	mustStatus(t, rec, http.StatusCreated)
	roomID := decodeBody(t, rec)["id"].(string)

	// count=0 should fail
	rec = post(t, srv, "/api/rooms/"+roomID+"/tokens", map[string]any{"count": 0}, apiKey)
	mustStatus(t, rec, http.StatusBadRequest)

	// count=101 should fail
	rec = post(t, srv, "/api/rooms/"+roomID+"/tokens", map[string]any{"count": 101}, apiKey)
	mustStatus(t, rec, http.StatusBadRequest)
}

func TestHeartbeatWrongHost(t *testing.T) {
	srv := setupServer(t)

	rec := post(t, srv, "/api/hosts/register", map[string]any{"name": "h1"}, "")
	mustStatus(t, rec, http.StatusCreated)
	apiKey1 := decodeBody(t, rec)["api_key"].(string)

	rec = post(t, srv, "/api/hosts/register", map[string]any{"name": "h2"}, "")
	mustStatus(t, rec, http.StatusCreated)
	apiKey2 := decodeBody(t, rec)["api_key"].(string)

	rec = post(t, srv, "/api/rooms", map[string]any{"name": "R"}, apiKey1)
	mustStatus(t, rec, http.StatusCreated)
	roomID := decodeBody(t, rec)["id"].(string)

	// h2 should not be able to heartbeat h1's room
	rec = post(t, srv, "/api/rooms/"+roomID+"/heartbeat",
		map[string]any{"online": true}, apiKey2)
	mustStatus(t, rec, http.StatusNotFound)
}

// Ensure the test file doesn't have import issues with os/strings.
var _ = os.Getenv
var _ = strings.Contains
