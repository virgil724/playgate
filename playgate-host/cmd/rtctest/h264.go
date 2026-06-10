package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// readAnnexB reads an entire Annex-B H.264 byte stream from path and splits it
// into access units (AUs). Each returned EncodedPacket is one AU (a run of NAL
// units up to, but not including, the start code of the next AU's first VCL NAL).
//
// We keep this deliberately simple: we split on access-unit boundaries detected
// by the first VCL NAL (slice types 1 and 5) following non-VCL NALs. For most
// ffmpeg-produced elementary streams (one slice per frame) this yields one AU per
// frame, which is exactly what the sample track wants.
func readAnnexB(path string, fps int) ([]core.EncodedPacket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read h264 file: %w", err)
	}
	nals := splitNALUs(data)
	if len(nals) == 0 {
		return nil, fmt.Errorf("no NAL units found in %q (is it raw Annex-B H.264?)", path)
	}

	frameDur := time.Second / time.Duration(maxInt(fps, 1))
	var (
		packets   []core.EncodedPacket
		cur       []byte
		curIsKey  bool
		seenVCL   bool
		pts       time.Duration
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		packets = append(packets, core.EncodedPacket{
			Data:       append([]byte(nil), cur...),
			PTS:        pts,
			IsKeyframe: curIsKey,
		})
		pts += frameDur
		cur = cur[:0]
		curIsKey = false
		seenVCL = false
	}

	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1f
		isVCL := nalType >= 1 && nalType <= 5
		// A new VCL NAL after we already have a VCL NAL in the current AU starts a
		// new access unit.
		if isVCL && seenVCL {
			flush()
		}
		// Re-prepend the 4-byte start code so the AU stays valid Annex-B.
		cur = append(cur, 0x00, 0x00, 0x00, 0x01)
		cur = append(cur, nal...)
		if isVCL {
			seenVCL = true
			if nalType == 5 { // IDR slice
				curIsKey = true
			}
		}
	}
	flush()

	if len(packets) == 0 {
		return nil, fmt.Errorf("no access units assembled from %q", path)
	}
	return packets, nil
}

// splitNALUs splits an Annex-B stream into raw NAL unit payloads (start codes
// stripped). It accepts both 3-byte (00 00 01) and 4-byte (00 00 00 01) start
// codes.
func splitNALUs(data []byte) [][]byte {
	var nals [][]byte
	r := bufio.NewReader(bytes.NewReader(data))
	full, err := io.ReadAll(r)
	if err != nil {
		return nil
	}
	// Find every start-code offset.
	var starts []int
	for i := 0; i+3 <= len(full); i++ {
		if full[i] == 0 && full[i+1] == 0 && full[i+2] == 1 {
			starts = append(starts, i+3)
		}
	}
	for idx, s := range starts {
		end := len(full)
		if idx+1 < len(starts) {
			// next start payload begins at starts[idx+1]; back up over its start code
			// (3 or 4 bytes) to find where this NAL ends.
			next := starts[idx+1]
			end = next - 3
			if end-1 >= 0 && full[end-1] == 0 { // 4-byte start code
				end--
			}
		}
		if end > s {
			nals = append(nals, full[s:end])
		}
	}
	return nals
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
