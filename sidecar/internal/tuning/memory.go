package tuning

import (
	"slices"
	"sync"
	"time"
)

// Case memory: the model is not asked about a case again while its last
// SkipAfter answers were all wasted and the case has not changed
// materially since (the W2-C streak rule, per case instead of per table).
// An answer is wasted when nothing it proposed was admitted, including an
// empty or unparseable answer; a provider failure neither extends nor
// breaks the streak.

type caseMemorySettings struct {
	SkipAfter int
	MaxAge    time.Duration
	CallRatio float64
	MeanRatio float64
}

// caseOutcome is how one answer about a case went.
type caseOutcome struct {
	Proposals int
	Wasted    int
	Malformed bool
	Failed    bool
}

type caseStreak struct {
	wasted int
	at     time.Time
	shape  []CaseStatement
}

type caseMemory struct {
	mu       sync.Mutex
	settings caseMemorySettings
	streaks  map[string]caseStreak
}

func newCaseMemory(s caseMemorySettings) *caseMemory {
	return &caseMemory{settings: s, streaks: map[string]caseStreak{}}
}

// note records an answer about c.
func (m *caseMemory) note(c Case, o caseOutcome, now time.Time) {
	if m == nil || o.Failed {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !o.Malformed && o.Wasted < o.Proposals {
		delete(m.streaks, c.ID)
		return
	}
	s := m.streaks[c.ID]
	if s.wasted > 0 && m.changed(s, c, now) {
		s = caseStreak{}
	}
	s.wasted++
	s.at, s.shape = now, slices.Clone(c.Statements)
	m.streaks[c.ID] = s
}

// skip reports whether the model should not be asked about c now.
func (m *caseMemory) skip(c Case, now time.Time) bool {
	if m == nil || m.settings.SkipAfter <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.streaks[c.ID]
	return ok && s.wasted >= m.settings.SkipAfter && !m.changed(s, c, now)
}

// changed reports a material change of c since the streak's last answer:
// the statement set, a call volume or mean time ratio of at least the
// configured ratio either way, or the memory's maximum age.
func (m *caseMemory) changed(s caseStreak, c Case, now time.Time) bool {
	if m.settings.MaxAge > 0 && !now.Before(s.at.Add(m.settings.MaxAge)) {
		return true
	}
	if len(s.shape) != len(c.Statements) {
		return true
	}
	for i, was := range s.shape {
		now := c.Statements[i]
		if now.QueryID != was.QueryID ||
			ratioAtLeast(float64(now.Calls), float64(was.Calls), m.settings.CallRatio) ||
			ratioAtLeast(now.MeanMs, was.MeanMs, m.settings.MeanRatio) {
			return true
		}
	}
	return false
}

// ratioAtLeast reports a/b or b/a of at least r (r <= 1: never).
func ratioAtLeast(a, b, r float64) bool {
	if r <= 1 || a <= 0 || b <= 0 {
		return r > 1 && (a > 0) != (b > 0)
	}
	return a/b >= r || b/a >= r
}
