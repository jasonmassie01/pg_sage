package migration

import (
	"context"
	"time"

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
	repack       *repackCache         // nil without a pool: never named
}

// NewRiskAssessor creates a RiskAssessor backed by the given pool.
func NewRiskAssessor(
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) *RiskAssessor {
	ra := &RiskAssessor{
		pool: pool, logFn: logFn, rowThreshold: defaultDDLRowThreshold,
		timeouts: catalogread.Default(),
	}
	if pool != nil {
		ra.repack = newRepackCache(ra.probeRepack)
	}
	return ra
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
		SessionPID:      c.SessionPID,
		SafeAlternative: ra.safeAlternative(ctx, c),
		Description:     c.Description,
	}

	if ra.pool != nil {
		at := time.Now().UTC()
		if c.TableName != "" && c.SchemaName == "" {
			ra.resolveTableSchema(ctx, risk, c.SessionRole)
		}
		if c.TableName != "" && !risk.NameAmbiguous {
			ra.fetchTableStats(ctx, risk)
			ra.fetchPendingLocks(ctx, risk)
		}
		if c.TableName != "" {
			ra.fetchActiveQueries(ctx, risk)
		}
		ra.fetchReplicationLag(ctx, risk)
		// Only a read that succeeded is a measurement to date.
		if risk.Measured.any() {
			risk.MeasuredAt = at
		}
	}

	risk.RiskScore = computeRiskScore(risk)
	capSmallIdleTable(risk, ra.rowThreshold)
	risk.EstimatedLockMs = estimateLockDuration(risk)
	return risk, nil
}
