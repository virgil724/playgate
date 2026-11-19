package ffmpeg

// Normalised returns the options with zero-valued fields replaced by their
// defaults. Other encoder backends (internal/encoder/libav) use it so their
// defaults stay identical to the CLI backend's.
func (o Options) Normalised() Options { return o.normalise() }

// VBVBufsize returns the rate-control VBV buffer size in bits for the given
// bitrate and frame rate (the same rule rateControlArgs applies to -bufsize).
func VBVBufsize(bitrate, fps int) int { return vbvBufsize(bitrate, fps) }
