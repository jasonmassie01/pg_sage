package autonomy

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
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
	// detail reads a pinning session's own state before it is signalled
	// (readSessionDetail unless a test replaces it).
	detail sessionDetailReader
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
	c.detail = c.readSessionDetail
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
	ext, err := c.extensions(ctx)
	if err != nil {
		return nil, err
	}
	holders, horizons, err := c.readAges(ctx)
	if err != nil {
		return nil, err
	}
	c.confirmBloat(ctx, horizons, ext.pgstattuple)
	return c.plan(ctx, horizons, rates, holders, ext.pgRepack)
}

// readAges reads the xmin holders and the table horizons in one read-only
// transaction. age() measures from a reference XID that stays fixed for a
// transaction, so every holder's age and every table's age share one
// reference: XIDs the cluster consumes between the two reads cannot make a
// holder look younger than the tables it pins (and a futile freeze look
// useful).
func (c *PostgresFreezeCustodian) readAges(
	ctx context.Context,
) ([]freeze.XminBlocker, []freezeRow, error) {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin freeze age read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // read-only
	holders, err := c.xminHolders(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	horizons, err := c.readHorizons(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("end freeze age read: %w", err)
	}
	return holders, horizons, nil
}

// readHorizons reads every table's freeze horizon and catalog bloat
// estimate (the estimate only for heaps at the size floor).
func (c *PostgresFreezeCustodian) readHorizons(
	ctx context.Context, q catalogread.Querier,
) ([]freezeRow, error) {
	rows, err := q.Query(ctx, freezeHorizonSQL, c.bloatMinBytes)
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
// plan from the bloat estimate (never from the dead-tuple ratio). A red
// table also returns the holder that pins it, if any.
func (c *PostgresFreezeCustodian) respond(
	row freezeRow, rates freezeRateSample, holders []freeze.XminBlocker, repack bool,
) (Proposal, *freeze.XminBlocker, error) {
	sample := row.sample
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rates.xid, rates.mxid
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, nil, fmt.Errorf("evaluate freeze response: %w", err)
	}
	input := freeze.ResponseInput{Schema: sample.Schema, Table: sample.Table,
		Urgency: assessment.Urgency, XIDAge: sample.XIDAge, MultiXactAge: sample.MultiXactAge,
		Aggressive: sample.Aggressive, DeadTupleRatio: row.deadRatio,
		BloatRatio: row.bloat, BloatKnown: row.bloatKnown, BloatSource: row.bloatSource,
		SizeBytes: row.heapBytes, MinBloatBytes: c.bloatMinBytes,
		PGRepackAvailable: repack}
	if assessment.Urgency == freeze.UrgencyRed {
		input.Holder = freeze.PinningCandidate(holders, row.userCatalog)
	}
	response, err := freeze.PlanResponse(input)
	if err != nil {
		return Proposal{}, nil, err
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
	if proposal.Deadline != nil {
		deadlineEvidence(proposal.Evidence, assessment.Deadline, rates.failsafeAge)
	}
	return proposal, response.PinnedBy, nil
}

// deadlineEvidence records which counter set the policy deadline (its
// Kind stays xid, the key of deadline_overrides) and the server's
// vacuum_failsafe_age when it is known (PG14+).
func deadlineEvidence(evidence map[string]any, deadline freeze.Deadline, failsafeAge int64) {
	evidence["deadline_threat"] = string(deadline.Threat)
	if failsafeAge > 0 {
		evidence["vacuum_failsafe_age"] = failsafeAge
	}
}

func freezeResponseProposal(
	database string, sample freeze.HorizonSample,
	deadline freeze.Proposal, response freeze.Response,
) Proposal {
	feature := "freeze"
	switch response.Kind {
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
	// failsafeAge is vacuum_failsafe_age (PG14+), 0 when the server does
	// not report it; deadline evidence only, read with the counters.
	failsafeAge int64
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
	var failsafe *string
	if err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		pg_snapshot_xmax(pg_current_snapshot())::text::bigint,
		mxid_age(datminmxid)::bigint,
		current_setting('vacuum_failsafe_age', true)
		FROM pg_database WHERE datname=current_database()`).
		Scan(&cur.nextXID, &cur.mxidAge, &failsafe); err != nil {
		return freezeRateSample{}, fmt.Errorf("read transaction counters: %w", err)
	}
	failsafeAge := parseFailsafeAge(failsafe)
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
		return freezeRateSample{xid: minimumFreezeRate, mxid: minimumFreezeRate,
			failsafeAge: failsafeAge}, nil
	}
	return freezeRateSample{xid: xid, mxid: math.Max(mxid, minimumFreezeRate), known: true,
		failsafeAge: failsafeAge}, nil
}

// parseFailsafeAge reads vacuum_failsafe_age best-effort: a missing
// setting (PG13 and older, or hidden by a managed provider) or an
// unparsable or non-positive value is unknown (0).
func parseFailsafeAge(setting *string) int64 {
	if setting == nil {
		return 0
	}
	age, err := strconv.ParseInt(strings.TrimSpace(*setting), 10, 64)
	if err != nil || age <= 0 {
		return 0
	}
	return age
}

// freezeHorizonSQL reads the catalog only (measured.md M3): the heap size
// is relpages, and the bloat estimate's pg_stats lookup runs only for heaps
// at the size floor ($1), so a small table costs one catalog row. The last
// column marks user catalog tables, which a slot's catalog_xmin holds; the
// last two are the freeze table ages that make a plain VACUUM aggressive
// (NULL when a provider hides them: the custodian then keeps FREEZE).
var freezeHorizonSQL = `/* pg_sage */
SELECT n.nspname, c.relname, age(c.relfrozenxid)::bigint,
       current_setting('autovacuum_freeze_max_age')::bigint,
       mxid_age(c.relminmxid)::bigint,
       current_setting('autovacuum_multixact_freeze_max_age')::bigint,
       CASE WHEN t.live + t.dead = 0 THEN 0 ELSE t.dead::float8 / (t.live + t.dead) END,
       c.oid::bigint, c.relpages::bigint, h.heap_bytes,
       CASE WHEN h.heap_bytes >= $1 THEN ` + catalogread.HeapExpectedPagesSQL("c", "n") +
	`::float8 END,
       EXISTS (SELECT 1 FROM pg_options_to_table(c.reloptions) o
               WHERE o.option_name = 'user_catalog_table'
                 AND lower(o.option_value) IN ('true', 'on', 'yes', '1')),
       current_setting('vacuum_freeze_table_age', true)::bigint,
       current_setting('vacuum_multixact_freeze_table_age', true)::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
CROSS JOIN LATERAL (SELECT pg_stat_get_live_tuples(c.oid) AS live,
                           pg_stat_get_dead_tuples(c.oid) AS dead) t
CROSS JOIN LATERAL (SELECT c.relpages::bigint
                           * current_setting('block_size')::bigint AS heap_bytes) h
WHERE c.relkind IN ('r','m')
AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')`
