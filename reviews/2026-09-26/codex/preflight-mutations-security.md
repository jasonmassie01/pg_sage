# Focused pre-remediation validation: mutations, authorization and AgentDB

Date: 2026-09-26. Frozen source: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Validation checkout: `../validation-surface-repo`. Production source was not changed.

## Outcome

**Ready to prioritize remediation; not a clean-release or safety certification.**
Real mounted HTTP tests reproduced the MCP viewer authorization bypass, OIDC account
linking through an unverified email, forged Terraform approval attribution, and an
AgentDB resource orphan on rejected input. The tests retain their failing assertions.
Positive controls demonstrate that the real authentication/role mechanisms are active.

The mutation inventory covers the enumerated source sink vocabulary, with reviewed
family classifications and explicit exclusions. It is **not exhaustive semantic write
closure**: indirect calls, runtime-generated SQL, dynamically selected commands, and
external provider implementations still require targeted tracing and runtime probes.
No cloud resource, external clone, live deployment, or production database was used.

## Evidence and repeatability

- [Final staged test patch](evidence/preflight-surface-tests-final.patch)
- [Initial staged test patch](evidence/preflight-surface-tests-staged.patch)
- [Final readable test output](evidence/preflight-surface-tests-run3.txt)
- [Final structured test output](evidence/preflight-surface-tests-run3.jsonl)
- [CSV mutation inventory](evidence/preflight-surface-mutation-inventory.csv)
- [TSV mutation inventory](evidence/preflight-surface-mutation-inventory.tsv)
- [Inventory counts](evidence/preflight-surface-inventory-summary.json)
- [AST sink extraction](evidence/preflight-surface-inventory.go)
- [Classification/coverage generator](evidence/preflight-surface-inventory.py)
- [Instrumented coverage profile](evidence/preflight-surface-coverage.out)
- [Coverage by package](evidence/preflight-surface-coverage.json)
- [Container identity and binding](evidence/preflight-surface-container.json)
- [Skip scan](evidence/preflight-surface-skip-scan.txt)

All tests were staged before execution. The original and restaged timestamps are saved.
The first invocation failed during argument parsing: PowerShell treated an unquoted
coverage-profile path as a package. The first executed run found a test fixture error:
policy proposals live in `sage.policy` with `status='proposed'`, not a separate table.
The original output is retained. That lookup was corrected. A live-authorization negative
test initially rejected a nonexistent deployment with 404; its fixture was strengthened
to create an actual **plan only**, then require rejection without a resource ID or state
change. Neither change weakened a safety assertion or changed production behavior.

The final run used PostgreSQL17/pgvector in the uniquely owned container
`pgsage-validate-surface-20260926`, bound only to `127.0.0.1:55441`.
`internal/testdb` created and dropped its own per-package database. The new tests fail,
rather than skip, when the endpoint is not the owned local port or the database is absent.
The local mock OIDC issuer listens on a disposable loopback HTTP server. The application
router uses a real TLS test server, actual PostgreSQL users, the real password login,
secure session cookies, real `SessionAuthMiddleware`, and production route registration.

For reproduction, create the same isolated fixture container, apply the patch to the
frozen commit, and run from `validation-surface-repo/sidecar` (quote arguments in PowerShell):

```text
go test -json -count=1 -coverprofile=surface.cover \
  -coverpkg=./internal/api,./internal/auth,./internal/mcp,./internal/policy,./internal/agentdb \
  -run '^TestPreflightSurface' ./internal/api
```

Set `SAGE_TEST_DATABASE_URL` to the owned disposable endpoint before invocation.
Do not repoint these tests to an existing database. No environment credentials are saved
in the report or fixture artifacts.

## Confirmed authorization and lifecycle findings

### PF-S01: A viewer persists MCP proposals and operational consumer registrations

Severity P1; confidence high; reproduces SURF-01.

The same viewer login gets HTTP403 from REST `POST /api/v1/policy/proposals`, but HTTP200
from the mounted MCP `propose_policy_change` tool. A direct database read proves proposed
policy rows increase from0 to1. A second test invokes `register_consumer` and proves the
consumer registry gains the requested row while MCP reports `decision=granted` and
`applied=true`. This is a persisted operational metadata change, not merely a preview.

Sources: `sidecar/internal/api/router.go:158`,
`sidecar/internal/mcp/production_backend.go:78`,
`sidecar/internal/mcp/production_backend.go:128`,
`sidecar/internal/mcp/postgres_access.go:57`.

The MCP dependency composition uses the actual transport, server, deterministic planner,
production backend, production intent executor, PostgreSQL access layer, persisted standing
policy document, and real authorization Gate. Its runtime-state callback is deliberately
configured as enabled/autonomous/auto. It does **not** instantiate the production fleet
Gate dispatcher; the proof is strongest for the mounted HTTP role boundary. The missing
API principal/role check is before fleet selection. There is no claim that MCP bypasses
all standing policy: anonymous requests return401, and setting emergency stop in the Gate
prevents the consumer write with a zero-row assertion.

Acceptance: role-check every mutating MCP tool using the authenticated principal; bind
agent capability/scopes to that principal; preserve read-only tools for viewers; require
REST/MCP parity tests for every tool and prove both response and durable non-mutation.

### PF-S02: Unverified OIDC email receives an existing administrator's session

Severity P1; confidence high; reproduces SURF-02.

For a configured OIDC issuer, the local issuer returns an existing admin email and an
unrelated subject with either `email_verified=false` or no verification field. Both flows
complete the real authorize/state-cookie/token/userinfo/callback exchange. Callback302
sets a session; `/api/v1/auth/me` returns200 and the existing user's `role=admin`.
A verified, previously unseen user correctly receives the configured viewer role.

Sources: `sidecar/internal/auth/oauth.go:329`, `sidecar/internal/auth/auth.go:361`,
`sidecar/internal/api/auth_handlers.go:512`.

Prerequisite matters: the configured issuer must allow the attacker to obtain a token
whose userinfo contains the victim's unverified email. This is not evidence that an
arbitrary Internet issuer or forged unsigned token is accepted by an unrelated deployment.
The callback currently trusts the configured userinfo response, ignores verification and
subject binding, and links by email. The fixture intentionally supplies no ID token,
matching the access-token/userinfo flow implemented in this code.

Acceptance: identity key is issuer+subject; verify the provider's asserted email before
using it for any email-link flow; never silently link a new external subject to an existing
privileged local account. Require explicit authenticated linking or administrator migration.
Test false/missing verification, changed email, same email/different subject, issuer change,
state replay, token/userinfo errors and provider-specific verification semantics.

### PF-S03: Rejected local schema registration leaves an untracked physical schema

Severity P2; confidence high; newly confirmed beyond the original audit.

A real operator submits `POST /api/v1/agent-dbs` with local schema provisioning and a
schema name, but omits required tenant identity. The API returns400. PostgreSQL still
contains the schema, while the deployment registry contains no row. Resource creation
precedes `Register` validation and is not rolled back with the rejected request.

Sources: `sidecar/internal/agentdb/schema.go:74`,
`sidecar/internal/agentdb/schema.go:83`, `sidecar/internal/agentdb/store.go:111`,
`sidecar/internal/agentdb/store.go:403`.

The analogous local database path also issues CREATE before Register; it is statically
exposed but was not executed in this bounded suite. Existing-schema adoption, registry
insert failure, and naming collisions need separate tests. Do not describe the database
variant as runtime-confirmed from the schema-only reproduction.

Acceptance: validate the complete request before DDL; use transactional schema creation
and registration where possible; for CREATE DATABASE, use a durable provisioning intent,
strict ownership tags and compensation/reconciliation. Never drop a pre-existing object
while cleaning up a failed request. Prove no orphan after each injected failure.

### PF-S04: Terraform approval attribution trusts a forged body field

Severity P2; confidence high; reproduces SURF-17.

A real operator creates a template and posts approval with a different administrator's
email in `approved_by`. Both HTTP response and a direct database read preserve the forged
actor. The approval field does not match the session identity.

Sources: `sidecar/internal/api/agent_db_terraform_template_handlers.go:44`,
`sidecar/internal/api/agent_db_terraform_template_handlers.go:60`.

This proves audit identity spoofing for template approval, not successful live cloud
provisioning. The live authority path is stronger: operator authorization issuance is403;
forged approval/actor/cost claims against an existing AWS plan return400, keep its status
`planned`, and leave its provider resource ID empty. No real provider call is attempted.

Acceptance: derive created_by/approved_by from the server principal; reject or ignore
caller identity fields consistently across templates, blueprints, requests and deployment
reviews. Preserve actor ID, role, request ID and reviewed content hash in immutable history.

### AgentDB local lifecycle controls and limits

CHECK-S05 passed: operator provisioning created a real PostgreSQL schema and durable
active/provisioned registry record; ping, lease extension and archive returned success.
Deletion while backup-required and without restore evidence returned409. A trusted operator's
`restore_verified` backup attestation then allowed deletion of the registry record's active
state (a durable `deleted` tombstone). The physical schema remained present.

This establishes **registry lifecycle**, not actual backup verification, restore success,
credential isolation or physical resource cleanup. The backup route accepts operator
attestation; it did not run a backup or restore. Local schema provisioning did not create
an isolated PostgreSQL role. No claim is made that the new schema isolates tenants by itself.

Acceptance: label registry removal, resource destruction and archive distinctly; explicitly
specify who may attest restores and what immutable evidence is required; expose actual
resource ownership and credential scope; prove that deleted/archived records cannot regain
worker claims or remain unintentionally active in fleet monitoring. Root owns the real-binary
fleet lifecycle check; this suite does not substitute registry assertions for that check.

## Mutation-path inventory and exclusions

The CSV/TSV contains1533 candidates across265 files:870 Go AST callsites plus663 native,
SQL-installation and operational-script lines. Every row has source location, enclosing
Go function where applicable, sink, operation classification, reviewed feature family,
caller/entrypoint, gate, emergency-stop relationship, role, durability and verification.
All enumerated families have a classification; dynamic semantics remain explicit.

Counts are **candidate callsites/statements**, not unique features or dangerous writes.
277 entries are literal SELECT candidates;22 are session/lock controls;61 are transaction
controls. Other rows include HTTP reads, response-buffer writes and repeated SQL installation
versions. These stay visible to make exclusion reasoning auditable. A SELECT can still be a
mutation when it invokes a signaling/reload/drop function, so method names alone are not a
safety classification. Family metadata is a review aid, not proof every call shares one gate.
Direct caller lists match Go function/selector names and may overinclude overloaded names;
they do not constitute a type-resolved call graph.

| Family / production path | Concrete write or effect | Gate, stop and role boundary | Durability and proof status |
|---|---|---|---|
| Bootstrap/migrations/startup | CREATE/ALTER sage schema, extensions, grants, default policy, initial admin/config | Trusted process/database administrator. Deliberately separate from autonomous emergency stop | Advisory locks/transactions where used; migration DDL and seed data. Not a finding merely because it bypasses action Gate |
| Auth/users/sessions | User creation, password/role changes, session insert/delete, last_login | Public login/OIDC callback validates identity; user administration route roles | Real login/session used; unverified-email identity defect reproduced |
| API configuration/fleet registration | Persist config/database rows; start/stop pools/workers; optional config-file writes | Administrative route roles, config version/CAS; not the optimizer Gate | Root owns binary/fleet verification. No assumption that database stop terminates every worker |
| Scheduled executor | DDL/DML, index creation/drop, vacuum/analyze, ALTER SYSTEM, backend signal | Typed policy, trust/execution/feature controls, emergency stop, replica restrictions on normal executor paths | DDL and action/audit writes are often separate; root/core evidence required for atomicity |
| Manual action and manual rollback | Recommendation SQL and stored rollback SQL | `manualMutationBlock`, SQL validation, actionable finding match, API operator/admin role; repeated emergency checks | `manual.go` and low-level DDL helpers; not automatically identical to standing-policy auto execution |
| Legacy delayed rollback | `MonitorAndRollback` invokes ExecConcurrently/ExecInTransaction | DB emergency stop checked. Optional standing-policy callback; manual caller omits callback (`manual.go:105`) | In-memory timer; post-DDL outcome/verification updates; cannot claim durable restart recovery or universal policy equality |
| Verified index lifecycle and retained cleanup | Apply/revert/drop retained invalid or obsolete indexes | Per-phase authorization and object ownership/identity checks at higher-level caller | Verification records; root/core fault/restart tests, not covered by surface router suite |
| Retention enforcer | Batched DELETE on application table and separate retention_run insert | Confirmed bypass of runtime executor/trust/execution/emergency controls | Root reproduced partition wrong-row deletion and committed DELETE when audit insertion fails; see root retention evidence |
| Freeze/WAL/schema autonomy | Vacuum/freeze, slot-related action plans and contract/audit metadata | Adapter-specific typed Gate and limits; retention must remain its own exception | Background workers and durable metadata; do not infer retention protection from neighboring workers |
| Query hints | Metadata sage.query_hints versus live hint_plan.hints INSERT/DELETE | Tuner metadata is a recommendation; actual apply/rollback follows executor SQL. Revalidation marks metadata retired/broken | `tuner.go:833,883,899`; `revalidate.go:291`. Metadata retirement alone does not establish removal of a live hint |
| Optimizer/EXPLAIN/hypothetical indexes/vectorlab | Session GUCs, hypothetical objects, bounded plans/experiments | Diagnostic SQL restrictions, session/transaction scope; vectorlab CLI uses read transaction | Session cleanup and clone boundaries need their own checks; core agent owns runtime diagnostic validation |
| Migration runtime | Shadow relation/backfill/swap/cleanup SQL and migration_run journal | Planned typed contracts and per-step runtime authorization; low-level applier is not a global gate | Step/transaction-specific recovery. Root/core failure and resume evidence; no destructive migration here |
| Clones/trials | DLE POST /clones and DELETE /clones/id; SnapshotAPI.Restore/Destroy | Configured service credentials and clone validation; not API viewer roles | DLE is wired by mcp_migration_runtime.go; generic snapshot implementation needs provider injection. No external clone probe performed |
| MCP | Policy proposals, table contracts, replication consumer registration; migration/candidate intents | HTTP session, missing mutating-tool role check; operational typed policy remains active | Persisted viewer writes reproduced; proposal is not ratification or arbitrary SQL execution |
| AgentDB local provisioning | CREATE SCHEMA / CREATE DATABASE and registry record | Operator/admin API, database DDL privilege; administrative workflow separate from optimizer stop | Rejected-schema orphan reproduced; local database variant static only |
| AgentDB live provider execution | AWS RDS SDK, CloudSQL/Lakebase/Neon/Supabase HTTP create/delete and credential persistence | Persisted plan/estimate/authorization/idempotency, layered live policy, ownership and runner selection | Live claims/attempts and eventual provider state. Spoofed request rejected locally; live service behavior untested |
| AgentDB requests/templates/blueprints | Plans, approvals, template files/manifests, request state | Operator/admin; live authorization and provider configuration admin-only | Forged template approval actor reproduced; approved template content is a separate wiring issue in surface audit |
| AgentDB lifecycle/claims/pings | Archive/tombstone, lease, monitoring work claim/revoke, tokens, audit | Lifecycle state machine, leases/tokens, trusted operator controls | Local registry lifecycle proven; physical cleanup/fleet removal separate. Shared Ensure bootstrap writes are intentional |
| AgentDB cost/backup/restore records | Cost samples, budget state, backup/check/restore attestation | Operator APIs and service reconciliation; not autonomous DDL Gate | Attestation is metadata, not proof an external backup or restore happened |
| Internal telemetry/evidence | Snapshots, findings, incidents, query store, forecast, action/value/verification/ledger rows | Internal service authority; monitoring should continue during emergency stop | Ordinary metadata is excluded from dangerous application DDL claims; atomicity/error handling still assessed per path |
| Alerts/LLM/OAuth/provider observations | HTTP service requests and delivery logs | Configured endpoints/auth and request scope; separate from action emergency stop | Notification outbox gap and provider token lifetime remain original findings. No live service calls in validation |
| Native C extension | SPI_execute/sage_spi_exec, native finding/action log, libpq DDL worker and native rollback | PostgreSQL function grants, native trust/safety/feature GUCs; independent from Go HTTP Gate | SPI transaction versus dedicated libpq execution differ; native runtime is not verified by Go router test coverage |
| Installation and operational tooling | SQL install/upgrade, cloudsqltests helpers, shell/PowerShell/Python launchers and deployment commands | Explicit developer/admin invocation and supplied credentials; not normal daemon routes | Enumerated but never executed here. Direct DB grants, trust backdating and data loaders must stay isolated to chosen dev targets |

Source inventory includes low-level direct SQL and wrapper calls, generic cloud create/delete
methods, outbound requests, filesystem/process effects, native SPI/libpq boundaries, SQL
installation scripts and developer tooling. In-memory mutations, pure SQL construction,
ordinary log rendering, and UI form state are not independently treated as resource writes.
They are represented through downstream sinks or discussed as wiring gaps in the main audit.

Remaining inventory blindspots: unknown future interface implementations; reflection or
function values; SQL fragments resolved at runtime; batch contents hidden behind abstractions;
side effects of PostgreSQL functions and triggers; extension/shared-preload internals;
provider SDK calls below the repository boundary; subprocess effects selected by configuration;
cloud CLI behavior; user-supplied scripts; SQL/command source embedded in data not present at
this commit; shell/SQL semantics beyond the lexical scan. Generated UI assets are excluded.
This inventory should become a maintained review artifact and a basis for policy-gate coverage,
not a claim that a static grep or AST walk proves universal authorization.

## Test Results

**Command:** `go test -json -count=1 -coverprofile=surface.cover -coverpkg=./internal/api,./internal/auth,./internal/mcp,./internal/policy,./internal/agentdb -run '^TestPreflightSurface' ./internal/api`

**Total:** 4 passed, 5 failed, 0 skipped top-level tests. Counting leaves instead gives
4 passed, 6 failed, 0 skipped (the OIDC failure has false/missing subcases).
Exit code1 is expected evidence of unresolved bugs, not a successful test session.
Root separately owns the full baseline and union coverage; this targeted package run does
not replace `go test -cover -count=1 ./...` or the required broader integration suite.

**Coverage:** weighted statement coverage of production packages instrumented by this
focused run (not the complete existing package suites):

| Package | Covered / statements | Coverage |
|---|---:|---:|
| internal/agentdb | 399 / 3922 | 10.17% |
| internal/api | 526 / 4992 | 10.54% |
| internal/auth | 127 / 355 | 35.77% |
| internal/mcp | 128 / 437 | 29.29% |
| internal/policy | 206 / 675 | 30.52% |

### Skipped Tests (must be zero or justified)

Zero in the focused final run. SKIP/TODO/PENDING output scan found no matches.
No unavailable database or extension was silently treated as a passing regression.

### Failures

- MCP viewer proposal persisted despite REST403.
- MCP viewer consumer registration persisted and reported granted/applied.
- OIDC unverified or missing-verification email linked to an existing admin.
- Invalid AgentDB registration created a physical schema without a registry row.
- Terraform template approval attributed to a forged actor from the request body.

### Coverage Gaps (packages below threshold)

All five instrumented packages are below the70% business-logic threshold in this focused
run. Missing paths include the rest of API CRUD/error branches, provider reconciliation,
cloud authorization issuance/replay/concurrency, token lifecycle, OIDC issuer/subject/account
link changes, all other MCP tools, and policy budgets/windows/ratification/lease concurrency.
Root's full baseline is the source for actual whole-suite package coverage. No clean-release
claim or waiver of remaining coverage requirements is made. Expanding unrelated tests or
fixing production code was outside the explicitly authorized pre-remediation scope.

### Bugs Found This Session

1. Confirmed MCP role bypass (SURF-01) with durable operational and proposal writes.
2. Confirmed OIDC unverified-email account linking (SURF-02) with real admin session.
3. New local provisioning orphan on rejected registration (PF-S03).
4. Confirmed forged approval audit actor (SURF-17), without claiming cloud authorization bypass.

### Post-test audit

The assertions check observable persisted rows, role identity, resource existence and lifecycle
state rather than only `err==nil`. Positive controls prove the same real login/middleware can
deny anonymous users, viewers and non-admins appropriately. The MCP Gate stop test asserts both
reason and absent row. The OIDC provider is a local protocol test double; it replaces only the
external identity service, not the callback, state cookie, account lookup or session middleware.
MCP runtime state is configured by callback; fleet dispatch is a remaining integration boundary.

Uncovered break inputs: concurrent duplicate registration and permission-denied/create-success
registry-failure; existing schema/database name collision; loss of connection after external
create; OIDC subject/email change and cross-issuer linking; stale/expired state; all MCP tool
roles and database scopes; idempotency races; provider timeouts and delayed deletion; approved
content changes between review and execution. These are retained as follow-up gates, not claimed
passes. No concurrent tests were added here: the reproduced boundaries are sequential HTTP
identity/authorization and lifecycle transitions, while concurrency behavior remains explicitly
unverified. Root/core owns the concurrent execution and crash-recovery probes.

### Manual Checks Remaining / exact remaining gates

CHECK-S01: FAIL — Viewer cannot persist MCP policy proposals.
CHECK-S02: FAIL — Viewer cannot register operational replication consumers.
CHECK-S03: PASS — Anonymous MCP requests denied; stopped Gate cannot persist consumer.
CHECK-S04: FAIL — False/missing OIDC email verification cannot obtain existing admin session.
CHECK-S05: PASS — Verified new OIDC user receives viewer session.
CHECK-S06: PASS — Viewer AgentDB write denied; operator provider-config and authorize-live denied.
CHECK-S07: PASS — Existing cloud plan rejects forged live claims without provider resource/state change.
CHECK-S08: PASS — Local schema creation and registry lifecycle/backup-required guard behave as observed.
CHECK-S09: FAIL — Invalid local registration leaves no orphan.
CHECK-S10: FAIL — Template approval actor matches authenticated principal.
CHECK-S11: MANUAL — Real supported OIDC provider configuration and subject-link migration after fixes.
CHECK-S12: MANUAL — Provider/clone staging commissioning; credentials, policy and explicit cloud scope required.
CHECK-S13: MANUAL — Native extension privilege and mutation controls in actual supported PostgreSQL versions.
CHECK-S14: MANUAL — Fleet routing, browser affordances and real-binary lifecycle; root-owned evidence required.
CHECK-S15: MANUAL — Semantic closure of dynamic SQL/indirect subprocess/provider effects beyond static inventory.

No production fixes were performed. The new failing tests remain staged and reviewable in
the isolated checkout. The uniquely owned validation container is removed after evidence capture;
see the cleanup receipt in evidence. No existing containers or live databases are changed.
