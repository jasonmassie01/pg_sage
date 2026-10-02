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
	"time"

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
// starts warning: 2^31 - 1 - 40,000,000. It is the runway's limit; the
// server stops assigning XIDs 3,000,000 before 2^31.
const WraparoundWarnAge = 2147483647 - 40000000

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
	Lookback    time.Duration
	MinSamples  int
	MinSpan     time.Duration
	Retention   time.Duration
	// Horizons: a runway inside the horizon opens a finding, inside the
	// critical horizon a critical one.
	WraparoundHorizon  time.Duration
	WraparoundCritical time.Duration
	DiskHorizon        time.Duration
	DiskCritical       time.Duration
	SequenceHorizon    time.Duration
	SequenceCritical   time.Duration
	// DiskCapacityBytes is the operator's declared capacity (0: unknown).
	DiskCapacityBytes float64
	// WALRetainedLimitBytes is a slot's limit while max_slot_wal_keep_size
	// is unbounded: the WAL custodian's retained-WAL ceiling.
	WALRetainedLimitBytes float64
	// Sizes shares the databases' total size between the runtimes of one
	// process, one measurement per cluster per pass (nil: this runtime
	// measures it every pass).
	Sizes *SizeShare
}

// ProbeRunner runs one catalog probe (probes.Runner).
type ProbeRunner interface {
	Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result
}
