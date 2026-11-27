package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// TimescaleDB health rules (market-16b): compression, retention and
// continuous-aggregate refresh run as background jobs, and a failing one
// fails quietly. They read TimescaleDB 2.x's timescaledb_information views
// in the schema market-16's gate found, behind that gate.
const (
	extTimescale = "timescaledb"

	categoryTSJobFailed = "timescale_job_failed"
	categoryTSCaggStale = "timescale_cagg_stale"

	// tsMaxJobs bounds the failing jobs reported per cycle.
	tsMaxJobs = 50
	// tsMaxCaggs bounds the continuous aggregates judged per cycle.
	tsMaxCaggs = 100
	// tsCaggLagFactor times (schedule_interval + end_offset) is the
	// watermark lag past which a continuous aggregate is stale.
	tsCaggLagFactor = 3
	// tsMinWatermarkMicros is 0001-01-01 UTC in Unix microseconds. A
	// continuous aggregate that never materialized anything reports its
	// time type's minimum, which is earlier.
	tsMinWatermarkMicros = -62135596800000000
	// tsRefreshProc is the refresh policy's job procedure.
	tsRefreshProc = "policy_refresh_continuous_aggregate"
	// tsTelemetryProc is TimescaleDB's telemetry job procedure.
	tsTelemetryProc = "policy_telemetry"
)

// tsCategories are the TimescaleDB rules' categories; each rule has the
// name of its one category.
var tsCategories = []string{categoryTSJobFailed, categoryTSCaggStale}

// tsWatermarkFnSchemas are where cagg_watermark lives: TimescaleDB 2.12
// moved it from _timescaledb_internal to _timescaledb_functions.
var tsWatermarkFnSchemas = []string{"_timescaledb_functions", "_timescaledb_internal"}

// tsReading is one cycle's TimescaleDB reading. jobsRead and caggsRead
// say which parts were read; an unread part emits nothing.
type tsReading struct {
	Schema          string
	Jobs            []tsJobFailure
	jobsRead        bool
	WatermarkSchema string
	Caggs           []tsCagg
	caggsRead       bool
}

// tsJobFailure is one scheduled job whose last run failed.
type tsJobFailure struct {
	JobID            int64
	ProcSchema       string
	ProcName         string
	HypertableSchema *string
	HypertableName   *string
	TotalFailures    int64
	NextStart        *time.Time
	LastRunStartedAt *time.Time
}

// tsCagg is one time-based continuous aggregate with a scheduled refresh
// policy and a materialized watermark.
type tsCagg struct {
	ViewSchema, ViewName             string
	HypertableSchema, HypertableName string
	MaterializedOnly                 bool
	JobID                            int64
	ScheduleInterval, EndOffset      string
	// AllowedLag is the policy's schedule_interval plus end_offset.
	AllowedLag time.Duration
	Watermark  time.Time
	// LastSuccess is the policy's last successful finish; nil when it never
	// succeeded or job_stats could not be read.
	LastSuccess *time.Time
}

// tsProbe is what the catalog says about TimescaleDB's views.
type tsProbe struct {
	hasJobs      bool
	hasCaggs     bool
	watermarkFns *string
}

// tsProbeSQL reads, from the catalog only, whether the 2.x job views
// ($1 jobs, $2 job_stats) and the aggregate views ($3, $4) exist, and
// which schema holds cagg_watermark, the 2.12+ one first ($5).
const tsProbeSQL = `/* pg_sage analyzer */
SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL,
       to_regclass($3) IS NOT NULL AND to_regclass($4) IS NOT NULL,
       (SELECT n.nspname::text FROM pg_proc p
          JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE p.proname = 'cagg_watermark' AND p.pronargs = 1
           AND n.nspname = ANY($5)
         ORDER BY array_position($5, n.nspname::text) LIMIT 1)`

func readTSProbe(ctx context.Context, q catalogread.Querier, schema string) (
	tsProbe, error,
) {
	var p tsProbe
	err := q.QueryRow(ctx, tsProbeSQL, sqlident.Qualified(schema, "jobs"),
		sqlident.Qualified(schema, "job_stats"),
		sqlident.Qualified(schema, "continuous_aggregates"),
		sqlident.Qualified(schema, "dimensions"), tsWatermarkFnSchemas).
		Scan(&p.hasJobs, &p.hasCaggs, &p.watermarkFns)
	return p, err
}

// readTimescale reads TimescaleDB's state in schema; it marks what it
// could not read as not evaluated.
func (a *Analyzer) readTimescale(ctx context.Context, q catalogread.Querier,
	schema string) *tsReading {
	r := &tsReading{Schema: schema}
	p, err := readTSProbe(ctx, q, schema)
	if err != nil {
		a.extWarn("ts_probe", "analyzer: timescaledb: read the catalog entries of %s: %v; "+
			"TimescaleDB checks not evaluated", sqlident.Ident(schema), err)
		a.evalFail(tsCategories...)
		return r
	}
	a.extOK("ts_probe")
	if !p.hasJobs {
		a.extWarn("ts_views", "analyzer: timescaledb: %s.jobs or job_stats does not exist "+
			"(TimescaleDB 2.0 or later has them); TimescaleDB checks not evaluated",
			sqlident.Ident(schema))
		a.evalFail(tsCategories...)
		return r
	}
	a.extOK("ts_views")
	a.readTSJobs(ctx, q, r)
	a.readTSCaggs(ctx, q, r, p)
	a.readTSRefreshSuccess(ctx, q, r)
	return r
}

// tsUnread marks category not evaluated and logs what failed once per
// streak under key.
func (a *Analyzer) tsUnread(key, category, format string, args ...any) {
	a.evalFail(category)
	a.extWarn(key, "analyzer: timescaledb: "+format+"; "+category+" not evaluated", args...)
}

func (a *Analyzer) readTSJobs(ctx context.Context, q catalogread.Querier, r *tsReading) {
	jobs, err := readTSJobRows(ctx, q, r.Schema)
	if err != nil {
		tbl := sqlident.Qualified(r.Schema, "job_stats")
		a.tsUnread("ts_jobs", categoryTSJobFailed, "read %s: %v (the monitoring role "+
			"needs SELECT on it)", tbl, err)
		return
	}
	a.extOK("ts_jobs")
	if len(jobs) > tsMaxJobs {
		// The jobs past the limit may hold open findings.
		jobs = jobs[:tsMaxJobs]
		a.evalFail(categoryTSJobFailed)
	}
	r.Jobs, r.jobsRead = jobs, true
}

// tsJobsSQL lists the scheduled jobs whose last run failed, newest run
// first, at most $1. A job that never ran has a NULL status. TimescaleDB's
// own telemetry job is left out: it fails wherever the server cannot reach
// the internet, and no operator policy depends on it.
func tsJobsSQL(schema string) string {
	s := sqlident.Ident(schema)
	return `/* pg_sage analyzer */
SELECT s.job_id::bigint, j.proc_schema::text, j.proc_name::text,
       j.hypertable_schema::text, j.hypertable_name::text,
       COALESCE(s.total_failures, 0)::bigint,
       CASE WHEN isfinite(s.next_start) THEN s.next_start END,
       CASE WHEN isfinite(s.last_run_started_at) THEN s.last_run_started_at END
  FROM ` + s + `.job_stats s
  JOIN ` + s + `.jobs j ON j.job_id = s.job_id
 WHERE s.last_run_status = 'Failed' AND j.scheduled
   AND NOT (j.proc_name = '` + tsTelemetryProc + `'
            AND j.proc_schema IN ('_timescaledb_functions', '_timescaledb_internal'))
 ORDER BY s.last_run_started_at DESC NULLS LAST, s.job_id
 LIMIT $1`
}

func readTSJobRows(ctx context.Context, q catalogread.Querier, schema string) (
	[]tsJobFailure, error,
) {
	rows, err := q.Query(ctx, tsJobsSQL(schema), tsMaxJobs+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tsJobFailure
	for rows.Next() {
		var j tsJobFailure
		if err := rows.Scan(&j.JobID, &j.ProcSchema, &j.ProcName, &j.HypertableSchema,
			&j.HypertableName, &j.TotalFailures, &j.NextStart,
			&j.LastRunStartedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (a *Analyzer) readTSCaggs(ctx context.Context, q catalogread.Querier, r *tsReading,
	p tsProbe) {
	if !p.hasCaggs {
		a.tsUnread("ts_cagg_views", categoryTSCaggStale, "%s.continuous_aggregates or "+
			"dimensions does not exist", sqlident.Ident(r.Schema))
		return
	}
	a.extOK("ts_cagg_views")
	if p.watermarkFns == nil {
		a.tsUnread("ts_watermark", categoryTSCaggStale, "neither "+
			"_timescaledb_functions.cagg_watermark (2.12 and later) nor "+
			"_timescaledb_internal.cagg_watermark exists")
		return
	}
	a.extOK("ts_watermark")
	caggs, truncated, err := readTSCaggRows(ctx, q, r.Schema, *p.watermarkFns)
	if err != nil {
		a.tsUnread("ts_caggs", categoryTSCaggStale, "read the continuous aggregates "+
			"in %s and their watermarks: %v", sqlident.Ident(r.Schema), err)
		return
	}
	a.extOK("ts_caggs")
	if truncated {
		// The aggregates past the limit may hold open findings.
		a.evalFail(categoryTSCaggStale)
	}
	r.Caggs, r.caggsRead, r.WatermarkSchema = caggs, true, *p.watermarkFns
}

// readTSRefreshSuccess adds each judged aggregate's refresh policy's last
// successful finish. The watermark is the end of the newest materialized
// bucket, so it stops when the source stops receiving rows even while the
// policy keeps succeeding; a recent success clears the aggregate. Without
// job_stats (the jobs read already warned) the rule judges by the
// watermark alone.
func (a *Analyzer) readTSRefreshSuccess(ctx context.Context, q catalogread.Querier,
	r *tsReading) {
	if !r.jobsRead || len(r.Caggs) == 0 {
		return
	}
	ids := make([]int64, 0, len(r.Caggs))
	for _, c := range r.Caggs {
		ids = append(ids, c.JobID)
	}
	last, err := readTSSuccessRows(ctx, q, r.Schema, ids)
	if err != nil {
		a.extWarn("ts_cagg_success", "analyzer: timescaledb: read the refresh policies' "+
			"last successful runs in %s: %v; %s judged by the watermark alone",
			sqlident.Qualified(r.Schema, "job_stats"), err, categoryTSCaggStale)
		return
	}
	a.extOK("ts_cagg_success")
	for i := range r.Caggs {
		if at, ok := last[r.Caggs[i].JobID]; ok {
			r.Caggs[i].LastSuccess = &at
		}
	}
}

// tsSuccessSQL reads the last successful finish of the jobs $1.
func tsSuccessSQL(schema string) string {
	return `/* pg_sage analyzer */
SELECT job_id::bigint, last_successful_finish
  FROM ` + sqlident.Ident(schema) + `.job_stats
 WHERE job_id = ANY($1) AND isfinite(last_successful_finish)`
}

func readTSSuccessRows(ctx context.Context, q catalogread.Querier, schema string,
	ids []int64) (map[int64]time.Time, error) {
	rows, err := q.Query(ctx, tsSuccessSQL(schema), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]time.Time, len(ids))
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out[id] = at
	}
	return out, rows.Err()
}
