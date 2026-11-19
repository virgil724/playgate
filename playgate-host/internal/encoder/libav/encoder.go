//go:build cgo && libav

// Package libav is the in-process libav (FFmpeg libraries) H.264 encoder
// backend, built with `-tags libav` and CGO_ENABLED=1 against go-astiav. Unlike
// the CLI backend, bitrate changes and keyframe requests are applied between
// frames of the running encoder instead of restarting a subprocess.
//
// Supported codecs: libx264 and h264_nvenc. h264_vaapi and h264_v4l2m2m are
// rejected by New: they need hardware frame contexts / a different bitrate-change
// strategy (close + reopen) that is not implemented yet, so selecting them
// should use the CLI backend.
//
// Input: YUYV and NV12 VideoFrames are converted with swscale to yuv420p
// (libx264) or nv12 (h264_nvenc). MJPEG is not supported.
package libav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
)

// packetBufferSize bounds the encoded-packet output channel (same as the CLI
// backend: small, to keep latency low).
const packetBufferSize = 4

// frameQueueSize bounds the input queue between the channel reader and the
// encode loop; on overflow the oldest queued frame is dropped.
const frameQueueSize = 2

// maxInFlight bounds the capture-stamp table.
const maxInFlight = 64

// ErrUnsupported is returned by New for configurations the libav backend cannot
// handle (MJPEG input, VAAPI / v4l2m2m codecs).
var ErrUnsupported = errors.New("encoder backend libav: unsupported configuration")

type frameStamp struct {
	capture time.Time
	written time.Time
}

// Encoder is the in-process libav H.264 encoder. It implements
// core.VideoEncoder. Run is the sole writer of the packets and errs channels and
// closes them on exit.
type Encoder struct {
	log  *slog.Logger
	opts ffmpeg.Options
	in   <-chan core.VideoFrame

	packets chan core.EncodedPacket
	errs    chan error
	emit    *ffmpeg.PacketEmitter

	// Requests from other goroutines, applied by the encode loop before the next
	// frame: a pending IDR request and a pending target bitrate (0 = none).
	forceKey atomic.Bool
	bitrate  atomic.Int64

	codecName string
	encPixFmt astiav.PixelFormat

	// Everything below belongs to the encode goroutine (Run) after New returns.
	cc       *astiav.CodecContext
	srcFrame *astiav.Frame
	dstFrame *astiav.Frame
	sws      *astiav.SoftwareScaleContext
	pkt      *astiav.Packet
	curRate  int
	nextPts  int64
	stamps   map[int64]frameStamp
	closed   sync.Once
}

var _ core.VideoEncoder = (*Encoder)(nil)

// New builds and opens a libav encoder reading frames from in. Options are the
// same ffmpeg.Options the CLI backend takes.
func New(log *slog.Logger, opts ffmpeg.Options, in <-chan core.VideoFrame) (core.VideoEncoder, error) {
	o := opts.Normalised()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	var srcFmt astiav.PixelFormat
	switch o.InputFormat {
	case core.PixelFormatYUYV:
		srcFmt = astiav.PixelFormatYuyv422
	case core.PixelFormatNV12:
		srcFmt = astiav.PixelFormatNv12
	case core.PixelFormatMJPEG:
		return nil, fmt.Errorf("%w: MJPEG input (use YUYV or NV12, or the cli backend)", ErrUnsupported)
	default:
		return nil, fmt.Errorf("%w: input format %v", ErrUnsupported, o.InputFormat)
	}
	if o.Width%2 != 0 || o.Height%2 != 0 {
		return nil, fmt.Errorf("%w: frame size %dx%d must be even for 4:2:0", ErrUnsupported, o.Width, o.Height)
	}

	name := o.Codec.Name()
	e := &Encoder{
		log:       log.With("module", "encoder", "backend", "libav", "codec", name),
		opts:      o,
		in:        in,
		packets:   make(chan core.EncodedPacket, packetBufferSize),
		errs:      make(chan error, 1),
		emit:      newEmitter(log, o),
		codecName: name,
		curRate:   o.Bitrate,
		stamps:    make(map[int64]frameStamp),
	}
	switch name {
	case ffmpeg.CodecLibX264:
		e.encPixFmt = astiav.PixelFormatYuv420P
	case ffmpeg.CodecNVENC:
		e.encPixFmt = astiav.PixelFormatNv12
	default:
		return nil, fmt.Errorf("%w: codec %q (libav backend supports %s and %s; use the cli backend for others)",
			ErrUnsupported, name, ffmpeg.CodecLibX264, ffmpeg.CodecNVENC)
	}

	if err := e.open(srcFmt); err != nil {
		e.release()
		return nil, err
	}
	return e, nil
}

// newEmitter builds the packet emitter for o, carrying the intra-refresh mode.
func newEmitter(log *slog.Logger, o ffmpeg.Options) *ffmpeg.PacketEmitter {
	p := ffmpeg.NewPacketEmitter(log, o.FPS)
	p.IntraRefresh = o.IntraRefresh
	return p
}

// open allocates and opens the codec, frames and scaler.
func (e *Encoder) open(srcFmt astiav.PixelFormat) error {
	o := e.opts
	codec := astiav.FindEncoderByName(e.codecName)
	if codec == nil {
		return fmt.Errorf("libav: encoder %q not found in this FFmpeg build", e.codecName)
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return errors.New("libav: cannot allocate codec context")
	}
	e.cc = cc

	cc.SetWidth(o.Width)
	cc.SetHeight(o.Height)
	cc.SetPixelFormat(e.encPixFmt)
	cc.SetTimeBase(astiav.NewRational(1, o.FPS))
	cc.SetFramerate(astiav.NewRational(o.FPS, 1))
	cc.SetGopSize(o.GOPSize)
	cc.SetBitRate(int64(o.Bitrate))
	cc.SetRateControlMaxRate(int64(o.Bitrate))
	cc.SetRateControlBufferSize(ffmpeg.VBVBufsize(o.Bitrate, o.FPS))
	cc.SetMaxBFrames(0)
	// Low delay, and NO global header: SPS/PPS must be in-band in front of every
	// IDR so a viewer that joins or recovers mid-stream can decode.
	cc.SetFlags(astiav.NewCodecContextFlags(astiav.CodecContextFlagLowDelay))

	d := astiav.NewDictionary()
	defer d.Free()
	set := func(k, v string) error {
		if err := d.Set(k, v, 0); err != nil {
			return fmt.Errorf("libav: set option %s=%s: %w", k, v, err)
		}
		return nil
	}
	g := strconv.Itoa(o.GOPSize)
	opts := [][2]string{
		// AVCodecContext options (parsed from the dictionary by avcodec_open2);
		// bit rate, max rate, VBV buffer and B-frames use typed setters above.
		{"keyint_min", g},
		{"profile", "baseline"},
	}
	switch e.codecName {
	case ffmpeg.CodecLibX264:
		opts = append(opts,
			[2]string{"preset", o.Preset},
			[2]string{"tune", o.Tune},
			[2]string{"slices", "1"},
			// libx264 wrapper option: a frame with pict_type I becomes a real IDR
			// (X264_TYPE_IDR), not a plain I frame.
			[2]string{"forced-idr", "1"},
		)
		if o.IntraRefresh {
			// Rolling intra refresh (same libx264 option the cli backend passes as
			// -intra-refresh 1). The emitter's IntraRefresh flag stops it waiting
			// for IDRs that will no longer come.
			opts = append(opts, [2]string{"intra-refresh", "1"})
		}
	case ffmpeg.CodecNVENC:
		// Mirrors ffmpeg.NVENC.OutputArgs.
		opts = append(opts,
			[2]string{"preset", "p1"},
			[2]string{"tune", "ll"},
			[2]string{"rc", "cbr"},
			[2]string{"zerolatency", "1"},
			[2]string{"delay", "0"},
			[2]string{"forced-idr", "1"},
		)
	}
	for _, kv := range opts {
		if err := set(kv[0], kv[1]); err != nil {
			return err
		}
	}
	if err := cc.Open(codec, d); err != nil {
		return fmt.Errorf("libav: open %s: %w", e.codecName, err)
	}

	// Source frame in the capture format, destination frame in the encoder's.
	e.srcFrame = astiav.AllocFrame()
	e.srcFrame.SetWidth(o.Width)
	e.srcFrame.SetHeight(o.Height)
	e.srcFrame.SetPixelFormat(srcFmt)
	if err := e.srcFrame.AllocBuffer(0); err != nil {
		return fmt.Errorf("libav: allocate source frame buffer: %w", err)
	}
	e.dstFrame = astiav.AllocFrame()
	e.dstFrame.SetWidth(o.Width)
	e.dstFrame.SetHeight(o.Height)
	e.dstFrame.SetPixelFormat(e.encPixFmt)
	if err := e.dstFrame.AllocBuffer(0); err != nil {
		return fmt.Errorf("libav: allocate destination frame buffer: %w", err)
	}
	sws, err := astiav.CreateSoftwareScaleContext(o.Width, o.Height, srcFmt, o.Width, o.Height, e.encPixFmt,
		astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear))
	if err != nil {
		return fmt.Errorf("libav: create swscale context: %w", err)
	}
	e.sws = sws
	e.pkt = astiav.AllocPacket()
	return nil
}

// release frees every libav resource. Safe to call more than once.
func (e *Encoder) release() {
	e.closed.Do(func() {
		if e.sws != nil {
			e.sws.Free()
		}
		if e.pkt != nil {
			e.pkt.Free()
		}
		if e.srcFrame != nil {
			e.srcFrame.Free()
		}
		if e.dstFrame != nil {
			e.dstFrame.Free()
		}
		if e.cc != nil {
			e.cc.Free()
		}
	})
}

// Name implements core.Module.
func (e *Encoder) Name() string { return "encoder" }

// Packets implements core.VideoEncoder.
func (e *Encoder) Packets() <-chan core.EncodedPacket { return e.packets }

// Errors returns non-fatal encoder errors. The Encoder owns and closes it.
func (e *Encoder) Errors() <-chan error { return e.errs }

// Caps implements core.VideoEncoder: both operations are applied in-process
// between frames. (For nvenc the bitrate path relies on the driver accepting an
// in-place reconfigure; it is not verified on hardware here.)
func (e *Encoder) Caps() core.EncoderCaps {
	return core.EncoderCaps{LiveBitrate: true, CheapKeyframe: true}
}

// SetBitrate requests a new target bitrate in bits per second, applied (with
// matching maxrate and VBV buffer) before the next frame is encoded. Safe for
// concurrent use; non-blocking.
func (e *Encoder) SetBitrate(bps int) {
	if bps <= 0 {
		return
	}
	e.bitrate.Store(int64(bps))
}

// ForceKeyframe makes the next encoded frame an IDR. Safe for concurrent use;
// non-blocking; requests made before that frame coalesce.
func (e *Encoder) ForceKeyframe() { e.forceKey.Store(true) }

// Run implements core.Module. It encodes frames from the input channel until ctx
// is cancelled or the input closes, then flushes, frees libav state and closes
// Packets and Errors.
func (e *Encoder) Run(ctx context.Context) error {
	defer close(e.packets)
	defer close(e.errs)
	defer e.release()
	defer e.log.Info("encoder stopped")

	// The reader gets its own context so an encode error (ctx still live, input
	// still open) can stop it; otherwise the deferred wait below would block
	// forever and skip release/close.
	readCtx, cancelRead := context.WithCancel(ctx)
	queue := make(chan core.VideoFrame, frameQueueSize)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(queue)
		e.readFrames(readCtx, queue)
	}()
	defer func() { <-readerDone }()
	defer cancelRead() // runs before the wait above (LIFO)

	e.log.Info("libav encoder running", "bitrate", e.curRate, "gop", e.opts.GOPSize,
		"size", fmt.Sprintf("%dx%d", e.opts.Width, e.opts.Height))
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-queue:
			if !ok {
				if ctx.Err() == nil {
					e.flush(ctx)
				}
				return nil
			}
			if err := e.encode(ctx, frame); err != nil {
				e.log.Warn("encode failed; stopping", "err", err)
				select {
				case e.errs <- err:
				default:
				}
				return nil
			}
		}
	}
}

// readFrames pulls frames from the input channel into queue, dropping the oldest
// queued frame on overflow so a slow encoder never blocks capture.
func (e *Encoder) readFrames(ctx context.Context, queue chan core.VideoFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-e.in:
			if !ok {
				return
			}
			if f.PixelFormat != e.opts.InputFormat {
				e.log.Warn("dropping frame: format mismatch", "got", f.PixelFormat, "want", e.opts.InputFormat)
				continue
			}
			if f.Width != e.opts.Width || f.Height != e.opts.Height {
				e.log.Warn("dropping frame: resolution mismatch",
					"got", fmt.Sprintf("%dx%d", f.Width, f.Height),
					"want", fmt.Sprintf("%dx%d", e.opts.Width, e.opts.Height))
				continue
			}
			select {
			case queue <- f:
				continue
			default:
			}
			select {
			case <-queue:
				e.log.Debug("dropped frame: encoder not keeping up")
			default:
			}
			select {
			case queue <- f:
			default:
			}
		}
	}
}

// applyPending applies queued bitrate / keyframe requests to the next frame.
func (e *Encoder) applyPending() {
	if v := int(e.bitrate.Swap(0)); v > 0 && v != e.curRate {
		// libavcodec's libx264 wrapper notices these fields change before the
		// next frame and reconfigures x264 in place (no flush, no new GOP).
		e.cc.SetBitRate(int64(v))
		e.cc.SetRateControlMaxRate(int64(v))
		e.cc.SetRateControlBufferSize(ffmpeg.VBVBufsize(v, e.opts.FPS))
		e.log.Info("bitrate changed", "from", e.curRate, "to", v)
		e.curRate = v
	}
	if e.forceKey.Swap(false) {
		e.dstFrame.SetPictureType(astiav.PictureTypeI)
		e.log.Debug("forcing IDR on next frame")
	} else {
		e.dstFrame.SetPictureType(astiav.PictureTypeNone)
	}
}

func (e *Encoder) encode(ctx context.Context, f core.VideoFrame) error {
	written := time.Now()
	if err := e.fillFrame(f.Data); err != nil {
		e.log.Warn("dropping frame: bad raw data", "err", err, "len", len(f.Data))
		return nil
	}
	if err := e.dstFrame.MakeWritable(); err != nil {
		return fmt.Errorf("libav: make frame writable: %w", err)
	}
	if err := e.sws.ScaleFrame(e.srcFrame, e.dstFrame); err != nil {
		return fmt.Errorf("libav: swscale: %w", err)
	}
	e.applyPending()

	pts := e.nextPts
	e.nextPts++
	e.dstFrame.SetPts(pts)
	if len(e.stamps) >= maxInFlight {
		for k := range e.stamps {
			if k < pts-maxInFlight {
				delete(e.stamps, k)
			}
		}
	}
	e.stamps[pts] = frameStamp{capture: f.Timestamp, written: written}

	if err := e.cc.SendFrame(e.dstFrame); err != nil {
		return fmt.Errorf("libav: send frame: %w", err)
	}
	return e.drain(ctx)
}

// drain emits every packet the encoder has ready.
func (e *Encoder) drain(ctx context.Context) error {
	for {
		e.pkt.Unref()
		err := e.cc.ReceivePacket(e.pkt)
		if err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return nil
			}
			return fmt.Errorf("libav: receive packet: %w", err)
		}
		st := e.stamps[e.pkt.Pts()]
		delete(e.stamps, e.pkt.Pts())
		key := e.pkt.Flags().Has(astiav.PacketFlagKey)
		e.emit.Emit(ctx, e.packets, e.pkt.Data(), key, st.capture, st.written)
	}
}

// flush drains the encoder at end of stream.
func (e *Encoder) flush(ctx context.Context) {
	if err := e.cc.SendFrame(nil); err != nil {
		return
	}
	_ = e.drain(ctx)
}

// fillFrame copies one tightly packed raw image (no row padding, the layout
// V4L2 delivers) into the source frame.
func (e *Encoder) fillFrame(raw []byte) error {
	need, err := e.srcFrame.ImageBufferSize(1)
	if err != nil {
		return err
	}
	if len(raw) < need {
		return fmt.Errorf("short frame: %d bytes, need %d", len(raw), need)
	}
	if err := e.srcFrame.MakeWritable(); err != nil {
		return err
	}
	return e.srcFrame.Data().SetBytes(raw[:need], 1)
}
