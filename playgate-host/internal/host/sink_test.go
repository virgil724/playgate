package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/session"
)

// signToken builds a minimal EdDSA JWT for the session manager.
func signToken(t *testing.T, priv ed25519.PrivateKey, room, viewer string, seconds int) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	claims := map[string]any{
		"exp":             time.Now().Add(time.Hour).Unix(),
		"room_id":         room,
		"viewer_id":       viewer,
		"session_seconds": seconds,
	}
	pj, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(pj)
	msg := header + "." + payload
	sig := ed25519.Sign(priv, []byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestSinkPassthrough verifies commands flow straight to the target when gating
// is disabled.
func TestSinkPassthrough(t *testing.T) {
	in := newFakeInput()
	sink := &inputSink{log: discardLogger(), target: in, manager: nil}

	raw := make(chan core.InputCommand, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.HandleCommands(ctx, raw, nil)

	raw <- core.InputCommand{Buttons: core.ButtonA}
	raw <- core.InputCommand{Buttons: core.ButtonB}
	close(raw)

	deadline := time.Now().Add(2 * time.Second)
	for in.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 commands forwarded, got %d", in.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSinkGatedAuthorize verifies that with gating enabled, a valid token allows
// commands through, and session events are forwarded to the control channel.
func TestSinkGatedAuthorize(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.NewManager(session.Config{
		PublicKeyBase64: base64.RawURLEncoding.EncodeToString(pub),
		RoomID:          "room1",
		TickInterval:    time.Hour, // avoid tick spam in the test
	})
	if err != nil {
		t.Fatal(err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	go func() { _ = mgr.Run(mgrCtx) }()

	in := newFakeInput()
	sink := &inputSink{log: discardLogger(), target: in, manager: mgr}

	// Collect control-channel events.
	gotGranted := make(chan struct{}, 1)
	sendControl := func(b []byte) error {
		var ev session.SessionEvent
		if err := json.Unmarshal(b, &ev); err == nil && ev.Kind == session.EventGranted {
			select {
			case gotGranted <- struct{}{}:
			default:
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.forwardEvents(ctx, sendControl)

	raw := make(chan core.InputCommand, 4)
	token := signToken(t, priv, "room1", "viewerX", 60)

	authDone := make(chan error, 1)
	go func() { authDone <- sink.Authorize(ctx, token, raw) }()

	// Expect a granted event.
	select {
	case <-gotGranted:
	case <-time.After(2 * time.Second):
		t.Fatal("no granted event forwarded to control channel")
	}

	// Commands now flow through the gate to the target.
	raw <- core.InputCommand{Buttons: core.ButtonA}
	deadline := time.Now().Add(2 * time.Second)
	for in.count() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("authorized command did not reach the target")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A bad token is rejected.
	if err := sink.Authorize(ctx, "not.a.jwt", make(chan core.InputCommand)); err == nil {
		t.Error("expected Authorize to reject an invalid token")
	}

	close(raw)
	cancel()
	<-authDone
}
