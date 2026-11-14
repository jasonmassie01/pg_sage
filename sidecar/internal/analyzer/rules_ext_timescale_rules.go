package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// tsCaggSQL lists, at most $1, the continuous aggregates whose source
// hypertable is time-based (timestamptz, timestamp or date) and that have
// a scheduled refresh policy, with the policy whose schedule_interval plus
// end_offset is largest, and each one's watermark from fnSchema. Integer
// time aggregates measure lag in their own units and are not judged.
func tsCaggSQL(schema, fnSchema string) string {
	s := sqlident.Ident(schema)
	return `/* pg_sage analyzer */
WITH pol AS (
  SELECT c.view_schema::text AS view_schema, c.view_name::text AS view_name,
         COALESCE(c.hypertable_schema::text, '') AS ht_schema,
         COALESCE(c.hypertable_name::text, '') AS ht_name,
         COALESCE(c.materialized_only, false) AS materialized_only,
         j.job_id::bigint AS job_id, (j.config->>'mat_hypertable_id')::int AS mat_id,
         j.schedule_interval,
         CASE WHEN jsonb_typeof(j.config->'end_offset') = 'string'
              THEN GREATEST((j.config->>'end_offset')::interval, interval '0')
              ELSE interval '0' END AS end_offset
    FROM ` + s + `.continuous_aggregates c
    JOIN ` + s + `.jobs j
      ON j.hypertable_schema = c.materialization_hypertable_schema
     AND j.hypertable_name = c.materialization_hypertable_name
     AND j.proc_name = '` + tsRefreshProc + `' AND j.scheduled
   WHERE EXISTS (SELECT 1 FROM ` + s + `.dimensions d
                  WHERE d.hypertable_schema = c.hypertable_schema
                    AND d.hypertable_name = c.hypertable_name
                    AND d.dimension_number = 1
                    AND d.column_type IN ('timestamptz'::regtype,
                                          'timestamp'::regtype, 'date'::regtype))
), one AS (
  SELECT DISTINCT ON (view_schema, view_name) *
    FROM pol
   ORDER BY view_schema, view_name, schedule_interval + end_offset DESC, job_id
)
SELECT view_schema, view_name, ht_schema, ht_name, materialized_only, job_id,
       schedule_interval::text, end_offset::text,
       (EXTRACT(EPOCH FROM schedule_interval + end_offset) * 1000000)::bigint,
       ` + sqlident.Qualified(fnSchema, "cagg_watermark") + `(mat_id)
  FROM one
 ORDER BY view_schema, view_name
 LIMIT $1`
}

// readTSCaggRows reads at most tsMaxCaggs aggregates; truncated says more
// exist. Aggregates that never materialized anything are left out.
func readTSCaggRows(ctx context.Context, q catalogread.Querier, schema, fnSchema string) (
	caggs []tsCagg, truncated bool, err error,
) {
	rows, err := q.Query(ctx, tsCaggSQL(schema, fnSchema), tsMaxCaggs+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n++; n > tsMaxCaggs {
			truncated = true
			break
		}
		var c tsCagg
		var allowedMicros, watermark *int64
		if err := rows.Scan(&c.ViewSchema, &c.ViewName, &c.HypertableSchema,
			&c.HypertableName, &c.MaterializedOnly, &c.JobID, &c.ScheduleInterval,
			&c.EndOffset, &allowedMicros, &watermark); err != nil {
			return nil, false, fmt.Errorf("scan: %w", err)
		}
		if allowedMicros == nil || watermark == nil {
			continue
		}
		at, ok := tsWatermarkTime(*watermark)
		if !ok {
			continue
		}
		c.AllowedLag, c.Watermark = time.Duration(*allowedMicros)*time.Microsecond, at
		caggs = append(caggs, c)
	}
	return caggs, truncated, rows.Err()
}

// tsWatermarkTime converts a watermark in Unix microseconds; ok is false
// for a continuous aggregate that never materialized anything.
func tsWatermarkTime(us int64) (time.Time, bool) {
	if us < tsMinWatermarkMicros {
		return time.Time{}, false
	}
	return time.UnixMicro(us).UTC(), true
}

// ruleTimescaleJobFailed reports each scheduled TimescaleDB job whose
// last run failed.
func ruleTimescaleJobFailed(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.tsReading()
	if r == nil || !r.jobsRead {
		return nil
	}
	out := make([]Finding, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		out = append(out, tsJobFinding(r.Schema, j))
	}
	return out
}

func tsJobFinding(schema string, j tsJobFailure) Finding {
	detail := map[string]any{
		"job_id":         j.JobID,
		"proc_schema":    j.ProcSchema,
		"proc_name":      j.ProcName,
		"total_failures": j.TotalFailures,
		"schema":         schema,
	}
	label := sqlident.Qualified(j.ProcSchema, j.ProcName)
	if j.HypertableSchema != nil && j.HypertableName != nil {
		detail["hypertable_schema"] = *j.HypertableSchema
		detail["hypertable_name"] = *j.HypertableName
		label = j.ProcName + " on " + sqlident.Qualified(*j.HypertableSchema,
			*j.HypertableName)
	}
	if j.NextStart != nil {
		detail["next_start"] = j.NextStart.UTC().Format(time.RFC3339)
	}
	if j.LastRunStartedAt != nil {
		detail["last_run_started_at"] = j.LastRunStartedAt.UTC().Format(time.RFC3339)
	}
	return Finding{
		Category:         categoryTSJobFailed,
		Severity:         "warning",
		ObjectType:       "timescaledb_job",
		ObjectIdentifier: fmt.Sprintf("timescaledb:job:%d", j.JobID),
		Title: fmt.Sprintf("TimescaleDB job %d (%s) failed its last run (%d failures "+
			"in total)", j.JobID, label, j.TotalFailures),
		Detail: detail,
		Recommendation: fmt.Sprintf("Run the job by hand with CALL run_job(%d) to see "+
			"its error (TimescaleDB 2.15 and later also keep it in "+
			"timescaledb_information.job_errors), fix the cause, and check the %s "+
			"policy's settings in %s (alter_job changes them). Advisory only: pg_sage "+
			"runs nothing.", j.JobID, j.ProcName, sqlident.Qualified(schema, "jobs")),
	}
}

// ruleTimescaleCaggStale reports each continuous aggregate whose
// watermark lags the clock by more than tsCaggLagFactor times its refresh
// policy's schedule_interval plus end_offset, while the policy has not
// succeeded within that time: a policy that keeps succeeding has
// materialized all the data there is.
func ruleTimescaleCaggStale(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.tsReading()
	if r == nil || !r.caggsRead {
		return nil
	}
	now := extras.now()
	var out []Finding
	for _, c := range r.Caggs {
		limit := tsCaggLagFactor * c.AllowedLag
		lag := now.Sub(c.Watermark)
		if c.AllowedLag <= 0 || lag <= limit {
			continue
		}
		if c.LastSuccess != nil && now.Sub(*c.LastSuccess) <= limit {
			continue
		}
		out = append(out, tsCaggFinding(r, c, lag, limit))
	}
	return out
}

func tsCaggFinding(r *tsReading, c tsCagg, lag, limit time.Duration) Finding {
	view := sqlident.Qualified(c.ViewSchema, c.ViewName)
	f := Finding{
		Category:         categoryTSCaggStale,
		Severity:         "warning",
		ObjectType:       "continuous_aggregate",
		ObjectIdentifier: view,
		Title: fmt.Sprintf("Continuous aggregate %s is %s behind (stale past %s)",
			view, lag.Round(time.Second), limit),
		Detail: map[string]any{
			"view_schema":        c.ViewSchema,
			"view_name":          c.ViewName,
			"hypertable_schema":  c.HypertableSchema,
			"hypertable_name":    c.HypertableName,
			"materialized_only":  c.MaterializedOnly,
			"job_id":             c.JobID,
			"schedule_interval":  c.ScheduleInterval,
			"end_offset":         c.EndOffset,
			"watermark":          c.Watermark.UTC().Format(time.RFC3339),
			"lag_seconds":        int64(lag / time.Second),
			"threshold_seconds":  int64(limit / time.Second),
			"watermark_function": sqlident.Qualified(r.WatermarkSchema, "cagg_watermark"),
			"schema":             r.Schema,
		},
		Recommendation: fmt.Sprintf("Its refresh policy (job %d, %s, every %s with "+
			"end_offset %s) has not moved the watermark or succeeded within the "+
			"threshold. Check the job in %s and run it by hand with CALL run_job(%d); "+
			"if it never starts, check that TimescaleDB's job scheduler runs "+
			"(timescaledb.max_background_workers, timescaledb_post_restore() after a "+
			"restore). Until then "+
			"queries on the aggregate miss recent data or compute it at query time. "+
			"Advisory only: pg_sage runs nothing.", c.JobID, tsRefreshProc,
			c.ScheduleInterval, c.EndOffset, sqlident.Qualified(r.Schema, "job_stats"),
			c.JobID),
	}
	if c.LastSuccess != nil {
		f.Detail["last_successful_finish"] = c.LastSuccess.UTC().Format(time.RFC3339)
	}
	return f
}

func (e *RuleExtras) tsReading() *tsReading {
	if e == nil || e.ext == nil {
		return nil
	}
	return e.ext.timescale
}
