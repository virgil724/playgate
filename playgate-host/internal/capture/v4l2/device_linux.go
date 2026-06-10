//go:build linux

package v4l2

// device is the pure-Go equivalent of go4vl's device.Device, scoped to exactly
// what the capture source needs: open a /dev/video* node, query capabilities and
// formats, set the pixel format and frame rate, set up mmap streaming with a
// small ring of buffers, and dequeue/requeue frames.

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmapBuffer is one memory-mapped streaming buffer.
type mmapBuffer struct {
	data   []byte // mmap'd region
	length uint32
}

// device wraps an open V4L2 capture device using mmap streaming I/O.
type device struct {
	fd      int
	path    string
	buffers []mmapBuffer
	started bool
}

// openDevice opens a V4L2 device node (non-blocking is avoided; we use blocking
// DQBUF which is simplest for a dedicated dequeue goroutine).
func openDevice(path string) (*device, error) {
	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &device{fd: fd, path: path}, nil
}

// close releases all resources: stops streaming, unmaps buffers, closes the fd.
func (d *device) close() error {
	if d.started {
		_ = d.streamOff()
	}
	for _, b := range d.buffers {
		if b.data != nil {
			_ = unix.Munmap(b.data)
		}
	}
	d.buffers = nil
	if d.fd >= 0 {
		err := unix.Close(d.fd)
		d.fd = -1
		return err
	}
	return nil
}

// queryCapability issues VIDIOC_QUERYCAP.
func (d *device) queryCapability() (v4l2Capability, error) {
	var cap v4l2Capability
	if err := ioctl(uintptr(d.fd), vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return cap, fmtError("QUERYCAP", err)
	}
	return cap, nil
}

// isCaptureCapable reports whether the capability flags advertise streaming
// video capture (checking device_caps when present, else legacy capabilities).
func (c v4l2Capability) isCaptureCapable() bool {
	caps := c.Capabilities
	if c.DeviceCaps != 0 {
		caps = c.DeviceCaps
	}
	return caps&v4l2CapVideoCapture != 0
}

// enumFormats issues VIDIOC_ENUM_FMT repeatedly to list capture pixel formats.
func (d *device) enumFormats() ([]v4l2Fmtdesc, error) {
	var out []v4l2Fmtdesc
	for i := uint32(0); ; i++ {
		desc := v4l2Fmtdesc{Index: i, Type: v4l2BufTypeVideoCapture}
		if err := ioctl(uintptr(d.fd), vidiocEnumFmt, unsafe.Pointer(&desc)); err != nil {
			if err == unix.EINVAL {
				break // past the last format
			}
			return out, fmtError("ENUM_FMT", err)
		}
		out = append(out, desc)
	}
	return out, nil
}

// enumFrameSizes issues VIDIOC_ENUM_FRAMESIZES for a pixel format and flattens
// the results to (width,height) pairs (discrete entries, plus min/max corners
// of stepwise/continuous ranges).
func (d *device) enumFrameSizes(pixfmt uint32) []FrameSize {
	var sizes []FrameSize
	for i := uint32(0); ; i++ {
		e := v4l2Frmsizeenum{Index: i, PixelFormat: pixfmt}
		if err := ioctl(uintptr(d.fd), vidiocEnumFramesizes, unsafe.Pointer(&e)); err != nil {
			break // EINVAL = end, or unsupported: stop quietly
		}
		switch e.Type {
		case v4l2FrmsizeTypeDiscrete:
			ds := e.discrete()
			sizes = append(sizes, FrameSize{Width: int(ds.Width), Height: int(ds.Height)})
		case v4l2FrmsizeTypeStepwise, v4l2FrmsizeTypeContinuous:
			sw := e.stepwise()
			sizes = append(sizes,
				FrameSize{Width: int(sw.MinWidth), Height: int(sw.MinHeight)},
				FrameSize{Width: int(sw.MaxWidth), Height: int(sw.MaxHeight)},
			)
			// Stepwise enumeration returns a single entry; stop after it.
			return sizes
		default:
			return sizes
		}
	}
	return sizes
}

// setFormat issues VIDIOC_S_FMT for single-planar capture and returns the
// format the driver actually granted (it may clamp width/height).
func (d *device) setFormat(width, height, pixfmt uint32) (v4l2PixFormat, error) {
	var f v4l2Format
	f.Type = v4l2BufTypeVideoCapture
	p := f.pix()
	p.Width = width
	p.Height = height
	p.PixelFormat = pixfmt
	p.Field = v4l2FieldNone
	if err := ioctl(uintptr(d.fd), vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		return v4l2PixFormat{}, fmtError("S_FMT", err)
	}
	return *f.pix(), nil
}

// setFrameRate best-effort sets the capture frame interval via VIDIOC_S_PARM.
// Drivers that don't support it return an error we tolerate.
func (d *device) setFrameRate(fps uint32) error {
	if fps == 0 {
		return nil
	}
	var p v4l2StreamParm
	p.Type = v4l2BufTypeVideoCapture
	cp := p.capture()
	cp.Capability = v4l2CapTimePerFrame
	cp.TimePerFrame = v4l2Fract{Numerator: 1, Denominator: fps}
	if err := ioctl(uintptr(d.fd), vidiocSParm, unsafe.Pointer(&p)); err != nil {
		return fmtError("S_PARM", err)
	}
	return nil
}

// initBuffers requests `count` mmap buffers, queries each, mmaps it, and queues
// it ready for streaming.
func (d *device) initBuffers(count uint32) error {
	req := v4l2Requestbuffers{
		Count:  count,
		Type:   v4l2BufTypeVideoCapture,
		Memory: v4l2MemoryMMAP,
	}
	if err := ioctl(uintptr(d.fd), vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmtError("REQBUFS", err)
	}
	if req.Count < 1 {
		return fmt.Errorf("v4l2 REQBUFS: driver granted no buffers")
	}

	d.buffers = make([]mmapBuffer, req.Count)
	for i := uint32(0); i < req.Count; i++ {
		buf := v4l2Buffer{
			Index:  i,
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMMAP,
		}
		if err := ioctl(uintptr(d.fd), vidiocQueryBuf, unsafe.Pointer(&buf)); err != nil {
			return fmtError("QUERYBUF", err)
		}
		data, err := unix.Mmap(d.fd, int64(buf.MOffset()), int(buf.Length),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("v4l2 mmap buffer %d: %w", i, err)
		}
		d.buffers[i] = mmapBuffer{data: data, length: buf.Length}

		// Queue the buffer so the driver can fill it.
		if err := d.queueBuffer(i); err != nil {
			return err
		}
	}
	return nil
}

// queueBuffer issues VIDIOC_QBUF for the buffer at index.
func (d *device) queueBuffer(index uint32) error {
	buf := v4l2Buffer{
		Index:  index,
		Type:   v4l2BufTypeVideoCapture,
		Memory: v4l2MemoryMMAP,
	}
	if err := ioctl(uintptr(d.fd), vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
		return fmtError("QBUF", err)
	}
	return nil
}

// dequeueBuffer issues VIDIOC_DQBUF (blocking) and returns the dequeued buffer
// descriptor. The caller reads d.buffers[index].data[:bytesused], then must
// requeue the buffer via queueBuffer once done with the data.
func (d *device) dequeueBuffer() (v4l2Buffer, error) {
	buf := v4l2Buffer{
		Type:   v4l2BufTypeVideoCapture,
		Memory: v4l2MemoryMMAP,
	}
	if err := ioctl(uintptr(d.fd), vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
		return buf, fmtError("DQBUF", err)
	}
	return buf, nil
}

// streamOn starts streaming (VIDIOC_STREAMON).
func (d *device) streamOn() error {
	typ := int32(v4l2BufTypeVideoCapture)
	if err := ioctl(uintptr(d.fd), vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return fmtError("STREAMON", err)
	}
	d.started = true
	return nil
}

// streamOff stops streaming (VIDIOC_STREAMOFF).
func (d *device) streamOff() error {
	typ := int32(v4l2BufTypeVideoCapture)
	if err := ioctl(uintptr(d.fd), vidiocStreamOff, unsafe.Pointer(&typ)); err != nil {
		return fmtError("STREAMOFF", err)
	}
	d.started = false
	return nil
}
