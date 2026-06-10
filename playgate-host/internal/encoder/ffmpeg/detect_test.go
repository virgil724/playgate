package ffmpeg

import "testing"

// sampleEncoderOutput mimics the relevant lines of `ffmpeg -hide_banner
// -encoders` (header + legend + a few encoder rows).
const sampleEncoderOutput = `Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
 V....D libx265              libx265 H.265 / HEVC
 V..... h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V..... h264_vaapi           H.264/AVC (VAAPI) (codec h264)
 A....D aac                  AAC (Advanced Audio Coding)
`

func TestParseEncoderList(t *testing.T) {
	set := parseEncoderList(sampleEncoderOutput)
	for _, want := range []string{"libx264", "libx265", "h264_nvenc", "h264_vaapi", "aac"} {
		if !set[want] {
			t.Errorf("expected encoder %q in parsed set", want)
		}
	}
	// Header / legend / separator lines must not become encoders.
	for _, notWant := range []string{"Encoders:", "------", "=", "Video"} {
		if set[notWant] {
			t.Errorf("did not expect %q in parsed set", notWant)
		}
	}
	// h264_v4l2m2m is absent from this build.
	if set["h264_v4l2m2m"] {
		t.Error("h264_v4l2m2m should be absent")
	}
}

func TestDetectCodecPresent(t *testing.T) {
	avail := parseEncoderList(sampleEncoderOutput)
	if err := DetectCodec("h264_nvenc", avail); err != nil {
		t.Errorf("nvenc is present, expected nil, got %v", err)
	}
	if err := DetectCodec("", avail); err != nil {
		t.Errorf("default libx264 present, expected nil, got %v", err)
	}
}

func TestDetectCodecMissing(t *testing.T) {
	avail := parseEncoderList(sampleEncoderOutput)
	err := DetectCodec("h264_v4l2m2m", avail)
	if err == nil {
		t.Fatal("expected error for missing codec")
	}
	// The error must name the fallback and the available encoders (actionable).
	msg := err.Error()
	for _, want := range []string{"h264_v4l2m2m", "libx264", "h264_nvenc"} {
		if !contains(msg, want) {
			t.Errorf("error message %q should mention %q", msg, want)
		}
	}
}

func TestDetectCodecProbeUnavailable(t *testing.T) {
	// Empty map = probe failed; detection is skipped (best-effort), never blocks.
	if err := DetectCodec("h264_vaapi", nil); err != nil {
		t.Errorf("nil availability should skip detection, got %v", err)
	}
	if err := DetectCodec("h264_vaapi", map[string]bool{}); err != nil {
		t.Errorf("empty availability should skip detection, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
