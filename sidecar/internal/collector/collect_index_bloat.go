package collector

import (
	"context"
	"fmt"
	"math"
)

// indexBloatRowCap bounds the index_bloat reading to the largest btree
// indexes above analyzer.index_bloat_min_bytes (rules-13).
const indexBloatRowCap = 200

// IndexBloat is one btree index's bloat estimate from planner statistics
// (rules-13): the leaf pages its reltuples need at its fillfactor, from
// the key columns' pg_stats widths, against the pages it has (relpages).
// Both counts are as of the last VACUUM, ANALYZE or rebuild.
type IndexBloat struct {
	SchemaName     string  `json:"schemaname"`
	RelName        string  `json:"relname"`
	IndexRelName   string  `json:"indexrelname"`
	IndexRelID     uint32  `json:"indexrelid"`
	RelPages       int64   `json:"relpages"`
	EstimatedPages int64   `json:"estimated_pages"`
	IndexBytes     int64   `json:"index_bytes"`
	BloatBytes     int64   `json:"bloat_bytes"`
	BloatPct       float64 `json:"bloat_pct"`
	Fillfactor     int     `json:"fillfactor"`
	Reltuples      float64 `json:"reltuples"`
}

// indexBloatSQL is the well-known statistics-based btree estimate
// (check_postgres, pgsql-bloat-estimation): a leaf tuple is the
// MAXALIGN(8)-padded IndexTupleData (plus the null bitmap when a key can
// be null) and key data, the key widths being pg_stats avg_width net of
// null_frac, plus its 4-byte line pointer; a page holds (block - 24-byte
// header - 16-byte btree special) x fillfactor of them, plus the metapage.
// It reads the catalog only: no relation is opened or stat()ed. Only
// valid leaf (relkind i) btree indexes outside the system and sage schemas
// whose table and index have reltuples (not -1) are read, at most $2 of
// the largest of at least $1 bytes. est_pages is NULL when a key column
// has no statistics (never analyzed, or not readable by this role).
// INCLUDE columns count: they are stored in the leaf tuples.
const indexBloatSQL = sageTag + `
WITH idx AS (
  SELECT n.nspname, tc.relname AS tblname, ic.relname AS idxname, ic.oid AS idxoid,
         ix.indrelid AS tbloid, ix.indkey, ix.indnatts,
         ic.reltuples::float8 AS reltuples, ic.relpages::bigint AS relpages,
         COALESCE((SELECT substring(o FROM '^fillfactor=([0-9]+)$')::int
                     FROM unnest(ic.reloptions) AS o
                    WHERE o LIKE 'fillfactor=%'), 90) AS fillfactor
    FROM pg_index ix
    JOIN pg_class ic ON ic.oid = ix.indexrelid
    JOIN pg_class tc ON tc.oid = ix.indrelid
    JOIN pg_namespace n ON n.oid = tc.relnamespace
   WHERE ic.relkind = 'i'
     AND ic.relam = (SELECT oid FROM pg_am WHERE amname = 'btree')
     AND ix.indisvalid AND ix.indisready
     AND tc.relpersistence <> 't'
     AND tc.reltuples >= 0 AND ic.reltuples >= 0 AND ic.relpages > 0
     AND ic.relpages::bigint * current_setting('block_size')::bigint >= $1
     AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
     AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'
   ORDER BY ic.relpages DESC, ic.oid
   LIMIT $2
), cols AS (
  SELECT i.idxoid, s.null_frac, s.avg_width
    FROM idx i
   CROSS JOIN LATERAL generate_series(0, i.indnatts - 1) AS k(pos)
    LEFT JOIN pg_attribute ta
      ON i.indkey[k.pos] <> 0 AND ta.attrelid = i.tbloid AND ta.attnum = i.indkey[k.pos]
    LEFT JOIN pg_attribute xa
      ON i.indkey[k.pos] = 0 AND xa.attrelid = i.idxoid AND xa.attnum = k.pos + 1
    LEFT JOIN pg_stats s
      ON s.schemaname = i.nspname AND NOT s.inherited
     AND ((s.tablename = i.tblname AND s.attname = ta.attname)
       OR (s.tablename = i.idxname AND s.attname = xa.attname))
), widths AS (
  SELECT idxoid, count(avg_width) = count(*) AS complete,
         ceil((8 + CASE WHEN max(COALESCE(null_frac, 0)) > 0 THEN 4 ELSE 0 END) / 8.0) * 8
         + ceil(sum((1 - COALESCE(null_frac, 0)) * COALESCE(avg_width, 0)) / 8.0) * 8
           AS tuple_bytes
    FROM cols
   GROUP BY idxoid
)
SELECT i.nspname, i.tblname, i.idxname, i.idxoid, i.relpages, i.fillfactor, i.reltuples,
       current_setting('block_size')::bigint AS block_size,
       CASE WHEN w.complete THEN (1 + ceil(i.reltuples / GREATEST(1, floor(
         (current_setting('block_size')::bigint - 40) * i.fillfactor /
         (100 * (4 + w.tuple_bytes))))))::bigint END AS est_pages
  FROM idx i
  JOIN widths w ON w.idxoid = i.idxoid
 ORDER BY i.relpages DESC, i.idxoid`

// indexBloatGaps is what one index_bloat reading could not judge, so an
// index missing from its rows is no proof that it shrank: truncated is set
// when more indexes were above the floor than the row cap read, and
// unknown names (schema.index) the indexes read whose estimate is unknown
// (a key column without statistics).
type indexBloatGaps struct {
	truncated bool
	unknown   []string
}

// collectIndexBloat reads the index_bloat category in one bounded catalog
// statement. It asks for one row past the cap: when that row exists the
// reading is truncated. An index without complete key statistics is left
// out of the rows and named in the gaps. The rows are never nil on
// success: no index above the floor is an empty reading.
func (c *Collector) collectIndexBloat(
	ctx context.Context,
) ([]IndexBloat, indexBloatGaps, error) {
	var gaps indexBloatGaps
	rows, err := c.catalogQuery(ctx, indexBloatSQL,
		c.cfg.Analyzer.IndexBloatFloorBytes(), indexBloatRowCap+1)
	if err != nil {
		return nil, gaps, fmt.Errorf("collect index bloat: %w", err)
	}
	defer rows.Close()
	out := []IndexBloat{}
	for read := 1; rows.Next(); read++ {
		if read > indexBloatRowCap {
			gaps.truncated = true
			break
		}
		ib, ok, err := scanIndexBloat(rows)
		if err != nil {
			return nil, indexBloatGaps{}, fmt.Errorf("collect index bloat: scan: %w", err)
		}
		if ok {
			out = append(out, ib)
		} else {
			gaps.unknown = append(gaps.unknown, ib.SchemaName+"."+ib.IndexRelName)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, indexBloatGaps{}, fmt.Errorf("collect index bloat: %w", err)
	}
	return out, gaps, nil
}

type rowScanner interface{ Scan(dest ...any) error }

// scanIndexBloat reads one row; ok is false when the estimate is unknown
// (a key column without statistics), and only the names are then set.
func scanIndexBloat(row rowScanner) (IndexBloat, bool, error) {
	var ib IndexBloat
	var blockSize int64
	var est *int64
	if err := row.Scan(&ib.SchemaName, &ib.RelName, &ib.IndexRelName, &ib.IndexRelID,
		&ib.RelPages, &ib.Fillfactor, &ib.Reltuples, &blockSize, &est); err != nil {
		return IndexBloat{}, false, err
	}
	if est == nil || ib.RelPages <= 0 {
		return IndexBloat{SchemaName: ib.SchemaName, IndexRelName: ib.IndexRelName}, false, nil
	}
	ib.EstimatedPages = *est
	ib.IndexBytes = ib.RelPages * blockSize
	excess := max(ib.RelPages-ib.EstimatedPages, 0)
	ib.BloatBytes = excess * blockSize
	ib.BloatPct = math.Round(10000*float64(excess)/float64(ib.RelPages)) / 100
	return ib, true, nil
}
