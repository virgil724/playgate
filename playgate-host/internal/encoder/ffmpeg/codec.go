package ffmpeg

import (
	"fmt"
	"strconv"
)

// Codec name constants. These are the values accepted in config (encoder.codec)
// and the ffmpeg encoder identifiers reported by `ffmpeg -encoders`.
const (
	// CodecLibX264 is the software (CPU) H.264 encoder. Default; always available
	// in a normal ffmpeg build.
	CodecLibX264 = "libx264"
	// CodecV4L2M2M is the Raspberry Pi / V4L2 stateful hardware H.264 encoder
	// (h264_v4l2m2m). Linux + a v4l2 m2m encode device.
	CodecV4L2M2M = "h264_v4l2m2m"
	// CodecVAAPI is the VA-API hardware H.264 encoder (h264_vaapi) for Intel /
	// AMD GPUs on Linux via a render node (e.g. /dev/dri/renderD128).
	CodecVAAPI = "h264_vaapi"
	// CodecNVENC is the NVIDIA hardware H.264 encoder (h264_nvenc).
	CodecNVENC = "h264_nvenc"
)

// DefaultVAAPIDevice is the render node VA-API uses when none is configured.
const DefaultVAAPIDevice = "/dev/dri/renderD128"

// CodecFromName maps a config string to a Codec implementation. An empty string
// selects the libx264 default. Unknown names return an error so misconfiguration
// surfaces at startup rather than silently falling back.
func CodecFromName(name string, vaapiDevice string) (Codec, error) {
	switch name {
	case "", CodecLibX264:
		return LibX264{}, nil
	case CodecV4L2M2M:
		return V4L2M2M{}, nil
	case CodecVAAPI:
		dev := vaapiDevice
		if dev == "" {
			dev = DefaultVAAPIDevice
		}
		return VAAPI{Device: dev}, nil
	case CodecNVENC:
		return NVENC{}, nil
	default:
		return nil, fmt.Errorf("encoder: unknown codec %q (want %s|%s|%s|%s)",
			name, CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC)
	}
}

// hwInitArgs is the optional Codec hook for input-side hardware-acceleration
// flags (e.g. VA-API device init + upload filter) that must appear before the
// input. A Codec that does not need any returns nil for both.
//
// Codecs implementing this interface have their InitArgs spliced in just before
// the "-i" input flag, and their FilterArgs appended after the input spec.
type hwInitArgs interface {
	// InitArgs returns flags that must precede the input (e.g. -vaapi_device).
	InitArgs(o Options) []string
	// FilterArgs returns a -vf filter chain applied to the decoded frames (e.g.
	// hwupload for VA-API). Empty means none.
	FilterArgs(o Options) []string
}

// rateControlArgs returns the bitrate flags shared by every codec: a hard target
// + maxrate with a small bufsize for low latency. Centralised so every codec
// applies the same low-latency rate-control discipline (and so ABR bitrate
// changes are honoured identically).
func rateControlArgs(o Options) []string {
	br := strconv.Itoa(o.Bitrate)
	return []string{
		"-b:v", br,
		"-maxrate", br,
		"-bufsize", strconv.Itoa(o.Bitrate / 2), // small buffer = low latency
	}
}

// gopArgs returns the GOP / no-B-frame flags shared by every codec. Short GOP +
// no B-frames keeps latency and recovery time low for live streaming.
func gopArgs(o Options) []string {
	g := strconv.Itoa(o.GOPSize)
	return []string{
		"-g", g,
		"-keyint_min", g,
		"-bf", "0",
	}
}

// --- h264_v4l2m2m (Raspberry Pi) -----------------------------------------

// V4L2M2M is the Raspberry Pi / generic V4L2 mem2mem stateful hardware H.264
// encoder. It has no x264-style preset/tune; low latency is expressed through
// short GOP, no B-frames and a tight rate-control buffer. The driver does NOT
// accept yuv420p directly on every platform, but ffmpeg's v4l2m2m wrapper
// handles the upload, so we still request yuv420p input frames.
type V4L2M2M struct{}

// Name implements Codec.
func (V4L2M2M) Name() string { return CodecV4L2M2M }

// OutputArgs implements Codec.
func (V4L2M2M) OutputArgs(o Options) []string {
	args := []string{"-c:v", CodecV4L2M2M}
	// v4l2m2m wants the frames as NV12/YUV420 in system memory; -pix_fmt before
	// the encoder converts. No preset/tune: those are x264-only.
	args = append(args, rateControlArgs(o)...)
	args = append(args, gopArgs(o)...)
	args = append(args, "-pix_fmt", "yuv420p")
	return args
}

// --- h264_vaapi (Intel / AMD) --------------------------------------------

// VAAPI is the VA-API hardware H.264 encoder for Intel / AMD GPUs. VA-API needs
// the frames uploaded to GPU surfaces, so it sets a device on the input side and
// inserts a "format=nv12,hwupload" filter; rate control uses CBR-ish maxrate.
type VAAPI struct {
	// Device is the DRM render node, e.g. /dev/dri/renderD128.
	Device string
}

// Name implements Codec.
func (VAAPI) Name() string { return CodecVAAPI }

// InitArgs implements hwInitArgs: select the VA-API device before the input.
func (v VAAPI) InitArgs(Options) []string {
	dev := v.Device
	if dev == "" {
		dev = DefaultVAAPIDevice
	}
	return []string{"-vaapi_device", dev}
}

// FilterArgs implements hwInitArgs: convert to nv12 and upload to a VA surface so
// the hardware encoder can consume the frames.
func (VAAPI) FilterArgs(Options) []string {
	return []string{"-vf", "format=nv12,hwupload"}
}

// OutputArgs implements Codec.
func (VAAPI) OutputArgs(o Options) []string {
	args := []string{"-c:v", CodecVAAPI}
	args = append(args, rateControlArgs(o)...)
	args = append(args, gopArgs(o)...)
	// VA-API low-latency: CBR rate control mode 2, no look-ahead.
	args = append(args, "-rc_mode", "CBR")
	return args
}

// --- h264_nvenc (NVIDIA) -------------------------------------------------

// NVENC is the NVIDIA hardware H.264 encoder. It has its own preset/tune
// vocabulary distinct from x264: the "p1" preset is fastest and "ll"/"ull"
// tunes target (ultra) low latency. We use the modern p-preset + low-latency
// tune and disable B-frames.
type NVENC struct{}

// Name implements Codec.
func (NVENC) Name() string { return CodecNVENC }

// OutputArgs implements Codec.
func (NVENC) OutputArgs(o Options) []string {
	args := []string{
		"-c:v", CodecNVENC,
		// nvenc-specific low-latency knobs (NOT x264 preset/tune values):
		"-preset", "p1", // fastest preset
		"-tune", "ll", // low-latency tune
		"-rc", "cbr", // constant bitrate for predictable bandwidth
		"-zerolatency", "1",
		"-delay", "0",
	}
	args = append(args, rateControlArgs(o)...)
	args = append(args, gopArgs(o)...)
	args = append(args, "-pix_fmt", "yuv420p")
	return args
}
