package earned

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Set-based evidence reads (roadmap 1.2). Each read is one statement for
// a list of pairs (or families): one pair for a promotion or an
// authorization, every pair of the grid for the Trust view (read by the
// Trust page and every approval card). The statement unnests the list and
// reads each pair through its index in a LATERAL subquery, so a database
// with a long history is never scanned whole, and a pair's read is the
// grid's read for that pair: the two cannot disagree.

// pairArrays splits pairs into the family and class arrays a statement
// unnests together.
func pairArrays(pairs []pairKey) ([]string, []string) {
	families := make([]string, 0, len(pairs))
	classes := make([]string, 0, len(pairs))
	for _, p := range pairs {
		families = append(families, string(p.family))
		classes = append(classes, string(p.class))
	}
	return families, classes
}

// familiesOf lists the distinct families of pairs, in order.
func familiesOf(pairs []pairKey) []string {
	seen := map[Family]bool{}
	var out []string
	for _, p := range pairs {
		if !seen[p.family] {
			seen[p.family] = true
			out = append(out, string(p.family))
		}
	}
	return out
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

const shadowSetSQL = `/* pg_sage */ SELECT f.family, s.reviewed, s.accepted, s.first
	FROM unnest($3::text[]) AS f(family)
	CROSS JOIN LATERAL (
		SELECT count(*) FILTER (WHERE r.reviewed_at >= $4) AS reviewed,
		       count(*) FILTER (WHERE r.reviewed_at >= $4 AND r.verdict = 'accepted')
		           AS accepted,
		       min(r.reviewed_at) AS first
		  FROM sage.sre_packet_reviews r
		 WHERE r.deployment_id = $1 AND r.database_name = $2 AND r.family = f.family
		   AND r.counts_as_evidence) s`

// shadowSet counts each family's packet reviews by a person since since,
// and the first such review ever. Reviews recorded through MCP are kept
// but are not evidence.
func (s *PostgresStore) shadowSet(ctx context.Context, families []string,
	since time.Time) (map[Family]Shadow, error) {
	out := map[Family]Shadow{}
	err := s.queryEach(ctx, "read shadow record", shadowSetSQL,
		[]any{s.deployment, s.database, families, since}, func(rows pgx.Rows) error {
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
	set, err := s.shadowSet(ctx, []string{string(f)}, since)
	return set[f], err
}

const liveSetSQL = `/* pg_sage */ SELECT p.family, p.action_class, l.verified, l.harmful,
	l.unverified
	FROM unnest($3::text[], $4::text[]) AS p(family, action_class)
	CROSS JOIN LATERAL (
		SELECT count(*) FILTER (WHERE o.result = 'verified_recovery' AND o.level = 2)
		           AS verified,
		       count(*) FILTER (WHERE o.result IN ('harmful', 'safety_violation'))
		           AS harmful,
		       count(*) FILTER (WHERE o.result = 'unverified') AS unverified
		  FROM sage.sre_autonomy_outcomes o
		 WHERE o.deployment_id = $1 AND o.database_name = $2 AND o.family = p.family
		   AND o.action_class = p.action_class) l`

// liveSet counts each pair's verified L2 recoveries, harmful and
// unverified outcomes.
func (s *PostgresStore) liveSet(ctx context.Context, pairs []pairKey) (map[pairKey]Live,
	error) {
	families, classes := pairArrays(pairs)
	out := map[pairKey]Live{}
	err := s.queryEach(ctx, "read live record", liveSetSQL,
		[]any{s.deployment, s.database, families, classes}, func(rows pgx.Rows) error {
			var f, c string
			var l Live
			if err := rows.Scan(&f, &c, &l.VerifiedL2, &l.HarmfulPair,
				&l.Unverified); err != nil {
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
	set, err := s.liveSet(ctx, []pairKey{{f, c}})
	return set[pairKey{f, c}], err
}

// familySafetyRow is a family's harmful or unsafe outcomes in a window
// and the newest one's time.
type familySafetyRow struct {
	n    int
	last time.Time
}

const safetySetSQL = `/* pg_sage */ SELECT f.family, s.n, s.last
	FROM unnest($4::text[]) AS f(family)
	CROSS JOIN LATERAL (
		SELECT count(*) AS n, max(o.recorded_at) AS last
		  FROM sage.sre_autonomy_outcomes o
		 WHERE o.deployment_id = $1 AND o.database_name = $2 AND o.family = f.family
		   AND o.recorded_at >= $3 AND o.result IN ('harmful', 'safety_violation')) s`

// safetySet counts each family's harmful or unsafe outcomes since since.
func (s *PostgresStore) safetySet(ctx context.Context, families []string,
	since time.Time) (map[Family]familySafetyRow, error) {
	out := map[Family]familySafetyRow{}
	err := s.queryEach(ctx, "read family safety record", safetySetSQL,
		[]any{s.deployment, s.database, since, families}, func(rows pgx.Rows) error {
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
	set, err := s.safetySet(ctx, []string{string(f)}, since)
	return set[f].n, err
}
