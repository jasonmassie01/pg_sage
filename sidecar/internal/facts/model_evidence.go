package facts

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// schemaSummarySQL summarizes the largest user schemas: tables, activity
// and size. pg_sage's own and the system schemas are never shown.
const schemaSummarySQL = `/* pg_sage */
SELECT n.nspname::text, count(c.oid)::int,
       COALESCE(sum(COALESCE(s.seq_scan, 0) + COALESCE(s.idx_scan, 0) +
                    COALESCE(s.n_tup_ins, 0) + COALESCE(s.n_tup_upd, 0) +
                    COALESCE(s.n_tup_del, 0)), 0)::bigint,
       COALESCE(sum(COALESCE(s.n_tup_upd, 0) + COALESCE(s.n_tup_del, 0)), 0)::bigint
FROM pg_catalog.pg_namespace n
JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE n.nspname !~ '^pg_' AND n.nspname NOT IN ('information_schema', 'sage')
GROUP BY n.nspname
ORDER BY count(c.oid) DESC, n.nspname
LIMIT 20`

// CollectModelEvidence gathers the bounded, citable catalog observations
// the model proposes facts from: schema summaries, logical slots and the
// indexes pg_sage dropped that came back.
func CollectModelEvidence(ctx context.Context, pool *pgxpool.Pool) ([]EvidenceItem, error) {
	var out []EvidenceItem
	add := func(kind, ref, detail string) {
		if len(out) < maxModelEvidence {
			out = append(out, EvidenceItem{ID: fmt.Sprintf("E%d", len(out)+1), Kind: kind,
				Ref: ref, Detail: detail})
		}
	}
	if err := schemaEvidence(ctx, pool, add); err != nil {
		return nil, err
	}
	slots, err := slotConsumerDetector{pool: pool}.Detect(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range slots {
		add("slot", p.Evidence[0].Ref, p.Evidence[0].Detail)
	}
	managed, err := analyzer.AppManagedIndexes(ctx, catalogread.New(pool,
		catalogread.Default()))
	if err != nil {
		return nil, fmt.Errorf("app-managed indexes: %w", err)
	}
	keys := make([]string, 0, len(managed))
	for k := range managed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("action_log", "drop_index:"+k, fmt.Sprintf("dropped by pg_sage %d times, "+
			"recreated with the same definition each time", managed[k].Drops))
	}
	return out, nil
}

func schemaEvidence(ctx context.Context, pool *pgxpool.Pool,
	add func(kind, ref, detail string)) error {
	rows, err := pool.Query(ctx, schemaSummarySQL)
	if err != nil {
		return fmt.Errorf("summarize schemas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var tables int
		var activity, changes int64
		if err := rows.Scan(&name, &tables, &activity, &changes); err != nil {
			return fmt.Errorf("scan schema summary: %w", err)
		}
		add("catalog", "schema:"+quoteIdent(name), fmt.Sprintf("%d tables, %d scans and "+
			"writes since statistics reset, %d updates or deletes", tables, activity,
			changes))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("summarize schemas: %w", err)
	}
	return nil
}
