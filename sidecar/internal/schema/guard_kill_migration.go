package schema

// Agent governance G1 kill switch and freeze (AGENTDB-SPEC §6.10, §7).
// All idempotent.
//
// Control database:
//   - sage.guard_kills: one row per kill, with its durable report.
//   - sage.guard_freezes: freeze flags on a principal, a database or the
//     whole fleet (D1); at most one open row per (scope, target). kill_id
//     marks a freeze made by a kill, whose unfreeze needs two people.
//   - sage.guard_unfreeze_requests: the first admin's request to lift a
//     freeze, waiting for a second admin (§6.11); one pending per freeze.
//   - sage.guard_inflight: backends running an agent's request (the
//     executor's applies, the broker's statements) that a kill cancels.
//
// Every monitored database: an index for the kill's cancel of open
// approvals by principal (status 'cancelled_kill').
const ddlGuardKill = `
CREATE TABLE IF NOT EXISTS sage.guard_kills (
    id           bigserial PRIMARY KEY,
    scope        text NOT NULL CHECK (scope IN ('all', 'principal', 'database')),
    target       text NOT NULL DEFAULT '' CHECK (length(target) <= 200),
    reason       text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    requested_by text NOT NULL CHECK (length(requested_by) BETWEEN 1 AND 200),
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    report       jsonb
);
CREATE TABLE IF NOT EXISTS sage.guard_freezes (
    id         bigserial PRIMARY KEY,
    scope      text NOT NULL CHECK (scope IN ('principal', 'database', 'fleet')),
    target     text NOT NULL DEFAULT '' CHECK (length(target) <= 200),
    kill_id    bigint REFERENCES sage.guard_kills(id),
    reason     text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    set_by     text NOT NULL CHECK (length(set_by) BETWEEN 1 AND 200),
    set_at     timestamptz NOT NULL DEFAULT now(),
    cleared_at timestamptz,
    cleared_by text
);
CREATE UNIQUE INDEX IF NOT EXISTS guard_freezes_open
    ON sage.guard_freezes (scope, target) WHERE cleared_at IS NULL;
CREATE INDEX IF NOT EXISTS guard_freezes_kill ON sage.guard_freezes (kill_id);
CREATE TABLE IF NOT EXISTS sage.guard_unfreeze_requests (
    id                bigserial PRIMARY KEY,
    scope             text NOT NULL CHECK (scope IN ('principal', 'database', 'fleet')),
    target            text NOT NULL DEFAULT '' CHECK (length(target) <= 200),
    freeze_id         bigint NOT NULL REFERENCES sage.guard_freezes(id),
    requested_by      text NOT NULL CHECK (length(requested_by) BETWEEN 1 AND 200),
    requested_by_user integer NOT NULL,
    reason            text NOT NULL CHECK (length(reason) <= 2000),
    requested_at      timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    approved_by       text,
    approved_by_user  integer,
    decided_at        timestamptz,
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'applied', 'expired', 'superseded'))
);
CREATE UNIQUE INDEX IF NOT EXISTS guard_unfreeze_requests_pending
    ON sage.guard_unfreeze_requests (freeze_id) WHERE status = 'pending';
CREATE TABLE IF NOT EXISTS sage.guard_inflight (
    principal_id text NOT NULL CHECK (principal_id ~ '^agp_[a-z2-7]{20}$'),
    database_id  uuid NOT NULL,
    backend_pid  integer NOT NULL CHECK (backend_pid > 0),
    action_id    bigint,
    started_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_id, backend_pid)
);
CREATE INDEX IF NOT EXISTS guard_inflight_principal
    ON sage.guard_inflight (principal_id);
CREATE INDEX IF NOT EXISTS action_queue_principal_open
    ON sage.action_queue (principal_id)
    WHERE principal_id IS NOT NULL AND status IN ('pending', 'approved');
`
