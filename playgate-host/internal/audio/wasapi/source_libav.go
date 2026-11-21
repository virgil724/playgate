//go:build windows && cgo && libav

package wasapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unsafe"

	"github.com/asticode/go-astiav"
	"github.com/virgil724/caplat/capture"
	cw "github.com/virgil724/caplat/capture/wasapi"

	"github.com/playgate/playgate-host/internal/audio/opus"
	"github.com/playgate/playgate-host/internal/core"
)

const (
	opusRate         = 48000
	packetBufferSize = 16
	restartBackoff   = time.Second
	stallTimeout     = 2 * time.Second
)

// Config configures the source.
type Config struct {
	Device    string  // WASAPI capture endpoint, e.g. "HDMI (Live Gamer Ultra-Audio)"
	Bitrate   int     // Opus bits per second
	FrameMS   float64 // Opus frame: 2.5, 5, 10 (default) or 20 ms
	Exclusive bool    // WASAPI exclusive mode (3 ms periods on a GC553; the endpoint is ours alone)
}

// Source captures with WASAPI, encodes Opus with libav and emits packets.
type Source struct {
	log     *slog.Logger
	cfg     Config
	packets chan opus.Packet
}

var _ core.Module = (*Source)(nil)

// New constructs a Source.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if cfg.Device == "" {
		return nil, errors.New("wasapi audio: audio.device is required")
	}
	if cfg.FrameMS == 0 {
		cfg.FrameMS = 10
	}
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = 128000
	}
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		log:     log.With("module", "audio", "source", "wasapi", "device", cfg.Device),
		cfg:     cfg,
		packets: make(chan opus.Packet, packetBufferSize),
	}, nil
}

// Name implements core.Module.
func (s *Source) Name() string { return "audio" }

// Packets returns the Opus packets; Source closes the channel on exit.
func (s *Source) Packets() <-chan opus.Packet { return s.packets }

// Run implements core.Module: it captures and encodes until ctx ends,
// reopening the device after an error or a stall.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.packets)
	defer s.log.Info("audio source stopped")
	for {
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("wasapi audio ended; will restart", "err", err)
		}
		t := time.NewTimer(restartBackoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (s *Source) runOnce(ctx context.Context) error {
	enc, err := newOpusEncoder(s.cfg.Bitrate, s.cfg.FrameMS)
	if err != nil {
		return err
	}
	defer enc.free()

	var last time.Time
	lastCh := make(chan time.Time, 1)
	var encErr error
	ch := Chunker{Frames: enc.frameSize, OnBlock: func(stereo []float32) {
		if encErr != nil {
			return
		}
		encErr = enc.encode(stereo, func(data []byte, dur time.Duration) {
			s.forward(ctx, opus.Packet{Data: data, Duration: dur})
		})
	}}
	rate := 0
	capt, err := cw.Start(cw.Config{Endpoint: s.cfg.Device, Exclusive: s.cfg.Exclusive}, capture.QPC, capture.QPCFrequency(),
		func(p cw.Packet) {
			if p.Discontinuity {
				ch.Reset()
			}
			ch.Push(p.Samples, p.Frames, len(p.Samples)/max(p.Frames, 1))
			select {
			case lastCh <- time.Now():
			default:
			}
		})
	if err != nil {
		return err
	}
	defer capt.Stop()
	rate = capt.Rate
	if rate != opusRate {
		return fmt.Errorf("wasapi audio: %s runs at %d Hz; Opus needs %d Hz (set the device's format to 48 kHz)", s.cfg.Device, rate, opusRate)
	}
	period := "default (10 ms)"
	if capt.Period > 0 {
		period = capt.Period.String()
	}
	s.log.Info("wasapi audio started", "channels", capt.Channels, "rate", rate, "period", period,
		"exclusive", s.cfg.Exclusive, "opus_frame_ms", s.cfg.FrameMS, "bitrate", s.cfg.Bitrate)

	last = time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-capt.Done():
			if err == nil {
				err = errors.New("capture stopped")
			}
			return err
		case t := <-lastCh:
			last = t
		case now := <-tick.C:
			if encErr != nil {
				return encErr
			}
			if now.Sub(last) > stallTimeout {
				return fmt.Errorf("wasapi audio: no audio for %s", now.Sub(last).Round(time.Millisecond))
			}
		}
	}
}

// forward pushes a packet, dropping the oldest if the consumer is behind.
func (s *Source) forward(ctx context.Context, p opus.Packet) {
	select {
	case s.packets <- p:
		return
	default:
	}
	select {
	case <-s.packets:
	default:
	}
	select {
	case s.packets <- p:
	case <-ctx.Done():
	default:
	}
}

// opusEncoder is libav's libopus encoder for 48 kHz stereo float.
type opusEncoder struct {
	cc        *astiav.CodecContext
	frame     *astiav.Frame
	pkt       *astiav.Packet
	frameSize int
	pts       int64
}

func newOpusEncoder(bitrate int, frameMS float64) (*opusEncoder, error) {
	codec := astiav.FindEncoderByName("libopus")
	if codec == nil {
		return nil, errors.New("wasapi audio: this FFmpeg build has no libopus encoder")
	}
	e := &opusEncoder{cc: astiav.AllocCodecContext(codec)}
	if e.cc == nil {
		return nil, errors.New("wasapi audio: cannot allocate the Opus codec context")
	}
	e.cc.SetSampleRate(opusRate)
	e.cc.SetSampleFormat(astiav.SampleFormatFlt)
	e.cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	e.cc.SetBitRate(int64(bitrate))
	e.cc.SetTimeBase(astiav.NewRational(1, opusRate))
	d := astiav.NewDictionary()
	defer d.Free()
	// Same settings as the ffmpeg path: low-delay mode, short frames.
	for _, kv := range [][2]string{{"application", "lowdelay"}, {"frame_duration", strconv.FormatFloat(frameMS, 'f', -1, 64)}} {
		if err := d.Set(kv[0], kv[1], 0); err != nil {
			e.free()
			return nil, fmt.Errorf("wasapi audio: opus option %s: %w", kv[0], err)
		}
	}
	if err := e.cc.Open(codec, d); err != nil {
		e.free()
		return nil, fmt.Errorf("wasapi audio: open opus encoder: %w", err)
	}
	e.frameSize = e.cc.FrameSize()
	if e.frameSize <= 0 {
		e.frameSize = int(frameMS * opusRate / 1000)
	}
	e.frame = astiav.AllocFrame()
	e.frame.SetNbSamples(e.frameSize)
	e.frame.SetSampleFormat(astiav.SampleFormatFlt)
	e.frame.SetChannelLayout(astiav.ChannelLayoutStereo)
	e.frame.SetSampleRate(opusRate)
	if err := e.frame.AllocBuffer(0); err != nil {
		e.free()
		return nil, fmt.Errorf("wasapi audio: frame buffer: %w", err)
	}
	e.pkt = astiav.AllocPacket()
	return e, nil
}

// encode encodes one block of frameSize stereo frames and calls emit with
// a copy of every packet ready.
func (e *opusEncoder) encode(stereo []float32, emit func([]byte, time.Duration)) error {
	if err := e.frame.MakeWritable(); err != nil {
		return fmt.Errorf("wasapi audio: frame writable: %w", err)
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&stereo[0])), len(stereo)*4)
	if err := e.frame.Data().SetBytes(raw, 1); err != nil {
		return fmt.Errorf("wasapi audio: fill frame: %w", err)
	}
	e.frame.SetPts(e.pts)
	e.pts += int64(e.frameSize)
	if err := e.cc.SendFrame(e.frame); err != nil {
		return fmt.Errorf("wasapi audio: send frame: %w", err)
	}
	for {
		e.pkt.Unref()
		if err := e.cc.ReceivePacket(e.pkt); err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return nil
			}
			return fmt.Errorf("wasapi audio: receive packet: %w", err)
		}
		dur := time.Duration(e.pkt.Duration()) * time.Second / opusRate
		if dur <= 0 {
			dur = time.Duration(e.frameSize) * time.Second / opusRate
		}
		emit(append([]byte(nil), e.pkt.Data()...), dur)
	}
}

func (e *opusEncoder) free() {
	if e.pkt != nil {
		e.pkt.Free()
	}
	if e.frame != nil {
		e.frame.Free()
	}
	if e.cc != nil {
		e.cc.Free()
	}
}
