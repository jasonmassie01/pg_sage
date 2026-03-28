package schema

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// expectedTables lists every table the sage schema must contain.
var expectedTables = []struct {
	name string
	ddl  string
}{
	{"action_log", ddlActionLog},
	{"snapshots", ddlSnapshots},
	{"findings", ddlFindings},
	{"explain_cache", ddlExplainCache},
	{"briefings", ddlBriefings},
	{"config", ddlConfig},
	{"alert_log", ddlAlertLog},
	{"query_hints", ddlQueryHints},
	{"users", ddlUsers},
	{"sessions", ddlSessions},
	{"databases", ddlDatabases},
	{"notification_channels", ddlNotificationChannels},
	{"notification_rules", ddlNotificationRules},
	{"notification_log", ddlNotificationLog},
	{"action_queue", ddlActionQueue},
}

// Bootstrap acquires an advisory lock, then ensures the sage schema and
// all required tables exist. It never drops existing objects.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	if err := acquireAdvisoryLock(ctx, pool); err != nil {
		return err
	}

	exists, err := schemaExists(ctx, pool)
	if err != nil {
		return fmt.Errorf("checking sage schema: %w", err)
	}

	if !exists {
		return createFullSchema(ctx, pool)
	}

	return ensureTablesExist(ctx, pool)
}

// ReleaseAdvisoryLock releases the pg_sage advisory lock.
func ReleaseAdvisoryLock(ctx context.Context, pool *pgxpool.Pool) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = pool.Exec(qctx, "SELECT pg_advisory_unlock(hashtext('pg_sage'))")
}

// PersistTrustRampStart reads or initialises the trust_ramp_start
// timestamp in sage.config, returning the effective start time.
// If the key does not yet exist and configRampStart is non-zero,
// that value is used instead of now().
func PersistTrustRampStart(
	ctx context.Context, pool *pgxpool.Pool, configRampStart time.Time,
) (time.Time, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var raw string
	err := pool.QueryRow(
		qctx,
		"SELECT value FROM sage.config WHERE key = 'trust_ramp_start'",
	).Scan(&raw)
	if err == nil {
		// Try multiple timestamp formats PG may produce.
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999-07",
			"2006-01-02T15:04:05.999999-07:00",
			"2006-01-02 15:04:05.999999-07",
			"2006-01-02 15:04:05.999999-07:00",
		} {
			if t, parseErr := time.Parse(layout, raw); parseErr == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf(
			"parsing trust_ramp_start %q: no matching format", raw,
		)
	}

	// Key does not exist — insert configRampStart (if set) or now().
	qctx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()

	var t time.Time
	if !configRampStart.IsZero() {
		err = pool.QueryRow(
			qctx2,
			"INSERT INTO sage.config (key, value, updated_by) "+
				"VALUES ('trust_ramp_start', $1, 'bootstrap') "+
				"ON CONFLICT (key) DO NOTHING "+
				"RETURNING value::timestamptz",
			configRampStart.Format(time.RFC3339Nano),
		).Scan(&t)
	} else {
		err = pool.QueryRow(
			qctx2,
			"INSERT INTO sage.config (key, value, updated_by) "+
				"VALUES ('trust_ramp_start', to_char(now(), 'YYYY-MM-DD\"T\"HH24:MI:SS.USOF'), 'bootstrap') "+
				"ON CONFLICT (key) DO NOTHING "+
				"RETURNING value::timestamptz",
		).Scan(&t)
	}
	if err != nil {
		// Race: another instance inserted between our SELECT and INSERT.
		qctx3, cancel3 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel3()
		err = pool.QueryRow(
			qctx3,
			"SELECT value::timestamptz FROM sage.config "+
				"WHERE key = 'trust_ramp_start'",
		).Scan(&t)
		if err != nil {
			return time.Time{}, fmt.Errorf(
				"reading trust_ramp_start after insert: %w", err,
			)
		}
	}
	return t, nil
}

func acquireAdvisoryLock(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var acquired bool
	err := pool.QueryRow(
		qctx,
		"SELECT pg_try_advisory_lock(hashtext('pg_sage'))",
	).Scan(&acquired)
	if err != nil {
		return fmt.Errorf("advisory lock query failed: %w", err)
	}
	if !acquired {
		return fmt.Errorf(
			"another pg_sage instance holds the advisory lock",
		)
	}
	return nil
}

func schemaExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := pool.QueryRow(
		qctx,
		"SELECT 1 FROM information_schema.schemata "+
			"WHERE schema_name = 'sage'",
	).Scan(&one)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func createFullSchema(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(qctx)

	_, err = tx.Exec(qctx, fullSchemaDDL)
	if err != nil {
		return fmt.Errorf(
			"executing schema DDL: %w\n"+
				"hint: if the user lacks CREATE privilege, "+
				"run as superuser: CREATE SCHEMA sage; "+
				"GRANT ALL ON SCHEMA sage TO sage_agent;", err)
	}

	return tx.Commit(qctx)
}

func ensureTablesExist(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for _, tbl := range expectedTables {
		var one int
		err := pool.QueryRow(
			qctx,
			"SELECT 1 FROM information_schema.tables "+
				"WHERE table_schema = 'sage' AND table_name = $1",
			tbl.name,
		).Scan(&one)
		if err != nil {
			// Table missing — create it.
			_, execErr := pool.Exec(qctx, tbl.ddl)
			if execErr != nil {
				return fmt.Errorf("creating table sage.%s: %w", tbl.name, execErr)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DDL constants
// ---------------------------------------------------------------------------

const fullSchemaDDL = `
CREATE SCHEMA sage;
` + ddlActionLog + ddlSnapshots + ddlFindings +
	ddlExplainCache + ddlBriefings + ddlConfig +
	ddlAlertLog + ddlQueryHints + ddlExplainSourceIdx +
	ddlUsers + ddlSessions + ddlDatabases +
	ddlNotificationChannels + ddlNotificationRules +
	ddlNotificationLog + ddlActionQueue +
	ddlActionLogApprovalCols

const ddlActionLog = `
CREATE TABLE IF NOT EXISTS sage.action_log (
    id              bigserial PRIMARY KEY,
    executed_at     timestamptz NOT NULL DEFAULT now(),
    action_type     text NOT NULL,
    finding_id      bigint,
    sql_executed    text NOT NULL,
    rollback_sql    text,
    before_state    jsonb,
    after_state     jsonb,
    outcome         text NOT NULL DEFAULT 'pending',
    rollback_reason text,
    measured_at     timestamptz
);
CREATE INDEX IF NOT EXISTS idx_action_log_time
    ON sage.action_log (executed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_log_finding
    ON sage.action_log (finding_id)
    WHERE finding_id IS NOT NULL;
`

const ddlSnapshots = `
CREATE TABLE IF NOT EXISTS sage.snapshots (
    id              bigserial PRIMARY KEY,
    collected_at    timestamptz NOT NULL DEFAULT now(),
    category        text NOT NULL,
    data            jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_time
    ON sage.snapshots (collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_category
    ON sage.snapshots (category, collected_at DESC);
`

const ddlFindings = `
CREATE TABLE IF NOT EXISTS sage.findings (
    id                  bigserial PRIMARY KEY,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_seen           timestamptz NOT NULL DEFAULT now(),
    occurrence_count    integer NOT NULL DEFAULT 1,
    category            text NOT NULL,
    severity            text NOT NULL,
    object_type         text,
    object_identifier   text,
    title               text NOT NULL,
    detail              jsonb NOT NULL,
    recommendation      text,
    recommended_sql     text,
    rollback_sql        text,
    estimated_cost_usd  numeric(10,2),
    status              text NOT NULL DEFAULT 'open',
    suppressed_until    timestamptz,
    resolved_at         timestamptz,
    acted_on_at         timestamptz,
    action_log_id       bigint REFERENCES sage.action_log(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_findings_dedup
    ON sage.findings (category, object_identifier)
    WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_findings_status
    ON sage.findings (status, severity, last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_findings_object
    ON sage.findings (object_identifier, category);
CREATE INDEX IF NOT EXISTS idx_findings_category_status
    ON sage.findings (category, severity)
    WHERE status = 'open';
`

const ddlExplainCache = `
CREATE TABLE IF NOT EXISTS sage.explain_cache (
    id              bigserial PRIMARY KEY,
    captured_at     timestamptz NOT NULL DEFAULT now(),
    queryid         bigint NOT NULL,
    query_text      text,
    plan_json       jsonb NOT NULL,
    source          text NOT NULL,
    total_cost      float,
    execution_time  float
);
CREATE INDEX IF NOT EXISTS idx_explain_queryid
    ON sage.explain_cache (queryid, captured_at DESC);
`

const ddlBriefings = `
CREATE TABLE IF NOT EXISTS sage.briefings (
    id              bigserial PRIMARY KEY,
    generated_at    timestamptz NOT NULL DEFAULT now(),
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    mode            text NOT NULL,
    content_text    text NOT NULL,
    content_json    jsonb NOT NULL,
    llm_used        boolean NOT NULL DEFAULT false,
    token_count     integer,
    delivery_status jsonb
);
`

const ddlConfig = `
CREATE TABLE IF NOT EXISTS sage.config (
    key             text PRIMARY KEY,
    value           text NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      text
);
`

const ddlAlertLog = `
CREATE TABLE IF NOT EXISTS sage.alert_log (
    id            bigserial PRIMARY KEY,
    sent_at       timestamptz NOT NULL DEFAULT now(),
    finding_id    bigint REFERENCES sage.findings(id),
    severity      text NOT NULL,
    channel       text NOT NULL,
    dedup_key     text NOT NULL,
    status        text NOT NULL DEFAULT 'sent',
    error_message text
);
CREATE INDEX IF NOT EXISTS idx_alert_log_dedup
    ON sage.alert_log (dedup_key, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_alert_log_finding
    ON sage.alert_log (finding_id);
`

const ddlQueryHints = `
CREATE TABLE IF NOT EXISTS sage.query_hints (
    id             bigserial PRIMARY KEY,
    created_at     timestamptz NOT NULL DEFAULT now(),
    queryid        bigint NOT NULL,
    hint_plan_id   bigint,
    hint_text      text NOT NULL,
    symptom        text NOT NULL,
    before_cost    float,
    after_cost     float,
    status         text NOT NULL DEFAULT 'active',
    verified_at    timestamptz,
    rolled_back_at timestamptz
);
CREATE INDEX IF NOT EXISTS idx_query_hints_queryid
    ON sage.query_hints (queryid) WHERE status = 'active';
`

const ddlExplainSourceIdx = `
CREATE INDEX IF NOT EXISTS idx_explain_source
    ON sage.explain_cache (source, queryid, captured_at DESC);
`

const ddlUsers = `
CREATE TABLE IF NOT EXISTS sage.users (
    id          SERIAL PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    password    TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'viewer',
    created_at  TIMESTAMPTZ DEFAULT now(),
    last_login  TIMESTAMPTZ
);
`

const ddlSessions = `
CREATE TABLE IF NOT EXISTS sage.sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     INT REFERENCES sage.users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);
`

const ddlActionQueue = `
CREATE TABLE IF NOT EXISTS sage.action_queue (
    id              SERIAL PRIMARY KEY,
    database_id     INT,
    finding_id      INT,
    proposed_sql    TEXT NOT NULL,
    rollback_sql    TEXT,
    action_risk     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    proposed_at     TIMESTAMPTZ DEFAULT now(),
    decided_by      INT,
    decided_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ DEFAULT now() + INTERVAL '7 days',
    reason          TEXT
);
CREATE INDEX IF NOT EXISTS idx_action_queue_status
    ON sage.action_queue (status, proposed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_queue_finding
    ON sage.action_queue (finding_id)
    WHERE status = 'pending';
`

const ddlActionLogApprovalCols = `
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_by INT;
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
`
