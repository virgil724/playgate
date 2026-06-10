package rtc

// minimalH264Keyframe returns a tiny Annex-B byte stream containing an SPS, PPS
// and an IDR-slice NAL. It is NOT a fully valid decodable picture; it exists only
// to exercise the RTP packetiser in the loopback test (the test asserts RTP flows,
// not that a real decoder renders it). The leading start codes ensure the H.264
// payloader splits it into NAL units.
func minimalH264Keyframe() []byte {
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	// nal_unit_type 7 = SPS, 8 = PPS, 5 = IDR slice (0x65 = forbidden_zero=0,
	// nal_ref_idc=3, type=5).
	sps := append(append([]byte{}, startCode...), 0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2)
	pps := append(append([]byte{}, startCode...), 0x68, 0xce, 0x38, 0x80)
	idr := append(append([]byte{}, startCode...), 0x65, 0x88, 0x84, 0x00, 0x10, 0xff)
	out := append([]byte{}, sps...)
	out = append(out, pps...)
	out = append(out, idr...)
	return out
}
