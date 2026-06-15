package host

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/audio/opus"
	"github.com/playgate/playgate-host/internal/capture/synthetic"
	"github.com/playgate/playgate-host/internal/capture/v4l2"
	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
	"github.com/playgate/playgate-host/internal/input/logtarget"
	"github.com/playgate/playgate-host/internal/input/nxbt"
	"github.com/playgate/playgate-host/internal/metrics"
	"github.com/playgate/playgate-host/internal/session"
)

// buildDeps constructs the real pipeline modules from cfg, inserting latency
// taps between capture→encoder→router and wiring a real signaling-backed
// connection loop.
func buildDeps(log *slog.Logger, cfg config.Config, mc *metrics.Collector) (Deps, error) {
	// --- capture ---
	capture, err := buildCapture(log, cfg)
	if err != nil {
		return Deps{}, err
	}

	// --- latency tap between capture and encoder ---
	tap := newLatencyTap(log, mc, capture.Frames())

	// --- encoder ---
	pixfmt, err := v4l2.ParsePixelFormat(cfg.Capture.Format)
	if err != nil {
		return Deps{}, fmt.Errorf("host: capture format: %w", err)
	}
	encOpts := ffmpeg.DefaultOptions(cfg.Capture.Width, cfg.Capture.Height, cfg.Capture.FPS, pixfmt)
	encOpts.FFmpegPath = cfg.Encoder.FFmpegPath
	encOpts.Bitrate = cfg.Encoder.Bitrate
	encOpts.Preset = cfg.Encoder.Preset
	encOpts.GOPSize = cfg.Encoder.KeyframeInterval

	// T13: select the configured codec (libx264 / v4l2m2m / vaapi / nvenc).
	codec, err := ffmpeg.CodecFromName(cfg.EncoderCodecKind(), cfg.Encoder.VAAPIDevice)
	if err != nil {
		return Deps{}, fmt.Errorf("host: encoder codec: %w", err)
	}
	encOpts.Codec = codec

	// T13: best-effort detection — probe `ffmpeg -encoders` and fail with an
	// actionable error (no silent downgrade) if the chosen codec is missing.
	detectCodec(log, cfg.Encoder.FFmpegPath, cfg.EncoderCodecKind())

	enc, err := ffmpeg.New(log, encOpts, tap.out())
	if err != nil {
		return Deps{}, fmt.Errorf("host: build encoder: %w", err)
	}

	// Wrap capture+tap+encoder so the host runs the tap alongside them and so the
	// encode-stage latency can be recorded as packets emerge.
	encWrap := newEncoderWrapper(log, mc, capture, tap, enc)

	// --- optional audio source (ALSA→Opus) ---
	audioSrc := buildAudio(log, cfg)

	// --- input target ---
	input, err := buildInput(log, cfg, mc)
	if err != nil {
		return Deps{}, err
	}

	// --- session manager ---
	mgr, err := buildSession(cfg)
	if err != nil {
		return Deps{}, err
	}

	return Deps{
		Capture: capture,
		Encoder: encWrap,
		Audio:   audioSrc,
		Input:   input,
		Session: mgr,
		Connect: makeSignalingConnect(log, cfg, mc, encWrap),
	}, nil
}

// buildAudio constructs the ALSA→Opus source when audio is enabled, else returns
// a nil AudioSource (interface nil, so host.Run's nil check works — returning a
// typed nil *opus.Source would defeat it). It falls back to the encoder's ffmpeg
// path when audio.ffmpeg_path is unset.
func buildAudio(log *slog.Logger, cfg config.Config) AudioSource {
	if !cfg.Audio.Enabled {
		return nil
	}
	ffmpegPath := cfg.Audio.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = cfg.Encoder.FFmpegPath
	}
	return opus.New(log, opus.Config{
		FFmpegPath: ffmpegPath,
		Device:     cfg.Audio.Device,
		SampleRate: cfg.Audio.SampleRate,
		Channels:   cfg.Audio.Channels,
		Bitrate:    cfg.Audio.Bitrate,
	})
}

// detectCodec runs the best-effort `ffmpeg -encoders` probe and logs a clear,
// actionable error if the configured codec is not present. It deliberately does
// NOT abort startup or silently downgrade: a probe failure (e.g. ffmpeg not yet
// on PATH in a container) must not block the host, and an operator who set a
// hardware codec should see exactly why it won't work and which fallback to use.
func detectCodec(log *slog.Logger, ffmpegPath, codecName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	avail, err := ffmpeg.EncoderList(ctx, ffmpegPath)
	if err != nil {
		log.Warn("encoder detection skipped (could not probe ffmpeg -encoders)",
			"err", err, "codec", codecName)
		return
	}
	if derr := ffmpeg.DetectCodec(codecName, avail); derr != nil {
		log.Error("encoder codec not available", "err", derr)
		return
	}
	log.Info("encoder codec available", "codec", codecName)
}

// buildCapture selects the configured capture backend.
func buildCapture(log *slog.Logger, cfg config.Config) (core.CaptureSource, error) {
	switch cfg.CaptureSourceKind() {
	case config.CaptureSynthetic:
		return synthetic.New(log, synthetic.Config{
			Width:  cfg.Capture.Width,
			Height: cfg.Capture.Height,
			FPS:    cfg.Capture.FPS,
		})
	case config.CaptureV4L2:
		pixfmt, err := v4l2.ParsePixelFormat(cfg.Capture.Format)
		if err != nil {
			return nil, fmt.Errorf("host: capture format: %w", err)
		}
		return v4l2.New(log, v4l2.Config{
			Device:           cfg.Capture.Device,
			Width:            cfg.Capture.Width,
			Height:           cfg.Capture.Height,
			FPS:              cfg.Capture.FPS,
			PreferredFormats: []core.PixelFormat{pixfmt},
		})
	default:
		return nil, fmt.Errorf("host: unknown capture source %q", cfg.CaptureSourceKind())
	}
}

// buildInput selects the configured input backend.
func buildInput(log *slog.Logger, cfg config.Config, mc *metrics.Collector) (core.InputTarget, error) {
	switch cfg.InputTargetKind() {
	case config.InputLog:
		// Sample ~one log line per second at 60 Hz.
		return logtarget.New(log, 60), nil
	case config.InputNXBT:
		return nxbt.New(log, cfg.Input.SocketPath,
			nxbt.WithRateHz(cfg.Input.RateHz),
			nxbt.WithLatencyHistogram(mc.Input),
			nxbt.WithDaemonHistogram(mc.DaemonApply),
		), nil
	default:
		return nil, fmt.Errorf("host: unknown input target %q", cfg.InputTargetKind())
	}
}

// buildSession constructs the session manager when gating is enabled, else nil.
func buildSession(cfg config.Config) (*session.Manager, error) {
	if !cfg.Session.Enabled {
		return nil, nil
	}
	return session.NewManager(session.Config{
		PublicKeyBase64: cfg.Session.PublicKeyBase64,
		PublicKeyFile:   cfg.Session.PublicKeyFile,
		RoomID:          cfg.Signaling.RoomID,
		IdleTimeout:     time.Duration(cfg.Session.IdleTimeoutSeconds) * time.Second,
	})
}

// --- latency tap ---------------------------------------------------------

// latencyTap forwards frames from capture to the encoder, recording for each
// frame how long it waited in the capture buffer (capture stage) and the time it
// entered the encoder (so the encode stage can be computed when a packet emerges).
type latencyTap struct {
	log     *slog.Logger
	metrics *metrics.Collector
	in      <-chan core.VideoFrame
	outCh   chan core.VideoFrame

	mu        sync.Mutex
	lastEnter time.Time // wall time the most recent frame was handed to the encoder
}

func newLatencyTap(log *slog.Logger, mc *metrics.Collector, in <-chan core.VideoFrame) *latencyTap {
	return &latencyTap{
		log:     log.With("module", "latency-tap"),
		metrics: mc,
		in:      in,
		outCh:   make(chan core.VideoFrame, 4),
	}
}

func (t *latencyTap) out() <-chan core.VideoFrame { return t.outCh }

// lastEnterTime returns the wall time the most recent frame entered the encoder.
func (t *latencyTap) lastEnterTime() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastEnter
}

// run forwards frames, recording capture-stage latency and the enter time.
func (t *latencyTap) run(ctx context.Context) {
	defer close(t.outCh)
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-t.in:
			if !ok {
				return
			}
			if t.metrics != nil && !frame.Timestamp.IsZero() {
				t.metrics.Capture.Observe(time.Since(frame.Timestamp))
			}
			t.mu.Lock()
			t.lastEnter = time.Now()
			t.mu.Unlock()
			select {
			case t.outCh <- frame:
			case <-ctx.Done():
				return
			}
		}
	}
}

// --- encoder wrapper -----------------------------------------------------

// encoderWrapper bundles the capture-source Run, the latency tap Run, and the
// real ffmpeg encoder Run into a single core.Module-style unit, and instruments
// the encode stage by timing packets against the latency tap's last enter time.
// It exposes the encoder's packet output (re-tapped) as Packets.
type encoderWrapper struct {
	log     *slog.Logger
	metrics *metrics.Collector
	capture core.CaptureSource
	tap     *latencyTap
	enc     *ffmpeg.Encoder

	outCh chan core.EncodedPacket
}

func newEncoderWrapper(log *slog.Logger, mc *metrics.Collector, capture core.CaptureSource, tap *latencyTap, enc *ffmpeg.Encoder) *encoderWrapper {
	return &encoderWrapper{
		log:     log,
		metrics: mc,
		capture: capture,
		tap:     tap,
		enc:     enc,
		outCh:   make(chan core.EncodedPacket, 4),
	}
}

// Name implements core.Module.
func (w *encoderWrapper) Name() string { return "encoder" }

// Packets returns the instrumented encoded-packet output.
func (w *encoderWrapper) Packets() <-chan core.EncodedPacket { return w.outCh }

// SetBitrate forwards an ABR bitrate decision to the underlying ffmpeg encoder,
// which restarts the subprocess to apply it (T14).
func (w *encoderWrapper) SetBitrate(bps int) { w.enc.SetBitrate(bps) }

// ForceKeyframe forwards a viewer keyframe request (RTCP PLI/FIR) to the encoder,
// which restarts the subprocess to emit a fresh IDR for decoder recovery.
func (w *encoderWrapper) ForceKeyframe() { w.enc.ForceKeyframe() }

// Run starts the tap and encoder, and re-emits packets with encode-stage latency
// recorded. The capture source itself is run by the host's main group; the tap
// and encoder are owned here. It returns when ctx is cancelled or the encoder
// finishes.
func (w *encoderWrapper) Run(ctx context.Context) error {
	defer close(w.outCh)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.tap.run(ctx) }()
	go func() { defer wg.Done(); _ = w.enc.Run(ctx) }()

	// Re-tap the encoder's packets to record encode-stage latency: the time from
	// the most recent frame entering the encoder to the packet emerging. This is
	// an approximation (frames and packets are not 1:1) but gives a useful p50/p95.
	encPackets := w.enc.Packets()
	for {
		select {
		case <-ctx.Done():
			// Drain in the background so the encoder can close cleanly.
			go func() {
				for range encPackets {
				}
			}()
			wg.Wait()
			return nil
		case pkt, ok := <-encPackets:
			if !ok {
				wg.Wait()
				return nil
			}
			if w.metrics != nil {
				if enter := w.tap.lastEnterTime(); !enter.IsZero() {
					w.metrics.Encode.Observe(time.Since(enter))
				}
			}
			select {
			case w.outCh <- pkt:
			case <-ctx.Done():
			}
		}
	}
}
