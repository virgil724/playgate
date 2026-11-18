package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/signaling"
)

// TestHandleFeedMessageIgnores verifies the feed dispatcher drops messages it
// cannot act on without creating a session: messages with no viewerId, and
// answers/candidates for a viewer that never said hello.
func TestHandleFeedMessageIgnores(t *testing.T) {
	cm := &connManager{log: discardLogger(), sessions: map[string]*viewerSession{}}
	mk := func(v any) signaling.Message {
		b, _ := json.Marshal(v)
		return signaling.Message{Payload: b}
	}

	cm.handleFeedMessage(context.Background(), mk(map[string]any{"kind": "hello"})) // no viewerId
	cm.handleFeedMessage(context.Background(), mk(map[string]any{
		"type": "answer", "sdp": "x", "viewerId": "V", // unknown viewer, not a hello
	}))

	if len(cm.sessions) != 0 {
		t.Fatalf("expected no sessions created, got %d", len(cm.sessions))
	}
}

// TestHelloWithNewSessionReplacesSession checks a viewer that rebuilt its peer
// connection (new hello nonce) gets a fresh session and the old one is
// cancelled, while a duplicate hello (same nonce) and a legacy hello without a
// nonce leave the existing session alone.
func TestHelloWithNewSessionReplacesSession(t *testing.T) {
	type started struct {
		ctx  context.Context
		sess *viewerSession
	}
	starts := make(chan started, 8)
	cm := &connManager{
		log:      discardLogger(),
		sessions: map[string]*viewerSession{},
		startViewer: func(ctx context.Context, _ string, sess *viewerSession) {
			starts <- started{ctx, sess}
		},
	}
	hello := func(session string) {
		v := map[string]any{"kind": "hello", "viewerId": "V"}
		if session != "" {
			v["session"] = session
		}
		b, _ := json.Marshal(v)
		cm.handleFeedMessage(context.Background(), signaling.Message{Payload: b})
	}
	next := func() started {
		select {
		case s := <-starts:
			return s
		case <-time.After(time.Second):
			t.Fatal("expected a session to start")
			return started{}
		}
	}
	noStart := func(what string) {
		select {
		case <-starts:
			t.Fatalf("%s started a new session", what)
		case <-time.After(50 * time.Millisecond):
		}
	}

	hello("a")
	first := next()
	hello("a")
	noStart("duplicate hello")
	hello("")
	noStart("legacy hello without a nonce")
	if first.ctx.Err() != nil {
		t.Fatal("first session cancelled by a duplicate hello")
	}

	hello("b")
	second := next()
	if first.ctx.Err() == nil {
		t.Error("old session not cancelled after the viewer restarted")
	}
	if second.ctx.Err() != nil {
		t.Error("new session born cancelled")
	}
	cm.mu.Lock()
	cur := cm.sessions["V"]
	cm.mu.Unlock()
	if cur != second.sess || cur.nonce != "b" {
		t.Errorf("sessions[V] = %+v, want the new session with nonce b", cur)
	}
}

// TestAwaitPeerEndDisconnectGrace checks disconnected is given time to
// recover, while failed ends the session immediately.
func TestAwaitPeerEndDisconnectGrace(t *testing.T) {
	run := func(states ...webrtc.PeerConnectionState) (time.Duration, chan webrtc.PeerConnectionState) {
		ch := make(chan webrtc.PeerConnectionState, len(states)+1)
		for _, s := range states {
			ch <- s
		}
		start := time.Now()
		done := make(chan struct{})
		go func() { awaitPeerEnd(context.Background(), ch, 100*time.Millisecond); close(done) }()
		select {
		case <-done:
			return time.Since(start), ch
		case <-time.After(300 * time.Millisecond):
			return -1, ch
		}
	}
	if d, _ := run(webrtc.PeerConnectionStateFailed); d < 0 || d > 50*time.Millisecond {
		t.Errorf("failed should end at once, took %v", d)
	}
	if d, _ := run(webrtc.PeerConnectionStateDisconnected); d < 90*time.Millisecond {
		t.Errorf("disconnected ended after %v, want the 100ms grace", d)
	}
	if d, _ := run(webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateConnected); d >= 0 {
		t.Errorf("recovered connection should keep the session, ended after %v", d)
	}
}

type fakeKeyEnc struct{ n int }

func (f *fakeKeyEnc) SetBitrate(int)   {}
func (f *fakeKeyEnc) ForceKeyframe()   { f.n++ }

func TestKeyframeForcerOnlyInIntraRefresh(t *testing.T) {
	enc := &fakeKeyEnc{}
	cfg := config.Default()
	if keyframeForcer(cfg, enc) != nil {
		t.Fatal("forcer must be nil when intra_refresh is off")
	}
	cfg.Encoder.IntraRefresh = true
	f := keyframeForcer(cfg, enc)
	if f == nil {
		t.Fatal("forcer must be set in intra-refresh mode")
	}
	f()
	if enc.n != 1 {
		t.Fatalf("ForceKeyframe calls = %d", enc.n)
	}
}

type fakeCapsEnc struct {
	fakeKeyEnc
	cheap bool
}

func (f *fakeCapsEnc) Caps() core.EncoderCaps { return core.EncoderCaps{CheapKeyframe: f.cheap} }

func newTestKeyMgr(enc *fakeCapsEnc, cfg config.Config) *connManager {
	cm := &connManager{
		kf:       newKeyframeCoalescer(time.Hour, enc.ForceKeyframe),
		cheapKey: cheapKeyframe(enc),
		forceKey: keyframeForcer(cfg, enc),
	}
	return cm
}

func TestPLIOnlyWithCheapKeyframe(t *testing.T) {
	cli := &fakeCapsEnc{cheap: false}
	cm := newTestKeyMgr(cli, config.Default())
	defer cm.kf.Stop()
	if cm.pliHandler() != nil {
		t.Fatal("PLI must not be wired for a non-cheap-keyframe encoder")
	}

	libav := &fakeCapsEnc{cheap: true}
	cm = newTestKeyMgr(libav, config.Default())
	defer cm.kf.Stop()
	h := cm.pliHandler()
	if h == nil {
		t.Fatal("PLI must be wired when CheapKeyframe is true")
	}
	for i := 0; i < 10; i++ { // burst coalesces: first fires, the rest are deferred
		h()
	}
	if libav.n != 1 {
		t.Fatalf("ForceKeyframe calls = %d, want 1 (coalesced)", libav.n)
	}
}

func TestJoinKeyframe(t *testing.T) {
	// cli, no intra-refresh: nothing.
	cli := &fakeCapsEnc{}
	cm := newTestKeyMgr(cli, config.Default())
	defer cm.kf.Stop()
	cm.requestJoinKeyframe()
	if cli.n != 0 {
		t.Fatalf("cli join fired %d", cli.n)
	}

	// cheap keyframe: exactly one via the coalescer.
	libav := &fakeCapsEnc{cheap: true}
	cm = newTestKeyMgr(libav, config.Default())
	defer cm.kf.Stop()
	cm.requestJoinKeyframe()
	if libav.n != 1 {
		t.Fatalf("libav join fired %d, want 1", libav.n)
	}

	// cheap keyframe + intra-refresh: still one, not two.
	cfg := config.Default()
	cfg.Encoder.IntraRefresh = true
	ir := &fakeCapsEnc{cheap: true}
	cm = newTestKeyMgr(ir, cfg)
	defer cm.kf.Stop()
	cm.requestJoinKeyframe()
	if ir.n != 1 {
		t.Fatalf("libav+intra-refresh join fired %d, want 1 (no double fire)", ir.n)
	}

	// cli + intra-refresh keeps T2 behaviour: direct force.
	cliIR := &fakeCapsEnc{}
	cm = newTestKeyMgr(cliIR, cfg)
	defer cm.kf.Stop()
	cm.requestJoinKeyframe()
	if cliIR.n != 1 {
		t.Fatalf("cli+intra-refresh join fired %d, want 1", cliIR.n)
	}
}

// TestApplyAnswerGuards verifies applyAnswer is a safe no-op before the peer
// exists, ignores non-answers, and never marks a session answered in those cases.
func TestApplyAnswerGuards(t *testing.T) {
	cm := &connManager{log: discardLogger()}
	sess := &viewerSession{} // peer not set yet

	cm.applyAnswer(sess, viewerEnvelope{Type: "answer", SDP: "x"})
	if sess.answered {
		t.Fatal("answered set despite nil peer")
	}
	cm.applyAnswer(sess, viewerEnvelope{Type: "candidate"})
	if sess.answered {
		t.Fatal("answered set for a non-answer envelope")
	}
}
