// Package wasapi is the Windows audio source that reads the card in-process
// with WASAPI (github.com/virgil724/caplat/capture/wasapi) and encodes Opus
// in-process with libav, instead of an ffmpeg DirectShow subprocess.
// Measured on a GC553: ffmpeg + DirectShow hands audio over about 21.6 ms
// after it is played; WASAPI shared mode about 12 ms, exclusive mode (3 ms
// periods) a few ms sooner still.
package wasapi

// Chunker regroups interleaved float32 packets of any channel count into
// stereo blocks of exactly Frames frames: mono is duplicated, extra
// channels are dropped.
type Chunker struct {
	Frames int
	// OnBlock gets each full block, Frames*2 interleaved samples; the
	// slice is reused after it returns.
	OnBlock func(stereo []float32)

	buf []float32
}

// Push adds frames frames of interleaved samples with channels channels.
func (c *Chunker) Push(samples []float32, frames, channels int) {
	if channels <= 0 || frames <= 0 {
		return
	}
	for i := 0; i < frames; i++ {
		l := samples[i*channels]
		r := l
		if channels > 1 {
			r = samples[i*channels+1]
		}
		c.buf = append(c.buf, l, r)
		if len(c.buf) == c.Frames*2 {
			c.OnBlock(c.buf)
			c.buf = c.buf[:0]
		}
	}
}

// Reset drops a partial block (after a discontinuity or a restart).
func (c *Chunker) Reset() { c.buf = c.buf[:0] }
