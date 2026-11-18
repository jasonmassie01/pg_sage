package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/pressure"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// DatabaseInstance holds the runtime state for a single managed database.
// Direct access to Status fields is NOT safe under concurrent use; callers
// must use UpdateStatus (writers) and SnapshotStatus (readers) so the
// statusMu guards them. See gemini_review.md MEDIUM #73.
type DatabaseInstance struct {
	Name       string
	DatabaseID int
	Config     config.DatabaseConfig
	Pool       *pgxpool.Pool
	// ActionPool is the separate action identity (action_dsn); nil when one
	// role does everything. Closed with Pool.
	ActionPool *pgxpool.Pool
	Collector  *collector.Collector
	Analyzer   *analyzer.Analyzer
	Executor   *executor.Executor
	// HA is the database's one HA monitor (fleet-04): the HA loop and the
	// executor check it, every other consumer reads its snapshot. Nil
	// without a pool, which every consumer treats as not writable.
	HA *ha.Monitor
	// Investigations is the Sage SRE investigator of this database: the
	// API and MCP tools read investigations through it. Nil when the
	// investigator could not be built.
	Investigations *sre.Service
	// SLO evaluates this database's SLOs (error-budget state, burn
	// triggers, pushed SLIs); Changes is its change feed. Nil when off.
	SLO     *slo.Engine
	Changes *changefeed.Feed
	// Actions proposes, hands off and verifies this database's Sage SRE
	// actions (M5). Nil when the action service could not be built.
	Actions *action.ActionService
	Status  *InstanceStatus
	// InstanceOwner reports who manages this database's instance settings
	// (advisors-08); nil outside fleet mode.
	InstanceOwner func() InstanceOwnership
	// DiskCapacity resolves the database's disk capacity for growth
	// forecasts (forecast-06): the provider's when fresh, else the
	// declared one. Nil reads as undeclared.
	DiskCapacity func(context.Context) forecaster.Capacity
	// CapabilityRefresh asks the runtime's capability refresher for a
	// prompt recollection without blocking; nil when none runs.
	CapabilityRefresh func()
	// PlanCaps reads the plan caps the database's standing policy sets (its
	// plans block, plan-02b), which plan approvals are checked against;
	// nil reads the defaults.
	PlanCaps func(context.Context) (policy.PlanCaps, error)
	// Pressure answers check_pressure for this database (guard-01a); nil
	// without a pool, which the route and the MCP tool answer as
	// unavailable.
	Pressure *pressure.Checker
	// PressureJobs registers the database's backfill jobs (guard-01b);
	// nil without a pool, which the job routes answer as unavailable.
	PressureJobs pressure.JobRegistry
	// FirstLookRecheck re-runs the first look on request; nil without one.
	FirstLookRecheck func(context.Context) (firstlook.Report, error)
	Stopped          bool
	// StoppedBy and StoppedAt attribute the active emergency stop. Like
	// Stopped, they are guarded by the manager lock.
	StoppedBy string
	StoppedAt time.Time
	// RegisteredAt is when the runtime was published to the manager; a
	// collector that never sampled is stale only once it is old enough.
	// Guarded by the manager lock.
	RegisteredAt time.Time
	// stopsPending counts stops latched in memory whose durable flag is
	// not yet written; an overlapping resume must not release them.
	stopsPending int
	// stopUnsaved marks a latched stop whose durable flag could not be
	// written: the reconcile retries the write and never releases it.
	// restoredFleet marks a runtime restored with emergency_stop_scope
	// 'fleet'. Both are guarded by the manager lock.
	stopUnsaved   bool
	restoredFleet bool
	// Cancel stops the per-instance goroutines (collector, analyzer,
	// orchestrator). Set by bootstrap code; called by RemoveInstance.
	// EmergencyStop does not call Cancel because monitoring should
	// continue and Resume must not need to rebuild goroutines.
	Cancel context.CancelFunc
	// Workers tracks every goroutine owned by this instance. Lifecycle
	// teardown waits for the group after cancellation and executor
	// shutdown, without retaining the manager lock.
	Workers *sync.WaitGroup
	// ExecutorShutdown is the instance-owned executor teardown hook.
	// Bootstrap code should set it to Executor.Shutdown. When it is nil,
	// lifecycle teardown falls back to Executor.Shutdown directly.
	ExecutorShutdown func(context.Context) error
	// Quiesce drains in-flight actions before Cancel, bounded by
	// DrainTimeout, and returns the release to call once cancelled. Nil
	// skips the drain.
	Quiesce func(context.Context) (func(), error)
	// DrainTimeout bounds Quiesce; zero uses defaultInstanceDrainTimeout.
	DrainTimeout time.Duration
	// PoolClose is an internal lifecycle seam used to close the pool.
	// Production instances normally leave it nil, which falls back to
	// Pool.Close. Tests use it to make close ordering observable.
	PoolClose func()
	// teardownTimeout overrides the internal cleanup bound in focused tests.
	// Production instances use defaultInstanceTeardownTimeout.
	teardownTimeout time.Duration

	// LogSource reports the live log-source status; nil reports disabled.
	LogSource func() logwatch.SourceStatus

	// statusMu guards reads/writes to the InstanceStatus pointed to by
	// Status. Always access via UpdateStatus / SnapshotStatus.
	statusMu sync.RWMutex
	// configMu (leaf) guards Config swaps; writers also hold m.mu, so code
	// under m.mu reads Config directly and everything else ConfigSnapshot.
	configMu sync.RWMutex
	// trustPolicyMu protects the runtime source bit for per-database trust.
	trustPolicyMu       sync.RWMutex
	trustOverrideKnown  bool
	trustOverrideActive bool
	// lifecycleMu protects single-owner asynchronous teardown state.
	// Teardown may outlive a caller whose lifecycle context expires.
	lifecycleMu      sync.Mutex
	lifecycleStarted bool
	lifecycleDone    chan struct{}
	lifecycleErr     error
}

// HasTrustLevelOverride reports whether global trust changes must leave this
// database's executor policy untouched.
func (d *DatabaseInstance) HasTrustLevelOverride() bool {
	d.trustPolicyMu.RLock()
	defer d.trustPolicyMu.RUnlock()
	if d.trustOverrideKnown {
		return d.trustOverrideActive
	}
	return d.ConfigSnapshot().TrustLevelExplicit
}

// SetTrustLevelOverride records the runtime source of effective trust.
func (d *DatabaseInstance) SetTrustLevelOverride(active bool) {
	d.trustPolicyMu.Lock()
	d.trustOverrideKnown = true
	d.trustOverrideActive = active
	d.trustPolicyMu.Unlock()
}

// UpdateStatus runs mutator with the underlying *InstanceStatus held
// under a write lock. Safe under concurrency.
func (d *DatabaseInstance) UpdateStatus(mutator func(*InstanceStatus)) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	if d.Status == nil {
		d.Status = &InstanceStatus{}
	}
	mutator(d.Status)
}

// SnapshotStatus returns a shallow copy of Status and the collector's latest
// in-memory observation. Collection runs independently of analyzer/status
// refreshes, so a completed sample must be visible before the next analysis.
func (d *DatabaseInstance) SnapshotStatus() *InstanceStatus {
	d.statusMu.RLock()
	var cp InstanceStatus
	if d.Status != nil {
		cp = *d.Status
	}
	d.statusMu.RUnlock()
	cp.LogSource = d.logSourceStatus()
	applyHASnapshot(&cp, d.HA)
	if d.Collector != nil {
		health := d.Collector.Status()
		cp.Collector = &health
		sample := d.Collector.LatestSnapshot()
		if sample != nil && !sample.CollectedAt.IsZero() && !sample.CollectedAt.Before(cp.CollectorLastRun) {
			cp.CollectorLastRun = sample.CollectedAt
			cp.DatabaseSize = sample.System.DBSizeBytes
		}
	}
	return &cp
}

// logSourceStatus is the live log-source status, never a zero value.
func (d *DatabaseInstance) logSourceStatus() logwatch.SourceStatus {
	var st logwatch.SourceStatus
	if d.LogSource != nil {
		st = d.LogSource()
	}
	return st.OrDisabled("no log source is wired for this database")
}

// InstanceStatus tracks the health of a single database.
type InstanceStatus struct {
	Connected        bool      `json:"connected"`
	PGVersion        string    `json:"pg_version"`
	Platform         string    `json:"platform"`
	DatabaseSize     int64     `json:"database_size_bytes"`
	TrustLevel       string    `json:"trust_level"`
	CollectorLastRun time.Time `json:"collector_last_run"`
	AnalyzerLastRun  time.Time `json:"analyzer_last_run"`
	FindingsOpen     int       `json:"findings_open"`
	FindingsCritical int       `json:"findings_critical"`
	FindingsWarning  int       `json:"findings_warning"`
	FindingsInfo     int       `json:"findings_info"`
	// Unresolved incidents; IncidentsUnknown (-1) when unread (cases-11).
	IncidentsCritical   int    `json:"incidents_critical"`
	IncidentsWarning    int    `json:"incidents_warning"`
	CollectionFreshness string `json:"collection_freshness"` // fresh, stale or unknown
	ActionsTotal        int    `json:"actions_total"`
	LLMTokensUsed       int    `json:"llm_tokens_used"`
	HealthScore         int    `json:"health_score"`
	// HealthReasons explains HealthScore ("collector stale 42m",
	// "2 critical findings"); set only on scored snapshots.
	HealthReasons []string             `json:"health_reasons"`
	Error         string               `json:"error,omitempty"`
	LastSeen      time.Time            `json:"last_seen"`
	DatabaseName  string               `json:"database_name"`
	Capabilities  ProviderCapabilities `json:"capabilities,omitempty"`
	// HARole, HASafeMode and HALastChange are the HA monitor's last check
	// (ha_status.go); filled when the status is read, never stored.
	HARole       string     `json:"ha_role"`
	HASafeMode   bool       `json:"ha_safe_mode"`
	HALastChange *time.Time `json:"ha_last_change,omitempty"`
	// LogSource is the log-based RCA source of this database (rca-09); in
	// fleet mode it is its cluster's watcher.
	LogSource logwatch.SourceStatus `json:"log_source"`
	// Collector is the collector's health (breaker state, unavailable
	// categories, failure counts); nil when the database has none.
	Collector *collector.Status `json:"collector,omitempty"`
}
