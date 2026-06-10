// Package queue maintains per-room redemption order in memory.
// On server restart queue positions reset (sessions in DB keep their stored position).
package queue

import (
	"sync"
)

// Manager tracks how many sessions have been issued per room.
type Manager struct {
	mu      sync.Mutex
	counters map[string]int
}

// New creates a new Manager.
func New() *Manager {
	return &Manager{counters: make(map[string]int)}
}

// Next increments and returns the next queue position for a room.
// Position is 1-based: the first redeemer gets position 1.
func (m *Manager) Next(roomID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[roomID]++
	return m.counters[roomID]
}

// Current returns the current queue depth for a room without incrementing.
func (m *Manager) Current(roomID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[roomID]
}
