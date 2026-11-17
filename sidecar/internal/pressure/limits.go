package pressure

import "github.com/pg-sage/sidecar/internal/policy"

// Limit is one input's bounds: at or past Stop the answer stops, at or
// past Slow it slows (past means below for free storage). A nil bound is
// no band. SlowAtFreezeMax makes the slow bound the server's freeze
// maximum (autovacuum_freeze_max_age or its multixact twin) when that is
// under Stop, else half of Stop.
type Limit struct {
	Slow            *float64
	Stop            *float64
	SlowAtFreezeMax bool
}

// Limits are every input's bounds and the inputs whose being unknown
// stops the answer.
type Limits struct {
	Inputs   map[string]Limit
	Required map[string]bool
}

// Alerts are the alert thresholds the defaults are derived from; a zero
// field is pg_sage's default threshold.
type Alerts struct {
	// ReplicaLagSeconds is the replication-lag alert (RCA
	// replication_lag_threshold_seconds, default 30).
	ReplicaLagSeconds float64
	// XIDWarning is the wraparound warning age (analyzer
	// xid_wraparound_warning, default 500 million); multixact age uses it
	// too.
	XIDWarning float64
	// MinFreeStoragePct is the cloud host guard's free storage floor
	// (default 10%).
	MinFreeStoragePct float64
}

// The alert thresholds pg_sage ships, and the defaults of the inputs no
// alert covers.
const (
	defaultLagAlertSeconds  = 30
	defaultXIDWarning       = 500_000_000
	defaultMinFreeStorage   = 10
	defaultWALSlowPerSecond = 32 << 20
	defaultWALStopPerSecond = 64 << 20
	defaultLockQueueSlow    = 1
	defaultLockQueueStop    = 3
)

// defaultRequired are the inputs plain SQL always reads: one of them
// unknown stops the answer. The WAL rate and free storage are reported
// but not required (free storage needs a cloud adapter).
var defaultRequired = []string{policy.PressureReplicaLag, policy.PressureXIDAge,
	policy.PressureMultixactAge, policy.PressureVacuum, policy.PressureLockQueue}

func ptr(v float64) *float64 { return &v }

func orDefault(v, d float64) float64 {
	if v > 0 {
		return v
	}
	return d
}

// DefaultLimits stop each input at half of its matching alert threshold,
// so the signal stops a job before the alert fires; a floor (free
// storage) stops at twice its alert and slows at three times, at most
// 100%. Replica lag slows at half its stop. Inputs no alert covers: a
// vacuum on the target slows (never stops), the WAL rate slows at 32 MiB/s
// and stops at 64 MiB/s, and the lock queue slows at one waiter and stops
// at three.
func DefaultLimits(a Alerts) Limits {
	lagStop := orDefault(a.ReplicaLagSeconds, defaultLagAlertSeconds) / 2
	xidStop := orDefault(a.XIDWarning, defaultXIDWarning) / 2
	floor := orDefault(a.MinFreeStoragePct, defaultMinFreeStorage)
	return Limits{
		Inputs: map[string]Limit{
			policy.PressureReplicaLag:   {Slow: ptr(lagStop / 2), Stop: ptr(lagStop)},
			policy.PressureXIDAge:       {Stop: ptr(xidStop), SlowAtFreezeMax: true},
			policy.PressureMultixactAge: {Stop: ptr(xidStop), SlowAtFreezeMax: true},
			policy.PressureVacuum:       {Slow: ptr(1)},
			policy.PressureWALRate: {Slow: ptr(defaultWALSlowPerSecond),
				Stop: ptr(defaultWALStopPerSecond)},
			policy.PressureLockQueue: {Slow: ptr(defaultLockQueueSlow),
				Stop: ptr(defaultLockQueueStop)},
			policy.PressureFreeStorage: {Slow: ptr(min(100, 3*floor)),
				Stop: ptr(min(100, 2*floor))},
		},
		Required: setOf(defaultRequired),
	}
}

// LimitsFromPolicy is DefaultLimits with the policy's pressure block on
// top: each bound it sets replaces the default, and its required list,
// when present, replaces the default set.
func LimitsFromPolicy(p policy.PressurePolicy, a Alerts) Limits {
	l := DefaultLimits(a)
	for name, set := range p.Limits {
		cur := l.Inputs[name]
		if set.Slow != nil {
			cur.Slow, cur.SlowAtFreezeMax = ptr(*set.Slow), false
		}
		if set.Stop != nil {
			cur.Stop = ptr(*set.Stop)
		}
		l.Inputs[name] = cur
	}
	if p.Required != nil {
		l.Required = setOf(p.Required)
	}
	return l
}

func setOf(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// units are each input's unit in an answer.
var units = map[string]string{
	policy.PressureReplicaLag:   "s",
	policy.PressureXIDAge:       "xids",
	policy.PressureMultixactAge: "multixacts",
	policy.PressureVacuum:       "vacuums",
	policy.PressureWALRate:      "bytes/s",
	policy.PressureLockQueue:    "waiters",
	policy.PressureFreeStorage:  "%",
}
