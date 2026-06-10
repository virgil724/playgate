package ffmpeg

import (
	"strings"
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

// argsFor builds the full ffmpeg arg vector for a codec at a fixed geometry so
// the per-codec snapshot tests share one setup.
func argsFor(t *testing.T, codec Codec) []string {
	t.Helper()
	o := DefaultOptions(1280, 720, 30, core.PixelFormatYUYV)
	o.Bitrate = 5_000_000
	o.GOPSize = 30
	o.Codec = codec
	args, err := BuildArgs(o)
	if err != nil {
		t.Fatalf("BuildArgs(%s): %v", codec.Name(), err)
	}
	return args
}

// indexOf returns the position of the first occurrence of v in args, or -1.
func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}

func TestCodecFromName(t *testing.T) {
	cases := []struct {
		name     string
		want     string
		wantErr  bool
	}{
		{"", CodecLibX264, false},
		{CodecLibX264, CodecLibX264, false},
		{CodecV4L2M2M, CodecV4L2M2M, false},
		{CodecVAAPI, CodecVAAPI, false},
		{CodecNVENC, CodecNVENC, false},
		{"h265_magic", "", true},
	}
	for _, tc := range cases {
		c, err := CodecFromName(tc.name, "")
		if tc.wantErr {
			if err == nil {
				t.Errorf("CodecFromName(%q): expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("CodecFromName(%q): unexpected error %v", tc.name, err)
			continue
		}
		if c.Name() != tc.want {
			t.Errorf("CodecFromName(%q).Name() = %q, want %q", tc.name, c.Name(), tc.want)
		}
	}
}

func TestCodecFromNameVAAPIDevice(t *testing.T) {
	c, err := CodecFromName(CodecVAAPI, "/dev/dri/renderD129")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := c.(VAAPI)
	if !ok {
		t.Fatalf("expected VAAPI, got %T", c)
	}
	if v.Device != "/dev/dri/renderD129" {
		t.Errorf("device = %q, want /dev/dri/renderD129", v.Device)
	}

	// Empty device defaults to the standard render node.
	c, _ = CodecFromName(CodecVAAPI, "")
	if c.(VAAPI).Device != DefaultVAAPIDevice {
		t.Errorf("default device = %q, want %q", c.(VAAPI).Device, DefaultVAAPIDevice)
	}
}

// TestLibX264Args is the software-encoder snapshot: x264 preset/tune + shared
// rate control + GOP.
func TestLibX264Args(t *testing.T) {
	args := argsFor(t, LibX264{})
	if !hasPair(args, "-c:v", "libx264") {
		t.Error("missing -c:v libx264")
	}
	if !hasPair(args, "-preset", "ultrafast") || !hasPair(args, "-tune", "zerolatency") {
		t.Error("libx264 should keep x264 preset/tune")
	}
	if !hasPair(args, "-b:v", "5000000") || !hasPair(args, "-maxrate", "5000000") || !hasPair(args, "-bufsize", "2500000") {
		t.Errorf("rate control not applied; args: %s", strings.Join(args, " "))
	}
	if !hasPair(args, "-g", "30") || !hasPair(args, "-keyint_min", "30") || !hasPair(args, "-bf", "0") {
		t.Error("GOP/no-B-frame flags missing")
	}
	if !hasPair(args, "-pix_fmt", "yuv420p") {
		t.Error("expected yuv420p output")
	}
}

// TestV4L2M2MArgs is the Raspberry Pi snapshot: NO x264 preset/tune, shared rate
// control + GOP, yuv420p.
func TestV4L2M2MArgs(t *testing.T) {
	args := argsFor(t, V4L2M2M{})
	if !hasPair(args, "-c:v", "h264_v4l2m2m") {
		t.Error("missing -c:v h264_v4l2m2m")
	}
	// v4l2m2m has no x264 preset/tune vocabulary; emitting them would be rejected.
	if hasFlag(args, "-preset") || hasFlag(args, "-tune") {
		t.Errorf("v4l2m2m must NOT emit x264 -preset/-tune; args: %s", strings.Join(args, " "))
	}
	if !hasPair(args, "-b:v", "5000000") || !hasPair(args, "-maxrate", "5000000") {
		t.Error("rate control not applied")
	}
	if !hasPair(args, "-g", "30") || !hasPair(args, "-bf", "0") {
		t.Error("GOP/no-B-frame flags missing")
	}
}

// TestVAAPIArgs is the Intel/AMD snapshot: device init before input, hwupload
// filter after input, CBR rate control.
func TestVAAPIArgs(t *testing.T) {
	o := DefaultOptions(1280, 720, 30, core.PixelFormatYUYV)
	o.Bitrate = 5_000_000
	o.Codec = VAAPI{Device: "/dev/dri/renderD128"}
	args, err := BuildArgs(o)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(args, "-c:v", "h264_vaapi") {
		t.Error("missing -c:v h264_vaapi")
	}
	if !hasPair(args, "-vaapi_device", "/dev/dri/renderD128") {
		t.Error("missing -vaapi_device")
	}
	if !hasPair(args, "-vf", "format=nv12,hwupload") {
		t.Error("missing hwupload filter")
	}
	if !hasPair(args, "-rc_mode", "CBR") {
		t.Error("missing CBR rate control")
	}
	// Device init must come BEFORE the input; filter AFTER the input.
	devIdx := indexOf(args, "-vaapi_device")
	inIdx := indexOf(args, "-i")
	vfIdx := indexOf(args, "-vf")
	if !(devIdx >= 0 && inIdx >= 0 && vfIdx >= 0 && devIdx < inIdx && inIdx < vfIdx) {
		t.Errorf("ordering wrong: vaapi_device=%d input=%d vf=%d; args: %s",
			devIdx, inIdx, vfIdx, strings.Join(args, " "))
	}
	if hasFlag(args, "-tune") {
		t.Error("vaapi must not emit x264 -tune")
	}
}

// TestNVENCArgs is the NVIDIA snapshot: nvenc-specific preset/tune (p1/ll), CBR,
// zerolatency.
func TestNVENCArgs(t *testing.T) {
	args := argsFor(t, NVENC{})
	if !hasPair(args, "-c:v", "h264_nvenc") {
		t.Error("missing -c:v h264_nvenc")
	}
	// nvenc preset/tune values differ from x264: p1 / ll, not ultrafast / zerolatency.
	if !hasPair(args, "-preset", "p1") || !hasPair(args, "-tune", "ll") {
		t.Error("nvenc should use p1/ll preset/tune")
	}
	if hasPair(args, "-tune", "zerolatency") || hasPair(args, "-preset", "ultrafast") {
		t.Error("nvenc must NOT use x264 preset/tune values")
	}
	if !hasPair(args, "-rc", "cbr") || !hasPair(args, "-zerolatency", "1") {
		t.Error("nvenc low-latency rate control missing")
	}
	if !hasPair(args, "-b:v", "5000000") || !hasPair(args, "-g", "30") || !hasPair(args, "-bf", "0") {
		t.Error("shared rate-control/GOP flags missing")
	}
}

// TestAllCodecsHonourBitrate documents that every codec routes the configured
// bitrate through the same rate-control path so ABR restarts behave identically.
func TestAllCodecsHonourBitrate(t *testing.T) {
	for _, c := range []Codec{LibX264{}, V4L2M2M{}, VAAPI{}, NVENC{}} {
		o := DefaultOptions(640, 480, 30, core.PixelFormatYUYV)
		o.Bitrate = 1_234_000
		o.Codec = c
		args, err := BuildArgs(o)
		if err != nil {
			t.Fatalf("%s: %v", c.Name(), err)
		}
		if !hasPair(args, "-b:v", "1234000") {
			t.Errorf("%s: bitrate not propagated", c.Name())
		}
	}
}
