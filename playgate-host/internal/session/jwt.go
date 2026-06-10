package session

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// jwtHeader is the expected fixed header for EdDSA JWTs issued by
// playgate-server. Any other alg/typ combination is rejected.
const jwtHeader = `{"alg":"EdDSA","typ":"JWT"}`

// Claims contains the verified JWT payload.
type Claims struct {
	Issuer          string `json:"iss"`
	IssuedAt        int64  `json:"iat"`
	ExpiresAt       int64  `json:"exp"`
	RoomID          string `json:"room_id"`
	ViewerID        string `json:"viewer_id"`
	SessionSeconds  int    `json:"session_seconds"`
}

// ParseAndVerify parses a compact-serialised JWT, verifies the EdDSA signature
// using pubKey, checks that exp is in the future (relative to now), and returns
// the claims. It does NOT check room_id — the caller must do that.
//
// Only the exact header {"alg":"EdDSA","typ":"JWT"} is accepted. The payload
// must contain the fields documented in Claims. No third-party library is used.
func ParseAndVerify(token string, pubKey ed25519.PublicKey, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("jwt: malformed token (expected 3 dot-separated parts)")
	}

	// --- decode & check header ---
	headerJSON, err := base64URLDecode(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("jwt: decode header: %w", err)
	}
	// Normalise whitespace for comparison: unmarshal then re-encode canonical.
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return Claims{}, fmt.Errorf("jwt: parse header: %w", err)
	}
	if hdr.Alg != "EdDSA" || hdr.Typ != "JWT" {
		return Claims{}, fmt.Errorf("jwt: unsupported alg/typ %q/%q (want EdDSA/JWT)", hdr.Alg, hdr.Typ)
	}

	// --- decode payload ---
	payloadJSON, err := base64URLDecode(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("jwt: decode payload: %w", err)
	}

	// --- verify signature ---
	// The signed message is the ASCII encoding of header.payload (the first two
	// parts of the compact serialisation, not the decoded bytes).
	msg := []byte(parts[0] + "." + parts[1])
	sig, err := base64URLDecode(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("jwt: decode signature: %w", err)
	}
	if len(pubKey) != ed25519.PublicKeySize {
		return Claims{}, fmt.Errorf("jwt: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(pubKey))
	}
	if !ed25519.Verify(pubKey, msg, sig) {
		return Claims{}, errors.New("jwt: signature verification failed")
	}

	// --- parse claims ---
	var c Claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return Claims{}, fmt.Errorf("jwt: parse claims: %w", err)
	}

	// --- expiry check ---
	if c.ExpiresAt == 0 {
		return Claims{}, errors.New("jwt: missing exp claim")
	}
	if now.Unix() >= c.ExpiresAt {
		return Claims{}, errors.New("jwt: token has expired")
	}

	// --- required fields ---
	if c.RoomID == "" {
		return Claims{}, errors.New("jwt: missing room_id claim")
	}
	if c.ViewerID == "" {
		return Claims{}, errors.New("jwt: missing viewer_id claim")
	}
	if c.SessionSeconds <= 0 {
		return Claims{}, errors.New("jwt: session_seconds must be positive")
	}

	return c, nil
}

// base64URLDecode decodes a standard base64url-encoded string (no padding).
func base64URLDecode(s string) ([]byte, error) {
	// Add padding if necessary.
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}
