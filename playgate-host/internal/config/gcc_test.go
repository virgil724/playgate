package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebRTCGCCConfig(t *testing.T) {
	if Default().WebRTC.GCC {
		t.Fatal("webrtc.gcc must default to false")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("encoder:\n  backend: libav\nwebrtc:\n  gcc: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WebRTC.GCC {
		t.Error("webrtc.gcc: true not applied")
	}
}

func TestVideoTransportValidate(t *testing.T) {
	c := Default()
	if c.WebRTC.VideoTransport != VideoTransportTrack || c.WebRTC.VideoFragmentBytes != 1200 {
		t.Fatalf("defaults wrong: %+v", c.WebRTC)
	}
	c.WebRTC.VideoTransport = "bogus"
	if c.Validate() == nil {
		t.Fatal("bogus transport must be rejected")
	}
	c.WebRTC.VideoTransport = VideoTransportAuto
	c.WebRTC.VideoFragmentBytes = 100
	if c.Validate() == nil {
		t.Fatal("fragment size out of range must be rejected")
	}
	c.WebRTC.VideoFragmentBytes = 1200
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWebRTCGCCRequiresLibav(t *testing.T) {
	c := Default()
	c.WebRTC.GCC = true
	c.Encoder.Backend = BackendCLI
	if err := c.Validate(); err == nil {
		t.Fatal("gcc with cli backend must be rejected")
	}
	c.Encoder.Backend = BackendLibav
	if err := c.Validate(); err != nil {
		t.Fatalf("gcc with libav: %v", err)
	}
}

func TestVideoFECValidate(t *testing.T) {
	for _, fec := range []string{"", "off", "xor", "rs"} {
		cfg := Default()
		cfg.WebRTC.VideoFEC = fec
		if err := cfg.Validate(); err != nil {
			t.Errorf("video_fec %q: %v", fec, err)
		}
	}
	bad := Default()
	bad.WebRTC.VideoFEC = "ldpc"
	if err := bad.Validate(); err == nil {
		t.Error("unknown video_fec should fail validation")
	}
	for _, r := range []float64{-0.1, 1.5} {
		cfg := Default()
		cfg.WebRTC.VideoFECRatio = r
		if err := cfg.Validate(); err == nil {
			t.Errorf("video_fec_ratio %v should fail validation", r)
		}
	}
	if d := Default().WebRTC; d.VideoFEC != "off" || d.VideoFECRatio != 0.1 {
		t.Errorf("defaults: video_fec=%q ratio=%v", d.VideoFEC, d.VideoFECRatio)
	}
}

// TestVideoFECYAMLOff checks an unquoted `off` decodes as the string "off"
// (yaml.v3 does not treat it as a YAML 1.1 boolean for a string field).
func TestVideoFECYAMLOff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("webrtc:\n  video_fec: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.WebRTC.VideoFEC != "off" {
		t.Errorf("video_fec = %q, want off", cfg.WebRTC.VideoFEC)
	}
}
