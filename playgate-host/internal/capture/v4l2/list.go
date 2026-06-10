//go:build linux

package v4l2

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

// ListDevices enumerates every /dev/video* node and queries each for its
// capture capabilities and supported formats. Devices that cannot be opened or
// do not support video capture are skipped (with their error surfaced only if
// every device fails). It is the programmatic backend for the listing command.
func ListDevices() ([]DeviceInfo, error) {
	paths, err := devicePaths()
	if err != nil {
		return nil, fmt.Errorf("enumerate v4l2 devices: %w", err)
	}

	var (
		infos   []DeviceInfo
		lastErr error
		anyOpen bool
	)
	for _, p := range paths {
		info, err := QueryDevice(p)
		if err != nil {
			lastErr = err
			continue
		}
		anyOpen = true
		infos = append(infos, info)
	}

	// Only bubble an error up if nothing at all could be opened. A mixed set
	// (some metadata nodes, some real capture devices) is the common case.
	if len(infos) == 0 && !anyOpen && lastErr != nil {
		return nil, lastErr
	}
	return infos, nil
}

// devicePaths returns the sorted list of /dev/video* device node paths.
func devicePaths() ([]string, error) {
	paths, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// QueryDevice opens a single device path and returns its DeviceInfo. It returns
// an error if the device cannot be opened or does not support video capture.
func QueryDevice(path string) (DeviceInfo, error) {
	dev, err := openDevice(path)
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer dev.close()

	cap, err := dev.queryCapability()
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("%s: %w", path, err)
	}
	if !cap.isCaptureCapable() {
		return DeviceInfo{}, fmt.Errorf("%s: not a video-capture device", path)
	}

	info := DeviceInfo{
		Path:   path,
		Driver: cString(cap.Driver[:]),
		Card:   cString(cap.Card[:]),
	}

	descs, err := dev.enumFormats()
	if err != nil {
		// Some drivers refuse ENUM_FMT; return what we have rather than failing.
		return info, nil
	}

	for _, d := range descs {
		pf, _ := FourCCToPixelFormat(d.PixelFormat)
		fi := FormatInfo{
			FourCC:      d.PixelFormat,
			PixelFormat: pf,
			Description: cString(d.Description[:]),
			FrameSizes:  dev.enumFrameSizes(d.PixelFormat),
		}
		info.Formats = append(info.Formats, fi)
	}
	return info, nil
}

// errNoDevices is returned when enumeration finds nothing. Exposed for callers
// that want to distinguish "no devices" from a hard failure.
var errNoDevices = errors.New("no v4l2 capture devices found")
