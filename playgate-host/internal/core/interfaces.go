package core

import "context"

// Module is the uniform lifecycle contract every PlayGate Host component
// implements. main wires modules together and runs each one's Run method inside
// an errgroup. Run is expected to block until the work is done or ctx is
// cancelled.
//
// Contract:
//   - Run MUST honour ctx: when ctx is cancelled, Run should stop promptly,
//     release resources, close any output channels it owns, and return.
//   - Returning a non-nil error (other than ctx.Err()) signals a fatal failure
//     and triggers a coordinated shutdown of all other modules.
//   - Returning nil or ctx.Err() on cancellation is the normal, clean exit.
//   - Name is used purely for logging and must be stable for a given module.
type Module interface {
	Name() string
	Run(ctx context.Context) error
}

// CaptureSource is a pluggable producer of raw video frames (e.g. a v4l2 capture
// card, a test pattern generator, or in future an HDMI grabber). It is the head
// of the media pipeline.
//
// Ownership: the CaptureSource owns the channels returned by Frames and Errors.
// It creates them, is the sole writer, and closes both when its Run loop exits.
// Start kicks off the capture loop; callers should call Start exactly once and
// then range over Frames until it is closed.
type CaptureSource interface {
	Module

	// Start begins capturing. It is non-blocking: the actual loop runs in the
	// goroutine driven by Run. Returning an error means capture could not be
	// initialised (e.g. device busy).
	Start(ctx context.Context) error

	// Frames returns the receive-only channel of captured frames. Closed by the
	// source when capture stops.
	Frames() <-chan VideoFrame

	// Errors returns the receive-only channel of non-fatal capture errors.
	// Fatal errors are reported via Run's return value instead.
	Errors() <-chan error
}

// InputTarget is a pluggable sink for controller commands (e.g. the NXBT Unix
// socket bridge driving a virtual Switch Pro Controller). It is the tail of the
// control pipeline.
//
// Ownership: the InputTarget owns the channel returned by Status; it is the sole
// writer and closes it when Run exits.
type InputTarget interface {
	Module

	// Start establishes the link to the target device. Non-blocking; the link
	// is maintained by the Run goroutine. An error means the link could not be
	// brought up.
	Start(ctx context.Context) error

	// Send delivers a single controller state to the target. It is safe to call
	// from a different goroutine than Run. It should be non-blocking / best
	// effort: returning an error means the command could not be queued (e.g.
	// link down), and the caller may drop it.
	Send(cmd InputCommand) error

	// Status returns a receive-only channel that emits the latest TargetStatus
	// whenever the connection state changes. Closed by the target when Run exits.
	Status() <-chan TargetStatus
}
