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
	{"incidents", ddlIncidents},
	{"size_history", ddlSizeHistory},
	{"explain_results", ddlExplainResults},
	{"schema_findings", ddlSchemaFindings},
	{"crypto_meta", ddlCryptoMeta},
	{"health_history", ddlHealthHistory},
}

// Bootstrap acquires an advisory lock, then ensures the sage schema and
// all required tables exist. It never drops existing objects.
//
// v0.8.5 — Bootstrap now folds in MigrateConfigSchema as a final step so
// every caller receives a fully-migrated sage.config (with database_id /
// updated_by_user_id columns) and a live sage.config_audit table. Before
// this change, any code path that dropped + re-bootstrapped the schema
// (notably the schema-package tests) left sage.config_audit missing,
// which caused unrelated integration tests elsewhere to fail intermittently
// when run in parallel.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	if err := acquireAdvisoryLock(ctx, pool); err != nil {
		return err
	}

	exists, err := schemaExists(ctx, pool)
	if err != nil {
		return fmt.Errorf("checking sage schema: %w", err)
	}

	if !exists {
		if err := createFullSchema(ctx, pool); err != nil {
			return err
		}
	} else {
		if err := ensureTablesExist(ctx, pool); err != nil {
			return err
		}
	}

	if err := MigrateConfigSchema(ctx, pool); err != nil {
		return fmt.Errorf("config migration: %w", err)
	}
	if err := migrateIncidentConstraints(ctx, pool); err != nil {
		return fmt.Errorf("incident constraint migration: %w", err)
	}
	return nil
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
				"RETURNING value::timestamptz",
			configRampStart.Format(time.RFC3339Nano),
		).Scan(&t)
	} else {
		err = pool.QueryRow(
			qctx2,
			"INSERT INTO sage.config (key, value, updated_by) "+
				"VALUES ('trust_ramp_start', to_char(now(), 'YYYY-MM-DD\"T\"HH24:MI:SS.USOF'), 'bootstrap') "+
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
	// Use blocking pg_advisory_lock with a timeout instead of
	// pg_try_advisory_lock. This prevents spurious failures when
	// multiple sidecar instances or test packages start concurrently
	// — the lock is held only briefly during schema bootstrap, so
	// waiting up to 30 seconds is acceptable.
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := pool.Exec(
		qctx,
		"SELECT pg_advisory_lock(hashtext('pg_sage'))",
	)
	if err != nil {
		return fmt.Errorf(
			"advisory lock: %w (another pg_sage instance "+
				"may be bootstrapping)", err,
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

	// Run idempotent migrations for existing schemas.
	if err := runMigrations(ctx, pool); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}

// runMigrations applies idempotent schema changes to existing installs.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	migrations := []string{
		ddlActionLogApprovalCols,
		ddlUsersOAuth,
		ddlQueryHintsRewrite,
		ddlQueryHintsRevalidate,
		ddlIncidentsLastDetected,
		ddlActionQueueLifecycleCols,
		ddlSchemaFindingsLintRunner,
		ddlFindingsAbsorbsSchemaFindings,
		ddlFindingsBackfillFromSchemaFindings,
	}
	for _, m := range migrations {
		if _, err := pool.Exec(qctx, m); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DDL constants
// ---------------------------------------------------------------------------

const fullSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS sage;
` + ddlActionLog + ddlSnapshots + ddlFindings +
	ddlExplainCache + ddlBriefings + ddlConfig +
	ddlAlertLog + ddlQueryHints + ddlExplainSourceIdx +
	ddlUsers + ddlSessions + ddlDatabases +
	ddlNotificationChannels + ddlNotificationRules +
	ddlNotificationLog + ddlActionQueue +
	ddlActionLogApprovalCols + ddlUsersOAuth +
	ddlQueryHintsRewrite + ddlQueryHintsRevalidate +
	ddlIncidents + ddlSizeHistory + ddlExplainResults +
	ddlSchemaFindings + ddlCryptoMeta + ddlHealthHistory

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
    action_log_id       bigint REFERENCES sage.action_log(id),
    -- v0.11 — absorbs sage.schema_findings. rule_id and impact_score are
    -- populated only for findings with category = 'schema_lint:<rule>'.
    rule_id             text,
    impact_score        real
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

const ddlCryptoMeta = `
CREATE TABLE IF NOT EXISTS sage.crypto_meta (
    id          integer PRIMARY KEY DEFAULT 1,
    kdf_salt    bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT crypto_meta_singleton CHECK (id = 1)
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
    reason          TEXT,
    action_type     TEXT,
    identity_key    TEXT,
    policy_decision TEXT,
    guardrails      JSONB NOT NULL DEFAULT '[]'::jsonb,
    attempt_count   INT NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    cooldown_until  TIMESTAMPTZ,
    failure_fingerprint TEXT,
    last_failure_fingerprint TEXT,
    verification_status TEXT NOT NULL DEFAULT 'not_started',
    shadow_toil_minutes INT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_action_queue_status
    ON sage.action_queue (status, proposed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_queue_finding
    ON sage.action_queue (finding_id)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_action_queue_identity
    ON sage.action_queue (identity_key, status)
    WHERE identity_key IS NOT NULL;
`

const ddlActionLogApprovalCols = `
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_by INT;
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
`

const ddlActionQueueLifecycleCols = `
ALTER TABLE sage.action_queue
    ADD COLUMN IF NOT EXISTS action_type TEXT,
    ADD COLUMN IF NOT EXISTS identity_key TEXT,
    ADD COLUMN IF NOT EXISTS policy_decision TEXT,
    ADD COLUMN IF NOT EXISTS guardrails JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cooldown_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS failure_fingerprint TEXT,
    ADD COLUMN IF NOT EXISTS last_failure_fingerprint TEXT,
    ADD COLUMN IF NOT EXISTS verification_status TEXT NOT NULL DEFAULT 'not_started',
    ADD COLUMN IF NOT EXISTS shadow_toil_minutes INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_action_queue_identity
    ON sage.action_queue (identity_key, status)
    WHERE identity_key IS NOT NULL;
`

const ddlUsersOAuth = `
ALTER TABLE sage.users
    ADD COLUMN IF NOT EXISTS oauth_provider TEXT DEFAULT '';
ALTER TABLE sage.users
    ALTER COLUMN password DROP NOT NULL;
`

const ddlQueryHintsRewrite = `
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS suggested_rewrite TEXT DEFAULT '';
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS rewrite_rationale TEXT DEFAULT '';
`

// ddlQueryHintsRevalidate adds the two columns the Feature 1 revalidation
// loop needs to detect dead queryids (calls_at_last_check) and throttle how
// often a hint is re-examined (last_revalidated_at).
const ddlQueryHintsRevalidate = `
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS calls_at_last_check BIGINT;
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS last_revalidated_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_query_hints_revalidate
    ON sage.query_hints (last_revalidated_at NULLS FIRST)
    WHERE status = 'active';
`

// v0.9.2 — Add last_detected_at to incidents for accurate outage duration.
const ddlIncidentsLastDetected = `
ALTER TABLE sage.incidents
    ADD COLUMN IF NOT EXISTS last_detected_at TIMESTAMPTZ;
UPDATE sage.incidents
    SET last_detected_at = detected_at
    WHERE last_detected_at IS NULL;
`

// v0.11 — absorb sage.schema_findings into sage.findings. Add optional
// rule_id and impact_score columns; lint runner starts writing to
// sage.findings with category = 'schema_lint:<rule_id>'. sage.schema_findings
// remains in place as a read-only historical record until a later release.
const ddlFindingsAbsorbsSchemaFindings = `
ALTER TABLE sage.findings
    ADD COLUMN IF NOT EXISTS rule_id      TEXT,
    ADD COLUMN IF NOT EXISTS impact_score REAL;
CREATE INDEX IF NOT EXISTS idx_findings_rule_id
    ON sage.findings (rule_id)
    WHERE rule_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_findings_schema_lint
    ON sage.findings (category, severity, last_seen DESC)
    WHERE category LIKE 'schema_lint:%' AND status = 'open';
`

// v0.11 — one-time backfill of sage.schema_findings rows into
// sage.findings. Idempotent via NOT EXISTS on (category, object_identifier).
// The source table is left intact so older clients / rollbacks still work;
// it is scheduled for DROP in a later release once all writers are gone.
const ddlFindingsBackfillFromSchemaFindings = `
INSERT INTO sage.findings (
    created_at, last_seen, occurrence_count,
    category, severity, object_type, object_identifier,
    title, detail, recommendation, recommended_sql,
    status, resolved_at, rule_id, impact_score
)
SELECT
    sf.first_seen,
    sf.last_seen,
    1,
    'schema_lint:' || sf.rule_id,
    sf.severity,
    CASE WHEN sf.index_name  <> '' THEN 'index'
         WHEN sf.column_name <> '' THEN 'column'
         ELSE 'table' END,
    sf.schema_name || '.' || sf.table_name
        || CASE WHEN sf.column_name <> ''
                THEN '/column=' || sf.column_name ELSE '' END
        || CASE WHEN sf.index_name  <> ''
                THEN '/index='  || sf.index_name  ELSE '' END,
    sf.description,
    jsonb_build_object(
        'thematic_category', sf.category,
        'schema_name',       sf.schema_name,
        'table_name',        sf.table_name,
        'column_name',       sf.column_name,
        'index_name',        sf.index_name,
        'impact',            sf.impact,
        'table_size',        sf.table_size,
        'query_count',       sf.query_count,
        'database_name',     sf.database_name
    ),
    sf.suggestion,
    sf.suggested_sql,
    CASE WHEN sf.resolved_at IS NOT NULL THEN 'resolved'
         WHEN sf.suppressed               THEN 'suppressed'
         ELSE 'open' END,
    sf.resolved_at,
    sf.rule_id,
    sf.impact_score
FROM sage.schema_findings sf
WHERE NOT EXISTS (
    SELECT 1 FROM sage.findings f
    WHERE f.category = 'schema_lint:' || sf.rule_id
      AND f.object_identifier = sf.schema_name || '.' || sf.table_name
          || CASE WHEN sf.column_name <> ''
                  THEN '/column=' || sf.column_name ELSE '' END
          || CASE WHEN sf.index_name  <> ''
                  THEN '/index='  || sf.index_name  ELSE '' END
);
`

// v0.10.1 — lint runner: add query_count column and partial unique index
// for active findings (needed by the upsert in lint.Runner).
const ddlSchemaFindingsLintRunner = `
ALTER TABLE sage.schema_findings
    ADD COLUMN IF NOT EXISTS query_count BIGINT;
ALTER TABLE sage.schema_findings
    ADD COLUMN IF NOT EXISTS impact TEXT NOT NULL DEFAULT '';
ALTER TABLE sage.schema_findings
    ALTER COLUMN column_name SET DEFAULT '',
    ALTER COLUMN index_name SET DEFAULT '';
UPDATE sage.schema_findings
    SET column_name = '' WHERE column_name IS NULL;
UPDATE sage.schema_findings
    SET index_name = '' WHERE index_name IS NULL;
ALTER TABLE sage.schema_findings
    ALTER COLUMN column_name SET NOT NULL,
    ALTER COLUMN index_name SET NOT NULL;
DROP INDEX IF EXISTS sage.idx_schema_findings_active_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_schema_findings_active_key
    ON sage.schema_findings (
        rule_id, schema_name, table_name, column_name, index_name
    )
    WHERE resolved_at IS NULL;
ALTER TABLE sage.schema_findings
    DROP CONSTRAINT IF EXISTS schema_findings_category_check;
ALTER TABLE sage.schema_findings
    ADD CONSTRAINT schema_findings_category_check
    CHECK (category IN (
        'safety', 'performance', 'correctness', 'convention',
        'indexing', 'data_integrity', 'schema_design', 'maintenance'
    ));
`

// v0.9 — Root Cause Analysis incidents table.
// v0.9.1 — expanded CHECK constraints for log-based RCA sources,
// info severity, and Tier 2 action_risk values.
const ddlIncidents = `
CREATE TABLE IF NOT EXISTS sage.incidents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_detected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    severity          TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    root_cause        TEXT NOT NULL,
    causal_chain      JSONB NOT NULL DEFAULT '[]',
    affected_objects  TEXT[] NOT NULL DEFAULT '{}',
    signal_ids        TEXT[] NOT NULL DEFAULT '{}',
    recommended_sql   TEXT,
    rollback_sql      TEXT,
    action_risk       TEXT CHECK (action_risk IN (
        'safe', 'moderate', 'high_risk', 'low', 'medium', 'high'
    ) OR action_risk IS NULL),
    source            TEXT NOT NULL CHECK (source IN (
        'deterministic', 'log_deterministic',
        'self_action', 'manual_review_required', 'llm',
        'schema_advisor', 'schema_lint', 'n_plus_one'
    )),
    confidence        NUMERIC(3,2) NOT NULL DEFAULT 1.0,
    related_findings  UUID[],
    resolved_at       TIMESTAMPTZ,
    database_name     TEXT,
    occurrence_count  INT NOT NULL DEFAULT 1,
    escalated_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_incidents_active ON sage.incidents (detected_at DESC) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_severity ON sage.incidents (severity) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_database ON sage.incidents (database_name, detected_at DESC);
`

// v0.9 — Storage growth forecasting history table.
const ddlSizeHistory = `
CREATE TABLE IF NOT EXISTS sage.size_history (
    id              BIGSERIAL PRIMARY KEY,
    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    metric_type     TEXT NOT NULL CHECK (metric_type IN ('database', 'table', 'wal_slot')),
    object_name     TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    dead_tuple_pct  NUMERIC(5,2),
    database_name   TEXT
);
CREATE INDEX IF NOT EXISTS idx_size_history_lookup
    ON sage.size_history (metric_type, object_name, collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_size_history_recent
    ON sage.size_history (collected_at DESC);
`

// v0.10 — Schema lint findings table.
const ddlSchemaFindings = `
CREATE TABLE IF NOT EXISTS sage.schema_findings (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id       TEXT NOT NULL,
    schema_name   TEXT NOT NULL,
    table_name    TEXT NOT NULL,
    column_name   TEXT NOT NULL DEFAULT '',
    index_name    TEXT NOT NULL DEFAULT '',
    severity      TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    category      TEXT NOT NULL CHECK (category IN (
        'safety', 'performance', 'correctness', 'convention',
        'indexing', 'data_integrity', 'schema_design', 'maintenance'
    )),
    description   TEXT NOT NULL,
    impact        TEXT NOT NULL DEFAULT '',
    suggestion    TEXT,
    suggested_sql TEXT,
    table_size    BIGINT,
    impact_score  REAL,
    first_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    suppressed    BOOLEAN NOT NULL DEFAULT false,
    database_name TEXT,
    query_count   BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_schema_findings_active_key
    ON sage.schema_findings (
        rule_id, schema_name, table_name, column_name, index_name
    )
    WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_schema_findings_severity
    ON sage.schema_findings (severity) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_schema_findings_table
    ON sage.schema_findings (schema_name, table_name);
`

// v0.9 — Natural Language EXPLAIN cache (replaces the old explain_cache for this purpose).
// The existing explain_cache stores auto_explain plan captures.
// This new table caches LLM-generated explain results with TTL.
const ddlExplainResults = `
CREATE TABLE IF NOT EXISTS sage.explain_results (
    query_hash      BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    plan_json       JSONB NOT NULL,
    explanation     JSONB NOT NULL,
    database_name   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (query_hash, database_name)
);
`

// v0.12 — Per-database fleet health time series. Written by the
// fleet manager every time a health score is recomputed (or at a
// periodic tick so charts stay populated even when health is flat).
// Used by GET /api/v1/fleet/health.
const ddlHealthHistory = `
CREATE TABLE IF NOT EXISTS sage.health_history (
    id            BIGSERIAL PRIMARY KEY,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    database_name TEXT NOT NULL,
    health_score  INTEGER NOT NULL,
    findings_open INTEGER NOT NULL DEFAULT 0,
    findings_critical INTEGER NOT NULL DEFAULT 0,
    findings_warning INTEGER NOT NULL DEFAULT 0,
    findings_info INTEGER NOT NULL DEFAULT 0,
    actions_total INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_health_history_lookup
    ON sage.health_history (database_name, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_health_history_recent
    ON sage.health_history (recorded_at DESC);
`
