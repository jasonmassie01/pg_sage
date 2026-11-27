package probes

import "fmt"

// TableFreezeHorizonSQL (v1) reads one table's freeze horizon ($1, a
// regclass name) the way freezeRankCTE ranks it for the custodian: each
// of the heap and its TOAST relation is measured against its effective
// maximum (its reloption when lower than the setting), and the one that
// has used the larger share is returned with its ages and maxima. A
// relation without storage of its own (a partitioned parent,
// relfrozenxid 0) returns no row. Columns: xid_age, freeze_max_age,
// mxid_age, mxid_freeze_max_age.
var TableFreezeHorizonSQL = `/* pg_sage sre:table_freeze_horizon v1 */
WITH g AS (
    SELECT pg_catalog.current_setting('autovacuum_freeze_max_age')::int8 AS xid_max,
           pg_catalog.current_setting('autovacuum_multixact_freeze_max_age')::int8
               AS mxid_max
), rel AS (
    SELECT o.oid FROM pg_catalog.pg_class o WHERE o.oid = pg_catalog.to_regclass($1)
    UNION ALL
    SELECT o.reltoastrelid FROM pg_catalog.pg_class o
    WHERE o.oid = pg_catalog.to_regclass($1) AND o.reltoastrelid <> 0
), t AS (
    SELECT pg_catalog.age(c.relfrozenxid)::int8 AS xid_age,
           pg_catalog.mxid_age(c.relminmxid)::int8 AS mxid_age,
           LEAST(g.xid_max, COALESCE((` + fmt.Sprintf(reloptionSQL, "c",
	"autovacuum_freeze_max_age") + `)::int8, g.xid_max)) AS freeze_max_age,
           LEAST(g.mxid_max, COALESCE((` + fmt.Sprintf(reloptionSQL, "c",
	"autovacuum_multixact_freeze_max_age") + `)::int8, g.mxid_max)) AS mxid_freeze_max_age
    FROM rel JOIN pg_catalog.pg_class c ON c.oid = rel.oid CROSS JOIN g
    WHERE c.relfrozenxid <> '0'::xid
)
SELECT xid_age, freeze_max_age, mxid_age, mxid_freeze_max_age FROM t
ORDER BY GREATEST(xid_age::float8 / NULLIF(freeze_max_age, 0),
                  mxid_age::float8 / NULLIF(mxid_freeze_max_age, 0)) DESC NULLS LAST
LIMIT 1`
