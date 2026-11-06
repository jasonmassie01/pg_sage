package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// custodianBloatConfirmPerCycle bounds the pgstattuple confirmations of one
// custodian cycle: pgstattuple_approx reads every page that is not
// all-visible, so it never runs inside the per-cycle catalog scan.
const custodianBloatConfirmPerCycle = 3

// bloatConfirmer measures one heap's bloat ratio (table oid).
type bloatConfirmer func(ctx context.Context, oid int64) (float64, error)

// freezeRow is one table of freezeHorizonSQL with its bloat estimate.
type freezeRow struct {
	sample       freeze.HorizonSample
	deadRatio    float64
	oid          int64
	heapBytes    int64
	bloat        float64
	bloatKnown   bool
	bloatSource  string
	confirmError string
	// userCatalog is a user_catalog_table: a slot's catalog_xmin holds it.
	userCatalog bool
}

func scanHorizonRow(row interface{ Scan(...any) error }) (freezeRow, error) {
	var out freezeRow
	var pages int64
	var expected *float64
	s := &out.sample
	if err := row.Scan(&s.Schema, &s.Table, &s.XIDAge, &s.XIDMaxAge,
		&s.MultiXactAge, &s.MultiXactMaxAge, &out.deadRatio,
		&out.oid, &pages, &out.heapBytes, &expected, &out.userCatalog); err != nil {
		return freezeRow{}, fmt.Errorf("scan freeze response: %w", err)
	}
	out.bloat, out.bloatKnown = freeze.EstimatedBloat(pages, expected)
	out.bloatSource = freeze.BloatSourceEstimate
	return out, nil
}

// WithBloatBounds sets the least heap size for a bloat plan and the bounds
// of each pgstattuple confirmation. A zero floor or timeout keeps the
// shipped value: a zero bound would mean an unbounded probe.
func (c *PostgresFreezeCustodian) WithBloatBounds(
	minBytes int64, confirm catalogread.Timeouts,
) *PostgresFreezeCustodian {
	if minBytes > 0 {
		c.bloatMinBytes = minBytes
	}
	defaults := catalogread.Default()
	if confirm.Statement <= 0 {
		confirm.Statement = defaults.Statement
	}
	if confirm.Lock <= 0 {
		confirm.Lock = defaults.Lock
	}
	c.confirmBounds = confirm
	return c
}

// isBloatCandidate reports a heap whose catalog estimate already plans a
// rebuild: only those are worth a pgstattuple confirmation.
func (c *PostgresFreezeCustodian) isBloatCandidate(row freezeRow) bool {
	return row.bloatKnown && row.bloat >= freeze.BloatPlanThreshold &&
		row.heapBytes >= c.bloatMinBytes
}

// confirmBloat measures at most custodianBloatConfirmPerCycle candidates,
// largest first, when pgstattuple is installed. A failed or timed-out
// measurement keeps the catalog estimate and records why.
func (c *PostgresFreezeCustodian) confirmBloat(
	ctx context.Context, rows []freezeRow, available bool,
) {
	if !available || c.confirm == nil {
		return
	}
	var candidates []int
	for i := range rows {
		if c.isBloatCandidate(rows[i]) {
			candidates = append(candidates, i)
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return rows[candidates[a]].heapBytes > rows[candidates[b]].heapBytes
	})
	if len(candidates) > custodianBloatConfirmPerCycle {
		candidates = candidates[:custodianBloatConfirmPerCycle]
	}
	for _, i := range candidates {
		ratio, err := c.confirm(ctx, rows[i].oid)
		if err != nil {
			rows[i].confirmError = err.Error()
			continue
		}
		rows[i].bloat, rows[i].bloatSource = ratio, freeze.BloatSourcePgstattuple
	}
}

// pgstattupleConfirmSQL is the measured bloat: the share of the heap not
// holding live tuples, beyond the free space the fillfactor reserves.
const pgstattupleConfirmSQL = `/* pg_sage */
SELECT CASE WHEN a.table_len > 0 THEN greatest(0, least(1,
         1 - a.approx_tuple_len::float8 / (a.table_len * COALESCE(
           (SELECT ro.option_value::float8 FROM pg_class pc,
                   pg_options_to_table(pc.reloptions) ro
             WHERE pc.oid = $1::bigint::oid AND ro.option_name = 'fillfactor'),
           100) / 100))) END
FROM pgstattuple_approx($1::bigint::oid::regclass) a`

var errNoBloatMeasurement = errors.New("pgstattuple_approx returned no measurement")

// pgstattupleApprox runs one confirmation in its own bounded read-only
// transaction (statement and lock timeouts).
func (c *PostgresFreezeCustodian) pgstattupleApprox(
	ctx context.Context, oid int64,
) (float64, error) {
	tx, err := catalogread.Begin(ctx, c.pool, c.confirmBounds)
	if err != nil {
		return 0, fmt.Errorf("confirm bloat of relation %d: %w", oid, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var ratio *float64
	err = tx.QueryRow(ctx, pgstattupleConfirmSQL, oid).Scan(&ratio)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ratio == nil) {
		return 0, fmt.Errorf("confirm bloat of relation %d: %w", oid, errNoBloatMeasurement)
	}
	if err != nil {
		return 0, fmt.Errorf("confirm bloat of relation %d: %w", oid, err)
	}
	if math.IsNaN(*ratio) {
		return 0, fmt.Errorf("confirm bloat of relation %d: %w", oid, errNoBloatMeasurement)
	}
	return *ratio, nil
}
