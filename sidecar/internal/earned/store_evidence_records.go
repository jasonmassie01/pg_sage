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
	   AND ((f.family = '' AND cells @? '$[*] ? (@.family != "all")')
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
// report that scored any family; a replay-only model-lift report, which
// carries no family cell, never stands in for the release bench). A
// report stamped for another build never counts; an unstamped (operator)
// report does. Bench evidence is about pg_sage, not
// a database, so every database of the deployment shares it.
func (s *PostgresStore) benchSet(ctx context.Context, families []Family) (
	map[Family]*EvalRun, error) {
	out := map[Family]*EvalRun{}
	return out, s.readBench(ctx, s, families, out)
}

func (s *PostgresStore) readBench(ctx context.Context, r reads, families []Family,
	out map[Family]*EvalRun) error {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, string(f))
	}
	b := s.runningBuild()
	return r.each(ctx, "read latest bench report", benchSetSQL,
		[]any{s.deployment, b.Commit, b.Version, names}, func(rows pgx.Rows) error {
			var f string
			run, err := s.scanEvalRun(trailingRow{Row: rows, extra: &f})
			if err != nil {
				return err
			}
			out[Family(f)] = &run
			return nil
		})
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
// recorded before outcomes kept their verdict count by result. Shadow
// evidence (roadmap 1.4) is counted apart: by score, and since the last
// demerit by distinct decision (fingerprint); an incorrect shadow
// decision is a demerit for this count (it never demotes).
const classRecordSetSQL = `/* pg_sage */ SELECT p.family, p.action_class,
	c.improved, c.neutral, c.regressed, c.rolled_back, c.rejected, c.insufficient,
	c.unverifiable, c.successes, c.uncredited, s.correct, s.incorrect, s.neutral,
	s.successes, s.uncredited, l.at, l.cause
	FROM unnest($3::text[], $4::text[]) AS p(family, action_class)
	LEFT JOIN LATERAL (
		SELECT d.at, d.cause FROM (
			SELECT COALESCE(o.observed_at, o.recorded_at) AS at,
			       COALESCE(o.verdict, o.result) AS cause
			  FROM sage.sre_autonomy_outcomes o
			 WHERE o.deployment_id = $1 AND o.database_name = $2
			   AND o.family = p.family AND o.action_class = p.action_class
			   AND o.result IN ('harmful', 'safety_violation', 'rejected')
			UNION ALL
			SELECT e.observed_at, 'shadow_incorrect' FROM sage.trust_shadow_evidence e
			 WHERE e.deployment_id = $1 AND e.database_name = $2
			   AND e.family = p.family AND e.action_class = p.action_class
			   AND e.score = 'incorrect') d
		 ORDER BY 1 DESC LIMIT 1) l ON true
	CROSS JOIN LATERAL (
		SELECT
		count(*) FILTER (WHERE o.verdict = 'improved'
		    OR (o.verdict IS NULL AND o.result = 'verified_recovery')) AS improved,
		count(*) FILTER (WHERE o.verdict = 'neutral') AS neutral,
		count(*) FILTER (WHERE o.verdict = 'regressed' OR (o.verdict IS NULL
		    AND o.result IN ('harmful', 'safety_violation'))) AS regressed,
		count(*) FILTER (WHERE o.verdict = 'rolled_back') AS rolled_back,
		count(*) FILTER (WHERE o.verdict = 'rejected'
		    OR (o.verdict IS NULL AND o.result = 'rejected')) AS rejected,
		count(*) FILTER (WHERE o.verdict = 'insufficient_evidence') AS insufficient,
		count(*) FILTER (WHERE o.verdict = 'unverifiable'
		    OR (o.verdict IS NULL AND o.result = 'unverified')) AS unverifiable,
		count(*) FILTER (WHERE o.result = 'verified_recovery' AND (l.at IS NULL
		    OR COALESCE(o.observed_at, o.recorded_at) > l.at)) AS successes,
		count(*) FILTER (WHERE o.result = 'unverified' AND o.verdict = 'neutral'
		    AND (l.at IS NULL OR COALESCE(o.observed_at, o.recorded_at) > l.at))
		    AS uncredited
		  FROM sage.sre_autonomy_outcomes o
		 WHERE o.deployment_id = $1 AND o.database_name = $2 AND o.family = p.family
		   AND o.action_class = p.action_class) c
	CROSS JOIN LATERAL (
		SELECT count(*) FILTER (WHERE e.score = 'correct') AS correct,
		count(*) FILTER (WHERE e.score = 'incorrect') AS incorrect,
		count(*) FILTER (WHERE e.score = 'neutral') AS neutral,
		count(DISTINCT e.fingerprint) FILTER (WHERE e.score = 'correct'
		    AND (l.at IS NULL OR e.observed_at > l.at)) AS successes,
		count(DISTINCT e.fingerprint) FILTER (WHERE e.score = 'neutral'
		    AND (l.at IS NULL OR e.observed_at > l.at)) AS uncredited
		  FROM sage.trust_shadow_evidence e
		 WHERE e.deployment_id = $1 AND e.database_name = $2 AND e.family = p.family
		   AND e.action_class = p.action_class) s`

// classRecordSet reads the verdict record of each pair, real and shadow
// evidence apart (Shadow*) and together (Successes, Uncredited).
func (s *PostgresStore) classRecordSet(ctx context.Context, pairs []pairKey) (
	map[pairKey]ClassRecord, error) {
	out := map[pairKey]ClassRecord{}
	return out, s.readClassRecords(ctx, s, pairs, out)
}

func (s *PostgresStore) readClassRecords(ctx context.Context, rd reads, pairs []pairKey,
	out map[pairKey]ClassRecord) error {
	families, classes := pairArrays(pairs)
	return rd.each(ctx, "read class record", classRecordSetSQL,
		[]any{s.deployment, s.database, families, classes}, func(rows pgx.Rows) error {
			var f, c string
			var r ClassRecord
			var cause *string
			if err := rows.Scan(&f, &c, &r.Improved, &r.Neutral, &r.Regressed,
				&r.RolledBack, &r.Rejected, &r.Insufficient, &r.Unverifiable, &r.Successes,
				&r.Uncredited, &r.ShadowCorrect, &r.ShadowIncorrect, &r.ShadowNeutral,
				&r.ShadowSuccesses, &r.ShadowUncredited, &r.LastDemeritAt, &cause); err != nil {
				return err
			}
			r.Successes += r.ShadowSuccesses
			r.Uncredited += r.ShadowUncredited
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
}

// ClassRecord reads a pair's verdict record on the database.
func (s *PostgresStore) ClassRecord(ctx context.Context, f Family, c ActionClass) (
	ClassRecord, error) {
	set, err := s.classRecordSet(ctx, []pairKey{{f, c}})
	if err != nil {
		return ClassRecord{}, err
	}
	return set[pairKey{f, c}], nil
}

// levelMap keys stored states by pair.
func levelMap(levels []State) map[pairKey]State {
	out := make(map[pairKey]State, len(levels))
	for _, st := range levels {
		out[pairKey{st.Family, st.Class}] = st
	}
	return out
}
