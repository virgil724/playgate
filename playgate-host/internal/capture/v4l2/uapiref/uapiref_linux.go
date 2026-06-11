//go:build linux && cgo

package uapiref

/*
#include <stddef.h>
#include <sys/time.h>
#include <linux/videodev2.h>

// ioctl request codes are > INT_MAX (dir bits live in the top two bits), so
// they cannot live in the enum below; expose them through static functions,
// which cgo links reliably (file-scope consts in the preamble do not).
static unsigned long c_VIDIOC_QUERYCAP(void)        { return VIDIOC_QUERYCAP; }
static unsigned long c_VIDIOC_ENUM_FMT(void)        { return VIDIOC_ENUM_FMT; }
static unsigned long c_VIDIOC_S_FMT(void)           { return VIDIOC_S_FMT; }
static unsigned long c_VIDIOC_REQBUFS(void)         { return VIDIOC_REQBUFS; }
static unsigned long c_VIDIOC_QUERYBUF(void)        { return VIDIOC_QUERYBUF; }
static unsigned long c_VIDIOC_QBUF(void)            { return VIDIOC_QBUF; }
static unsigned long c_VIDIOC_DQBUF(void)           { return VIDIOC_DQBUF; }
static unsigned long c_VIDIOC_STREAMON(void)        { return VIDIOC_STREAMON; }
static unsigned long c_VIDIOC_STREAMOFF(void)       { return VIDIOC_STREAMOFF; }
static unsigned long c_VIDIOC_S_PARM(void)          { return VIDIOC_S_PARM; }
static unsigned long c_VIDIOC_ENUM_FRAMESIZES(void) { return VIDIOC_ENUM_FRAMESIZES; }

// Everything else fits in int, so an enum keeps cgo access simple and constant.
enum {
	// sizeof
	c_sizeof_v4l2_capability       = sizeof(struct v4l2_capability),
	c_sizeof_v4l2_fmtdesc          = sizeof(struct v4l2_fmtdesc),
	c_sizeof_v4l2_pix_format       = sizeof(struct v4l2_pix_format),
	c_sizeof_v4l2_format           = sizeof(struct v4l2_format),
	c_sizeof_v4l2_format_union     = sizeof(((struct v4l2_format *)0)->fmt),
	c_sizeof_v4l2_requestbuffers   = sizeof(struct v4l2_requestbuffers),
	c_sizeof_v4l2_buffer           = sizeof(struct v4l2_buffer),
	c_sizeof_timeval               = sizeof(struct timeval),
	c_sizeof_v4l2_timecode         = sizeof(struct v4l2_timecode),
	c_sizeof_v4l2_frmsize_discrete = sizeof(struct v4l2_frmsize_discrete),
	c_sizeof_v4l2_frmsize_stepwise = sizeof(struct v4l2_frmsize_stepwise),
	c_sizeof_v4l2_frmsizeenum      = sizeof(struct v4l2_frmsizeenum),
	c_sizeof_v4l2_fract            = sizeof(struct v4l2_fract),
	c_sizeof_v4l2_captureparm      = sizeof(struct v4l2_captureparm),
	c_sizeof_v4l2_streamparm       = sizeof(struct v4l2_streamparm),
	c_sizeof_v4l2_streamparm_union = sizeof(((struct v4l2_streamparm *)0)->parm),
	c_sizeof_int                   = sizeof(int), // VIDIOC_STREAMON/OFF argument

	// offsetof: struct v4l2_capability
	c_off_capability_card         = offsetof(struct v4l2_capability, card),
	c_off_capability_bus_info     = offsetof(struct v4l2_capability, bus_info),
	c_off_capability_version      = offsetof(struct v4l2_capability, version),
	c_off_capability_capabilities = offsetof(struct v4l2_capability, capabilities),
	c_off_capability_device_caps  = offsetof(struct v4l2_capability, device_caps),
	c_off_capability_reserved     = offsetof(struct v4l2_capability, reserved),

	// offsetof: struct v4l2_fmtdesc
	c_off_fmtdesc_flags       = offsetof(struct v4l2_fmtdesc, flags),
	c_off_fmtdesc_description = offsetof(struct v4l2_fmtdesc, description),
	c_off_fmtdesc_pixelformat = offsetof(struct v4l2_fmtdesc, pixelformat),
	c_off_fmtdesc_mbus_code   = offsetof(struct v4l2_fmtdesc, mbus_code),
	c_off_fmtdesc_reserved    = offsetof(struct v4l2_fmtdesc, reserved),

	// offsetof: struct v4l2_pix_format (ycbcr_enc sits in an anonymous union)
	c_off_pix_field        = offsetof(struct v4l2_pix_format, field),
	c_off_pix_bytesperline = offsetof(struct v4l2_pix_format, bytesperline),
	c_off_pix_sizeimage    = offsetof(struct v4l2_pix_format, sizeimage),
	c_off_pix_priv         = offsetof(struct v4l2_pix_format, priv),
	c_off_pix_flags        = offsetof(struct v4l2_pix_format, flags),
	c_off_pix_ycbcr_enc    = offsetof(struct v4l2_pix_format, ycbcr_enc),
	c_off_pix_quantization = offsetof(struct v4l2_pix_format, quantization),
	c_off_pix_xfer_func    = offsetof(struct v4l2_pix_format, xfer_func),

	// offsetof: struct v4l2_format — the fmt union offset is the critical
	// padding check (u32 type, then 4 bytes of implicit padding on 64-bit).
	c_off_format_fmt = offsetof(struct v4l2_format, fmt),

	// offsetof: struct v4l2_requestbuffers
	c_off_reqbufs_memory       = offsetof(struct v4l2_requestbuffers, memory),
	c_off_reqbufs_capabilities = offsetof(struct v4l2_requestbuffers, capabilities),
	c_off_reqbufs_flags        = offsetof(struct v4l2_requestbuffers, flags),
	c_off_reqbufs_reserved     = offsetof(struct v4l2_requestbuffers, reserved),

	// offsetof: struct v4l2_buffer — timestamp follows implicit padding; m is
	// a pointer-sized union; request_fd lives in a trailing anonymous union.
	c_off_buffer_field      = offsetof(struct v4l2_buffer, field),
	c_off_buffer_timestamp  = offsetof(struct v4l2_buffer, timestamp),
	c_off_buffer_timecode   = offsetof(struct v4l2_buffer, timecode),
	c_off_buffer_sequence   = offsetof(struct v4l2_buffer, sequence),
	c_off_buffer_memory     = offsetof(struct v4l2_buffer, memory),
	c_off_buffer_m          = offsetof(struct v4l2_buffer, m),
	c_off_buffer_m_offset   = offsetof(struct v4l2_buffer, m.offset),
	c_off_buffer_length     = offsetof(struct v4l2_buffer, length),
	c_off_buffer_reserved2  = offsetof(struct v4l2_buffer, reserved2),
	c_off_buffer_request_fd = offsetof(struct v4l2_buffer, request_fd),

	// offsetof: struct timeval (inside v4l2_buffer.timestamp)
	c_off_timeval_usec = offsetof(struct timeval, tv_usec),

	// offsetof: struct v4l2_timecode
	c_off_timecode_frames   = offsetof(struct v4l2_timecode, frames),
	c_off_timecode_userbits = offsetof(struct v4l2_timecode, userbits),

	// offsetof: struct v4l2_frmsizeenum (anonymous discrete/stepwise union)
	c_off_frmsize_type     = offsetof(struct v4l2_frmsizeenum, type),
	c_off_frmsize_discrete = offsetof(struct v4l2_frmsizeenum, discrete),
	c_off_frmsize_stepwise = offsetof(struct v4l2_frmsizeenum, stepwise),
	c_off_frmsize_reserved = offsetof(struct v4l2_frmsizeenum, reserved),

	// offsetof: struct v4l2_frmsize_stepwise
	c_off_stepwise_min_height = offsetof(struct v4l2_frmsize_stepwise, min_height),

	// offsetof: struct v4l2_captureparm
	c_off_captureparm_timeperframe = offsetof(struct v4l2_captureparm, timeperframe),
	c_off_captureparm_extendedmode = offsetof(struct v4l2_captureparm, extendedmode),
	c_off_captureparm_readbuffers  = offsetof(struct v4l2_captureparm, readbuffers),
	c_off_captureparm_reserved     = offsetof(struct v4l2_captureparm, reserved),

	// offsetof: struct v4l2_streamparm — union at offset 4, no padding (the
	// inverse of v4l2_format, so check it explicitly too).
	c_off_streamparm_parm = offsetof(struct v4l2_streamparm, parm),

	// enum / flag / fourcc constants
	c_V4L2_BUF_TYPE_VIDEO_CAPTURE  = V4L2_BUF_TYPE_VIDEO_CAPTURE,
	c_V4L2_MEMORY_MMAP             = V4L2_MEMORY_MMAP,
	c_V4L2_CAP_VIDEO_CAPTURE       = V4L2_CAP_VIDEO_CAPTURE,
	c_V4L2_CAP_STREAMING           = V4L2_CAP_STREAMING,
	c_V4L2_FRMSIZE_TYPE_DISCRETE   = V4L2_FRMSIZE_TYPE_DISCRETE,
	c_V4L2_FRMSIZE_TYPE_CONTINUOUS = V4L2_FRMSIZE_TYPE_CONTINUOUS,
	c_V4L2_FRMSIZE_TYPE_STEPWISE   = V4L2_FRMSIZE_TYPE_STEPWISE,
	c_V4L2_FIELD_NONE              = V4L2_FIELD_NONE,
	c_V4L2_CAP_TIMEPERFRAME        = V4L2_CAP_TIMEPERFRAME,
	c_V4L2_PIX_FMT_YUYV            = V4L2_PIX_FMT_YUYV,
	c_V4L2_PIX_FMT_MJPEG           = V4L2_PIX_FMT_MJPEG,
};
*/
import "C"

// Sizeof maps a C struct name to its sizeof on this architecture/header.
var Sizeof = map[string]uintptr{
	"v4l2_capability":       uintptr(C.c_sizeof_v4l2_capability),
	"v4l2_fmtdesc":          uintptr(C.c_sizeof_v4l2_fmtdesc),
	"v4l2_pix_format":       uintptr(C.c_sizeof_v4l2_pix_format),
	"v4l2_format":           uintptr(C.c_sizeof_v4l2_format),
	"v4l2_format.fmt":       uintptr(C.c_sizeof_v4l2_format_union),
	"v4l2_requestbuffers":   uintptr(C.c_sizeof_v4l2_requestbuffers),
	"v4l2_buffer":           uintptr(C.c_sizeof_v4l2_buffer),
	"timeval":               uintptr(C.c_sizeof_timeval),
	"v4l2_timecode":         uintptr(C.c_sizeof_v4l2_timecode),
	"v4l2_frmsize_discrete": uintptr(C.c_sizeof_v4l2_frmsize_discrete),
	"v4l2_frmsize_stepwise": uintptr(C.c_sizeof_v4l2_frmsize_stepwise),
	"v4l2_frmsizeenum":      uintptr(C.c_sizeof_v4l2_frmsizeenum),
	"v4l2_fract":            uintptr(C.c_sizeof_v4l2_fract),
	"v4l2_captureparm":      uintptr(C.c_sizeof_v4l2_captureparm),
	"v4l2_streamparm":       uintptr(C.c_sizeof_v4l2_streamparm),
	"v4l2_streamparm.parm":  uintptr(C.c_sizeof_v4l2_streamparm_union),
	"int":                   uintptr(C.c_sizeof_int),
}

// Offsetof maps "struct.field" to its offsetof on this architecture/header.
var Offsetof = map[string]uintptr{
	"v4l2_capability.card":         uintptr(C.c_off_capability_card),
	"v4l2_capability.bus_info":     uintptr(C.c_off_capability_bus_info),
	"v4l2_capability.version":      uintptr(C.c_off_capability_version),
	"v4l2_capability.capabilities": uintptr(C.c_off_capability_capabilities),
	"v4l2_capability.device_caps":  uintptr(C.c_off_capability_device_caps),
	"v4l2_capability.reserved":     uintptr(C.c_off_capability_reserved),

	"v4l2_fmtdesc.flags":       uintptr(C.c_off_fmtdesc_flags),
	"v4l2_fmtdesc.description": uintptr(C.c_off_fmtdesc_description),
	"v4l2_fmtdesc.pixelformat": uintptr(C.c_off_fmtdesc_pixelformat),
	"v4l2_fmtdesc.mbus_code":   uintptr(C.c_off_fmtdesc_mbus_code),
	"v4l2_fmtdesc.reserved":    uintptr(C.c_off_fmtdesc_reserved),

	"v4l2_pix_format.field":        uintptr(C.c_off_pix_field),
	"v4l2_pix_format.bytesperline": uintptr(C.c_off_pix_bytesperline),
	"v4l2_pix_format.sizeimage":    uintptr(C.c_off_pix_sizeimage),
	"v4l2_pix_format.priv":         uintptr(C.c_off_pix_priv),
	"v4l2_pix_format.flags":        uintptr(C.c_off_pix_flags),
	"v4l2_pix_format.ycbcr_enc":    uintptr(C.c_off_pix_ycbcr_enc),
	"v4l2_pix_format.quantization": uintptr(C.c_off_pix_quantization),
	"v4l2_pix_format.xfer_func":    uintptr(C.c_off_pix_xfer_func),

	"v4l2_format.fmt": uintptr(C.c_off_format_fmt),

	"v4l2_requestbuffers.memory":       uintptr(C.c_off_reqbufs_memory),
	"v4l2_requestbuffers.capabilities": uintptr(C.c_off_reqbufs_capabilities),
	"v4l2_requestbuffers.flags":        uintptr(C.c_off_reqbufs_flags),
	"v4l2_requestbuffers.reserved":     uintptr(C.c_off_reqbufs_reserved),

	"v4l2_buffer.field":      uintptr(C.c_off_buffer_field),
	"v4l2_buffer.timestamp":  uintptr(C.c_off_buffer_timestamp),
	"v4l2_buffer.timecode":   uintptr(C.c_off_buffer_timecode),
	"v4l2_buffer.sequence":   uintptr(C.c_off_buffer_sequence),
	"v4l2_buffer.memory":     uintptr(C.c_off_buffer_memory),
	"v4l2_buffer.m":          uintptr(C.c_off_buffer_m),
	"v4l2_buffer.m.offset":   uintptr(C.c_off_buffer_m_offset),
	"v4l2_buffer.length":     uintptr(C.c_off_buffer_length),
	"v4l2_buffer.reserved2":  uintptr(C.c_off_buffer_reserved2),
	"v4l2_buffer.request_fd": uintptr(C.c_off_buffer_request_fd),

	"timeval.tv_usec": uintptr(C.c_off_timeval_usec),

	"v4l2_timecode.frames":   uintptr(C.c_off_timecode_frames),
	"v4l2_timecode.userbits": uintptr(C.c_off_timecode_userbits),

	"v4l2_frmsizeenum.type":     uintptr(C.c_off_frmsize_type),
	"v4l2_frmsizeenum.discrete": uintptr(C.c_off_frmsize_discrete),
	"v4l2_frmsizeenum.stepwise": uintptr(C.c_off_frmsize_stepwise),
	"v4l2_frmsizeenum.reserved": uintptr(C.c_off_frmsize_reserved),

	"v4l2_frmsize_stepwise.min_height": uintptr(C.c_off_stepwise_min_height),

	"v4l2_captureparm.timeperframe": uintptr(C.c_off_captureparm_timeperframe),
	"v4l2_captureparm.extendedmode": uintptr(C.c_off_captureparm_extendedmode),
	"v4l2_captureparm.readbuffers":  uintptr(C.c_off_captureparm_readbuffers),
	"v4l2_captureparm.reserved":     uintptr(C.c_off_captureparm_reserved),

	"v4l2_streamparm.parm": uintptr(C.c_off_streamparm_parm),
}

// Ioctl maps a VIDIOC_* macro name to its expanded request code.
var Ioctl = map[string]uintptr{
	"VIDIOC_QUERYCAP":        uintptr(C.c_VIDIOC_QUERYCAP()),
	"VIDIOC_ENUM_FMT":        uintptr(C.c_VIDIOC_ENUM_FMT()),
	"VIDIOC_S_FMT":           uintptr(C.c_VIDIOC_S_FMT()),
	"VIDIOC_REQBUFS":         uintptr(C.c_VIDIOC_REQBUFS()),
	"VIDIOC_QUERYBUF":        uintptr(C.c_VIDIOC_QUERYBUF()),
	"VIDIOC_QBUF":            uintptr(C.c_VIDIOC_QBUF()),
	"VIDIOC_DQBUF":           uintptr(C.c_VIDIOC_DQBUF()),
	"VIDIOC_STREAMON":        uintptr(C.c_VIDIOC_STREAMON()),
	"VIDIOC_STREAMOFF":       uintptr(C.c_VIDIOC_STREAMOFF()),
	"VIDIOC_S_PARM":          uintptr(C.c_VIDIOC_S_PARM()),
	"VIDIOC_ENUM_FRAMESIZES": uintptr(C.c_VIDIOC_ENUM_FRAMESIZES()),
}

// Const maps an enum/flag/fourcc macro name to its value.
var Const = map[string]uint32{
	"V4L2_BUF_TYPE_VIDEO_CAPTURE":  uint32(C.c_V4L2_BUF_TYPE_VIDEO_CAPTURE),
	"V4L2_MEMORY_MMAP":             uint32(C.c_V4L2_MEMORY_MMAP),
	"V4L2_CAP_VIDEO_CAPTURE":       uint32(C.c_V4L2_CAP_VIDEO_CAPTURE),
	"V4L2_CAP_STREAMING":           uint32(C.c_V4L2_CAP_STREAMING),
	"V4L2_FRMSIZE_TYPE_DISCRETE":   uint32(C.c_V4L2_FRMSIZE_TYPE_DISCRETE),
	"V4L2_FRMSIZE_TYPE_CONTINUOUS": uint32(C.c_V4L2_FRMSIZE_TYPE_CONTINUOUS),
	"V4L2_FRMSIZE_TYPE_STEPWISE":   uint32(C.c_V4L2_FRMSIZE_TYPE_STEPWISE),
	"V4L2_FIELD_NONE":              uint32(C.c_V4L2_FIELD_NONE),
	"V4L2_CAP_TIMEPERFRAME":        uint32(C.c_V4L2_CAP_TIMEPERFRAME),
	"V4L2_PIX_FMT_YUYV":            uint32(C.c_V4L2_PIX_FMT_YUYV),
	"V4L2_PIX_FMT_MJPEG":           uint32(C.c_V4L2_PIX_FMT_MJPEG),
}
