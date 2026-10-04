package earned

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Set-based evidence reads (roadmap 1.2). Each read is one statement for
// a scope: one pair (a promotion, an authorization) or every pair of the
// database (the Trust view, read by the Trust page and every approval
// card). A pair's read is the database read filtered to that pair, so the
// two can never disagree.

// pairScope selects one family (and class) or, zero, the whole database.
type pairScope struct {
	family Family
	class  ActionClass
}

func (p pairScope) all() bool { return p.family == "" }

// scoped puts the scope's filter in a statement's "/*scope*/" slot and
// returns its arguments after base; withClass filters on the class too.
func scoped(sql string, p pairScope, withClass bool, base ...any) (string, []any) {
	if p.all() {
		return strings.Replace(sql, "/*scope*/", "", 1), base
	}
	n := len(base)
	filter := " AND family = $" + strconv.Itoa(n+1)
	args := append(base, string(p.family))
	if withClass {
		filter += " AND action_class = $" + strconv.Itoa(n+2)
		args = append(args, string(p.class))
	}
	return strings.Replace(sql, "/*scope*/", filter, 1), args
}

// queryEach runs a statement and scans every row.
func (s *PostgresStore) queryEach(ctx context.Context, what, sql string, args []any,
	scan func(pgx.Rows) error) error {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return storeErr(what, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return storeErr(what, err)
		}
	}
	return storeErr(what, rows.Err())
}

const shadowSetSQL = `/* pg_sage */ SELECT family,
	count(*) FILTER (WHERE reviewed_at >= $3),
	count(*) FILTER (WHERE reviewed_at >= $3 AND verdict = 'accepted'),
	min(reviewed_at)
	FROM sage.sre_packet_reviews
	WHERE deployment_id = $1 AND database_name = $2 AND counts_as_evidence /*scope*/
	GROUP BY family`

// shadowSet counts the scope's packet reviews by a person since since,
// per family, and the first such review ever. Reviews recorded through
// MCP are kept but are not evidence.
func (s *PostgresStore) shadowSet(ctx context.Context, p pairScope, since time.Time) (
	map[Family]Shadow, error) {
	sql, args := scoped(shadowSetSQL, p, false, s.deployment, s.database, since)
	out := map[Family]Shadow{}
	err := s.queryEach(ctx, "read shadow record", sql, args, func(rows pgx.Rows) error {
		var f string
		var sh Shadow
		var first *time.Time
		if err := rows.Scan(&f, &sh.Reviewed, &sh.Accepted, &first); err != nil {
			return err
		}
		if first != nil {
			sh.FirstReviewAt = first.UTC()
		}
		out[Family(f)] = sh
		return nil
	})
	return out, err
}

// ShadowStats is one family's shadow record on the database.
func (s *PostgresStore) ShadowStats(ctx context.Context, f Family, since time.Time) (Shadow,
	error) {
	set, err := s.shadowSet(ctx, pairScope{family: f}, since)
	return set[f], err
}

const liveSetSQL = `/* pg_sage */ SELECT family, action_class,
	count(*) FILTER (WHERE result = 'verified_recovery' AND level = 2),
	count(*) FILTER (WHERE result IN ('harmful', 'safety_violation')),
	count(*) FILTER (WHERE result = 'unverified')
	FROM sage.sre_autonomy_outcomes
	WHERE deployment_id = $1 AND database_name = $2 /*scope*/
	GROUP BY family, action_class`

// liveSet counts the scope's verified L2 recoveries, harmful and
// unverified outcomes per pair.
func (s *PostgresStore) liveSet(ctx context.Context, p pairScope) (map[pairKey]Live, error) {
	sql, args := scoped(liveSetSQL, p, true, s.deployment, s.database)
	out := map[pairKey]Live{}
	err := s.queryEach(ctx, "read live record", sql, args, func(rows pgx.Rows) error {
		var f, c string
		var l Live
		if err := rows.Scan(&f, &c, &l.VerifiedL2, &l.HarmfulPair, &l.Unverified); err != nil {
			return err
		}
		out[pairKey{Family(f), ActionClass(c)}] = l
		return nil
	})
	return out, err
}

// LiveStats is one pair's live record on the database.
func (s *PostgresStore) LiveStats(ctx context.Context, f Family, c ActionClass) (Live,
	error) {
	set, err := s.liveSet(ctx, pairScope{f, c})
	return set[pairKey{f, c}], err
}

// familySafetyRow is a family's harmful or unsafe outcomes in a window
// and the newest one's time.
type familySafetyRow struct {
	n    int
	last time.Time
}

const safetySetSQL = `/* pg_sage */ SELECT family, count(*), max(recorded_at)
	FROM sage.sre_autonomy_outcomes
	WHERE deployment_id = $1 AND database_name = $2 AND recorded_at >= $3
	  AND result IN ('harmful', 'safety_violation') /*scope*/
	GROUP BY family`

// safetySet counts the scope's harmful or unsafe outcomes since since,
// per family (families without any are absent).
func (s *PostgresStore) safetySet(ctx context.Context, p pairScope, since time.Time) (
	map[Family]familySafetyRow, error) {
	sql, args := scoped(safetySetSQL, p, false, s.deployment, s.database, since)
	out := map[Family]familySafetyRow{}
	err := s.queryEach(ctx, "read family safety record", sql, args,
		func(rows pgx.Rows) error {
			var f string
			var row familySafetyRow
			var last *time.Time
			if err := rows.Scan(&f, &row.n, &last); err != nil {
				return err
			}
			if last != nil {
				row.last = last.UTC()
			}
			out[Family(f)] = row
			return nil
		})
	return out, err
}

// FamilyViolations counts the database's harmful or unsafe outcomes of a
// family since since.
func (s *PostgresStore) FamilyViolations(ctx context.Context, f Family,
	since time.Time) (int, error) {
	set, err := s.safetySet(ctx, pairScope{family: f}, since)
	return set[f].n, err
}
