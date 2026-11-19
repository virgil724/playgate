// Package config loads and validates the PlayGate Host YAML configuration.
package config

import (
	"math"
	"fmt"
	"os"
	"time"

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
	Overlay   OverlayConfig   `yaml:"overlay"`
}

// Capture source kinds.
const (
	// CaptureV4L2 reads from a Linux V4L2 device (production; Linux only).
	CaptureV4L2 = "v4l2"
	// CaptureSynthetic generates a pure-Go test pattern (dev mode, any OS).
	CaptureSynthetic = "synthetic"
	// CaptureTestSrc runs ffmpeg's lavfi testsrc2 with frame-counter and
	// input-overlay text burned in (dev/diagnostics; needs ffmpeg).
	CaptureTestSrc = "testsrc"
	// CaptureDShow reads capture.device through ffmpeg's DirectShow input
	// (a capture card on Windows; needs ffmpeg).
	CaptureDShow = "dshow"
)

// Input target kinds.
const (
	// InputNXBT forwards commands to the NXBT daemon over a socket (TCP or Unix).
	InputNXBT = "nxbt"
	// InputLog logs commands instead of forwarding them (dev mode, any OS).
	InputLog = "log"
	// InputOverlay writes the received controller state to overlay.text_file,
	// which the testsrc capture source burns into the picture.
	InputOverlay = "overlay"
	// InputGateway drives the PlayGate Switch 2 gateway (ESP32-C6) over USB
	// serial; see input.gateway.
	InputGateway = "gateway"
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
	// TestSrc configures the "testsrc" capture source.
	TestSrc TestSrcConfig `yaml:"testsrc"`
}

// TestSrcConfig configures the ffmpeg lavfi test-picture capture source.
type TestSrcConfig struct {
	// FontFile is an optional TrueType font for drawtext, e.g.
	// C:/Windows/Fonts/consola.ttf. Empty uses ffmpeg's fontconfig default.
	FontFile string `yaml:"font_file"`
}

// OverlayConfig is shared by the overlay input target (writer) and the testsrc
// capture source (reader).
type OverlayConfig struct {
	// TextFile is the file the overlay input target writes and ffmpeg drawtext
	// reloads each frame.
	TextFile string `yaml:"text_file"`
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
	// KeyframeInterval is an explicit GOP size in frames. When > 0 it takes
	// precedence over GOPSeconds; leave it 0 (default) to derive the GOP from
	// GOPSeconds * capture.fps.
	KeyframeInterval int `yaml:"keyframe_interval"`
	// GOPSeconds is the keyframe interval in seconds; the effective GOP is
	// round(capture.fps * gop_seconds) frames. Unset/0 picks a backend-dependent
	// default: 3 for backend libav (keyframes are cheap and PLI-driven there, so
	// a long GOP is safe), 2 for backend cli. Ignored when keyframe_interval is
	// set.
	GOPSeconds float64 `yaml:"gop_seconds"`
	// KeyframeDebounceMS is the minimum spacing between encoder ForceKeyframe
	// calls triggered by viewers (RTCP PLI/FIR, WebCodecs keyframe requests, new
	// viewer joins). It is the single keyframe-rate knob: one host-wide coalescer
	// uses it, and the cli backend also uses it as its restart debounce. Unset/0
	// picks a backend default: 150 for libav (a forced IDR is cheap), 1000 for
	// cli (a forced IDR restarts ffmpeg). webrtc.video_keyframe_min_interval_ms is
	// a legacy alias that applies only while this is unset.
	KeyframeDebounceMS int `yaml:"keyframe_debounce_ms"`
	// FFmpegPath is the path to the ffmpeg binary.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// Codec selects the H.264 encoder (T13): "libx264" (software, default),
	// "h264_v4l2m2m" (Raspberry Pi), "h264_vaapi" (Intel/AMD GPU) or
	// "h264_nvenc" (NVIDIA GPU).
	Codec string `yaml:"codec"`
	// VAAPIDevice is the DRM render node for the h264_vaapi codec, e.g.
	// /dev/dri/renderD128. Ignored by other codecs; empty uses the default node.
	VAAPIDevice string `yaml:"vaapi_device"`
	// Backend selects the encoder implementation: "cli" (ffmpeg subprocess,
	// default) or "libav" (in-process libav; requires a -tags libav cgo build).
	Backend string `yaml:"backend"`
	// IntraRefresh replaces periodic IDR frames with x264 rolling intra refresh
	// (-intra-refresh 1): no keyframe spikes, and packet loss heals within one
	// GOP-sized refresh period. libx264 only. Default false.
	IntraRefresh bool `yaml:"intra_refresh"`
}

// Encoder backend kinds.
const (
	BackendCLI   = "cli"
	BackendLibav = "libav"
)

// GOPFrames returns the effective GOP size in frames for the given frame rate:
// keyframe_interval when explicitly set (> 0), otherwise round(fps*gop_seconds)
// (gop_seconds unset/0 means the backend default, see DefaultGOPSeconds), never
// less than 1.
func (e EncoderConfig) GOPFrames(fps int) int {
	if e.KeyframeInterval > 0 {
		return e.KeyframeInterval
	}
	if fps <= 0 {
		fps = 30
	}
	sec := e.GOPSeconds
	if sec <= 0 {
		sec = e.DefaultGOPSeconds()
	}
	g := int(math.Round(float64(fps) * sec))
	if g < 1 {
		g = 1
	}
	return g
}

// DefaultGOPSeconds is the gop_seconds used when it is unset: 3 for the libav
// backend (cheap on-demand keyframes via PLI), 2 for the cli backend.
func (e EncoderConfig) DefaultGOPSeconds() float64 {
	if e.Backend == BackendLibav {
		return 3
	}
	return 2
}

// Default keyframe debounce per backend (see EncoderConfig.KeyframeDebounceMS).
const (
	DefaultKeyframeDebounceLibavMS = 150
	DefaultKeyframeDebounceCLIMS   = 1000
)

// KeyframeDebounce resolves the effective minimum spacing between encoder
// ForceKeyframe calls: encoder.keyframe_debounce_ms, else the legacy
// webrtc.video_keyframe_min_interval_ms, else the backend default.
func (c Config) KeyframeDebounce() time.Duration {
	ms := c.Encoder.KeyframeDebounceMS
	if ms <= 0 {
		ms = c.WebRTC.VideoKeyframeMinIntervalMS
	}
	if ms <= 0 {
		if c.EncoderBackendKind() == BackendLibav {
			ms = DefaultKeyframeDebounceLibavMS
		} else {
			ms = DefaultKeyframeDebounceCLIMS
		}
	}
	return time.Duration(ms) * time.Millisecond
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
	// Device is the capture device: an ALSA device on Linux, e.g. "default" or
	// "hw:CARD=MS2109,DEV=0", or a DirectShow audio device name on Windows,
	// e.g. "HDMI (Live Gamer Ultra-Audio)".
	Device string `yaml:"device"`
	// Format is the ffmpeg audio input: "alsa" or "dshow". Empty picks by OS
	// (dshow on Windows, alsa elsewhere).
	Format string `yaml:"format"`
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
	// controller. 0 picks by backend: 500ms with a live-bitrate encoder
	// (libav), 2s otherwise.
	SampleIntervalMS int `yaml:"sample_interval_ms"`
	// CooldownSeconds is the minimum time between two bitrate changes (debounces
	// the encoder restart). 0 uses a 10s default.
	CooldownSeconds int `yaml:"cooldown_seconds"`
}

// WebRTCConfig configures the Pion WebRTC peer connection (T4).
type WebRTCConfig struct {
	// ICEServers is the list of STUN/TURN server URLs.
	ICEServers []string `yaml:"ice_servers"`
	// GCC enables Pion's Google Congestion Control bandwidth estimator (TWCC).
	// Off by default. Requires encoder.backend "libav" (live bitrate changes);
	// validation rejects it with the CLI backend. WebCodecs-transport viewers
	// produce no TWCC feedback and are ignored by the estimate aggregation.
	GCC bool `yaml:"gcc"`

	// VideoTransport selects how video reaches viewers: "track" (default, RTP
	// H.264 track only), "webcodecs" (Annex-B fragments over an unreliable
	// DataChannel, RTP track kept as fallback) or "auto" (viewer chooses).
	// See docs/webcodecs-transport.md. Unrelated to the twitch bot's token
	// delivery mode.
	VideoTransport string `yaml:"video_transport"`
	// VideoFragmentBytes is the total DataChannel message size (header +
	// payload) in WebCodecs mode. Range 600-1400, default 1200.
	VideoFragmentBytes int `yaml:"video_fragment_bytes"`
	// VideoBufferMaxBytes: drop a whole frame when the video DataChannel's
	// BufferedAmount exceeds this. Default 262144.
	VideoBufferMaxBytes int `yaml:"video_buffer_max_bytes"`
	// VideoIDRPaceMS spreads frames larger than 8 fragments (IDRs) over this
	// many ms to avoid self-induced burst loss. Default 8; 0 uses the default,
	// negative disables pacing.
	VideoIDRPaceMS int `yaml:"video_idr_pace_ms"`
	// VideoFEC selects application-level FEC for the WebCodecs transport:
	// "off" (default), "xor" or "rs" (Reed-Solomon over GF(256)). Parity is
	// only sent to viewers whose HELLO advertises the scheme.
	VideoFEC string `yaml:"video_fec"`
	// VideoFECRatio is parity fragments per data fragment: m = ceil(k*ratio),
	// at least 1 for frames of 2+ fragments. Range 0-1, default 0.1.
	VideoFECRatio float64 `yaml:"video_fec_ratio"`
	// VideoKeyframeMinIntervalMS coalesces viewer keyframe requests to at most
	// one encoder ForceKeyframe per interval. Legacy alias of
	// encoder.keyframe_debounce_ms (which wins when both are set); unset uses the
	// encoder's backend default.
	VideoKeyframeMinIntervalMS int `yaml:"video_keyframe_min_interval_ms"`
}

// Video transport values for WebRTCConfig.VideoTransport.
const (
	VideoTransportTrack     = "track"
	VideoTransportWebCodecs = "webcodecs"
	VideoTransportAuto      = "auto"
)

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
	// Gateway configures the Switch 2 gateway target (target: gateway).
	Gateway GatewayConfig `yaml:"gateway"`
}

// GatewayConfig configures the PlayGate Switch 2 gateway input target.
type GatewayConfig struct {
	// Port is the serial port, e.g. "COM5" or "/dev/ttyACM0". "auto" (the
	// default) picks the single Espressif USB Serial/JTAG port present.
	Port string `yaml:"port"`
	// RateHz spaces queued button edges so two never land in one gateway
	// report. The default 150 suits the event-driven firmware (a report at
	// most every 5-6 ms); use 90 with firmware that reports at a fixed 100 Hz.
	// 0 disables limiting.
	RateHz int `yaml:"rate_hz"`
	// KeepaliveMS is how often a held state is re-sent; it must stay well
	// below the gateway's 100 ms input timeout.
	KeepaliveMS int `yaml:"keepalive_ms"`
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
			FFmpegPath:       "ffmpeg",
			Codec:            CodecLibX264,
			Backend:          BackendCLI,
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
			SampleIntervalMS: 0, // by backend, see abrTimings
			CooldownSeconds:  10,
		},
		WebRTC: WebRTCConfig{
			ICEServers:                 []string{"stun:stun.l.google.com:19302"},
			VideoTransport:             VideoTransportTrack,
			VideoFragmentBytes:         1200,
			VideoBufferMaxBytes:        262144,
			VideoIDRPaceMS:             8,
			VideoFEC:                   "off",
			VideoFECRatio:              0.1,
		},
		Overlay: OverlayConfig{TextFile: "playgate-overlay.txt"},
		Input: InputConfig{
			Target:     InputNXBT,
			SocketPath: "/run/nxbt/nxbt.sock",
			RateHz:     120,
			Gateway:    GatewayConfig{Port: "auto", RateHz: 150, KeepaliveMS: 40},
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
	case "", CaptureV4L2, CaptureSynthetic, CaptureTestSrc, CaptureDShow:
	default:
		return fmt.Errorf("capture source must be %q, %q, %q or %q, got %q",
			CaptureV4L2, CaptureDShow, CaptureSynthetic, CaptureTestSrc, c.Capture.Source)
	}
	if c.Capture.Source == CaptureDShow && c.Capture.Device == "" {
		return fmt.Errorf("capture.device must name the DirectShow video device for source %q", CaptureDShow)
	}
	if c.Capture.Width <= 0 || c.Capture.Height <= 0 {
		return fmt.Errorf("capture resolution must be positive, got %dx%d", c.Capture.Width, c.Capture.Height)
	}
	if c.Capture.FPS <= 0 {
		return fmt.Errorf("capture fps must be positive, got %d", c.Capture.FPS)
	}
	if c.Encoder.KeyframeInterval < 0 {
		return fmt.Errorf("encoder keyframe_interval must be >= 0, got %d", c.Encoder.KeyframeInterval)
	}
	if c.Encoder.GOPSeconds < 0 || math.IsNaN(c.Encoder.GOPSeconds) || math.IsInf(c.Encoder.GOPSeconds, 0) {
		return fmt.Errorf("encoder gop_seconds must be a finite value >= 0, got %v", c.Encoder.GOPSeconds)
	}
	if c.Encoder.KeyframeDebounceMS < 0 {
		return fmt.Errorf("encoder keyframe_debounce_ms must be >= 0, got %d", c.Encoder.KeyframeDebounceMS)
	}
	if c.Encoder.Bitrate <= 0 {
		return fmt.Errorf("encoder bitrate must be positive, got %d", c.Encoder.Bitrate)
	}
	if c.Encoder.IntraRefresh && c.EncoderCodecKind() != CodecLibX264 {
		return fmt.Errorf("encoder intra_refresh requires codec %q, got %q", CodecLibX264, c.EncoderCodecKind())
	}
	switch c.Encoder.Codec {
	case "", CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC:
	default:
		return fmt.Errorf("encoder codec must be one of %q/%q/%q/%q, got %q",
			CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC, c.Encoder.Codec)
	}
	switch c.Encoder.Backend {
	case "", BackendCLI, BackendLibav:
	default:
		return fmt.Errorf("encoder backend must be %q or %q, got %q",
			BackendCLI, BackendLibav, c.Encoder.Backend)
	}
	if c.WebRTC.GCC && c.EncoderBackendKind() != BackendLibav {
		// GCC retunes the encoder every few hundred ms; the CLI backend restarts
		// ffmpeg on every bitrate change, which would stutter constantly.
		return fmt.Errorf("webrtc gcc requires encoder backend %q (cli restarts ffmpeg on every bitrate change)", BackendLibav)
	}
	switch c.WebRTC.VideoTransport {
	case "", VideoTransportTrack, VideoTransportWebCodecs, VideoTransportAuto:
	default:
		return fmt.Errorf("webrtc video_transport must be %q, %q or %q, got %q",
			VideoTransportTrack, VideoTransportWebCodecs, VideoTransportAuto, c.WebRTC.VideoTransport)
	}
	if fb := c.WebRTC.VideoFragmentBytes; fb != 0 && (fb < 600 || fb > 1400) {
		return fmt.Errorf("webrtc video_fragment_bytes must be within 600-1400, got %d", fb)
	}
	switch c.WebRTC.VideoFEC {
	case "", "off", "xor", "rs":
	default:
		return fmt.Errorf("webrtc video_fec must be \"off\", \"xor\" or \"rs\", got %q", c.WebRTC.VideoFEC)
	}
	if r := c.WebRTC.VideoFECRatio; r < 0 || r > 1 {
		return fmt.Errorf("webrtc video_fec_ratio must be within 0-1, got %v", r)
	}
	if c.WebRTC.VideoBufferMaxBytes < 0 || c.WebRTC.VideoKeyframeMinIntervalMS < 0 {
		return fmt.Errorf("webrtc video_buffer_max_bytes and video_keyframe_min_interval_ms must be >= 0")
	}
	if c.Audio.Enabled {
		if c.Audio.Device == "" {
			return fmt.Errorf("audio.enabled requires a device")
		}
		switch c.Audio.Format {
		case "", "alsa", "dshow":
		default:
			return fmt.Errorf("audio format must be \"alsa\" or \"dshow\", got %q", c.Audio.Format)
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
	case "", InputNXBT, InputLog, InputOverlay, InputGateway:
	default:
		return fmt.Errorf("input target must be %q, %q, %q or %q, got %q",
			InputNXBT, InputGateway, InputLog, InputOverlay, c.Input.Target)
	}
	if c.Input.Target == InputGateway {
		if c.Input.Gateway.RateHz < 0 {
			return fmt.Errorf("input.gateway.rate_hz must be >= 0, got %d", c.Input.Gateway.RateHz)
		}
		if k := c.Input.Gateway.KeepaliveMS; k < 0 || k >= 100 {
			return fmt.Errorf("input.gateway.keepalive_ms must be in [0,100) (the gateway neutralises after 100ms of silence), got %d", k)
		}
	}
	if (c.Input.Target == InputOverlay || c.Capture.Source == CaptureTestSrc) && c.Overlay.TextFile == "" {
		return fmt.Errorf("overlay.text_file must be set when using the overlay input target or testsrc capture")
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

// EncoderBackendKind returns the effective encoder backend, defaulting to cli.
func (c Config) EncoderBackendKind() string {
	if c.Encoder.Backend == "" {
		return BackendCLI
	}
	return c.Encoder.Backend
}
