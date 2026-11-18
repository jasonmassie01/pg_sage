// Package runway is Sage SRE's runway monitor (AI-SRE-SPEC §4 R2): on the
// collector tick it samples the series a hard limit is approached along
// (XID and multixact counters, WAL position, retained WAL per slot,
// database and disk usage, sequences) into sage.runway_samples, projects
// each measured trend against its limit, opens a forecast finding when a
// runway crosses its horizon and starts one read-only pre-incident
// investigation per finding and severity. It also decides avoided-
// incident credit for a WAL bound from the measured fill trend. Nothing
// here executes an action.
package runway

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Forecast finding categories (Cases shows them as forecasts).
const (
	CategoryWraparound = "forecast_wraparound_runway"
	CategoryWAL        = "forecast_wal_runway"
	CategorySequence   = "forecast_sequence_runway"
)

// Finding severities.
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// WraparoundWarnAge is the XID (and multixact) age at which PostgreSQL
// starts warning (freeze.WraparoundWarnAge). It is the runway's limit.
const WraparoundWarnAge = freeze.WraparoundWarnAge

// Overdue freezing (product defaults): a table this far past its
// effective freeze maximum should have been frozen by anti-wraparound
// autovacuum already; twice its maximum is critical.
const (
	OverdueRatio         = 1.25
	OverdueCriticalRatio = 2.0
)

// Steady level series (disk usage, retained WAL) need this fit before a
// projection is believed; a sawtooth projects nothing.
const levelMinR2 = 0.5

// Options are one database's runway settings.
type Options struct {
	// Database is the instance name findings and cases use.
	Database    string
	Investigate bool
	Interval    time.Duration
	// SequenceInterval is the sequences' own, slower sampling period (their
	// consumption is slow and reading them is the costliest probe); 0
	// reads them every tick.
	SequenceInterval time.Duration
	Lookback         time.Duration
	MinSamples       int
	MinSpan          time.Duration
	Retention        time.Duration
	// SizeInterval is the databases' total size's own, slower measuring
	// period (summing every database's size stats every file; the disk
	// runway forecasts over hours); between measurements the last one is
	// reused and not sampled again. 0 measures it every tick.
	SizeInterval time.Duration
	// MXMemberInterval is the multixact member space's own sampling
	// period (guard-02a; its rate moves with checkpoints); between readings
	// the last one is kept. 0 reads it every tick.
	MXMemberInterval time.Duration
	// Horizons: a runway inside the horizon opens a finding, inside the
	// critical horizon a critical one.
	WraparoundHorizon  time.Duration
	WraparoundCritical time.Duration
	DiskHorizon        time.Duration
	DiskCritical       time.Duration
	SequenceHorizon    time.Duration
	SequenceCritical   time.Duration
	// WraparoundGateMargin lowers the wraparound gate below the effective
	// freeze maximum by this share (0-MaxWraparoundGateMargin, default 0;
	// no configuration key): an xid or mxid trend is projected only at or
	// past freeze maximum x (1 - margin).
	WraparoundGateMargin float64
	// DiskCapacityBytes is the operator's declared capacity (0: unknown).
	DiskCapacityBytes float64
	// Capacity is the provider's measured storage capacity (managed-cloud
	// telemetry), preferred over DiskCapacityBytes while it is fresh; nil
	// without telemetry. ok is false without a fresh bounded value; source
	// is then CapacityUnbounded for storage that grows without a bound.
	Capacity CapacitySource
	// WALRetainedLimitBytes is a slot's limit while max_slot_wal_keep_size
	// is unbounded: the WAL custodian's retained-WAL ceiling.
	WALRetainedLimitBytes float64
	// Sizes shares the databases' total size between the runtimes of one
	// process, one measurement per cluster per SizeInterval (nil: a
	// monitor keeps its own when SizeInterval is set, else it measures
	// every tick).
	Sizes *SizeShare
	// Owners elects, per cluster, the one runtime of the process that
	// reports the cluster's series (forecast-09); nil: this monitor
	// reports them for its database, as a single database does.
	Owners *ClusterOwners
}

// ProbeRunner runs one catalog probe (probes.Runner).
type ProbeRunner interface {
	Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result
	// RunBackground runs a probe with its background budget (slow-cadence
	// sampling).
	RunBackground(ctx context.Context, id probes.ID, args probes.Args) probes.Result
}

// validate checks the options a monitor needs.
func (o Options) validate() error {
	if o.Interval <= 0 || o.Lookback <= 0 || o.Retention < o.Lookback || o.MinSamples < 3 {
		return fmt.Errorf("runway monitor options are invalid: %+v", o)
	}
	if o.SequenceInterval < 0 || (o.SequenceInterval > 0 && o.SequenceInterval < o.Interval) {
		return fmt.Errorf("runway monitor sequence interval %s must be 0 (every tick) or "+
			"at least the interval %s", o.SequenceInterval, o.Interval)
	}
	if !(o.WraparoundGateMargin >= 0 && o.WraparoundGateMargin <= MaxWraparoundGateMargin) {
		return fmt.Errorf("runway monitor wraparound gate margin %v must be 0-%v",
			o.WraparoundGateMargin, MaxWraparoundGateMargin)
	}
	if o.MXMemberInterval < 0 || (o.MXMemberInterval > 0 && o.MXMemberInterval < o.Interval) {
		return fmt.Errorf("runway monitor multixact member interval %s must be 0 (every "+
			"tick) or at least the interval %s", o.MXMemberInterval, o.Interval)
	}
	if o.SizeInterval < 0 || (o.SizeInterval > 0 && o.SizeInterval < o.Interval) {
		return fmt.Errorf("runway monitor size interval %s must be 0 (every tick) or "+
			"at least the interval %s", o.SizeInterval, o.Interval)
	}
	return nil
}
