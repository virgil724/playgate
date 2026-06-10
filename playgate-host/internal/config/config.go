// Package config loads and validates the PlayGate Host YAML configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration object loaded from the YAML file.
type Config struct {
	Capture   CaptureConfig   `yaml:"capture"`
	Encoder   EncoderConfig   `yaml:"encoder"`
	WebRTC    WebRTCConfig    `yaml:"webrtc"`
	Input     InputConfig     `yaml:"input"`
	Signaling SignalingConfig `yaml:"signaling"`
}

// CaptureConfig configures the video capture source (T2: v4l2).
type CaptureConfig struct {
	// Device is the capture device path, e.g. /dev/video0.
	Device string `yaml:"device"`
	// Width / Height are the requested capture resolution.
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
	// FPS is the requested capture frame rate.
	FPS int `yaml:"fps"`
	// Format is the requested pixel format string, e.g. "YUYV" or "MJPEG".
	Format string `yaml:"format"`
}

// EncoderConfig configures the H.264 encoder (T3: ffmpeg subprocess).
type EncoderConfig struct {
	// Bitrate is the target video bitrate in bits per second.
	Bitrate int `yaml:"bitrate"`
	// Preset is the ffmpeg x264 preset, e.g. "ultrafast".
	Preset string `yaml:"preset"`
	// KeyframeInterval is the GOP size in frames.
	KeyframeInterval int `yaml:"keyframe_interval"`
	// FFmpegPath is the path to the ffmpeg binary.
	FFmpegPath string `yaml:"ffmpeg_path"`
}

// WebRTCConfig configures the Pion WebRTC peer connection (T4).
type WebRTCConfig struct {
	// ICEServers is the list of STUN/TURN server URLs.
	ICEServers []string `yaml:"ice_servers"`
}

// InputConfig configures the controller input target (T5: NXBT bridge).
type InputConfig struct {
	// SocketPath is the Unix socket path of the NXBT bridge.
	SocketPath string `yaml:"socket_path"`
}

// SignalingConfig configures the connection to the signaling server.
type SignalingConfig struct {
	// URL is the websocket URL of the signaling server.
	URL string `yaml:"url"`
	// RoomID identifies which streaming room this host joins.
	RoomID string `yaml:"room_id"`
}

// Default returns a Config populated with sensible defaults. Load starts from
// these defaults and overlays the YAML file on top, so unspecified fields keep
// their default value.
func Default() Config {
	return Config{
		Capture: CaptureConfig{
			Device: "/dev/video0",
			Width:  1280,
			Height: 720,
			FPS:    30,
			Format: "YUYV",
		},
		Encoder: EncoderConfig{
			Bitrate:          6_000_000,
			Preset:           "ultrafast",
			KeyframeInterval: 60,
			FFmpegPath:       "ffmpeg",
		},
		WebRTC: WebRTCConfig{
			ICEServers: []string{"stun:stun.l.google.com:19302"},
		},
		Input: InputConfig{
			SocketPath: "/run/nxbt/nxbt.sock",
		},
		Signaling: SignalingConfig{
			URL:    "ws://localhost:8080/ws",
			RoomID: "default",
		},
	}
}

// Load reads the YAML file at path, overlaying it on top of Default(), then
// validates the result.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return cfg, nil
}

// Validate checks the configuration for obviously-wrong values. It is kept
// intentionally light for T1; downstream modules add their own checks.
func (c Config) Validate() error {
	if c.Capture.Width <= 0 || c.Capture.Height <= 0 {
		return fmt.Errorf("capture resolution must be positive, got %dx%d", c.Capture.Width, c.Capture.Height)
	}
	if c.Capture.FPS <= 0 {
		return fmt.Errorf("capture fps must be positive, got %d", c.Capture.FPS)
	}
	if c.Encoder.Bitrate <= 0 {
		return fmt.Errorf("encoder bitrate must be positive, got %d", c.Encoder.Bitrate)
	}
	if c.Signaling.URL == "" {
		return fmt.Errorf("signaling url must not be empty")
	}
	return nil
}
