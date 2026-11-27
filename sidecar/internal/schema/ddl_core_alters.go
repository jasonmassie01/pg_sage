package schema

// config-31: the column and backfill steps of the base tables, split out of
// bootstrap.go. Each is a pinned migration step (migration_steps.go and
// testdata/migration_checksums.txt): a released step never changes, so a new
// change is a new step appended in migration_steps.go.

const ddlActionLogApprovalCols = `
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_by INT;
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS justification TEXT;
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
    ADD COLUMN IF NOT EXISTS shadow_toil_minutes INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS action_log_id BIGINT REFERENCES sage.action_log(id);
CREATE INDEX IF NOT EXISTS idx_action_queue_identity
    ON sage.action_queue (identity_key, status)
    WHERE identity_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_action_queue_database_pending
    ON sage.action_queue (database_id, status, proposed_at DESC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_action_queue_expiry
    ON sage.action_queue (expires_at)
    WHERE status IN ('pending', 'failed');
`

const ddlUsersOAuth = `
ALTER TABLE sage.users
    ADD COLUMN IF NOT EXISTS oauth_provider TEXT DEFAULT '';
-- Checked first, so a second boot takes no lock on sage.users (config-04).
DO $users_password$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_attribute
                WHERE attrelid = 'sage.users'::regclass AND attname = 'password'
                  AND attnotnull) THEN
        ALTER TABLE sage.users ALTER COLUMN password DROP NOT NULL;
    END IF;
END $users_password$;
-- OIDC identities are keyed on issuer+subject (G6-B04 / SURF-02).
ALTER TABLE sage.users
    ADD COLUMN IF NOT EXISTS oauth_issuer TEXT;
ALTER TABLE sage.users
    ADD COLUMN IF NOT EXISTS oauth_subject TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oauth_identity
    ON sage.users (oauth_issuer, oauth_subject)
    WHERE oauth_issuer IS NOT NULL;
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
// The backfill runs when the column is added: every writer sets it, so a
// re-run must not look for NULLs again (it read all of sage.incidents on
// every startup; performance gate, v1.8.3).
const ddlIncidentsLastDetected = `
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'sage.incidents'::regclass
                      AND attname = 'last_detected_at' AND NOT attisdropped) THEN
        ALTER TABLE sage.incidents ADD COLUMN last_detected_at TIMESTAMPTZ;
        UPDATE sage.incidents SET last_detected_at = detected_at;
    END IF;
END $$;
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
const ddlFleetScaleIndexes = `
CREATE INDEX IF NOT EXISTS idx_sessions_expires
    ON sage.sessions (expires_at);
CREATE INDEX IF NOT EXISTS idx_sessions_user
    ON sage.sessions (user_id);
CREATE INDEX IF NOT EXISTS idx_notification_rules_event_enabled
    ON sage.notification_rules (event, min_severity)
    WHERE enabled = true;
CREATE INDEX IF NOT EXISTS idx_notification_log_sent_at
    ON sage.notification_log (sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_notification_log_channel_sent
    ON sage.notification_log (channel_id, sent_at DESC)
    WHERE channel_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_action_queue_database_pending
    ON sage.action_queue (database_id, status, proposed_at DESC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_action_queue_expiry
    ON sage.action_queue (expires_at)
    WHERE status IN ('pending', 'failed');
CREATE INDEX IF NOT EXISTS idx_action_log_outcome_time
    ON sage.action_log (outcome, executed_at DESC);
`

const ddlSchemaFindingsLintRunner = `
ALTER TABLE sage.schema_findings
    ADD COLUMN IF NOT EXISTS query_count BIGINT;
ALTER TABLE sage.schema_findings
    ADD COLUMN IF NOT EXISTS impact TEXT NOT NULL DEFAULT '';
-- Each reshape checks the catalog first, so a second boot takes no lock on
-- sage.schema_findings (config-04). The NOT NULL columns get '' defaults,
-- then the active-key index is rebuilt over them (or when it is invalid).
DO $lint_runner$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_attribute a
                 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
                WHERE a.attrelid = 'sage.schema_findings'::regclass
                  AND a.attname IN ('column_name', 'index_name') AND NOT a.attisdropped
                  AND (NOT a.attnotnull OR d.adbin IS NULL
                       OR pg_get_expr(d.adbin, d.adrelid) <> '''''::text')) THEN
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
    ELSIF EXISTS (SELECT 1 FROM pg_index
                   WHERE indexrelid = to_regclass('sage.idx_schema_findings_active_key')
                     AND NOT indisvalid) THEN
        DROP INDEX sage.idx_schema_findings_active_key;
    END IF;
END $lint_runner$;
CREATE UNIQUE INDEX IF NOT EXISTS idx_schema_findings_active_key
    ON sage.schema_findings (
        rule_id, schema_name, table_name, column_name, index_name
    )
    WHERE resolved_at IS NULL;
-- The category check is replaced only when it does not list exactly these
-- eight values.
DO $lint_category$
DECLARE
    def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint
     WHERE conrelid = 'sage.schema_findings'::regclass
       AND conname = 'schema_findings_category_check';
    IF def IS NOT NULL AND def LIKE ALL (ARRAY['%''safety''%', '%''performance''%',
           '%''correctness''%', '%''convention''%', '%''indexing''%',
           '%''data_integrity''%', '%''schema_design''%', '%''maintenance''%'])
       AND (SELECT count(*) FROM regexp_matches(def, '''[^'']*''', 'g')) = 8 THEN
        RETURN;
    END IF;
    ALTER TABLE sage.schema_findings
        DROP CONSTRAINT IF EXISTS schema_findings_category_check;
    ALTER TABLE sage.schema_findings
        ADD CONSTRAINT schema_findings_category_check
        CHECK (category IN (
            'safety', 'performance', 'correctness', 'convention',
            'indexing', 'data_integrity', 'schema_design', 'maintenance'
        ));
END $lint_category$;
`
