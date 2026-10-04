package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Dogfood round 2 item 5 (lifeos 2026-10-04): 1.8.5 queued a legacy
// graph_nodes (node_type, name) create beside an index on (node_type,
// name) INCLUDE (id), and kept a proposal whose finding was resolved.
// The coverage rule (optimizer.CoveredBy) now applies before any index
// create is queued or run, and pending proposals whose reason is gone are
// superseded with that reason. Supersession and expiry are pg_sage's own
// housekeeping: the trust ledger counts only an operator's rejection.

// tableIndexDefsSQL lists the valid, ready indexes of a table.
const tableIndexDefsSQL = `/* pg_sage */ SELECT c.relname, pg_get_indexdef(i.indexrelid)
	FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
	WHERE i.indrelid = to_regclass(CASE WHEN $1 = '' THEN quote_ident($2)
	                                    ELSE quote_ident($1) || '.' || quote_ident($2) END)
	  AND i.indisvalid AND i.indisready
	ORDER BY c.relname`

// coveringIndex names a valid index that already covers the CREATE INDEX
// sql ("" when none does, or sql is not an index create).
func (e *Executor) coveringIndex(ctx context.Context, sql string) (string, error) {
	if categorizeAction(sql) != "create_index" {
		return "", nil
	}
	spec, err := optimizer.ParseIndexDDL(sql)
	if err != nil {
		return "", nil
	}
	rows, err := e.pool.Query(ctx, tableIndexDefsSQL, spec.TableSchema, spec.TableName)
	if err != nil {
		return "", fmt.Errorf("list indexes of %s: %w", spec.TableName, err)
	}
	type indexDef struct{ name, def string }
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (indexDef, error) {
		var d indexDef
		return d, r.Scan(&d.name, &d.def)
	})
	if err != nil {
		return "", fmt.Errorf("list indexes of %s: %w", spec.TableName, err)
	}
	for _, d := range defs {
		if optimizer.CoveredBy(sql, d.def) {
			return d.name, nil
		}
	}
	return "", nil
}

// skipCoveredCreate rules out a background index create an existing index
// already covers: before the gate (no decision, no queue item, no run),
// its finding resolves naming the index. A failed catalog read never
// skips (the gate and the executor's own checks still apply).
func (e *Executor) skipCoveredCreate(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	index, err := e.coveringIndex(ctx, f.RecommendedSQL)
	if err != nil {
		e.logFn("executor", "coverage check for %q: %v", f.Title, err)
		return false
	}
	if index == "" {
		return false
	}
	_, err = e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.findings
		SET status = 'resolved', resolved_at = now(),
		    detail = COALESCE(detail, '{}'::jsonb) || jsonb_build_object('covered_by', $2::text)
		WHERE id = $1 AND status = 'open' AND acted_on_at IS NULL`, findingID, index)
	if err != nil {
		e.logFn("executor", "resolve covered finding %d: %v", findingID, err)
	}
	e.logFn("executor", "not proposing %q: index %s already covers it", f.Title, index)
	return true
}

// supersedeGoneSQL closes pending proposals whose finding is no longer
// open, and those an equivalent newer pending proposal replaces (same
// identity key, or the same SQL).
const supersedeGoneSQL = `/* pg_sage */ UPDATE sage.action_queue q
	SET status = 'superseded',
	    reason = 'superseded: finding ' || q.finding_id || ' is no longer open'
	WHERE q.status = 'pending' AND q.finding_id IS NOT NULL
	  AND NOT EXISTS (SELECT 1 FROM sage.findings f
	                  WHERE f.id = q.finding_id AND f.status = 'open'
	                    AND f.resolved_at IS NULL)
	RETURNING q.id`

const supersedeNewerSQL = `/* pg_sage */ WITH newer AS (
		SELECT q.id, max(n.id) AS by
		FROM sage.action_queue q JOIN sage.action_queue n
		  ON n.status = 'pending' AND n.id > q.id
		 AND n.database_id IS NOT DISTINCT FROM q.database_id
		 AND ((q.identity_key IS NOT NULL AND n.identity_key = q.identity_key)
		      OR btrim(n.proposed_sql) = btrim(q.proposed_sql))
		WHERE q.status = 'pending'
		GROUP BY q.id)
	UPDATE sage.action_queue q
	SET status = 'superseded',
	    reason = 'superseded by the newer equivalent proposal, queue item ' || newer.by
	FROM newer WHERE q.id = newer.id AND q.status = 'pending'
	RETURNING q.id`

// pendingCreatesSQL lists pending index-create proposals for the coverage
// check.
const pendingCreatesSQL = `/* pg_sage */ SELECT id, proposed_sql FROM sage.action_queue
	WHERE status = 'pending' AND proposed_sql ~* '^\s*CREATE\s+(UNIQUE\s+)?INDEX\s'
	ORDER BY id`

// supersedeStaleApprovals closes the pending proposals whose reason is
// gone and returns how many it closed. The approval card follow-up posts
// each closed item's reason to the chat it went to.
func (e *Executor) supersedeStaleApprovals(ctx context.Context) (int, error) {
	if e.pool == nil {
		return 0, nil
	}
	total := 0
	for _, stmt := range []string{supersedeGoneSQL, supersedeNewerSQL} {
		rows, err := e.pool.Query(ctx, stmt)
		if err != nil {
			return total, fmt.Errorf("supersede stale proposals: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return total, fmt.Errorf("supersede stale proposals: %w", err)
		}
		total += len(ids)
	}
	covered, err := e.supersedeCoveredCreates(ctx)
	return total + covered, err
}

// supersedeCoveredCreates closes pending index creates an existing index
// already covers.
func (e *Executor) supersedeCoveredCreates(ctx context.Context) (int, error) {
	rows, err := e.pool.Query(ctx, pendingCreatesSQL)
	if err != nil {
		return 0, fmt.Errorf("list pending index proposals: %w", err)
	}
	type pending struct {
		id  int64
		sql string
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
		var p pending
		return p, r.Scan(&p.id, &p.sql)
	})
	if err != nil {
		return 0, fmt.Errorf("list pending index proposals: %w", err)
	}
	n := 0
	for _, p := range items {
		index, err := e.coveringIndex(ctx, strings.TrimSpace(p.sql))
		if err != nil {
			e.logFn("executor", "coverage check of queued proposal %d: %v", p.id, err)
			continue
		}
		if index == "" {
			continue
		}
		tag, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
			SET status = 'superseded', reason = $2 WHERE id = $1 AND status = 'pending'`,
			p.id, "superseded: index "+index+" already covers this index")
		if err != nil {
			return n, fmt.Errorf("supersede covered proposal %d: %w", p.id, err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}
