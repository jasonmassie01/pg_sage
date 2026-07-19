package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
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
	Collector  *collector.Collector
	Analyzer   *analyzer.Analyzer
	Executor   *executor.Executor
	Status     *InstanceStatus
	Stopped    bool
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
	// PoolClose is an internal lifecycle seam used to close the pool.
	// Production instances normally leave it nil, which falls back to
	// Pool.Close. Tests use it to make close ordering observable.
	PoolClose func()
	// teardownTimeout overrides the internal cleanup bound in focused tests.
	// Production instances use defaultInstanceTeardownTimeout.
	teardownTimeout time.Duration

	// statusMu guards reads/writes to the InstanceStatus pointed to by
	// Status. Always access via UpdateStatus / SnapshotStatus.
	statusMu sync.RWMutex
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
	return d.Config.TrustLevelExplicit
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

// SnapshotStatus returns a shallow copy of the current Status taken
// under the read lock. Callers may read and marshal the returned
// pointer without further synchronisation.
func (d *DatabaseInstance) SnapshotStatus() *InstanceStatus {
	d.statusMu.RLock()
	defer d.statusMu.RUnlock()
	if d.Status == nil {
		return &InstanceStatus{}
	}
	cp := *d.Status
	return &cp
}

// RemoveInstance keeps the legacy bool API while bounding its caller.
func (m *DatabaseManager) RemoveInstance(name string) bool {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultInstanceTeardownTimeout,
	)
	defer cancel()
	err := m.RemoveInstanceContext(ctx, name)
	return !errors.Is(err, ErrDatabaseNotFound)
}

// RemoveInstanceContext detaches under the manager lock, then tears down
// the runtime without retaining that lock.
func (m *DatabaseManager) RemoveInstanceContext(
	ctx context.Context,
	name string,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var inst *DatabaseInstance
	err := m.WithLifecycle(ctx, func(*LifecycleMutation) error {
		var ok bool
		inst, ok = m.detachInstance(name)
		if !ok {
			return ErrDatabaseNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	return awaitInstanceTeardown(ctx, inst)
}

// ShutdownInstance drains an unpublished or rejected runtime generation.
// It is primarily used by lifecycle owners that prepared a candidate but
// could not publish it.
func ShutdownInstance(ctx context.Context, inst *DatabaseInstance) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return awaitInstanceTeardown(ctx, inst)
}

func (m *DatabaseManager) detachInstance(
	name string,
) (*DatabaseInstance, bool) {
	m.mu.Lock()
	inst, ok := m.instances[name]
	if ok {
		delete(m.instances, name)
		if m.primaryName == name {
			m.primaryName = m.firstConnectedNameLocked()
		}
	}
	m.mu.Unlock()
	return inst, ok
}

// ReplaceInstance validates a fully built candidate before atomic
// publication, then retires the old generation outside the manager lock.
func (m *DatabaseManager) ReplaceInstance(
	ctx context.Context,
	oldName string,
	candidate *DatabaseInstance,
	healthCheck func(context.Context, *DatabaseInstance) error,
) error {
	return m.ReplaceInstanceIfCurrent(
		ctx, oldName, nil, candidate, healthCheck,
	)
}

// ReplaceInstanceIfCurrent health-checks and publishes candidate only when
// expected is still the active generation. Reconnect callers pass the failed
// instance they observed so a concurrent update cannot be overwritten.
func (m *DatabaseManager) ReplaceInstanceIfCurrent(
	ctx context.Context,
	oldName string,
	expected *DatabaseInstance,
	candidate *DatabaseInstance,
	healthCheck func(context.Context, *DatabaseInstance) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if candidate == nil || candidate.Name == "" || healthCheck == nil {
		return rejectCandidate(ErrInvalidInstance, candidate)
	}
	if err := ctx.Err(); err != nil {
		return rejectCandidate(err, candidate)
	}
	var old *DatabaseInstance
	err := m.WithLifecycle(ctx, func(op *LifecycleMutation) error {
		var validateErr error
		old, validateErr = op.ValidateReplacement(
			oldName, candidate.Name, expected,
		)
		if validateErr != nil {
			return validateErr
		}
		if checkErr := healthCheck(ctx, candidate); checkErr != nil {
			return checkErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return op.PublishReplacement(oldName, old, candidate)
	})
	if err != nil {
		return rejectCandidate(err, candidate)
	}
	return awaitInstanceTeardown(ctx, old)
}

func (m *DatabaseManager) commitReplacement(
	oldName string,
	old, candidate *DatabaseInstance,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.instances[oldName]; current != old {
		return ErrInstanceConflict
	}
	if candidate.Name != oldName {
		if m.instances[candidate.Name] != nil {
			return replacementConflict(candidate.Name)
		}
		delete(m.instances, oldName)
	}
	m.instances[candidate.Name] = candidate
	if m.primaryName == oldName {
		m.primaryName = candidate.Name
		if candidate.Pool == nil {
			m.primaryName = m.firstConnectedNameLocked()
		}
	}
	return nil
}

func replacementConflict(name string) error {
	return fmt.Errorf("%w: target name %q", ErrInstanceConflict, name)
}

func rejectCandidate(err error, candidate *DatabaseInstance) error {
	return errors.Join(err, cleanupRejectedCandidate(candidate))
}

func cleanupRejectedCandidate(candidate *DatabaseInstance) error {
	if candidate == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultInstanceTeardownTimeout,
	)
	defer cancel()
	return awaitInstanceTeardown(ctx, candidate)
}

func awaitInstanceTeardown(
	ctx context.Context,
	inst *DatabaseInstance,
) error {
	if inst == nil {
		return nil
	}
	done := startInstanceTeardown(inst)
	select {
	case <-done:
		inst.lifecycleMu.Lock()
		err := inst.lifecycleErr
		inst.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startInstanceTeardown(inst *DatabaseInstance) <-chan struct{} {
	inst.lifecycleMu.Lock()
	defer inst.lifecycleMu.Unlock()
	if inst.lifecycleStarted {
		return inst.lifecycleDone
	}
	inst.lifecycleStarted = true
	inst.lifecycleDone = make(chan struct{})
	go runInstanceTeardown(inst, inst.lifecycleDone)
	return inst.lifecycleDone
}

func runInstanceTeardown(inst *DatabaseInstance, done chan struct{}) {
	if inst.Cancel != nil {
		inst.Cancel()
	}
	timeout := inst.teardownTimeout
	if timeout <= 0 {
		timeout = defaultInstanceTeardownTimeout
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	workerErr := waitForInstanceWorkers(cleanupCtx, inst.Workers)
	shutdownErr := shutdownInstanceExecutor(cleanupCtx, inst)
	closeErr := closeInstancePool(cleanupCtx, inst)
	lifecycleErr := errors.Join(workerErr, shutdownErr, closeErr)
	inst.lifecycleMu.Lock()
	inst.lifecycleErr = lifecycleErr
	close(done)
	inst.lifecycleMu.Unlock()
}

func waitForInstanceWorkers(
	ctx context.Context, workers *sync.WaitGroup,
) error {
	if workers == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for instance workers: %w", ctx.Err())
	}
}

func shutdownInstanceExecutor(
	ctx context.Context, inst *DatabaseInstance,
) error {
	done := make(chan error, 1)
	go func() {
		if inst.ExecutorShutdown != nil {
			done <- inst.ExecutorShutdown(ctx)
			return
		}
		if inst.Executor != nil {
			done <- inst.Executor.Shutdown(ctx)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("shutting down instance executor: %w", ctx.Err())
	}
}

func closeInstancePool(
	ctx context.Context, inst *DatabaseInstance,
) error {
	done := make(chan struct{})
	go func() {
		if inst.PoolClose != nil {
			inst.PoolClose()
		} else if inst.Pool != nil {
			inst.Pool.Close()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("closing instance pool: %w", ctx.Err())
	}
}

func (m *DatabaseManager) firstConnectedNameLocked() string {
	names := make([]string, 0, len(m.instances))
	for name, inst := range m.instances {
		if inst != nil && inst.Pool != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// InstanceStatus tracks the health of a single database.
type InstanceStatus struct {
	Connected        bool                 `json:"connected"`
	PGVersion        string               `json:"pg_version"`
	Platform         string               `json:"platform"`
	DatabaseSize     int64                `json:"database_size_bytes"`
	TrustLevel       string               `json:"trust_level"`
	CollectorLastRun time.Time            `json:"collector_last_run"`
	AnalyzerLastRun  time.Time            `json:"analyzer_last_run"`
	FindingsOpen     int                  `json:"findings_open"`
	FindingsCritical int                  `json:"findings_critical"`
	FindingsWarning  int                  `json:"findings_warning"`
	FindingsInfo     int                  `json:"findings_info"`
	ActionsTotal     int                  `json:"actions_total"`
	LLMTokensUsed    int                  `json:"llm_tokens_used"`
	AdvisoryLockHeld bool                 `json:"advisory_lock_held"`
	HealthScore      int                  `json:"health_score"`
	Error            string               `json:"error,omitempty"`
	LastSeen         time.Time            `json:"last_seen"`
	DatabaseName     string               `json:"database_name"`
	Capabilities     ProviderCapabilities `json:"capabilities,omitempty"`
}

// FindingRow is a finding as returned from the database.
type FindingRow struct {
	ID               string         `json:"id"`
	CreatedAt        time.Time      `json:"created_at"`
	LastSeen         time.Time      `json:"last_seen"`
	OccurrenceCount  int            `json:"occurrence_count"`
	Category         string         `json:"category"`
	Severity         string         `json:"severity"`
	ObjectType       string         `json:"object_type"`
	ObjectIdentifier string         `json:"object_identifier"`
	Title            string         `json:"title"`
	Detail           map[string]any `json:"detail"`
	Recommendation   string         `json:"recommendation"`
	RecommendedSQL   string         `json:"recommended_sql"`
	RollbackSQL      string         `json:"rollback_sql"`
	Status           string         `json:"status"`
	ResolvedAt       *time.Time     `json:"resolved_at,omitempty"`
	DatabaseName     string         `json:"database_name"`
}

// ActionRow is an action as returned from the database.
type ActionRow struct {
	ID           string    `json:"id"`
	ExecutedAt   time.Time `json:"executed_at"`
	ActionType   string    `json:"action_type"`
	FindingID    *string   `json:"finding_id,omitempty"`
	SQLExecuted  string    `json:"sql_executed"`
	RollbackSQL  string    `json:"rollback_sql,omitempty"`
	BeforeState  string    `json:"before_state,omitempty"`
	AfterState   string    `json:"after_state,omitempty"`
	Outcome      string    `json:"outcome"`
	DatabaseName string    `json:"database_name"`
}

// FindingFilters are query parameters for finding listings.
// Source is a subsystem filter. Accepted values and their mapping to
// sage.findings.category are documented in docs/ui-redesign-v2.md
// §16 and implemented in api.buildFindingsWhere:
//
//	""               — no filter
//	"schema_lint"    — category LIKE 'schema_lint:%'
//	"rules"          — analyzer Tier-1 rule categories
//	"forecaster"     — forecast_* / storage_forecast categories
//	"query_tuning"   — query_tuning / runaway_query / stale_statistics
//	"advisor"        — LLM advisor output (detail->>'subsystem')
//	"optimizer"      — LLM optimizer output (detail->>'subsystem')
//	"migration_advisor" — reserved, not yet emitted
//	"incident"       — incident-class categories
//
// ThematicCategory filters on detail->>'thematic_category' and is
// only meaningful when Source=="schema_lint".
//
// From/To are ISO-8601 timestamps implementing overlapping-window
// semantics on findings: a finding is included when
// created_at <= To AND (resolved_at IS NULL OR resolved_at >= From).
// Both zero → no time filter.
type FindingFilters struct {
	Status           string
	Severity         string
	Category         string
	Source           string
	ThematicCategory string
	Sort             string
	Order            string
	Limit            int
	Offset           int
	From             time.Time
	To               time.Time
}
