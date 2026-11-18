package rtc

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"testing"
)

// Golden vectors shared with playgate-web/src/lib/webcodecs/fec.test.ts.
// data[i][b] = byte(i*37 + b*11 + 5), k=4, stride=8, m=2.
const (
	goldXORP0 = "4a4a7e564abad6ce"
	goldXORP1 = "5e4acadef6cadab6"
	goldRSP0  = "b84d6d2bb562d3ff"
	goldRSP1  = "8a99c8d08dfeb396"
)

func goldData() [][]byte {
	data := make([][]byte, 4)
	for i := range data {
		data[i] = make([]byte, 8)
		for b := range data[i] {
			data[i][b] = byte(i*37 + b*11 + 5)
		}
	}
	return data
}

func TestFECGoldenVectors(t *testing.T) {
	if gfMul(0x53, 0xca) != 143 || gfInv(0x53) != 140 || gfExp[8] != 0x1d || gfExp[254] != 142 {
		t.Fatalf("GF(256) tables differ from golden values")
	}
	if cauchy(4, 0, 0) != 71 || cauchy(4, 1, 3) != 122 {
		t.Fatalf("cauchy coefficients differ from golden values")
	}
	for _, c := range []struct {
		scheme int
		p0, p1 string
	}{{FECXOR, goldXORP0, goldXORP1}, {FECRS, goldRSP0, goldRSP1}} {
		par, err := fecEncode(c.scheme, goldData(), 2)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(par[0]) != c.p0 || hex.EncodeToString(par[1]) != c.p1 {
			t.Fatalf("scheme %d parity %x %x", c.scheme, par[0], par[1])
		}
	}
}

func randShards(rng *rand.Rand, k, sz int) [][]byte {
	d := make([][]byte, k)
	for i := range d {
		d[i] = make([]byte, sz)
		rng.Read(d[i])
	}
	return d
}

func TestRSRecoversUpToMLosses(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, km := range [][2]int{{2, 1}, {5, 2}, {10, 3}, {30, 6}, {200, 55}} {
		k, m := km[0], km[1]
		data := randShards(rng, k, 64)
		par, err := fecEncode(FECRS, data, m)
		if err != nil {
			t.Fatal(err)
		}
		for trial := 0; trial < 40; trial++ {
			shards := make([][]byte, k+m)
			for i := 0; i < k; i++ {
				shards[i] = data[i]
			}
			for j := 0; j < m; j++ {
				shards[k+j] = par[j]
			}
			lose := rng.Intn(m + 1)
			for _, idx := range rng.Perm(k + m)[:lose] {
				shards[idx] = nil
			}
			if _, err := fecRecover(FECRS, k, m, shards); err != nil {
				t.Fatalf("k=%d m=%d lose=%d: %v", k, m, lose, err)
			}
			for i := 0; i < k; i++ {
				if !bytes.Equal(shards[i], data[i]) {
					t.Fatalf("k=%d m=%d shard %d mismatch", k, m, i)
				}
			}
		}
	}
}

func TestRSTooManyLossesFailsCleanly(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	k, m := 8, 2
	data := randShards(rng, k, 16)
	par, _ := fecEncode(FECRS, data, m)
	shards := make([][]byte, k+m)
	copy(shards, data)
	copy(shards[k:], par)
	shards[0], shards[3], shards[5] = nil, nil, nil // 3 data lost > m
	if _, err := fecRecover(FECRS, k, m, shards); err != ErrFECUnrecoverable {
		t.Fatalf("want ErrFECUnrecoverable, got %v", err)
	}
	if shards[0] != nil || shards[3] != nil || shards[5] != nil {
		t.Fatal("shards modified on failure")
	}
	// m data lost but one parity also lost
	copy(shards, data)
	shards[1], shards[2], shards[k] = nil, nil, nil
	if _, err := fecRecover(FECRS, k, m, shards); err != ErrFECUnrecoverable {
		t.Fatalf("want ErrFECUnrecoverable, got %v", err)
	}
}

func TestXORGroupRecovery(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	k, m := 9, 3 // groups: {0,3,6} {1,4,7} {2,5,8}
	data := randShards(rng, k, 32)
	par, _ := fecEncode(FECXOR, data, m)
	build := func() [][]byte {
		s := make([][]byte, k+m)
		copy(s, data)
		copy(s[k:], par)
		return s
	}
	// One loss per group, including a burst of 3 consecutive fragments.
	s := build()
	s[3], s[4], s[5] = nil, nil, nil
	n, err := fecRecover(FECXOR, k, m, s)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for i := 0; i < k; i++ {
		if !bytes.Equal(s[i], data[i]) {
			t.Fatalf("shard %d mismatch", i)
		}
	}
	// Two losses in the same group fail.
	s = build()
	s[0], s[3] = nil, nil
	if _, err := fecRecover(FECXOR, k, m, s); err != ErrFECUnrecoverable {
		t.Fatalf("got %v", err)
	}
	// Lost parity for the affected group fails.
	s = build()
	s[1], s[k+1] = nil, nil
	if _, err := fecRecover(FECXOR, k, m, s); err != ErrFECUnrecoverable {
		t.Fatalf("got %v", err)
	}
}

func TestFECParityCount(t *testing.T) {
	cases := []struct {
		scheme, k int
		ratio     float64
		max, want int
	}{
		{FECRS, 1, 0.5, 255, 0},
		{FECRS, 10, 0, 255, 0},
		{FECRS, 10, 0.1, 255, 1},
		{FECRS, 10, 0.11, 255, 2},
		{FECRS, 3, 0.01, 255, 1},
		{FECRS, 100, 0.5, 8, 8},
		{FECRS, 100, 0.5, 0, 0},
		{FECRS, 250, 0.5, 255, 5},
		{FECRS, 255, 0.5, 255, 0},
		{FECXOR, 2, 1, 255, 2},
		{FECNone, 10, 0.5, 255, 0},
	}
	for _, c := range cases {
		if got := FECParityCount(c.scheme, c.k, c.ratio, c.max); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

// fecReassemble parses messages, drops the given fragment indices and returns the
// recovered access unit the way a receiver would (trim to frame_size).
func fecReassemble(t *testing.T, msgs [][]byte, drop map[int]bool) ([]byte, error) {
	t.Helper()
	var h0 VideoHeader
	shards := map[int][]byte{}
	for _, m := range msgs {
		h, p, err := ParseVideoHeader(m)
		if err != nil {
			t.Fatal(err)
		}
		h0 = h
		if !drop[int(h.FragIdx)] {
			shards[int(h.FragIdx)] = p
		}
	}
	k, par := int(h0.DataCount), int(h0.ParityCount)
	sl := make([][]byte, k+par)
	for i, p := range shards {
		sl[i] = p
	}
	// Data shards shorter than stride (the last) are zero-padded.
	stride := len(msgs[len(msgs)-1]) - VideoHeaderSize
	for i := 0; i < k; i++ {
		if sl[i] != nil && len(sl[i]) < stride {
			sl[i] = append(sl[i], make([]byte, stride-len(sl[i]))...)
		}
	}
	if _, err := fecRecover(int(h0.FECScheme), k, par, sl); err != nil {
		return nil, err
	}
	var out []byte
	for i := 0; i < k; i++ {
		out = append(out, sl[i]...)
	}
	return out[:h0.FrameSize], nil
}

func TestFragmentFrameFECLayout(t *testing.T) {
	au := make([]byte, 10*100+37) // k=11, last fragment short
	rand.New(rand.NewSource(4)).Read(au)
	meta := FrameMeta{Flags: FlagKey | FlagParam, Epoch: 2, FrameID: 9, PTSUs: 5, SendMS: 6}
	for _, scheme := range []int{FECXOR, FECRS} {
		msgs, err := FragmentFrameFEC(meta, au, 100, scheme, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 14 {
			t.Fatalf("len %d", len(msgs))
		}
		for i, m := range msgs {
			h, p, err := ParseVideoHeader(m)
			if err != nil {
				t.Fatal(err)
			}
			parity := i >= 11
			if (h.Flags&FlagParity != 0) != parity || int(h.FragIdx) != i ||
				h.DataCount != 11 || h.ParityCount != 3 || int(h.FECScheme) != scheme ||
				h.FrameSize != uint32(len(au)) || h.Flags&FlagKey == 0 {
				t.Fatalf("frag %d header %+v", i, h)
			}
			if parity && len(p) != 100 {
				t.Fatalf("parity %d len %d", i, len(p))
			}
			if i == 10 && len(p) != 37 {
				t.Fatalf("last data len %d", len(p))
			}
		}
		// Lose the short last fragment and (RS) two more, or (XOR) one per group.
		drop := map[int]bool{10: true, 0: true, 4: true}
		if scheme == FECXOR {
			drop = map[int]bool{10: true, 8: true, 9: true} // groups 1, 2, 0
		}
		got, err := fecReassemble(t, msgs, drop)
		if err != nil || !bytes.Equal(got, au) {
			t.Fatalf("scheme %d: err=%v equal=%v", scheme, err, bytes.Equal(got, au))
		}
		if _, err := fecReassemble(t, msgs, map[int]bool{0: true, 1: true, 2: true, 3: true}); err == nil {
			t.Fatalf("scheme %d: 4 losses with m=3 should fail", scheme)
		}
	}
	// k == 1 is never protected.
	if _, err := FragmentFrameFEC(meta, au[:50], 100, FECRS, 1); err == nil {
		t.Fatal("k=1 with parity should be rejected")
	}
	// m=0 is identical to FragmentFrame.
	a, _ := FragmentFrameFEC(meta, au, 100, FECRS, 0)
	b, _ := FragmentFrame(meta, au, 100)
	if len(a) != len(b) {
		t.Fatal("m=0 should equal no-FEC")
	}
}
