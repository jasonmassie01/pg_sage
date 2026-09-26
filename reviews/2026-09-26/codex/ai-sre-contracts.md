# Sage Incident Investigator: R1 implementation contracts

Proposed appendix, 2026-09-26. This clarifies `ai-sre-spec.md`; no implementation was changed.
Four-table DDL was accepted by disposable PostgreSQL 17 in BEGIN/ROLLBACK; exit 0.
Evidence: `evidence/sre-proposed-ddl-result.txt`. JSON parsed; YAML/defaults checked.
Behavior and the 35 acceptance checks remain unexecuted. This is not the complete migration:
hypotheses, steps, events and recovery tables still require the companion spec's migrations.

## 1. Three distinct clocks and identities

**Investigation job:** at most 120 seconds of active wall time, 12 probes, two model turns,
16,000 total input tokens and 4,000 total output tokens across those turns. Deadlines include
tool/model waits and parsing. Pausing cannot replenish these counters; resuming uses only
remaining budget. Queue residence is bounded separately by a proposed 10-minute expiry.

**Recovery job:** independently scheduled after a reported intervention. At least three fresh
samples spanning two minutes; deadline 30 minutes after intervention by default. Recovery
does not consume the investigation's expired 120-second clock and uses no LLM. It shares
target concurrency/load safety and has its own ceiling of 30 bounded probes, one per minute.
An investigation can conclude while its recovery job remains observing. Scheduling delays
do not extend either job's original deadline. Unknown recovery is never success.

**Daily budget:** UTC calendar date, including year, persisted independently of process life.
Model reservation holds survive pause, restart and midnight. Reconciliation charges the
original reservation day; a retry on another day is a new reservation and retains old usage.

Generate deployment/database UUIDs in Go, once, before use; never derive them from aliases,
IP addresses, names or DSNs. The metadata deployment stores its UUID durably. In config-fleet
and standalone mode persist a stable opaque connection-entry key and its UUID binding; do
not create fake credential-bearing `sage.databases` records merely to satisfy a foreign key.
Metadata-fleet binds its existing integer record ID. Alias changes preserve the binding.
Target replacement requires explicit rebinding/new identity or epoch, never silent reuse.

`cluster_epoch` identifies an observed server incarnation/recovery boundary, not the alias.
If provider identity is unavailable, mark identity strength `configured`, retain that
limitation, and prohibit stronger cross-host correlation. UUIDs scope data; they do not prove
the actual target is still the same server. Every probe rechecks the available target identity.

## 2. Four foundational tables

Run under existing bootstrap migration locking with a metadata-owner role. Target reader
credentials cannot run these statements. UUID values are supplied by Go; no extra extension
is needed. Existing `sage.databases` must already exist for its optional integer reference.
SQL below is a first-time versioned migration; retries use the migration ledger, not blind
`IF NOT EXISTS` that could hide an incompatible pre-existing table.

```sql
CREATE TABLE sage.sre_database_bindings (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    runtime_key text NOT NULL CHECK (length(runtime_key) BETWEEN 1 AND 128),
    legacy_database_id integer REFERENCES sage.databases(id) ON DELETE RESTRICT,
    identity_strength text NOT NULL
        CHECK (identity_strength IN ('configured', 'provider', 'cluster')),
    cluster_epoch text NOT NULL CHECK (length(cluster_epoch) BETWEEN 1 AND 128),
    identity_hash bytea NOT NULL CHECK (octet_length(identity_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id),
    UNIQUE (deployment_id, runtime_key)
);
CREATE UNIQUE INDEX sre_binding_legacy_unique
    ON sage.sre_database_bindings (deployment_id, legacy_database_id)
    WHERE legacy_database_id IS NOT NULL;

CREATE TABLE sage.sre_investigations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    source_case_id text NOT NULL,
    source_incident_id text,
    trigger_fingerprint bytea NOT NULL
        CHECK (octet_length(trigger_fingerprint) = 32),
    state text NOT NULL CHECK (state IN (
        'queued', 'collecting', 'evaluating', 'needs_evidence', 'concluded',
        'inconclusive', 'paused', 'cancelled', 'expired', 'failed')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    fence_token bigint NOT NULL DEFAULT 0 CHECK (fence_token >= 0),
    lease_owner uuid,
    lease_until timestamptz,
    active_ms bigint NOT NULL DEFAULT 0 CHECK (active_ms BETWEEN 0 AND 120000),
    segment_deadline timestamptz,
    probe_count integer NOT NULL DEFAULT 0 CHECK (probe_count BETWEEN 0 AND 12),
    model_turns integer NOT NULL DEFAULT 0 CHECK (model_turns BETWEEN 0 AND 2),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    failure_code text,
    summary jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (deployment_id, database_id, id),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK (expires_at > created_at),
    CHECK (jsonb_typeof(summary) = 'object'),
    CHECK (octet_length(summary::text) <= 65536)
);
CREATE UNIQUE INDEX sre_one_live_trigger
    ON sage.sre_investigations
        (deployment_id, database_id, trigger_fingerprint)
    WHERE state IN ('queued', 'collecting', 'evaluating', 'needs_evidence', 'paused');
CREATE INDEX sre_investigation_queue
    ON sage.sre_investigations (deployment_id, database_id, state, updated_at);

CREATE TABLE sage.sre_evidence (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    source_kind text NOT NULL,
    probe_version text NOT NULL,
    observed_at timestamptz,
    collected_at timestamptz NOT NULL,
    valid_until timestamptz,
    interval_start timestamptz,
    interval_end timestamptz,
    reset_epoch text,
    capability_state text NOT NULL CHECK (capability_state IN (
        'available', 'unsupported', 'permission_denied', 'unreachable', 'unknown')),
    reason_code text,
    payload_version integer NOT NULL CHECK (payload_version > 0),
    classification text NOT NULL CHECK (classification IN ('redacted', 'restricted')),
    payload jsonb NOT NULL,
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (octet_length(payload::text) <= 262144),
    CHECK ((interval_start IS NULL) = (interval_end IS NULL)),
    CHECK (interval_end IS NULL OR interval_end > interval_start),
    CHECK (valid_until IS NULL OR observed_at IS NOT NULL),
    CHECK (valid_until IS NULL OR valid_until >= observed_at),
    CHECK (capability_state = 'available' OR reason_code IS NOT NULL)
);
CREATE INDEX sre_evidence_time
    ON sage.sre_evidence
        (deployment_id, database_id, investigation_id, observed_at);

CREATE TABLE sage.sre_budget_reservations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    investigation_id uuid,
    utc_day date NOT NULL,
    caller_kind text NOT NULL,
    request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 160),
    state text NOT NULL CHECK (state IN
        ('reserved', 'inflight', 'settled', 'unknown', 'cancelled')),
    input_reserved bigint NOT NULL CHECK (input_reserved >= 0),
    output_reserved bigint NOT NULL CHECK (output_reserved >= 0),
    input_used bigint CHECK (input_used >= 0),
    output_used bigint CHECK (output_used >= 0),
    provider_request_id text,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    settled_at timestamptz,
    PRIMARY KEY (deployment_id, database_id, id),
    UNIQUE (deployment_id, database_id, request_key),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK ((input_used IS NULL) = (output_used IS NULL)),
    CHECK ((state = 'settled') = (input_used IS NOT NULL))
);
CREATE INDEX sre_budget_day
    ON sage.sre_budget_reservations (deployment_id, utc_day, database_id);
```

Non-SRE callers have NULL investigation_id but the same durable daily budget admission.
The application service role receives no UPDATE/DELETE on evidence; a separate retention
role follows pin/tombstone rules from the full spec. Cross-scope joins include all three
parent key columns. `source_case_id` is a projected case identity, so validate its scoped
existence in the store rather than pretending there is a stable case-table foreign key.
Limits in this R1 migration are hard ceilings; configuration can tighten, not raise, them.

## 3. Go boundaries (signatures, not an implementation)

```go
package sre

import (
    "context"
    "encoding/json"
    "time"
)

type UUID string // Parse canonical UUID at every external/config boundary.
type Scope struct { DeploymentID, DatabaseID UUID }
type Capability string
type State string
type ProbeID string
type BackendIdentity struct {
    PID int32
    BackendStart time.Time
    DatabaseOID, RoleOID uint32
    QueryID *int64
}
type ProbeArgs struct {
    Backend *BackendIdentity
    Start, End *time.Time
}
type ProbeSpec struct {
    ID ProbeID
    Version string
    Capabilities []Capability
    StatementTimeout, LockTimeout time.Duration
    MaxRows, MaxBytes uint32
}
type Observation struct {
    Scope Scope
    ObservedAt, ValidUntil *time.Time
    CollectedAt time.Time
    Capability Capability
    ReasonCode string
    SchemaVersion uint32
    Payload json.RawMessage // Registry validates its probe-specific schema.
}
type Lease struct {
    Scope Scope
    InvestigationID, WorkerID UUID
    Version, Fence uint64
    Until, SegmentDeadline time.Time
}
type StartRequest struct {
    Scope Scope
    CaseID, TriggerKind, IdempotencyKey string
}
type Investigation struct {
    Scope Scope
    ID UUID
    State State
    Version uint64
    Remaining time.Duration
}
type TokenRequest struct {
    Input, Output uint64
    RequestKey string
}
type Reservation struct { ID UUID; Version uint64 }
type Usage struct { Input, Output uint64; Known bool }
type StepResult struct {
    StepID UUID
    Observations []Observation
    NextState State
    ErrorCode string
}
type Probe interface {
    Spec() ProbeSpec
    Run(context.Context, Scope, ProbeArgs) ([]Observation, error)
}
type Store interface {
    Create(context.Context, StartRequest) (Investigation, error)
    Claim(context.Context, Scope, UUID, UUID) (Lease, error)
    ReserveModel(context.Context, Lease, TokenRequest) (Reservation, error)
    MarkDispatched(context.Context, Scope, Reservation) (Reservation, error)
    SettleModel(context.Context, Scope, Reservation, Usage) error
    CommitStep(context.Context, Lease, StepResult) (Investigation, error)
    Stop(context.Context, Scope, UUID, uint64) error
}
type Coordinator interface {
    Start(context.Context, StartRequest) (Investigation, error)
    RunClaimed(context.Context, Lease) error
}
```

Probe implementations receive a privately wired catalog reader, not caller-supplied SQL,
credentials or arbitrary URLs. Return typed errors with codes and retry classification.
Unknown numeric fields are nullable with a reason; zero is reserved for an observed zero.
For example, `disk_free_bytes:null, reason_code:"provider_metric_unavailable"` is valid.
`observed_at:null` cannot be accepted as fresh evidence. JSON schema discriminates each
probe payload; RawMessage is not permission to persist unvalidated model text.

## 4. API example: investigation ends without a supported cause

Request: `POST /api/v1/databases/{database_uuid}/investigations`, with existing session,
operator role, CSRF protection and an Idempotency-Key header. Header/body keys must match.

```json
{
  "source_case_id": "case:connection-pressure:opaque-key",
  "trigger_kind": "connection_pressure",
  "requested_budget_profile": "r1_default",
  "idempotency_key": "operator-request-042"
}
```

Return 202 with scoped investigation ID and `state:"queued"`. A later GET may return:

```json
{
  "id": "40bfc229-bd01-4ec4-a7dc-c7dbf323b933",
  "database_id": "cf1168de-7f21-4931-9b86-b5caac8a0b24",
  "version": 7,
  "state": "inconclusive",
  "incident_state": "open",
  "reason_code": "insufficient_evidence",
  "observed": [{
    "text": "Backend pressure was observed",
    "evidence_ids": ["81f6f309-a936-47f5-a325-1d62c3530941"]
  }],
  "hypotheses": [{
    "label": "Application pool fan-out",
    "status": "unproven",
    "supporting_ids": ["81f6f309-a936-47f5-a325-1d62c3530941"],
    "contradicting_ids": [],
    "missing_checks": ["pool_client_queue", "application_replica_count"]
  }],
  "missing_evidence": [{
    "source": "pool_metrics", "capability_state": "unsupported",
    "reason_code": "connector_not_configured", "value": null
  }],
  "evidence": [{
    "id": "81f6f309-a936-47f5-a325-1d62c3530941",
    "observed_at": "2026-09-26T19:02:00Z",
    "collected_at": "2026-09-26T19:02:01Z",
    "valid_until": "2026-09-26T19:02:30Z",
    "capability_state": "available", "payload_version": 1,
    "payload": {"backend_count": 95, "configured_max_connections": 100}
  }],
  "customer_impact": {"state": "unknown", "slo_burn_rate": null},
  "actions": [],
  "recovery": {"state": "not_requested", "reason": "no_intervention_recorded"},
  "budget": {"active_ms": 24000, "probes": 3, "model_turns": 1}
}
```

The evidence supports pressure at its observation time, not a current healthy/unhealthy claim
once expired. A later GET retains its timestamp and marks current freshness separately.
No-data is HTTP 200 domain information, not an exception or healthy status. Infrastructure
failure still gets a canonical error and appropriate HTTP status, without leaking credentials.

## 5. Validated configuration contract

```yaml
sre:
  enabled: false
  automatic_start: false
  mode: read_only
  queue:
    max_pending: 100
    expiry: 10m
  investigation:
    max_active_time: 120s
    max_probes: 12
    max_model_turns: 2
    max_input_tokens: 16000
    max_output_tokens: 4000
  probe:
    per_database_concurrency: 1
    sidecar_concurrency: 4
    statement_timeout: 500ms
    lock_timeout: 100ms
    max_rows: 500
    max_bytes: 262144
  recovery:
    minimum_samples: 3
    minimum_span: 2m
    interval: 1m
    deadline: 30m
    max_probes: 30
  budget:
    inherit_daily_limits: true
    accounting: durable
    day_boundary: UTC
  target:
    require_read_only_role: true
    require_direct_or_session_connection: true
```

Reject negative/zero budgets, unknown enum values and unknown keys. Omitted fields use the
documented defaults; explicit zero is not omission. Require lock_timeout < statement_timeout;
per-database concurrency <= sidecar concurrency; minimum_span <= deadline; minimum_samples
<= max_probes; and enough scheduled interval slots for minimum_samples before the deadline.
The limits above are ceilings for R1; positive smaller values are allowed if internally valid.
If configured daily limits are unlimited/missing, R1 model use requires a finite explicit
database AND deployment allocation before enablement; deterministic investigations still work.
Changing scope/credentials requires reconnect; lowering limits applies before the next step;
disabling cancels active work. Increasing a cap cannot retroactively refill an investigation.

## 6. Transactions, leases and reservations

Creation: expire stale live triggers first; authorize scope, lock binding, check source case and
idempotency request, and INSERT investigation. On live-trigger unique conflict return that
scoped live investigation. A stable event/request mapping in `sre_events` preserves repeated
idempotency requests even after conclusion; a genuinely new trigger gets a new request key.

Claim: SELECT eligible row FOR UPDATE SKIP LOCKED; reject expired/terminal/budget-exhausted
work; increment fence_token and version; set worker, lease and segment_deadline using the
metadata database clock and remaining active_ms. On an orphaned lease, conservatively charge
the elapsed reserved segment; never give time back merely because the worker died. Heartbeat
extends the lease only up to segment_deadline. A pause charges elapsed time before clearing
lease fields. CommitStep checks scope, worker, version, fence and nonexpired deadline in the
same UPDATE/transaction as append-only step/evidence/event insertion. Zero updated rows is
`lease_lost`, not success. A stale worker may release resources but cannot commit its result.

ReserveModel transaction, before external provider I/O:
1. Lock the deployment/UTC-day budget using a transaction advisory lock; then lock the
   investigation. All callers use this order. Database daily checks occur under that same
   deployment lock, so concurrent databases cannot overspend the shared pool.
2. Sum ledger charges: settled rows use actual tokens; reserved/inflight/unknown rows use
   their full allowance; cancelled rows use zero. Include all callers, not just SRE rows.
3. Check deployment/day, database/day, investigation aggregate input/output, turn count,
   remaining active time and request-key uniqueness. Reserve conservatively for the full
   serialized prompt plus capped completion/reasoning tokens. Reject an adapter that cannot
   count input conservatively or bound its total completion usage under the selected model.
4. Insert reservation and step intent; increment model_turns before dispatch. Commit. Mark
   inflight durably before sending. A repair/retry consumes another turn/reservation and
   must fit remaining aggregate budgets. Existing hidden transport retries must be bounded
   or disabled so provider calls cannot escape accounting.

Settlement is a versioned, idempotent transaction. Known provider usage settles actuals;
unknown timeout/crash after dispatch retains the full hold as `unknown`. Never automatically
release an uncertain billed call. Proven pre-dispatch cancellation may release its allowance.
If provider usage exceeds the reservation, record actual usage, flag the contract violation,
and disable further model dispatch; do not truncate billing evidence to fit a CHECK constraint.

Existing `llm/client_controls.go` uses in-memory counters. Its checks can remain an additional
guard, but every caller sharing a daily allowance must use this durable admission wrapper.
Do not claim a shared crash-safe limit until non-SRE callers are migrated. Report durable
actuals once; do not add the in-memory reservation as a second charge for the same request.

Recovery enqueue commits an attributed intervention and a `sre_recovery_checks` row together.
Its worker leases/fences independently, has no model dispatch path, and uses the shared probe
semaphore/circuit breaker. It ends recovered/not_recovered/inconclusive/expired within its
own deadline. No intervention record means no automatically invented recovery job.

## 7. Migration and target-safety gates

Deploy additive schema first with feature disabled. Backfill UUID bindings in an advisory-
locked transaction, preserving integer IDs and all old API aliases. Store the mapping before
starting workers. Restart must read the same mapping. Resolve ambiguous target identity by
blocking that target; never merge by display name. Migrate existing incident references only
when their database association is proven. No synthetic history or fabricated evidence.

Install remaining steps/events/hypotheses/recovery migrations from the full spec before R1
enablement. Verify foreign keys, indexes, enum checks and role grants against real PostgreSQL
14–18 fixtures; PostgreSQL 17 accepted the declarations, but behavior remains unverified.
Roll back failed migrations; application rollback disables workers and retains evidence tables.
Do not drop audit data as an automatic software rollback action.

Reader role must lack application-table writes and dangerous function grants; use fixed
catalog templates, schema-qualified catalog references, empty/fixed search_path, read-only
transactions and bounded timeouts. Direct/session connections allow reliable connection
identity; unsupported pooling is rejected. A read-only transaction still permits expensive
reads and does not ban all side-effecting functions, so positive probe allowlisting remains
mandatory. Unknown privileges, identity, capacity or freshness withhold corresponding claims.
Metadata outage may produce redacted local status; it cannot authorize target actions.
