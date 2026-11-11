package fleet

import (
	"fmt"
	"time"
)

const (
	// staleIntervals is how many collector intervals may pass without a
	// sample before the database's numbers are treated as stale.
	staleIntervals = 3
	// staleScoreCap is the highest score a stale database can have: its
	// finding counts describe the past, not the database today.
	staleScoreCap = 25
	// healthyScoreFloor is the lowest score Summary.Healthy counts.
	healthyScoreFloor = 50
)

// computeHealthScore scores connectivity and open findings: 0 when
// disconnected or erroring, else 100 - 25 per critical - 5 per warning.
func computeHealthScore(s *InstanceStatus) int {
	if !s.Connected {
		return 0
	}
	if s.Error != "" {
		return 0
	}
	score := 100
	score -= s.FindingsCritical * 25
	score -= s.FindingsWarning * 5
	if score < 0 {
		score = 0
	}
	return score
}

// healthOf scores one status snapshot 0-100 and explains the score. A
// collector silent for more than staleIntervals intervals caps the score
// at staleScoreCap. A zero last run counts as stale only once the
// instance has been registered longer than that; an unknown registration
// time or a zero interval leaves staleness unjudged.
func healthOf(
	s *InstanceStatus, interval time.Duration, registeredAt, now time.Time,
) (int, []string) {
	reasons := []string{}
	if r := connectivityReason(s); r != "" {
		reasons = append(reasons, r)
	}
	stale := staleReason(s.CollectorLastRun, interval, registeredAt, now)
	if stale != "" {
		reasons = append(reasons, stale)
	}
	reasons = append(reasons, findingReasons(s)...)
	score := computeHealthScore(s)
	if stale != "" && score > staleScoreCap {
		score = staleScoreCap
	}
	return score, reasons
}

func connectivityReason(s *InstanceStatus) string {
	switch {
	case !s.Connected && s.Error != "":
		return "disconnected: " + s.Error
	case !s.Connected:
		return "disconnected"
	case s.Error != "":
		return "error: " + s.Error
	}
	return ""
}

func staleReason(
	lastRun time.Time, interval time.Duration, registeredAt, now time.Time,
) string {
	if interval <= 0 {
		return ""
	}
	limit := staleIntervals * interval
	if lastRun.IsZero() {
		if registeredAt.IsZero() || now.Sub(registeredAt) <= limit {
			return ""
		}
		return "collector stale: no sample in " +
			formatHealthAge(now.Sub(registeredAt))
	}
	age := now.Sub(lastRun)
	if age <= limit {
		return ""
	}
	return "collector stale " + formatHealthAge(age)
}

// Collection freshness values (InstanceStatus.CollectionFreshness).
const (
	FreshnessFresh   = "fresh"
	FreshnessStale   = "stale"
	FreshnessUnknown = "unknown"
)

// IncidentsUnknown marks incident counts that could not be read; 0 would
// claim there are none.
const IncidentsUnknown = -1

// collectionFreshness judges a database's collection evidence by the same
// rule as staleReason: stale exactly when staleReason gives a reason. A
// database still in its first window without a sample, or one with no
// interval to judge by, is unknown rather than fresh.
func collectionFreshness(
	lastRun time.Time, interval time.Duration, registeredAt, now time.Time,
) string {
	if interval <= 0 {
		return FreshnessUnknown
	}
	if staleReason(lastRun, interval, registeredAt, now) != "" {
		return FreshnessStale
	}
	if lastRun.IsZero() {
		return FreshnessUnknown
	}
	return FreshnessFresh
}

// addCaseCounts adds one scored row's open work and evidence gaps to the
// summary. A row never scored counts as freshness unknown.
func addCaseCounts(sum *FleetSummary, s *InstanceStatus) {
	sum.TotalWarning += s.FindingsWarning
	if s.IncidentsCritical < 0 || s.IncidentsWarning < 0 {
		sum.IncidentsUnknown++
	} else {
		sum.IncidentsCritical += s.IncidentsCritical
		sum.IncidentsWarning += s.IncidentsWarning
	}
	switch s.CollectionFreshness {
	case FreshnessFresh:
	case FreshnessStale:
		sum.StaleCollection++
	default:
		sum.FreshnessUnknown++
	}
}

func findingReasons(s *InstanceStatus) []string {
	var out []string
	switch {
	case s.FindingsCritical == 1:
		out = append(out, "1 critical finding")
	case s.FindingsCritical > 1:
		out = append(out, fmt.Sprintf("%d critical findings", s.FindingsCritical))
	}
	switch {
	case s.FindingsWarning == 1:
		out = append(out, "1 warning")
	case s.FindingsWarning > 1:
		out = append(out, fmt.Sprintf("%d warnings", s.FindingsWarning))
	}
	return out
}

// stopHealthReason explains an emergency stop; it carries no penalty.
func stopHealthReason(by string) string {
	if by == "" {
		return "emergency stopped"
	}
	return "emergency stopped by " + by
}

// formatHealthAge renders an age in its largest whole unit: 59s, 42m,
// 5h, then days from 48 hours on.
func formatHealthAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// collectorIntervalLocked is the period the instance's collector runs at.
// A started collector reports its ticker period, which follows hot reloads
// of its own config and the dormant interval; the manager's config can
// disagree with it. Before the ticker starts it falls back to the
// instance override, else the global interval. Caller holds m.mu.
func (m *DatabaseManager) collectorIntervalLocked(inst *DatabaseInstance) time.Duration {
	if inst.Collector != nil {
		if period := inst.Collector.Interval(); period > 0 {
			return period
		}
	}
	secs := inst.Config.CollectorIntervalSeconds
	if secs <= 0 && m.cfg != nil {
		secs = m.cfg.Collector.IntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// scoreLocked fills snap's health score, reasons and collection
// freshness. Caller holds m.mu.
func (m *DatabaseManager) scoreLocked(
	inst *DatabaseInstance, snap *InstanceStatus, now time.Time,
) {
	interval := m.collectorIntervalLocked(inst)
	score, reasons := healthOf(snap, interval, inst.RegisteredAt, now)
	if inst.Stopped {
		reasons = append(reasons, stopHealthReason(inst.StoppedBy))
	}
	snap.CollectionFreshness = collectionFreshness(snap.CollectorLastRun,
		interval, inst.RegisteredAt, now)
	snap.HealthScore = score
	snap.HealthReasons = reasons
}

// databaseStatusLocked snapshots and scores one instance, the row both
// FleetStatus and InstanceStatus return. Caller holds m.mu.
func (m *DatabaseManager) databaseStatusLocked(
	inst *DatabaseInstance, now time.Time, budgets map[string]BudgetUsage,
) DatabaseStatus {
	// Snapshot fields under the instance lock so that background writers
	// (updateInstanceFindings, config_handlers) don't race with the
	// health-score compute or the JSON marshaller.
	snap := inst.SnapshotStatus()
	snap = EnsureCapabilities(inst, snap)
	m.scoreLocked(inst, snap, now)
	snap.DatabaseName = inst.Name
	ds := DatabaseStatus{
		ID:         inst.DatabaseID,
		DatabaseID: inst.DatabaseID,
		Name:       inst.Name,
		Tags:       cloneTags(inst.Config.Tags),
		Status:     snap,
	}
	applyStopStatus(&ds, inst)
	ds.LLMBudget = budgetFor(budgets, inst.Name)
	return ds
}

// InstanceStatus returns one database's scored status row, the same row
// FleetStatus lists for it, and false when no such database exists.
func (m *DatabaseManager) InstanceStatus(name string) (DatabaseStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst := m.instances[name]
	if inst == nil {
		return DatabaseStatus{}, false
	}
	return m.databaseStatusLocked(inst, time.Now(), m.budgetSnapshotLocked()), true
}

// stampRegistered records when a runtime was published, unless the caller
// already set it. Caller holds m.mu.
func stampRegistered(inst *DatabaseInstance, now time.Time) {
	if inst.RegisteredAt.IsZero() {
		inst.RegisteredAt = now
	}
}
