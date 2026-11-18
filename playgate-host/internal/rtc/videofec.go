package rtc

import (
	"errors"
	"fmt"
)

// Application-level FEC for the WebCodecs transport
// (docs/webcodecs-transport.md section 8).
//
// Every shard is `stride` bytes. Data shard i is the i-th stride-sized slice of
// the access unit; the last one is zero-padded to stride for the computation
// only. Parity shard j travels in fragment k+j.

// FEC schemes (header fec_scheme).
const (
	FECNone = 0
	FECXOR  = 1
	FECRS   = 2

	// MaxFECShards is the GF(256) limit on k+m.
	MaxFECShards = 255
)

// GF(256), primitive polynomial 0x11d, generator 2.
var (
	gfExp [512]byte
	gfLog [256]int
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

func gfInv(a byte) byte { return gfExp[255-gfLog[a]] }

// cauchy returns the RS coefficient of parity row j for data column i:
// 1/(x_j ^ y_i) with x_j = k+j and y_i = i (all distinct while k+m <= 256).
func cauchy(k, j, i int) byte { return gfInv(byte(k+j) ^ byte(i)) }

// mulAdd computes dst ^= c*src.
func mulAdd(dst, src []byte, c byte) {
	switch c {
	case 0:
		return
	case 1:
		for i, v := range src {
			dst[i] ^= v
		}
		return
	}
	lc := gfLog[c]
	for i, v := range src {
		if v != 0 {
			dst[i] ^= gfExp[lc+gfLog[v]]
		}
	}
}

func scale(b []byte, c byte) {
	for i, v := range b {
		b[i] = gfMul(v, c)
	}
}

// FECParityCount returns m for k data fragments at the given ratio:
// ceil(k*ratio), at least 1 when ratio > 0, capped by maxParity (0 = no
// parity), by GF(256) (k+m <= 255) and, for XOR, by k. It returns 0 when FEC
// does not apply (k < 2, ratio <= 0, or k too large).
func FECParityCount(scheme, k int, ratio float64, maxParity int) int {
	if scheme != FECXOR && scheme != FECRS {
		return 0
	}
	if k < 2 || ratio <= 0 || maxParity <= 0 {
		return 0
	}
	m := int(float64(k)*ratio + 0.999999999)
	if m < 1 {
		m = 1
	}
	if m > maxParity {
		m = maxParity
	}
	if lim := MaxFECShards - k; m > lim {
		m = lim
	}
	if scheme == FECXOR && m > k {
		m = k
	}
	if m < 0 {
		m = 0
	}
	return m
}

// fecEncode returns m parity shards for the k data shards (all equal length).
func fecEncode(scheme int, data [][]byte, m int) ([][]byte, error) {
	k := len(data)
	if k == 0 || m <= 0 {
		return nil, errors.New("rtc: fec needs k>0 and m>0")
	}
	if k+m > MaxFECShards {
		return nil, fmt.Errorf("rtc: fec k+m=%d exceeds %d", k+m, MaxFECShards)
	}
	sz := len(data[0])
	par := make([][]byte, m)
	for j := range par {
		par[j] = make([]byte, sz)
	}
	switch scheme {
	case FECXOR:
		if m > k {
			return nil, errors.New("rtc: xor fec needs m <= k")
		}
		for i, d := range data {
			mulAdd(par[i%m], d, 1)
		}
	case FECRS:
		for j := 0; j < m; j++ {
			for i, d := range data {
				mulAdd(par[j], d, cauchy(k, j, i))
			}
		}
	default:
		return nil, fmt.Errorf("rtc: unknown fec scheme %d", scheme)
	}
	return par, nil
}

// ErrFECUnrecoverable is returned when too many shards are missing.
var ErrFECUnrecoverable = errors.New("rtc: fec: not enough shards to recover")

// fecRecover fills the nil entries among shards[:k] (data) in place using the
// present data and parity shards (shards has k+m entries, nil = missing,
// all non-nil entries have the same length). It returns the number of data
// shards recovered, or ErrFECUnrecoverable (shards untouched) if impossible.
func fecRecover(scheme, k, m int, shards [][]byte) (int, error) {
	if len(shards) != k+m {
		return 0, errors.New("rtc: fec shard count mismatch")
	}
	var missing []int
	for i := 0; i < k; i++ {
		if shards[i] == nil {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	sz := 0
	for _, s := range shards {
		if s != nil {
			sz = len(s)
			break
		}
	}
	if sz == 0 {
		return 0, ErrFECUnrecoverable
	}
	switch scheme {
	case FECXOR:
		if m < 1 || m > k {
			return 0, ErrFECUnrecoverable
		}
		// Groups are i mod m; each group tolerates one missing data shard.
		perGroup := make(map[int]int)
		for _, i := range missing {
			perGroup[i%m]++
		}
		for g, c := range perGroup {
			if c > 1 || shards[k+g] == nil {
				return 0, ErrFECUnrecoverable
			}
		}
		for _, i := range missing {
			g := i % m
			out := make([]byte, sz)
			copy(out, shards[k+g])
			for d := g; d < k; d += m {
				if d != i {
					mulAdd(out, shards[d], 1)
				}
			}
			shards[i] = out
		}
		return len(missing), nil
	case FECRS:
		e := len(missing)
		var rows []int // present parity rows used
		for j := 0; j < m && len(rows) < e; j++ {
			if shards[k+j] != nil {
				rows = append(rows, j)
			}
		}
		if len(rows) < e {
			return 0, ErrFECUnrecoverable
		}
		// rhs_r = parity_r - sum over known data of C[r][i]*d_i
		rhs := make([][]byte, e)
		for r, j := range rows {
			v := make([]byte, sz)
			copy(v, shards[k+j])
			for i := 0; i < k; i++ {
				if shards[i] != nil {
					mulAdd(v, shards[i], cauchy(k, j, i))
				}
			}
			rhs[r] = v
		}
		// Solve A*x = rhs with A[r][c] = C[rows[r]][missing[c]] (Gauss-Jordan).
		a := make([][]byte, e)
		for r := range a {
			a[r] = make([]byte, e)
			for c := range a[r] {
				a[r][c] = cauchy(k, rows[r], missing[c])
			}
		}
		for col := 0; col < e; col++ {
			p := -1
			for r := col; r < e; r++ {
				if a[r][col] != 0 {
					p = r
					break
				}
			}
			if p < 0 {
				return 0, ErrFECUnrecoverable // cannot happen for Cauchy
			}
			a[col], a[p] = a[p], a[col]
			rhs[col], rhs[p] = rhs[p], rhs[col]
			inv := gfInv(a[col][col])
			for c := 0; c < e; c++ {
				a[col][c] = gfMul(a[col][c], inv)
			}
			scale(rhs[col], inv)
			for r := 0; r < e; r++ {
				if r == col || a[r][col] == 0 {
					continue
				}
				f := a[r][col]
				for c := 0; c < e; c++ {
					a[r][c] ^= gfMul(f, a[col][c])
				}
				mulAdd(rhs[r], rhs[col], f)
			}
		}
		for c, i := range missing {
			shards[i] = rhs[c]
		}
		return e, nil
	}
	return 0, fmt.Errorf("rtc: unknown fec scheme %d", scheme)
}

// ParseFECScheme maps the config string ("off", "xor", "rs") to a scheme.
func ParseFECScheme(s string) int {
	switch s {
	case "xor":
		return FECXOR
	case "rs":
		return FECRS
	}
	return FECNone
}
