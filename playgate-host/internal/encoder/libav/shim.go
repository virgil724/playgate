//go:build cgo && libav

package libav

/*
#cgo pkg-config: libavcodec libavutil
#include <errno.h>
#include <libavcodec/avcodec.h>
#include <libavutil/frame.h>
#include <libavutil/imgutils.h>
#include <libavutil/pixfmt.h>

// pg_frame_get_buffer allocates a refcounted buffer for a frame whose width,
// height and format are already set.
static int pg_frame_get_buffer(AVFrame *f) { return av_frame_get_buffer(f, 0); }

// pg_frame_make_writable makes the frame's buffer exclusively owned (it may
// still be referenced by the encoder after avcodec_send_frame).
static int pg_frame_make_writable(AVFrame *f) { return av_frame_make_writable(f); }

// pg_frame_fill copies one tightly packed raw image (no row padding, the layout
// V4L2 delivers) into the frame's planes. Returns a negative AVERROR when buf is
// too short.
static int pg_frame_fill(AVFrame *f, const uint8_t *buf, int len) {
	uint8_t *sd[4];
	int sl[4];
	int need = av_image_get_buffer_size(f->format, f->width, f->height, 1);
	if (need < 0) return need;
	if (len < need) return AVERROR(EINVAL);
	int r = av_image_fill_arrays(sd, sl, buf, f->format, f->width, f->height, 1);
	if (r < 0) return r;
	av_image_copy(f->data, f->linesize, (const uint8_t **)sd, sl, f->format, f->width, f->height);
	return 0;
}

static int pg_raw_size(int fmt, int w, int h) { return av_image_get_buffer_size(fmt, w, h, 1); }

// pg_set_rate updates the rate-control fields of an opened encoder. libavcodec's
// libx264 wrapper notices the change before the next frame and reconfigures
// x264 in place (no flush, no new GOP).
static void pg_set_rate(AVCodecContext *c, int64_t bitrate, int64_t maxrate, int bufsize) {
	c->bit_rate = bitrate;
	c->rc_max_rate = maxrate;
	c->rc_buffer_size = bufsize;
}

static int pg_ctx_width(AVCodecContext *c) { return c->width; }
static int pg_ctx_height(AVCodecContext *c) { return c->height; }
static int64_t pg_ctx_bitrate(AVCodecContext *c) { return c->bit_rate; }
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// avCtx returns the *C.AVCodecContext behind an astiav codec context.
//
// go-astiav v0.19.0 (the newest release that still compiles against FFmpeg 6.1
// headers; later releases need FFmpeg 7.0) has no setters for rc_max_rate /
// rc_buffer_size and no way to reach the underlying pointer. Its CodecContext
// struct starts with that pointer (`c *C.AVCodecContext`), so we read it from
// there. The layout is pinned by the go.mod version, and checkCtx verifies at
// runtime that the pointer we recovered really is this context.
func avCtx(cc *astiav.CodecContext) *C.AVCodecContext {
	return *(**C.AVCodecContext)(unsafe.Pointer(cc))
}

// checkCtx guards the avCtx layout assumption against an astiav upgrade.
func checkCtx(cc *astiav.CodecContext) error {
	c := avCtx(cc)
	if c == nil || int(C.pg_ctx_width(c)) != cc.Width() || int(C.pg_ctx_height(c)) != cc.Height() ||
		int64(C.pg_ctx_bitrate(c)) != cc.BitRate() {
		return errors.New("libav: go-astiav CodecContext layout changed; shim.go needs updating")
	}
	return nil
}

// setRate applies new rate-control values to an opened encoder between frames.
func setRate(cc *astiav.CodecContext, bitrate, maxrate, bufsize int) {
	C.pg_set_rate(avCtx(cc), C.int64_t(bitrate), C.int64_t(maxrate), C.int(bufsize))
}

func allocFrameBuffer(f *astiav.Frame) error {
	if r := C.pg_frame_get_buffer((*C.AVFrame)(f.UnsafePointer())); r < 0 {
		return fmt.Errorf("libav: allocate frame buffer: %w", astiav.Error(r))
	}
	return nil
}

func makeWritable(f *astiav.Frame) error {
	if r := C.pg_frame_make_writable((*C.AVFrame)(f.UnsafePointer())); r < 0 {
		return fmt.Errorf("libav: make frame writable: %w", astiav.Error(r))
	}
	return nil
}

// fillFrame copies a tightly packed raw image into f.
func fillFrame(f *astiav.Frame, raw []byte) error {
	if len(raw) == 0 {
		return errors.New("libav: empty frame")
	}
	if r := C.pg_frame_fill((*C.AVFrame)(f.UnsafePointer()), (*C.uint8_t)(unsafe.Pointer(&raw[0])), C.int(len(raw))); r < 0 {
		return fmt.Errorf("libav: copy raw frame (%d bytes): %w", len(raw), astiav.Error(r))
	}
	return nil
}
