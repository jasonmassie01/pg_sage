package probes

import (
	"fmt"
	"strconv"
)

// SequenceScanCap bounds how many sequences one sequence_runway statement
// reads. Reading a sequence's last value opens and locks it until the
// statement's transaction ends, so the read set is capped (performance
// gate, v1.8.3: 616-773 ms at 5,000 sequences, over the 500 ms incident
// budget). Past the cap the sequences with the smallest catalog capacity
// (effective limit minus minimum: small integer types, low MAXVALUEs,
// columns narrower than their sequence) are read first and the coverage
// columns say how many were not. A larger catalog is read in slices
// (Args.Slices): the runway monitor reads every slice on every pass.
const SequenceScanCap = 2000

// sequenceLockBudgetSQL is the share of the shared lock table one run may
// take: a quarter of max_locks_per_transaction x (max_connections +
// max_prepared_transactions). Found on PG14/PG18 with max_connections
// 100: 20,000 last-value reads (20,000 locks) made other sessions fail with
// "out of shared memory".
const sequenceLockBudgetSQL = `(pg_catalog.current_setting('max_locks_per_transaction')::int8 *
        (pg_catalog.current_setting('max_connections')::int8 +
         pg_catalog.current_setting('max_prepared_transactions')::int8) / 4)`

// sequenceSliceSQL keeps the sequences of slice $3 of $2 (all of them
// when $2 <= 1): a hash of the OID spreads any catalog evenly.
const sequenceSliceSQL = `($2::int4 <= 1 OR (pg_catalog.hashint8(q.seqrelid::pg_catalog.int8)
          ::pg_catalog.int8 & 2147483647) % $2::int4 = $3::int4)`

// SequenceTopN is how many sequences one sequence_runway statement lists
// by the share of their limit used; the keep-list (Args.Keep, at most
// MaxSequenceKeep) is listed beside them.
const SequenceTopN = 50

// sequenceTypeBoundSQL is the maximum (or minimum) of the integer type %s.
const sequenceTypeBoundSQL = `CASE %s WHEN 'pg_catalog.int2'::pg_catalog.regtype THEN %s
                WHEN 'pg_catalog.int4'::pg_catalog.regtype THEN %s
                WHEN 'pg_catalog.int8'::pg_catalog.regtype THEN %s
           END::int8`

func typeMax(col string) string {
	return fmt.Sprintf(sequenceTypeBoundSQL, col, "32767", "2147483647",
		"9223372036854775807")
}

func typeMin(col string) string {
	return fmt.Sprintf(sequenceTypeBoundSQL, col, "-32768", "-2147483648",
		"-9223372036854775808")
}

// sequenceRunwaySQL ranks the used sequences of one slice by the share of
// their effective limit used, in their direction of travel: an ascending
// sequence toward the lower of its maximum and the maximum of the integer
// column that owns it, from its minimum; a descending one toward the
// higher of its minimum and the column's minimum, from its maximum. It
// reads last values directly (pg_sequence_last_value), never the
// pg_sequences view joined by name; never-called sequences and other
// sessions' temporary ones are skipped, sequences the role may not read
// are counted. It lists the top SequenceTopN plus the kept sequences ($4,
// names as rendered here), which are also read first under the scan cap;
// listing_cut marks a used sequence left out.
// Every row carries the slice's coverage: sequences in it, read, used
// and unreadable; a slice without a used sequence answers one
// coverage_only row. Owner names are rendered for the returned rows only.
var sequenceRunwaySQL = `/* pg_sage sre:sequence_runway v4 */
WITH k AS (
    SELECT q.seqrelid, q.seqtypid, q.seqincrement, q.seqcycle, q.seqmin, q.seqmax,
           o.refobjid AS owner_rel, o.attname AS owner_att, o.atttypid AS owner_typid,
           ` + typeMax("o.atttypid") + ` AS owner_type_max,
           ` + typeMin("o.atttypid") + ` AS owner_type_min,
           pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname)
               = ANY ($4::text[]) AS kept
    FROM pg_catalog.pg_sequence q
    JOIN pg_catalog.pg_class c ON c.oid = q.seqrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN LATERAL (
        SELECT d.refobjid, a.attname, a.atttypid
        FROM pg_catalog.pg_depend d
        JOIN pg_catalog.pg_attribute a
            ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
        WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
          AND d.objid = q.seqrelid AND d.objsubid = 0
          AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
          AND d.refobjsubid > 0 AND d.deptype IN ('a', 'i')
        ORDER BY d.refobjid, d.refobjsubid
        LIMIT 1
    ) o ON true
    WHERE NOT pg_catalog.pg_is_other_temp_schema(c.relnamespace)
      AND ` + sequenceSliceSQL + `
), l AS (
    SELECT k.*, CASE WHEN k.seqincrement < 0
               THEN GREATEST(k.seqmin, COALESCE(k.owner_type_min, k.seqmin))
               ELSE LEAST(k.seqmax, COALESCE(k.owner_type_max, k.seqmax)) END
               AS effective_limit
    FROM k
), r AS (
    SELECT l.*, CASE WHEN l.seqincrement < 0 THEN l.seqmax::numeric - l.effective_limit
               ELSE l.effective_limit::numeric - l.seqmin END AS capacity
    FROM l
    ORDER BY l.kept DESC, capacity, l.seqrelid
    LIMIT LEAST(` + strconv.Itoa(SequenceScanCap) + `, ` + sequenceLockBudgetSQL + `)
), v AS (
    SELECT r.*, pg_catalog.has_sequence_privilege(r.seqrelid, 'SELECT,USAGE') AS readable
    FROM r
), w AS (
    SELECT v.*, CASE WHEN v.readable THEN pg_catalog.pg_sequence_last_value(
               v.seqrelid::pg_catalog.regclass) END AS last_value
    FROM v
), cov AS (
    SELECT (SELECT count(*) FROM k)::int8 AS sequences_total,
           count(*)::int8 AS sequences_scanned,
           count(w.last_value)::int8 AS sequences_used,
           count(*) FILTER (WHERE NOT w.readable)::int8 AS sequences_unreadable
    FROM w
), f AS (
    SELECT w.*, CASE WHEN w.seqincrement < 0 THEN w.seqmax::numeric - w.last_value
               ELSE w.last_value::numeric - w.seqmin END
               / NULLIF(w.capacity, 0) AS fraction_used
    FROM w WHERE w.last_value IS NOT NULL
), ranked AS (
    SELECT f.*, row_number() OVER (ORDER BY f.fraction_used DESC NULLS LAST, f.seqrelid)
               AS rank
    FROM f
), top AS (
    SELECT * FROM ranked
    WHERE ranked.rank <= ` + strconv.Itoa(SequenceTopN) + ` OR ranked.kept
    ORDER BY ranked.rank
    LIMIT $1
), cut AS (
    SELECT EXISTS (SELECT 1 FROM ranked
                   WHERE ranked.rank > ` + strconv.Itoa(SequenceTopN) + `
                     AND NOT ranked.kept) AS listing_cut
)
SELECT ` + fmt.Sprintf(relationName, "t.seqrelid") + ` AS sequence,
       pg_catalog.format_type(t.seqtypid, NULL) AS data_type,
       t.seqincrement AS increment_by, t.seqcycle AS cycle, t.last_value,
       t.seqmin AS min_value, t.seqmax AS max_value,
       ` + typeMax("t.seqtypid") + ` AS type_max,
       ` + typeMin("t.seqtypid") + ` AS type_min,
       ` + fmt.Sprintf(relationName, "t.owner_rel") + ` || '.' ||
           pg_catalog.quote_ident(t.owner_att) AS owner_column,
       pg_catalog.format_type(t.owner_typid, NULL) AS owner_type,
       t.owner_type_max, t.owner_type_min, t.effective_limit, t.fraction_used,
       c.sequences_total, c.sequences_scanned, c.sequences_used, c.sequences_unreadable,
       x.listing_cut, false AS coverage_only
FROM top t CROSS JOIN cov c CROSS JOIN cut x
UNION ALL
SELECT NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
       NULL, NULL, c.sequences_total, c.sequences_scanned, c.sequences_used,
       c.sequences_unreadable, false, true
FROM cov c WHERE NOT EXISTS (SELECT 1 FROM top)
ORDER BY coverage_only, fraction_used DESC NULLS LAST, sequence`

// sequenceRunwaySpec is v4 (forecast-05, forecast-10): v3 (performance
// gate, v1.8.3: one statement reads at most SequenceScanCap sequences of
// one slice and always reports its coverage, with a background budget
// for the runway monitor's slow-cadence sampling) plus descending
// sequences and the keep-list, listed beside the top SequenceTopN.
func sequenceRunwaySpec() Spec {
	s := capped(spec(SequenceRunwayProbe, FamilySequences, ArgsSlice,
		Variant{MinVersion: 140000, SQL: sequenceRunwaySQL}), SequenceTopN+MaxSequenceKeep)
	s.Version = "v4"
	s.BackgroundTimeout = MaxBackgroundStatementTimeout
	return s
}
