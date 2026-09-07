package migration

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RiskAssessor enriches a DDLClassification with live database metrics
// and computes a composite risk score.
type RiskAssessor struct {
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)
}

// NewRiskAssessor creates a RiskAssessor backed by the given pool.
func NewRiskAssessor(
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) *RiskAssessor {
	return &RiskAssessor{pool: pool, logFn: logFn}
}

// Assess queries live database metrics and computes a risk score for
// the given DDL classification.
func (ra *RiskAssessor) Assess(
	ctx context.Context, c DDLClassification,
) (*DDLRisk, error) {
	risk := &DDLRisk{
		Statement:       c.Statement,
		RuleID:          c.RuleID,
		LockLevel:       c.LockLevel,
		RequiresRewrite: c.RequiresRewrite,
		TableName:       c.TableName,
		SchemaName:      c.SchemaName,
		SafeAlternative: c.SafeAlternative,
		Description:     c.Description,
	}

	if c.TableName != "" {
		ra.fetchTableStats(ctx, risk)
		ra.fetchActiveQueries(ctx, risk)
		ra.fetchPendingLocks(ctx, risk)
	}
	ra.fetchReplicationLag(ctx, risk)

	risk.RiskScore = computeRiskScore(risk)
	risk.EstimatedLockMs = estimateLockDuration(risk)
	return risk, nil
}
