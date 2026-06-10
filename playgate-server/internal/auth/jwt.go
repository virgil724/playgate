// Package auth handles ed25519 key management and JWT signing for session tokens.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// KeyPair holds an ed25519 signing key pair.
type KeyPair struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// LoadOrGenerate loads the key pair from disk; generates and saves if not found.
func LoadOrGenerate(path string) (*KeyPair, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parsePrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	// Generate new key pair.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	kp := &KeyPair{Private: priv, Public: pub}
	if err := savePrivateKey(path, priv); err != nil {
		return nil, fmt.Errorf("save key: %w", err)
	}
	return kp, nil
}

func savePrivateKey(path string, priv ed25519.PrivateKey) error {
	block := &pem.Block{
		Type:  "ED25519 PRIVATE KEY",
		Bytes: priv.Seed(),
	}
	return os.WriteFile(path, pem.EncodeToMemory(block), 0600)
}

func parsePrivateKey(data []byte) (*KeyPair, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM data")
	}
	if len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("unexpected seed length %d", len(block.Bytes))
	}
	priv := ed25519.NewKeyFromSeed(block.Bytes)
	return &KeyPair{Private: priv, Public: priv.Public().(ed25519.PublicKey)}, nil
}

// PublicKeyBase64 returns the public key as standard base64.
func (kp *KeyPair) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(kp.Public)
}

// ---- JWT (compact, no library dependency) ----
// We implement a minimal JWT with ed25519 (EdDSA / OKP) since no external
// JWT library is required.

// Claims represents the payload of a PlayGate session JWT.
// All fields use the canonical JWT names where applicable.
type Claims struct {
	// Standard
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`

	// PlayGate-specific
	RoomID          string `json:"room_id"`
	ViewerID        string `json:"viewer_id"`
	SessionSeconds  int    `json:"session_seconds"`
}

const jwtHeader = `{"alg":"EdDSA","typ":"JWT"}`

// IssueJWT creates and signs a JWT for a session.
func (kp *KeyPair) IssueJWT(c Claims) (string, error) {
	headerB64 := base64.RawURLEncoding.EncodeToString([]byte(jwtHeader))

	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)

	sigInput := headerB64 + "." + payloadB64
	sig := ed25519.Sign(kp.Private, []byte(sigInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return sigInput + "." + sigB64, nil
}

// VerifyJWT verifies and parses a JWT signed with the given ed25519 public key.
func VerifyJWT(token string, pub ed25519.PublicKey) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT")
	}

	sigInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(pub, []byte(sigInput), sig) {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}
	if time.Now().Unix() > c.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}
	return &c, nil
}
