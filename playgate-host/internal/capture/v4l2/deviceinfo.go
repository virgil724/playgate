package v4l2

import (
	"fmt"
	"sort"
	"strings"

	"github.com/playgate/playgate-host/internal/core"
)

// DeviceInfo describes one V4L2 capture device and the formats it advertises.
// It is platform-independent so the listing command can render it identically
// regardless of where the enumeration happened.
type DeviceInfo struct {
	// Path is the device node, e.g. "/dev/video0".
	Path string
	// Driver / Card are human-readable identifiers from VIDIOC_QUERYCAP.
	Driver string
	Card   string
	// Formats are the capture formats the device supports, in driver order.
	Formats []FormatInfo
}

// FormatInfo describes a single supported pixel format and its resolutions.
type FormatInfo struct {
	// FourCC is the raw V4L2 format code.
	FourCC uint32
	// PixelFormat is the mapped core type, or PixelFormatUnknown if unmodelled.
	PixelFormat core.PixelFormat
	// Description is the driver-supplied human description.
	Description string
	// FrameSizes are the discrete resolutions advertised for this format.
	// Stepwise/continuous ranges are flattened to their min and max corners.
	FrameSizes []FrameSize
}

// FrameSize is a single advertised resolution.
type FrameSize struct {
	Width  int
	Height int
}

// String renders a FrameSize as "WxH".
func (f FrameSize) String() string { return fmt.Sprintf("%dx%d", f.Width, f.Height) }

// String renders a FormatInfo for CLI output.
func (f FormatInfo) String() string {
	sizes := make([]string, len(f.FrameSizes))
	for i, s := range f.FrameSizes {
		sizes[i] = s.String()
	}
	name := FourCCString(f.FourCC)
	if f.Description != "" {
		name = fmt.Sprintf("%s (%s)", name, f.Description)
	}
	if len(sizes) == 0 {
		return name
	}
	return fmt.Sprintf("%s: %s", name, strings.Join(sizes, ", "))
}

// String renders a DeviceInfo as a multi-line CLI block.
func (d DeviceInfo) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", d.Path)
	if d.Card != "" || d.Driver != "" {
		fmt.Fprintf(&b, "  [%s / %s]", d.Card, d.Driver)
	}
	b.WriteByte('\n')
	if len(d.Formats) == 0 {
		b.WriteString("    (no capture formats reported)\n")
		return b.String()
	}
	for _, f := range d.Formats {
		fmt.Fprintf(&b, "    %s\n", f.String())
	}
	return b.String()
}

// FormatDeviceList renders a slice of DeviceInfo for the listing command.
func FormatDeviceList(devices []DeviceInfo) string {
	if len(devices) == 0 {
		return "no V4L2 capture devices found\n"
	}
	// Stable order by path for deterministic output.
	sort.Slice(devices, func(i, j int) bool { return devices[i].Path < devices[j].Path })
	var b strings.Builder
	for _, d := range devices {
		b.WriteString(d.String())
	}
	return b.String()
}

// AvailableFourCCSet collapses a DeviceInfo's formats into the set NegotiateFormat
// consumes.
func (d DeviceInfo) AvailableFourCCSet() map[uint32]bool {
	set := make(map[uint32]bool, len(d.Formats))
	for _, f := range d.Formats {
		set[f.FourCC] = true
	}
	return set
}
