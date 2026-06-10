// Command rtctest is a manual WebRTC signaling harness for PlayGate T4. It runs
// the *host* side of a single PeerConnection: it pushes an H.264 video stream to
// the browser over a MediaTrack and receives controller input over an unreliable
// DataChannel.
//
// Signaling is manual copy-paste over stdin/stdout using base64-encoded SDP. This
// tool is the OFFERER: it prints an offer, you paste it into the browser page
// (cmd/rtctest/index.html), the page produces an answer, and you paste that back
// here on stdin.
//
// Flow:
//
//  1. Run:  go run ./cmd/rtctest -h264 path/to/test.h264
//  2. Copy the printed base64 OFFER.
//  3. Open index.html in a browser, paste the offer, click "Start", copy the
//     generated ANSWER.
//  4. Paste the ANSWER into this terminal and press Enter.
//  5. The video plays in the browser; key presses stream back and print here.
//
// Provide your own raw Annex-B H.264 elementary stream via -h264 (e.g. produced
// with: ffmpeg -i in.mp4 -an -c:v libx264 -bsf:v h264_mp4toannexb -f h264 out.h264).
// A tiny built-in synthetic stream is used if -h264 is omitted, but it will NOT
// decode to a visible picture in a real browser — it only proves the pipe works.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/rtc"
)

func main() {
	h264Path := flag.String("h264", "", "path to a raw Annex-B H.264 elementary stream to loop (recommended)")
	fps := flag.Int("fps", 30, "frame rate used to pace the H.264 stream and compute PTS")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN/TURN URLs (empty for host-local only)")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log, *h264Path, *fps, *stun); err != nil {
		log.Error("rtctest failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, h264Path string, fps int, stun string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Prepare the video source.
	var packets []core.EncodedPacket
	if h264Path != "" {
		var err error
		packets, err = readAnnexB(h264Path, fps)
		if err != nil {
			return err
		}
		log.Info("loaded H.264 stream", "path", h264Path, "access_units", len(packets))
	} else {
		packets = builtinTestStream(fps)
		log.Warn("no -h264 file given: using built-in synthetic stream (will not render in a real decoder)")
	}

	// Build the host Peer (offerer).
	var ice []webrtc.ICEServer
	if urls := splitCSV(stun); len(urls) > 0 {
		ice = rtc.ICEServersFromURLs(urls)
	}
	peer, err := rtc.NewPeer(rtc.Config{ICEServers: ice, Logger: log})
	if err != nil {
		return err
	}
	defer peer.Close()

	// Log connection state and decoded controller input.
	go func() {
		for s := range peer.ConnState() {
			log.Info("peer connection state", "state", s.String())
		}
	}()
	go func() {
		for cmd := range peer.Commands() {
			fmt.Fprintf(os.Stderr, "INPUT buttons=%#010x LX=%+.3f LY=%+.3f RX=%+.3f RY=%+.3f\n",
				cmd.Buttons, cmd.LX, cmd.LY, cmd.RX, cmd.RY)
		}
	}()

	// --- Manual signaling ---
	offer, err := peer.CreateOffer(ctx)
	if err != nil {
		return err
	}
	offerB64, err := rtc.EncodeSDP(offer)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "\n================ COPY THIS OFFER INTO THE BROWSER ================")
	fmt.Println(offerB64)
	fmt.Fprintln(os.Stderr, "=================================================================")
	fmt.Fprintln(os.Stderr, "Paste the browser's ANSWER (base64) below and press Enter:")

	answerB64, err := readLine(os.Stdin)
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	answer, err := rtc.DecodeSDP(answerB64)
	if err != nil {
		return err
	}
	if err := peer.SetRemoteDescription(answer); err != nil {
		return err
	}
	log.Info("answer applied; establishing connection")

	// Send a demo control message every 5s on the reliable channel (T9 will carry
	// real session info such as remaining time).
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				msg := fmt.Sprintf(`{"type":"hello","ts":%d}`, t.Unix())
				if err := peer.SendControl([]byte(msg)); err != nil {
					log.Debug("control send skipped", "err", err)
				}
			}
		}
	}()

	// --- Pump the looping H.264 stream into the track at FPS pace. ---
	return pumpLoop(ctx, peer, packets, fps)
}

// pumpLoop writes the access units to the peer on a fixed-rate ticker, looping
// the stream forever until ctx is cancelled. PTS is monotonically increasing
// across loop iterations so sample durations stay positive.
func pumpLoop(ctx context.Context, peer *rtc.Peer, packets []core.EncodedPacket, fps int) error {
	frameDur := time.Second / time.Duration(maxInt(fps, 1))
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	var (
		i       int
		basePTS time.Duration
		loopLen = packets[len(packets)-1].PTS + frameDur
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pkt := packets[i]
			// Offset PTS by the accumulated loop length so it always increases.
			out := core.EncodedPacket{
				Data:       pkt.Data,
				PTS:        basePTS + pkt.PTS,
				IsKeyframe: pkt.IsKeyframe,
			}
			if err := peer.WriteSample(out, frameDur); err != nil {
				return fmt.Errorf("write sample: %w", err)
			}
			i++
			if i >= len(packets) {
				i = 0
				basePTS += loopLen
			}
		}
	}
}

// builtinTestStream synthesises a tiny SPS/PPS/IDR Annex-B access unit repeated as
// keyframes. It exercises the full path but is not a decodable picture.
func builtinTestStream(fps int) []core.EncodedPacket {
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sps := append(append([]byte{}, startCode...), 0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2)
	pps := append(append([]byte{}, startCode...), 0x68, 0xce, 0x38, 0x80)
	idr := append(append([]byte{}, startCode...), 0x65, 0x88, 0x84, 0x00, 0x10, 0xff)
	au := append(append(append([]byte{}, sps...), pps...), idr...)

	frameDur := time.Second / time.Duration(maxInt(fps, 1))
	pkts := make([]core.EncodedPacket, fps) // ~1s loop
	for i := range pkts {
		pkts[i] = core.EncodedPacket{
			Data:       au,
			PTS:        time.Duration(i) * frameDur,
			IsKeyframe: true,
		}
	}
	return pkts
}

func readLine(f *os.File) (string, error) {
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		return line, nil
	}
	return "", err
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
