package wasapi

import (
	"math/rand"
	"testing"
)

// Packets of odd sizes come out as exact, in-order stereo blocks.
func TestChunkerBlocks(t *testing.T) {
	for _, channels := range []int{1, 2, 6} {
		var got []float32
		blocks := 0
		c := Chunker{Frames: 480, OnBlock: func(b []float32) {
			if len(b) != 960 {
				t.Fatalf("block of %d samples", len(b))
			}
			blocks++
			got = append(got, b...)
		}}
		r := rand.New(rand.NewSource(1))
		frame := 0
		for frame < 480*7+100 {
			n := 1 + r.Intn(300)
			pk := make([]float32, n*channels)
			for i := 0; i < n; i++ {
				for ch := 0; ch < channels; ch++ {
					pk[i*channels+ch] = float32(frame+i) + float32(ch)/10
				}
			}
			c.Push(pk, n, channels)
			frame += n
		}
		if blocks != frame/480 {
			t.Errorf("%d channels: %d blocks for %d frames", channels, blocks, frame)
		}
		for i := 0; i < len(got)/2; i++ {
			l, rr := got[2*i], got[2*i+1]
			wantR := float32(i)
			if channels > 1 {
				wantR += 0.1
			}
			if l != float32(i) || rr != wantR {
				t.Fatalf("%d channels: frame %d = (%v, %v)", channels, i, l, rr)
			}
		}
	}
}

func TestChunkerReset(t *testing.T) {
	n := 0
	c := Chunker{Frames: 4, OnBlock: func([]float32) { n++ }}
	c.Push(make([]float32, 3*2), 3, 2)
	c.Reset()
	c.Push(make([]float32, 3*2), 3, 2)
	if n != 0 {
		t.Errorf("a block from %d frames after a reset", 3)
	}
}
