// Package session implements the PlayGate Host control-authority manager (T9).
//
// # Business model
//
// Viewers exchange tokens with playgate-server for a time-limited JWT that
// grants control of the host for session_seconds. Only one viewer holds control
// at a time; others are queued or rejected depending on the configured Policy.
// Video is never interrupted; only controller input is gated.
//
// # State machine
//
//	              Claim()
//	    idle ──────────────────────────▶ active
//	                                       │
//	                     session_seconds   │   idle_timeout
//	                     elapsed           │   (no input)
//	                    ┌──────────────────┴──────────────────┐
//	                    ▼                                      ▼
//	                 expired                              idle_kicked
//	                    │                                      │
//	                    └──────────────────┬───────────────────┘
//	                                       │
//	                         next in FIFO queue?
//	                        yes ──────▶ active (new viewer)
//	                        no  ──────▶ idle
//
// # Integration with T4 / T6 (WebRTC DataChannels)
//
// The T4 rtc module maintains one DataChannel per connected viewer. For each
// viewer the rtc module calls Manager.Gate to obtain a gated output channel:
//
//	rawIn  := make(chan core.InputCommand) // filled by DataChannel reads
//	gatedOut := manager.Gate(viewerID, rawIn)
//	// forward gatedOut to the input module
//
// Gate returns a channel that only forwards commands when viewerID is the
// current active controller. When the session expires or the viewer is kicked,
// Gate closes gatedOut automatically so the rtc module can clean up.
//
// The Manager also exposes Events(), a channel of SessionEvent values. The T6
// signaling / control-channel module should listen on this channel and push the
// events as JSON over the reliable control DataChannel to the frontend (T11).
// See SessionEvent and its JSON shape documented on the type.
//
// # JWT verification
//
// JWT verification is fully in stdlib (crypto/ed25519, encoding/base64). No
// third-party JWT library is added. See jwt.go for the parser.
package session
