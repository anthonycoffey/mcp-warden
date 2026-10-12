// Package metrics collects per-instance counters and request latencies.
package metrics

import (
	"slices"
	"sync"
	"time"
)

// reservoirSize bounds latency memory; percentiles are over the most recent
// reservoirSize requests, which is what an operator looking at `status` wants.
const reservoirSize = 1024

// Metrics is safe for concurrent use.
type Metrics struct {
	mu            sync.Mutex
	requests      int64
	notifications int64
	errors        int64
	latencies     []time.Duration // ring buffer
	next          int
	max           time.Duration
}

// New returns an empty Metrics.
func New() *Metrics {
	return &Metrics{latencies: make([]time.Duration, 0, reservoirSize)}
}

// Request counts a client→server request.
func (m *Metrics) Request() { m.mu.Lock(); m.requests++; m.mu.Unlock() }

// Notification counts a client→server notification.
func (m *Metrics) Notification() { m.mu.Lock(); m.notifications++; m.mu.Unlock() }

// Error counts an error: an error response, a synthesized error, or a
// protocol violation.
func (m *Metrics) Error() { m.mu.Lock(); m.errors++; m.mu.Unlock() }

// Observe records how long a request took to be answered.
func (m *Metrics) Observe(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latencies) < reservoirSize {
		m.latencies = append(m.latencies, d)
	} else {
		m.latencies[m.next] = d
		m.next = (m.next + 1) % reservoirSize
	}
	m.max = max(m.max, d)
}

// Snapshot is a point-in-time copy, shaped for the state file.
type Snapshot struct {
	Requests      int64
	Notifications int64
	Errors        int64
	P50, P95, Max time.Duration
}

// Snapshot returns the current values.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	s := Snapshot{Requests: m.requests, Notifications: m.notifications, Errors: m.errors, Max: m.max}
	lat := slices.Clone(m.latencies)
	m.mu.Unlock()

	if len(lat) > 0 {
		slices.Sort(lat)
		s.P50 = percentile(lat, 0.50)
		s.P95 = percentile(lat, 0.95)
	}
	return s
}

// percentile uses the nearest-rank method on sorted data.
func percentile(sorted []time.Duration, p float64) time.Duration {
	idx := int(float64(len(sorted))*p+0.5) - 1
	idx = max(0, min(idx, len(sorted)-1))
	return sorted[idx]
}
