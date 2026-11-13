package analyzer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// lookupProcs reads the volatility of every pg_proc entry named in names.
func lookupProcs(ctx context.Context, pool *pgxpool.Pool, names []string) ([]procInfo, error) {
	if pool == nil {
		return nil, errors.New("no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `/* pg_sage analyzer */ SELECT DISTINCT p.proname::text,
		p.provolatile::text FROM pg_catalog.pg_proc p WHERE p.proname = ANY($1::text[])`,
		names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (procInfo, error) {
		var p procInfo
		err := r.Scan(&p.Name, &p.Volatility)
		return p, err
	})
}

// checkSessionSQL bounds the check's transaction: its own statement
// timeout, a 100 ms lock timeout and the pg_sage application name.
const checkSessionSQL = `/* pg_sage analyzer */ SELECT
	set_config('statement_timeout', $1, true),
	set_config('lock_timeout', '100ms', true),
	set_config('application_name', 'pg_sage', true)`

// execCustomCheck runs the canonical text in a READ ONLY transaction and
// reads at most max_rows + 1 rows. The transaction always rolls back.
func (a *Analyzer) execCustomCheck(
	ctx context.Context, p config.PreparedCheck,
) ([]string, [][]any, error) {
	if a.pool == nil {
		return nil, nil, errors.New("no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx,
		time.Duration(p.TimeoutMs)*time.Millisecond+5*time.Second)
	defer cancel()
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, checkSessionSQL, fmt.Sprintf("%dms", p.TimeoutMs)); err != nil {
		return nil, nil, fmt.Errorf("bound the check's transaction: %w", err)
	}
	rows, err := tx.Query(ctx, p.Canonical)
	if err != nil {
		return nil, nil, fmt.Errorf("run the check: %w", err)
	}
	defer rows.Close()
	var cols []string
	for _, fd := range rows.FieldDescriptions() {
		cols = append(cols, fd.Name)
	}
	var data [][]any
	for len(data) <= p.MaxRows && rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, fmt.Errorf("read a check row: %w", err)
		}
		data = append(data, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("run the check: %w", err)
	}
	return cols, data, nil
}

// checkFindings turns rows into custom:<id> findings keyed by the object
// column, keeping the first max_rows. Rows with an empty object or one
// already seen are skipped. A custom finding never carries SQL.
func checkFindings(
	p config.PreparedCheck, cols []string, rows [][]any,
) ([]Finding, bool, error) {
	idx := -1
	for i, c := range cols {
		if c == p.ObjectColumn {
			idx = i
		}
	}
	if idx < 0 {
		return nil, false, fmt.Errorf("object_column %q is not a result column (got %v)",
			p.ObjectColumn, cols)
	}
	truncated := len(rows) > p.MaxRows
	if truncated {
		rows = rows[:p.MaxRows]
	}
	seen := map[string]bool{}
	var out []Finding
	for _, row := range rows {
		obj := checkValue(row[idx])
		if obj == "" || seen[obj] {
			continue
		}
		seen[obj] = true
		out = append(out, customFinding(p, obj, cols, row, truncated))
	}
	return out, truncated, nil
}

func customFinding(p config.PreparedCheck, obj string, cols []string, row []any,
	truncated bool) Finding {
	values := make(map[string]any, len(cols))
	for i, c := range cols {
		values[c] = checkValue(row[i])
	}
	return Finding{
		Category: p.Category(), Severity: p.Severity, ObjectType: "custom_check",
		ObjectIdentifier: obj, Title: p.Title + ": " + obj,
		Recommendation: p.Message,
		Detail: map[string]any{"check_id": p.ID, "fingerprint": p.Fingerprint,
			"row": values, "truncated": truncated},
	}
}

// checkValue renders one result value as text ("" for NULL).
func checkValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(t)
	}
}
