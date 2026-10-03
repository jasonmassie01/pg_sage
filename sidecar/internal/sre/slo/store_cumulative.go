package slo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Running counters (measured.md M12, static.md F10; v1.8.3). Every SLO
// window used to be re-aggregated from raw samples on every evaluation (a
// 30-day window: ~43,000 samples per series per minute). Each sample now
// carries its series' running reset-compensated totals since its chain
// started (chain_start): cum_bad and cum_eligible (the sums of the
// increases the raw aggregation computes, a decrease counting as a reset
// whose new value is the increase), cum_samples and cum_resets. A window
// is then the difference between its newest sample and its baseline
// (the newest sample before it, within the lookback, else its oldest):
// three primary-key probes per series. Writes to a series are serialized
// and a late (out-of-order) sample re-chains the samples after it, so the
// counters stay exact. Samples stored before the counters existed (NULL)
// are aggregated raw, per series, until they age out.

// insertSampleSQL stores a sample chained to the series' previous one.
const insertSampleSQL = `/* pg_sage sre:slo */
INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series, observed_at, bad,
    eligible, value, cum_bad, cum_eligible, cum_samples, cum_resets, chain_start)
SELECT $1, $2, $3, $4, $5, $6, $7,
       CASE WHEN p.cum_samples IS NULL THEN 0
            ELSE p.cum_bad + CASE WHEN $5 >= p.bad THEN $5 - p.bad ELSE $5 END END,
       CASE WHEN p.cum_samples IS NULL THEN 0
            ELSE p.cum_eligible + CASE WHEN $6 >= p.eligible THEN $6 - p.eligible
                                       ELSE $6 END END,
       COALESCE(p.cum_samples, 0) + 1,
       CASE WHEN p.cum_samples IS NULL THEN 0
            ELSE p.cum_resets + ($5 < p.bad OR $6 < p.eligible)::int END,
       CASE WHEN p.cum_samples IS NULL THEN $4 ELSE p.chain_start END
  FROM (SELECT 1) AS one
  LEFT JOIN LATERAL (
       SELECT x.bad, x.eligible, x.cum_bad, x.cum_eligible, x.cum_samples, x.cum_resets,
              x.chain_start
         FROM sage.sre_sli_samples x
        WHERE x.deployment_id = $1 AND x.slo_name = $2 AND x.series = $3
          AND x.observed_at < $4
        ORDER BY x.observed_at DESC LIMIT 1) p ON true
ON CONFLICT (deployment_id, slo_name, series, observed_at) DO NOTHING`

// A constant-only CASE (THEN 1 ELSE 0) is avoided in these statements:
// normalized by pg_stat_statements its constants are untyped and it
// resolves to text, which the performance gate could not explain.

// rechainSQL recomputes the counters of the samples after the one at $4
// (a late sample), continuing its chain.
const rechainSQL = `/* pg_sage sre:slo */
WITH b AS (
    SELECT cum_bad, cum_eligible, cum_samples, cum_resets, chain_start
      FROM sage.sre_sli_samples
     WHERE deployment_id = $1 AND slo_name = $2 AND series = $3 AND observed_at = $4
), s AS (
    SELECT x.observed_at, x.bad, x.eligible,
           lag(x.bad) OVER w AS pbad, lag(x.eligible) OVER w AS pel,
           row_number() OVER w AS rn
      FROM sage.sre_sli_samples x
     WHERE x.deployment_id = $1 AND x.slo_name = $2 AND x.series = $3
       AND x.observed_at >= $4
    WINDOW w AS (ORDER BY x.observed_at)
), c AS (
    SELECT s.observed_at, b.chain_start, b.cum_samples + s.rn - 1 AS cn,
           b.cum_bad + sum(CASE WHEN s.pbad IS NULL THEN 0 WHEN s.bad >= s.pbad
                                THEN s.bad - s.pbad ELSE s.bad END) OVER o AS cb,
           b.cum_eligible + sum(CASE WHEN s.pel IS NULL THEN 0 WHEN s.eligible >= s.pel
                                     THEN s.eligible - s.pel ELSE s.eligible END) OVER o AS ce,
           b.cum_resets + sum((s.pbad IS NOT NULL
                               AND (s.bad < s.pbad OR s.eligible < s.pel))::int) OVER o AS cr
      FROM s CROSS JOIN b
    WINDOW o AS (ORDER BY s.observed_at)
)
UPDATE sage.sre_sli_samples t
   SET cum_bad = c.cb, cum_eligible = c.ce, cum_samples = c.cn, cum_resets = c.cr,
       chain_start = c.chain_start
  FROM c
 WHERE t.deployment_id = $1 AND t.slo_name = $2 AND t.series = $3
   AND t.observed_at = c.observed_at AND c.observed_at > $4`

// recordSample stores one sample with its running counters: the series'
// writes are serialized (a transaction-scoped advisory lock), and a late
// sample re-chains the ones after it.
func (s *Store) recordSample(ctx context.Context, dep sre.UUID, slo string, p PushSample,
	value *float64) (bool, error) {
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"sre:sli:"+string(dep)+"\x1f"+slo+"\x1f"+p.Series); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, insertSampleSQL, string(dep), slo, p.Series, p.ObservedAt,
			p.Bad, p.Eligible, value)
		if err != nil || tag.RowsAffected() != 1 {
			return err
		}
		created = true
		_, err = tx.Exec(ctx, rechainSQL, string(dep), slo, p.Series, p.ObservedAt)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("record SLI sample of %s: %w", slo, err)
	}
	return created, nil
}

// windowPointsSQL reads, per series, the window's newest (e) and oldest
// (f) sample in [$4, $5] and the newest sample before $4 (b: the baseline
// when it lies within the lookback, checked by the caller). The time
// bounds are written as row comparisons with the series so that only the
// primary key can serve them: a bound on observed_at alone made the
// planner walk sre_sli_samples_time (every SLO's samples of the
// deployment) when a series had few samples. %s is the series source: one
// named series ($3) or every series of the SLO (a skip scan of the
// primary key).
const windowPointsSQL = `/* pg_sage sre:slo */
WITH RECURSIVE %s
SELECT ser.series,
       e.observed_at, e.cum_bad, e.cum_eligible, e.cum_samples, e.cum_resets, e.chain_start,
       f.observed_at, f.cum_bad, f.cum_eligible, f.cum_samples, f.cum_resets, f.chain_start,
       b.observed_at, b.cum_bad, b.cum_eligible, b.cum_samples, b.cum_resets, b.chain_start
  FROM ser
  LEFT JOIN LATERAL (` + windowPointSQL + `
        AND (x.series, x.observed_at) >= (ser.series, $4)
        AND (x.series, x.observed_at) <= (ser.series, $5)
      ORDER BY x.observed_at DESC LIMIT 1) e ON true
  LEFT JOIN LATERAL (` + windowPointSQL + `
        AND (x.series, x.observed_at) >= (ser.series, $4)
        AND (x.series, x.observed_at) <= (ser.series, $5)
      ORDER BY x.observed_at LIMIT 1) f ON true
  LEFT JOIN LATERAL (` + windowPointSQL + `
        AND (x.series, x.observed_at) < (ser.series, $4)
      ORDER BY x.observed_at DESC LIMIT 1) b ON true
 WHERE ser.series IS NOT NULL
 ORDER BY ser.series`

const windowPointSQL = `
      SELECT x.observed_at, x.cum_bad, x.cum_eligible, x.cum_samples, x.cum_resets,
             x.chain_start
        FROM sage.sre_sli_samples x
       WHERE x.deployment_id = $1 AND x.slo_name = $2 AND x.series = ser.series`

var (
	// oneSeriesPointsSQL reads the window of series $3.
	oneSeriesPointsSQL = fmt.Sprintf(windowPointsSQL,
		`ser(series) AS (SELECT $3::text WHERE $3::text <> '')`)
	// allSeriesPointsSQL reads the window of every series ($3 unused).
	allSeriesPointsSQL = fmt.Sprintf(windowPointsSQL, `ser(series) AS (
    (SELECT x.series FROM sage.sre_sli_samples x
      WHERE x.deployment_id = $1 AND x.slo_name = $2 AND $3::text = ''
      ORDER BY x.series LIMIT 1)
    UNION ALL
    SELECT (SELECT x.series FROM sage.sre_sli_samples x
             WHERE x.deployment_id = $1 AND x.slo_name = $2 AND x.series > ser.series
             ORDER BY x.series LIMIT 1)
      FROM ser WHERE ser.series IS NOT NULL
)`)
)

// rawSeriesSQL is the raw aggregation of one series: the reference the
// running counters reproduce, used for samples stored without them.
const rawSeriesSQL = `/* pg_sage sre:slo */
WITH s AS (
    SELECT series, observed_at, bad, eligible,
           observed_at >= $4 AS inside,
           row_number() OVER (PARTITION BY series, observed_at >= $4
                              ORDER BY observed_at DESC) AS rn
    FROM sage.sre_sli_samples
    WHERE deployment_id = $1 AND slo_name = $2 AND series = $3
      AND observed_at > $4::timestamptz - make_interval(secs => $6)
      AND observed_at <= $5
), kept AS (
    SELECT series, observed_at, bad, eligible,
           lag(bad) OVER w AS pbad, lag(eligible) OVER w AS pel
    FROM s WHERE inside OR rn = 1
    WINDOW w AS (PARTITION BY series ORDER BY observed_at)
)
SELECT series, count(*)::int, min(observed_at), max(observed_at),
       COALESCE(sum(CASE WHEN pbad IS NULL THEN 0 WHEN bad >= pbad THEN bad - pbad
                         ELSE bad END), 0),
       COALESCE(sum(CASE WHEN pel IS NULL THEN 0 WHEN eligible >= pel THEN eligible - pel
                         ELSE eligible END), 0),
       count(*) FILTER (WHERE pbad IS NOT NULL AND (bad < pbad OR eligible < pel))::int
FROM kept GROUP BY series`
