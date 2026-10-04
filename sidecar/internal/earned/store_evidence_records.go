package earned

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Set-based reads of bench reports and class records (see
// store_evidence_set.go).

const benchSetSQL = `/* pg_sage */ SELECT r.*, f.family FROM unnest($4::text[]) AS f(family)
CROSS JOIN LATERAL (` + evalRunSelect + `
	 WHERE deployment_id = $1 AND source = 'bench'
	   AND (f.family = ''
	        OR cells @> jsonb_build_array(jsonb_build_object('family', f.family)))
	   AND ((pg_sage_version = '' AND pg_sage_commit = '')
	     OR ($2 <> '' AND pg_sage_commit = $2)
	     OR (($2 = '' OR pg_sage_commit = '') AND $3 <> '' AND pg_sage_version = $3))
	 ORDER BY generated_at DESC, ingested_at DESC LIMIT 1) r`

// trailingRow scans a row whose last column follows the ones its reader
// scans.
type trailingRow struct {
	pgx.Row
	extra any
}

func (r trailingRow) Scan(dest ...any) error { return r.Row.Scan(append(dest, r.extra)...) }

// benchSet is, per family, the deployment's newest bench report that
// scored it and counts for the running build (family "": the newest
// report at all). A report stamped for another build never counts; an
// unstamped (operator) report does. Bench evidence is about pg_sage, not
// a database, so every database of the deployment shares it.
func (s *PostgresStore) benchSet(ctx context.Context, families []Family) (
	map[Family]*EvalRun, error) {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, string(f))
	}
	b := s.runningBuild()
	out := map[Family]*EvalRun{}
	err := s.queryEach(ctx, "read latest bench report", benchSetSQL,
		[]any{s.deployment, b.Commit, b.Version, names}, func(rows pgx.Rows) error {
			var f string
			run, err := s.scanEvalRun(trailingRow{Row: rows, extra: &f})
			if err != nil {
				return err
			}
			out[Family(f)] = &run
			return nil
		})
	return out, err
}

// LatestBench is the deployment's newest bench report that counts for
// the running build, or nil; with a family, the newest such report that
// scored that family.
func (s *PostgresStore) LatestBench(ctx context.Context, f Family) (*EvalRun, error) {
	set, err := s.benchSet(ctx, []Family{f})
	return set[f], err
}

// classRecordSetSQL counts each pair's outcomes on the database, and its
// credited and uncredited decided outcomes since its last demerit. Rows
// recorded before outcomes kept their verdict count by result.
const classRecordSetSQL = `/* pg_sage */ WITH pair AS (
	SELECT family, action_class, verdict, result,
	       COALESCE(observed_at, recorded_at) AS at
	  FROM sage.sre_autonomy_outcomes
	 WHERE deployment_id = $1 AND database_name = $2 /*scope*/
), last AS (
	SELECT DISTINCT ON (family, action_class) family, action_class, at,
	       COALESCE(verdict, result) AS cause
	  FROM pair WHERE result IN ('harmful', 'safety_violation', 'rejected')
	 ORDER BY family, action_class, at DESC
)
SELECT pair.family, pair.action_class,
	count(*) FILTER (WHERE verdict = 'improved'
	                    OR (verdict IS NULL AND result = 'verified_recovery')),
	count(*) FILTER (WHERE verdict = 'neutral'),
	count(*) FILTER (WHERE verdict = 'regressed'
	                    OR (verdict IS NULL AND result IN ('harmful', 'safety_violation'))),
	count(*) FILTER (WHERE verdict = 'rolled_back'),
	count(*) FILTER (WHERE verdict = 'rejected'
	                    OR (verdict IS NULL AND result = 'rejected')),
	count(*) FILTER (WHERE verdict = 'insufficient_evidence'),
	count(*) FILTER (WHERE verdict = 'unverifiable'
	                    OR (verdict IS NULL AND result = 'unverified')),
	count(*) FILTER (WHERE result = 'verified_recovery'
	                   AND (l.at IS NULL OR pair.at > l.at)),
	count(*) FILTER (WHERE result = 'unverified' AND verdict = 'neutral'
	                   AND (l.at IS NULL OR pair.at > l.at)),
	max(l.at), max(l.cause)
FROM pair LEFT JOIN last l
  ON l.family = pair.family AND l.action_class = pair.action_class
GROUP BY pair.family, pair.action_class`

// classRecordSet reads the verdict record of every pair of the scope
// (pairs without outcomes are absent: a zero record).
func (s *PostgresStore) classRecordSet(ctx context.Context, p pairScope) (
	map[pairKey]ClassRecord, error) {
	sql, args := scoped(classRecordSetSQL, p, true, s.deployment, s.database)
	out := map[pairKey]ClassRecord{}
	err := s.queryEach(ctx, "read class record", sql, args, func(rows pgx.Rows) error {
		var f, c string
		var r ClassRecord
		var cause *string
		if err := rows.Scan(&f, &c, &r.Improved, &r.Neutral, &r.Regressed, &r.RolledBack,
			&r.Rejected, &r.Insufficient, &r.Unverifiable, &r.Successes, &r.Uncredited,
			&r.LastDemeritAt, &cause); err != nil {
			return err
		}
		if r.LastDemeritAt != nil {
			at := r.LastDemeritAt.UTC()
			r.LastDemeritAt = &at
		}
		if cause != nil {
			r.LastDemerit = demeritName(*cause)
		}
		out[pairKey{Family(f), ActionClass(c)}] = r
		return nil
	})
	return out, err
}

// ClassRecord reads a pair's verdict record on the database.
func (s *PostgresStore) ClassRecord(ctx context.Context, f Family, c ActionClass) (
	ClassRecord, error) {
	set, err := s.classRecordSet(ctx, pairScope{f, c})
	if err != nil {
		return ClassRecord{}, err
	}
	return set[pairKey{f, c}], nil
}

// levelSet is every stored pair of the database.
func (s *PostgresStore) levelSet(ctx context.Context) (map[pairKey]State, error) {
	levels, err := s.Levels(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[pairKey]State, len(levels))
	for _, st := range levels {
		out[pairKey{st.Family, st.Class}] = st
	}
	return out, nil
}
