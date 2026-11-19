// This file implements the input-rate limiter / coalescer. It is pure stdlib,
// carries no build constraints, and is safe to use on any platform.
package nxbt

import (
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// maxPendingEdges bounds how many distinct button states the coalescer queues
// while rate-limited. Beyond it the oldest is dropped, so a burst can delay the
// newest state by at most this many rate intervals.
const maxPendingEdges = 4

// coalescer queues InputCommands and makes them available for consumption at a
// controlled rate. Commands that arrive faster than the rate limit are
// coalesced while the buttons are unchanged: only the latest stick position is
// kept. A command that changes the buttons is queued behind an unsent button
// change instead of replacing it, so a press and release that land inside one
// rate interval are both delivered rather than cancelling out. Commands that arrive
// slower than the rate limit are emitted as-is with no artificial delay.
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
	pending  []core.InputCommand // oldest first; empty when nothing to send
	interval time.Duration       // minimum time between emitted commands
	lastSent time.Time

	// sentButtons is the button state of the last command handed out by poll,
	// valid once haveSent is set.
	sentButtons uint32
	haveSent    bool

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

// push queues cmd. It replaces the newest queued command when that command
// holds no button change the consumer still needs to see: either it has the
// same buttons as cmd, or the same buttons as the command before it (the queued
// one before it, or the last one sent) so it is only a stick update, which cmd
// supersedes. Otherwise cmd is appended. Push is
// non-blocking and safe to call from multiple goroutines. After storing, it
// signals notify so an idle send loop wakes; the signal is dropped if one is
// already buffered.
func (c *coalescer) push(cmd core.InputCommand) {
	c.mu.Lock()
	n := len(c.pending)
	if n > 0 && (c.pending[n-1].Buttons == cmd.Buttons || c.stickOnlyLocked(n-1)) {
		c.pending[n-1] = cmd
	} else {
		if n == maxPendingEdges {
			c.pending = append(c.pending[:0], c.pending[1:]...)
		}
		c.pending = append(c.pending, cmd)
	}
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// stickOnlyLocked reports whether pending[i] carries no button edge: its buttons
// match the command delivered just before it. Comparing the newest entry with
// sentButtons alone is wrong once two entries are queued: with sent=0 and
// pending [A, 0], the release would look stick-only and a new A would overwrite
// it. Caller holds mu.
func (c *coalescer) stickOnlyLocked(i int) bool {
	if i > 0 {
		return c.pending[i].Buttons == c.pending[i-1].Buttons
	}
	return c.haveSent && c.pending[i].Buttons == c.sentButtons
}

// poll returns the oldest pending command if the rate-limit interval has
// elapsed since the last successful poll. Returns (_, false) when there is
// nothing to send (either no pending command or the interval has not elapsed
// yet).
func (c *coalescer) poll() (core.InputCommand, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.pending) == 0 {
		return core.InputCommand{}, false
	}

	now := time.Now()
	if c.interval > 0 && now.Sub(c.lastSent) < c.interval {
		return core.InputCommand{}, false
	}

	cmd := c.pending[0]
	c.pending = append(c.pending[:0], c.pending[1:]...)
	c.lastSent = now
	c.sentButtons, c.haveSent = cmd.Buttons, true
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

	if len(c.pending) == 0 {
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
