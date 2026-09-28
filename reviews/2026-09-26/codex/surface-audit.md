# pg_sage feature surface and completion audit

Audit date: 2026-09-26. Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Repository: `audit-repo`. All references below are relative to its root.
Scope: API/UI/auth/config/fleet, Cases/value, alerts/metrics/MCP and AgentDB.

This is a source-trace audit. Findings labeled confirmed mean the relevant
production call chain and data contract establish the defect; they do not mean a
live exploit or cloud mutation was executed. No source fixes, live provider
operations, or external notifications were performed. The coordinating audit
owns test execution and its test-results report. `CLAUDE.md` was absent from this
snapshot. The independent Gemini review could not run; see `gemini-review.md`.

## Overall assessment

The repository has substantial working machinery, including authenticated REST
routes, policy-gated actions, live provider runners, cleanup claims and durable
verification tables. It also has a recurring completion problem: a carefully
tested package can exist without its production caller, or the UI can present a
stronger claim than the persisted evidence supports. The first release priority
should be identity/authorization, truthful verification, and complete runtime
wiring before adding autonomous SRE scope.

The highest-risk defects are the HTTP MCP role bypass, unverified OIDC email
linking, and value attribution/read failures. The most visible unfinished
surfaces are provider readiness, Terraform template semantics, adaptive
monitoring, incident value credit, and the replacement of actionable legacy
pages with a read-only Cases view.

Priorities: P1 before production expansion; P2 functional defect; P3 robustness.
This audit does not certify unlisted code as bug-free.

## Findings

### SURF-01 — P1: HTTP MCP bypasses the viewer role boundary

**Confirmed, high confidence.** `sidecar/internal/api/router.go:158` mounts
`POST /api/v1/mcp` directly, without `RequireRole`. Session auth still applies,
but every logged-in role can reach the tools. In contrast,
`sidecar/internal/api/policy_handlers.go:49` explicitly rejects viewers when
creating a policy proposal through REST.

`sidecar/internal/mcp/production_backend.go:75` forwards proposal writes;
`sidecar/internal/mcp/postgres_access.go:200` persists them with a generic actor.
The mutation chain in `production_backend.go:117`, `intent_adapters.go:13`, and
`production_intent_executor.go:68` does not consume the authenticated user's
role. A permissive standing policy can therefore authorize a viewer's real
table-contract or slot-consumer INSERT/UPDATE via `postgres_access.go:18` and
`:46`. Standing action policy is not a substitute for caller authorization.

**Trigger/reproduction:** enable HTTP MCP, log in as viewer, call
`propose_policy_change` with a valid policy delta; compare with REST's 403.
With executor enabled, auto mode, sufficient trust and a policy permitting the
class, call `register_consumer` or `declare_table_contract` and inspect the
target table. No live reproduction was performed.

**Fix/acceptance:** bind a trusted principal to MCP requests; enforce per-tool
read/write roles before planning or persistence; preserve actor ID in evidence.
Viewer mutation calls must be denied before any DB write under every policy
profile. Test the real mounted router, not just a mocked backend.

### SURF-02 — P1: OIDC email linking accepts an unverified identity

**Confirmed, high confidence; exploitability depends on the configured issuer.**
`sidecar/internal/auth/oauth.go:329` decodes only `email`, ignores
`email_verified`, and returns any nonempty email. The exchange does not retain
issuer/subject as the durable user key. `sidecar/internal/auth/auth.go:361-375`
looks up the existing user solely by email and returns that user's role,
including admin, without an explicit account-linking step.

**Trigger/reproduction:** configure an OIDC test issuer whose userinfo returns
an existing admin's email with `email_verified:false`; complete its otherwise
valid code exchange. The local user lookup grants the existing admin identity.
An issuer that always verifies emails may reduce exploitability, but the
application does not enforce its own requirement.

**Fix/acceptance:** use issuer+subject identity, require verified email for any
email-based linkage, and require deliberate authorized linking of existing
password accounts. Reject missing/false verification; test subject change,
issuer change, duplicate emails, unverified email and concurrent first login.

### SURF-03 — P2: Value reporting loses normal action attribution and can fail

**Confirmed data-contract mismatch, high confidence; integration validation
belongs in the coordinating test report.**
`sidecar/internal/executor/executor.go:1243` and
`sidecar/internal/executor/manual.go:325` insert action logs without
`database_id`. `sidecar/internal/schema/ddl_agent_value.go:42` makes this column
nullable; no production repair/trigger was found. Crediting can subsequently
populate toil minutes through `executor/rollback.go:380` and
`executor/index_verification_runtime.go:90`.

`sidecar/internal/value/postgres.go:139-163` LEFT JOINs `sage.databases`, selects
`d.name` without COALESCE and scans it into a nonnullable string. A credited
row with NULL database attribution therefore fails the entire all-databases
Value response; filtering by database instead silently omits it.

There is a second topology mismatch: `sidecar/internal/api/router.go:183-184`
constructs Value from the auth/meta pool only, while normal fleet executors write
to each monitored database's pool. Unlike Cases, Value does not aggregate
selected fleet pools. Depending on topology it can show empty/incomplete value
even when target databases contain verified credit.

**Trigger/reproduction:** execute a normal action through the actual executor,
complete verification, inspect its database_id, then read `/api/v1/value` with
and without a database filter. Repeat with separate meta DB and two targets.

**Fix/acceptance:** pick one explicit ledger storage topology, stamp canonical
database identity at write time, migrate old rows, handle unknown identity
without failing, and use it consistently in API/MCP/metrics. A two-database
fixture must yield correct disjoint per-DB and summed fleet totals.

### SURF-04 — P2: Provider readiness never receives its runtime dependencies

**Confirmed, high confidence.**
`sidecar/internal/api/agent_db_provider_handlers.go:12` calls
`ProviderReadinessList(r.Context())` without options.
`sidecar/internal/agentdb/provider_readiness.go:34` consequently always chooses
the no-runtime branch, marking every cloud provider unavailable with
`runtime_dependencies_missing`. The subrouter already has a runtime registry
and authority, but passes neither to this handler.

`sidecar/web/src/pages/AgentDBsPage.jsx:139` consumes this endpoint, and
`pages/agentdb/AgentDBProvisioningPanels.jsx:284-285` displays missing/readiness
from the resulting `found` flag.

**Reproduction:** configure any live runner plus valid effective policy and GET
`/api/v1/agent-dbs/providers`; it still reports missing runtime dependencies.
**Acceptance:** endpoint must derive readiness from the same registry,
credential checks and policy snapshot used by execution, with current reasons,
version/hash and timestamp. Test enabled/disabled/expired-credential transitions.

### SURF-05 — P2: Terraform template provisioning does not use template content

**Confirmed, high confidence.**
`sidecar/internal/agentdb/provision_links.go:97-146` loads the approved template
but uses only its status and ID. It constructs a generic provider profile from
the new request and calls `BuildProvisionPlan`; `template.Files` and
`template.Manifest` do not affect the plan. Uploaded infrastructure settings
such as networking, retention, extensions or instance class are therefore not
the settings that this flow provisions.

**Reproduction:** approve two templates with meaningfully different resource
settings and submit identical provision parameters to each; the effective
provider plan is identical apart from template provenance metadata.

**Acceptance:** either implement a constrained parsed-template-to-plan contract
and show an exact reviewed semantic diff, or explicitly name this feature
template storage/review and remove provisioning claims. Bind approval to an
immutable content hash and require supported attributes to survive into the
execution plan. Arbitrary Terraform execution is not recommended as a shortcut.

### SURF-06 — P2: Dry-run backup checks claim verified backup evidence

**Confirmed, high confidence.**
`sidecar/internal/agentdb/backup_assurance.go:17-39` executes the command-runner
path then records `Status: "verified"` unconditionally after a zero exit code.
The default registry's runner is a dry run (`provider_runner.go:101`), and live
runners without the legacy command interface also fall back to dry run.
`provider_runner.go:147` even returns a verified status for its dry-run backup
method. `BackupAssurancePanel.jsx:12-14` renders verified green without exposing
execution mode.

**Reproduction:** with no cloud runner configured, register a planned cloud
deployment and click Check backups. No provider backup has been observed, but a
verified record is stored. This alone is not the restore-required deletion
bypass: teardown separately requires `restore_verified`; keep that distinction.

**Acceptance:** dry runs produce only `planned`/`not_checked`; verified requires
provider or artifact evidence including ID, observed time, integrity status and
provenance. The UI must show plan-only, provider-observed and restore-tested as
different states. Backup retention settings alone do not prove a usable backup.

### SURF-07 — P2: AgentDB monitoring is partial and not reconciled on removal

**Confirmed, high confidence.**
`sidecar/cmd/pg_sage_sidecar/agentdb_fleet.go:87-106` only adds eligible
deployments and skips any already registered. It never removes inactive,
archived, expired-secret or deleted deployments, nor replaces a changed
connection/credential. `:110-155` attaches only a Collector; analyzer, executor,
per-target safety/readiness initialization and full pipeline are absent.

The secret resolver in `agentdb_secret_ref.go:25` supports only `env:` URIs.
For example AWS runners return AWS Secrets Manager ARNs
(`agentdb/aws_rds_runner.go:376`), so successful provider creation cannot
automatically become a monitored database through that path. Cloud SQL and
Lakebase also have endpoint-to-usable-credentials gaps that need explicit
commissioning rather than an assumption of readiness.

**Reproduction:** register/monitor one inline or env-backed deployment; archive
it or expire/change its secret; run the reconciler and inspect fleet instances
and active pools. Separately provision an ARN-backed RDS deployment and inspect
its absent fleet instance. No cloud operations were performed here.

**Acceptance:** desired-state reconciliation must add/update/remove instances
and cancel/join workers, with credential version/expiry handling. Clearly label
collector-only mode or install the full desired pipeline with read-only defaults.
Resolve provider secrets just in time without persisting credentials. Demonstrate
create -> connect -> collect -> analyze -> candidate -> approved action -> verify
-> expire -> stop -> cleanup for each supported provider.

### SURF-08 — P2: Adaptive monitoring exists only as uncalled storage primitives

**Confirmed, high confidence; documented limitation.**
`sidecar/internal/agentdb/monitoring_schedule.go:17` and
`monitoring_claims.go:46` have no production callers. No runtime worker completes
these claims. `docs/agent-db-deployments.md:252-258` admits this explicitly, so it
is unfinished scope rather than a hidden claim in that guide.

The scheduling query also always orders the first 10,000 deployment IDs and
selects at most 100 targets by count/key, without using due time or rotating a
cursor. If wired unchanged, the same targets can monopolize passes and others
starve. `state.next_due_at` is written but not used to select eligible targets.

**Acceptance:** wire a bounded scheduler and probe worker, claim fencing,
completion/retry/dead-letter states, last-success freshness and shutdown. Test
101+ targets and 10,001+ deployments across repeated passes to prove eventual
service, due-time behavior, provider/tenant budgets and stale claim recovery.

### SURF-09 — P2: Query-hint IDs are corrupted before generated cleanup SQL

**Confirmed, high confidence.**
`sidecar/internal/api/cases_handlers.go:643` converts an int64 `queryid` through
float64. PostgreSQL query IDs commonly exceed 2^53, where that conversion loses
integer precision. The exact helper `int64Value` already exists nearby, but this
projection does not use it. `cases/query_hint_projector.go:29` propagates the
rounded identifier into case identity, and `cases/query_actions.go:41-47`
propagates it into cleanup/verification scripts.

**Reproduction:** project a broken hint with queryid `9007199254740993`; the
generated identity/SQL uses `9007199254740992`, targeting no row or the wrong row.
**Acceptance:** preserve int64 end-to-end and serialize identifiers as strings
at JavaScript boundaries. Test positive/negative 64-bit limits and neighbors
around 2^53 through DB -> API -> browser -> action, not only isolated helpers.

### SURF-10 — P2: The Cases replacement removed existing operator workflows

**Confirmed, high confidence.** `sidecar/web/src/App.jsx:152-196` routes Findings,
advanced Findings, Incidents, forecasts, schema health and query hints to
`CasesPage`. That page (`CasesPage.jsx:56`) only fetches, filters and displays
evidence/scripts; it has no suppression, unsuppression, incident resolution,
historical status view, or candidate-to-approval command.

Those actual actions survive in unimported `Findings.jsx:346-459` and
`IncidentsPage.jsx:170`, along with unreachable old `ForecastsPage`,
`QueryHintsPage`, `SchemaHealthPage` and `DatabaseSettingsPage` components.
The separate Actions page remains reachable and can handle already-queued
actions; the gap is specifically Cases and the replaced lifecycle workflows.

**Reproduction:** browse Findings explorer or Incidents as operator and try to
suppress/unsuppress or resolve an incident; no control exists despite active API
routes and legacy component code.

**Acceptance:** restore the needed workflows in Cases with source IDs and DB
context, provide resolved/suppressed history and case-to-action navigation, then
delete superseded components. Route tests must exercise actual operator outcomes
and viewer denial, not just page headings or fixture rendering.

### SURF-11 — P2: Cases advertise ranking and freshness they do not preserve

**Confirmed, high confidence.** `CasesPage.jsx:81` says ranked work items and
`:152-153` displays impact/urgency. `cases/case.go:52` has no score fields;
`api/cases_handlers.go:90-140` concatenates findings, incidents and hints per
database with no final global ranking. A critical incident can appear after
hundreds of informational findings. SourceFinding also omits original timestamps,
so `cases/case.go:155-158` assigns the projection time to old finding cases.

Candidate expiry is similarly recalculated from `time.Now()` on each projection
(`cases/query_actions.go:10`, `:27`, `:57`, `:71`), making the displayed freshness
window slide with every read. This does not prove executor stale-evidence bypass,
but it makes the review artifact itself misleading.

**Acceptance:** preserve observation time and immutable candidate generation /
evidence expiry, define reproducible global ordering, and either populate scores
with explained formulas or remove the labels. Test mixed-source, multi-DB
ranking and stale candidates across repeated reads.

### SURF-12 — P2: Execution success is mislabeled as outcome verification

**Confirmed, high confidence.**
`sidecar/internal/api/cases_handlers.go:486-502` returns `verified` for any action
with `outcome == "success"`, even with no `measured_at` or durable verification
record. The Value service correctly has a stronger contract: completed success
verification is required (`value/service.go:51-54`). These surfaces disagree.

**Reproduction:** project an action-log row `{outcome:"success", measured_at:nil}`;
Cases says verified. **Acceptance:** join/use the authoritative verification
record and show applied/pending verification/inconclusive/verified/reverted
separately. A successful SQL return must never by itself assert benefit or safety.

### SURF-13 — P2: Shadow savings ignore the policy decision for candidates

**Confirmed, high confidence.** `sidecar/internal/cases/shadow.go:52` increments
WouldAutoResolve for `safe` and an empty blocked reason even when its attached
policy decision is `queue_for_approval` or another nonexecute verdict. The proof
row can simultaneously display approval required while the totals claim auto
resolution. There is also no candidate expiry check in this counting path.

**Reproduction:** build a safe candidate with no blocked reason but
PolicyDecision=`queue_for_approval`; inspect inconsistent aggregate and proof.
**Acceptance:** explicitly evaluate a named hypothetical auto-safe policy against
current capabilities/freshness or use the provided authoritative decision; count
only eligible unique work, and show uncertainty/model version for toil estimates.

### SURF-14 — P2: Value evidence links are dead and incident credit has no producer

**Confirmed, high confidence.** `sidecar/web/src/pages/ValuePage.jsx:207` links
to `#/ledger?evidence_id=...`; App's exact switch in `App.jsx:146-216` has no
ledger route or query parsing, so it opens Page not found.

The incident-avoided value card/metric is backed by `sage.incident_avoided`, but
`sidecar/internal/value/postgres.go:110`'s `RecordIncident` has no production
caller, and no other production INSERT into that table was found. The UI works
with hand-seeded rows, while actual custodians do not produce that output.

**Acceptance:** provide a routable evidence/decision/action/verification detail
view, integrate qualifying incident credit from verified outcomes with unique
evidence IDs, and label counterfactual prevention separately from observed
recovery. End-to-end tests must start from a real supported detection/action
path and verify both the value row and its clickable evidence.

### SURF-15 — P2: Alert delivery failure can permanently consume the event

**Confirmed, high confidence.**
`sidecar/internal/alerting/alerting.go:106-135` queries findings since lastCheck,
then advances lastCheck to the time after dispatch regardless of delivery
failure. Not recording throttle on all-channel failure (`:185-191`) does not
retry the row because the next query excludes it unless its last_seen changes.
Findings arriving between query snapshot and post-dispatch time can also fall
behind the advanced watermark without being read.

The newer notification dispatcher (`notify/dispatcher.go:47-55`) logs per-rule
errors and returns nil, and has no durable delivery retry queue. Both paths need
one clearly defined reliability contract.

**Reproduction:** create one finding, make sender fail once, recover sender
without updating the finding, run two evaluation cycles; the row is not retried.
Add a finding while a slow send is blocked to reproduce the watermark window.

**Acceptance:** durable outbox and per-channel delivery state with bounded
retry/backoff, idempotency and dead-letter visibility. Commit a read watermark
captured before the query and only advance safely; queue delivery independently.
Test partial channel failure, restart, quiet hours, slow sender and concurrent
arrivals. Never claim a page was delivered from enqueue success alone.

### SURF-16 — P2: Login limiter capacity can be bypassed by its allow path

**Confirmed, high confidence.** `sidecar/internal/api/auth_handlers.go:117`
stores an empty slice for a new email during `allow`. On failure `record` sees
that key already exists (`:129`) and returns before its 10,000-entry eviction
logic. Distinct failed emails therefore grow the map past the claimed bound
between cleanup passes. The non-atomic allow -> authenticate -> record sequence
also permits a simultaneous burst above the per-email attempt limit.

**Reproduction:** call allow/record for 10,001 different emails; map size exceeds
loginMaxEntries. Submit simultaneous failed requests for one account to test
reservation semantics. **Acceptance:** atomically reserve bounded attempts at
entry, normalize identity, enforce capacity in all insertion paths, and test the
actual request sequence plus concurrency. Keep global/IP controls as well.

### SURF-17 — P2: Approval audit identities are accepted from request JSON

**Confirmed, high confidence; this is attribution spoofing, not a live authority
bypass.** `api/agent_db_terraform_template_handlers.go:42,65` and
`api/agent_db_blueprint_handlers.go:39,63` take `created_by` and `approved_by`
from user-supplied JSON. Any authorized operator can record another person's
name as template/blueprint approver. The separate live execution authority has
stronger server-owned checks and should not be conflated with this defect.

**Acceptance:** derive audit actor from authenticated user context, keep any
caller annotation in a separate field, and bind immutable revision/hash to the
approval. Two operators submitting forged actor strings must still record their
real identities. Race an update against approval to verify atomic validation.

### SURF-18 — P3: Prometheus declares reversible savings as a counter

**Confirmed, high confidence.**
`sidecar/cmd/pg_sage_sidecar/value_metrics.go:17` declares
`pg_sage_toil_minutes_saved_total` a counter, but derives it by summing current
successful action credits. Rollback zeroes credit (`value/postgres.go:98`), so
this series can decrease. Prometheus rates interpret that as a reset and can
report false positive savings. Query failures also silently emit no samples.

**Acceptance:** expose current net value as a gauge, and separate monotonic
earned/retracted counters if needed. Add scrape-error/freshness metrics and
prove a rollback does not create a bogus rate spike.

### SURF-19 — P3: Incident resolution reason is discarded

**Confirmed, high confidence.** `api/handlers_v09.go:436` assigns reason to `_`
after updating resolved_at. Its comment claims it is logged, but this function
does not persist or log it. **Acceptance:** record resolver, reason, source
evidence and time in an immutable event; surface it in resolved case history.

### SURF-20 — P3: Cloud SQL provisioning uses a permanently captured access token

**Confirmed lifecycle limitation, high confidence.**
`agentdb/runtime_registry.go:45-58` captures `PG_SAGE_GCP_ACCESS_TOKEN` into
`staticToken`; registries are created at startup/router construction and never
refresh that token. A long-lived sidecar loses Cloud SQL status/cleanup access
when the configured short-lived OAuth token expires.

**Acceptance:** use a supported refreshable ADC/workload identity token source,
report credential expiry/readiness, and demonstrate create/status/cleanup across
token rotation without restarting. Keep preflight honest: the CloudSQL runner's
Preflight validates configuration but does not make a credential-validation call.

## Existing feature improvements and acceptance criteria

The matrix covers the groups owned by this audit. The companion execution audit
covers collector/analyzer/optimizer/advisor internals, tuning, forecasting,
executor, custodians, clones, migration runtime and other engine logic.

| Feature group | Current path and completion boundary | Highest-value improvement | Acceptance criteria |
|---|---|---|---|
| Auth/password/users | Login -> session cookie -> per-request user lookup; admin protection exists; limiter has SURF-16 | Strong bounded login admission, session/device management, explicit role matrix | Concurrent login flood bounded; revoked/deleted/demoted user loses capability immediately; last-admin invariant survives races |
| OAuth/OIDC | Discovery -> code exchange -> email -> local account; SURF-02 | Issuer+subject identity and deliberate account linking | False/missing verification denied; changed issuer/subject cannot inherit admin; human-readable linking history |
| REST/API | Session middleware, roles on most mutation routes, JSON/body/deadline controls | Generate API capability inventory from route table and test authorization per endpoint | Every route has owner, required role, request schema, error contract and nonmock request test; unsupported features report explicit reason |
| Dashboard navigation | Embedded React SPA; source pages and compiled assets; multiple dead legacy pages | One complete Cases experience with task-oriented routes and route health tests | Every visible link resolves; every operator workflow has an outcome; legacy components deleted; asset build matches source |
| Cases queue | Findings/incidents/hints -> projections -> read-only cards; SURF-09/10/11/12 | Ranked work, preserved source identity/time, evidence drilldown, act/suppress/resolve controls | Global mixed-source ranking; exact IDs; no freshening on GET; full case-to-action and status history |
| DDL/PR/script artifacts | Cases shows preflight, script, rollback, verification SQL and PR metadata | Bind artifacts to source schema revision and evidence expiry; distinguish generated artifact from executed migration | Stale artifact visibly invalidated; scripts explain irreversible forward-fix; generated PR text never implies applied DDL |
| Actions review/audit | Pending/history/action routes, approvals, rejection, rollback; reachable Actions UI | Deep links from Cases with immutable target, actor, policy and verification timeline | Same ID on different DB cannot cross-route; race/retry behavior is visible; success/verified/reverted remain distinct |
| Shadow mode | Cases-derived policy/toil proof; embedded in Settings; SURF-13 | Explicit hypothetical policy version and measured adoption readiness | Proof rows reconcile exactly with aggregates; blocked/expired/manual candidates excluded from auto totals |
| Value/ROI | Root page + value repository + Prometheus; SURF-03/14/18 | Complete target attribution and evidence lineage, editable versioned toil model | Real verified action creates appropriate value in standalone/meta/fleet; rollback retracts it; every claim has a working evidence link |
| Config and hot reload | Desired/active snapshots, typed lifecycles, persistent overrides, audit and restart flow | Show owner/applicability/restart behavior per setting, surface partial activation | Readback shows desired vs active and effective per DB; invalid batch is atomic; restart-only changes never look active early |
| Fleet/database management | Managed database CRUD/import and per-DB runtimes; AgentDB separate path | One reconciliation architecture for normal and agent-created databases, isolation/freshness first | Remove/readd/rotate credentials while polling without stale pools; per-DB mode/budget/config ownership maintained |
| Provider capability observability | Provider facts and action readiness; live APIs and target SQL have distinct limits | Normalize capability with source, observation time and degraded reasons | Provider outage = unknown/stale rather than supported/healthy; action readiness follows verified capability |
| AgentDB inventory/identity/lease | Register and token ping -> leases -> archive/delete; durable metadata | Clear tenant/agent ownership and idempotent lifecycle state machine | Repeated pings do not evade policy; revoked credential stops probes; lease expiry, backup gates and cancellation survive restart |
| Local schema/database provisioning | Actual SQL provisioning separate from cloud planned instances | Provide tested handoff credentials, resource quotas, isolation proof and workload bootstrap | New role can access only intended objects; reconnect and cleanup verified; failed partial creation has a recoverable record |
| Cloud runners (RDS/CloudSQL/Lakebase/Neon/Supabase) | Live registry/gates -> create/status/destroy; no live commissioning in this audit | Credential refresh, scope proof, ambiguous-operation recovery and full usable-connection handoff | Disposable canaries prove create -> usable DB -> backup evidence -> cleanup per provider; duplicate requests create one resource |
| Provider readiness | Rich evaluator exists, endpoint omits dependencies; SURF-04 | Use execution's exact dependencies and policy snapshot | Readiness changes with actual runtime/policy/credential state and explains every disabled state |
| AgentDB blueprints | LLM strict JSON -> normalized spec -> policy findings -> approve -> generic plan | Constrain generated intent to supported semantics and immutable reviewed diff | Networking/backup/class/region values survive generation -> review -> provision; uncertain requirements stay unresolved |
| Terraform intake | Zip/inline -> regex policy -> stored template -> generic provision; SURF-05 | Typed supported-template parser and semantic plan binding, or explicit review-only scope | Two differing templates yield corresponding effective settings; unsupported blocks rejected; aggregate zip expansion capped |
| Backup/restore assurance | Record backup, check provider/dry run, plan drill, manually mark restored; SURF-06 | Automated disposable restore with integrity/business probes and signed evidence | Simulation never marks verified; restore-tested state names real artifact/target/checks/RPO/RTO; stale proof cannot authorize deletion |
| Cost/budget | Manual cost samples plus hardcoded approximate provider estimates | Region/HA/storage/backup/network-aware estimates with source/version and observed billing reconciliation | Unknown components visible and conservative; estimate cannot become high confidence without evidence; enforce user budget over lifecycle |
| Cleanup/lifecycle | Scheduled archive/live status/teardown with restore claim checks | Reconcile DB, provider resource and monitoring workers as one lifecycle | Crash after provider accepts create/destroy resumes safely; archived/deleted records do not leave active collectors or orphaned billable resources |
| Adaptive monitoring | Scheduler/claim tables and functions exist; no runtime caller; SURF-08 | Budgeted adaptive worker with freshness and fairness | >100 physical targets eventually observed; due times respected; claims expire/recover; worker output feeds Cases |
| Agent query recommendations/feedback | Record recommendations and agent feedback in deployment scope | Connect recommendations to evidence and verified application effects | Recommendation accepted/applied/rejected differs; agent feedback does not equal verification; corrected plan carries traceable version |
| Promotion/deploy requests | Stores reviewed promotion intentions and plan artifacts | Explicit source/target revision, data policy and executable staged plan | Promotion cannot silently cross tenant/provider; preview matches apply; rollback/forward-fix is tested for supported modes |
| Alerting/notifications | YAML alert channels plus DB-backed notification rules; SURF-15 | One durable outbox/retry model and clear source-specific routing | Simulated outage/restart/partial delivery recovers without missing pages or duplicate floods; test button and real event use same path |
| Prometheus | Exports process/DB/action/LLM/value series | Correct metric types, bounded labels, separate absent/zero/degraded | Rollback doesn't reset false counters; disappeared DB removed; scrape failure and freshness independently visible |
| MCP agent tools | HTTP/stdio -> deterministic intent -> policy -> target adapters; SURF-01 | Principal-bound tools, consistent protocol/error semantics and capability discoverability | Viewer cannot mutate; stdio shutdown/oversize requests bounded; unsupported tool path explains gating; durable outcomes linked to source request |
| Deployment/documentation | Binary/Docker, embedded UI, provider docs and manual/live tests | A generated capability manifest stating implemented/wired/verified/unsupported for each platform | Fresh install plus upgrade smoke per supported mode; every "works on" claim linked to actual provider receipts and limitations |

## Meta findings and additional questions the product should answer

1. **Where is the source of truth?** There are overlapping action history,
   decision, verification, queued action, value and incident records across
   target and meta databases. Define authoritative ownership before adding SRE
   incident state. Every displayed result should resolve to one immutable chain.
2. **What counts as done?** Compilation and package fixtures do not prove a
   feature's entry point reaches its outcome. Maintain an executable feature
   manifest: route/command -> principal -> policy -> runtime owner -> storage ->
   side effect -> verifier -> API/UI evidence -> failure/restart path.
3. **Which claims are observations versus estimates?** Verified, restored,
   ready, avoided incident, savings and ranked are operational promises. Encode
   their evidence requirements as schemas and shared query views; do not let
   each UI projector invent a weaker definition.
4. **Who may delegate authority?** Human API roles, standing operational policy,
   agent identity, tenant scope and database credentials are separate checks.
   Adding an AI SRE should not collapse them into model-supplied claims.
5. **Does permission expire with evidence?** Bind approval to revision, policy,
   target identity, preconditions and time. Projection time must never refresh
   safety evidence. Revalidate immediately before execution.
6. **What happens after an uncertain external result?** Create/destroy timeout,
   token expiration, lost response and process restart need reconciliation,
   stable operation IDs and operator-visible uncertainty; never retry blindly.
7. **What does an agent-created DB receive?** Provisioned is not connected,
   monitored, analyzed, protected or backed up. Expose each commissioning gate
   rather than a single green state.
8. **How is benefit measured honestly?** Time saved is a model; incident avoided
   is a counterfactual. Separate these from observed latency/error/recovery
   improvements, include uncertainty and prevent double credit.

## Recommended implementation order

1. Repair SURF-01/02 identity and authorization, then add real router-level
   regression coverage.
2. Repair Value's storage/attribution contract and truthful verification states.
3. Restore actionable Cases navigation, ID integrity and evidence freshness.
4. Fix provider readiness and complete or explicitly narrow Terraform/backup
   claims; establish provider commissioning receipts.
5. Unify fleet desired-state reconciliation and finish adaptive monitoring,
   secret refresh and lifecycle cleanup.
6. Add durable alert delivery and audit actor provenance.
7. Build the proposed AI SRE feature on these completed primitives, with causal
   evidence, scoped authority and independently verified recovery.

## Verification limitations

- This sub-audit did not run Go, browser or provider test suites; do not count it
  as a passing test run. The coordinating report owns test totals/coverage/skips.
- No production data, live user identities, credentials or cloud resources were
  used to demonstrate a vulnerability.
- Source-only absence-of-caller checks excluded tests and compiled JS bundles.
  Public APIs intentionally available to embedders are not automatically dead
  code; unfinished runtime features above have explicit advertised product paths.
- Gemini review was blocked before execution by automatic approval review, which
  rejected sending authentication source to the external Gemini service. No
  workaround or alternate-model upload was attempted.

