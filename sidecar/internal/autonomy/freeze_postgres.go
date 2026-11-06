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
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
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
	// bloatMinBytes is the least heap size for a bloat plan;
	// confirmBounds bound each pgstattuple confirmation, which confirm
	// runs (pgstattupleApprox unless a test replaces it).
	bloatMinBytes int64
	confirmBounds catalogread.Timeouts
	confirm       bloatConfirmer
}

// NewPostgresFreezeCustodian builds the custodian with the shipped bloat
// floor (1 GiB) and the default catalog read bounds; WithBloatBounds
// applies the configured ones.
func NewPostgresFreezeCustodian(
	pool *pgxpool.Pool, database string, redBufferPct float64,
) *PostgresFreezeCustodian {
	c := &PostgresFreezeCustodian{
		pool: pool, database: database,
		threshold:     freeze.BufferThresholds(redBufferPct),
		bloatMinBytes: config.DefaultTableBloatMinBytes,
		confirmBounds: catalogread.Default(),
	}
	c.confirm = c.pgstattupleApprox
	return c
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
	ext, err := c.extensions(ctx)
	if err != nil {
		return nil, err
	}
	horizons, err := c.readHorizons(ctx)
	if err != nil {
		return nil, err
	}
	c.confirmBloat(ctx, horizons, ext.pgstattuple)
	result := make([]Proposal, 0)
	for _, row := range horizons {
		proposal, err := c.respond(row, rates, blocker, ext.pgRepack)
		if err != nil {
			return nil, err
		}
		if proposal.SQL != "" || proposal.Plan != "" {
			result = append(result, proposal)
		}
	}
	return result, nil
}

// readHorizons reads every table's freeze horizon and catalog bloat
// estimate (the estimate only for heaps at the size floor).
func (c *PostgresFreezeCustodian) readHorizons(ctx context.Context) ([]freezeRow, error) {
	rows, err := c.pool.Query(ctx, freezeHorizonSQL, c.bloatMinBytes)
	if err != nil {
		return nil, fmt.Errorf("read freeze horizons: %w", err)
	}
	defer rows.Close()
	var result []freezeRow
	for rows.Next() {
		row, err := scanHorizonRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate freeze horizons: %w", err)
	}
	return result, nil
}

// respond answers one table's horizon: the freeze ladder, then a bloat
// plan from the bloat estimate (never from the dead-tuple ratio).
func (c *PostgresFreezeCustodian) respond(
	row freezeRow, rates freezeRateSample, blocker *freeze.XminBlocker, repack bool,
) (Proposal, error) {
	sample := row.sample
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rates.xid, rates.mxid
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, fmt.Errorf("evaluate freeze response: %w", err)
	}
	input := freeze.ResponseInput{Schema: sample.Schema, Table: sample.Table,
		Urgency: assessment.Urgency, DeadTupleRatio: row.deadRatio,
		BloatRatio: row.bloat, BloatKnown: row.bloatKnown, BloatSource: row.bloatSource,
		SizeBytes: row.heapBytes, MinBloatBytes: c.bloatMinBytes,
		PGRepackAvailable: repack}
	if assessment.Urgency == freeze.UrgencyRed {
		input.Blocker = blocker
	}
	response, err := freeze.PlanResponse(input)
	if err != nil {
		return Proposal{}, err
	}
	if row.confirmError != "" && response.Evidence != nil {
		response.Evidence["bloat_confirm_error"] = row.confirmError
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
	case freeze.ResponsePlanRepack, freeze.ResponseParkBloat:
		feature = "bloat_remediation"
	}
	evidence := response.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	if response.Plan != "" {
		evidence["plan"] = response.Plan
	}
	return Proposal{Database: database, Feature: feature, SQL: response.SQL,
		Plan: response.Plan, ResponseKind: string(response.Kind), Evidence: evidence,
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

// freezeExtensions are the optional extensions the custodian uses.
type freezeExtensions struct{ pgRepack, pgstattuple bool }

func (c *PostgresFreezeCustodian) extensions(ctx context.Context) (freezeExtensions, error) {
	var ext freezeExtensions
	err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		EXISTS(SELECT 1 FROM pg_extension WHERE extname='pg_repack'),
		EXISTS(SELECT 1 FROM pg_extension WHERE extname='pgstattuple')`).
		Scan(&ext.pgRepack, &ext.pgstattuple)
	if err != nil {
		return freezeExtensions{}, fmt.Errorf("read custodian extensions: %w", err)
	}
	return ext, nil
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

// freezeHorizonSQL reads the catalog only (measured.md M3): the heap size
// is relpages, and the bloat estimate's pg_stats lookup runs only for heaps
// at the size floor ($1), so a small table costs one catalog row.
var freezeHorizonSQL = `/* pg_sage */
SELECT n.nspname, c.relname, age(c.relfrozenxid)::bigint,
       current_setting('autovacuum_freeze_max_age')::bigint,
       mxid_age(c.relminmxid)::bigint,
       current_setting('autovacuum_multixact_freeze_max_age')::bigint,
       CASE WHEN t.live + t.dead = 0 THEN 0 ELSE t.dead::float8 / (t.live + t.dead) END,
       c.oid::bigint, c.relpages::bigint, h.heap_bytes,
       CASE WHEN h.heap_bytes >= $1 THEN ` + catalogread.HeapExpectedPagesSQL("c", "n") +
	`::float8 END
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
CROSS JOIN LATERAL (SELECT pg_stat_get_live_tuples(c.oid) AS live,
                           pg_stat_get_dead_tuples(c.oid) AS dead) t
CROSS JOIN LATERAL (SELECT c.relpages::bigint
                           * current_setting('block_size')::bigint AS heap_bytes) h
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
