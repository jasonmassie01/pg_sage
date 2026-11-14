package analyzer

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

const (
	// cronWindow is how far back job failures are read; a run exactly
	// this old is included.
	cronWindow = 24 * time.Hour
	// cronMaxJobs bounds the failing jobs reported per cycle.
	cronMaxJobs = 50
	// cronHistoryMaxRows is the job_run_details size (pg_class.reltuples)
	// above which the history is reported as unbounded.
	cronHistoryMaxRows = 1000000
	// cronMessageRunes clips a failed run's message.
	cronMessageRunes = 300
)

// cronReading is one cycle's pg_cron reading. failuresRead and historyRead
// say which parts were read; an unread part emits nothing.
type cronReading struct {
	Schema       string
	Failures     []cronFailure
	failuresRead bool
	// OwnJobsOnly is set when row security shows the role only the jobs
	// it owns.
	OwnJobsOnly bool
	HistoryRows float64
	historyRead bool
}

// cronFailure is one job whose latest finished run in the window failed.
type cronFailure struct {
	JobID        int64
	JobName      *string
	Failures     int64
	LastFailedAt time.Time
	LastMessage  string
}

// cronProbe is what the catalog says about pg_cron's tables.
type cronProbe struct {
	hasDetails  bool
	hasJobname  bool
	reltuples   float64
	rowSecurity bool
}

// cronProbeSQL reads job_run_details' presence, size estimate and row
// security, and whether job has jobname, from the catalog only ($1 is the
// quoted job_run_details, $2 the quoted job).
const cronProbeSQL = `/* pg_sage analyzer */
SELECT to_regclass($1) IS NOT NULL,
       EXISTS (SELECT 1 FROM pg_attribute
                WHERE attrelid = to_regclass($2) AND attname = 'jobname'
                  AND attnum > 0 AND NOT attisdropped),
       COALESCE((SELECT reltuples::float8 FROM pg_class
                  WHERE oid = to_regclass($1)), -1),
       CASE WHEN to_regclass($1) IS NULL THEN false
            ELSE row_security_active(to_regclass($1)) END`

func readCronProbe(ctx context.Context, q catalogread.Querier, schema string) (
	cronProbe, error,
) {
	var p cronProbe
	err := q.QueryRow(ctx, cronProbeSQL, sqlident.Qualified(schema, "job_run_details"),
		sqlident.Qualified(schema, "job")).
		Scan(&p.hasDetails, &p.hasJobname, &p.reltuples, &p.rowSecurity)
	return p, err
}

// readCron reads pg_cron's state in schema; it marks what it could not
// read as not evaluated.
func (a *Analyzer) readCron(ctx context.Context, q catalogread.Querier,
	schema string) *cronReading {
	r := &cronReading{Schema: schema}
	tbl := sqlident.Qualified(schema, "job_run_details")
	p, err := readCronProbe(ctx, q, schema)
	if err != nil {
		a.extWarn("cron_probe", "analyzer: pg_cron: read the catalog entry of %s: %v; "+
			"pg_cron checks not evaluated", tbl, err)
		a.evalFail(cronCategories...)
		return r
	}
	a.extOK("cron_probe")
	if !p.hasDetails {
		a.extWarn("cron_details", "analyzer: pg_cron: %s does not exist (pg_cron 1.4 or "+
			"later keeps run history there); pg_cron checks not evaluated", tbl)
		a.evalFail(cronCategories...)
		return r
	}
	a.extOK("cron_details")
	r.HistoryRows, r.historyRead = p.reltuples, true
	a.readCronFailures(ctx, q, r, p)
	return r
}

// readCronFailures reads the failing jobs. Under row security a role
// that sees no job reports its visibility instead of health.
func (a *Analyzer) readCronFailures(ctx context.Context, q catalogread.Querier,
	r *cronReading, p cronProbe) {
	tbl := sqlident.Qualified(r.Schema, "job_run_details")
	if p.rowSecurity {
		r.OwnJobsOnly = true
		var visible int64
		err := q.QueryRow(ctx, "/* pg_sage analyzer */ SELECT count(*) FROM "+
			sqlident.Qualified(r.Schema, "job")).Scan(&visible)
		if err != nil || visible == 0 {
			a.cronFailuresUnread(tbl, err)
			r.failuresRead = err == nil
			return
		}
	}
	cutoff := a.extras.now().Add(-cronWindow)
	fs, err := readCronFailureRows(ctx, q, r.Schema, p.hasJobname, cutoff)
	if err != nil {
		a.cronFailuresUnread(tbl, err)
		return
	}
	a.extOK("cron_failures")
	if len(fs) > cronMaxJobs {
		// The jobs past the limit may hold open findings.
		fs = fs[:cronMaxJobs]
		a.evalFail(categoryCronJobFailed)
	}
	r.Failures, r.failuresRead = fs, true
}

// cronFailuresUnread marks pg_cron_job_failed not evaluated; err is nil
// when row security hides every job.
func (a *Analyzer) cronFailuresUnread(tbl string, err error) {
	a.evalFail(categoryCronJobFailed)
	if err == nil {
		return
	}
	a.extWarn("cron_failures", "analyzer: pg_cron: read %s: %v; %s not evaluated until "+
		"the monitoring role can read it (GRANT SELECT ON %s)", tbl, err,
		categoryCronJobFailed, tbl)
}

// cronFailuresSQL lists the jobs whose latest finished run since $1
// failed, newest failure first, at most $2. A failure and a success at
// the same instant count as a failure.
func cronFailuresSQL(schema string, hasJobname bool) string {
	s := sqlident.Ident(schema)
	name := "NULL::text"
	if hasJobname {
		name = "max(j.jobname)"
	}
	return `/* pg_sage analyzer */
WITH runs AS (
  SELECT d.jobid, d.status, d.return_message,
         COALESCE(d.end_time, d.start_time) AS at
    FROM ` + s + `.job_run_details d
   WHERE d.status IN ('failed', 'succeeded')
     AND COALESCE(d.end_time, d.start_time) >= $1
), last AS (
  SELECT DISTINCT ON (jobid) jobid, status
    FROM runs ORDER BY jobid, at DESC, (status = 'failed') DESC
)
SELECT r.jobid, ` + name + `, count(*), max(r.at),
       (array_agg(r.return_message ORDER BY r.at DESC))[1]
  FROM runs r
  JOIN last l ON l.jobid = r.jobid AND l.status = 'failed'
  LEFT JOIN ` + s + `.job j ON j.jobid = r.jobid
 WHERE r.status = 'failed'
 GROUP BY r.jobid
 ORDER BY max(r.at) DESC, r.jobid
 LIMIT $2`
}

func readCronFailureRows(ctx context.Context, q catalogread.Querier, schema string,
	hasJobname bool, cutoff time.Time) ([]cronFailure, error) {
	rows, err := q.Query(ctx, cronFailuresSQL(schema, hasJobname), cutoff, cronMaxJobs+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cronFailure
	for rows.Next() {
		var f cronFailure
		var msg *string
		if err := rows.Scan(&f.JobID, &f.JobName, &f.Failures, &f.LastFailedAt,
			&msg); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if msg != nil {
			f.LastMessage = cronMessage(*msg)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// cronMessage redacts a run's message's literals and clips it to
// cronMessageRunes runes.
func cronMessage(msg string) string {
	msg = llm.RedactSQLLiterals(msg)
	if utf8.RuneCountInString(msg) <= cronMessageRunes {
		return msg
	}
	return string([]rune(msg)[:cronMessageRunes]) + "…"
}

// rulePgCronJobFailed reports each pg_cron job whose latest run failed.
func rulePgCronJobFailed(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.cronReading()
	if r == nil || !r.failuresRead {
		return nil
	}
	visibility := "all_jobs"
	if r.OwnJobsOnly {
		visibility = "own_jobs_only"
		if len(r.Failures) == 0 {
			return []Finding{cronVisibilityFinding(r.Schema)}
		}
	}
	out := make([]Finding, 0, len(r.Failures))
	for _, f := range r.Failures {
		out = append(out, cronFailureFinding(r.Schema, f, visibility))
	}
	return out
}

func cronFailureFinding(schema string, f cronFailure, visibility string) Finding {
	label := fmt.Sprintf("%d", f.JobID)
	detail := map[string]any{
		"jobid":          f.JobID,
		"failures":       f.Failures,
		"last_failed_at": f.LastFailedAt.UTC().Format(time.RFC3339),
		"last_message":   f.LastMessage,
		"window_hours":   int(cronWindow / time.Hour),
		"visibility":     visibility,
		"schema":         schema,
	}
	if f.JobName != nil {
		detail["jobname"] = *f.JobName
		label = fmt.Sprintf("%q (jobid %d)", *f.JobName, f.JobID)
	}
	return Finding{
		Category:         categoryCronJobFailed,
		Severity:         "warning",
		ObjectType:       "pg_cron_job",
		ObjectIdentifier: fmt.Sprintf("pg_cron:job:%d", f.JobID),
		Title: fmt.Sprintf("pg_cron job %s failed %d times in the last 24 hours",
			label, f.Failures),
		Detail: detail,
		Recommendation: fmt.Sprintf("Read the job's recent runs in %s, fix the "+
			"command or what it depends on, and remove an obsolete job with "+
			"cron.unschedule(%d). Advisory only: pg_sage runs nothing.",
			sqlident.Qualified(schema, "job_run_details"), f.JobID),
	}
}

func cronVisibilityFinding(schema string) Finding {
	tbl := sqlident.Qualified(schema, "job_run_details")
	return Finding{
		Category:         categoryCronJobFailed,
		Severity:         "info",
		ObjectType:       "extension",
		ObjectIdentifier: "pg_cron:visibility",
		Title:            "pg_sage sees only its own pg_cron jobs, so job health is not reported",
		Detail:           map[string]any{"visibility": "own_jobs_only", "schema": schema},
		Recommendation: fmt.Sprintf("Row security on %s shows the monitoring role only "+
			"the jobs it owns. Monitor with a role that reads every job (superuser on a "+
			"self-managed server; the provider's admin role on RDS or Cloud SQL).", tbl),
	}
}

// rulePgCronHistoryUnbounded reports a job_run_details that grew past
// cronHistoryMaxRows: pg_cron never purges it.
func rulePgCronHistoryUnbounded(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.cronReading()
	if r == nil || !r.historyRead || r.HistoryRows <= cronHistoryMaxRows {
		return nil
	}
	tbl := sqlident.Qualified(r.Schema, "job_run_details")
	return []Finding{{
		Category:         categoryCronHistory,
		Severity:         "info",
		ObjectType:       "table",
		ObjectIdentifier: tbl,
		Title: fmt.Sprintf("pg_cron run history %s holds about %.0f rows",
			tbl, r.HistoryRows),
		Detail: map[string]any{
			"estimated_rows": int64(r.HistoryRows),
			"threshold_rows": cronHistoryMaxRows,
			"schema":         r.Schema,
		},
		Recommendation: fmt.Sprintf("pg_cron never purges %s. Schedule a cleanup job, "+
			"for example SELECT cron.schedule('purge-cron-history', '0 3 * * *', "+
			"$$DELETE FROM %s WHERE end_time < now() - interval '7 days'$$), or set "+
			"cron.log_run = off when run history is not needed. Advisory only.", tbl, tbl),
	}}
}

func (e *RuleExtras) cronReading() *cronReading {
	if e == nil || e.ext == nil {
		return nil
	}
	return e.ext.cron
}
