package migration

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// RiskAssessor enriches a DDLClassification with live database metrics
// and computes a composite risk score.
type RiskAssessor struct {
	pool         *pgxpool.Pool
	logFn        func(string, string, ...any)
	rowThreshold int64
	timeouts     catalogread.Timeouts // bounds each live-metric read
}

// NewRiskAssessor creates a RiskAssessor backed by the given pool.
func NewRiskAssessor(
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) *RiskAssessor {
	return &RiskAssessor{
		pool: pool, logFn: logFn, rowThreshold: defaultDDLRowThreshold,
		timeouts: catalogread.Default(),
	}
}

// SetCatalogReadTimeouts bounds the live-metric reads (table stats,
// activity, locks, replication lag) with the safety configuration's
// statement and lock timeouts (static.md F11): a slow read is cut off and
// the risk is scored from what is known.
func (ra *RiskAssessor) SetCatalogReadTimeouts(t catalogread.Timeouts) { ra.timeouts = t }

// catalog is the bounded reader over the assessor's pool.
func (ra *RiskAssessor) catalog() catalogread.Reader {
	return catalogread.New(ra.pool, ra.timeouts)
}

// Assess queries live database metrics and computes a risk score for
// the given DDL classification. Without a pool only the intrinsic rule
// hazard is scored.
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

	if ra.pool != nil {
		if c.TableName != "" {
			ra.fetchTableStats(ctx, risk)
			ra.fetchActiveQueries(ctx, risk)
			ra.fetchPendingLocks(ctx, risk)
		}
		ra.fetchReplicationLag(ctx, risk)
	}

	risk.RiskScore = computeRiskScore(risk)
	capSmallIdleTable(risk, ra.rowThreshold)
	risk.EstimatedLockMs = estimateLockDuration(risk)
	return risk, nil
}
