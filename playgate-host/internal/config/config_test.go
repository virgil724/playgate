package config

import (
	"os"
	"path/filepath"
	"testing"
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
