// Package ffmpeg implements a core H.264 encoder that shells out to an ffmpeg
// subprocess: it writes raw frames (rawvideo YUYV or mjpeg) to ffmpeg's stdin
// and reads an H.264 Annex-B byte stream from its stdout, which it splits into
// per-access-unit core.EncodedPacket values.
//
// The package is deliberately CGo-free (it execs the ffmpeg binary) so it
// cross-compiles to every platform. The Linux pipeline test that actually runs
// the binary lives under cmd/. The argument assembly and Annex-B parsing logic
// here are pure and unit-tested on any OS.
package ffmpeg

import (
	"fmt"
	"strconv"

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
	case core.PixelFormatYUYV, core.PixelFormatMJPEG:
	default:
		return fmt.Errorf("encoder: unsupported input format %v (want YUYV or MJPEG)", o.InputFormat)
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
	args = append(args, "-pix_fmt", "yuv420p") // 4:2:0 is what H.264/WebRTC decoders expect
	return args
}

// inputArgs returns the ffmpeg input-side flags describing the raw frames we
// feed on stdin. For YUYV we use the rawvideo demuxer with explicit geometry;
// for MJPEG we use the mjpeg demuxer (each frame is a self-describing JPEG).
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
	case core.PixelFormatMJPEG:
		return []string{
			"-f", "mjpeg",
			"-framerate", strconv.Itoa(o.FPS),
			"-i", "pipe:0",
		}, nil
	default:
		return nil, fmt.Errorf("encoder: unsupported input format %v", o.InputFormat)
	}
}

// BuildArgs assembles the full ffmpeg argument vector (excluding the binary
// name itself) for the given options. The output is an H.264 Annex-B byte
// stream on stdout.
func BuildArgs(o Options) ([]string, error) {
	o = o.normalise()
	if err := o.Validate(); err != nil {
		return nil, err
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-fflags", "nobuffer", // don't buffer input: minimise latency
	}

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

	// Output: raw H.264 Annex-B byte stream on stdout.
	args = append(args,
		"-f", "h264",
		"-bsf:v", "h264_mp4toannexb", // ensure Annex-B start codes (no-op if already AnnexB)
		"pipe:1",
	)
	return args, nil
}
