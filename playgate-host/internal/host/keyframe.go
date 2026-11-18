package host

import (
	"sync"
	"time"
)

// keyframeCoalescer turns bursts of viewer keyframe requests into at most one
// encoder ForceKeyframe per minInterval. The encoder is shared by every viewer,
// so one coalescer is shared too. A request inside the window is not dropped:
// it arms a single timer that fires at the window end
// (docs/webcodecs-transport.md section 3.4).
type keyframeCoalescer struct {
	fire        func()
	minInterval time.Duration
	now         func() time.Time

	mu      sync.Mutex
	last    time.Time
	pending *time.Timer
}

func newKeyframeCoalescer(minInterval time.Duration, fire func()) *keyframeCoalescer {
	return &keyframeCoalescer{fire: fire, minInterval: minInterval, now: time.Now}
}

// Request asks for a keyframe. It is safe for concurrent use and never blocks.
func (c *keyframeCoalescer) Request() {
	if c == nil || c.fire == nil {
		return
	}
	c.mu.Lock()
	if c.pending != nil {
		c.mu.Unlock() // the armed timer already covers this request
		return
	}
	now := c.now()
	wait := c.minInterval - now.Sub(c.last)
	if c.last.IsZero() || wait <= 0 {
		c.last = now
		c.mu.Unlock()
		c.fire()
		return
	}
	c.pending = time.AfterFunc(wait, func() {
		c.mu.Lock()
		c.pending = nil
		c.last = c.now()
		c.mu.Unlock()
		c.fire()
	})
	c.mu.Unlock()
}

// Stop cancels a pending deferred request.
func (c *keyframeCoalescer) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.pending != nil {
		c.pending.Stop()
		c.pending = nil
	}
	c.mu.Unlock()
}
