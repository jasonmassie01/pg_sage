package connections

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Pooler sizing: an application with at least PoolerMinSessions sessions
// on average, at least PoolerIdleShare of them idle, is advised a pooler
// in transaction mode with a pool of its active peak plus PoolerMargin.
const (
	PoolerMinSessions = 10
	PoolerIdleShare   = 0.5
	PoolerMargin      = 0.2
)

// AppMix is one application's mean sessions per snapshot by state over
// the window: active, idle, idle in transaction (aborted ones included)
// and any other state, and its most active sessions in one snapshot.
type AppMix struct {
	Application  string
	Active       float64
	Idle         float64
	IdleInTx     float64
	Other        float64
	PeakActive   int
	PeakActiveAt time.Time
	// PeakAt is when the application's largest state count was seen.
	PeakAt time.Time
	peak   int
}

// Total is the application's mean sessions.
func (m AppMix) Total() float64 { return m.Active + m.Idle + m.IdleInTx + m.Other }

// IdleShare is the idle share of its mean sessions (0 with none).
func (m AppMix) IdleShare() float64 {
	if m.Total() == 0 {
		return 0
	}
	return m.Idle / m.Total()
}

// Mixes is each application's state mix, in application order.
func Mixes(in Inputs) []AppMix {
	by := map[string]*AppMix{}
	for _, s := range in.Mix {
		m := by[s.Application]
		if m == nil {
			m = &AppMix{Application: s.Application}
			by[s.Application] = m
		}
		switch {
		case s.State == "active":
			m.Active += s.Mean
			m.PeakActive, m.PeakActiveAt = s.Peak, s.PeakAt
		case s.State == "idle":
			m.Idle += s.Mean
		case strings.HasPrefix(s.State, "idle in transaction"):
			m.IdleInTx += s.Mean
		default:
			m.Other += s.Mean
		}
		if s.Peak > m.peak || m.PeakAt.IsZero() {
			m.peak, m.PeakAt = s.Peak, s.PeakAt
		}
	}
	out := make([]AppMix, 0, len(by))
	for _, m := range by {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b AppMix) int {
		return strings.Compare(a.Application, b.Application)
	})
	return out
}

// poolers advises a pooler for each mostly idle application.
func (b *builder) poolers() {
	for _, m := range Mixes(b.in) {
		if m.Total() < PoolerMinSessions || m.IdleShare() < PoolerIdleShare {
			continue
		}
		pool := max(1, int(math.Ceil(float64(m.PeakActive)*(1+PoolerMargin))))
		at := m.PeakActiveAt
		if at.IsZero() {
			at = m.PeakAt
		}
		idlePct := math.Round(m.IdleShare()*1000) / 10
		reason := fmt.Sprintf("%s holds %.1f sessions on average: %.1f active, %.1f idle "+
			"(%.1f%%) and %.1f idle in a transaction. Idle sessions hold a server backend "+
			"each. A pooler in transaction mode would serve its peak of %d active "+
			"sessions with a pool of %d (the peak plus %.0f%%). Transaction mode breaks "+
			"session state (SET, advisory locks, temporary tables, LISTEN, and prepared "+
			"statements before PgBouncer 1.21): check the application first, or use "+
			"session mode with the same pool size. pg_sage never changes pooler mode.",
			appName(m.Application), m.Total(), m.Active, m.Idle, idlePct, m.IdleInTx,
			m.PeakActive, pool, PoolerMargin*100)
		b.emit(candidate.Candidate{Kind: KindPoolerSizing, Target: appTarget(m.Application),
			ActionType: candidate.ActionAdvice, EvidenceIDs: []string{b.v.hour(at)},
			Reason: reason, Prediction: candidate.Prediction{Metric: "server_connections",
				Direction: "decrease", Source: "workload_sessions state mix by application",
				Quantities: map[string]float64{QuantityPoolSize: float64(pool),
					QuantityIdleSharePct: idlePct}}})
	}
}
