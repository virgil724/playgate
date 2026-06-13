// Package v4l2 implements a core.CaptureSource backed by a Linux V4L2 device
// (e.g. an HDMI capture card exposed at /dev/video*). The Linux-specific
// device I/O lives in files guarded by the `linux` build tag; this file holds
// the platform-independent negotiation and parameter logic so it can be unit
// tested on any OS.
package v4l2

import (
	"fmt"
	"strings"

	"github.com/playgate/playgate-host/internal/core"
)

// FourCC packs four ASCII characters into the little-endian uint32 the V4L2
// API uses to identify a pixel format (a "FourCC"). It mirrors the v4l2_fourcc
// macro from <linux/videodev2.h> so the pure-logic code does not need cgo.
func FourCC(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

// Well-known V4L2 FourCC codes we care about. Values match
// V4L2_PIX_FMT_YUYV ("YUYV") and V4L2_PIX_FMT_MJPEG ("MJPG").
var (
	FourCCYUYV  = FourCC('Y', 'U', 'Y', 'V')
	FourCCMJPEG = FourCC('M', 'J', 'P', 'G')
	FourCCNV12  = FourCC('N', 'V', '1', '2')
)

// PixelFormatToFourCC maps a core.PixelFormat to its V4L2 FourCC code. The
// boolean is false for formats this capture source cannot request.
func PixelFormatToFourCC(pf core.PixelFormat) (uint32, bool) {
	switch pf {
	case core.PixelFormatYUYV:
		return FourCCYUYV, true
	case core.PixelFormatMJPEG:
		return FourCCMJPEG, true
	case core.PixelFormatNV12:
		return FourCCNV12, true
	default:
		return 0, false
	}
}

// FourCCToPixelFormat maps a V4L2 FourCC code back to a core.PixelFormat. The
// boolean is false for FourCC codes we do not model.
func FourCCToPixelFormat(fourcc uint32) (core.PixelFormat, bool) {
	switch fourcc {
	case FourCCYUYV:
		return core.PixelFormatYUYV, true
	case FourCCMJPEG:
		return core.PixelFormatMJPEG, true
	case FourCCNV12:
		return core.PixelFormatNV12, true
	default:
		return core.PixelFormatUnknown, false
	}
}

// ParsePixelFormat parses a human/config string ("YUYV", "MJPEG"/"MJPG") into a
// core.PixelFormat. It is case-insensitive and tolerant of surrounding space.
func ParsePixelFormat(s string) (core.PixelFormat, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "YUYV":
		return core.PixelFormatYUYV, nil
	case "MJPEG", "MJPG":
		return core.PixelFormatMJPEG, nil
	case "NV12":
		return core.PixelFormatNV12, nil
	default:
		return core.PixelFormatUnknown, fmt.Errorf("unsupported pixel format %q (want YUYV, NV12 or MJPEG)", s)
	}
}

// FourCCString renders a FourCC code as its four-character mnemonic for logging.
func FourCCString(fourcc uint32) string {
	b := [4]byte{
		byte(fourcc),
		byte(fourcc >> 8),
		byte(fourcc >> 16),
		byte(fourcc >> 24),
	}
	// Replace non-printable bytes so log output stays readable.
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b[:])
}

// Config is the platform-independent capture configuration. It is consumed both
// by the Linux capture source and by the negotiation logic below.
type Config struct {
	// Device is the V4L2 device path, e.g. "/dev/video0".
	Device string
	// Width / Height are the requested capture resolution in pixels.
	Width  int
	Height int
	// FPS is the requested capture frame rate.
	FPS int
	// PreferredFormats lists pixel formats in priority order. The first one the
	// device actually supports is chosen. If empty, DefaultPreferredFormats is
	// used.
	PreferredFormats []core.PixelFormat
}

// DefaultPreferredFormats is the fallback negotiation order: raw YUYV first
// (lowest latency, no decode), MJPEG second (smaller bandwidth on USB cards).
var DefaultPreferredFormats = []core.PixelFormat{
	core.PixelFormatYUYV,
	core.PixelFormatMJPEG,
}

// DefaultConfig returns a Config with sensible defaults (720p30 YUYV-preferred
// on /dev/video0).
func DefaultConfig() Config {
	return Config{
		Device:           "/dev/video0",
		Width:            1280,
		Height:           720,
		FPS:              30,
		PreferredFormats: append([]core.PixelFormat(nil), DefaultPreferredFormats...),
	}
}

// Validate checks the config for obviously-wrong values.
func (c Config) Validate() error {
	if c.Device == "" {
		return fmt.Errorf("capture device path must not be empty")
	}
	if c.Width <= 0 || c.Height <= 0 {
		return fmt.Errorf("capture resolution must be positive, got %dx%d", c.Width, c.Height)
	}
	if c.FPS <= 0 {
		return fmt.Errorf("capture fps must be positive, got %d", c.FPS)
	}
	for _, pf := range c.PreferredFormats {
		if _, ok := PixelFormatToFourCC(pf); !ok {
			return fmt.Errorf("preferred format %v is not requestable", pf)
		}
	}
	return nil
}

// preferred returns the effective preference list, substituting the default
// when none was supplied.
func (c Config) preferred() []core.PixelFormat {
	if len(c.PreferredFormats) == 0 {
		return DefaultPreferredFormats
	}
	return c.PreferredFormats
}

// NegotiateFormat picks the first preferred pixel format that appears in the
// device's advertised format set. It returns the chosen format and its FourCC.
//
// `available` is the set of FourCC codes the device reports (as obtained from
// VIDIOC_ENUM_FMT). Negotiation is intentionally pure so it can be unit tested
// without a device.
func NegotiateFormat(preferred []core.PixelFormat, available map[uint32]bool) (core.PixelFormat, uint32, error) {
	if len(preferred) == 0 {
		preferred = DefaultPreferredFormats
	}
	for _, pf := range preferred {
		fourcc, ok := PixelFormatToFourCC(pf)
		if !ok {
			continue
		}
		if available[fourcc] {
			return pf, fourcc, nil
		}
	}
	return core.PixelFormatUnknown, 0, fmt.Errorf("none of the preferred formats are supported by the device (available: %s)", formatAvailable(available))
}

// formatAvailable renders an available-format set for error messages.
func formatAvailable(available map[uint32]bool) string {
	if len(available) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(available))
	for fourcc, ok := range available {
		if ok {
			parts = append(parts, FourCCString(fourcc))
		}
	}
	return strings.Join(parts, ",")
}

// FrameBufferSize returns the buffer length (cap, defaults to 2) used for the
// Frames() channel. Buffer of 1-2 keeps latency low; the producer drops the
// oldest queued frame when the consumer falls behind (see source.go).
const FrameBufferSize = 2
