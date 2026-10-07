package autonomy

// unboundedAppendSQL emits one row per contracted table: the newest
// contract wins, so repeated declarations (NULL database_id rows are not
// deduplicated by the UNIQUE constraint) cannot yield duplicate invariants.
// The declared column is returned only when it is live and temporal; the
// created_at/occurred_at heuristic is returned separately as a suggestion
// for the owner and is never used to delete.
const unboundedAppendSQL = `/* pg_sage */
SELECT tc.schema_name, tc.table_name,
       COALESCE(declared.attname::text,''), COALESCE(suggested.attname::text,'')
FROM (
    SELECT DISTINCT ON (schema_name, table_name)
           schema_name, table_name, append_only, retention_interval, retention_column
    FROM sage.table_contract
    ORDER BY schema_name, table_name, updated_at DESC, id DESC
) tc
JOIN pg_namespace ns ON ns.nspname=tc.schema_name
JOIN pg_class tbl ON tbl.relnamespace=ns.oid AND tbl.relname=tc.table_name
LEFT JOIN LATERAL (
    SELECT att.attname
    FROM pg_attribute att
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE att.attrelid=tbl.oid AND att.attnum>0 AND NOT att.attisdropped
      AND typ.typname IN ('timestamp','timestamptz','date')
      AND att.attname=tc.retention_column
) declared ON true
LEFT JOIN LATERAL (
    SELECT att.attname
    FROM pg_attribute att
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE att.attrelid=tbl.oid AND att.attnum>0 AND NOT att.attisdropped
      AND typ.typname IN ('timestamp','timestamptz','date')
      AND att.attname IN ('created_at', 'occurred_at')
    ORDER BY CASE att.attname WHEN 'created_at' THEN 0 ELSE 1 END
    LIMIT 1
) suggested ON true
WHERE tc.append_only AND tc.retention_interval IS NOT NULL
  AND tbl.relkind IN ('r','p')
ORDER BY tc.schema_name, tc.table_name`

// structuralPathologySQL aggregates each table's columns once: every
// column text (three or more), and text columns named like a number. A
// window over every column of every table spilled to disk at 5,000
// relations (perf gate, 111 ms on CI).
const structuralPathologySQL = `/* pg_sage */
WITH tables AS (
    SELECT ns.nspname AS schema_name, tbl.relname AS table_name,
           count(*) AS column_count,
           count(*) FILTER (WHERE typ.typname IN ('text','varchar')) AS text_count,
           array_agg(att.attname) FILTER (WHERE typ.typname IN ('text','varchar')
             AND (att.attname='count_text' OR att.attname ~ '(_id|_count|_number)$'))
             AS tightening
    FROM pg_class tbl
    JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
    JOIN pg_attribute att ON att.attrelid=tbl.oid
      AND att.attnum>0 AND NOT att.attisdropped
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE tbl.relkind IN ('r','p')
      AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
    GROUP BY tbl.oid, ns.nspname, tbl.relname
)
SELECT schema_name, table_name, ''::name AS column_name, 'everything_text' AS kind
FROM tables WHERE column_count>=3 AND text_count=column_count
UNION ALL
SELECT schema_name, table_name, unnest(tightening), 'type_tightening' AS kind
FROM tables WHERE tightening IS NOT NULL
ORDER BY 1,2,4,3`

const missingFKIndexSQL = `/* pg_sage */
SELECT ns.nspname, tbl.relname, con.conname,
       array_agg(att.attname::text ORDER BY keys.ordinality)
FROM pg_constraint con
JOIN pg_class tbl ON tbl.oid=con.conrelid
JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
JOIN unnest(con.conkey) WITH ORDINALITY keys(attnum, ordinality) ON true
JOIN pg_attribute att ON att.attrelid=con.conrelid AND att.attnum=keys.attnum
WHERE con.contype='f'
  AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
  AND NOT EXISTS (
      SELECT 1 FROM pg_index idx
      WHERE idx.indrelid=con.conrelid AND idx.indisvalid
        AND con.conkey <@ idx.indkey::smallint[])
GROUP BY ns.nspname, tbl.relname, con.conname
ORDER BY ns.nspname, tbl.relname, con.conname`
