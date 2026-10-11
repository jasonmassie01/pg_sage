package executor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// EventDispatcher sends notification events. Nil means no
// notifications (backward-compatible default).
type EventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// ActionProposer defines the subset of ActionStore the executor needs.
// Nil means auto mode (no queueing).
type ActionProposer interface {
	Propose(ctx context.Context, databaseID *int,
		findingID int, sql, rollbackSQL, risk string) (int, error)
}

type ActionMetadataProposer interface {
	ProposeWithMetadata(ctx context.Context, databaseID *int,
		findingID int, sql, rollbackSQL, risk string,
		meta store.ActionProposalMetadata) (int, error)
}

// PendingActionChecker is implemented by stores that can detect an existing
// unresolved proposal for the same finding.
type PendingActionChecker interface {
	HasPendingForFinding(ctx context.Context, findingID int) (bool, error)
}

// PendingActionSQLChecker is implemented by stores that can detect an
// unresolved proposal with the same SQL text.
type PendingActionSQLChecker interface {
	HasPendingForSQL(ctx context.Context, sql string) (bool, error)
}

// RejectedActionChecker is implemented by stores that can detect a recent
// operator rejection. Approval mode uses this to avoid immediately
// re-proposing an action the user just rejected.
type RejectedActionChecker interface {
	HasRecentlyRejectedForFinding(
		ctx context.Context, findingID int, cooldown time.Duration,
	) (bool, error)
	HasRecentlyRejectedForSQL(
		ctx context.Context, sql string, cooldown time.Duration,
	) (bool, error)
}

// maxConcurrentDDL limits how many DDL operations can run
// in parallel to avoid overwhelming the database.
const maxConcurrentDDL = 3

// Executor runs the autonomous remediation loop after each
// analyzer cycle.
type Executor struct {
	pool               *pgxpool.Pool
	cfg                *config.Config
	recs               *recommendation.Store // durable recommendations (C07)
	rampStart          time.Time
	recentMu           sync.Mutex
	recentActions      map[string]time.Time
	logFn              func(string, string, ...any)
	actionStore        ActionProposer
	execMode           string // auto, approval, manual
	dispatcher         EventDispatcher
	databaseName       string
	databaseID         *int
	settingWait        time.Duration // bounds reloaded-setting post-checks (0 = default)
	trustLevelOverride string
	ddlSem             chan struct{}   // limits concurrent DDL ops
	quiesced           atomic.Int32    // DDL slots a drain holds (Quiesce)
	analyzeSem         chan struct{}   // shared fleet-wide for ANALYZE
	justifier          ActionJustifier // optional LLM action justification (C4)
	policyMu           sync.RWMutex
	executorDisabled   bool
	emergencyStopFn    func(context.Context) bool
	policyGate         policy.Gate
	autonomy           policy.AutonomyLimiter // earned-autonomy ledger (M7)
	facts              policy.FactBinder      // confirmed facts (roadmap 2.3)
	agentDecider       policy.AgentDecider    // agent governance (spec §6.2)
	principalHold      PrincipalHold          // §6.2.7 cross-database re-check
	managedConfig      ManagedConfigAdapter
	indexVerification  *verifiedIndexLifecycle
	hostCPU            HostCPUReader
	ioEvidence         IOEvidenceReader
	retainedCleanupMu  sync.Mutex
	resumeOnce         sync.Once
	leaseQueueCfg      policy.LeaseQueueConfig // serialize_mode queue bounds
	postDDLMu          sync.RWMutex
	postDDLHook        func(context.Context) error
	// approvedRunner runs approved queue items it owns (Sage SRE M5).
	approvedRunner approvedRunnerSlot
	// replaceWatch and replaceHooks: the replace action's verification
	// watches and its test seams (roadmap 2.3).
	replaceWatch replaceWatchSet
	replaceHooks replaceHooks

	// monitors tracks background MonitorAndRollback goroutines so
	// Shutdown can wait for them. shutdownCh is closed to signal
	// monitors to abort their rollback window early.
	monitors     sync.WaitGroup
	shutdownCh   chan struct{}
	shutdownMu   sync.Mutex
	shuttingDown bool
}

// WithAnalyzeSemaphore wires a shared process-wide semaphore
// used to serialize ANALYZE actions across every database
// managed by the sidecar. nil is safe and disables gating.
func (e *Executor) WithAnalyzeSemaphore(sem chan struct{}) {
	e.analyzeSem = sem
}

// WithPolicyGate installs the standing-policy gate. Once installed, every
// background candidate is authorized exclusively by this gate.
func (e *Executor) WithPolicyGate(gate policy.Gate) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.policyGate = gate
}

// WithManagedConfigAdapter installs the provider-specific configuration
// integration used instead of ALTER SYSTEM on managed PostgreSQL services.
// A managed configuration change fails closed when no adapter is installed.
func (e *Executor) WithManagedConfigAdapter(adapter ManagedConfigAdapter) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.managedConfig = adapter
}

func (e *Executor) WithPostDDLHook(hook func(context.Context) error) {
	e.postDDLMu.Lock()
	defer e.postDDLMu.Unlock()
	e.postDDLHook = hook
}

// StandingPolicyGate returns the exact gate used by autonomous executor work.
// External intent surfaces must reuse this instance instead of recreating
// authorization logic.
func (e *Executor) StandingPolicyGate() policy.Gate {
	if e == nil {
		return nil
	}
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	return e.policyGate
}

// New creates a new Executor. Its candidates are the durable
// recommendations in pool's sage schema.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	rampStart time.Time,
	logFn func(string, string, ...any),
) *Executor {
	executor := &Executor{
		pool:          pool,
		cfg:           cfg,
		recs:          newRecommendationStore(pool),
		rampStart:     rampStart,
		recentActions: make(map[string]time.Time),
		logFn:         logFn,
		execMode:      "auto",
		ddlSem:        make(chan struct{}, maxConcurrentDDL),
		shutdownCh:    make(chan struct{}),
		emergencyStopFn: func(ctx context.Context) bool {
			return CheckEmergencyStop(ctx, pool)
		},
	}
	executor.configureIndexVerification()
	return executor
}

// Shutdown signals in-flight rollback monitors to abort their
// wait window and waits until they have returned (or the
// provided context is done, whichever comes first). After
// Shutdown returns, RunCycle must not be called again.
func (e *Executor) Shutdown(ctx context.Context) error {
	e.shutdownMu.Lock()
	if !e.shuttingDown {
		e.shuttingDown = true
		close(e.shutdownCh)
	}
	e.shutdownMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.monitors.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startRollbackMonitor registers a background monitor only while the
// executor still accepts work. Holding shutdownMu across WaitGroup.Add makes
// Add mutually exclusive with Shutdown starting Wait, as required by the
// sync.WaitGroup contract.
func (e *Executor) startRollbackMonitor(run func()) bool {
	if run == nil {
		return false
	}
	e.shutdownMu.Lock()
	defer e.shutdownMu.Unlock()
	if e.shuttingDown {
		return false
	}
	e.monitors.Add(1)
	go func() {
		defer e.monitors.Done()
		run()
	}()
	return true
}

// WithActionStore sets the action store and execution mode.
// This enables approval/manual mode queueing.
func (e *Executor) WithActionStore(
	as ActionProposer, mode string,
) {
	e.actionStore = as
	if mode != "" {
		e.SetExecutionMode(mode)
	}
}

// WithDispatcher sets the notification dispatcher. Nil is safe
// and means no notifications are sent (default).
func (e *Executor) WithDispatcher(d EventDispatcher) {
	e.dispatcher = d
}

// WithDatabaseName sets the database name included in events.
func (e *Executor) WithDatabaseName(name string) {
	e.databaseName = name
}

// validTrustLevels enumerates the accepted trust level strings.
// Empty string is allowed to clear an override.
var validTrustLevels = map[string]bool{
	"observation": true,
	"advisory":    true,
	"autonomous":  true,
	"":            true,
}

// SetTrustLevel overrides the global trust level for this
// executor instance. Empty string clears the override.
// Returns an error if the level is not recognized.
func (e *Executor) SetTrustLevel(level string) error {
	if !validTrustLevels[level] {
		return fmt.Errorf("invalid trust level: %q", level)
	}
	e.policyMu.Lock()
	e.trustLevelOverride = level
	e.policyMu.Unlock()
	return nil
}

// TrustLevel returns the effective trust level for this executor.
func (e *Executor) TrustLevel() string {
	e.policyMu.RLock()
	override := e.trustLevelOverride
	e.policyMu.RUnlock()
	if override != "" {
		return override
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return e.cfg.Trust.Level
}

// SetExecutionMode changes the execution mode at runtime.
func (e *Executor) SetExecutionMode(mode string) {
	e.policyMu.Lock()
	e.execMode = mode
	e.policyMu.Unlock()
}

// ExecutionMode returns the current execution mode.
func (e *Executor) ExecutionMode() string {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	return e.execMode
}

// SetExecutorEnabled applies the per-database executor hard gate.
func (e *Executor) SetExecutorEnabled(enabled bool) {
	e.policyMu.Lock()
	e.executorDisabled = !enabled
	e.policyMu.Unlock()
}

// ExecutorEnabled reports whether mutation and queueing are enabled.
func (e *Executor) ExecutorEnabled() bool {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	return !e.executorDisabled
}

// effectiveExecMode returns the explicitly configured mode. Trust is an
// independent ceiling and never promotes manual mode to auto.
func (e *Executor) effectiveExecMode() string {
	return e.ExecutionMode()
}
