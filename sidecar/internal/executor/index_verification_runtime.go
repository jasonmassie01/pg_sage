package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
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

func (a *executorIndexActions) Revert(
	ctx context.Context, actionID int64, rollbackSQL string, verdict verify.Verdict,
) error {
	if strings.TrimSpace(rollbackSQL) == "" {
		return errors.New("verified index rollback SQL is empty")
	}
	if a.exec.checkEmergencyStop(ctx) {
		updateActionOutcome(ctx, a.exec.pool, actionID, "rollback_skipped",
			"emergency stop active; automatic rollback withheld")
		return errors.New("emergency stop withheld verified index rollback")
	}
	candidate := analyzer.Finding{
		RecommendedSQL:   rollbackSQL,
		ObjectIdentifier: extractIndexName(rollbackSQL),
		ObjectType:       "index",
	}
	decision := a.exec.evaluateFindingPolicy(ctx, candidate, false)
	if decision.Decision != PolicyDecisionExecute {
		updateActionOutcome(ctx, a.exec.pool, actionID, "rollback_skipped",
			"standing policy withheld automatic rollback")
		return errors.New("standing policy withheld verified index rollback")
	}
	if err := ExecConcurrently(ctx, a.exec.pool, rollbackSQL, 60*time.Second); err != nil {
		updateActionOutcome(ctx, a.exec.pool, actionID, "rollback_failed", err.Error())
		return fmt.Errorf("revert verified index action %d: %w", actionID, err)
	}
	updateActionOutcome(ctx, a.exec.pool, actionID, "rolled_back", verdict.Reason)
	_, _ = value.NewPostgresRepository(a.exec.pool).
		ZeroCreditOnRevert(ctx, actionID, "rolled_back")
	return nil
}

func (e *Executor) configureIndexVerification() {
	if e == nil || e.pool == nil || e.cfg == nil {
		return
	}
	options := verificationOptions(e.cfg)
	store := verify.NewPostgresStateStore(e.pool, options.MinSamples)
	engine, err := verify.NewEngine(
		verify.NewPostgresObservationSource(e.pool), store, options,
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
	if cfg.Safety.CPUCeilingPct > 0 {
		options.CPUCeilingPct = float64(cfg.Safety.CPUCeilingPct)
		options.DataIOCeilingPct = float64(cfg.Safety.CPUCeilingPct)
		options.LogIOCeilingPct = float64(cfg.Safety.CPUCeilingPct)
	}
	return options
}

func verifiedActionForFinding(f analyzer.Finding) (verifiedIndexAction, error) {
	queryIDs := targetQueryIDs(f)
	indexName := extractIndexName(f.RecommendedSQL)
	table := strings.TrimSpace(f.ObjectIdentifier)
	if indexName == "" || table == "" || len(queryIDs) == 0 || f.RollbackSQL == "" {
		return verifiedIndexAction{}, ErrVerificationUnavailable
	}
	return verifiedIndexAction{
		SQL: f.RecommendedSQL, RollbackSQL: f.RollbackSQL,
		Table: table, IndexName: indexName, QueryIDs: queryIDs,
		Criterion: verify.Criterion{Kind: "per_query_latency"},
	}, nil
}
