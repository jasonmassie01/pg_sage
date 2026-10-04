package schema

import "strings"

// Roadmap 1.4 (shadow mode, 2026-10-04). Below a class's earned level
// pg_sage records every action it would have taken as a shadow decision
// in the monitored database (sage.shadow_decision): the exact SQL and
// rollback, the prediction, the evidence and the gate's verdict had the
// class been trusted. A scorer later scores it from what happened
// (operator decision, the same change applied, HypoPG what-if) and the
// trust ledger copies the scores it counts into the control database
// (sage.trust_shadow_evidence, one row per shadow decision), read by the
// reconciler from its own cursor (trust_ledger_state.shadow_cursor).
//
// At most one decision per (database, fingerprint) is pending; scored
// rows are history. Additive and idempotent: tables are created when
// missing, the cursor column only when the catalog lacks it (ALTER TABLE
// would lock the table on every start), indexes through the checked loop.
const ddlShadowModeTables = `
CREATE TABLE IF NOT EXISTS sage.shadow_decision (
    id                  bigserial PRIMARY KEY,
    database_id         integer,
    database_name       text NOT NULL DEFAULT '' CHECK (length(database_name) <= 200),
    fingerprint         text NOT NULL CHECK (length(fingerprint) BETWEEN 1 AND 128),
    family              text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class        text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    finding_id          bigint,
    recommendation_id   bigint,
    title               text NOT NULL DEFAULT '',
    object_identifier   text NOT NULL DEFAULT '',
    sql                 text NOT NULL CHECK (length(sql) > 0),
    rollback_sql        text NOT NULL DEFAULT '',
    shape               text NOT NULL,
    prediction          jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence            jsonb NOT NULL DEFAULT '{}'::jsonb,
    gate_verdict        text NOT NULL,
    gate_reason         text NOT NULL DEFAULT '',
    trusted_verdict     text NOT NULL,
    trusted_reason      text NOT NULL DEFAULT '',
    trusted_detail      text NOT NULL DEFAULT '',
    granted_level       smallint NOT NULL CHECK (granted_level BETWEEN 0 AND 4),
    decision_id         bigint,
    status              text NOT NULL DEFAULT 'pending',
    score               text,
    score_source        text,
    counted             boolean NOT NULL DEFAULT false,
    score_reason        text NOT NULL DEFAULT '',
    score_detail        jsonb NOT NULL DEFAULT '{}'::jsonb,
    ref_action_log_id   bigint,
    ref_queue_id        bigint,
    applied_after       timestamptz,
    applied_detected_at timestamptz,
    seen_count          integer NOT NULL DEFAULT 1,
    recorded_at         timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    scored_at           timestamptz,
    CONSTRAINT shadow_decision_gate_verdict CHECK (gate_verdict IN
        ('observe_only', 'queue_approval', 'blocked')),
    CONSTRAINT shadow_decision_trusted_verdict CHECK (trusted_verdict IN
        ('execute', 'queue_approval', 'observe_only', 'blocked')),
    CONSTRAINT shadow_decision_status CHECK (status IN ('pending', 'scored')),
    CONSTRAINT shadow_decision_score CHECK (score IS NULL OR score IN
        ('correct', 'incorrect', 'neutral', 'unscored')),
    CONSTRAINT shadow_decision_source CHECK (score_source IS NULL OR score_source IN
        ('operator', 'applied', 'external', 'hypopg', 'none')),
    CONSTRAINT shadow_decision_scored CHECK (
        (status = 'pending' AND score IS NULL AND score_source IS NULL AND NOT counted) OR
        (status = 'scored' AND score IS NOT NULL AND score_source IS NOT NULL)),
    CONSTRAINT shadow_decision_counted CHECK (NOT counted OR
        (score IN ('correct', 'incorrect', 'neutral')
         AND score_source IN ('external', 'hypopg')))
);

CREATE TABLE IF NOT EXISTS sage.trust_shadow_evidence (
    id            bigserial PRIMARY KEY,
    deployment_id uuid NOT NULL,
    database_name text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    shadow_id     bigint NOT NULL,
    fingerprint   text NOT NULL CHECK (length(fingerprint) BETWEEN 1 AND 128),
    family        text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class  text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    score         text NOT NULL CHECK (score IN ('correct', 'incorrect', 'neutral')),
    source        text NOT NULL CHECK (source IN ('external', 'hypopg')),
    observed_at   timestamptz NOT NULL,
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (deployment_id, database_name, shadow_id)
);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                   WHERE attrelid = 'sage.trust_ledger_state'::regclass
                     AND attname = 'shadow_cursor' AND NOT attisdropped) THEN
        ALTER TABLE sage.trust_ledger_state ADD COLUMN IF NOT EXISTS shadow_cursor timestamptz;
    END IF;
END $$;
`

// shadowModeIndexes: one pending decision per (database, fingerprint);
// the dedupe and history reads by fingerprint; the scorer's pending
// decisions, oldest first; the reconciler's read of
// counted scores by score time; the API's per-class newest-first list;
// and the ledger's per-pair read.
var shadowModeIndexes = []ledgerIndex{
	{"shadow_decision_one_pending", "shadow_decision", "UNIQUE INDEX %I ON " +
		"sage.shadow_decision (COALESCE(database_id, 0), fingerprint) " +
		"WHERE status = 'pending'"},
	{"shadow_decision_fingerprint_recent", "shadow_decision", "INDEX %I ON " +
		"sage.shadow_decision (fingerprint, recorded_at DESC)"},
	{"shadow_decision_pending", "shadow_decision", "INDEX %I ON " +
		"sage.shadow_decision (recorded_at) WHERE status = 'pending'"},
	{"shadow_decision_scored", "shadow_decision", "INDEX %I ON " +
		"sage.shadow_decision (scored_at) WHERE status = 'scored' AND counted"},
	{"shadow_decision_class_recent", "shadow_decision", "INDEX %I ON " +
		"sage.shadow_decision (action_class, recorded_at DESC)"},
	{"trust_shadow_evidence_pair", "trust_shadow_evidence", "INDEX %I ON " +
		"sage.trust_shadow_evidence (deployment_id, database_name, family, action_class)"},
}

// ddlShadowMode is the migration: tables and cursor, then the checked
// index loop (catalog first, INVALID rebuilt).
func ddlShadowMode() string {
	values := make([]string, 0, len(shadowModeIndexes))
	for _, index := range shadowModeIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return ddlShadowModeTables + "DO $$\nDECLARE spec record;\nBEGIN" + loop + "\nEND $$;"
}
