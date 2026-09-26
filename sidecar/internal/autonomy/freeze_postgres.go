package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

type PostgresFreezeCustodian struct {
	pool         *pgxpool.Pool
	database     string
	threshold    freeze.Thresholds
	mu           sync.Mutex
	last         freezeCounters
	lastMxidRate float64
}

func NewPostgresFreezeCustodian(
	pool *pgxpool.Pool, database string, redBufferPct float64,
) *PostgresFreezeCustodian {
	red, amber := freezeThresholds(redBufferPct)
	return &PostgresFreezeCustodian{
		pool: pool, database: database,
		threshold: freeze.Thresholds{RedBufferPct: red, AmberBufferPct: amber},
	}
}

func (c *PostgresFreezeCustodian) Scan(ctx context.Context) ([]Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rates, err := c.transactionRates(ctx)
	if err != nil {
		return nil, err
	}
	blocker, err := c.oldestXminBlocker(ctx)
	if err != nil {
		return nil, err
	}
	repack, err := c.pgRepackAvailable(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, freezeHorizonSQL)
	if err != nil {
		return nil, fmt.Errorf("read freeze horizons: %w", err)
	}
	defer rows.Close()
	result := make([]Proposal, 0)
	for rows.Next() {
		proposal, scanErr := c.scanResponseRow(rows, rates, blocker, repack)
		if scanErr != nil {
			return nil, scanErr
		}
		if proposal.SQL != "" || proposal.Plan != "" {
			result = append(result, proposal)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate freeze horizons: %w", err)
	}
	return result, nil
}

func (c *PostgresFreezeCustodian) scanResponseRow(
	row interface{ Scan(...any) error }, rates freezeRateSample,
	blocker *freeze.XminBlocker, repack bool,
) (Proposal, error) {
	var sample freeze.HorizonSample
	var deadRatio float64
	if err := row.Scan(&sample.Schema, &sample.Table, &sample.XIDAge, &sample.XIDMaxAge,
		&sample.MultiXactAge, &sample.MultiXactMaxAge, &deadRatio); err != nil {
		return Proposal{}, fmt.Errorf("scan freeze response: %w", err)
	}
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rates.xid, rates.mxid
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, fmt.Errorf("evaluate freeze response: %w", err)
	}

	input := freeze.ResponseInput{Schema: sample.Schema, Table: sample.Table,
		Urgency: assessment.Urgency, DeadTupleRatio: deadRatio,
		BloatRatio: deadRatio, PGRepackAvailable: repack}
	if assessment.Urgency == freeze.UrgencyRed {
		input.Blocker = blocker
	}
	response, err := freeze.PlanResponse(input)
	if err != nil {
		return Proposal{}, err
	}
	proposal := freezeResponseProposal(c.database, sample, assessment.Proposal, response)
	if !rates.known {
		// Without two samples the consumption rate (and so the hard deadline)
		// is unknown: act on urgency, but claim no deadline override.
		proposal.Deadline = nil
	}
	return proposal, nil
}

func freezeResponseProposal(
	database string, sample freeze.HorizonSample,
	deadline freeze.Proposal, response freeze.Response,
) Proposal {
	feature := "freeze"
	switch response.Kind {
	case freeze.ResponseCancelBlocker, freeze.ResponseTerminateBlocker:
		feature = "freeze_blocker"
	case freeze.ResponseTuneAutovacuum:
		feature = "autovacuum_tuning"
	}
	evidence := response.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	if response.Plan != "" {
		evidence["plan"] = response.Plan
	}
	return Proposal{Database: database, Feature: feature, SQL: response.SQL,
		Plan: response.Plan, Evidence: evidence,
		TargetObjects: []string{sample.Schema + "." + sample.Table},
		Deadline:      freezePolicyDeadline(deadline)}
}

func (c *PostgresFreezeCustodian) oldestXminBlocker(
	ctx context.Context,
) (*freeze.XminBlocker, error) {
	var blocker freeze.XminBlocker
	err := c.pool.QueryRow(ctx, oldestXminBlockerSQL).Scan(
		&blocker.PID, &blocker.XminAge, &blocker.User, &blocker.State, &blocker.Query,
		&blocker.AppName, &blocker.BackendStart, &blocker.QueryStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("diagnose xmin blocker: %w", err)
	}
	return &blocker, nil
}

func (c *PostgresFreezeCustodian) pgRepackAvailable(ctx context.Context) (bool, error) {
	var available bool
	err := c.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM pg_extension WHERE extname='pg_repack')`).Scan(&available)
	return available, err
}

func freezePolicyDeadline(proposal freeze.Proposal) *policy.DeadlineContext {
	if proposal.Urgency != freeze.UrgencyRed {
		return nil
	}
	return &policy.DeadlineContext{
		Kind: policy.DeadlineXID, Urgency: policy.UrgencyCritical,
		HardAt: proposal.Deadline.HardAt,
	}
}

// freezeCounters is one sample of the XID and multixact consumption
// counters. nextXID is the snapshot xmax (it advances only for transactions
// that were assigned an XID, i.e. the ones that consume wraparound runway).
type freezeCounters struct {
	at      time.Time
	nextXID int64
	mxidAge int64
}

type freezeRateSample struct {
	xid, mxid float64
	known     bool
}

const minimumFreezeRate = 0.001

// freezeRates derives per-second rates from two samples. The first sample
// yields no rate; a negative multixact delta (datminmxid advanced) yields an
// unknown multixact rate (-1).
func freezeRates(prev, cur freezeCounters) (float64, float64, bool) {
	if prev.at.IsZero() || !cur.at.After(prev.at) {
		return 0, 0, false
	}
	seconds := cur.at.Sub(prev.at).Seconds()
	xid := math.Max(float64(cur.nextXID-prev.nextXID)/seconds, minimumFreezeRate)
	mxid := -1.0
	if delta := cur.mxidAge - prev.mxidAge; delta >= 0 {
		mxid = math.Max(float64(delta)/seconds, minimumFreezeRate)
	}
	return xid, mxid, true
}

func (c *PostgresFreezeCustodian) transactionRates(ctx context.Context) (freezeRateSample, error) {
	var cur freezeCounters
	if err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		pg_snapshot_xmax(pg_current_snapshot())::text::bigint,
		mxid_age(datminmxid)::bigint
		FROM pg_database WHERE datname=current_database()`).
		Scan(&cur.nextXID, &cur.mxidAge); err != nil {
		return freezeRateSample{}, fmt.Errorf("read transaction counters: %w", err)
	}
	cur.at = time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	xid, mxid, known := freezeRates(c.last, cur)
	c.last = cur
	if mxid > 0 {
		c.lastMxidRate = mxid
	} else if c.lastMxidRate > 0 {
		mxid = c.lastMxidRate
	}
	if !known {
		return freezeRateSample{xid: minimumFreezeRate, mxid: minimumFreezeRate}, nil
	}
	return freezeRateSample{xid: xid, mxid: math.Max(mxid, minimumFreezeRate), known: true}, nil
}

func freezeThresholds(red float64) (float64, float64) {
	if red <= 0 || red >= 100 {
		red = 25
	}
	amber := red * 2
	if amber > 100 {
		amber = 100
	}
	return red, amber
}

const freezeHorizonSQL = `/* pg_sage */
SELECT n.nspname, c.relname, age(c.relfrozenxid)::bigint,
       current_setting('autovacuum_freeze_max_age')::bigint,
       mxid_age(c.relminmxid)::bigint,
       current_setting('autovacuum_multixact_freeze_max_age')::bigint,
	       CASE WHEN COALESCE(s.n_live_tup,0)+COALESCE(s.n_dead_tup,0)=0 THEN 0
            ELSE s.n_dead_tup::float8/(s.n_live_tup+s.n_dead_tup) END
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid=c.oid
WHERE c.relkind IN ('r','m')
AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')`

// oldestXminBlockerSQL only considers client sessions of the connected
// database; walsenders, autovacuum, other databases, backup/dump tools and
// pg_sage itself are never candidates for cancellation.
const oldestXminBlockerSQL = `/* pg_sage */ SELECT pid, age(backend_xmin)::bigint,
COALESCE(usename,''), COALESCE(state,''), LEFT(COALESCE(query,''),500),
COALESCE(application_name,''), backend_start, COALESCE(query_start, backend_start)
FROM pg_stat_activity
WHERE backend_xmin IS NOT NULL AND pid<>pg_backend_pid()
  AND datname = current_database() AND backend_type = 'client backend'
  AND application_name NOT ILIKE '%pg_sage%'
  AND application_name NOT ILIKE '%pg_dump%'
  AND application_name NOT ILIKE '%pg_basebackup%'
  AND application_name NOT ILIKE '%pg_restore%'
ORDER BY age(backend_xmin) DESC LIMIT 1`
