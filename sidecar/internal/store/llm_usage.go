package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LLMUsageTimeout bounds every ledger statement, so an unreachable control
// database never stalls a flush or a startup restore for long.
const LLMUsageTimeout = 5 * time.Second

// LLMUsageRow is one counter of today's LLM token spend (fleet-10).
// DatabaseKey is id:<id>, name:<name> or process; Role is general,
// optimizer, shared or fleet.
type LLMUsageRow struct {
	DatabaseKey string
	Role        string
	Tokens      int64
}

var llmUsageRoles = map[string]bool{
	"general": true, "optimizer": true, "shared": true, "fleet": true,
}

// LLMUsageStore reads and writes sage.llm_usage in the control database.
type LLMUsageStore struct {
	pool *pgxpool.Pool
}

// NewLLMUsageStore returns a store over the control database pool.
func NewLLMUsageStore(pool *pgxpool.Pool) *LLMUsageStore {
	return &LLMUsageStore{pool: pool}
}

var errNoControlPool = errors.New("LLM usage ledger has no control database pool")

const flushLLMUsageSQL = `
INSERT INTO sage.llm_usage (day, database_key, role, tokens, updated_at)
SELECT $1::date, u.database_key, u.role, u.tokens, now()
  FROM unnest($2::text[], $3::text[], $4::bigint[]) AS u(database_key, role, tokens)
ON CONFLICT (day, database_key, role, purpose, model) DO UPDATE
   SET tokens = GREATEST(sage.llm_usage.tokens, EXCLUDED.tokens),
       updated_at = now()
 WHERE EXCLUDED.tokens > sage.llm_usage.tokens`

// Flush records day's counters. It only ever raises a stored count, so a
// repeated or stale flush is harmless. Rows are validated before any SQL.
func (s *LLMUsageStore) Flush(ctx context.Context, day time.Time, rows []LLMUsageRow) error {
	if err := validateLLMUsageRows(rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if s.pool == nil {
		return errNoControlPool
	}
	rows = mergeLLMUsageRows(rows)
	keys := make([]string, len(rows))
	roles := make([]string, len(rows))
	tokens := make([]int64, len(rows))
	for i, row := range rows {
		keys[i], roles[i], tokens[i] = row.DatabaseKey, row.Role, row.Tokens
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	if _, err := s.pool.Exec(qctx, flushLLMUsageSQL, utcDate(day), keys, roles,
		tokens); err != nil {
		return fmt.Errorf("flush %d LLM usage rows for %s: %w", len(rows),
			utcDate(day).Format(time.DateOnly), err)
	}
	return nil
}

// Load returns every budget counter (purpose '_total') recorded for day.
func (s *LLMUsageStore) Load(ctx context.Context, day time.Time) ([]LLMUsageRow, error) {
	if s.pool == nil {
		return nil, errNoControlPool
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, `SELECT database_key, role, tokens
		FROM sage.llm_usage WHERE day = $1::date AND purpose = '_total'`, utcDate(day))
	if err != nil {
		return nil, fmt.Errorf("load LLM usage for %s: %w",
			utcDate(day).Format(time.DateOnly), err)
	}
	defer rows.Close()
	var out []LLMUsageRow
	for rows.Next() {
		var row LLMUsageRow
		if err := rows.Scan(&row.DatabaseKey, &row.Role, &row.Tokens); err != nil {
			return nil, fmt.Errorf("scan LLM usage row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read LLM usage for %s: %w",
			utcDate(day).Format(time.DateOnly), err)
	}
	return out, nil
}

// Clear deletes day's budget counters (an admin budget reset); the
// attributed usage of llm-04 is history and stays.
func (s *LLMUsageStore) Clear(ctx context.Context, day time.Time) error {
	if s.pool == nil {
		return errNoControlPool
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	if _, err := s.pool.Exec(qctx, `DELETE FROM sage.llm_usage
		WHERE day = $1::date AND purpose = '_total'`,
		utcDate(day)); err != nil {
		return fmt.Errorf("clear LLM usage for %s: %w",
			utcDate(day).Format(time.DateOnly), err)
	}
	return nil
}

// Prune deletes the budget counters of days before cutoff and returns how
// many; PruneAttribution keeps the attributed usage on its own retention.
func (s *LLMUsageStore) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.pool == nil {
		return 0, errNoControlPool
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `DELETE FROM sage.llm_usage
		WHERE day < $1::date AND purpose = '_total'`,
		utcDate(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune LLM usage before %s: %w",
			utcDate(cutoff).Format(time.DateOnly), err)
	}
	return tag.RowsAffected(), nil
}

func validateLLMUsageRows(rows []LLMUsageRow) error {
	for _, row := range rows {
		switch {
		case row.DatabaseKey == "":
			return fmt.Errorf("LLM usage row %+v: empty database key", row)
		case !llmUsageRoles[row.Role]:
			return fmt.Errorf("LLM usage row %+v: unknown role %q "+
				"(want general, optimizer, shared or fleet)", row, row.Role)
		case row.Tokens < 0:
			return fmt.Errorf("LLM usage row %+v: negative token count", row)
		}
	}
	return nil
}

// mergeLLMUsageRows keeps the highest count per key and role: one upsert
// statement cannot touch the same row twice.
func mergeLLMUsageRows(rows []LLMUsageRow) []LLMUsageRow {
	type key struct{ database, role string }
	index := make(map[key]int, len(rows))
	out := make([]LLMUsageRow, 0, len(rows))
	for _, row := range rows {
		k := key{row.DatabaseKey, row.Role}
		if i, ok := index[k]; ok {
			out[i].Tokens = max(out[i].Tokens, row.Tokens)
			continue
		}
		index[k] = len(out)
		out = append(out, row)
	}
	return out
}

// utcDate is the UTC calendar day of t at midnight, as the date column
// stores it.
func utcDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
