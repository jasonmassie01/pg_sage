package srebench

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// RulesOnly is the trivial "deterministic rules only" baseline (AI-SRE-
// SPEC §12): one first-match rule list per family over the evidence the
// causal graph read, with naive pooled counts and no contradictions,
// per-subject attribution or amplification. Its thresholds mirror the
// graph's so the comparison isolates what the graph's structure adds.
type RulesOnly struct{}

// Rule thresholds.
const (
	ruleBacklogWaiting = 3
	ruleFanOutIdle     = 10
	ruleSurgeRate      = 4 << 20 // bytes per second
	rulePlanRatio      = 1.5
)

// Name implements DerivedArm.
func (RulesOnly) Name() string { return ArmRulesOnly }

// Derive implements DerivedArm: the same probes, at the same time, as
// the live arm whose evidence it reads.
func (RulesOnly) Derive(sc Scenario, t Trace) Outcome {
	o := Outcome{State: sre.StateInconclusive, ProbeCount: t.Outcome.ProbeCount,
		Measured: t.Outcome.Measured, FirstEvidence: t.Outcome.FirstEvidence,
		Packet: t.Outcome.Packet}
	var root string
	switch sc.Family {
	case sre.TriggerLock:
		root = lockRule(t.Evidence)
	case sre.TriggerConnections:
		root = connRule(t.Evidence)
	case sre.TriggerWAL:
		root = walRule(t.Evidence)
	case sre.TriggerPlan:
		root = planRule(t.Evidence, sc.Subject)
	case sre.TriggerWraparound:
		root = wrapRule(t.Evidence)
	case sre.TriggerDiskWAL:
		root = diskRule(t.Evidence)
	case sre.TriggerSequence:
		root = seqRule(t.Evidence, sc.Subject)
	}
	if root != "" {
		o.State, o.Root, o.Ranked = sre.StateConcluded, root, []string{root}
	}
	return o
}

// series is the usable results of one probe in time order.
func series(ev []probes.Result, id probes.ID) []probes.Result {
	var out []probes.Result
	for _, r := range ev {
		if r.ProbeID == id && r.Status.Usable() {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out
}

func lockRule(ev []probes.Result) string {
	s := series(ev, probes.LockGraph)
	if len(s) == 0 {
		return ""
	}
	edges, err := probes.LockEdges(s[len(s)-1])
	if err != nil || len(edges) == 0 {
		return ""
	}
	has := func(f func(probes.LockEdge) bool) bool {
		for _, e := range edges {
			if f(e) {
				return true
			}
		}
		return false
	}
	switch {
	case has(func(e probes.LockEdge) bool { return e.BlockerKind == "prepared_xact" }):
		return "prepared_xact_holder"
	case has(probes.LockEdge.StrongRelationWait):
		return "ddl_lock_queue"
	case has(func(e probes.LockEdge) bool {
		return strings.HasPrefix(e.BlockerState, "idle in transaction")
	}):
		return "idle_in_tx_holder"
	}
	return "hot_row_contention"
}

// connCounts is one sample's idle and lock-waiting backends of this
// database, pooled across applications.
func connCounts(r probes.Result) (idle, waiting int64, ok bool) {
	gs, err := probes.ConnectionGroups(r)
	if err != nil {
		return 0, 0, false
	}
	for _, g := range gs {
		if !g.InCurrentDatabase {
			continue
		}
		waiting += g.WaitingOnLock
		if g.State == "idle" {
			idle += g.Backends
		}
	}
	return idle, waiting, true
}

func connRule(ev []probes.Result) string {
	s := series(ev, probes.ConnectionSaturation)
	if len(s) == 0 {
		return ""
	}
	idle, waiting, ok := connCounts(s[len(s)-1])
	if !ok {
		return ""
	}
	firstIdle, _, firstOK := connCounts(s[0])
	switch {
	case waiting >= ruleBacklogWaiting:
		return "blocked_backlog"
	case len(s) > 1 && firstOK && idle > firstIdle:
		return "connection_leak"
	case idle >= ruleFanOutIdle:
		return "pool_fan_out"
	}
	return ""
}

func anySlot(slots []probes.Slot, active bool) bool {
	for _, sl := range slots {
		if sl.Active == active {
			return true
		}
	}
	return false
}

func walRule(ev []probes.Result) string {
	if s := series(ev, probes.ReplicationSlots); len(s) > 0 {
		slots, err := probes.Slots(s[len(s)-1])
		switch {
		case err != nil:
		case anySlot(slots, false):
			return "inactive_slot"
		case anySlot(slots, true):
			return "slow_consumer"
		}
	}
	if s := series(ev, probes.Archiver); len(s) > 0 {
		a, err := probes.ArchiverStats(s[len(s)-1])
		if err == nil && !a.LastFailedAt.IsZero() && a.LastFailedAt.After(a.LastArchivedAt) {
			return "archiver_failure"
		}
	}
	if walRate(series(ev, probes.WALCheckpoint)) >= ruleSurgeRate {
		return "write_surge"
	}
	return ""
}

// walRate is the WAL bytes per second between the first and last
// samples; 0 when it cannot be measured.
func walRate(s []probes.Result) float64 {
	if len(s) < 2 {
		return 0
	}
	a, errA := probes.WALStats(s[0])
	b, errB := probes.WALStats(s[len(s)-1])
	span := s[len(s)-1].ObservedAt.Sub(s[0].ObservedAt).Seconds()
	if errA != nil || errB != nil || span <= 0 || !probes.Known(a.WALBytes) ||
		!probes.Known(b.WALBytes) {
		return 0
	}
	return (b.WALBytes - a.WALBytes) / span
}

func planRule(ev []probes.Result, subject string) string {
	s := series(ev, probes.PlanRegressions)
	if len(s) == 0 {
		return ""
	}
	shifts, err := probes.PlanShifts(s[len(s)-1])
	if err != nil {
		return ""
	}
	for _, sh := range shifts {
		if fmt.Sprintf("queryid %d", sh.QueryID) != subject {
			continue
		}
		switch {
		case sh.Flipped:
			return "plan_flip_regression"
		case sh.Ratio() >= rulePlanRatio:
			return "same_plan_latency_regression"
		}
	}
	return ""
}
