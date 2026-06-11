//go:build linux && cgo

package v4l2

// This file verifies, against the real <linux/videodev2.h> UAPI header, that
// every hand-written struct in v4l2_linux.go has exactly the C layout (size
// and field offsets, including the implicit padding and union overlays), and
// that every ioctl request code and enum/flag constant matches the C macro
// expansion.
//
// Go forbids cgo inside _test.go files, so the C side lives in the cgo-only
// subpackage uapiref (imported by nothing but this test); the production v4l2
// package itself stays pure Go and keeps building with CGO_ENABLED=0. This
// test runs wherever cgo is available — e.g. the CI host job on the ubuntu
// runner, which tests with -race and therefore has cgo enabled.
//
// If this test fails, the pure-Go definitions in v4l2_linux.go are wrong for
// this architecture/header and must be fixed there — do not loosen the test.

import (
	"testing"
	"unsafe"

	"github.com/playgate/playgate-host/internal/capture/v4l2/uapiref"
)

// want fetches a reference value recorded by the uapiref cgo bridge, failing
// the test if the key is missing (which would mean silent loss of coverage).
func want[V uintptr | uint32](t *testing.T, m map[string]V, key string) V {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("uapiref has no reference value for %q", key)
	}
	return v
}

// TestStructSizesMatchUAPI compares unsafe.Sizeof of every hand-written Go
// struct against sizeof of the corresponding C struct from the kernel header.
func TestStructSizesMatchUAPI(t *testing.T) {
	cases := []struct {
		cName  string
		goSize uintptr
	}{
		{"v4l2_capability", unsafe.Sizeof(v4l2Capability{})},
		{"v4l2_fmtdesc", unsafe.Sizeof(v4l2Fmtdesc{})},
		{"v4l2_pix_format", unsafe.Sizeof(v4l2PixFormat{})},
		{"v4l2_format", unsafe.Sizeof(v4l2Format{})},
		{"v4l2_format.fmt", unsafe.Sizeof(v4l2Format{}.Raw)}, // union backing array
		{"v4l2_requestbuffers", unsafe.Sizeof(v4l2Requestbuffers{})},
		{"v4l2_buffer", unsafe.Sizeof(v4l2Buffer{})},
		{"timeval", unsafe.Sizeof(v4l2Timeval{})},
		{"v4l2_timecode", unsafe.Sizeof(v4l2Timecode{})},
		{"v4l2_frmsize_discrete", unsafe.Sizeof(v4l2FrmsizeDiscrete{})},
		{"v4l2_frmsize_stepwise", unsafe.Sizeof(v4l2FrmsizeStepwise{})},
		{"v4l2_frmsizeenum", unsafe.Sizeof(v4l2Frmsizeenum{})},
		{"v4l2_fract", unsafe.Sizeof(v4l2Fract{})},
		{"v4l2_captureparm", unsafe.Sizeof(v4l2CaptureParm{})},
		{"v4l2_streamparm", unsafe.Sizeof(v4l2StreamParm{})},
		{"v4l2_streamparm.parm", unsafe.Sizeof(v4l2StreamParm{}.Raw)}, // union backing array
		{"int", unsafe.Sizeof(int32(0))},                              // VIDIOC_STREAMON/OFF argument
	}
	for _, tc := range cases {
		if c := want(t, uapiref.Sizeof, tc.cName); tc.goSize != c {
			t.Errorf("sizeof mismatch for %s: Go %d, C %d", tc.cName, tc.goSize, c)
		}
	}
	// The frmsizeenum union backing array must hold its largest member.
	if got, wantSz := unsafe.Sizeof(v4l2Frmsizeenum{}.Union), unsafe.Sizeof(v4l2FrmsizeStepwise{}); got != wantSz {
		t.Errorf("v4l2Frmsizeenum.Union size = %d, want %d (sizeof stepwise, the largest union member)", got, wantSz)
	}
}

// TestFieldOffsetsMatchUAPI compares unsafe.Offsetof of the fields that sit
// near unions or implicit padding (plus a spread of ordinary fields) against
// C offsetof. A mismatch here means a silent one-byte-off corruption bug.
func TestFieldOffsetsMatchUAPI(t *testing.T) {
	var (
		cap     v4l2Capability
		fd      v4l2Fmtdesc
		pix     v4l2PixFormat
		format  v4l2Format
		req     v4l2Requestbuffers
		buf     v4l2Buffer
		tv      v4l2Timeval
		tcd     v4l2Timecode
		frm     v4l2Frmsizeenum
		sw      v4l2FrmsizeStepwise
		capParm v4l2CaptureParm
		sp      v4l2StreamParm
	)
	cases := []struct {
		cName string
		goOff uintptr
	}{
		{"v4l2_capability.card", unsafe.Offsetof(cap.Card)},
		{"v4l2_capability.bus_info", unsafe.Offsetof(cap.BusInfo)},
		{"v4l2_capability.version", unsafe.Offsetof(cap.Version)},
		{"v4l2_capability.capabilities", unsafe.Offsetof(cap.Capabilities)},
		{"v4l2_capability.device_caps", unsafe.Offsetof(cap.DeviceCaps)},
		{"v4l2_capability.reserved", unsafe.Offsetof(cap.Reserved)},

		{"v4l2_fmtdesc.flags", unsafe.Offsetof(fd.Flags)},
		{"v4l2_fmtdesc.description", unsafe.Offsetof(fd.Description)},
		{"v4l2_fmtdesc.pixelformat", unsafe.Offsetof(fd.PixelFormat)},
		{"v4l2_fmtdesc.mbus_code", unsafe.Offsetof(fd.MBusCode)},
		{"v4l2_fmtdesc.reserved", unsafe.Offsetof(fd.Reserved)},

		{"v4l2_pix_format.field", unsafe.Offsetof(pix.Field)},
		{"v4l2_pix_format.bytesperline", unsafe.Offsetof(pix.BytesPerLine)},
		{"v4l2_pix_format.sizeimage", unsafe.Offsetof(pix.SizeImage)},
		{"v4l2_pix_format.priv", unsafe.Offsetof(pix.Priv)},
		{"v4l2_pix_format.flags", unsafe.Offsetof(pix.Flags)},
		{"v4l2_pix_format.ycbcr_enc", unsafe.Offsetof(pix.Enc)}, // anonymous union member
		{"v4l2_pix_format.quantization", unsafe.Offsetof(pix.Quantization)},
		{"v4l2_pix_format.xfer_func", unsafe.Offsetof(pix.XferFunc)},

		// The critical padding check: u32 type, then 4 bytes of implicit
		// padding before the 8-byte-aligned fmt union on 64-bit.
		{"v4l2_format.fmt", unsafe.Offsetof(format.Raw)},

		{"v4l2_requestbuffers.memory", unsafe.Offsetof(req.Memory)},
		{"v4l2_requestbuffers.capabilities", unsafe.Offsetof(req.Capabilities)},
		{"v4l2_requestbuffers.flags", unsafe.Offsetof(req.Flags)},
		{"v4l2_requestbuffers.reserved", unsafe.Offsetof(req.Reserved)},

		// v4l2_buffer: timestamp follows implicit padding; m is a
		// pointer-sized union; request_fd lives in a trailing anonymous union.
		{"v4l2_buffer.field", unsafe.Offsetof(buf.Field)},
		{"v4l2_buffer.timestamp", unsafe.Offsetof(buf.Timestamp)},
		{"v4l2_buffer.timecode", unsafe.Offsetof(buf.Timecode)},
		{"v4l2_buffer.sequence", unsafe.Offsetof(buf.Sequence)},
		{"v4l2_buffer.memory", unsafe.Offsetof(buf.Memory)},
		{"v4l2_buffer.m", unsafe.Offsetof(buf.M)},
		{"v4l2_buffer.m.offset", unsafe.Offsetof(buf.M)}, // MOffset reads the union's low 4 bytes
		{"v4l2_buffer.length", unsafe.Offsetof(buf.Length)},
		{"v4l2_buffer.reserved2", unsafe.Offsetof(buf.Reserved2)},
		{"v4l2_buffer.request_fd", unsafe.Offsetof(buf.RequestFD)},

		{"timeval.tv_usec", unsafe.Offsetof(tv.Usec)},

		{"v4l2_timecode.frames", unsafe.Offsetof(tcd.Frames)},
		{"v4l2_timecode.userbits", unsafe.Offsetof(tcd.Userbits)},

		{"v4l2_frmsizeenum.type", unsafe.Offsetof(frm.Type)},
		{"v4l2_frmsizeenum.discrete", unsafe.Offsetof(frm.Union)}, // union overlay
		{"v4l2_frmsizeenum.stepwise", unsafe.Offsetof(frm.Union)}, // union overlay
		{"v4l2_frmsizeenum.reserved", unsafe.Offsetof(frm.Reserved)},

		{"v4l2_frmsize_stepwise.min_height", unsafe.Offsetof(sw.MinHeight)},

		{"v4l2_captureparm.timeperframe", unsafe.Offsetof(capParm.TimePerFrame)},
		{"v4l2_captureparm.extendedmode", unsafe.Offsetof(capParm.ExtendedMode)},
		{"v4l2_captureparm.readbuffers", unsafe.Offsetof(capParm.ReadBuffers)},
		{"v4l2_captureparm.reserved", unsafe.Offsetof(capParm.Reserved)},

		// Counterpart to v4l2_format: union at offset 4 with no padding,
		// because v4l2_streamparm's union only has 4-byte-aligned members.
		{"v4l2_streamparm.parm", unsafe.Offsetof(sp.Raw)},
	}
	for _, tc := range cases {
		if c := want(t, uapiref.Offsetof, tc.cName); tc.goOff != c {
			t.Errorf("offsetof mismatch for %s: Go %d, C %d", tc.cName, tc.goOff, c)
		}
	}
}

// TestIoctlRequestCodesMatchUAPI compares every hand-computed VIDIOC_* request
// code against the C macro expansion. Because the argument size is part of the
// _IOC encoding, this also re-checks each request struct's size through an
// independent path.
func TestIoctlRequestCodesMatchUAPI(t *testing.T) {
	cases := []struct {
		cName string
		goRq  uintptr
	}{
		{"VIDIOC_QUERYCAP", vidiocQueryCap},
		{"VIDIOC_ENUM_FMT", vidiocEnumFmt},
		{"VIDIOC_S_FMT", vidiocSFmt},
		{"VIDIOC_REQBUFS", vidiocReqbufs},
		{"VIDIOC_QUERYBUF", vidiocQueryBuf},
		{"VIDIOC_QBUF", vidiocQBuf},
		{"VIDIOC_DQBUF", vidiocDQBuf},
		{"VIDIOC_STREAMON", vidiocStreamOn},
		{"VIDIOC_STREAMOFF", vidiocStreamOff},
		{"VIDIOC_S_PARM", vidiocSParm},
		{"VIDIOC_ENUM_FRAMESIZES", vidiocEnumFramesizes},
	}
	for _, tc := range cases {
		if c := want(t, uapiref.Ioctl, tc.cName); tc.goRq != c {
			t.Errorf("ioctl request mismatch for %s: Go %#x, C %#x", tc.cName, tc.goRq, c)
		}
	}
}

// TestConstantsMatchUAPI compares every enum/flag/fourcc constant the package
// uses against the header values.
func TestConstantsMatchUAPI(t *testing.T) {
	cases := []struct {
		cName string
		goVal uint32
	}{
		{"V4L2_BUF_TYPE_VIDEO_CAPTURE", v4l2BufTypeVideoCapture},
		{"V4L2_MEMORY_MMAP", v4l2MemoryMMAP},
		{"V4L2_CAP_VIDEO_CAPTURE", v4l2CapVideoCapture},
		{"V4L2_CAP_STREAMING", v4l2CapStreaming},
		{"V4L2_FRMSIZE_TYPE_DISCRETE", v4l2FrmsizeTypeDiscrete},
		{"V4L2_FRMSIZE_TYPE_CONTINUOUS", v4l2FrmsizeTypeContinuous},
		{"V4L2_FRMSIZE_TYPE_STEPWISE", v4l2FrmsizeTypeStepwise},
		{"V4L2_FIELD_NONE", v4l2FieldNone},
		{"V4L2_CAP_TIMEPERFRAME", v4l2CapTimePerFrame},
		{"V4L2_PIX_FMT_YUYV", FourCCYUYV},
		{"V4L2_PIX_FMT_MJPEG", FourCCMJPEG},
	}
	for _, tc := range cases {
		if c := want(t, uapiref.Const, tc.cName); tc.goVal != c {
			t.Errorf("constant mismatch for %s: Go %#x, C %#x", tc.cName, tc.goVal, c)
		}
	}
}
