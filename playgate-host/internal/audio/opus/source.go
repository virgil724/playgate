// Package opus provides a live Opus audio source: it captures audio from an ALSA
// device through an ffmpeg subprocess that muxes Opus into an Ogg stream, then
// parses the Ogg pages into Opus payloads ready to write onto a WebRTC audio
// track.
//
// The design mirrors the video encoder (internal/encoder/ffmpeg): one ffmpeg
// subprocess fed/drained by goroutines, a bounded drop-oldest output channel so a
// slow consumer never blocks capture, and a supervision loop that relaunches
// ffmpeg if it dies so a transient ALSA glitch self-heals. Audio runs as its own
// independent pipeline; the browser resynchronises it with video from the RTP
// timestamps on each track.
package opus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// opusClockRate is Opus's fixed wire clock (48 kHz), used only for documentation
// of the duration maths; per-packet durations are derived from the TOC byte.
const opusClockRate = 48000

// packetBufferSize bounds the output channel. Opus pages are tiny (~one per
// frame_duration); a small buffer keeps latency low while absorbing brief jitter.
const packetBufferSize = 16

// restartBackoff is how long the supervision loop waits before relaunching ffmpeg
// after it exits unexpectedly, so a missing/busy device cannot hot-spin.
const restartBackoff = time.Second

// Packet is one Opus access unit (the payload of a single Ogg page) plus its
// presentation duration, derived from the Ogg granule position. It is written
// straight onto a Pion audio track as a media.Sample.
type Packet struct {
	Data     []byte
	Duration time.Duration
}

// Config configures the ALSA→Opus source. The zero value is not valid; use
// DefaultConfig and override fields as needed.
type Config struct {
	// FFmpegPath is the ffmpeg binary to exec. Defaults to "ffmpeg" (PATH lookup).
	FFmpegPath string
	// Device is the ALSA capture device, e.g. "default", "hw:CARD=MS2109,DEV=0".
	Device string
	// SampleRate / Channels describe the ALSA capture format. Opus output is
	// always resampled to 48 kHz stereo on the wire.
	SampleRate int
	Channels   int
	// Bitrate is the target Opus bitrate in bits per second.
	Bitrate int
}

// DefaultConfig returns a Config with sensible defaults for an HDMI capture
// card's ALSA endpoint.
func DefaultConfig() Config {
	return Config{
		FFmpegPath: "ffmpeg",
		Device:     "default",
		SampleRate: 48000,
		Channels:   2,
		Bitrate:    128000,
	}
}

func (c Config) normalise() Config {
	if c.FFmpegPath == "" {
		c.FFmpegPath = "ffmpeg"
	}
	if c.Device == "" {
		c.Device = "default"
	}
	if c.SampleRate <= 0 {
		c.SampleRate = 48000
	}
	if c.Channels <= 0 {
		c.Channels = 2
	}
	if c.Bitrate <= 0 {
		c.Bitrate = 128000
	}
	return c
}

// Source captures Opus audio from ALSA via ffmpeg and emits Packets. It
// implements core.Module.
//
// Ownership: Source owns the packets channel — Run is the sole writer and closes
// it on exit, per the core channel contract.
type Source struct {
	log  *slog.Logger
	cfg  Config
	opts Config // normalised

	packets chan Packet
}

var _ core.Module = (*Source)(nil)

// New constructs an ALSA→Opus Source.
func New(log *slog.Logger, cfg Config) *Source {
	if log == nil {
		log = slog.Default()
	}
	o := cfg.normalise()
	return &Source{
		log:     log.With("module", "audio", "device", o.Device),
		cfg:     cfg,
		opts:    o,
		packets: make(chan Packet, packetBufferSize),
	}
}

// Name implements core.Module.
func (s *Source) Name() string { return "audio" }

// Packets returns the receive-only channel of Opus packets. Source owns and
// closes it on exit.
func (s *Source) Packets() <-chan Packet { return s.packets }

// args builds the ffmpeg arg vector: capture from ALSA, encode to Opus, mux into
// an Ogg stream on stdout. -application lowdelay + a short frame_duration favour
// latency over compression efficiency, which suits interactive game audio.
func (s *Source) args() []string {
	return []string{
		"-hide_banner", "-nostats",
		"-f", "alsa",
		"-ac", itoa(s.opts.Channels),
		"-ar", itoa(s.opts.SampleRate),
		"-i", s.opts.Device,
		"-c:a", "libopus",
		"-b:a", itoa(s.opts.Bitrate),
		"-ar", itoa(opusClockRate),
		"-ac", "2",
		"-application", "lowdelay",
		"-frame_duration", "20",
		// Without this the Ogg muxer buffers ~1s of audio per page (default
		// page_duration), adding ~1s of latency and bursting packets. Flush small
		// pages and disable output buffering so packets stream out promptly.
		"-page_duration", "20000",
		"-flush_packets", "1",
		"-f", "ogg",
		"pipe:1",
	}
}

// Run implements core.Module. It supervises the ffmpeg subprocess, relaunching it
// with a short backoff if it exits unexpectedly, until ctx is cancelled.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.packets)
	defer s.log.Info("audio source stopped")

	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.runSubprocess(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("audio ffmpeg ended; will restart", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		// Backoff so a missing/busy device does not hot-spin.
		t := time.NewTimer(restartBackoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// runSubprocess launches one ffmpeg, parses its Ogg/Opus output into Packets, and
// returns when ctx is cancelled or the subprocess exits/errors.
func (s *Source) runSubprocess(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := s.args()
	cmd := exec.CommandContext(runCtx, s.opts.FFmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("audio: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("audio: stderr pipe: %w", err)
	}

	s.log.Info("starting audio ffmpeg", "path", s.opts.FFmpegPath, "args", args)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("audio: start ffmpeg: %w", err)
	}

	// Drain stderr so ffmpeg never blocks on a full stderr pipe.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			s.log.Debug("ffmpeg", "msg", sc.Text())
		}
	}()

	readErr := s.readPages(runCtx, stdout)
	cancel() // ensure ffmpeg is torn down before we wait
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	return waitErr
}

// readPages demuxes the raw Ogg stream from ffmpeg into individual Opus packets.
//
// Pion's oggreader returns a whole Ogg page's payload as one blob, but ffmpeg
// packs ~1.8 Opus packets per page, and a WebRTC RTP payload must carry exactly
// ONE Opus packet — concatenating two makes the browser's decoder choke (silent
// or garbled audio). So we parse the page segment table (lacing values) ourselves
// to recover packet boundaries, and derive each packet's duration from its Opus
// TOC byte rather than the page granule, which would otherwise be wrong per
// packet.
func (s *Source) readPages(ctx context.Context, stdout io.Reader) error {
	br := bufio.NewReaderSize(stdout, 64*1024)
	header := make([]byte, 27) // Ogg page fixed header

	// partial accumulates bytes of an Opus packet that spans page boundaries (the
	// final lacing value of a page being 255 means "continues on the next page").
	var partial []byte

	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := io.ReadFull(br, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("audio: read ogg page header: %w", err)
		}
		if string(header[0:4]) != "OggS" {
			return fmt.Errorf("audio: lost ogg sync (got %q)", header[0:4])
		}

		nseg := int(header[26])
		segTable := make([]byte, nseg)
		if _, err := io.ReadFull(br, segTable); err != nil {
			return fmt.Errorf("audio: read ogg segment table: %w", err)
		}
		dataLen := 0
		for _, v := range segTable {
			dataLen += int(v)
		}
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(br, data); err != nil {
			return fmt.Errorf("audio: read ogg page data: %w", err)
		}

		// Walk the lacing values: each run of 255s followed by a value <255 marks
		// one packet. A trailing run of 255s (no terminator on this page) means the
		// packet continues onto the next page, so we carry it in `partial`.
		acc := partial
		partial = nil
		pos := 0
		for _, v := range segTable {
			acc = append(acc, data[pos:pos+int(v)]...)
			pos += int(v)
			if v < 255 {
				s.handlePacket(ctx, acc)
				acc = nil
			}
		}
		if len(acc) > 0 {
			partial = acc
		}
	}
}

// handlePacket emits one Opus packet, skipping the Ogg/Opus header packets
// (OpusHead, OpusTags) which carry no audio.
func (s *Source) handlePacket(ctx context.Context, pkt []byte) {
	if len(pkt) == 0 {
		return
	}
	if len(pkt) >= 8 {
		switch string(pkt[0:8]) {
		case "OpusHead", "OpusTags":
			return
		}
	}
	dur := opusPacketDuration(pkt)
	if dur <= 0 {
		return
	}
	// Copy: acc's backing array is reused across pages, so the packet must own its
	// bytes before it travels down the channel.
	out := make([]byte, len(pkt))
	copy(out, pkt)
	s.emit(ctx, Packet{Data: out, Duration: dur})
}

// opusPacketDuration computes an Opus packet's playback duration from its TOC
// byte (RFC 6716 §3.1): the config number selects the frame size and the code
// selects how many frames the packet carries.
func opusPacketDuration(pkt []byte) time.Duration {
	toc := pkt[0]
	config := toc >> 3
	code := toc & 0x03

	var frameUs int
	switch {
	case config < 12: // SILK-only: 10/20/40/60 ms
		frameUs = []int{10000, 20000, 40000, 60000}[config%4]
	case config < 16: // Hybrid: 10/20 ms
		frameUs = []int{10000, 20000}[config%2]
	default: // CELT-only: 2.5/5/10/20 ms
		frameUs = []int{2500, 5000, 10000, 20000}[config%4]
	}

	frames := 1
	switch code {
	case 1, 2:
		frames = 2
	case 3:
		if len(pkt) >= 2 {
			frames = int(pkt[1] & 0x3F)
		}
	}
	return time.Duration(frameUs*frames) * time.Microsecond
}

// emit pushes a packet with drop-oldest semantics so a stalled consumer never
// blocks the audio pipeline. A dropped Opus page is a brief audible gap, which is
// preferable to letting audio latency accumulate behind a slow viewer.
func (s *Source) emit(ctx context.Context, pkt Packet) {
	select {
	case s.packets <- pkt:
		return
	default:
	}
	select {
	case <-s.packets:
	default:
	}
	select {
	case s.packets <- pkt:
	case <-ctx.Done():
	default:
		s.log.Debug("dropped opus page: consumer not keeping up")
	}
}

// itoa is a tiny strconv.Itoa wrapper kept local so the arg builder reads cleanly.
func itoa(n int) string { return fmt.Sprintf("%d", n) }
