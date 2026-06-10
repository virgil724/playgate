// Package core defines the shared types and interfaces that wire together every
// PlayGate Host module. It deliberately has no dependencies on other internal
// packages so that every downstream module (capture, encoder, webrtc, input,
// session) can import it without creating import cycles.
//
// Pipeline overview:
//
//	CaptureSource ──VideoFrame──▶ Encoder ──EncodedPacket──▶ WebRTC ──MediaTrack──▶ viewer
//	                                                          WebRTC ──DataChannel──▶ InputCommand ──▶ InputTarget
//
// Channel ownership / close semantics (the golden rule of this codebase):
//
//	The producer owns the channel. The goroutine that creates a channel is the
//	only goroutine allowed to write to it and to close it, and it MUST close it
//	exactly once when its Run loop returns. Consumers only ever read and must
//	tolerate a closed channel (the zero value / !ok). Lifetime is bounded by the
//	context.Context passed to Run: when the context is cancelled the producer
//	stops, drains, and closes its output channels before returning.
package core

import "time"

// PixelFormat enumerates the raw pixel layouts a CaptureSource can emit.
type PixelFormat int

const (
	// PixelFormatUnknown is the zero value and indicates an uninitialised frame.
	PixelFormatUnknown PixelFormat = iota
	// PixelFormatYUYV is packed YUV 4:2:2 (V4L2 FourCC "YUYV"), 16 bits/pixel.
	PixelFormatYUYV
	// PixelFormatMJPEG is a Motion-JPEG frame (each VideoFrame.Data is one JPEG).
	PixelFormatMJPEG
)

// String implements fmt.Stringer for friendly logging.
func (p PixelFormat) String() string {
	switch p {
	case PixelFormatYUYV:
		return "YUYV"
	case PixelFormatMJPEG:
		return "MJPEG"
	default:
		return "UNKNOWN"
	}
}

// VideoFrame is a single raw (un-encoded) frame produced by a CaptureSource.
//
// Ownership: the CaptureSource owns Data until the frame is handed to the
// channel; once sent, the receiver owns it. Producers MUST NOT retain or mutate
// Data after sending. Receivers MUST treat Data as read-only and copy it if they
// need to retain it past the current iteration, because producers are free to
// reuse backing buffers in a future revision.
type VideoFrame struct {
	Data        []byte
	PixelFormat PixelFormat
	Width       int
	Height      int
	Timestamp   time.Time
}

// EncodedPacket is a single compressed video access unit emitted by the encoder.
//
// Data is H.264 in Annex-B byte-stream format (start-code prefixed NAL units),
// which is what we feed directly into Pion's media track.
type EncodedPacket struct {
	Data       []byte
	PTS        time.Duration // presentation timestamp relative to stream start
	IsKeyframe bool          // true for IDR access units (seek/recovery points)
}

// InputCommand is one controller state snapshot decoded from the WebRTC
// DataChannel. It represents the *full* desired controller state at Timestamp,
// not a delta, so a dropped packet simply means a slightly stale state rather
// than a stuck button.
type InputCommand struct {
	// Buttons is a bitmask of pressed buttons; see the Button* constants.
	Buttons uint32
	// Analog stick axes, normalised to [-1, 1]. LX/LY = left stick, RX/RY = right.
	LX float32
	LY float32
	RX float32
	RY float32
	// Timestamp is when the command was generated on the viewer side.
	Timestamp time.Time
}

// Button bitmask values for InputCommand.Buttons. Layout mirrors a standard
// Switch Pro Controller; the concrete InputTarget maps these to its protocol.
const (
	ButtonA uint32 = 1 << iota
	ButtonB
	ButtonX
	ButtonY
	ButtonL
	ButtonR
	ButtonZL
	ButtonZR
	ButtonPlus
	ButtonMinus
	ButtonHome
	ButtonCapture
	ButtonLStick // left stick click
	ButtonRStick // right stick click
	ButtonDpadUp
	ButtonDpadDown
	ButtonDpadLeft
	ButtonDpadRight
)

// TargetStatus reports the connection state of an InputTarget (e.g. whether the
// virtual controller is currently paired/connected to the console).
type TargetStatus int

const (
	// TargetStatusUnknown is the zero value before the target reports anything.
	TargetStatusUnknown TargetStatus = iota
	// TargetStatusConnecting means the target is establishing the link.
	TargetStatusConnecting
	// TargetStatusConnected means commands sent via Send will reach the console.
	TargetStatusConnected
	// TargetStatusDisconnected means the link is down; Send may fail or buffer.
	TargetStatusDisconnected
)

// String implements fmt.Stringer for friendly logging.
func (s TargetStatus) String() string {
	switch s {
	case TargetStatusConnecting:
		return "connecting"
	case TargetStatusConnected:
		return "connected"
	case TargetStatusDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}
