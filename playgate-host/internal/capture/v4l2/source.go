//go:build linux

package v4l2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/unix"

	"github.com/playgate/playgate-host/internal/core"
)

// Source is a core.CaptureSource backed by a Linux V4L2 device. It opens the
// device in Start (so configuration errors surface synchronously), then streams
// frames in Run until the context is cancelled or the device errors out.
//
// Ownership: Source owns the frames and errs channels. Run is the sole writer
// and closes both exactly once on exit, per the core channel contract.
type Source struct {
	log *slog.Logger
	cfg Config

	frames chan core.VideoFrame
	errs   chan error

	// negotiated state, set by Start.
	dev    *device
	fourcc uint32
	pixfmt core.PixelFormat
	width  int
	height int
	// bytesPerLine and sizeImage come from VIDIOC_S_FMT after negotiation. For
	// uncompressed formats, drivers may pad each row; ffmpeg rawvideo expects a
	// tightly packed frame, so forward repacks using this stride.
	bytesPerLine int
	sizeImage    int
}

// Ensure Source satisfies core.CaptureSource at compile time.
var _ core.CaptureSource = (*Source)(nil)

// New constructs a V4L2 capture Source. The config is validated eagerly so
// callers get an error before Run is scheduled. The device itself is not opened
// until Start.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("v4l2 capture: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		log:    log.With("module", "capture", "device", cfg.Device),
		cfg:    cfg,
		frames: make(chan core.VideoFrame, FrameBufferSize),
		errs:   make(chan error, 1),
	}, nil
}

// Name implements core.Module.
func (s *Source) Name() string { return "capture" }

// Frames implements core.CaptureSource.
func (s *Source) Frames() <-chan core.VideoFrame { return s.frames }

// Errors implements core.CaptureSource.
func (s *Source) Errors() <-chan error { return s.errs }

// Start opens the device, negotiates a pixel format from the configured
// preference order, applies the resolution/fps, sets up mmap streaming buffers,
// and begins streaming. It is non-blocking: the dequeue loop runs in Run.
// Returning an error means capture could not be initialised and Run should not
// be called.
func (s *Source) Start(_ context.Context) error {
	// Probe supported formats up front so negotiation is honest about what the
	// hardware can do (rather than blindly S_FMT-ing and hoping).
	info, err := QueryDevice(s.cfg.Device)
	if err != nil {
		return fmt.Errorf("v4l2 capture: query %s: %w", s.cfg.Device, err)
	}
	pixfmt, fourcc, err := NegotiateFormat(s.cfg.preferred(), info.AvailableFourCCSet())
	if err != nil {
		return fmt.Errorf("v4l2 capture: %w", err)
	}

	dev, err := openDevice(s.cfg.Device)
	if err != nil {
		return fmt.Errorf("v4l2 capture: open %s: %w", s.cfg.Device, err)
	}

	actual, err := dev.setFormat(uint32(s.cfg.Width), uint32(s.cfg.Height), fourcc)
	if err != nil {
		_ = dev.close()
		return fmt.Errorf("v4l2 capture: set format: %w", err)
	}
	if actual.PixelFormat != fourcc {
		_ = dev.close()
		return fmt.Errorf("v4l2 capture: driver chose %s, wanted %s",
			FourCCString(actual.PixelFormat), FourCCString(fourcc))
	}

	// Frame rate is best-effort: some drivers reject S_PARM. Log, don't fail.
	if err := dev.setFrameRate(uint32(s.cfg.FPS)); err != nil {
		s.log.Debug("could not set frame rate", "err", err)
	}

	if err := dev.initBuffers(FrameBufferSize); err != nil {
		_ = dev.close()
		return fmt.Errorf("v4l2 capture: init buffers: %w", err)
	}

	if err := dev.streamOn(); err != nil {
		_ = dev.close()
		return fmt.Errorf("v4l2 capture: start stream: %w", err)
	}

	s.dev = dev
	s.fourcc = fourcc
	s.pixfmt = pixfmt
	s.width = int(actual.Width)
	s.height = int(actual.Height)
	s.bytesPerLine = int(actual.BytesPerLine)
	s.sizeImage = int(actual.SizeImage)

	s.log.Info("capture started",
		"format", FourCCString(fourcc),
		"width", s.width, "height", s.height, "fps", s.cfg.FPS,
		"bytes_per_line", s.bytesPerLine, "size_image", s.sizeImage)
	return nil
}

// Run implements core.Module. It dequeues frames from the device, copies each
// into a core.VideoFrame, requeues the mmap buffer, and forwards the frame to
// Frames() using a drop-oldest policy so a slow consumer never blocks capture.
// It owns and closes the frames and errs channels on exit.
//
// Failure modes:
//   - ctx cancelled: clean shutdown, returns nil.
//   - device error (e.g. card unplugged → EIO/ENODEV): reported on Errors() and
//     the module ends itself (returns nil) so the supervisor can restart it
//     without treating it as a fatal pipeline error.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.frames)
	defer close(s.errs)
	defer s.log.Info("capture stopped")

	if s.dev == nil {
		return fmt.Errorf("v4l2 capture: Run called before successful Start")
	}
	defer func() { _ = s.dev.close() }()

	// DQBUF is a blocking ioctl, so run it in a goroutine and bridge frames /
	// errors back over channels we can select on alongside ctx.Done().
	type dq struct {
		buf v4l2Buffer
		err error
	}
	dqCh := make(chan dq)
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			buf, err := s.dev.dequeueBuffer()
			select {
			case dqCh <- dq{buf: buf, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return // fatal device error; stop dequeuing
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil

		case r := <-dqCh:
			if r.err != nil {
				// Device-level error (card unplugged → EIO/ENODEV, etc.).
				// Report and end the module; do not panic.
				s.emitError(ctx, fmt.Errorf("v4l2 capture: device error: %w", classifyDQErr(r.err)))
				return nil
			}
			s.forward(ctx, r.buf)
			// Requeue the buffer so the driver can fill it again.
			if err := s.dev.queueBuffer(r.buf.Index); err != nil {
				s.emitError(ctx, fmt.Errorf("v4l2 capture: requeue buffer: %w", err))
				return nil
			}
		}
	}
}

// classifyDQErr leaves the underlying errno intact but is a hook for any future
// remapping (e.g. distinguishing "device gone" from "transient"). EIO/ENODEV on
// a capture card almost always mean the card was unplugged.
func classifyDQErr(err error) error {
	if errors.Is(err, unix.ENODEV) {
		return fmt.Errorf("device disappeared: %w", err)
	}
	return err
}

// forward copies a dequeued mmap buffer into a core.VideoFrame and pushes it
// onto the output channel with a drop-oldest policy. The mmap region is reused
// once the buffer is requeued, so we always copy before returning.
// monotonicNow returns the current CLOCK_MONOTONIC reading as a Duration since
// boot, or 0 on error. Used to convert V4L2 buffer timestamps to wall-clock.
func monotonicNow() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)*time.Nanosecond
}

func (s *Source) forward(ctx context.Context, buf v4l2Buffer) {
	idx := int(buf.Index)
	if idx < 0 || idx >= len(s.dev.buffers) {
		return
	}
	src := s.dev.buffers[idx].data
	data, ok := s.copyFrameData(src, int(buf.BytesUsed))
	if !ok {
		return
	}

	// Build a wall-clock capture timestamp. V4L2's buf.Timestamp is CLOCK_MONOTONIC
	// (seconds since boot), NOT Unix epoch — feeding it to time.Unix would yield a
	// ~1970 time and make latency stats read as ~56 years. Convert by anchoring the
	// monotonic capture instant to wall-clock via the current monotonic↔wall offset:
	//   wall_capture = now - (mono_now - mono_capture)
	// so latency taps measure the real driver/USB capture-to-process delay.
	ts := time.Now()
	if buf.Timestamp.Sec != 0 || buf.Timestamp.Usec != 0 {
		capMono := time.Duration(buf.Timestamp.Sec)*time.Second + time.Duration(buf.Timestamp.Usec)*time.Microsecond
		if nowMono := monotonicNow(); nowMono > 0 && capMono > 0 && capMono <= nowMono {
			ts = time.Now().Add(-(nowMono - capMono))
		}
	}

	frame := core.VideoFrame{
		Data:        data,
		PixelFormat: s.pixfmt,
		Width:       s.width,
		Height:      s.height,
		Timestamp:   ts,
	}

	// Drop-oldest: if the buffered channel is full, discard the stalest queued
	// frame and retry once. Latency beats completeness for live streaming.
	select {
	case s.frames <- frame:
	default:
		select {
		case <-s.frames: // drop one stale frame
		default:
		}
		select {
		case s.frames <- frame:
		case <-ctx.Done():
		default:
			s.log.Debug("dropped frame: consumer not keeping up")
		}
	}
}

// emitError sends a non-fatal error on Errors() without blocking. The channel
// is buffered (cap 1); if it is already full the error is dropped because the
// module is about to exit anyway.
func (s *Source) emitError(ctx context.Context, err error) {
	s.log.Warn("capture error", "err", err)
	select {
	case s.errs <- err:
	case <-ctx.Done():
	default:
	}
}

// copyFrameData returns a frame payload suitable for the encoder. Compressed
// MJPEG is variable-length, so BytesUsed is authoritative. Raw formats are
// fixed-size; some capture drivers under-report BytesUsed, and some pad rows to
// bytesPerLine, so use the negotiated geometry/stride instead.
func (s *Source) copyFrameData(src []byte, bytesUsed int) ([]byte, bool) {
	switch s.pixfmt {
	case core.PixelFormatMJPEG:
		if bytesUsed > len(src) {
			bytesUsed = len(src)
		}
		if bytesUsed <= 0 {
			s.log.Warn("dropping frame: empty MJPEG buffer")
			return nil, false
		}
		data := make([]byte, bytesUsed)
		copy(data, src[:bytesUsed])
		return data, true
	case core.PixelFormatNV12:
		return s.copyNV12(src, bytesUsed)
	case core.PixelFormatYUYV:
		return s.copyYUYV(src, bytesUsed)
	default:
		s.log.Warn("dropping frame: unsupported pixel format", "format", s.pixfmt)
		return nil, false
	}
}

func (s *Source) copyYUYV(src []byte, bytesUsed int) ([]byte, bool) {
	rowBytes := s.width * 2
	stride := s.bytesPerLine
	if stride <= 0 {
		stride = rowBytes
	}
	if stride < rowBytes {
		s.log.Warn("dropping frame: invalid YUYV stride",
			"bytes_used", bytesUsed, "stride", stride, "row_bytes", rowBytes)
		return nil, false
	}
	required := stride * s.height
	if len(src) < required {
		s.log.Warn("dropping frame: YUYV buffer too small",
			"bytes_used", bytesUsed, "buffer_len", len(src), "required", required)
		return nil, false
	}
	data := make([]byte, rowBytes*s.height)
	copyRows(data, rowBytes, src, stride, s.height)
	return data, true
}

func (s *Source) copyNV12(src []byte, bytesUsed int) ([]byte, bool) {
	rowBytes := s.width
	stride := s.bytesPerLine
	if stride <= 0 {
		stride = rowBytes
	}
	if stride < rowBytes {
		s.log.Warn("dropping frame: invalid NV12 stride",
			"bytes_used", bytesUsed, "stride", stride, "row_bytes", rowBytes)
		return nil, false
	}
	uvRows := (s.height + 1) / 2
	required := stride * (s.height + uvRows)
	if len(src) < required {
		s.log.Warn("dropping frame: NV12 buffer too small",
			"bytes_used", bytesUsed, "buffer_len", len(src), "required", required)
		return nil, false
	}

	data := make([]byte, rowBytes*(s.height+uvRows))
	copyRows(data[:rowBytes*s.height], rowBytes, src[:stride*s.height], stride, s.height)
	copyRows(data[rowBytes*s.height:], rowBytes, src[stride*s.height:], stride, uvRows)
	return data, true
}

func copyRows(dst []byte, dstStride int, src []byte, srcStride int, rows int) {
	for y := 0; y < rows; y++ {
		copy(dst[y*dstStride:(y+1)*dstStride], src[y*srcStride:y*srcStride+dstStride])
	}
}
