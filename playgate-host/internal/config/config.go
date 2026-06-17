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
	Audio     AudioConfig     `yaml:"audio"`
	ABR       ABRConfig       `yaml:"abr"`
	WebRTC    WebRTCConfig    `yaml:"webrtc"`
	Input     InputConfig     `yaml:"input"`
	Session   SessionConfig   `yaml:"session"`
	Signaling SignalingConfig `yaml:"signaling"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Server    ServerConfig    `yaml:"server"`
}

// Capture source kinds.
const (
	// CaptureV4L2 reads from a Linux V4L2 device (production; Linux only).
	CaptureV4L2 = "v4l2"
	// CaptureSynthetic generates a pure-Go test pattern (dev mode, any OS).
	CaptureSynthetic = "synthetic"
)

// Input target kinds.
const (
	// InputNXBT forwards commands to the NXBT daemon over a socket (TCP or Unix).
	InputNXBT = "nxbt"
	// InputLog logs commands instead of forwarding them (dev mode, any OS).
	InputLog = "log"
)

// CaptureConfig configures the video capture source (T2: v4l2 / synthetic).
type CaptureConfig struct {
	// Source selects the capture backend: "v4l2" (production) or "synthetic"
	// (pure-Go test pattern for development on machines without a capture card).
	Source string `yaml:"source"`
	// Device is the capture device path, e.g. /dev/video0 (v4l2 only).
	Device string `yaml:"device"`
	// Width / Height are the requested capture resolution.
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
	// FPS is the requested capture frame rate.
	FPS int `yaml:"fps"`
	// Format is the requested pixel format string, e.g. "YUYV" or "MJPEG".
	Format string `yaml:"format"`
}

// EncoderConfig configures the H.264 encoder (T3: ffmpeg subprocess; T13: codec).
type EncoderConfig struct {
	// Bitrate is the target video bitrate in bits per second. When ABR is enabled
	// this is the starting bitrate; the controller adjusts it within [abr.min,
	// abr.max].
	Bitrate int `yaml:"bitrate"`
	// Preset is the ffmpeg x264 preset, e.g. "ultrafast". Used by the libx264
	// codec only; hardware codecs use their own equivalents.
	Preset string `yaml:"preset"`
	// KeyframeInterval is the GOP size in frames.
	KeyframeInterval int `yaml:"keyframe_interval"`
	// FFmpegPath is the path to the ffmpeg binary.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// Codec selects the H.264 encoder (T13): "libx264" (software, default),
	// "h264_v4l2m2m" (Raspberry Pi), "h264_vaapi" (Intel/AMD GPU) or
	// "h264_nvenc" (NVIDIA GPU).
	Codec string `yaml:"codec"`
	// VAAPIDevice is the DRM render node for the h264_vaapi codec, e.g.
	// /dev/dri/renderD128. Ignored by other codecs; empty uses the default node.
	VAAPIDevice string `yaml:"vaapi_device"`
}

// Encoder codec kinds (T13). These mirror ffmpeg encoder names.
const (
	CodecLibX264 = "libx264"
	CodecV4L2M2M = "h264_v4l2m2m"
	CodecVAAPI   = "h264_vaapi"
	CodecNVENC   = "h264_nvenc"
)

// AudioConfig configures the optional ALSA→Opus audio source sent alongside the
// video on its own WebRTC track. Disabled by default so video-only deployments
// are unaffected.
type AudioConfig struct {
	// Enabled turns on audio capture and adds an Opus track to each viewer peer.
	Enabled bool `yaml:"enabled"`
	// Device is the ALSA capture device, e.g. "default" or "hw:CARD=MS2109,DEV=0".
	Device string `yaml:"device"`
	// SampleRate / Channels describe the ALSA capture format. Opus output is always
	// 48 kHz stereo on the wire.
	SampleRate int `yaml:"sample_rate"`
	Channels   int `yaml:"channels"`
	// Bitrate is the target Opus bitrate in bits per second.
	Bitrate int `yaml:"bitrate"`
	// FFmpegPath overrides the ffmpeg binary used for audio capture. Empty falls
	// back to encoder.ffmpeg_path (and then "ffmpeg").
	FFmpegPath string `yaml:"ffmpeg_path"`
}

// ABRConfig configures the adaptive-bitrate controller (T14). When disabled the
// encoder runs at the fixed encoder.bitrate.
type ABRConfig struct {
	// Enabled turns on adaptive bitrate. Off by default (fixed bitrate).
	Enabled bool `yaml:"enabled"`
	// MinBitrate / MaxBitrate bound the adapted bitrate in bits per second.
	MinBitrate int `yaml:"min_bitrate"`
	MaxBitrate int `yaml:"max_bitrate"`
	// LossThreshold is the packet-loss fraction (0..1) above which the controller
	// decreases the bitrate. e.g. 0.05 = 5%.
	LossThreshold float64 `yaml:"loss_threshold"`
	// SampleIntervalMS is how often WebRTC stats are sampled and fed to the
	// controller. 0 uses a 2s default.
	SampleIntervalMS int `yaml:"sample_interval_ms"`
	// CooldownSeconds is the minimum time between two bitrate changes (debounces
	// the encoder restart). 0 uses a 10s default.
	CooldownSeconds int `yaml:"cooldown_seconds"`
}

// WebRTCConfig configures the Pion WebRTC peer connection (T4).
type WebRTCConfig struct {
	// ICEServers is the list of STUN/TURN server URLs.
	ICEServers []string `yaml:"ice_servers"`
}

// InputConfig configures the controller input target (T5: NXBT bridge / log).
type InputConfig struct {
	// Target selects the input backend: "nxbt" (production Unix-socket bridge)
	// or "log" (dev mode; logs commands instead of driving a Switch).
	Target string `yaml:"target"`
	// SocketPath is the address of the NXBT bridge (nxbt only). Unix socket
	// path (e.g. /run/nxbt/nxbt.sock) or TCP address (e.g. 192.168.1.5:12345).
	SocketPath string `yaml:"socket_path"`
	// RateHz caps commands forwarded per second (nxbt only). 0 disables limiting.
	RateHz int `yaml:"rate_hz"`
	// DisabledButtons is a list of button names (e.g. "home", "capture", "plus")
	// that will be silently dropped before forwarding to the target.
	DisabledButtons []string `yaml:"disabled_buttons"`
}

// SessionConfig configures the single-controller session gate (T9). When no
// public key is configured the host runs without session enforcement: every
// connecting viewer is allowed to control directly (dev convenience).
type SessionConfig struct {
	// Enabled turns on JWT-validated single-controller gating. When false (the
	// default) input flows straight through without a token.
	Enabled bool `yaml:"enabled"`
	// PublicKeyBase64 is the base64-encoded ed25519 public key used to verify
	// session JWTs. Exactly one of PublicKeyBase64 / PublicKeyFile is used.
	PublicKeyBase64 string `yaml:"public_key_base64"`
	// PublicKeyFile is a path to a file containing the base64 ed25519 public key.
	PublicKeyFile string `yaml:"public_key_file"`
	// IdleTimeoutSeconds kicks an idle controller after this many seconds. 0
	// disables idle kicking.
	IdleTimeoutSeconds int `yaml:"idle_timeout_seconds"`
}

// SignalingConfig configures the connection to the signaling Worker (T7/T8).
type SignalingConfig struct {
	// URL is the HTTP base URL of the signaling Worker, e.g.
	// "https://x.workers.dev" or "http://localhost:8787".
	URL string `yaml:"url"`
	// RoomID identifies which streaming room this host joins.
	RoomID string `yaml:"room_id"`
	// Token is an optional bearer token sent on every signaling request.
	Token string `yaml:"token"`
	// PollIntervalMS is how often the host polls the Worker for viewer messages.
	PollIntervalMS int `yaml:"poll_interval_ms"`
	// UseTURN fetches ICE servers from the Worker's /turn/credentials endpoint at
	// startup, falling back to the static WebRTC.ICEServers on failure.
	UseTURN bool `yaml:"use_turn"`
}

// MetricsConfig configures the latency reporter.
type MetricsConfig struct {
	// ReportIntervalSeconds is how often pipeline latency p50/p95 are logged.
	// 0 uses the 5s default.
	ReportIntervalSeconds int `yaml:"report_interval_seconds"`
	// ListenAddr, when non-empty, serves the live per-stage latency snapshot as
	// JSON at GET /metrics on this address. Use a localhost address — the data is
	// unauthenticated. Empty disables the endpoint.
	ListenAddr string `yaml:"listen_addr"`
}

// ServerConfig configures the optional connection to playgate-server. When URL
// is empty the whole feature is disabled and no heartbeats are sent.
// When enabled the host POSTs a heartbeat on each interval, which powers the
// streamer console's online status indicator and Force kick button.
type ServerConfig struct {
	// URL is the HTTP base URL of the playgate-server, e.g. http://localhost:8080.
	// Empty string disables the heartbeat feature entirely.
	URL string `yaml:"url"`
	// APIKey is the host API key sent as "Authorization: Bearer <key>" on every
	// heartbeat request. Obtain it from playgate-server's register-host endpoint.
	APIKey string `yaml:"api_key"`
	// HeartbeatIntervalSeconds is how often the host sends a heartbeat POST.
	// Values <= 0 default to 30.
	HeartbeatIntervalSeconds int `yaml:"heartbeat_interval_seconds"`
}

// Default returns a Config populated with sensible defaults. Load starts from
// these defaults and overlays the YAML file on top, so unspecified fields keep
// their default value.
func Default() Config {
	return Config{
		Capture: CaptureConfig{
			Source: CaptureV4L2,
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
			Codec:            CodecLibX264,
		},
		Audio: AudioConfig{
			Enabled:    false,
			Device:     "default",
			SampleRate: 48000,
			Channels:   2,
			Bitrate:    128000,
		},
		ABR: ABRConfig{
			Enabled:          false,
			MinBitrate:       1_000_000,
			MaxBitrate:       8_000_000,
			LossThreshold:    0.05,
			SampleIntervalMS: 2000,
			CooldownSeconds:  10,
		},
		WebRTC: WebRTCConfig{
			ICEServers: []string{"stun:stun.l.google.com:19302"},
		},
		Input: InputConfig{
			Target:     InputNXBT,
			SocketPath: "/run/nxbt/nxbt.sock",
			RateHz:     120,
		},
		Session: SessionConfig{
			Enabled:            false,
			IdleTimeoutSeconds: 0,
		},
		Signaling: SignalingConfig{
			URL:            "http://localhost:8787",
			RoomID:         "default",
			PollIntervalMS: 500,
			UseTURN:        false,
		},
		Metrics: MetricsConfig{
			ReportIntervalSeconds: 5,
			ListenAddr:            "127.0.0.1:9091",
		},
		Server: ServerConfig{
			HeartbeatIntervalSeconds: 30,
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
	switch c.Capture.Source {
	case "", CaptureV4L2, CaptureSynthetic:
	default:
		return fmt.Errorf("capture source must be %q or %q, got %q", CaptureV4L2, CaptureSynthetic, c.Capture.Source)
	}
	if c.Capture.Width <= 0 || c.Capture.Height <= 0 {
		return fmt.Errorf("capture resolution must be positive, got %dx%d", c.Capture.Width, c.Capture.Height)
	}
	if c.Capture.FPS <= 0 {
		return fmt.Errorf("capture fps must be positive, got %d", c.Capture.FPS)
	}
	if c.Encoder.Bitrate <= 0 {
		return fmt.Errorf("encoder bitrate must be positive, got %d", c.Encoder.Bitrate)
	}
	switch c.Encoder.Codec {
	case "", CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC:
	default:
		return fmt.Errorf("encoder codec must be one of %q/%q/%q/%q, got %q",
			CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC, c.Encoder.Codec)
	}
	if c.Audio.Enabled {
		if c.Audio.Device == "" {
			return fmt.Errorf("audio.enabled requires a device")
		}
		if c.Audio.SampleRate <= 0 || c.Audio.Channels <= 0 {
			return fmt.Errorf("audio sample_rate and channels must be positive when enabled (sample_rate=%d channels=%d)", c.Audio.SampleRate, c.Audio.Channels)
		}
		if c.Audio.Bitrate <= 0 {
			return fmt.Errorf("audio bitrate must be positive when enabled, got %d", c.Audio.Bitrate)
		}
	}
	if c.ABR.Enabled {
		if c.ABR.MinBitrate <= 0 || c.ABR.MaxBitrate <= 0 {
			return fmt.Errorf("abr min/max bitrate must be positive when enabled (min=%d max=%d)", c.ABR.MinBitrate, c.ABR.MaxBitrate)
		}
		if c.ABR.MaxBitrate < c.ABR.MinBitrate {
			return fmt.Errorf("abr max_bitrate %d < min_bitrate %d", c.ABR.MaxBitrate, c.ABR.MinBitrate)
		}
		if c.ABR.LossThreshold < 0 || c.ABR.LossThreshold >= 1 {
			return fmt.Errorf("abr loss_threshold must be in [0,1), got %v", c.ABR.LossThreshold)
		}
	}
	switch c.Input.Target {
	case "", InputNXBT, InputLog:
	default:
		return fmt.Errorf("input target must be %q or %q, got %q", InputNXBT, InputLog, c.Input.Target)
	}
	if c.Session.Enabled && c.Session.PublicKeyBase64 == "" && c.Session.PublicKeyFile == "" {
		return fmt.Errorf("session.enabled requires public_key_base64 or public_key_file")
	}
	if c.Signaling.URL == "" {
		return fmt.Errorf("signaling url must not be empty")
	}
	if c.Signaling.RoomID == "" {
		return fmt.Errorf("signaling room_id must not be empty")
	}
	return nil
}

// CaptureSourceKind returns the effective capture source, defaulting to v4l2.
func (c Config) CaptureSourceKind() string {
	if c.Capture.Source == "" {
		return CaptureV4L2
	}
	return c.Capture.Source
}

// InputTargetKind returns the effective input target, defaulting to nxbt.
func (c Config) InputTargetKind() string {
	if c.Input.Target == "" {
		return InputNXBT
	}
	return c.Input.Target
}

// EncoderCodecKind returns the effective encoder codec, defaulting to libx264.
func (c Config) EncoderCodecKind() string {
	if c.Encoder.Codec == "" {
		return CodecLibX264
	}
	return c.Encoder.Codec
}
