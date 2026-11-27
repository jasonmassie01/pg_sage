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
	// reader, when set, runs the live-metric reads instead of the pool
	// (Review: the caller's bounded querier).
	reader catalogread.Querier
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

// newQuerierAssessor is a RiskAssessor whose live-metric reads run on q
// (already bounded by the caller); nil q scores intrinsic hazards only.
func newQuerierAssessor(
	q catalogread.Querier, rowThreshold int64, logFn func(string, string, ...any),
) *RiskAssessor {
	ra := &RiskAssessor{logFn: logFn, rowThreshold: rowThreshold, reader: q}
	if q != nil {
		ra.repack = newRepackCache(ra.probeRepack)
	}
	return ra
}

// live reports whether the assessor reads live metrics.
func (ra *RiskAssessor) live() bool { return ra.pool != nil || ra.reader != nil }

// catalog is the bounded reader over the assessor's pool, or its querier.
func (ra *RiskAssessor) catalog() catalogread.Querier {
	if ra.reader != nil {
		return ra.reader
	}
	return catalogread.New(ra.pool, ra.timeouts)
}

// Assess queries live database metrics and computes a risk score for
// the given DDL classification. Without a pool only the intrinsic rule
// hazard is scored. A failed live read is logged and scored as unknown;
// it is also kept on the risk (readErr) for callers that must not score a
// migration they could not measure (Review).
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

	if ra.live() {
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
