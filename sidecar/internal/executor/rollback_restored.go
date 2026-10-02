package executor

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/value"
)

// Rollback target states for a CREATE INDEX rollback (dogfood lifeos-1:
// rollbacks failed with 42P07 because the application had already
// recreated the dropped index).
const (
	rollbackTargetAbsent = iota
	rollbackTargetSame
	rollbackTargetDifferent
)

var indexDefNoise = regexp.MustCompile(`(?i)\b(CONCURRENTLY|IF\s+NOT\s+EXISTS)\b`)

// sameIndexDefinition compares two CREATE INDEX statements ignoring
// CONCURRENTLY, IF NOT EXISTS, case, a trailing semicolon and whitespace
// (including inside the column list).
func sameIndexDefinition(a, b string) bool {
	na, nb := normalizeIndexDef(a), normalizeIndexDef(b)
	return na != "" && na == nb
}

func normalizeIndexDef(sql string) string {
	s := strings.TrimSuffix(strings.TrimSpace(sql), ";")
	s = indexDefNoise.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
	s = strings.ReplaceAll(s, ", ", ",")
	return strings.ReplaceAll(s, "( ", "(")
}

const existingIndexDefSQL = `/* pg_sage */
SELECT pg_catalog.pg_get_indexdef(i.oid)
FROM pg_catalog.pg_class i
JOIN pg_catalog.pg_class t ON t.oid = pg_catalog.to_regclass($1)
WHERE i.relkind IN ('i', 'I') AND i.relnamespace = t.relnamespace AND i.relname = $2`

// rollbackIndexTarget reports whether the index a CREATE INDEX rollback
// would create already exists, with the same or another definition.
// Other rollbacks are absent (nothing to compare).
func rollbackIndexTarget(ctx context.Context, pool *pgxpool.Pool,
	rollbackSQL string) (int, string, error) {
	name := createIndexIdentifier(rollbackSQL)
	schema, table, _, ok := parseCreateIndexTarget(rollbackSQL)
	if pool == nil || name == "" || !ok {
		return rollbackTargetAbsent, "", nil
	}
	target := pgx.Identifier{table}.Sanitize()
	if schema != "" {
		target = pgx.Identifier{schema, table}.Sanitize()
	}
	var def string
	err := pool.QueryRow(ctx, existingIndexDefSQL, target, name).Scan(&def)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return rollbackTargetAbsent, "", nil
	case err != nil:
		return rollbackTargetAbsent, "", err
	case sameIndexDefinition(def, rollbackSQL):
		return rollbackTargetSame, def, nil
	}
	return rollbackTargetDifferent, def, nil
}

// restoredOutside settles a rollback whose target index already exists:
// with the same definition the action is already_restored (reverted, not
// credited); with another definition the rollback fails, naming the
// conflict, and the existing index is left alone. It reports whether the
// action was settled.
func restoredOutside(ctx context.Context, pool *pgxpool.Pool, actionID int64,
	rollbackSQL string, logFn func(string, string, ...any)) bool {
	state, def, err := rollbackIndexTarget(ctx, pool, rollbackSQL)
	if err != nil {
		logFn("rollback", "action %d: could not read the rollback target: %v", actionID, err)
		return false
	}
	switch state {
	case rollbackTargetSame:
		reason := "the index is already back with the same definition (recreated " +
			"outside pg_sage); nothing to roll back"
		logFn("rollback", "action %d: %s", actionID, reason)
		updateActionOutcome(ctx, pool, actionID, "already_restored", reason)
		_, _ = finalizeActionVerification(ctx, pool, actionID, "revert", reason)
		_, _ = value.NewPostgresRepository(pool).ZeroCreditOnRevert(ctx, actionID,
			"already_restored")
		return true
	case rollbackTargetDifferent:
		reason := "rollback not run: an index of that name already exists with a " +
			"different definition (" + def + "); it was not replaced"
		logFn("rollback", "action %d: %s", actionID, reason)
		updateActionOutcome(ctx, pool, actionID, "rollback_failed", reason)
		return true
	}
	return false
}
