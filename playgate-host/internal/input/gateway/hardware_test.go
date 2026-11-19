package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// Hardware checks against a real gateway on USB serial. Skipped unless
// PLAYGATE_GATEWAY_PORT is set ("auto" or a port name such as COM4):
//
//	PLAYGATE_GATEWAY_PORT=COM4 go test -run Hardware -v ./internal/input/gateway/
func hardwarePort(t *testing.T) string {
	p := os.Getenv("PLAYGATE_GATEWAY_PORT")
	if p == "" {
		t.Skip("PLAYGATE_GATEWAY_PORT not set")
	}
	return p
}

// readStatuses collects status reports from r for d.
func readStatuses(t *testing.T, r io.Reader, d time.Duration) []Status {
	t.Helper()
	type res struct{ s []Status }
	out := make(chan res, 1)
	stop := time.Now().Add(d)
	go func() {
		var got []Status
		var frame []byte
		buf := make([]byte, 256)
		for time.Now().Before(stop) {
			n, err := r.Read(buf)
			for _, b := range buf[:n] {
				if b != 0 {
					frame = append(frame, b)
					continue
				}
				if typ, pl, err := DecodeFrame(frame); err == nil && typ == typeStatus {
					if s, err := ParseStatus(pl); err == nil {
						got = append(got, s)
					}
				}
				frame = frame[:0]
			}
			if err != nil {
				break
			}
		}
		out <- res{got}
	}()
	select {
	case r := <-out:
		return r.s
	case <-time.After(d + 3*time.Second):
		t.Fatal("status reader stuck")
		return nil
	}
}

func TestHardwareLink(t *testing.T) {
	name := hardwarePort(t)
	if name == "auto" {
		found, err := findGatewayPort()
		if err != nil {
			t.Fatalf("auto-detect: %v", err)
		}
		t.Logf("auto-detect found %s", found)
		name = found
	}

	// 1. Open, send 20 frames, read status.
	p, err := openSerial(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	seq := uint32(time.Now().UnixMilli())
	for i := 0; i < 20; i++ {
		seq++
		if _, err := p.Write(EncodeInput(core.InputCommand{}, seq)); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := readStatuses(t, p, 1500*time.Millisecond)
	_ = p.Close()
	if len(st) == 0 {
		t.Fatal("no status report within 1.5s")
	}
	first := st[len(st)-1]
	t.Logf("after 20 frames: %+v (hid=%s)", first, hidStateName(first.HIDState))
	if first.ActiveSource != 1 || first.LastSeq != seq || first.Invalid != 0 || first.Stale != 0 {
		t.Errorf("gateway did not accept the frames: %+v (want source 1, last seq %d)", first, seq)
	}

	// 2. Reopen: counters must survive, i.e. opening the port did not reset
	// the chip (DTR/RTS handling).
	time.Sleep(200 * time.Millisecond)
	p, err = openSerial(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	seq++
	_, _ = p.Write(EncodeInput(core.InputCommand{}, seq))
	st = readStatuses(t, p, 1500*time.Millisecond)
	_ = p.Close()
	if len(st) == 0 {
		t.Fatal("no status after reopen")
	}
	again := st[len(st)-1]
	t.Logf("after reopen: %+v", again)
	if again.Accepted < first.Accepted {
		t.Errorf("accepted dropped %d -> %d: reopening the port reset the gateway", first.Accepted, again.Accepted)
	}
}

// TestHardwareHeld drives the Target and checks a held state never times out
// to neutral, and a release is sent once.
func TestHardwareHeld(t *testing.T) {
	name := hardwarePort(t)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	open := func(ctx context.Context) (io.ReadWriteCloser, error) { return openSerial(ctx, name) }
	tg := New(log, name, WithOpenFunc(open))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = tg.Run(ctx) }()
	go func() {
		for range tg.Status() {
		}
	}()

	time.Sleep(500 * time.Millisecond)
	beforePress := logBuf.Len() // the opening neutral frame times out normally
	_ = tg.Send(core.InputCommand{Buttons: core.ButtonA, Timestamp: time.Now()})
	time.Sleep(2 * time.Second) // held: 50 keepalives, never neutral
	whileHeld := logBuf.String()[beforePress:]
	_ = tg.Send(core.InputCommand{Timestamp: time.Now()})
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	if bytes.Contains([]byte(whileHeld), []byte("neutralised after input timeout")) {
		t.Error("gateway timed out to neutral while A was held")
	}

	out := logBuf.String()
	t.Log(out)
	if bytes.Contains(logBuf.Bytes(), []byte("rejected input")) {
		t.Error("gateway rejected input")
	}
	if bytes.Contains(logBuf.Bytes(), []byte("link lost")) {
		t.Error("serial link dropped")
	}
}
