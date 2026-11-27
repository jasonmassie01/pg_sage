package executor

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A failed REINDEX CONCURRENTLY leaves <name>_ccnew[N] (the unfinished
// copy) or, after the swap, <name>_ccold[N] (the retired original) INVALID
// on the table. The names its table had before the statement are recorded
// first, so only a suffix name the failed statement added is a candidate:
// an older x_ccnew of someone else's is never touched.
var reindexRemnantName = regexp.MustCompile(`^(.+)_cc(?:new|old)[0-9]*$`)

// reindexSnapshot is the reindexed table and its index names before the
// statement ran.
type reindexSnapshot struct {
	TableOID int64
	Names    map[string]bool
}

// reindex runs a REINDEX CONCURRENTLY and on failure drops the INVALID
// copies it added.
func (r remnantRun) reindex(ctx context.Context, sql string) (*remnantCleanup, error) {
	snap, snapErr := snapshotReindex(ctx, r.pool, sql)
	err := ExecConcurrently(ctx, r.pool, sql, r.timeout, r.opts...)
	if err == nil {
		return nil, nil
	}
	cleanup := skippedCleanup(err)
	if cleanup == nil {
		cleanup = r.cleanReindex(ctx, snap, snapErr)
	}
	r.log("cleanup after the failed %q: %s", sql, cleanup.summary())
	return cleanup, withCleanup(err, cleanup)
}

func (r remnantRun) cleanReindex(
	ctx context.Context, snap reindexSnapshot, snapErr error,
) *remnantCleanup {
	if snapErr != nil {
		return &remnantCleanup{Outcome: remnantUnbound, Reason: "the reindex was not " +
			"tracked (" + snapErr.Error() + "); a remnant is left to the invalid_index finding"}
	}
	candidates, err := snap.remnants(ctx, r.pool)
	if err != nil {
		return &remnantCleanup{Outcome: remnantUnbound, Reason: err.Error()}
	}
	if len(candidates) == 0 {
		return &remnantCleanup{Outcome: remnantNothingToDrop,
			Reason: "the reindex added no INVALID index"}
	}
	return r.dropRemnants(ctx, candidates)
}

// snapshotReindex records the table of a REINDEX INDEX|TABLE CONCURRENTLY
// and the names of its indexes.
func snapshotReindex(ctx context.Context, pool *pgxpool.Pool, sql string) (
	reindexSnapshot, error) {
	kind, target, err := parseReindexTarget(sql)
	if err != nil {
		return reindexSnapshot{}, err
	}
	query := `/* pg_sage executor */ SELECT c.oid::bigint FROM pg_class c
WHERE c.oid = to_regclass($1)`
	if kind == "INDEX" {
		query = `/* pg_sage executor */ SELECT i.indrelid::bigint FROM pg_index i
			WHERE i.indexrelid = to_regclass($1)`
	}
	snap := reindexSnapshot{Names: map[string]bool{}}
	if err := pool.QueryRow(ctx, query, target).Scan(&snap.TableOID); err != nil {
		return reindexSnapshot{}, fmt.Errorf("resolve the reindexed table of %s: %w", target, err)
	}
	rows, err := pool.Query(ctx, `/* pg_sage executor */ SELECT c.relname FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = $1`, snap.TableOID)
	if err != nil {
		return reindexSnapshot{}, fmt.Errorf("list the indexes of %s: %w", target, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return reindexSnapshot{}, fmt.Errorf("list the indexes of %s: %w", target, err)
	}
	for _, name := range names {
		snap.Names[name] = true
	}
	return snap, nil
}

// remnants are the INVALID indexes on the table whose _ccnew/_ccold name
// the failed statement added, derived from a name the table had.
func (s reindexSnapshot) remnants(ctx context.Context, pool *pgxpool.Pool) (
	[]remnantCandidate, error) {
	rows, err := pool.Query(ctx, `/* pg_sage executor */ SELECT n.nspname, c.relname, c.oid::bigint
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE i.indrelid = $1 AND NOT i.indisvalid ORDER BY c.oid`, s.TableOID)
	if err != nil {
		return nil, fmt.Errorf("list the reindex remnants: %w", err)
	}
	defer rows.Close()
	var out []remnantCandidate
	for rows.Next() {
		var nsp, rel string
		var oid int64
		if err := rows.Scan(&nsp, &rel, &oid); err != nil {
			return nil, fmt.Errorf("read a reindex remnant: %w", err)
		}
		if s.added(rel) {
			out = append(out, remnantCandidate{Index: pgx.Identifier{nsp, rel}.Sanitize(),
				OID: oid, TableOID: s.TableOID})
		}
	}
	return out, rows.Err()
}

// added reports a _ccnew/_ccold name absent before the statement whose
// stem (truncated by the server for long names) begins a name it had.
func (s reindexSnapshot) added(name string) bool {
	match := reindexRemnantName.FindStringSubmatch(name)
	if match == nil || s.Names[name] {
		return false
	}
	for existing := range s.Names {
		if strings.HasPrefix(existing, match[1]) {
			return true
		}
	}
	return false
}

// parseReindexTarget reads REINDEX [(options)] INDEX|TABLE CONCURRENTLY
// <schema>.<name> and returns the kind and the name as written; the
// target must be schema-qualified.
func parseReindexTarget(sql string) (string, string, error) {
	fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(normalizeSQLText(sql)), ";"))
	for i := 1; i+2 < len(fields); i++ {
		kind := strings.ToUpper(fields[i])
		if (kind != "INDEX" && kind != "TABLE") ||
			!strings.EqualFold(fields[i+1], "CONCURRENTLY") {
			continue
		}
		if i+3 != len(fields) {
			return "", "", fmt.Errorf("REINDEX target is not one name")
		}
		parts := splitQualifiedIdentifier(fields[i+2])
		if len(parts) != 2 {
			return "", "", fmt.Errorf("REINDEX target %s is not schema-qualified", fields[i+2])
		}
		// to_regclass folds and unquotes the name as the server does.
		return kind, fields[i+2], nil
	}
	return "", "", fmt.Errorf("not a REINDEX INDEX|TABLE CONCURRENTLY")
}
