package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// EncoderList returns the set of ffmpeg video encoders advertised by the binary
// at ffmpegPath, as a name->true map (e.g. "libx264", "h264_v4l2m2m"). It parses
// the output of `ffmpeg -hide_banner -encoders`, whose lines look like:
//
//	 V....D libx264              libx264 H.264 / AVC ...
//	 V..... h264_vaapi           H.264/AVC (VAAPI) ...
//
// The leading capability flags are ignored; the second whitespace-separated
// token is the encoder name. This is best-effort: a parse miss simply omits an
// encoder from the set.
func EncoderList(ctx context.Context, ffmpegPath string) (map[string]bool, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("encoder: run %q -encoders: %w", ffmpegPath, err)
	}
	return parseEncoderList(string(out)), nil
}

// parseEncoderList extracts encoder names from `ffmpeg -encoders` output. It is
// split out from EncoderList so it can be unit-tested with canned output without
// invoking ffmpeg.
func parseEncoderList(out string) map[string]bool {
	set := make(map[string]bool)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		// A valid encoder line has at least: <flags> <name> <description...>, and
		// the flags token is exactly 6 capability characters (e.g. "V....D"). The
		// header ("Encoders:", " ------") and legend lines don't match this shape.
		if len(fields) < 2 {
			continue
		}
		flags := fields[0]
		if len(flags) != 6 || !validFlags(flags) {
			continue
		}
		name := fields[1]
		if !validEncoderName(name) {
			// Skip the legend row (" V..... = Video"), whose name token is "=".
			continue
		}
		set[name] = true
	}
	return set
}

// validFlags reports whether s is a plausible ffmpeg encoder capability token:
// the first char is the media type (V/A/S) and the rest are flag letters or '.'
// placeholders. This rejects the legend line (" V..... = Video"), whose middle
// token "=" would otherwise be mistaken for an encoder name.
func validFlags(s string) bool {
	if s[0] != 'V' && s[0] != 'A' && s[0] != 'S' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c != '.' && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// validEncoderName reports whether name looks like an ffmpeg encoder identifier
// (letters, digits, underscores), rejecting tokens like "=" from the legend.
func validEncoderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_':
		default:
			return false
		}
	}
	return true
}

// DetectCodec verifies that codecName is present in available. On success it
// returns nil. On failure it returns an actionable error naming a fallback,
// without performing any silent downgrade — the caller logs and decides.
//
// available is typically the result of EncoderList; an empty/nil map means the
// probe failed, in which case detection is skipped (best-effort) and nil is
// returned so a probe failure never blocks startup.
func DetectCodec(codecName string, available map[string]bool) error {
	if codecName == "" {
		codecName = CodecLibX264
	}
	if len(available) == 0 {
		// Probe unavailable (e.g. ffmpeg not on PATH yet, or -encoders failed):
		// don't block startup on a best-effort check.
		return nil
	}
	if available[codecName] {
		return nil
	}
	return fmt.Errorf(
		"encoder: configured codec %q is not built into this ffmpeg; "+
			"available H.264 encoders: %s; "+
			"fall back to encoder.codec: %s (software) or install an ffmpeg with %q",
		codecName, h264EncodersIn(available), CodecLibX264, codecName)
}

// h264EncodersIn returns a comma-separated, human-readable list of the H.264
// encoders present in available, for the fallback error message.
func h264EncodersIn(available map[string]bool) string {
	var found []string
	for _, c := range []string{CodecLibX264, CodecV4L2M2M, CodecVAAPI, CodecNVENC} {
		if available[c] {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		return "(none detected)"
	}
	return strings.Join(found, ", ")
}
