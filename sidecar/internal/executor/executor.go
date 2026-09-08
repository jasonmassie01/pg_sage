package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
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
	analyzer           *analyzer.Analyzer
	rampStart          time.Time
	recentActions      map[string]time.Time
	logFn              func(string, string, ...any)
	actionStore        ActionProposer
	execMode           string // auto, approval, manual
	dispatcher         EventDispatcher
	databaseName       string
	trustLevelOverride string
	ddlSem             chan struct{}   // limits concurrent DDL ops
	analyzeSem         chan struct{}   // shared fleet-wide for ANALYZE
	justifier          ActionJustifier // optional LLM action justification (C4)
	policyMu           sync.RWMutex
	executorDisabled   bool
	emergencyStopFn    func(context.Context) bool
	policyGate         policy.Gate
	managedConfig      ManagedConfigAdapter
	indexVerification  *verifiedIndexLifecycle
	hostLoad           HostLoadReader
	retainedCleanupMu  sync.Mutex
	postDDLMu          sync.RWMutex
	postDDLHook        func(context.Context) error

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

// New creates a new Executor.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	a *analyzer.Analyzer,
	rampStart time.Time,
	logFn func(string, string, ...any),
) *Executor {
	executor := &Executor{
		pool:          pool,
		cfg:           cfg,
		analyzer:      a,
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

// evaluateFindingPolicy is the single background-action authorization path.
// It reloads runtime mode, trust, enabled state, and emergency stop for every
// candidate so a safety downgrade takes effect before the next action.
func (e *Executor) evaluateFindingPolicy(
	ctx context.Context,
	f analyzer.Finding,
	isReplica bool,
) ActionPolicyDecision {
	e.policyMu.RLock()
	gate := e.policyGate
	e.policyMu.RUnlock()
	if gate != nil {
		return e.evaluateStandingPolicy(ctx, gate, f, isReplica)
	}
	contract, ok := contractForFinding(f)
	if !ok {
		return ActionPolicyDecision{
			Decision:      PolicyDecisionBlocked,
			RiskTier:      "unknown",
			BlockedReason: "action has no typed contract",
		}
	}
	cfg, mode, enabled := e.policySnapshot()
	emergencyStop := e.checkEmergencyStop(ctx)
	return EvaluateActionPolicy(contract, ActionPolicyContext{
		Config:              cfg,
		ExecutionMode:       mode,
		ExecutorEnabled:     &enabled,
		Now:                 time.Now(),
		RampStart:           e.rampStart,
		IsReplica:           isReplica,
		EmergencyStop:       emergencyStop,
		SafeActionsInFlight: len(e.analyzeSem),
		SafeActionLimit:     cap(e.analyzeSem),
	})
}

func (e *Executor) evaluateStandingPolicy(
	ctx context.Context, gate policy.Gate, finding analyzer.Finding, isReplica bool,
) ActionPolicyDecision {
	request := policy.ActionRequest{
		SQL: finding.RecommendedSQL, Feature: featureForFinding(finding),
		TargetObjs: targetObjectsForFinding(finding),
		IsReplica:  isReplica,
	}
	if contract, ok := contractForFinding(finding); ok {
		request.Contract = policyContract(contract)
	}
	return standingPolicyDecision(gate.Authorize(ctx, request))
}

func policyContract(contract ActionContract) *policy.ActionContract {
	result := &policy.ActionContract{
		ActionType: contract.ActionType, RiskTier: policy.RiskTier(contract.BaseRiskTier),
	}
	for _, guardrail := range contract.Guardrails {
		if strings.EqualFold(strings.TrimSpace(guardrail), "approval_required") {
			result.Guardrails = append(result.Guardrails, policy.GuardrailApprovalRequired)
		}
	}
	return result
}

func targetObjectsForFinding(finding analyzer.Finding) []string {
	target := strings.TrimSpace(finding.ObjectIdentifier)
	if target == "" {
		return nil
	}
	return []string{target}
}

func featureForFinding(finding analyzer.Finding) string {
	switch actionTypeForProposalSQL(finding.RecommendedSQL) {
	case "analyze_table":
		return "analyze"
	case "vacuum_table":
		return "vacuum"
	case "set_table_autovacuum":
		return "autovacuum_tuning"
	case "alter_system_guc", "alter_database_guc":
		return "config_guc"
	default:
		return "index"
	}
}

func standingPolicyDecision(decision policy.Decision) ActionPolicyDecision {
	result := ActionPolicyDecision{
		RiskTier: string(decision.RiskTier), BlockedReason: string(decision.Reason),
		RequiresApproval: decision.Verdict == policy.VerdictQueueApproval,
		EvidenceID:       decision.EvidenceID, DecisionID: decision.DecisionID,
	}
	switch decision.Verdict {
	case policy.VerdictExecute:
		result.Decision = PolicyDecisionExecute
	case policy.VerdictQueueApproval:
		result.Decision = PolicyDecisionQueueApproval
	case policy.VerdictPark:
		result.Decision = PolicyDecisionParked
	case policy.VerdictObserveOnly:
		result.Decision = PolicyDecisionObserveOnly
	default:
		result.Decision = PolicyDecisionBlocked
	}
	for _, guardrail := range decision.Guardrails {
		result.Guardrails = append(result.Guardrails, string(guardrail))
	}
	return result
}

func (e *Executor) policySnapshot() (*config.Config, string, bool) {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	if e.cfg == nil {
		return nil, e.execMode, !e.executorDisabled
	}
	cfgCopy := *e.cfg
	if e.trustLevelOverride != "" {
		cfgCopy.Trust.Level = e.trustLevelOverride
	}
	return &cfgCopy, e.execMode, !e.executorDisabled
}

func (e *Executor) checkEmergencyStop(ctx context.Context) bool {
	if e.emergencyStopFn != nil {
		return e.emergencyStopFn(ctx)
	}
	return CheckEmergencyStop(ctx, e.pool)
}

func contractForFinding(f analyzer.Finding) (ActionContract, bool) {
	if err := ValidateExecutorSQL(f.RecommendedSQL); err != nil {
		return ActionContract{}, false
	}
	actionType := actionTypeForProposalSQL(f.RecommendedSQL)
	return ContractForActionType(actionType)
}

// RunCycle is called after each analyzer cycle to evaluate and execute
// any actionable findings.
func (e *Executor) RunCycle(ctx context.Context, isReplica bool) {
	if e.indexVerification != nil {
		if err := e.indexVerification.ResumeDue(ctx); err != nil {
			e.logFn("executor", "resume index verification: %v", err)
		}
	}
	// Manual mode and executor-disabled are hard background-action stops.
	if e.effectiveExecMode() == "manual" || !e.ExecutorEnabled() {
		return
	}

	e.pruneRecentActions()
	findings := e.analyzer.Findings()

	for _, f := range findings {
		if f.RecommendedSQL == "" {
			continue
		}

		decision := e.evaluateFindingPolicy(ctx, f, isReplica)
		if decision.Decision == PolicyDecisionBlocked ||
			decision.Decision == PolicyDecisionObserveOnly {
			continue
		}

		if e.isCascadeCooldown(f.ObjectIdentifier) {
			continue
		}

		findingID := e.lookupFindingID(ctx, f)
		if findingID <= 0 {
			continue
		}

		if e.exceedsMaxRetries(ctx, findingID) {
			continue
		}

		// Anti-oscillation: if pg_sage has already applied this exact
		// action repeatedly (the object keeps reverting externally), stop.
		if e.exceedsOscillationLimit(ctx, f, findingID) {
			continue
		}

		// Approval mode: queue for approval instead of executing.
		if decision.Decision == PolicyDecisionQueueApproval {
			if e.actionStore == nil {
				e.logFn("executor",
					"cannot queue %q: action store unavailable", f.Title)
				continue
			}
			if checker, ok := e.actionStore.(PendingActionChecker); ok {
				hasPending, err := checker.HasPendingForFinding(
					ctx, int(findingID))
				if err != nil {
					e.logFn("executor",
						"failed to check pending approval for %q: %v",
						f.Title, err)
					continue
				}
				if hasPending {
					continue
				}
			}
			if checker, ok := e.actionStore.(PendingActionSQLChecker); ok {
				hasPending, err := checker.HasPendingForSQL(
					ctx, f.RecommendedSQL)
				if err != nil {
					e.logFn("executor",
						"failed to check duplicate approval SQL for %q: %v",
						f.Title, err)
					continue
				}
				if hasPending {
					continue
				}
			}
			if checker, ok := e.actionStore.(RejectedActionChecker); ok {
				cooldown := e.cascadeCooldown()
				rejected, err := checker.HasRecentlyRejectedForFinding(
					ctx, int(findingID), cooldown)
				if err != nil {
					e.logFn("executor",
						"failed to check rejected approval for %q: %v",
						f.Title, err)
					continue
				}
				if rejected {
					continue
				}
				rejected, err = checker.HasRecentlyRejectedForSQL(
					ctx, f.RecommendedSQL, cooldown)
				if err != nil {
					e.logFn("executor",
						"failed to check rejected approval SQL for %q: %v",
						f.Title, err)
					continue
				}
				if rejected {
					continue
				}
			}

			proposal := f
			proposal.ActionRisk = decision.RiskTier
			_, propErr := e.proposeForApproval(ctx, int(findingID), proposal)
			if propErr != nil {
				e.logFn("executor",
					"failed to queue %q for approval: %v",
					f.Title, propErr)
			} else {
				e.logFn("executor",
					"queued %q for approval", f.Title)
				e.dispatchEvent(ctx,
					notify.ApprovalNeededEvent(
						f.Title, f.RecommendedSQL,
						e.databaseName, decision.RiskTier))
			}
			continue
		}

		if CheckHysteresis(ctx, e.pool, findingID,
			e.cfg.Trust.RollbackCooldownDays) {
			e.logFn("executor",
				"skipping %q — rolled back recently (cooldown)",
				f.Title,
			)
			continue
		}

		// Reauthorize immediately before taking an execution slot. Runtime
		// safety changes made while evidence/retry checks ran must stop this
		// action rather than waiting for the next cycle.
		latest := e.evaluateFindingPolicy(ctx, f, isReplica)
		if latest.Decision != PolicyDecisionExecute {
			continue
		}

		// Limit concurrent DDL to avoid overwhelming the database.
		select {
		case e.ddlSem <- struct{}{}:
			// acquired — will release after execution
		default:
			e.logFn("executor",
				"DDL concurrency limit reached, skipping %s",
				f.RecommendedSQL)
			continue
		}

		// Release the DDL slot via defer so a panic in executeFinding
		// can't leak it and permanently shrink concurrency (C4).
		func() {
			defer func() { <-e.ddlSem }()
			latest = e.evaluateFindingPolicy(ctx, f, isReplica)
			if latest.Decision != PolicyDecisionExecute {
				return
			}
			e.executeFinding(ctx, f, findingID, latest.DecisionID)
		}()
	}
}

func (e *Executor) proposeForApproval(
	ctx context.Context,
	findingID int,
	f analyzer.Finding,
) (int, error) {
	if proposer, ok := e.actionStore.(ActionMetadataProposer); ok {
		return proposer.ProposeWithMetadata(
			ctx, nil, findingID,
			f.RecommendedSQL, f.RollbackSQL, f.ActionRisk,
			e.buildApprovalProposalMetadata(f, time.Now().UTC()),
		)
	}
	return e.actionStore.Propose(
		ctx, nil, findingID,
		f.RecommendedSQL, f.RollbackSQL, f.ActionRisk,
	)
}

func (e *Executor) buildApprovalProposalMetadata(
	f analyzer.Finding,
	now time.Time,
) store.ActionProposalMetadata {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	actionType := actionTypeForProposalSQL(f.RecommendedSQL)
	metadata := store.ActionProposalMetadata{
		ActionType:         actionType,
		IdentityKey:        actionIdentityKey(f, actionType),
		PolicyDecision:     PolicyDecisionQueueApproval,
		VerificationStatus: "not_started",
		ShadowToilMinutes:  estimatedToilForActionType(actionType),
	}
	expiresAt := now.Add(24 * time.Hour)
	metadata.ExpiresAt = &expiresAt
	if contract, ok := ContractForActionType(actionType); ok {
		decision := EvaluateActionPolicy(contract, e.policyContext(now))
		metadata.PolicyDecision = decision.Decision
		metadata.Guardrails = decision.Guardrails
	}
	return metadata
}

func (e *Executor) policyContext(now time.Time) ActionPolicyContext {
	cfg, mode, enabled := e.policySnapshot()
	return ActionPolicyContext{
		Config:              cfg,
		ExecutionMode:       mode,
		ExecutorEnabled:     &enabled,
		Now:                 now,
		RampStart:           e.rampStart,
		SafeActionsInFlight: len(e.analyzeSem),
		SafeActionLimit:     cap(e.analyzeSem),
	}
}

func actionIdentityKey(f analyzer.Finding, actionType string) string {
	parts := []string{f.Category, f.ObjectIdentifier, actionType}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ":")
}

func actionTypeForProposalSQL(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(upper, "ANALYZE "):
		return "analyze_table"
	case strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "CREATE UNIQUE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(upper, "DROP INDEX CONCURRENTLY "):
		return "drop_unused_index"
	case isConcurrentReindexSQL(upper):
		return "reindex_concurrently"
	case (strings.HasPrefix(upper, "VACUUM ") || upper == "VACUUM") &&
		!isVacuumFullSQL(upper):
		return "vacuum_table"
	case strings.Contains(upper, "PG_CANCEL_BACKEND"):
		return "cancel_backend"
	case strings.Contains(upper, "PG_TERMINATE_BACKEND"):
		return "terminate_backend"
	case strings.Contains(upper, "PG_CANCEL_BACKEND"):
		return "cancel_backend"
	case strings.HasPrefix(upper, "ALTER SYSTEM SET ") ||
		strings.HasPrefix(upper, "ALTER SYSTEM RESET "):
		return "alter_system_guc"
	case strings.HasPrefix(upper, "ALTER DATABASE ") &&
		allowedAlterDatabaseParam(upper):
		return "alter_database_guc"
	case isSetTableAutovacuumSQL(upper):
		return "set_table_autovacuum"
	case strings.HasPrefix(upper, "ALTER TABLE "):
		return "alter_table"
	case strings.HasPrefix(upper, "INSERT INTO HINT_PLAN.HINTS"):
		return "apply_query_hint"
	case strings.HasPrefix(upper, "DELETE FROM HINT_PLAN.HINTS"):
		return "retire_query_hint"
	default:
		return ""
	}
}

func isConcurrentReindexSQL(upper string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(upper, "REINDEX "))
	for _, objectType := range []string{
		"INDEX ", "TABLE ", "SCHEMA ", "DATABASE ", "SYSTEM ",
	} {
		if strings.HasPrefix(rest, objectType) {
			rest = strings.TrimSpace(strings.TrimPrefix(rest, objectType))
			return strings.HasPrefix(rest, "CONCURRENTLY ")
		}
	}
	return false
}

func isSetTableAutovacuumSQL(upper string) bool {
	if !strings.HasPrefix(upper, "ALTER TABLE ") {
		return false
	}
	subcommand := stripAlterTablePrefix(upper)
	return strings.HasPrefix(subcommand, "SET (") &&
		strings.Contains(subcommand, "AUTOVACUUM_")
}

func isVacuumFullSQL(upper string) bool {
	if strings.HasPrefix(upper, "VACUUM FULL ") || upper == "VACUUM FULL" {
		return true
	}
	if !strings.HasPrefix(upper, "VACUUM (") {
		return false
	}
	end := strings.IndexByte(upper, ')')
	if end < 0 {
		return true
	}
	options := strings.NewReplacer("(", " ", ")", " ", ",", " ").
		Replace(upper[:end+1])
	for _, option := range strings.Fields(options) {
		if option == "FULL" {
			return true
		}
	}
	return false
}

func estimatedToilForActionType(actionType string) int {
	if actionType == "analyze_table" {
		return 15
	}
	return 30
}

// executeFinding runs the DDL for a single finding and handles
// post-execution checks, rollback monitoring, and invalid index cleanup.
func (e *Executor) executeFinding(
	ctx context.Context, f analyzer.Finding, findingID int64, decisionID int64,
) {
	beforeState := e.snapshotBeforeState(ctx, targetQueryIDs(f))
	releaseLease, leaseErr := e.acquireDDLLease(ctx, f, decisionID)
	if leaseErr != nil {
		e.logActionWithDecision(ctx, f, findingID, beforeState, decisionID, leaseErr)
		e.logFn("executor", "DDL lease denied for %q: %v", f.Title, leaseErr)
		return
	}
	defer releaseLease()

	// Config changes on managed providers must go through the provider's
	// parameter group / database flags, not ALTER SYSTEM (which is blocked
	// there). Don't attempt it — record why so the operator applies it via
	// the cloud console instead of seeing a generic failure.
	if isAlterSystem(f.RecommendedSQL) &&
		isManagedProvider(e.cfg.CloudEnvironment) {
		param := configParamFromSQL(f.RecommendedSQL)
		guidance := managedConfigGuidance(e.cfg.CloudEnvironment, param)
		e.logFn("executor", "%s", guidance)
		e.logActionWithDecision(ctx, f, findingID, beforeState,
			decisionID,
			fmt.Errorf("%s", guidance))
		return
	}

	ddlTimeout := e.cfg.Safety.DDLTimeout()
	lockOpt := WithLockTimeout(e.cfg.Safety.LockTimeout())
	var verifiedAction verifiedIndexAction
	verifiedCreate := categorizeAction(f.RecommendedSQL) == "create_index"
	if verifiedCreate {
		var verificationErr error
		verifiedAction, verificationErr = verifiedActionForFinding(f)
		if verificationErr == nil && e.indexVerification != nil {
			verificationErr = e.indexVerification.Admit(ctx)
		} else if verificationErr == nil {
			verificationErr = ErrVerificationUnavailable
		}
		if verificationErr == nil {
			verificationErr = e.snapshotSupersededIndex(ctx, f, beforeState)
		}
		if verificationErr != nil {
			e.logActionWithDecision(
				ctx, f, findingID, beforeState, decisionID, verificationErr,
			)
			e.logFn("executor", "withheld unverifiable CREATE INDEX %q: %v",
				f.Title, verificationErr)
			return
		}
	}
	var execErr error
	switch {
	case categorizeAction(f.RecommendedSQL) == "analyze":
		execErr = e.executeAnalyze(ctx, f)
	case NeedsConcurrently(f.RecommendedSQL) ||
		NeedsTopLevel(f.RecommendedSQL):
		execErr = ExecConcurrently(
			ctx, e.pool, f.RecommendedSQL,
			ddlTimeout, lockOpt,
		)
	default:
		execErr = ExecInTransaction(
			ctx, e.pool, f.RecommendedSQL,
			ddlTimeout, lockOpt,
		)
	}

	actionID := e.logActionWithDecision(
		ctx, f, findingID, beforeState, decisionID, execErr,
	)
	if execErr != nil {
		_, _ = finalizeActionVerification(
			ctx, e.pool, actionID, "failed", execErr.Error(),
		)
		if errors.Is(execErr, ErrLockNotAvailable) {
			e.logFn("executor",
				"lock timeout for %q on %s — circuit-breaking table",
				f.Title, f.ObjectIdentifier,
			)
			e.recentActions[f.ObjectIdentifier] = time.Now()
		}
		e.logFn("executor",
			"execution failed for %q: %v", f.Title, execErr,
		)
		e.dispatchEvent(ctx,
			notify.ActionFailedEvent(
				f.Title, f.RecommendedSQL,
				e.databaseName, execErr.Error()))
		return
	}
	e.notifyPostDDL(ctx, f.RecommendedSQL)

	e.logFn("executor",
		"executed %q (action %d)", f.Title, actionID,
	)
	e.dispatchEvent(ctx,
		notify.ActionExecutedEvent(
			f.Title, f.RecommendedSQL, e.databaseName))
	e.recentActions[f.ObjectIdentifier] = time.Now()

	// Config changes: reload so a reload-only GUC takes effect now, or
	// record that a restart is still required. Without this, ALTER SYSTEM
	// only writes postgresql.auto.conf and the change never applies.
	if isAlterSystem(f.RecommendedSQL) {
		outcome := applyConfigChange(
			ctx, e.pool, f.RecommendedSQL, e.cfg.CloudEnvironment, e.logFn)
		e.logFn("executor", "config: %s", outcome.Note)
		updateActionOutcome(ctx, e.pool, actionID,
			outcomeStatus(outcome.InEffect), outcome.Note)
	}

	// Write a plain-English audit justification (C4). Async so the LLM
	// latency never blocks the cycle; WithoutCancel so it survives the
	// per-cycle context being cancelled.
	if e.justifier != nil {
		go e.justifyAndStore(context.WithoutCancel(ctx), actionID, f)
	}
	if verifiedCreate {
		verifiedAction.WatchID = fmt.Sprintf("index-action-%d", actionID)
		if err := e.indexVerification.WatchApplied(
			context.WithoutCancel(ctx), verifiedAction, actionID,
		); err != nil {
			e.logFn("executor", "index verification failed for action %d: %v",
				actionID, err)
		}
		return
	}

	if f.RollbackSQL != "" && actionID > 0 {
		// Detach the monitor from the cycle's context so the
		// rollback window can elapse even if RunCycle returns
		// (or is called from an HTTP handler). Shutdown signals
		// the monitor to abort early via e.shutdownCh.
		e.startRollbackMonitor(func() {
			MonitorAndRollback(
				context.WithoutCancel(ctx), e.pool, actionID, f.RollbackSQL,
				e.cfg.Trust.RollbackThresholdPct,
				e.cfg.Trust.RollbackWindowMinutes,
				e.logFn,
				e.shutdownCh,
				func(authCtx context.Context, rollbackSQL string) bool {
					candidate := f
					candidate.RecommendedSQL = rollbackSQL
					decision := e.evaluateFindingPolicy(authCtx, candidate, false)
					return decision.Decision == PolicyDecisionExecute
				},
			)
		})
	} else if actionID > 0 {
		// No rollback possible (VACUUM, ANALYZE, pg_terminate_backend)
		// — mark success immediately.
		updateActionSuccess(ctx, e.pool, actionID)
	}
}

func (e *Executor) acquireDDLLease(
	ctx context.Context, finding analyzer.Finding, decisionID int64,
) (func(), error) {
	if decisionID <= 0 || !isDDLMutation(finding.RecommendedSQL) {
		return func() {}, nil
	}
	objects, err := policy.NormalizeTargetObjects(
		targetObjectsForFinding(finding),
	)
	if err != nil {
		return func() {}, fmt.Errorf("normalize DDL lease targets: %w", err)
	}
	ttl := e.cfg.Safety.DDLTimeout() + time.Minute
	manager := policy.NewPostgresLeaseManager(e.pool, nil, decisionID, ttl)
	leaseID, err := manager.AcquireLease(
		ctx, "executor", objects, finding.RecommendedSQL,
	)
	if err != nil {
		return func() {}, err
	}
	return func() {
		if err := manager.ReleaseLease(context.WithoutCancel(ctx), leaseID); err != nil {
			e.logFn("executor", "release DDL lease %s: %v", leaseID, err)
		}
	}, nil
}

func isDDLMutation(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, prefix := range []string{
		"CREATE INDEX ", "CREATE UNIQUE INDEX ", "DROP INDEX ",
		"REINDEX ", "ALTER TABLE ",
	} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func (e *Executor) notifyPostDDL(ctx context.Context, sql string) {
	if !isDDLMutation(sql) {
		return
	}
	e.postDDLMu.RLock()
	hook := e.postDDLHook
	e.postDDLMu.RUnlock()
	if hook == nil {
		return
	}
	if err := hook(ctx); err != nil {
		e.logFn("executor", "post-DDL schema guard failed: %v", err)
	}
}

// dispatchEvent sends a notification event if the dispatcher is set.
// Errors are logged but do not interrupt the executor flow.
func (e *Executor) dispatchEvent(
	ctx context.Context, event notify.Event,
) {
	if e.dispatcher == nil {
		return
	}
	if err := e.dispatcher.Dispatch(ctx, event); err != nil {
		e.logFn("executor",
			"notification dispatch failed: %v", err)
	}
}

// cascadeCooldown returns the cooldown duration for the cascade
// guard, computed from config.
func (e *Executor) cascadeCooldown() time.Duration {
	cycles := e.cfg.Trust.CascadeCooldownCycles
	interval := e.cfg.Collector.IntervalSeconds
	d := time.Duration(cycles) *
		time.Duration(interval) * time.Second
	if d == 0 {
		d = 5 * time.Minute
	}
	return d
}

// isCascadeCooldown returns true if an action was recently
// executed for the given object identifier.
func (e *Executor) isCascadeCooldown(objID string) bool {
	t, ok := e.recentActions[objID]
	if !ok {
		return false
	}
	if time.Since(t) < e.cascadeCooldown() {
		e.logFn("executor",
			"cascade guard: skipping %q (action %v ago)",
			objID, time.Since(t),
		)
		return true
	}
	return false
}

// pruneRecentActions removes entries older than the cascade
// cooldown to prevent unbounded map growth.
func (e *Executor) pruneRecentActions() {
	maxAge := e.cascadeCooldown()
	for k, t := range e.recentActions {
		if time.Since(t) > maxAge {
			delete(e.recentActions, k)
		}
	}
}

// lookupFindingID retrieves the database ID for an open finding.
func (e *Executor) lookupFindingID(
	ctx context.Context, f analyzer.Finding,
) int64 {
	var id int64
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT id FROM sage.findings
		 WHERE category = $1
		   AND object_identifier = $2
		   AND status = 'open'
		   AND acted_on_at IS NULL
		 LIMIT 1`,
		f.Category, f.ObjectIdentifier,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// maxActionRetries is the maximum number of times the executor
// will retry a failed action before giving up permanently.
const maxActionRetries = 3

// oscillationLimit / oscillationWindowDays bound how many times pg_sage
// will repeat the SAME successful action on an object before backing off.
// An object that keeps reverting between cycles — e.g. an app re-creating
// an index pg_sage dropped as redundant — would otherwise churn forever
// (drop → recreate → drop → …). This is a persistent guard (reads the
// action log) so it survives restarts, unlike the in-memory cascade
// cooldown.
const (
	oscillationLimit      = 3
	oscillationWindowDays = 7
)

// exceedsOscillationLimit reports whether pg_sage has already SUCCESSFULLY
// executed this exact action enough times recently that repeating it is
// harmful churn. When it does, the finding is marked acted_on so it stops
// being re-proposed.
func (e *Executor) exceedsOscillationLimit(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	if e.pool == nil || f.RecommendedSQL == "" {
		return false
	}
	var n int
	if err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM sage.action_log
		 WHERE sql_executed = $1 AND outcome = 'success'
		   AND executed_at > now() - make_interval(days => $2)`,
		f.RecommendedSQL, oscillationWindowDays,
	).Scan(&n); err != nil {
		return false
	}
	if n >= oscillationLimit {
		e.logFn("executor",
			"anti-oscillation: already applied %q %d times in %dd — "+
				"backing off (object reverts externally)",
			f.Title, n, oscillationWindowDays)
		_, _ = e.pool.Exec(ctx,
			`/* pg_sage */ UPDATE sage.findings SET acted_on_at = now()
			 WHERE id = $1 AND acted_on_at IS NULL`, findingID)
		return true
	}
	return false
}

// exceedsMaxRetries checks if a finding has already failed more
// than maxActionRetries times, preventing infinite retry loops.
func (e *Executor) exceedsMaxRetries(
	ctx context.Context, findingID int64,
) bool {
	if e.pool == nil {
		return false
	}
	var failCount int
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM sage.action_log
		 WHERE finding_id = $1 AND outcome = 'failed'`,
		findingID,
	).Scan(&failCount)
	if err != nil {
		return false // on error, allow retry
	}
	if failCount >= maxActionRetries {
		// Mark the finding as acted_on so it stops appearing.
		// If the UPDATE fails silently we would retry forever, so
		// log the failure to surface it to operators.
		if _, err := e.pool.Exec(ctx,
			`/* pg_sage */ UPDATE sage.findings
			 SET acted_on_at = now()
			 WHERE id = $1 AND acted_on_at IS NULL`,
			findingID,
		); err != nil {
			e.logFn("executor",
				"failed to mark finding %d acted_on after "+
					"%d retries: %v — will retry this update next cycle",
				findingID, failCount, err)
		}
		return true
	}
	return false
}

func (e *Executor) markFindingActioned(
	ctx context.Context,
	findingID int64,
	actionID int64,
) {
	tag, err := e.pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.findings
		 SET acted_on_at = now(),
		     action_log_id = $1,
		     status = 'resolved',
		     resolved_at = COALESCE(resolved_at, now())
		 WHERE id = $2`,
		actionID, findingID,
	)
	if err != nil {
		e.logFn("executor",
			"failed to mark finding %d resolved after action %d: %v",
			findingID, actionID, err)
		return
	}
	if tag.RowsAffected() == 0 {
		e.logFn("executor",
			"manual action %d did not resolve finding %d: finding missing or already changed",
			actionID, findingID)
	}
}

// snapshotBeforeState captures current database health metrics
// to serve as a comparison baseline for rollback decisions.
func (e *Executor) snapshotBeforeState(
	ctx context.Context, queryIDs []int64,
) map[string]any {
	state := map[string]any{}
	// Record which queries this action targets so verify-and-revert can
	// compare their per-query latency before/after, instead of relying on
	// coarse global metrics (F1).
	if len(queryIDs) > 0 {
		state["target_queryids"] = queryIDs
	}

	var cacheHit float64
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(
			sum(blks_hit)::float /
			nullif(sum(blks_hit) + sum(blks_read), 0),
			1.0
		 ) FROM pg_stat_database`,
	).Scan(&cacheHit)
	if err == nil {
		state["cache_hit_ratio"] = cacheHit
	}

	var activeBackends int
	err = e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM pg_stat_activity
		 WHERE state = 'active'`,
	).Scan(&activeBackends)
	if err == nil {
		state["active_backends"] = activeBackends
	}

	var meanExecMs float64
	err = e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(avg(mean_exec_time), 0)
		 FROM pg_stat_statements
		 WHERE query LIKE 'INSERT%' OR query LIKE 'UPDATE%'`,
	).Scan(&meanExecMs)
	if err == nil && meanExecMs > 0 {
		state["mean_exec_time_ms"] = meanExecMs
	}

	return state
}

// logAction records the executed action in sage.action_log.
func (e *Executor) logAction(
	ctx context.Context,
	f analyzer.Finding,
	findingID int64,
	beforeState map[string]any,
	execErr error,
) int64 {
	return e.logActionWithDecision(ctx, f, findingID, beforeState, 0, execErr)
}

func (e *Executor) logActionWithDecision(
	ctx context.Context,
	f analyzer.Finding,
	findingID int64,
	beforeState map[string]any,
	decisionID int64,
	execErr error,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)

	outcome := actionOutcome(execErr)

	actionType := categorizeAction(f.RecommendedSQL)

	var errReason *string
	if execErr != nil {
		s := execErr.Error()
		errReason = &s
	}

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, rollback_reason, decision_id)
		 VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, $7, NULLIF($8, 0))
		 RETURNING id`,
		actionType, findingID, f.RecommendedSQL,
		nilIfEmpty(f.RollbackSQL), beforeJSON, outcome,
		errReason, decisionID,
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log action for %q: %v", f.Title, err,
		)
		return 0
	}

	// Link the finding to this action.
	// Only mark acted_on_at when the action succeeded so that failed
	// findings remain eligible for retry (lookupFindingID filters on
	// acted_on_at IS NULL).
	if outcome != "failed" && findingID > 0 {
		e.markFindingActioned(ctx, findingID, actionID)
	}

	return actionID
}

// categorizeAction derives an action_type label from the SQL statement.
func categorizeAction(sql string) string {
	upper := strings.ToUpper(sql)
	switch {
	case strings.Contains(upper, "CREATE INDEX"):
		return "create_index"
	case strings.Contains(upper, "DROP INDEX"):
		return "drop_index"
	case strings.Contains(upper, "REINDEX"):
		return "reindex"
	case strings.Contains(upper, "VACUUM"):
		return "vacuum"
	case strings.Contains(upper, "ANALYZE"):
		return "analyze"
	case strings.Contains(upper, "PG_TERMINATE_BACKEND"):
		return "terminate_backend"
	case strings.Contains(upper, "ALTER"):
		return "alter"
	default:
		return "ddl"
	}
}

// actionOutcome returns "failed" when execErr is non-nil, "monitoring"
// otherwise. Successful reversible actions stay in monitoring until
// post-action verification marks them success or rolled_back.
// This determines whether acted_on_at is set on the finding — failed actions
// must leave the finding retryable.
func actionOutcome(execErr error) string {
	if execErr != nil {
		return "failed"
	}
	return "monitoring"
}

// nilIfEmpty returns nil for empty strings, used for nullable SQL params.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// extractIndexName parses the index name from a CREATE INDEX statement.
func extractIndexName(sql string) string {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, "INDEX")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(sql[idx+5:])
	// Skip optional "CONCURRENTLY" and "IF NOT EXISTS".
	upper = strings.ToUpper(rest)
	if strings.HasPrefix(upper, "CONCURRENTLY") {
		rest = strings.TrimSpace(rest[len("CONCURRENTLY"):])
		upper = strings.ToUpper(rest)
	}
	if strings.HasPrefix(upper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	} else if strings.HasPrefix(upper, "IF EXISTS") {
		// DROP INDEX CONCURRENTLY IF EXISTS <name>
		rest = strings.TrimSpace(rest[len("IF EXISTS"):])
	}
	// Next token is the index name.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "ON") {
		return ""
	}
	name := strings.Trim(fields[0], "\"")
	return name
}

// unqualifyIndexName reduces a possibly schema-qualified, quoted, or
// semicolon-terminated index reference to a bare lowercase name for
// comparison (e.g. `public."idx_x";` -> `idx_x`).
func unqualifyIndexName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ";")
	name = strings.Trim(name, "\"")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.Trim(name, "\""))
}

// isSelfReferentialDrop reports whether dropDDL drops the same index that
// createSQL creates. The optimizer reuses a recommendation's rollback DDL
// (a DROP of the NEW index) as Detail["drop_ddl"], so the INCLUDE-upgrade
// path must skip it — otherwise it would immediately drop the index it just
// created. A genuine supersede targets a different, pre-existing index.
func isSelfReferentialDrop(createSQL, dropDDL string) bool {
	c := unqualifyIndexName(extractIndexName(createSQL))
	d := unqualifyIndexName(extractIndexName(dropDDL))
	return c != "" && c == d
}
