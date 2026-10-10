package perfgate

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Binding is the Sage SRE identity the runtime under test will bind to:
// pre-creating it lets most seeded SRE history belong to that runtime.
type Binding struct {
	DeploymentID string
	DatabaseID   string
	RuntimeKey   string // "<scope>:<name>", e.g. "startup:<database>"
}

// NewBinding returns a binding with fresh identifiers.
func NewBinding(runtimeKey string) Binding {
	return Binding{DeploymentID: newUUID(), DatabaseID: newUUID(), RuntimeKey: runtimeKey}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GrowingTables are the sage tables seeded with Scale.HistoryRows rows.
func GrowingTables() []string {
	out := []string{"snapshots"}
	for _, s := range historySteps {
		if s.full {
			out = append(out, s.table)
		}
	}
	for _, s := range sreSteps {
		if s.full {
			out = append(out, s.table)
		}
	}
	return out
}

// seedStep inserts one table's history. Every statement takes $1 = rows
// and spreads them over the table's retention-safe window, newest first.
type seedStep struct {
	table string
	full  bool // seeded with all HistoryRows rows (GrowingTables)
	sql   string
}

// SeedHistory pre-seeds the sage history a long-running deployment would
// have: HistoryRows rows in every growing table (snapshots in the
// keyframe+delta format), the schema guard's legacy flood
// (legacy_guard.go), with the SRE history mostly under own.
func SeedHistory(ctx context.Context, pool *pgxpool.Pool, s Scale, own Binding) error {
	if err := s.Validate(); err != nil {
		return err
	}
	var present bool
	if err := pool.QueryRow(ctx, `/* `+HarnessTag+` */ SELECT
		to_regclass('sage.snapshots') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("perfgate: check sage schema: %w", err)
	}
	if !present {
		return fmt.Errorf("perfgate: the sage schema is not bootstrapped")
	}
	if err := seedSnapshots(ctx, pool, s.HistoryRows); err != nil {
		return err
	}
	for _, step := range historySteps {
		if _, err := pool.Exec(ctx, step.sql, s.HistoryRows); err != nil {
			return fmt.Errorf("perfgate: seed sage.%s: %w", step.table, err)
		}
	}
	if err := seedLegacyGuardFlood(ctx, pool, s); err != nil {
		return err
	}
	return seedSRE(ctx, pool, s.HistoryRows, own)
}

// AnalyzeSage vacuums and analyzes the database, as autovacuum would have
// on a long-running deployment, so plans and live-row counts are real.
// VACUUM sets a table's live rows to the count it finds; statistics a
// seeding session had not flushed yet (PostgreSQL 15+ flushes an idle
// session's at most once a second, 14 every 500 ms) would be added on top
// afterwards and count those rows twice. So the pool's idle sessions are
// closed first: a backend flushes its statistics as it exits.
func AnalyzeSage(ctx context.Context, pool *pgxpool.Pool) error {
	if err := closeIdleSessions(ctx, pool); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE)"); err != nil {
		return fmt.Errorf("perfgate: vacuum analyze: %w", err)
	}
	return awaitAllVisible(ctx, pool, allVisibleWait)
}

// closeIdleSessions closes the pool's idle sessions and waits until their
// backends have left pg_stat_activity, which they do after flushing their
// statistics.
func closeIdleSessions(ctx context.Context, pool *pgxpool.Pool) error {
	var pids []int64
	for _, conn := range pool.AcquireAllIdle(ctx) {
		pids = append(pids, int64(conn.Conn().PgConn().PID()))
		if err := conn.Hijack().Close(ctx); err != nil {
			return fmt.Errorf("perfgate: close a seeding session: %w", err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var left int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE pid = ANY($1)`, pids).Scan(&left)
		if err != nil {
			return fmt.Errorf("perfgate: wait for seeding sessions to exit: %w", err)
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("perfgate: %d seeding sessions still running after 30s", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spread(window) is a timestamp for row g of $1, newest first.
func spread(window string) string {
	return "now() - (g::double precision / $1::int) * interval '" + window + "'"
}

var historySteps = []seedStep{
	{"query_store", true, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time, rows, plan_hash, stats_epoch)
		SELECT ` + spread("60 days") + `, 1000 + g % 2000, g, g * 1.5, 1.5, g,
		md5((g % 2000)::text), now() - interval '30 days'
		FROM generate_series(1, $1) g`},
	{"decision", true, `INSERT INTO sage.decision (feature, intent, target_objects,
		verdict, risk_tier, reason, evidence, evidence_id, created_at, resolved_at)
		SELECT (ARRAY['schema_guard','executor','index_advisor','retention','custodian'])
		[1 + g % 5], (ARRAY['retention_cleanup','append_only_unbounded','missing_pk'])
		[1 + g % 3], jsonb_build_array(jsonb_build_object('schema',
		'perf_app_' || lpad((g % 60)::text, 3, '0'), 'table',
		't_' || lpad((g % 3000)::text, 4, '0'))),
		(ARRAY['execute','queue_approval','parked','blocked','observe_only'])[1 + g % 5],
		(ARRAY['read_only','safe','moderate','high'])[1 + g % 4], 'perfgate history',
		jsonb_build_object('disposition', CASE WHEN g % 3 = 0 THEN 'dry_run' ELSE 'apply' END),
		'perfgate-' || g, ` + spread("60 days") + `, ` + spread("60 days") + ` + interval '1 minute'
		FROM generate_series(1, $1) g`},
	{"action_log", true, `INSERT INTO sage.action_log (executed_at, action_type,
		finding_id, sql_executed, outcome, decision_id)
		SELECT ` + spread("60 days") + `, (ARRAY['create_index','vacuum','analyze','reindex'])
		[1 + g % 4], g, 'SELECT 1', (ARRAY['success','failed','rolled_back'])[1 + g % 3],
		CASE WHEN g % 2 = 0 THEN d.first + g - 1 END
		FROM generate_series(1, $1) g, (SELECT min(id) AS first FROM sage.decision) d`},
	{"findings", true, `INSERT INTO sage.findings (created_at, last_seen, category,
		severity, object_type, object_identifier, title, detail, status, resolved_at, rule_id)
		SELECT ` + spread("60 days") + `, ` + spread("60 days") + `,
		(ARRAY['unused_index','missing_index','table_bloat','seq_scan_heavy','duplicate_index'])
		[1 + g % 5], (ARRAY['info','warning','critical'])[1 + g % 3], 'table',
		'perf_app_' || lpad((g % 60)::text, 3, '0') || '.t_' || g, 'perfgate history',
		jsonb_build_object('n', g), CASE WHEN g <= least($1 / 100, 500) THEN 'open'
		ELSE 'resolved' END, CASE WHEN g > least($1 / 100, 500) THEN ` + spread("60 days") + ` END,
		'perfgate'
		FROM generate_series(1, $1) g`},
	{"incidents", true, `INSERT INTO sage.incidents (detected_at, last_detected_at,
		severity, root_cause, source, database_name, identity_key, resolved_at, resolved_by)
		SELECT ` + spread("60 days") + `, ` + spread("60 days") + `,
		(ARRAY['info','warning','critical'])[1 + g % 3], 'perfgate history',
		(ARRAY['deterministic','log_deterministic','schema_lint'])[1 + g % 3],
		CASE WHEN g % 10 < 7 THEN current_database() ELSE 'other_' || g % 5 END,
		'perfgate-' || g % 5000, ` + spread("60 days") + ` + interval '5 minutes', 'perfgate'
		FROM generate_series(1, $1) g`},
	{"recommendation", true, `INSERT INTO sage.recommendation (identity_key, database_name,
		category, target, action_type, state, revision, content_hash, retry_budget,
		created_at, updated_at, last_seen_at)
		SELECT 'perfgate-' || g % 20000, current_database(), 'missing_index',
		'perf_app_000.t_' || lpad((g % 50)::text, 4, '0'), 'create_index',
		(ARRAY['verified','reverted','inconclusive','superseded','abandoned'])[1 + g % 5],
		1, md5(g::text), 3, ` + spread("60 days") + `, ` + spread("60 days") + `,
		` + spread("60 days") + `
		FROM generate_series(1, $1) g`},
	{"recommendation_revision", true, `INSERT INTO sage.recommendation_revision
		(recommendation_id, revision, content_hash, forward_sql, source, created_at)
		SELECT id, 1, content_hash, 'CREATE INDEX CONCURRENTLY ON ' || target || ' (id)',
		'analyzer', created_at FROM sage.recommendation
		WHERE identity_key LIKE 'perfgate-%' LIMIT $1`},
	{"recommendation_transition", true, `INSERT INTO sage.recommendation_transition
		(recommendation_id, from_state, to_state, revision, actor, created_at)
		SELECT id, 'proposed', state, 1, 'perfgate', updated_at FROM sage.recommendation
		WHERE identity_key LIKE 'perfgate-%' LIMIT $1`},
	{"verification", true, `INSERT INTO sage.verification (decision_id, criterion, baseline,
		minimum_samples, next_evaluation_at, hard_deadline_at, verdict, created_at, completed_at)
		SELECT d.first + g - 1, '{}', '{}', 3, ` + spread("60 days") + `,
		` + spread("60 days") + ` + interval '1 hour',
		(ARRAY['success','revert','failed','unverifiable'])[1 + g % 4],
		` + spread("60 days") + `, ` + spread("60 days") + ` + interval '1 hour'
		FROM generate_series(1, $1) g, (SELECT min(id) AS first FROM sage.decision) d`},
	{"size_history", true, `INSERT INTO sage.size_history (collected_at, metric_type,
		object_name, size_bytes, dead_tuple_pct, database_name)
		SELECT ` + spread("60 days") + `, (ARRAY['database','table'])[1 + g % 2],
		'perf_app_000.t_' || lpad((g % 3000)::text, 4, '0'), 8192 * g, 1.5, current_database()
		FROM generate_series(1, $1) g`},
	{"health_history", true, `INSERT INTO sage.health_history (recorded_at, database_name,
		health_score, findings_open) SELECT ` + spread("60 days") + `,
		CASE WHEN g % 10 < 7 THEN current_database() ELSE 'other_' || g % 5 END, 90, 3
		FROM generate_series(1, $1) g`},
	{"alert_log", true, `INSERT INTO sage.alert_log (sent_at, severity, channel, dedup_key)
		SELECT ` + spread("60 days") + `, 'warning', 'slack', 'perfgate-' || g % 1000
		FROM generate_series(1, $1) g`},
	{"notification_log", true, `INSERT INTO sage.notification_log (event, subject, status,
		sent_at) SELECT 'finding', 'perfgate history', 'sent', ` + spread("60 days") + `
		FROM generate_series(1, $1) g`},
	{"explain_cache", true, `INSERT INTO sage.explain_cache (captured_at, queryid, query_text,
		plan_json, source, total_cost) SELECT ` + spread("60 days") + `, 1000 + g % 2000,
		'SELECT 1', '[{"Plan": {"Node Type": "Result"}}]', 'collector', 1.0
		FROM generate_series(1, $1) g`},
	{"runway_samples", true, `INSERT INTO sage.runway_samples (kind, subject, epoch,
		sampled_at, value, counter, limit_value)
		SELECT (ARRAY['wraparound','disk','sequence','wal'])[1 + g % 4],
		'perf_app_000.t_' || lpad((g % 3000)::text, 4, '0') || '_id_seq', 'perfgate',
		` + spread("40 hours") + `, g, g, 1e9
		FROM generate_series(1, $1) g`},
	{"io_rate_sample", true, `INSERT INTO sage.io_rate_sample (database_name, sampled_at,
		interval_seconds, data_bytes_per_sec, wal_bytes_per_sec, source)
		SELECT current_database(), ` + spread("13 days") + `, 60, 1e6, 1e5, 'pg_stat_io'
		FROM generate_series(1, $1) g`},
}
