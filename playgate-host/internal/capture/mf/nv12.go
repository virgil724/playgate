// Package mf is the Windows capture source that reads the card in-process
// with Media Foundation (github.com/virgil724/caplat/capture/mf) instead of
// an ffmpeg DirectShow subprocess: measured on a GC553, 18 ms from the HDMI
// frame's vblank instead of 35 ms, and without DirectShow/ffmpeg's random
// extra queued frame.
package mf

import "fmt"

// TightNV12 copies an NV12 picture whose rows are pitch bytes apart (pitch
// >= width) into a new tightly packed buffer: height rows of Y, then
// height/2 rows of interleaved UV.
func TightNV12(src []byte, pitch, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("mf capture: bad NV12 size %dx%d", width, height)
	}
	if pitch < width {
		return nil, fmt.Errorf("mf capture: pitch %d below width %d", pitch, width)
	}
	rows := height * 3 / 2
	if len(src) < pitch*(rows-1)+width {
		return nil, fmt.Errorf("mf capture: NV12 buffer of %d bytes too short for %dx%d (pitch %d)", len(src), width, height, pitch)
	}
	dst := make([]byte, width*rows)
	if pitch == width {
		copy(dst, src[:width*rows])
		return dst, nil
	}
	for r := 0; r < rows; r++ {
		copy(dst[r*width:(r+1)*width], src[r*pitch:r*pitch+width])
	}
	return dst, nil
}
