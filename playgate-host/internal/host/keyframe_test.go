package host

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyframeCoalescer(t *testing.T) {
	var n atomic.Int32
	c := newKeyframeCoalescer(80*time.Millisecond, func() { n.Add(1) })
	defer c.Stop()

	c.Request() // fires immediately
	if got := n.Load(); got != 1 {
		t.Fatalf("first request: fires = %d, want 1", got)
	}
	for i := 0; i < 5; i++ { // burst inside the window: coalesced to one deferred fire
		c.Request()
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("burst fired early: %d", got)
	}
	deadline := time.Now().Add(time.Second)
	for n.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("after window fires = %d, want 2", got)
	}
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Fatalf("extra fires: %d, want 2", got)
	}
}
