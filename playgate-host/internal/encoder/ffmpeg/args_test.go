package ffmpeg

import (
	"strings"
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

// hasFlag reports whether flag appears anywhere in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// hasPair reports whether flag is immediately followed by value at ANY position
// in args. (A flag like -f appears twice — once for input, once for output — so
// matching only the first occurrence would be wrong.)
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestBuildArgsYUYV(t *testing.T) {
	o := DefaultOptions(1280, 720, 30, core.PixelFormatYUYV)
	o.Bitrate = 4_000_000
	o.GOPSize = 15
	args, err := BuildArgs(o)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	// Input side: rawvideo demuxer with explicit geometry and pixel format.
	if !hasPair(args, "-f", "rawvideo") {
		t.Errorf("YUYV input should use -f rawvideo; args: %v", args)
	}
	if !hasPair(args, "-pixel_format", "yuyv422") {
		t.Errorf("YUYV input should set -pixel_format yuyv422; args: %v", args)
	}
	if !hasPair(args, "-video_size", "1280x720") {
		t.Errorf("YUYV input should set -video_size 1280x720; args: %v", args)
	}
	if !hasPair(args, "-i", "pipe:0") {
		t.Error("input should read from pipe:0")
	}

	// Output side: libx264, zerolatency, no B-frames, short GOP, bitrate.
	if !hasPair(args, "-c:v", "libx264") {
		t.Error("missing -c:v libx264")
	}
	if !hasPair(args, "-tune", "zerolatency") {
		t.Error("missing -tune zerolatency")
	}
	if !hasPair(args, "-bf", "0") {
		t.Error("missing -bf 0 (no B-frames)")
	}
	if !hasPair(args, "-g", "15") || !hasPair(args, "-keyint_min", "15") {
		t.Errorf("GOP size not propagated; args: %v", args)
	}
	if !hasPair(args, "-b:v", "4000000") || !hasPair(args, "-maxrate", "4000000") {
		t.Errorf("bitrate not propagated; args: %v", args)
	}
	if !hasPair(args, "-pix_fmt", "yuv420p") {
		t.Error("output should be yuv420p for decoder compatibility")
	}

	// Output container: raw h264 annex-b on pipe:1.
	if !hasPair(args, "-f", "h264") {
		t.Error("output format should be h264")
	}
	if args[len(args)-1] != "pipe:1" {
		t.Errorf("last arg should be pipe:1, got %q", args[len(args)-1])
	}
}

func TestBuildArgsMJPEG(t *testing.T) {
	o := DefaultOptions(1920, 1080, 60, core.PixelFormatMJPEG)
	args, err := BuildArgs(o)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	// MJPEG input uses the mjpeg demuxer; frames are self-describing JPEGs, so
	// no -video_size / -pixel_format should be emitted on the input side.
	if !hasPair(args, "-f", "mjpeg") {
		t.Errorf("MJPEG input should use -f mjpeg; args: %v", args)
	}
	if hasFlag(args, "-pixel_format") {
		t.Error("MJPEG input must not set -pixel_format")
	}
	if hasFlag(args, "-video_size") {
		t.Error("MJPEG input must not set -video_size")
	}
	if !hasPair(args, "-framerate", "60") {
		t.Error("MJPEG input should propagate framerate")
	}
}

func TestBuildArgsZeroLatencyDefaults(t *testing.T) {
	// A bare Options should normalise to low-latency defaults.
	o := Options{Width: 640, Height: 480, InputFormat: core.PixelFormatYUYV}
	args, err := BuildArgs(o)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !hasPair(args, "-tune", "zerolatency") {
		t.Error("default tune should be zerolatency")
	}
	if !hasPair(args, "-preset", "ultrafast") {
		t.Error("default preset should be ultrafast")
	}
	if !hasPair(args, "-bf", "0") {
		t.Error("default should disable B-frames")
	}
	if !hasFlag(args, "-fflags") || !hasPair(args, "-fflags", "nobuffer") {
		t.Error("default should set -fflags nobuffer")
	}
}

func TestBuildArgsValidation(t *testing.T) {
	if _, err := BuildArgs(Options{Width: 0, Height: 480, InputFormat: core.PixelFormatYUYV}); err == nil {
		t.Error("expected error for non-positive width")
	}
	if _, err := BuildArgs(Options{Width: 640, Height: 480, InputFormat: core.PixelFormatUnknown}); err == nil {
		t.Error("expected error for unsupported input format")
	}
}

func TestInputArgsUnsupported(t *testing.T) {
	o := Options{Width: 640, Height: 480, InputFormat: core.PixelFormatUnknown}
	if _, err := o.inputArgs(); err == nil {
		t.Error("inputArgs should reject unknown pixel format")
	}
}

func TestLibX264Name(t *testing.T) {
	if got := (LibX264{}).Name(); got != "libx264" {
		t.Errorf("codec name = %q, want libx264", got)
	}
}

// TestBufsizeIsHalfBitrate documents the low-latency rate-control choice.
func TestBufsizeIsHalfBitrate(t *testing.T) {
	o := DefaultOptions(1280, 720, 30, core.PixelFormatYUYV)
	o.Bitrate = 6_000_000
	args := (LibX264{}).OutputArgs(o.normalise())
	if !hasPair(args, "-bufsize", "3000000") {
		t.Errorf("bufsize should be half of bitrate; args: %s", strings.Join(args, " "))
	}
}
