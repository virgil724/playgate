package mf

import (
	"bytes"
	"testing"
)

// A padded picture is packed row by row; a tight one is copied as is.
func TestTightNV12(t *testing.T) {
	const w, h, pitch = 4, 2, 6
	src := []byte{
		1, 2, 3, 4, 0, 0, // Y row 0
		5, 6, 7, 8, 0, 0, // Y row 1
		9, 10, 11, 12, 0, 0, // UV row
	}
	got, err := TightNV12(src, pitch, w, h)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// The last row may end at width: no padding after it is required.
	if _, err := TightNV12(src[:len(src)-2], pitch, w, h); err != nil {
		t.Errorf("unpadded last row: %v", err)
	}
	same, _ := TightNV12(want, w, w, h)
	if !bytes.Equal(same, want) || &same[0] == &want[0] {
		t.Errorf("tight copy %v (must be a copy)", same)
	}
}

func TestTightNV12Rejects(t *testing.T) {
	for _, c := range []struct {
		n, pitch, w, h int
	}{
		{12, 4, 4, 3}, // odd height
		{12, 3, 4, 2}, // pitch below width
		{10, 4, 4, 2}, // too short
	} {
		if _, err := TightNV12(make([]byte, c.n), c.pitch, c.w, c.h); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
}
