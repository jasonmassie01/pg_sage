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
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// custodianBloatConfirmPerCycle bounds the pgstattuple confirmations of one
// custodian cycle: pgstattuple_approx reads every page that is not
// all-visible, so it never runs inside the per-cycle catalog scan.
const custodianBloatConfirmPerCycle = 3

// bloatConfirmer measures one heap's bloat ratio (table oid).
type bloatConfirmer func(ctx context.Context, oid int64) (float64, error)

// freezeRow is one table horizon with its bloat estimate.
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
	// catalog is a system catalog: planned for an operator, never vacuumed
	// by the custodian.
	catalog bool
}

// newFreezeRow answers one horizon of the shared reader. A manual VACUUM
// is aggressive from the settings' maxima, not the table's reloption.
func newFreezeRow(h probes.FreezeHorizon) freezeRow {
	out := freezeRow{deadRatio: h.DeadTupleRatio, oid: h.OID, heapBytes: h.HeapBytes,
		userCatalog: h.UserCatalog, catalog: h.Catalog,
		sample: freeze.HorizonSample{Schema: h.Schema, Table: h.Table, XIDAge: h.XIDAge,
			XIDMaxAge: h.XIDMaxAge, MultiXactAge: h.MultiXactAge,
			MultiXactMaxAge: h.MultiXactMaxAge}}
	out.sample.Aggressive = aggressiveAges(h.FreezeTableAge, h.MultiXactFreezeTableAge,
		h.SettingXIDMaxAge, h.SettingMultiXactMax)
	out.bloat, out.bloatKnown = freeze.EstimatedBloat(h.Pages, h.ExpectedPages)
	out.bloatSource = freeze.BloatSourceEstimate
	return out
}

// aggressiveAges derives the ages at which a plain VACUUM is aggressive; a
// setting the server did not report leaves them unknown (VACUUM (FREEZE)).
func aggressiveAges(
	xidTableAge, mxidTableAge *int64, xidMax, mxidMax int64,
) freeze.AggressiveAges {
	if xidTableAge == nil || mxidTableAge == nil {
		return freeze.AggressiveAges{}
	}
	return freeze.AggressiveVacuumAges(*xidTableAge, xidMax, *mxidTableAge, mxidMax)
}

// WithBloatBounds sets the least heap size for a bloat plan and the bounds
// of each pgstattuple confirmation and of the sampler's reading. A zero
// floor or timeout keeps the shipped value: a zero bound would mean an
// unbounded probe.
func (c *PostgresFreezeCustodian) WithBloatBounds(
	minBytes int64, confirm catalogread.Timeouts,
) *PostgresFreezeCustodian {
	if minBytes > 0 {
		c.bloatMinBytes = minBytes
		c.sampler.bloatMinBytes = minBytes
	}
	defaults := catalogread.Default()
	if confirm.Statement <= 0 {
		confirm.Statement = defaults.Statement
	}
	if confirm.Lock <= 0 {
		confirm.Lock = defaults.Lock
	}
	c.confirmBounds = confirm
	c.sampler.bounds = confirm
	return c
}

// isBloatCandidate reports a heap whose catalog estimate already plans a
// rebuild: only those are worth a pgstattuple confirmation.
func (c *PostgresFreezeCustodian) isBloatCandidate(row freezeRow) bool {
	return !row.catalog && row.bloatKnown && row.bloat >= freeze.BloatPlanThreshold &&
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
// holding live tuples, beyond the free space the fillfactor reserves. fn
// is the qualified pgstattuple_approx.
func pgstattupleConfirmSQL(fn string) string {
	return `/* pg_sage */
SELECT CASE WHEN a.table_len > 0 THEN greatest(0, least(1,
         1 - a.approx_tuple_len::float8 / (a.table_len * COALESCE(
           (SELECT ro.option_value::float8 FROM pg_class pc,
                   pg_options_to_table(pc.reloptions) ro
             WHERE pc.oid = $1::bigint::oid AND ro.option_name = 'fillfactor'),
           100) / 100))) END
FROM ` + fn + `($1::bigint::oid::regclass) a`
}

var errNoBloatMeasurement = errors.New("pgstattuple_approx returned no measurement")

// pgstattupleFunc is pgstattuple_approx qualified by its extension's
// schema on q.
func pgstattupleFunc(ctx context.Context, q extschema.Querier) (string, error) {
	s, err := extschema.Detect(ctx, q)
	if err != nil {
		return "", err
	}
	return extschema.Func(s.Pgstattuple, "pgstattuple_approx")
}

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
	fn, err := pgstattupleFunc(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("confirm bloat of relation %d: %w", oid, err)
	}
	err = tx.QueryRow(ctx, pgstattupleConfirmSQL(fn), oid).Scan(&ratio)
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
