//go:build linux

package v4l2

// Pure-Go V4L2 (Video4Linux2) bindings built directly on golang.org/x/sys/unix.
//
// This file replaces the cgo-based go4vl dependency so the whole module can be
// cross-compiled from any platform (CGO_ENABLED=0). It implements just the slice
// of the V4L2 UAPI the capture source needs:
//
//   VIDIOC_QUERYCAP, VIDIOC_ENUM_FMT, VIDIOC_ENUM_FRAMESIZES, VIDIOC_S_FMT,
//   VIDIOC_REQBUFS, VIDIOC_QUERYBUF, VIDIOC_QBUF, VIDIOC_DQBUF,
//   VIDIOC_STREAMON, VIDIOC_STREAMOFF, plus VIDIOC_S_PARM for frame rate.
//
// Streaming uses memory-mapped (V4L2_MEMORY_MMAP) buffers.
//
// Struct layout: every struct below is laid out to match the 64-bit Linux UAPI
// definitions in <linux/videodev2.h>. Where the kernel uses a union we reserve
// a fixed-size byte array large enough for the largest member; where the kernel
// inserts implicit padding for alignment we add explicit padding fields. The
// ioctl request codes are computed with the same _IOR/_IOW/_IOWR encoding the
// kernel uses, so the binary contract is exact. See the per-struct comments for
// the byte offsets that matter.

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// --- ioctl request-code encoding (asm-generic/ioctl.h) ---------------------

const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14
	iocDirBits  = 2

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift)
}

func ior(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }
func iow(typ, nr, size uintptr) uintptr  { return ioc(iocWrite, typ, nr, size) }
func iowr(typ, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, typ, nr, size) }

// 'V' is the V4L2 ioctl type byte.
const vidiocType = uintptr('V')

// VIDIOC request codes. The sizes are computed from the matching structs so the
// encoding stays correct even if a struct grows.
var (
	vidiocQueryCap       = ior(vidiocType, 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt        = iowr(vidiocType, 2, unsafe.Sizeof(v4l2Fmtdesc{}))
	vidiocSFmt           = iowr(vidiocType, 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs        = iowr(vidiocType, 8, unsafe.Sizeof(v4l2Requestbuffers{}))
	vidiocQueryBuf       = iowr(vidiocType, 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocSParm          = iowr(vidiocType, 22, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocQBuf           = iowr(vidiocType, 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf          = iowr(vidiocType, 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn       = iow(vidiocType, 18, unsafe.Sizeof(int32(0)))
	vidiocStreamOff      = iow(vidiocType, 19, unsafe.Sizeof(int32(0)))
	vidiocEnumFramesizes = iowr(vidiocType, 74, unsafe.Sizeof(v4l2Frmsizeenum{}))
)

// --- V4L2 enum / flag constants (videodev2.h) ------------------------------

const (
	v4l2BufTypeVideoCapture = 1

	v4l2MemoryMMAP = 1

	v4l2CapVideoCapture = 0x00000001
	v4l2CapStreaming    = 0x04000000

	v4l2FrmsizeTypeDiscrete   = 1
	v4l2FrmsizeTypeContinuous = 2
	v4l2FrmsizeTypeStepwise   = 3

	v4l2FieldNone = 1
)

// --- structs (64-bit layout) -----------------------------------------------

// v4l2Capability matches struct v4l2_capability: 4+16+24 = 44 bytes of strings,
// then four u32. Total 104 bytes.
//
//	char driver[16]; char card[32]; char bus_info[32];
//	__u32 version; __u32 capabilities; __u32 device_caps; __u32 reserved[3];
type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

// v4l2Fmtdesc matches struct v4l2_fmtdesc:
//
//	__u32 index; __u32 type; __u32 flags; char description[32];
//	__u32 pixelformat; __u32 mbus_code; __u32 reserved[3];
//
// (mbus_code is part of a union with reserved in newer headers; we model the
// flat layout which is binary-identical.)
type v4l2Fmtdesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MBusCode    uint32
	Reserved    [3]uint32
}

// v4l2PixFormat matches struct v4l2_pix_format (single-planar):
//
//	__u32 width, height, pixelformat, field, bytesperline, sizeimage,
//	      colorspace, priv, flags; union {__u32 ycbcr_enc; __u32 hsv_enc;};
//	__u32 quantization, xfer_func;
//
// 13 x u32 = 52 bytes.
type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	Enc          uint32 // union ycbcr_enc / hsv_enc
	Quantization uint32
	XferFunc     uint32
}

// v4l2Format matches struct v4l2_format. The kernel struct is:
//
//	__u32 type;
//	union { v4l2_pix_format pix; v4l2_pix_format_mplane pix_mp;
//	        v4l2_window win; v4l2_vbi_format vbi; ... __u8 raw_data[200]; } fmt;
//
// The union is 200 bytes. After the u32 `type` the compiler inserts 4 bytes of
// padding so the union begins on an 8-byte boundary (the union contains 64-bit
// members in some arms). We model that with an explicit Pad field, then a fixed
// 200-byte backing array we overlay v4l2PixFormat onto.
type v4l2Format struct {
	Type uint32
	_    uint32 // padding to 8-byte align the union
	Raw  [200]byte
}

// pix returns a typed view of the format union as a single-planar pix_format.
func (f *v4l2Format) pix() *v4l2PixFormat {
	return (*v4l2PixFormat)(unsafe.Pointer(&f.Raw[0]))
}

// v4l2Requestbuffers matches struct v4l2_requestbuffers:
//
//	__u32 count, type, memory, capabilities; __u8 flags; __u8 reserved[3];
type v4l2Requestbuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

// v4l2Timeval mirrors the kernel's 64-bit struct timeval used inside
// v4l2_buffer (two long-sized fields on 64-bit).
type v4l2Timeval struct {
	Sec  int64
	Usec int64
}

// v4l2Buffer matches struct v4l2_buffer on 64-bit:
//
//	__u32 index; __u32 type; __u32 bytesused; __u32 flags; __u32 field;
//	struct timeval timestamp;          // 16 bytes, 8-byte aligned
//	struct v4l2_timecode timecode;     // 16 bytes
//	__u32 sequence; __u32 memory;
//	union { __u32 offset; unsigned long userptr; ...; } m;  // 8 bytes (ptr)
//	__u32 length; __u32 reserved2; union {__s32 request_fd; __u32 reserved;};
//
// The five leading u32 (20 bytes) are followed by timestamp which needs 8-byte
// alignment, so the compiler inserts 4 bytes of padding. We add an explicit Pad.
// The `m` union is pointer-sized (8 bytes on 64-bit); for MMAP we read m.offset
// from its low 4 bytes via the MOffset accessor.
type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	_         uint32 // pad to 8-byte align Timestamp
	Timestamp v4l2Timeval
	Timecode  v4l2Timecode
	Sequence  uint32
	Memory    uint32
	M         uint64 // union m: offset(u32) / userptr / planes ptr / fd
	Length    uint32
	Reserved2 uint32
	RequestFD int32 // union request_fd / reserved
}

// MOffset returns the mmap offset stored in the m union for MMAP buffers.
func (b *v4l2Buffer) MOffset() uint32 { return uint32(b.M) }

// v4l2Timecode matches struct v4l2_timecode: __u32 type, flags; __u8 frames,
// seconds, minutes, hours; __u8 userbits[4]. 16 bytes.
type v4l2Timecode struct {
	Type     uint32
	Flags    uint32
	Frames   uint8
	Seconds  uint8
	Minutes  uint8
	Hours    uint8
	Userbits [4]uint8
}

// v4l2FrmsizeDiscrete matches struct v4l2_frmsize_discrete: __u32 width, height.
type v4l2FrmsizeDiscrete struct {
	Width  uint32
	Height uint32
}

// v4l2FrmsizeStepwise matches struct v4l2_frmsize_stepwise: six u32.
type v4l2FrmsizeStepwise struct {
	MinWidth   uint32
	MaxWidth   uint32
	StepWidth  uint32
	MinHeight  uint32
	MaxHeight  uint32
	StepHeight uint32
}

// v4l2Frmsizeenum matches struct v4l2_frmsizeenum:
//
//	__u32 index; __u32 pixel_format; __u32 type;
//	union { v4l2_frmsize_discrete discrete;   // 8 bytes
//	        v4l2_frmsize_stepwise stepwise; } // 24 bytes
//	__u32 reserved[2];
//
// The union is 24 bytes; we overlay it on a fixed array.
type v4l2Frmsizeenum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	Union       [24]byte
	Reserved    [2]uint32
}

func (e *v4l2Frmsizeenum) discrete() *v4l2FrmsizeDiscrete {
	return (*v4l2FrmsizeDiscrete)(unsafe.Pointer(&e.Union[0]))
}

func (e *v4l2Frmsizeenum) stepwise() *v4l2FrmsizeStepwise {
	return (*v4l2FrmsizeStepwise)(unsafe.Pointer(&e.Union[0]))
}

// v4l2Fract matches struct v4l2_fract: __u32 numerator, denominator.
type v4l2Fract struct {
	Numerator   uint32
	Denominator uint32
}

// v4l2CaptureParm matches struct v4l2_captureparm:
//
//	__u32 capability, capturemode; struct v4l2_fract timeperframe;
//	__u32 extendedmode, readbuffers; __u32 reserved[4].
type v4l2CaptureParm struct {
	Capability   uint32
	CaptureMode  uint32
	TimePerFrame v4l2Fract
	ExtendedMode uint32
	ReadBuffers  uint32
	Reserved     [4]uint32
}

const v4l2CapTimePerFrame = 0x1000

// v4l2StreamParm matches struct v4l2_streamparm:
//
//	__u32 type; union { v4l2_captureparm capture; v4l2_outputparm output;
//	                    __u8 raw_data[200]; } parm;
//
// The union is 200 bytes and 4-byte aligned (only u32 members), so no extra
// padding is needed after the leading u32 type — but the union's largest member
// alignment is 4, and `type` is u32, so the union begins at offset 4. We model
// that directly (no pad).
type v4l2StreamParm struct {
	Type uint32
	Raw  [200]byte
}

func (p *v4l2StreamParm) capture() *v4l2CaptureParm {
	return (*v4l2CaptureParm)(unsafe.Pointer(&p.Raw[0]))
}

// --- ioctl helper ----------------------------------------------------------

// ioctl issues a V4L2 ioctl, retrying on EINTR. arg points to the request struct.
func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(arg))
		if errno == 0 {
			return nil
		}
		if errno == unix.EINTR {
			continue
		}
		return errno
	}
}

// cString trims a fixed C char array at its first NUL and returns a Go string.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// fmtError wraps an ioctl error with the operation name.
func fmtError(op string, err error) error {
	return fmt.Errorf("v4l2 %s: %w", op, err)
}
