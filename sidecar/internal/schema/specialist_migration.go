package schema

// The Postgres-specialist contract (roadmap phase 3). Idempotent.
//
// sage.specialist_requests is the audit trail of what external agents
// (named MCP-token identities: PagerDuty, Datadog, AWS DevOps Agent) asked
// pg_sage through the contract: which investigation they opened or
// attached to, which remediation they requested and the gate's verdict.
// It also queues the result posts adapters owe their system (outbound),
// claimed with a lease so two sidecars never post twice. Caller-supplied
// text (symptom) is stored as bounded data and is never an instruction.
const ddlSpecialist = `
CREATE TABLE IF NOT EXISTS sage.specialist_requests (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              text NOT NULL CHECK (kind IN ('open', 'attach', 'remediation')),
    token_id          text NOT NULL CHECK (length(token_id) BETWEEN 1 AND 128),
    identity_name     text NOT NULL DEFAULT '' CHECK (length(identity_name) <= 200),
    actor             text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    transport         text NOT NULL CHECK (transport IN ('http', 'mcp', 'pagerduty',
                          'webhook')),
    database_name     text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 128),
    investigation_id  text CHECK (length(investigation_id) <= 64),
    created           boolean NOT NULL DEFAULT false,
    match             text NOT NULL DEFAULT '' CHECK (length(match) <= 32),
    symptom           jsonb CHECK (symptom IS NULL OR (jsonb_typeof(symptom) = 'object'
                          AND pg_column_size(symptom) <= 16384)),
    time_window       jsonb CHECK (time_window IS NULL OR jsonb_typeof(time_window) = 'object'),
    external_ref      jsonb CHECK (external_ref IS NULL OR
                          jsonb_typeof(external_ref) = 'object'),
    remediation_id    text NOT NULL DEFAULT '' CHECK (length(remediation_id) <= 80),
    verdict           text NOT NULL DEFAULT '' CHECK (length(verdict) <= 40),
    reason            text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    outbound          text NOT NULL DEFAULT 'none' CHECK (outbound IN ('none', 'pending',
                          'delivered', 'failed')),
    outbound_attempts integer NOT NULL DEFAULT 0 CHECK (outbound_attempts >= 0),
    outbound_next_at  timestamptz NOT NULL DEFAULT now(),
    outbound_error    text NOT NULL DEFAULT '' CHECK (length(outbound_error) <= 2000),
    terminal_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS specialist_requests_token_live
    ON sage.specialist_requests (token_id, created_at DESC)
    WHERE kind = 'open' AND created AND terminal_at IS NULL;
CREATE INDEX IF NOT EXISTS specialist_requests_investigation
    ON sage.specialist_requests (investigation_id, token_id);
CREATE INDEX IF NOT EXISTS specialist_requests_outbound
    ON sage.specialist_requests (outbound_next_at) WHERE outbound = 'pending';
CREATE INDEX IF NOT EXISTS specialist_requests_external
    ON sage.specialist_requests ((external_ref->>'system'), (external_ref->>'id'))
    WHERE outbound <> 'none';
CREATE INDEX IF NOT EXISTS specialist_requests_created
    ON sage.specialist_requests (created_at DESC);
`
