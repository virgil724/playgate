package nxbt

import (
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// drain polls every command out of c, waiting out the rate limit between them.
func drain(c *coalescer) []core.InputCommand {
	var out []core.InputCommand
	for {
		if cmd, ok := c.poll(); ok {
			out = append(out, cmd)
			continue
		}
		d := c.untilReady()
		if d < 0 {
			return out
		}
		time.Sleep(d)
	}
}

// A press and its release arriving inside one rate interval must both reach the
// daemon; the release must not overwrite the unsent press.
func TestCoalescerKeepsTapInsideOneInterval(t *testing.T) {
	c := newCoalescer(50) // 20 ms interval
	c.push(core.InputCommand{})
	if _, ok := c.poll(); !ok {
		t.Fatal("first command should be sendable immediately")
	}

	c.push(core.InputCommand{Buttons: core.ButtonA})
	c.push(core.InputCommand{})

	got := drain(c)
	if len(got) != 2 || got[0].Buttons != core.ButtonA || got[1].Buttons != 0 {
		t.Fatalf("got %+v, want press then release", got)
	}
}

// Stick-only updates still coalesce to the latest position.
func TestCoalescerCoalescesStickUpdates(t *testing.T) {
	c := newCoalescer(50)
	c.push(core.InputCommand{})
	c.poll()

	for i := 1; i <= 10; i++ {
		c.push(core.InputCommand{LX: float32(i) / 10})
	}
	got := drain(c)
	if len(got) != 1 || got[0].LX != 1 {
		t.Fatalf("got %+v, want only the latest stick position", got)
	}
}

// A press arriving while a stick-only update is waiting replaces that update
// (its stick position is newer anyway) rather than queueing behind it.
func TestCoalescerPressNotQueuedBehindStickUpdate(t *testing.T) {
	c := newCoalescer(50)
	c.push(core.InputCommand{})
	c.poll()

	c.push(core.InputCommand{LX: 0.5})
	c.push(core.InputCommand{LX: 0.6, Buttons: core.ButtonA})

	got := drain(c)
	if len(got) != 1 || got[0].Buttons != core.ButtonA || got[0].LX != 0.6 {
		t.Fatalf("got %+v, want the press alone", got)
	}
}

// The queue is bounded: a burst of distinct button states keeps only the newest.
func TestCoalescerBoundsQueuedEdges(t *testing.T) {
	c := newCoalescer(50)
	c.push(core.InputCommand{})
	c.poll()

	for i := 1; i <= 20; i++ {
		c.push(core.InputCommand{Buttons: uint32(i)})
	}
	got := drain(c)
	if len(got) != maxPendingEdges || got[len(got)-1].Buttons != 20 {
		t.Fatalf("got %d commands ending in %+v, want %d ending in buttons=20",
			len(got), got[len(got)-1], maxPendingEdges)
	}
}
