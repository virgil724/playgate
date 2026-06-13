//go:build linux

package v4l2

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

func TestCopyFrameDataNV12RepackIgnoresUnderreportedBytesUsed(t *testing.T) {
	s := &Source{
		log:          slog.New(slog.DiscardHandler),
		pixfmt:       core.PixelFormatNV12,
		width:        4,
		height:       2,
		bytesPerLine: 6,
	}
	src := []byte{
		1, 2, 3, 4, 99, 99,
		5, 6, 7, 8, 99, 99,
		9, 10, 11, 12, 99, 99,
	}

	got, ok := s.copyFrameData(src, 7)
	if !ok {
		t.Fatal("copyFrameData returned ok=false")
	}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if !bytes.Equal(got, want) {
		t.Fatalf("NV12 repack = %v, want %v", got, want)
	}
}

func TestCopyFrameDataYUYVRepackIgnoresPadding(t *testing.T) {
	s := &Source{
		log:          slog.New(slog.DiscardHandler),
		pixfmt:       core.PixelFormatYUYV,
		width:        2,
		height:       2,
		bytesPerLine: 6,
	}
	src := []byte{
		1, 2, 3, 4, 99, 99,
		5, 6, 7, 8, 99, 99,
	}

	got, ok := s.copyFrameData(src, 4)
	if !ok {
		t.Fatal("copyFrameData returned ok=false")
	}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(got, want) {
		t.Fatalf("YUYV repack = %v, want %v", got, want)
	}
}

func TestCopyFrameDataMJPEGUsesBytesUsed(t *testing.T) {
	s := &Source{
		log:    slog.New(slog.DiscardHandler),
		pixfmt: core.PixelFormatMJPEG,
	}
	src := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9, 55, 66}

	got, ok := s.copyFrameData(src, 7)
	if !ok {
		t.Fatal("copyFrameData returned ok=false")
	}
	want := src[:7]
	if !bytes.Equal(got, want) {
		t.Fatalf("MJPEG copy = %v, want %v", got, want)
	}
}
