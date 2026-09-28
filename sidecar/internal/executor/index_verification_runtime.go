package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/value"
	"github.com/pg-sage/sidecar/internal/verify"
)

type correlatedVerifyEngine interface {
	OKToApplyNow(context.Context) (verify.Admission, error)
	Watch(context.Context, verify.WatchRequest) (verify.Verdict, error)
	ResumeDueResults(context.Context) ([]verify.ResumeResult, error)
}

type postgresIndexVerifier struct {
	engine correlatedVerifyEngine
	exec   *Executor
}

func (v *postgresIndexVerifier) OKToApplyNow(
	ctx context.Context,
) (verify.Admission, error) {
	return v.engine.OKToApplyNow(ctx)
}

func (v *postgresIndexVerifier) Watch(
	ctx context.Context, request verify.WatchRequest,
) (verify.Verdict, error) {
	return v.engine.Watch(ctx, request)
}

func (v *postgresIndexVerifier) ResumeDue(
	ctx context.Context,
) ([]resumedIndexVerification, error) {
	results, err := v.engine.ResumeDueResults(ctx)
	resumed := make([]resumedIndexVerification, 0, len(results))
	for _, result := range results {
		rollbackSQL, lookupErr := v.rollbackSQL(ctx, result.ActionID)
		if lookupErr != nil {
			return resumed, lookupErr
		}
		resumed = append(resumed, resumedIndexVerification{
			ActionID: result.ActionID, RollbackSQL: rollbackSQL,
			Verdict: result.Verdict,
		})
	}
	return resumed, err
}

func (v *postgresIndexVerifier) rollbackSQL(
	ctx context.Context, actionID int64,
) (string, error) {
	var rollbackSQL string
	err := v.exec.pool.QueryRow(ctx, `SELECT COALESCE(rollback_sql, '')
		FROM sage.action_log WHERE id=$1`, actionID).Scan(&rollbackSQL)
	if err != nil {
		return "", fmt.Errorf("load rollback SQL for action %d: %w", actionID, err)
	}
	if strings.TrimSpace(rollbackSQL) == "" {
		return "", fmt.Errorf("action %d has no rollback SQL", actionID)
	}
	return rollbackSQL, nil
}

type executorIndexActions struct {
	exec *Executor
}

func (a *executorIndexActions) Apply(
	context.Context, verifiedIndexAction,
) (int64, error) {
	return 0, errors.New("executor applies index DDL before starting its watch")
}

func (a *executorIndexActions) Retain(
	ctx context.Context, actionID int64, verdict verify.Verdict,
) error {
	if !verdict.Retain || verdict.Revert {
		return errors.New("superseded cleanup requires an unambiguous retained verdict")
	}
	updateActionSuccess(ctx, a.exec.pool, actionID)
	_, err := value.NewService(value.NewPostgresRepository(a.exec.pool)).
		CreditVerifiedAction(ctx, actionID)
	if err != nil && !errors.Is(err, value.ErrToilModelUnavailable) {
		return fmt.Errorf("credit verified action %d: %w", actionID, err)
	}
	return a.exec.cleanupRetainedIndex(ctx, actionID)
}

// Revert drops the index this action created. It proves identity first:
// the recorded OID must still own the name. Until the revert effect
// succeeds the durable verification stays open and is retried.
func (a *executorIndexActions) Revert(
	ctx context.Context, actionID int64, _ string, verdict verify.Verdict,
) error {
	exec := a.exec
	if exec.checkEmergencyStop(ctx) {
		updateActionOutcome(ctx, exec.pool, actionID, "rollback_skipped",
			"emergency stop active; automatic rollback withheld")
		return errors.New("emergency stop withheld verified index rollback")
	}
	identity, err := exec.loadCreatedIndexIdentity(ctx, actionID)
	if err != nil {
		return exec.abandonRevert(ctx, actionID, err.Error())
	}
	current, err := exec.currentIndexOID(ctx, identity.Name)
	if err != nil {
		return fmt.Errorf("resolve created index %s: %w", identity.Name, err)
	}
	if current == 0 {
		exec.finishRevert(ctx, actionID, verdict.Reason+"; index already absent")
		return nil
	}
	if current != identity.OID {
		return exec.abandonRevert(ctx, actionID,
			"index "+identity.Name+" is no longer the object pg_sage created")
	}
	return exec.dropCreatedIndex(ctx, actionID, identity.Name, verdict)
}

func (e *Executor) dropCreatedIndex(
	ctx context.Context, actionID int64, qualified string, verdict verify.Verdict,
) error {
	dropSQL := "DROP INDEX CONCURRENTLY IF EXISTS " + qualified
	if !e.authorizeCreatedIndexRevert(ctx, dropSQL, qualified) {
		updateActionOutcome(ctx, e.pool, actionID, "rollback_skipped",
			"standing policy withheld automatic rollback")
		return errors.New("standing policy withheld verified index rollback")
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	cfg, _, _ := e.policySnapshot()
	lockOpt := WithLockTimeout(0)
	timeout := time.Minute
	if cfg != nil {
		lockOpt, timeout = WithLockTimeout(cfg.Safety.LockTimeout()), cfg.Safety.DDLTimeout()
	}
	if err := ExecConcurrently(ctx, e.pool, dropSQL, timeout, lockOpt); err != nil {
		updateActionOutcome(ctx, e.pool, actionID, "rollback_failed", err.Error())
		return fmt.Errorf("revert verified index action %d: %w", actionID, err)
	}
	e.finishRevert(ctx, actionID, verdict.Reason)
	return nil
}

func (e *Executor) finishRevert(ctx context.Context, actionID int64, reason string) {
	updateActionOutcome(ctx, e.pool, actionID, "rolled_back", reason)
	e.completeVerificationRevert(ctx, actionID)
	_, _ = value.NewPostgresRepository(e.pool).ZeroCreditOnRevert(ctx, actionID, "rolled_back")
}

// abandonRevert records a revert that can never be performed safely and
// closes the verification so it is not retried against the wrong object.
func (e *Executor) abandonRevert(ctx context.Context, actionID int64, reason string) error {
	updateActionOutcome(ctx, e.pool, actionID, "rollback_skipped",
		"revert refused: "+reason)
	e.completeVerificationRevert(ctx, actionID)
	return fmt.Errorf("revert of action %d refused: %s", actionID, reason)
}

func (e *Executor) configureIndexVerification() {
	if e == nil || e.pool == nil || e.cfg == nil {
		return
	}
	options := verificationOptions(e.cfg)
	store := verify.NewPostgresStateStore(e.pool, options.MinSamples)
	engine, err := verify.NewEngine(
		executorObservationSource{
			ObservationSource: verify.NewPostgresObservationSource(e.pool), executor: e,
		}, store, options,
	)
	if err != nil {
		e.logFn("executor", "index verification unavailable: %v", err)
		return
	}
	verifier := &postgresIndexVerifier{engine: engine, exec: e}
	e.indexVerification = newVerifiedIndexLifecycle(
		verifier, &executorIndexActions{exec: e},
	)
}

func verificationOptions(cfg *config.Config) verify.Options {
	options := verify.DefaultOptions()
	if cfg.Verify.WindowMinutes > 0 {
		options.InitialWindow = time.Duration(cfg.Verify.WindowMinutes) * time.Minute
	}
	if cfg.Verify.WindowMaxMinutes > 0 {
		options.HardMax = time.Duration(cfg.Verify.WindowMaxMinutes) * time.Minute
	}
	if cfg.Verify.MinSamples > 0 {
		options.MinSamples = cfg.Verify.MinSamples
	}
	if cfg.Verify.MinGainPct > 0 {
		options.MinGainPct = cfg.Verify.MinGainPct
	}
	if cfg.Verify.RegressPct > 0 {
		options.RegressPct = cfg.Verify.RegressPct
	}
	if cfg.Verify.WriteImpactPct > 0 {
		options.WriteImpactPct = cfg.Verify.WriteImpactPct
	}
	return withAdmissionOptions(options, cfg)
}

// withAdmissionOptions maps load-admission config. The IO ceilings are
// separate keys and never inherit the CPU ceiling. io_baseline_days is
// taken as configured: an explicit 0 disables the learned baseline.
func withAdmissionOptions(options verify.Options, cfg *config.Config) verify.Options {
	if cfg.Safety.CPUCeilingPct > 0 {
		options.CPUCeilingPct = float64(cfg.Safety.CPUCeilingPct)
	}
	if cfg.Safety.DataIOCeilingPct > 0 {
		options.DataIOCeilingPct = float64(cfg.Safety.DataIOCeilingPct)
	}
	if cfg.Safety.WALIOCeilingPct > 0 {
		options.LogIOCeilingPct = float64(cfg.Safety.WALIOCeilingPct)
	}
	options.BaselineDays = float64(cfg.Verify.IOBaselineDays)
	return options
}
