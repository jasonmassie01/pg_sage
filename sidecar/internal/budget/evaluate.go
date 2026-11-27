package budget

import (
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Tally is one budget's would-be violations in one cycle.
//
//   - WouldCancel: statements (or idle in transaction waits) past the
//     budget's timeout, each counted once per day however many cycles
//     see it.
//   - WouldReject: the role's cluster-wide sessions past its connection
//     limit now (the ledger keeps the day's largest).
//   - ProtectedCancels: the would-cancels of a protected application
//     (runaway.safe_patterns), which block promotion to enforce.
//   - WorstMs: the longest matching statement or wait seen this cycle.
type Tally struct {
	Budget string
	// Definition is the budget's Definition: the ledger key with Budget.
	Definition       string
	WouldCancel      int
	WouldReject      int
	ProtectedCancels int
	WorstMs          float64
}

// seenKey identifies one statement (or one idle wait) of one budget.
type seenKey struct {
	budget string
	def    string
	pid    int
	start  time.Time
	state  string
}

// Evaluator counts would-be violations across cycles. It is safe for
// concurrent use.
type Evaluator struct {
	mu   sync.Mutex
	day  time.Time
	seen map[seenKey]bool
}

// NewEvaluator returns an evaluator with nothing seen.
func NewEvaluator() *Evaluator {
	return &Evaluator{seen: map[seenKey]bool{}}
}

// Evaluate tallies each budget against one workload reading at now. A
// nil reading (the category was unknown) counts nothing. protected are
// application_name substrings (case-insensitive) that are never to be
// cancelled.
func (e *Evaluator) Evaluate(budgets []Budget, ws *collector.WorkloadSessions,
	protected []string, now time.Time) []Tally {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollDay(now)
	out := make([]Tally, 0, len(budgets))
	for _, b := range budgets {
		t := Tally{Budget: b.Name, Definition: b.Definition()}
		if ws != nil {
			e.tallySessions(&t, b, ws.Long, protected)
			t.WouldReject = wouldReject(b, ws.Roles)
		}
		out = append(out, t)
	}
	return out
}

// rollDay forgets the statements seen on an earlier (UTC) day: the
// ledger counts per day.
func (e *Evaluator) rollDay(now time.Time) {
	day := now.UTC().Truncate(24 * time.Hour)
	if !day.Equal(e.day) {
		e.day, e.seen = day, map[seenKey]bool{}
	}
}

func (e *Evaluator) tallySessions(t *Tally, b Budget, long []collector.WorkloadLongSession,
	protected []string) {
	for _, s := range long {
		if !b.Match.covers(s) {
			continue
		}
		ageMs := s.AgeSeconds * 1000
		if ageMs > t.WorstMs {
			t.WorstMs = ageMs
		}
		limit := limitFor(b, s.State)
		// Strictly past the limit: a statement of exactly the timeout runs.
		if limit <= 0 || ageMs <= float64(limit) {
			continue
		}
		key := seenKey{budget: b.Name, def: t.Definition, pid: s.PID, start: s.QueryStart,
			state: s.State}
		if e.seen[key] {
			continue
		}
		e.seen[key] = true
		t.WouldCancel++
		if isProtected(s.Application, protected) {
			t.ProtectedCancels++
		}
	}
}

// limitFor is the budget's limit for a session in state (0: none).
func limitFor(b Budget, state string) int64 {
	switch {
	case state == "active":
		return b.StatementTimeoutMs
	case strings.HasPrefix(state, "idle in transaction"):
		return b.IdleInTransactionTimeoutMs
	}
	return 0
}

func wouldReject(b Budget, roles []collector.WorkloadRoleStats) int {
	if b.ConnectionLimit <= 0 || b.Match.Role == "" {
		return 0
	}
	for _, r := range roles {
		if r.Role == b.Match.Role && r.ClusterSessions > b.ConnectionLimit {
			return r.ClusterSessions - b.ConnectionLimit
		}
	}
	return 0
}

// covers reports whether the match selects session s.
func (m Match) covers(s collector.WorkloadLongSession) bool {
	switch {
	case m.Role != "":
		return s.Role == m.Role
	case m.ApplicationName != "":
		return s.Application == m.ApplicationName
	case m.Tag != "":
		k, v, _ := strings.Cut(m.Tag, "=")
		got, ok := s.Tags[k]
		return ok && got == v
	}
	return false
}

// isProtected matches application_name against the protected
// substrings, case-insensitively, as the runaway policy does.
func isProtected(app string, protected []string) bool {
	lower := strings.ToLower(app)
	for _, p := range protected {
		if p != "" && strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
