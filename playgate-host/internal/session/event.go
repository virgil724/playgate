package session

import "time"

// EventKind identifies the type of a SessionEvent.
type EventKind string

const (
	// EventGranted is emitted when a viewer gains control. RemainingSeconds
	// reflects the full session duration.
	EventGranted EventKind = "granted"

	// EventExpired is emitted when the session timer reaches zero normally.
	EventExpired EventKind = "expired"

	// EventIdleKicked is emitted when the viewer is removed for inactivity.
	EventIdleKicked EventKind = "idle_kicked"

	// EventQueued is emitted when a new Claim request is accepted but the
	// viewer must wait for the current session to end. QueuePosition is set.
	EventQueued EventKind = "queued"

	// EventTick is emitted every second (or configured tick interval) while
	// a session is active. RemainingSeconds counts down toward zero.
	EventTick EventKind = "tick"
)

// SessionEvent is the value pushed onto Manager.Events() to inform listeners
// (T6 control DataChannel, metrics, logging) about lifecycle changes and the
// remaining session time.
//
// # JSON shape pushed to the frontend (T11)
//
// Every event is serialised as a JSON object on the reliable control
// DataChannel. The frontend MUST handle all event kinds.
//
//	{
//	  "kind":              "granted" | "expired" | "idle_kicked" | "queued" | "tick",
//	  "viewer_id":         "<hex>",           // the viewer this event concerns
//	  "remaining_seconds": 120,               // 0 for expired/idle_kicked/queued
//	  "queue_position":    0,                 // 1-based; 0 when not queued
//	  "ts":                1718000000         // unix seconds (host wall-clock)
//	}
//
// The T6 module should send the event only to the viewer identified by
// ViewerID. The frontend uses remaining_seconds to render a countdown and
// queue_position to show "You are #N in queue".
type SessionEvent struct {
	Kind             EventKind `json:"kind"`
	ViewerID         string    `json:"viewer_id"`
	RemainingSeconds int       `json:"remaining_seconds"`
	QueuePosition    int       `json:"queue_position"` // 1-based; 0 means not queued
	Ts               int64     `json:"ts"`             // unix seconds
}

func newEvent(kind EventKind, viewerID string, remaining int, queuePos int) SessionEvent {
	return SessionEvent{
		Kind:             kind,
		ViewerID:         viewerID,
		RemainingSeconds: remaining,
		QueuePosition:    queuePos,
		Ts:               time.Now().Unix(),
	}
}
