// This file implements the input-rate limiter / coalescer. It is pure stdlib,
// carries no build constraints, and is safe to use on any platform.
package nxbt

import (
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// coalescer keeps the most-recently-seen InputCommand and makes it available
// for consumption at a controlled rate.  Commands that arrive faster than the
// rate limit are silently coalesced: only the latest one is kept. Commands that
// arrive slower than the rate limit are emitted as-is with no artificial delay.
//
// Usage:
//
//	c := newCoalescer(120) // 120 Hz cap
//	c.push(cmd)            // called from any goroutine, non-blocking
//	<-c.notifyC()         // send loop wakes when a command is pending
//	cmd, ok := c.poll()   // called from the send loop, returns false if nothing new
//
// The send loop is event-driven: push() signals notify after storing a command,
// so the loop sleeps when idle and wakes immediately on input. When poll()
// returns false because the rate limit has not elapsed, untilReady() tells the
// loop exactly how long to wait before the pending command becomes sendable.
type coalescer struct {
	mu       sync.Mutex
	pending  *core.InputCommand // nil when nothing to send
	interval time.Duration      // minimum time between emitted commands
	lastSent time.Time

	// notify carries a single buffered signal: push() does a non-blocking send
	// so the send loop wakes promptly without per-command allocation.
	notify chan struct{}
}

// newCoalescer returns a coalescer that will allow at most rateHz commands per
// second to pass through. rateHz ≤ 0 means unlimited.
func newCoalescer(rateHz int) *coalescer {
	var interval time.Duration
	if rateHz > 0 {
		interval = time.Second / time.Duration(rateHz)
	}
	return &coalescer{interval: interval, notify: make(chan struct{}, 1)}
}

// notifyC returns the channel signalled whenever a command is pushed. The send
// loop selects on it to wake from idle.
func (c *coalescer) notifyC() <-chan struct{} { return c.notify }

// push stores cmd as the pending command. Any previous pending command that has
// not yet been polled is overwritten (coalescing). Push is non-blocking and
// safe to call from multiple goroutines. After storing, it signals notify so an
// idle send loop wakes; the signal is dropped if one is already buffered.
func (c *coalescer) push(cmd core.InputCommand) {
	c.mu.Lock()
	c.pending = &cmd
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// poll returns the pending command if the rate-limit interval has elapsed since
// the last successful poll. Returns (_, false) when there is nothing to send
// (either no pending command or the interval has not elapsed yet).
func (c *coalescer) poll() (core.InputCommand, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return core.InputCommand{}, false
	}

	now := time.Now()
	if c.interval > 0 && now.Sub(c.lastSent) < c.interval {
		return core.InputCommand{}, false
	}

	cmd := *c.pending
	c.pending = nil
	c.lastSent = now
	return cmd, true
}

// untilReady reports how long the send loop should wait before the pending
// command becomes sendable, for arming a timer after poll() returns false:
//
//	< 0  no pending command — do not arm a timer
//	  0  a command is pending and sendable now (poll again)
//	> 0  a command is pending but rate-limited; wait this long, then poll
func (c *coalescer) untilReady() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return -1
	}
	if c.interval <= 0 {
		return 0
	}
	remaining := c.interval - time.Since(c.lastSent)
	if remaining < 0 {
		return 0
	}
	return remaining
}
