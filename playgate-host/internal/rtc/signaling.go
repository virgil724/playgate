package rtc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"
)

// EncodeSDP serialises a SessionDescription to a base64 string suitable for
// copy-paste manual signaling (and later for transport over the Cloudflare
// Worker in T7). The description is JSON-encoded then base64 (std encoding).
func EncodeSDP(desc webrtc.SessionDescription) (string, error) {
	raw, err := json.Marshal(desc)
	if err != nil {
		return "", fmt.Errorf("rtc: marshal sdp: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeSDP reverses EncodeSDP.
func DecodeSDP(s string) (webrtc.SessionDescription, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("rtc: base64 decode sdp: %w", err)
	}
	var desc webrtc.SessionDescription
	if err := json.Unmarshal(raw, &desc); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("rtc: unmarshal sdp: %w", err)
	}
	return desc, nil
}
