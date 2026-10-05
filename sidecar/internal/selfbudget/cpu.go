package selfbudget

import (
	"sync"
	"time"
)

// CPUMeter turns the process's cumulative CPU time into CPU per collector
// cycle between two observations. Safe for concurrent use.
type CPUMeter struct {
	read func() (time.Duration, bool)
	now  func() time.Time

	mu      sync.Mutex
	prevCPU time.Duration
	prevAt  time.Time
	has     bool
	last    float64
	known   bool
}

// NewCPUMeter reads CPU time with read (ProcessCPU in production) and the
// time with now (time.Now when nil).
func NewCPUMeter(read func() (time.Duration, bool), now func() time.Time) *CPUMeter {
	if now == nil {
		now = time.Now
	}
	return &CPUMeter{read: read, now: now}
}

// Observe reads the CPU time and returns the CPU used per cycle since the
// previous observation, in ms. ok is false on the first observation,
// after an unreadable one, and when the window is not usable (no wall
// time elapsed, the clock or the counter went back).
func (m *CPUMeter) Observe(cycle time.Duration) (float64, bool) {
	if m == nil || m.read == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cpu, readable := m.read()
	at := m.now()
	if !readable {
		m.has, m.known, m.last = false, false, 0
		return 0, false
	}
	prevCPU, prevAt, had := m.prevCPU, m.prevAt, m.has
	m.prevCPU, m.prevAt, m.has = cpu, at, true
	wall := at.Sub(prevAt)
	if !had || wall <= 0 || cpu < prevCPU || cycle <= 0 {
		m.known, m.last = false, 0
		return 0, false
	}
	used := float64(cpu-prevCPU) / float64(time.Millisecond)
	m.last = used * float64(cycle) / float64(wall)
	m.known = true
	return m.last, true
}

// Last is the latest observation's rate.
func (m *CPUMeter) Last() (float64, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.known
}
