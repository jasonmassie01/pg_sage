package probes

import (
	"fmt"
	"strconv"
)

// SequenceScanCap bounds how many sequences one sequence_runway run
// reads. Reading a sequence's last value opens and locks it until the
// probe's transaction ends, so the read set is capped: lifeos (12,038
// sequences) is read whole; past the cap the sequences with the
// smallest catalog capacity (effective limit minus minimum: small
// integer types, low MAXVALUEs, columns narrower than their sequence)
// are read first, and the coverage columns say how many were not.
const SequenceScanCap = 20000

// sequenceRunwaySQL ranks the used ascending sequences by the share of
// their effective limit used: the lower of the sequence's maximum and
// the maximum of the integer column that owns it. It reads last values
// directly (pg_sequence_last_value), never the pg_sequences view joined
// by name; never-called sequences and other sessions' temporary ones are
// skipped, sequences the role may not read are counted. Every row
// carries the coverage: sequences in the catalog, read, used and
// unreadable. Owner names are rendered for the returned rows only.
// Descending sequences are not covered.
var sequenceRunwaySQL = `/* pg_sage sre:sequence_runway v2 */
WITH k AS (
    SELECT q.seqrelid, q.seqtypid, q.seqincrement, q.seqcycle, q.seqmin, q.seqmax,
           o.refobjid AS owner_rel, o.attname AS owner_att, o.atttypid AS owner_typid,
           CASE o.atttypid WHEN 'pg_catalog.int2'::pg_catalog.regtype THEN 32767
                WHEN 'pg_catalog.int4'::pg_catalog.regtype THEN 2147483647
                WHEN 'pg_catalog.int8'::pg_catalog.regtype THEN 9223372036854775807
           END::int8 AS owner_type_max
    FROM pg_catalog.pg_sequence q
    JOIN pg_catalog.pg_class c ON c.oid = q.seqrelid
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
    WHERE q.seqincrement > 0 AND NOT pg_catalog.pg_is_other_temp_schema(c.relnamespace)
), r AS (
    SELECT k.*, LEAST(k.seqmax, COALESCE(k.owner_type_max, k.seqmax)) AS effective_limit,
           count(*) OVER () AS sequences_total
    FROM k
    ORDER BY LEAST(k.seqmax, COALESCE(k.owner_type_max, k.seqmax))::numeric - k.seqmin,
             k.seqrelid
    LIMIT ` + strconv.Itoa(SequenceScanCap) + `
), v AS (
    SELECT r.*, pg_catalog.has_sequence_privilege(r.seqrelid, 'SELECT,USAGE') AS readable
    FROM r
), w AS (
    SELECT v.*, CASE WHEN v.readable THEN pg_catalog.pg_sequence_last_value(
               v.seqrelid::pg_catalog.regclass) END AS last_value
    FROM v
), u AS (
    SELECT w.*, count(*) OVER () AS sequences_scanned,
           count(*) FILTER (WHERE NOT w.readable) OVER () AS sequences_unreadable,
           count(w.last_value) OVER () AS sequences_used,
           (w.last_value::numeric - w.seqmin)
               / NULLIF(w.effective_limit::numeric - w.seqmin, 0) AS fraction_used
    FROM w
), top AS (
    SELECT u.* FROM u WHERE u.last_value IS NOT NULL
    ORDER BY u.fraction_used DESC NULLS LAST, u.seqrelid
    LIMIT $1
)
SELECT ` + fmt.Sprintf(relationName, "t.seqrelid") + ` AS sequence,
       pg_catalog.format_type(t.seqtypid, NULL) AS data_type,
       t.seqincrement AS increment_by, t.seqcycle AS cycle, t.last_value,
       t.seqmin AS min_value, t.seqmax AS max_value,
       CASE t.seqtypid WHEN 'pg_catalog.int2'::pg_catalog.regtype THEN 32767
            WHEN 'pg_catalog.int4'::pg_catalog.regtype THEN 2147483647
            ELSE 9223372036854775807 END::int8 AS type_max,
       ` + fmt.Sprintf(relationName, "t.owner_rel") + ` || '.' ||
           pg_catalog.quote_ident(t.owner_att) AS owner_column,
       pg_catalog.format_type(t.owner_typid, NULL) AS owner_type,
       t.owner_type_max, t.effective_limit, t.fraction_used,
       t.sequences_total, t.sequences_scanned, t.sequences_used, t.sequences_unreadable
FROM top t
ORDER BY t.fraction_used DESC NULLS LAST, t.seqrelid`

// sequenceRunwaySpec is v2 (dogfood lifeos-1): bounded, coverage-
// reporting, with a background budget for the runway monitor's
// slow-cadence sampling.
func sequenceRunwaySpec() Spec {
	s := capped(spec(SequenceRunwayProbe, FamilySequences, ArgsNone,
		Variant{MinVersion: 140000, SQL: sequenceRunwaySQL}), 50)
	s.Version = "v2"
	s.BackgroundTimeout = MaxBackgroundStatementTimeout
	return s
}
