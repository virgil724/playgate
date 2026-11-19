package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOverlaysDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Only override a couple of fields; the rest should keep defaults.
	content := "capture:\n  width: 1920\n  height: 1080\nsignaling:\n  room_id: stream42\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Capture.Width != 1920 || cfg.Capture.Height != 1080 {
		t.Errorf("resolution not applied: %dx%d", cfg.Capture.Width, cfg.Capture.Height)
	}
	if cfg.Capture.FPS != Default().Capture.FPS {
		t.Errorf("default FPS not preserved: got %d", cfg.Capture.FPS)
	}
	if cfg.Signaling.RoomID != "stream42" {
		t.Errorf("room_id not applied: %q", cfg.Signaling.RoomID)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("capture:\n  width: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for zero width, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateCodec(t *testing.T) {
	cfg := Default()
	for _, c := range []string{"", CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC} {
		cfg.Encoder.Codec = c
		if err := cfg.Validate(); err != nil {
			t.Errorf("codec %q should validate: %v", c, err)
		}
	}
	cfg.Encoder.Codec = "h265_bogus"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown codec")
	}
}

func TestValidateGatewayInput(t *testing.T) {
	cfg := Default()
	cfg.Input.Target = InputGateway
	if err := cfg.Validate(); err != nil {
		t.Fatalf("gateway defaults should validate: %v", err)
	}
	if cfg.Input.Gateway.Port != "auto" || cfg.Input.Gateway.RateHz != 150 || cfg.Input.Gateway.KeepaliveMS != 40 {
		t.Errorf("gateway defaults = %+v", cfg.Input.Gateway)
	}
	cfg.Input.Gateway.KeepaliveMS = 100
	if err := cfg.Validate(); err == nil {
		t.Error("keepalive_ms 100 must be rejected (gateway times out at 100ms)")
	}
	cfg.Input.Gateway.KeepaliveMS = 40
	cfg.Input.Gateway.RateHz = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative rate_hz must be rejected")
	}
}

func TestValidateIntraRefresh(t *testing.T) {
	cfg := Default()
	cfg.Encoder.IntraRefresh = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("libx264 intra_refresh should validate: %v", err)
	}
	cfg.Encoder.Codec = CodecNVENC
	if err := cfg.Validate(); err == nil {
		t.Error("intra_refresh with nvenc must be rejected")
	}
}

func TestEncoderCodecKindDefault(t *testing.T) {
	cfg := Default()
	cfg.Encoder.Codec = ""
	if cfg.EncoderCodecKind() != CodecLibX264 {
		t.Errorf("empty codec should default to libx264, got %q", cfg.EncoderCodecKind())
	}
	cfg.Encoder.Codec = CodecNVENC
	if cfg.EncoderCodecKind() != CodecNVENC {
		t.Errorf("codec kind = %q, want nvenc", cfg.EncoderCodecKind())
	}
}

func TestValidateABR(t *testing.T) {
	cfg := Default()
	cfg.ABR.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("default ABR config should validate when enabled: %v", err)
	}

	bad := Default()
	bad.ABR.Enabled = true
	bad.ABR.MinBitrate = 5_000_000
	bad.ABR.MaxBitrate = 1_000_000
	if err := bad.Validate(); err == nil {
		t.Error("expected error for max<min")
	}

	bad = Default()
	bad.ABR.Enabled = true
	bad.ABR.LossThreshold = 1.5
	if err := bad.Validate(); err == nil {
		t.Error("expected error for loss_threshold>=1")
	}

	// When disabled, bogus ABR values are tolerated (not used).
	off := Default()
	off.ABR.Enabled = false
	off.ABR.MinBitrate = 0
	if err := off.Validate(); err != nil {
		t.Errorf("disabled ABR should skip validation: %v", err)
	}
}

func TestValidateEncoderBackend(t *testing.T) {
	cfg := Default()
	for _, b := range []string{"", BackendCLI, BackendLibav} {
		cfg.Encoder.Backend = b
		if err := cfg.Validate(); err != nil {
			t.Errorf("backend %q: %v", b, err)
		}
	}
	cfg.Encoder.Backend = "gstreamer"
	if err := cfg.Validate(); err == nil {
		t.Error("bogus backend should fail validation")
	}
	cfg.Encoder.Backend = ""
	if cfg.EncoderBackendKind() != BackendCLI {
		t.Errorf("empty backend should default to cli")
	}
}

func TestGOPFrames(t *testing.T) {
	e := EncoderConfig{GOPSeconds: 1}
	if got := e.GOPFrames(30); got != 30 {
		t.Errorf("1s@30fps = %d, want 30", got)
	}
	e.GOPSeconds = 2.5
	if got := e.GOPFrames(30); got != 75 {
		t.Errorf("2.5s@30fps = %d, want 75", got)
	}
	e.GOPSeconds = 0
	if got := e.GOPFrames(60); got != 120 {
		t.Errorf("unset gop_seconds on cli should mean 2s, got %d", got)
	}
	e.Backend = BackendLibav
	if got := e.GOPFrames(60); got != 180 {
		t.Errorf("unset gop_seconds on libav should mean 3s, got %d", got)
	}
	e.GOPSeconds = 1 // explicit value wins over the backend default
	if got := e.GOPFrames(60); got != 60 {
		t.Errorf("explicit gop_seconds on libav = %d, want 60", got)
	}
	e.Backend = ""
	e.KeyframeInterval = 45 // explicit frames wins
	e.GOPSeconds = 3
	if got := e.GOPFrames(60); got != 45 {
		t.Errorf("keyframe_interval should take precedence, got %d", got)
	}
	if got := Default().Encoder.GOPFrames(30); got != 60 {
		t.Errorf("default GOP = %d, want 60 (2s)", got)
	}
	cfg := Default()
	cfg.Encoder.Backend = BackendLibav
	if got := cfg.Encoder.GOPFrames(30); got != 90 {
		t.Errorf("default libav GOP = %d, want 90 (3s)", got)
	}
}

func TestKeyframeDebounce(t *testing.T) {
	cfg := Default()
	if got := cfg.KeyframeDebounce(); got != time.Second {
		t.Errorf("cli default = %v, want 1s", got)
	}
	cfg.Encoder.Backend = BackendLibav
	if got := cfg.KeyframeDebounce(); got != 150*time.Millisecond {
		t.Errorf("libav default = %v, want 150ms", got)
	}
	cfg.WebRTC.VideoKeyframeMinIntervalMS = 400 // legacy alias applies when unset
	if got := cfg.KeyframeDebounce(); got != 400*time.Millisecond {
		t.Errorf("legacy alias = %v, want 400ms", got)
	}
	cfg.Encoder.KeyframeDebounceMS = 90 // new knob wins
	if got := cfg.KeyframeDebounce(); got != 90*time.Millisecond {
		t.Errorf("explicit = %v, want 90ms", got)
	}
	cfg.Encoder.KeyframeDebounceMS = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative keyframe_debounce_ms should be rejected")
	}
}

func TestValidateGOP(t *testing.T) {
	cfg := Default()
	cfg.Encoder.GOPSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative gop_seconds should be rejected")
	}
	cfg = Default()
	cfg.Encoder.KeyframeInterval = -5
	if err := cfg.Validate(); err == nil {
		t.Error("negative keyframe_interval should be rejected")
	}
}
