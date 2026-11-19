// Package ffmpeg implements a core H.264 encoder that shells out to an ffmpeg
// subprocess: it writes raw frames (rawvideo YUYV or mjpeg) to ffmpeg's stdin
// and reads an FLV-framed H.264 stream from its stdout, which it demuxes into
// per-access-unit core.EncodedPacket values (Annex-B payloads).
//
// The package is deliberately CGo-free (it execs the ffmpeg binary) so it
// cross-compiles to every platform. The Linux pipeline test that actually runs
// the binary lives under cmd/. The argument assembly and stream parsing logic
// here are pure and unit-tested on any OS.
package ffmpeg

import (
	"fmt"
	"strconv"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// Options configures the ffmpeg H.264 encoder. Zero values fall back to the
// defaults applied by DefaultOptions / normalise.
type Options struct {
	// FFmpegPath is the ffmpeg binary to exec. Defaults to "ffmpeg" (PATH lookup).
	FFmpegPath string
	// Width / Height of the incoming frames. Required for rawvideo input.
	Width  int
	Height int
	// FPS is the input/output frame rate.
	FPS int
	// InputFormat is the pixel format of the incoming VideoFrames.
	InputFormat core.PixelFormat
	// Bitrate is the target H.264 bitrate in bits per second.
	Bitrate int
	// GOPSize is the keyframe interval in frames (ffmpeg -g). Short GOP keeps
	// latency and recovery time low for live streaming.
	GOPSize int
	// Preset is the x264 speed/quality preset (e.g. "ultrafast").
	Preset string
	// Tune is the x264 tuning (e.g. "zerolatency").
	Tune string
	// Codec selects the encoder strategy. Defaults to a libx264 software codec.
	// Phase 3 hardware encoders (h264_v4l2m2m, vaapi, nvenc) plug in here by
	// supplying a different Codec without touching the subprocess plumbing.
	Codec Codec
	// IntraRefresh enables x264 rolling intra refresh instead of periodic IDRs
	// (libx264 only). GOPSize is then the refresh period.
	IntraRefresh bool
	// ForceKeyframeDebounce bounds how often ForceKeyframe may restart the ffmpeg
	// subprocess (CLI backend). 0 uses DefaultForceKeyframeDebounce (1s).
	ForceKeyframeDebounce time.Duration
}

// DefaultOptions returns low-latency software-encode defaults for live
// streaming at the given geometry.
func DefaultOptions(width, height, fps int, in core.PixelFormat) Options {
	return Options{
		FFmpegPath:  "ffmpeg",
		Width:       width,
		Height:      height,
		FPS:         fps,
		InputFormat: in,
		Bitrate:     6_000_000,
		GOPSize:     30,
		Preset:      "ultrafast",
		Tune:        "zerolatency",
		Codec:       LibX264{},
	}
}

// normalise fills in zero-valued fields with their defaults and returns the
// effective options, leaving the caller's struct untouched.
func (o Options) normalise() Options {
	if o.FFmpegPath == "" {
		o.FFmpegPath = "ffmpeg"
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	if o.Bitrate <= 0 {
		o.Bitrate = 6_000_000
	}
	if o.GOPSize <= 0 {
		o.GOPSize = 30
	}
	if o.Preset == "" {
		o.Preset = "ultrafast"
	}
	if o.Tune == "" {
		o.Tune = "zerolatency"
	}
	if o.Codec == nil {
		o.Codec = LibX264{}
	}
	return o
}

// Validate checks the options needed to assemble a valid command line.
func (o Options) Validate() error {
	if o.Width <= 0 || o.Height <= 0 {
		return fmt.Errorf("encoder: frame size must be positive, got %dx%d", o.Width, o.Height)
	}
	switch o.InputFormat {
	case core.PixelFormatYUYV, core.PixelFormatNV12, core.PixelFormatMJPEG:
	default:
		return fmt.Errorf("encoder: unsupported input format %v (want YUYV, NV12 or MJPEG)", o.InputFormat)
	}
	if o.IntraRefresh && o.Codec != nil && o.Codec.Name() != CodecLibX264 {
		return fmt.Errorf("encoder: intra refresh requires libx264, got %s", o.Codec.Name())
	}
	return nil
}

// Codec abstracts the encoder-specific portion of the ffmpeg command line so
// software (libx264) and future hardware encoders share the subprocess and
// stream-parsing machinery. Implementations return the output-side ffmpeg flags
// (codec selection, rate control, latency tuning).
type Codec interface {
	// Name is used for logging.
	Name() string
	// OutputArgs returns the ffmpeg arguments that follow the input spec, given
	// the normalised options.
	OutputArgs(o Options) []string
}

// LibX264 is the software (CPU) H.264 encoder. It is the default and Phase-1
// baseline. Hardware codecs implement the same Codec interface.
type LibX264 struct{}

// Name implements Codec.
func (LibX264) Name() string { return "libx264" }

// OutputArgs implements Codec for software x264 with low-latency tuning.
func (LibX264) OutputArgs(o Options) []string {
	args := []string{
		"-c:v", CodecLibX264,
		"-preset", o.Preset,
		"-tune", o.Tune,
	}
	args = append(args, rateControlArgs(o)...)
	args = append(args, gopArgs(o)...)
	if o.IntraRefresh {
		args = append(args, "-intra-refresh", "1")
	}
	args = append(args, "-pix_fmt", "yuv420p", "-slices", "1") // 4:2:0 is what H.264/WebRTC decoders expect; 1 slice per frame
	return args
}

// inputArgs returns the ffmpeg input-side flags describing the raw frames we
// feed on stdin. For YUYV and NV12 we use the rawvideo demuxer with explicit
// geometry and pixel format; for MJPEG we use the mjpeg demuxer (each frame is
// a self-describing JPEG).
func (o Options) inputArgs() ([]string, error) {
	switch o.InputFormat {
	case core.PixelFormatYUYV:
		return []string{
			"-f", "rawvideo",
			"-pixel_format", "yuyv422",
			"-video_size", fmt.Sprintf("%dx%d", o.Width, o.Height),
			"-framerate", strconv.Itoa(o.FPS),
			"-i", "pipe:0",
		}, nil
	case core.PixelFormatNV12:
		return []string{
			"-f", "rawvideo",
			"-pixel_format", "nv12",
			"-video_size", fmt.Sprintf("%dx%d", o.Width, o.Height),
			"-framerate", strconv.Itoa(o.FPS),
			"-i", "pipe:0",
		}, nil
	case core.PixelFormatMJPEG:
		return []string{
			// Without these ffmpeg reads several MB of frames to probe the stream
			// before emitting anything; the first JPEG already describes it.
			"-probesize", "32",
			"-analyzeduration", "0",
			"-f", "mjpeg",
			"-framerate", strconv.Itoa(o.FPS),
			"-i", "pipe:0",
		}, nil
	default:
		return nil, fmt.Errorf("encoder: unsupported input format %v", o.InputFormat)
	}
}

// BuildArgs assembles the full ffmpeg argument vector (excluding the binary
// name itself) for the given options. The output is an FLV stream on stdout
// (see flv.go for why not raw Annex-B).
func BuildArgs(o Options) ([]string, error) {
	o = o.normalise()
	if err := o.Validate(); err != nil {
		return nil, err
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	// No "-fflags nobuffer": it makes ffmpeg discard the frames it reads while
	// probing the input, so the first frame never comes out, and it does not make
	// later frames any faster.

	// Hardware codecs (e.g. VA-API) need device-init flags BEFORE the input and a
	// hwupload filter AFTER it. Software codecs implement neither and contribute
	// nothing here.
	hw, isHW := o.Codec.(hwInitArgs)
	if isHW {
		args = append(args, hw.InitArgs(o)...)
	}

	in, err := o.inputArgs()
	if err != nil {
		return nil, err
	}
	args = append(args, in...)

	if isHW {
		args = append(args, hw.FilterArgs(o)...)
	}

	args = append(args, o.Codec.OutputArgs(o)...)

	// Output: FLV on stdout, one tag per access unit, flushed per packet. stdout
	// is not seekable, so skip the metadata the muxer would try to patch at exit.
	args = append(args,
		"-f", "flv",
		"-flvflags", "no_duration_filesize+no_metadata",
		"-flush_packets", "1",
		"pipe:1",
	)
	return args, nil
}
