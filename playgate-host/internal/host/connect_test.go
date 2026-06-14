package host

import (
	"context"
	"encoding/json"
	"testing"

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
