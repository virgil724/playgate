package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeTestKeys generates a fresh ed25519 key pair for use in tests.
func makeTestKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// signJWT builds a compact JWT signed with priv. The header is always
// {"alg":"EdDSA","typ":"JWT"}.
func signJWT(t *testing.T, priv ed25519.PrivateKey, claims Claims) string {
	t.Helper()

	headerJSON := `{"alg":"EdDSA","typ":"JWT"}`
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	h := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))
	p := base64.RawURLEncoding.EncodeToString(payloadBytes)
	msg := h + "." + p

	sig := ed25519.Sign(priv, []byte(msg))
	s := base64.RawURLEncoding.EncodeToString(sig)
	return msg + "." + s
}

func now() time.Time { return time.Unix(1_718_000_000, 0) }

func TestParseAndVerify_Valid(t *testing.T) {
	pub, priv := makeTestKeys(t)

	claims := Claims{
		Issuer:         "playgate-server",
		IssuedAt:       now().Unix(),
		ExpiresAt:      now().Add(2 * time.Minute).Unix(),
		RoomID:         "deadbeef",
		ViewerID:       "cafe1234",
		SessionSeconds: 120,
	}
	token := signJWT(t, priv, claims)

	got, err := ParseAndVerify(token, pub, now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ViewerID != claims.ViewerID {
		t.Errorf("viewer_id: got %q, want %q", got.ViewerID, claims.ViewerID)
	}
	if got.RoomID != claims.RoomID {
		t.Errorf("room_id: got %q, want %q", got.RoomID, claims.RoomID)
	}
	if got.SessionSeconds != claims.SessionSeconds {
		t.Errorf("session_seconds: got %d, want %d", got.SessionSeconds, claims.SessionSeconds)
	}
}

func TestParseAndVerify_Expired(t *testing.T) {
	_, priv := makeTestKeys(t)
	pub, _ := makeTestKeys(t) // wrong key — but expiry check is first
	pub, priv = func() (ed25519.PublicKey, ed25519.PrivateKey) {
		p, k, _ := ed25519.GenerateKey(rand.Reader)
		return p, k
	}()

	claims := Claims{
		Issuer:         "playgate-server",
		IssuedAt:       now().Add(-5 * time.Minute).Unix(),
		ExpiresAt:      now().Add(-1 * time.Minute).Unix(), // already expired
		RoomID:         "deadbeef",
		ViewerID:       "cafe1234",
		SessionSeconds: 120,
	}
	token := signJWT(t, priv, claims)

	_, err := ParseAndVerify(token, pub, now())
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected 'expired' in error, got %q", err.Error())
	}
}

func TestParseAndVerify_BadSignature(t *testing.T) {
	pub, _ := makeTestKeys(t)
	_, priv2 := makeTestKeys(t) // different key for signing

	claims := Claims{
		Issuer:         "playgate-server",
		IssuedAt:       now().Unix(),
		ExpiresAt:      now().Add(2 * time.Minute).Unix(),
		RoomID:         "deadbeef",
		ViewerID:       "cafe1234",
		SessionSeconds: 120,
	}
	token := signJWT(t, priv2, claims)

	_, err := ParseAndVerify(token, pub, now())
	if err == nil {
		t.Fatal("expected error for bad signature, got nil")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("expected 'signature' in error, got %q", err.Error())
	}
}

func TestParseAndVerify_WrongAlg(t *testing.T) {
	pub, priv := makeTestKeys(t)

	// Manually build a token with alg=HS256.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := Claims{
		ExpiresAt:      now().Add(time.Minute).Unix(),
		RoomID:         "r",
		ViewerID:       "v",
		SessionSeconds: 60,
	}
	payloadBytes, _ := json.Marshal(claims)
	pay := base64.RawURLEncoding.EncodeToString(payloadBytes)
	msg := hdr + "." + pay
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(msg)))
	token := msg + "." + sig

	_, err := ParseAndVerify(token, pub, now())
	if err == nil {
		t.Fatal("expected error for wrong alg, got nil")
	}
	if !strings.Contains(err.Error(), "EdDSA") {
		t.Errorf("expected alg mention in error, got %q", err.Error())
	}
}

func TestParseAndVerify_MalformedToken(t *testing.T) {
	pub, _ := makeTestKeys(t)
	_, err := ParseAndVerify("not.a.valid.jwt.here", pub, now())
	if err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestParseAndVerify_MissingClaims(t *testing.T) {
	pub, priv := makeTestKeys(t)
	cases := []struct {
		name    string
		claims  Claims
		wantErr string
	}{
		{
			name: "missing room_id",
			claims: Claims{
				ExpiresAt: now().Add(time.Minute).Unix(), ViewerID: "v",
				SessionSeconds: 60,
			},
			wantErr: "room_id",
		},
		{
			name: "missing viewer_id",
			claims: Claims{
				ExpiresAt: now().Add(time.Minute).Unix(), RoomID: "r",
				SessionSeconds: 60,
			},
			wantErr: "viewer_id",
		},
		{
			name: "session_seconds zero",
			claims: Claims{
				ExpiresAt: now().Add(time.Minute).Unix(), RoomID: "r",
				ViewerID: "v", SessionSeconds: 0,
			},
			wantErr: "session_seconds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := signJWT(t, priv, tc.claims)
			_, err := ParseAndVerify(token, pub, now())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want %q in error, got %q", tc.wantErr, err.Error())
			}
		})
	}
}
