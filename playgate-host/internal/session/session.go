package session

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// Policy controls how concurrent Claim requests are handled.
type Policy int

const (
	// PolicyQueue puts new claimants in a FIFO queue behind the current holder.
	// When the holder's session ends, the next queued viewer automatically gets
	// control. This is the default.
	PolicyQueue Policy = iota

	// PolicyReject immediately rejects Claim when another viewer is active.
	PolicyReject
)

// Config holds all tunable parameters for the Manager.
type Config struct {
	// PublicKeyBase64 is a base64-encoded (standard or URL, no padding required)
	// ed25519 public key used to verify JWT signatures. Exactly one of
	// PublicKeyBase64 or PublicKeyFile must be set.
	PublicKeyBase64 string

	// PublicKeyFile is a path to a file whose contents are a base64-encoded
	// ed25519 public key (same format as PublicKeyBase64).
	PublicKeyFile string

	// RoomID must match the room_id claim in presented JWTs.
	RoomID string

	// IdleTimeout is how long a viewer may go without sending any InputCommand
	// before being kicked. Zero or negative disables idle kicking.
	IdleTimeout time.Duration

	// TickInterval is how often EventTick events are emitted. Defaults to 1s.
	TickInterval time.Duration

	// QueuePolicy controls whether new claimants are queued or rejected.
	QueuePolicy Policy

	// MaxQueueDepth is the maximum number of viewers that may wait in queue.
	// Zero means unlimited.
	MaxQueueDepth int

	// now is an injectable clock for tests; nil means time.Now.
	now func() time.Time
}

func (c *Config) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

// tickInterval returns the resolved tick interval (minimum 100ms to avoid spam).
func (c *Config) tickInterval() time.Duration {
	if c.TickInterval >= 100*time.Millisecond {
		return c.TickInterval
	}
	return time.Second
}

// --- public errors ---

// ErrControllerActive is returned by Claim when PolicyReject is in effect and
// another viewer currently holds control.
var ErrControllerActive = errors.New("session: another viewer is currently in control")

// ErrQueueFull is returned by Claim when the queue has reached MaxQueueDepth.
var ErrQueueFull = errors.New("session: queue is full")

// --- Manager ---

// Manager manages viewer session lifecycle: JWT validation, single-controller
// enforcement, session timing, idle detection, FIFO queuing, and event
// notification.
//
// Goroutine safety: all exported methods are safe for concurrent use.
type Manager struct {
	cfg    Config
	pubKey ed25519.PublicKey

	mu        sync.Mutex
	active    *sessionEntry   // current controller; nil when idle
	queue     []*sessionEntry // waiting claimants in FIFO order
	eventsOut chan SessionEvent
	stopped   bool // set to true by Run before closing eventsOut
}

// sessionEntry is the internal state for one viewer claim.
type sessionEntry struct {
	viewerID string
	claims   Claims

	// cancel cancels playerCtx. Called on session end.
	cancel context.CancelFunc
	// playerCtx is done when the session ends (timeout or manual cancel).
	playerCtx context.Context

	// idleTimer fires after IdleTimeout if not reset by incoming commands.
	// Protected by idleMu. Nil when idle kicking is disabled.
	idleMu       sync.Mutex
	idleTimer    *time.Timer
	idleDuration time.Duration

	// closedGates ensures gate channels are closed exactly once.
	gatesMu   sync.Mutex
	gatesDone bool
	gates     []*gateHandle
}

// gateHandle holds a Gate output channel and a once-close guard. done is closed
// alongside ch so the Gate goroutine stops consuming the shared command stream
// the moment the session ends — otherwise a re-authorization on the same
// connection would leave the old goroutine competing for peer.Commands().
type gateHandle struct {
	mu     sync.Mutex
	ch     chan core.InputCommand
	done   chan struct{}
	closed bool
}

func (g *gateHandle) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		close(g.ch)
		close(g.done)
		g.closed = true
	}
}

// NewManager constructs a Manager. Call Run to start background goroutines.
func NewManager(cfg Config) (*Manager, error) {
	pk, err := loadPublicKey(cfg)
	if err != nil {
		return nil, fmt.Errorf("session.NewManager: %w", err)
	}
	return &Manager{
		cfg:       cfg,
		pubKey:    pk,
		eventsOut: make(chan SessionEvent, 64),
	}, nil
}

// Events returns a receive-only channel of SessionEvent values.
//
// Listeners (T6 control-channel module) should drain this channel continuously.
// The channel is closed when the Manager's Run context is cancelled.
// Buffer is 64; slow consumers cause events to be dropped rather than blocking
// the session goroutines.
func (m *Manager) Events() <-chan SessionEvent {
	return m.eventsOut
}

// CurrentViewerID returns the viewer_id of the viewer currently holding
// control, or "" when no session is active. Safe for concurrent use.
func (m *Manager) CurrentViewerID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return ""
	}
	return m.active.viewerID
}

// KickCurrentController terminates the active session using the idle-kick path
// (EventIdleKicked), which revokes control authority and closes all Gate
// channels for the current controller. It is a no-op when no viewer is active.
// Safe for concurrent use.
func (m *Manager) KickCurrentController() {
	m.mu.Lock()
	entry := m.active
	m.mu.Unlock()
	if entry == nil {
		return
	}
	m.endSession(entry, EventIdleKicked)
}

// Claim validates token, checks room_id, and attempts to grant control to the
// viewer identified by the JWT's viewer_id claim.
//
// On success the returned *Session is non-nil; its Context is cancelled when
// the session ends. After the session ends the next queued viewer (if any) is
// promoted automatically.
//
// Possible errors:
//   - JWT invalid / expired / wrong room_id: descriptive error.
//   - PolicyReject and another viewer is active: ErrControllerActive.
//   - Queue full: ErrQueueFull.
func (m *Manager) Claim(token string) (*Session, error) {
	now := m.cfg.clock()()
	claims, err := ParseAndVerify(token, m.pubKey, now)
	if err != nil {
		return nil, fmt.Errorf("session.Claim: %w", err)
	}
	if claims.RoomID != m.cfg.RoomID {
		return nil, fmt.Errorf("session.Claim: room_id mismatch (token=%q, host=%q)", claims.RoomID, m.cfg.RoomID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active == nil {
		entry := m.startSessionLocked(claims)
		return &Session{entry: entry, manager: m}, nil
	}

	if m.cfg.QueuePolicy == PolicyReject {
		return nil, ErrControllerActive
	}

	if m.cfg.MaxQueueDepth > 0 && len(m.queue) >= m.cfg.MaxQueueDepth {
		return nil, ErrQueueFull
	}

	entry := m.enqueueLocked(claims)
	return &Session{entry: entry, manager: m}, nil
}

// Release ends the session for s if it currently holds control (promoting the
// next queued viewer), or removes it from the wait queue if it is still waiting.
// It is the host's signal that this viewer's command stream has ended (the viewer
// disconnected), so the controller slot is freed immediately instead of lingering
// until the JWT's session_seconds (or idle timeout) elapses and blocking the next
// viewer in the queue. No-op if the session already ended. Safe for concurrent use.
func (m *Manager) Release(s *Session) {
	if s == nil {
		return
	}
	entry := s.entry
	m.mu.Lock()
	if m.active == entry {
		m.mu.Unlock()
		m.endSession(entry, EventExpired)
		return
	}
	for i, e := range m.queue {
		if e == entry {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			e.cancel()
			e.closeGates()
			break
		}
	}
	m.mu.Unlock()
}

// Gate creates a command-forwarding goroutine for the given viewer. Commands
// received on rawIn are forwarded to the returned channel only while viewerID
// is the active controller. The returned channel is closed when:
//   - rawIn is closed (viewer disconnected), OR
//   - The session for viewerID ends (expiry or idle kick).
//
// Gate MUST be called after Claim returns successfully for viewerID.
// Multiple Gate calls for the same viewerID are allowed; each returns an
// independent channel, all closed simultaneously on session end.
//
// The caller owns rawIn and must close it when the viewer disconnects.
// After Gate returns, the caller should forward the returned channel to the
// input module:
//
//	gatedCh := manager.Gate(viewerID, rawCh)
//	for cmd := range gatedCh {
//	    inputTarget.Send(cmd)
//	}
func (m *Manager) Gate(viewerID string, rawIn <-chan core.InputCommand) <-chan core.InputCommand {
	outCh := make(chan core.InputCommand, 8)
	handle := &gateHandle{ch: outCh, done: make(chan struct{})}

	m.mu.Lock()
	entry := m.findEntryLocked(viewerID)
	if entry != nil && !entry.gatesDone {
		entry.gatesMu.Lock()
		entry.gates = append(entry.gates, handle)
		entry.gatesMu.Unlock()
	} else {
		// No active/queued session for this viewer; close immediately.
		m.mu.Unlock()
		close(outCh)
		return outCh
	}
	m.mu.Unlock()

	go func() {
		defer handle.close() // safe: mutex-protected inside
		for {
			var cmd core.InputCommand
			var ok bool
			select {
			case <-handle.done:
				// Session ended: stop consuming the shared command stream so a
				// re-authorized session can take it over cleanly.
				return
			case cmd, ok = <-rawIn:
				if !ok {
					return // viewer disconnected
				}
			}

			// Refresh idle timer on every incoming command.
			m.mu.Lock()
			cur := m.findEntryLocked(viewerID)
			m.mu.Unlock()
			if cur != nil {
				cur.touchIdle()
			}

			// Forward only if this viewer currently holds control.
			m.mu.Lock()
			isActive := m.active != nil && m.active.viewerID == viewerID
			m.mu.Unlock()

			if isActive {
				handle.mu.Lock()
				if !handle.closed {
					select {
					case outCh <- cmd:
					default:
						// Drop: downstream full.
					}
				}
				handle.mu.Unlock()
			}
		}
	}()

	return outCh
}

// Run blocks until ctx is cancelled. It implements core.Module so it can be
// wired into the same errgroup as other host modules. On shutdown it cancels
// all active/queued sessions and closes the Events channel.
func (m *Manager) Run(ctx context.Context) error {
	<-ctx.Done()

	m.mu.Lock()
	m.stopped = true
	if m.active != nil {
		m.active.cancel()
		m.active.closeGates()
	}
	for _, e := range m.queue {
		e.cancel()
		e.closeGates()
	}
	m.mu.Unlock()

	close(m.eventsOut)
	return nil
}

// Name implements core.Module.
func (m *Manager) Name() string { return "session-manager" }

// --- internal locked helpers (caller must hold m.mu) ---

// startSessionLocked grants control to claims. Must hold m.mu.
func (m *Manager) startSessionLocked(claims Claims) *sessionEntry {
	dur := time.Duration(claims.SessionSeconds) * time.Second
	playerCtx, cancel := context.WithTimeout(context.Background(), dur)

	entry := &sessionEntry{
		viewerID:  claims.ViewerID,
		claims:    claims,
		cancel:    cancel,
		playerCtx: playerCtx,
	}

	if m.cfg.IdleTimeout > 0 {
		entry.idleDuration = m.cfg.IdleTimeout
		// Capture entry pointer for the closure; safe because we never reuse entries.
		e := entry
		entry.idleTimer = time.AfterFunc(m.cfg.IdleTimeout, func() {
			m.endSession(e, EventIdleKicked)
		})
	}

	m.active = entry
	m.emitEvent(newEvent(EventGranted, claims.ViewerID, int(claims.SessionSeconds), 0))

	go m.watchSession(entry)

	return entry
}

// enqueueLocked adds claims to the FIFO wait list. Must hold m.mu.
func (m *Manager) enqueueLocked(claims Claims) *sessionEntry {
	// Use a plain cancelable context as a placeholder; it will be replaced with
	// a real timeout context when the viewer is promoted.
	bgCtx, cancel := context.WithCancel(context.Background())
	entry := &sessionEntry{
		viewerID:  claims.ViewerID,
		claims:    claims,
		cancel:    cancel,
		playerCtx: bgCtx,
	}
	m.queue = append(m.queue, entry)
	m.emitEvent(newEvent(EventQueued, claims.ViewerID, 0, len(m.queue)))
	return entry
}

// findEntryLocked returns the sessionEntry for viewerID (active or queued).
// Must hold m.mu.
func (m *Manager) findEntryLocked(viewerID string) *sessionEntry {
	if m.active != nil && m.active.viewerID == viewerID {
		return m.active
	}
	for _, e := range m.queue {
		if e.viewerID == viewerID {
			return e
		}
	}
	return nil
}

// emitEvent sends ev to eventsOut without blocking. Must hold m.mu (or be
// called from a goroutine that exclusively writes events).
// It is a no-op after Run has closed the channel (stopped == true).
func (m *Manager) emitEvent(ev SessionEvent) {
	if m.stopped {
		return
	}
	select {
	case m.eventsOut <- ev:
	default:
		// Drop — slow consumer.
	}
}

// --- goroutines ---

// watchSession monitors a session context and emits tick / expired events.
// Runs in its own goroutine per active session.
func (m *Manager) watchSession(entry *sessionEntry) {
	ticker := time.NewTicker(m.cfg.tickInterval())
	defer ticker.Stop()

	for {
		select {
		case <-entry.playerCtx.Done():
			// Normal timeout (context.WithTimeout fired). The idle-kick path
			// calls entry.cancel() which also reaches here, but endSession is
			// idempotent so the second call is a no-op.
			m.endSession(entry, EventExpired)
			return

		case <-ticker.C:
			deadline, ok := entry.playerCtx.Deadline()
			if !ok {
				continue
			}
			remaining := int(time.Until(deadline).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			m.mu.Lock()
			isStillActive := m.active == entry
			m.mu.Unlock()
			if isStillActive {
				m.mu.Lock()
				m.emitEvent(newEvent(EventTick, entry.viewerID, remaining, 0))
				m.mu.Unlock()
			}
		}
	}
}

// endSession terminates entry and promotes the next queued viewer if any.
// Idempotent: safe to call from both the watchSession goroutine and the idle
// timer goroutine simultaneously.
func (m *Manager) endSession(entry *sessionEntry, kind EventKind) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Guard: only act if this entry is still the active controller.
	if m.active == nil || m.active != entry {
		return
	}

	// Stop and disarm the idle timer before cancel so it cannot fire again.
	entry.idleMu.Lock()
	if entry.idleTimer != nil {
		entry.idleTimer.Stop()
		entry.idleTimer = nil
	}
	entry.idleMu.Unlock()

	// Cancel the context (no-op if already done by WithTimeout).
	entry.cancel()

	// Close all Gate output channels registered to this entry.
	entry.closeGates()

	m.active = nil
	m.emitEvent(newEvent(kind, entry.viewerID, 0, 0))

	// Promote the next queued viewer.
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		// Cancel the placeholder context the queued entry was holding.
		next.cancel()
		// Give it a fresh timed session.
		nextSess := m.startSessionLocked(next.claims)

		// Transfer registered gates from the queued sessionEntry to the new active sessionEntry.
		next.gatesMu.Lock()
		nextSess.gatesMu.Lock()
		nextSess.gates = next.gates
		next.gates = nil
		nextSess.gatesMu.Unlock()
		next.gatesMu.Unlock()
	}
}

// --- sessionEntry helpers ---

func (e *sessionEntry) touchIdle() {
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	if e.idleTimer != nil {
		e.idleTimer.Reset(e.idleDuration)
	}
}

func (e *sessionEntry) closeGates() {
	e.gatesMu.Lock()
	defer e.gatesMu.Unlock()
	e.gatesDone = true
	for _, h := range e.gates {
		h.close()
	}
	e.gates = nil
}

// --- Session (returned by Claim) ---

// Session represents a single viewer's claim on control authority.
// The session is active until its context is done. The caller can check
// Context().Done() or block on it to know when control is lost.
type Session struct {
	entry   *sessionEntry
	manager *Manager
}

// ViewerID returns the viewer this session belongs to.
func (s *Session) ViewerID() string { return s.entry.viewerID }

// Context is cancelled when the session ends (expiry, idle kick, or host
// shutdown). Callers may select on Context().Done() to react immediately.
func (s *Session) Context() context.Context { return s.entry.playerCtx }

// Claims returns the verified JWT claims for this session.
func (s *Session) Claims() Claims { return s.entry.claims }

// --- public key loading ---

func loadPublicKey(cfg Config) (ed25519.PublicKey, error) {
	switch {
	case cfg.PublicKeyBase64 != "" && cfg.PublicKeyFile != "":
		return nil, errors.New("specify exactly one of PublicKeyBase64 or PublicKeyFile, not both")
	case cfg.PublicKeyBase64 != "":
		return decodePublicKey(cfg.PublicKeyBase64)
	case cfg.PublicKeyFile != "":
		data, err := os.ReadFile(cfg.PublicKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read public key file %q: %w", cfg.PublicKeyFile, err)
		}
		return decodePublicKey(strings.TrimSpace(string(data)))
	default:
		return nil, errors.New("either PublicKeyBase64 or PublicKeyFile must be set")
	}
}

func decodePublicKey(s string) (ed25519.PublicKey, error) {
	// Accept standard or URL base64, with or without padding.
	s = strings.TrimRight(s, "=")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Fallback: try standard (non-URL) base64 without padding.
		b, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("decode public key: %w", err)
		}
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}
