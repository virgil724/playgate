package v4l2

import (
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

func TestFourCCRoundTrip(t *testing.T) {
	// "YUYV" little-endian should equal the documented V4L2 value 0x56595559.
	if got := FourCC('Y', 'U', 'Y', 'V'); got != 0x56595559 {
		t.Errorf("FourCC(YUYV) = %#x, want 0x56595559", got)
	}
	if got := FourCC('M', 'J', 'P', 'G'); got != 0x47504a4d {
		t.Errorf("FourCC(MJPG) = %#x, want 0x47504a4d", got)
	}
	if got := FourCCString(FourCCYUYV); got != "YUYV" {
		t.Errorf("FourCCString = %q, want YUYV", got)
	}
	if got := FourCCString(FourCCMJPEG); got != "MJPG" {
		t.Errorf("FourCCString = %q, want MJPG", got)
	}
}

func TestPixelFormatMapping(t *testing.T) {
	for _, pf := range []core.PixelFormat{core.PixelFormatYUYV, core.PixelFormatMJPEG} {
		fourcc, ok := PixelFormatToFourCC(pf)
		if !ok {
			t.Fatalf("PixelFormatToFourCC(%v) not ok", pf)
		}
		back, ok := FourCCToPixelFormat(fourcc)
		if !ok || back != pf {
			t.Errorf("round trip %v -> %#x -> %v", pf, fourcc, back)
		}
	}
	if _, ok := PixelFormatToFourCC(core.PixelFormatUnknown); ok {
		t.Error("unknown format should not be requestable")
	}
}

func TestParsePixelFormat(t *testing.T) {
	cases := map[string]core.PixelFormat{
		"YUYV":    core.PixelFormatYUYV,
		"yuyv":    core.PixelFormatYUYV,
		" MJPEG ": core.PixelFormatMJPEG,
		"mjpg":    core.PixelFormatMJPEG,
	}
	for in, want := range cases {
		got, err := ParsePixelFormat(in)
		if err != nil {
			t.Errorf("ParsePixelFormat(%q) err: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePixelFormat(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParsePixelFormat("rgb24"); err == nil {
		t.Error("expected error for unsupported format")
	}
}

func TestNegotiateFormat(t *testing.T) {
	tests := []struct {
		name      string
		preferred []core.PixelFormat
		available map[uint32]bool
		want      core.PixelFormat
		wantErr   bool
	}{
		{
			name:      "prefers first available",
			preferred: []core.PixelFormat{core.PixelFormatYUYV, core.PixelFormatMJPEG},
			available: map[uint32]bool{FourCCYUYV: true, FourCCMJPEG: true},
			want:      core.PixelFormatYUYV,
		},
		{
			name:      "falls back to second when first missing",
			preferred: []core.PixelFormat{core.PixelFormatYUYV, core.PixelFormatMJPEG},
			available: map[uint32]bool{FourCCMJPEG: true},
			want:      core.PixelFormatMJPEG,
		},
		{
			name:      "respects preference order over availability order",
			preferred: []core.PixelFormat{core.PixelFormatMJPEG, core.PixelFormatYUYV},
			available: map[uint32]bool{FourCCYUYV: true, FourCCMJPEG: true},
			want:      core.PixelFormatMJPEG,
		},
		{
			name:      "empty preference uses default (YUYV first)",
			preferred: nil,
			available: map[uint32]bool{FourCCYUYV: true, FourCCMJPEG: true},
			want:      core.PixelFormatYUYV,
		},
		{
			name:      "no overlap errors",
			preferred: []core.PixelFormat{core.PixelFormatYUYV},
			available: map[uint32]bool{FourCC('R', 'G', 'B', '3'): true},
			wantErr:   true,
		},
		{
			name:      "empty available errors",
			preferred: []core.PixelFormat{core.PixelFormatYUYV},
			available: map[uint32]bool{},
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pf, fourcc, err := NegotiateFormat(tc.preferred, tc.available)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v / %#x", pf, fourcc)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pf != tc.want {
				t.Errorf("got %v, want %v", pf, tc.want)
			}
			wantFourCC, _ := PixelFormatToFourCC(tc.want)
			if fourcc != wantFourCC {
				t.Errorf("fourcc = %#x, want %#x", fourcc, wantFourCC)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	good := DefaultConfig()
	if err := good.Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}

	bad := []Config{
		{Device: "", Width: 1280, Height: 720, FPS: 30},
		{Device: "/dev/video0", Width: 0, Height: 720, FPS: 30},
		{Device: "/dev/video0", Width: 1280, Height: 720, FPS: 0},
		{Device: "/dev/video0", Width: 1280, Height: 720, FPS: 30, PreferredFormats: []core.PixelFormat{core.PixelFormatUnknown}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("bad config %d passed validation", i)
		}
	}
}

func TestDeviceInfoAvailableSet(t *testing.T) {
	d := DeviceInfo{Formats: []FormatInfo{
		{FourCC: FourCCYUYV},
		{FourCC: FourCCMJPEG},
	}}
	set := d.AvailableFourCCSet()
	if !set[FourCCYUYV] || !set[FourCCMJPEG] {
		t.Errorf("missing formats in set: %v", set)
	}
	if len(set) != 2 {
		t.Errorf("set size = %d, want 2", len(set))
	}
}

func TestFormatDeviceListRendering(t *testing.T) {
	if got := FormatDeviceList(nil); got == "" {
		t.Error("empty list should render a message")
	}
	devs := []DeviceInfo{
		{
			Path: "/dev/video1", Card: "Cam B",
			Formats: []FormatInfo{{FourCC: FourCCMJPEG, Description: "MJPEG", FrameSizes: []FrameSize{{1920, 1080}}}},
		},
		{
			Path: "/dev/video0", Card: "Cam A",
			Formats: []FormatInfo{{FourCC: FourCCYUYV, Description: "YUYV", FrameSizes: []FrameSize{{1280, 720}, {640, 480}}}},
		},
	}
	out := FormatDeviceList(devs)
	// video0 must sort before video1.
	if i0, i1 := indexOf(out, "/dev/video0"), indexOf(out, "/dev/video1"); i0 < 0 || i1 < 0 || i0 > i1 {
		t.Errorf("devices not sorted by path:\n%s", out)
	}
	if indexOf(out, "1280x720") < 0 {
		t.Errorf("resolution missing from output:\n%s", out)
	}
}

// indexOf is a tiny helper to avoid importing strings just for tests.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
