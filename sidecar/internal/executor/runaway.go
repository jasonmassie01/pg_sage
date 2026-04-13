package executor

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// trackerKey uniquely identifies a backend query. Using both PID and
// QueryStart handles PID reuse: if PID 42 finishes and a new backend
// gets PID 42, the new QueryStart disambiguates.
type trackerKey struct {
	PID        int
	QueryStart time.Time
}

// TrackedQuery holds the escalation state for a single runaway query.
type TrackedQuery struct {
	PID              int
	QueryStart       time.Time
	QueryID          int64
	QueryText        string // Truncated to 200 chars.
	AppName          string
	FirstSeenCycle   uint64
	MatchedPolicy    string
	State            string // "", "warned", "cancelled", "terminated"
	WarnedAtCycle    uint64
	CancelledAtCycle uint64
}

// ActiveQuery represents a currently running query from pg_stat_activity.
type ActiveQuery struct {
	PID        int
	QueryStart time.Time
	QueryID    int64
	Query      string
	AppName    string
	Duration   time.Duration
	State      string
}

// RunawayTracker implements the warn -> cancel -> terminate state
// machine for runaway query detection. Each analyzer cycle calls
// Evaluate, which advances the state machine and returns findings.
type RunawayTracker struct {
	mu      sync.Mutex
	tracked map[trackerKey]*TrackedQuery
	cycle   uint64
	ownPID  int
	cfg     *config.RunawayConfig
	logFn   func(string, string, ...any)
}

// NewRunawayTracker creates a tracker bound to the given config.
// ownPID is the sidecar's own backend PID so it never terminates itself.
// logFn matches the sidecar's structured logger signature (level, msg, kv...).
func NewRunawayTracker(
	cfg *config.RunawayConfig,
	ownPID int,
	logFn func(string, string, ...any),
) *RunawayTracker {
	return &RunawayTracker{
		tracked: make(map[trackerKey]*TrackedQuery),
		ownPID:  ownPID,
		cfg:     cfg,
		logFn:   logFn,
	}
}

// isSafeRunawayProcess returns true when a process should never be
// cancelled or terminated. It protects the sidecar's own PID and any
// process whose application_name contains a configured safe substring.
func isSafeRunawayProcess(
	appName string, pid int, ownPID int, patterns []string,
) bool {
	if pid == ownPID {
		return true
	}
	lower := strings.ToLower(appName)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// Evaluate advances the runaway state machine by one cycle. It prunes
// queries that finished, tracks new matches, escalates existing ones,
// and returns findings for the executor to act on.
//
// lockChainBlockers maps PID -> total blocked session count, supplied
// by the analyzer's lock chain detection pass.
func (rt *RunawayTracker) Evaluate(
	activeQueries []ActiveQuery,
	lockChainBlockers map[int]int,
) []analyzer.Finding {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	rt.cycle++

	// Build set of currently active keys for pruning.
	activeKeys := make(map[trackerKey]struct{}, len(activeQueries))
	for _, aq := range activeQueries {
		activeKeys[trackerKey{PID: aq.PID, QueryStart: aq.QueryStart}] = struct{}{}
	}

	// Prune: remove tracked entries whose query finished or whose PID
	// was reused with a different QueryStart.
	for k := range rt.tracked {
		if _, alive := activeKeys[k]; !alive {
			delete(rt.tracked, k)
		}
	}

	// Evaluate each active query against policies.
	for i := range activeQueries {
		aq := &activeQueries[i]
		if isSafeRunawayProcess(aq.AppName, aq.PID, rt.ownPID, rt.cfg.SafePatterns) {
			continue
		}

		policy := rt.matchPolicy(aq, lockChainBlockers)
		if policy == nil {
			continue
		}

		key := trackerKey{PID: aq.PID, QueryStart: aq.QueryStart}
		tq, exists := rt.tracked[key]
		if !exists {
			rt.tracked[key] = &TrackedQuery{
				PID:            aq.PID,
				QueryStart:     aq.QueryStart,
				QueryID:        aq.QueryID,
				QueryText:      truncateQuery(aq.Query, 200),
				AppName:        aq.AppName,
				FirstSeenCycle: rt.cycle,
				MatchedPolicy:  policy.Name,
			}
			continue
		}
		rt.advanceState(tq, policy)
	}

	return rt.buildFindings()
}

// matchPolicy finds the best matching policy for an active query.
// "Best" means the policy with the shortest total escalation time
// (WarnCycles + CancelCycles). Returns nil when no policy matches.
func (rt *RunawayTracker) matchPolicy(
	aq *ActiveQuery,
	lockChainBlockers map[int]int,
) *config.RunawayPolicy {
	var best *config.RunawayPolicy
	bestTotal := int(^uint(0) >> 1) // max int

	for i := range rt.cfg.Policies {
		p := &rt.cfg.Policies[i]
		if !policyMatches(p, aq, lockChainBlockers) {
			continue
		}
		total := p.WarnCycles + p.CancelCycles
		if total < bestTotal {
			best = p
			bestTotal = total
		}
	}
	return best
}

// policyMatches returns true when at least one of the policy's
// non-zero thresholds is exceeded.
func policyMatches(
	p *config.RunawayPolicy,
	aq *ActiveQuery,
	lockChainBlockers map[int]int,
) bool {
	durationHit := p.MaxDurationMinutes > 0 &&
		aq.Duration >= time.Duration(p.MaxDurationMinutes)*time.Minute
	blockedHit := p.MaxBlockedSessions > 0 &&
		lockChainBlockers[aq.PID] >= p.MaxBlockedSessions
	return durationHit || blockedHit
}

// advanceState transitions a tracked query through the escalation
// ladder: "" -> "warned" -> "cancelled" -> "terminated".
func (rt *RunawayTracker) advanceState(
	tq *TrackedQuery,
	policy *config.RunawayPolicy,
) {
	switch tq.State {
	case "":
		// Need 2+ cycles of observation before warning.
		if rt.cycle-tq.FirstSeenCycle >= 2 {
			tq.State = "warned"
			tq.WarnedAtCycle = rt.cycle
			rt.logFn("WARN", "runaway query warned",
				"pid", tq.PID, "policy", tq.MatchedPolicy)
		}
	case "warned":
		if rt.cycle-tq.WarnedAtCycle >= uint64(policy.WarnCycles) {
			tq.State = "cancelled"
			tq.CancelledAtCycle = rt.cycle
			rt.logFn("WARN", "runaway query escalated to cancel",
				"pid", tq.PID, "policy", tq.MatchedPolicy)
		}
	case "cancelled":
		if rt.cycle-tq.CancelledAtCycle >= uint64(policy.CancelCycles) {
			tq.State = "terminated"
			rt.logFn("ERROR", "runaway query escalated to terminate",
				"pid", tq.PID, "policy", tq.MatchedPolicy)
		}
	}
}

// buildFindings generates one Finding per tracked query that has
// entered an actionable state.
func (rt *RunawayTracker) buildFindings() []analyzer.Finding {
	var findings []analyzer.Finding
	for _, tq := range rt.tracked {
		if tq.State == "" {
			continue
		}
		findings = append(findings, buildRunawayFinding(tq, rt.cycle))
	}
	return findings
}

func buildRunawayFinding(tq *TrackedQuery, cycle uint64) analyzer.Finding {
	elapsed := time.Since(tq.QueryStart)
	severity, sql, risk, narrative := runawayStateParams(tq, elapsed)

	return analyzer.Finding{
		Category:   "runaway_query",
		Severity:   severity,
		ObjectType: "process",
		ObjectIdentifier: fmt.Sprintf(
			"pid:%d:policy:%s", tq.PID, tq.MatchedPolicy),
		Title: fmt.Sprintf(
			"Runaway query PID %d: %s for %s (policy: %s)",
			tq.PID, tq.State, elapsed.Truncate(time.Second), tq.MatchedPolicy),
		Detail: map[string]any{
			"pid":         tq.PID,
			"query":       tq.QueryText,
			"app_name":    tq.AppName,
			"policy":      tq.MatchedPolicy,
			"state":       tq.State,
			"duration_s":  int(elapsed.Seconds()),
			"cycle_count": cycle - tq.FirstSeenCycle,
		},
		Recommendation: narrative,
		RecommendedSQL: sql,
		RollbackSQL:    "",
		ActionRisk:     risk,
	}
}

// runawayStateParams returns severity, SQL, risk, and narrative text
// appropriate for the tracked query's current state.
func runawayStateParams(
	tq *TrackedQuery, elapsed time.Duration,
) (severity, sql, risk, narrative string) {
	switch tq.State {
	case "warned":
		severity = "warning"
		narrative = fmt.Sprintf(
			"Query on PID %d (%s) has been running for %s and matches "+
				"policy %q. Monitoring for escalation.",
			tq.PID, tq.AppName, elapsed.Truncate(time.Second),
			tq.MatchedPolicy)
	case "cancelled":
		severity = "critical"
		sql = fmt.Sprintf("SELECT pg_cancel_backend(%d);", tq.PID)
		risk = "safe"
		narrative = fmt.Sprintf(
			"Query on PID %d (%s) exceeded warning period under "+
				"policy %q. Issuing pg_cancel_backend.",
			tq.PID, tq.AppName, tq.MatchedPolicy)
	case "terminated":
		severity = "critical"
		sql = fmt.Sprintf("SELECT pg_terminate_backend(%d);", tq.PID)
		risk = "moderate"
		narrative = fmt.Sprintf(
			"Query on PID %d (%s) survived cancellation under "+
				"policy %q. Issuing pg_terminate_backend.",
			tq.PID, tq.AppName, tq.MatchedPolicy)
	}
	return
}

// truncateQuery shortens a query string to maxLen characters, appending
// "..." when truncation occurs.
func truncateQuery(q string, maxLen int) string {
	if len(q) <= maxLen {
		return q
	}
	if maxLen <= 3 {
		return q[:maxLen]
	}
	return q[:maxLen-3] + "..."
}
