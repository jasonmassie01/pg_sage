package probes

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// FreezeHorizonsSQL (v1) is the freeze custodian's horizon reader: the
// $1 relations most advanced toward their effective freeze maximum
// (freezeRankCTE, the ranking wraparound_tables uses) plus up to $1 heaps
// of at least $2 bytes (the bloat plan floor; 0 reads none), so a large
// young heap still gets its bloat estimate. A TOAST relation is answered
// as the table that owns it (reltoastrelid): a VACUUM of the owner
// freezes its TOAST too. Statistics and the bloat estimate are read only
// for the rows returned. The first fourteen columns keep the order of the
// custodian's earlier per-table read; the last four are the settings'
// freeze maxima (a manual VACUUM is aggressive from the setting, never
// the reloption), whether the owner is a catalog (pg_catalog,
// information_schema or a shared relation) and whether the row came from
// a TOAST horizon.
var FreezeHorizonsSQL = `/* pg_sage sre:freeze_horizons v1 */
` + freezeRankCTE + `, big AS (
    SELECT t.*, GREATEST(t.xid_age::float8 / NULLIF(t.freeze_max_age, 0),
                         t.mxid_age::float8 / NULLIF(t.mxid_freeze_max_age, 0)) AS used
    FROM t JOIN pg_catalog.pg_class b ON b.oid = t.oid
    WHERE b.relkind IN ('r', 'm') AND $2::int8 > 0
      AND b.relpages::int8 * pg_catalog.current_setting('block_size')::int8 >= $2::int8
      AND NOT EXISTS (SELECT 1 FROM top WHERE top.oid = t.oid)
    ORDER BY b.relpages DESC, t.oid
    LIMIT $1
), cand AS (
    SELECT * FROM top UNION ALL SELECT * FROM big
), toast_owner AS (
    SELECT o.oid, o.reltoastrelid FROM pg_catalog.pg_class o WHERE o.reltoastrelid <> 0
)
SELECT n.nspname::text, c.relname::text, cand.xid_age, cand.freeze_max_age,
       cand.mxid_age, cand.mxid_freeze_max_age,
       CASE WHEN s.live + s.dead = 0 THEN 0
            ELSE s.dead::float8 / (s.live + s.dead) END,
       c.oid::int8, c.relpages::int8, h.heap_bytes,
       CASE WHEN c.relkind IN ('r', 'm') AND $2::int8 > 0 AND h.heap_bytes >= $2::int8
            THEN ` + catalogread.HeapExpectedPagesSQL("c", "n") + `::float8 END,
       EXISTS (SELECT 1 FROM pg_catalog.pg_options_to_table(c.reloptions) o
               WHERE o.option_name = 'user_catalog_table'
                 AND pg_catalog.lower(o.option_value) IN ('true', 'on', 'yes', '1')),
       pg_catalog.current_setting('vacuum_freeze_table_age', true)::int8,
       pg_catalog.current_setting('vacuum_multixact_freeze_table_age', true)::int8,
       g.xid_max, g.mxid_max,
       n.nspname IN ('pg_catalog', 'information_schema') OR c.relisshared,
       r.relkind = 't'
FROM cand CROSS JOIN g
JOIN pg_catalog.pg_class r ON r.oid = cand.oid
LEFT JOIN toast_owner w ON r.relkind = 't' AND w.reltoastrelid = r.oid
JOIN pg_catalog.pg_class c ON c.oid = COALESCE(w.oid, r.oid)
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL (SELECT pg_catalog.pg_stat_get_live_tuples(c.oid) AS live,
                           pg_catalog.pg_stat_get_dead_tuples(c.oid) AS dead) s
CROSS JOIN LATERAL (SELECT c.relpages::int8
                           * pg_catalog.current_setting('block_size')::int8 AS heap_bytes) h
WHERE r.relkind <> 't' OR w.oid IS NOT NULL
ORDER BY cand.used DESC NULLS LAST, cand.oid`

// FreezeHorizon is one table's freeze horizon. XIDMaxAge and
// MultiXactMaxAge are its effective maxima (LEAST of the setting and the
// table's reloption); the Setting maxima are the server settings alone.
type FreezeHorizon struct {
	Schema, Table                           string
	OID                                     int64
	XIDAge, XIDMaxAge                       int64
	MultiXactAge, MultiXactMaxAge           int64
	SettingXIDMaxAge, SettingMultiXactMax   int64
	DeadTupleRatio                          float64
	Pages, HeapBytes                        int64
	ExpectedPages                           *float64
	UserCatalog                             bool
	FreezeTableAge, MultiXactFreezeTableAge *int64
	// Catalog marks a system catalog (pg_catalog, information_schema or a
	// shared relation): the custodian plans it, never runs its VACUUM.
	Catalog bool
	// ViaTOAST is a horizon read from the table's TOAST relation.
	ViaTOAST bool
}

// Share is how much of its effective freeze maximum the table has used:
// the larger of its XID and multixact shares (0 for an unknown maximum).
func (h FreezeHorizon) Share() float64 {
	share := 0.0
	if h.XIDMaxAge > 0 {
		share = float64(h.XIDAge) / float64(h.XIDMaxAge)
	}
	if h.MultiXactMaxAge > 0 {
		share = max(share, float64(h.MultiXactAge)/float64(h.MultiXactMaxAge))
	}
	return share
}

// ScanFreezeHorizon scans one FreezeHorizonsSQL row.
func ScanFreezeHorizon(row interface{ Scan(...any) error }) (FreezeHorizon, error) {
	var h FreezeHorizon
	if err := row.Scan(&h.Schema, &h.Table, &h.XIDAge, &h.XIDMaxAge,
		&h.MultiXactAge, &h.MultiXactMaxAge, &h.DeadTupleRatio, &h.OID, &h.Pages,
		&h.HeapBytes, &h.ExpectedPages, &h.UserCatalog, &h.FreezeTableAge,
		&h.MultiXactFreezeTableAge, &h.SettingXIDMaxAge, &h.SettingMultiXactMax,
		&h.Catalog, &h.ViaTOAST); err != nil {
		return FreezeHorizon{}, fmt.Errorf("scan freeze horizon: %w", err)
	}
	return h, nil
}

// ReadFreezeHorizons reads at most limit horizons (plus up to limit heaps
// at the bloat floor minBloatBytes; 0 reads none) through q, most
// advanced first. A table met through its own row and its TOAST row is
// returned once, at its most advanced horizon.
func ReadFreezeHorizons(
	ctx context.Context, q catalogread.Querier, limit int, minBloatBytes int64,
) ([]FreezeHorizon, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("read freeze horizons: limit %d must be positive", limit)
	}
	if minBloatBytes < 0 {
		return nil, fmt.Errorf("read freeze horizons: bloat floor %d must not be negative",
			minBloatBytes)
	}
	rows, err := q.Query(ctx, FreezeHorizonsSQL, limit, minBloatBytes)
	if err != nil {
		return nil, fmt.Errorf("read freeze horizons: %w", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var out []FreezeHorizon
	for rows.Next() {
		h, err := ScanFreezeHorizon(rows)
		if err != nil {
			return nil, err
		}
		if seen[h.OID] {
			continue // the owner's second row ranks lower (ordered by share)
		}
		seen[h.OID] = true
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate freeze horizons: %w", err)
	}
	return out, nil
}
