# pg_sage Lakebase gap-closure plan and independent review packet

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

Date: 2026-10-29; implementation update: 2026-10-30. Status: **dev.5 built, installed and deployed; fresh complete local/native/App acceptance passed, with provider restrictions and unverified boundaries documented**.
Owner: test operator. Repository: `jasonmassie01/pg_sage`, local branch `lakebase-databricks-app`, base `cc0e4481` plus uncommitted development changes.

## Current revision: dev.5 retention and upgrade closure

The [dev.5 acceptance report](2026-10-30-v2.5-dev5-acceptance.md) is the current
accepted development revision within its explicit matrix. The complete uninterrupted dev.4 Go run reached all
142 packages with 16,335 passing events and four failures: two YAML tests inheriting
the integration DSN, migration teardown omitting the new child table, and missing
maintenance-retention coverage. The fixture corrections now pass their complete
packages. Independent review of retention found lifecycle hazards beyond the
missing declaration: old-case cascade, rollback/audit record aging, execution-log
commit before proposal save, concurrent child changes during purge, and held-row
batch starvation. Those are now covered by focused tests and the correction has
passed 56 race-enabled focused events with independent review. The first dev.5
native candidate then exposed a real older-schema upgrade defect: its nine-state
check rejected `uncertain`. Bootstrap now atomically upgrades that check, with
adversarial/idempotence/data-preservation regressions. A local PostgreSQL clock
step also exposed negative elapsed charges and refunds of already spent SRE
active time. Eight deterministic regressions failed before the nonnegative
increment fix and passed after it, alongside cap/fence/expiry tests. This clock
observation does not establish a Lakebase clock defect.

The new maintenance contract uses colocated SRE/target retention holds, fresh
post-lock READ COMMITTED rechecks, bounded rollback cleanup and indexed exact
record links. Split coordination/target databases explicitly withhold only the
typed maintenance bridge until distributed holds exist. Ordinary SRE observation
and existing actions remain available. The earlier full rerun5 was explicitly
interrupted when the upgrade failure required source changes; its incomplete
result and the complete failed rerun4 remain preserved. After the joint source
freeze, one fresh full rerun6 passed **16,394 events, zero failures, 55 allowed
skips**, covering all 142 packages exactly once with source unchanged. The
separate entire native PG18 suite passed **79 events, zero failures, two named
opt-in skips**, including all ten telemetry streams/24 strict delivery gates,
the real Gateway investigator and actual maintenance rollback/retention lifecycle.

Final App SNAPSHOT **`01f1d48e0748107d95b5bdd1ffad0e22`** passed immutable
seven-file provenance, API/browser, Databricks-only identity and real uncached
OAuth Gateway checks. Both history cases passed with all five adapters positive
and cited. The remaining positive DDL gap is closed by an exact owned
CREATE TABLE event through actual scheduler → authenticated API → durable
hashed/cited SRE evidence. The interrupted first DDL attempt and its cleanup
failure remain red; exact recovery cleanup was independently verified before a
fresh complete attempt passed. No trust/grant/payload setting was changed.

The same binary passed fifteen real-time SLI samples over seven minutes, both
burn windows, a cited Gateway case, actual stopped-source staleness at 123.06s
and scoped cleanup. Final baseline checks confirm no synthetic objectives,
samples/status/transitions or active writer; the case is retained. All eight
App action/approval ledgers stayed unchanged. Production/frontend/package inputs
and installed/deployed artifacts remain bound to the frozen sources.

Remaining cells are explicit restrictions or unverified configurations, not
silent passes: standard session auto_explain, active hints/HypoPG, complete
provider-log error SLOs/live host admission, App raw-plan payload cache,
App-principal workload maintenance, split-database maintenance retention,
optional legacy machine/MCP/agent protocols, broader versions/topologies/HA and
endurance. Working alternatives and evidence are in the current matrix. No
claim of universal feature parity or Claude review is made.

## Implemented baseline: dev.4 (candidate-specific evidence)

The [dev.4 acceptance matrix](2026-10-30-v2.5-dev4-acceptance.md) records its
version-specific feature disposition; final dev.5 results are above and in the
current acceptance report. The dev.3 section below is historical, including its zero-row and
missing-bridge findings. Those findings drove this iteration; they no longer
describe the current implementation.

- **Controlled telemetry is verified.** All ten native streams delivered;
  corrected strict plan/log/DDL acceptance has 24 pass events, zero failures or
  skips. Long application tags were truncated to 31 characters in provider
  plan/DDL metadata. A short unique exact tag removed that false negative.
- **Actual provider plans reach durable SRE evidence.** Two owned provider plans
  passed SDK read→scoped durable inbox→production plan-cache consumer→historical
  adapter→committed evidence/hash/event chain, with exact cleanup. The native
  integration uncovered a 500 ms whole-read deadline bug; the bounded5 s operation
  with 500 ms backend statements now passes in 1.23 s. Raw failures remain retained.
  Final App acceptance also found that filtering query IDs after the row cap
  hid older matching events behind monitoring rows. The predicate now precedes
  the cap; a pre-fix failing 130-distractor regression and rebuilt native
  provider-plan/history/hash-chain check pass. Fresh complete tests and the
  App model-selected historical citation target that fixed source. The final App
  now retrieves and cites the exact controlled event through its real model tool
  loop; hashes, scope, idempotency and event chain pass with unchanged ledgers.
- **Five historical SRE/RCA adapters and the citable `read_history` tool are
  implemented.** Database OID, policy, retention and source/event boundaries
  remain enforced. Partial history cannot determine current root cause,
  action admission, live host capacity or a complete database error-rate SLO.
- **Scoped application SLO ingestion is implemented.** Databricks-authenticated
  administrators can submit explicit database/objective counters through the
  new samples API. Actor-bound series and a full-hash database namespace prevent
  cross-database durable collisions. Native burn/case/replay/restart/staleness
  acceptance passed. The patched App also passed fifteen real-time samples over
  seven minutes, both burn windows, one cited Gateway case, actual stale-to-unknown
  behavior, unchanged ledgers and exact synthetic cleanup.
- **Typed statistics maintenance is implemented and natively verified.** The
  operator supplies the hypothesis/columns and requests human approval. Physical
  identities, retained evidence and existing execution gates are rechecked.
  Verification is structural, not a performance/recovery claim; rollback cannot
  restore ANALYZE statistics. The App stays observation-only, and its principal's
  execution on monitored workload tables remains unproven.
- **Policy refusals are separated from platform limits.** All 16 formerly denied
  cluster/filesystem probes execute under an explicit test-only allowlist on the
  isolated branch (11 OK, 5 empty). Default App policy remains database-only. Empty
  topology rows do not prove positive incident diagnosis.

Hints and HypoPG still have provider restrictions. Provider logs lack a complete
aligned application-error denominator; historical host metrics cannot admit
live actions. Optional MCP/agent/legacy HMAC protocols remain authenticated 501
in Apps. Broader PG versions, HA/failover, every incident family and default-hour
SLO endurance are not certified by the current fixtures. These are explicit
matrix cells, not silently skipped functionality. No Claude review has occurred.

The new native findings and public-documentation cross-checks are in the
[provider report](2026-10-30-provider-compatibility-gaps.md) and
[maintenance review](2026-10-30-sre-maintenance.md). Current work changes no
workspace-wide preview, account PII membership, broad grants or private provider
settings. Gateway AI continues to use Databricks OAuth M2M, with no UG API key.

## Historical implementation update: dev.3

The approved development iteration is built on the existing branch. The original findings
below are the dev.2 baseline, not a claim that the new connector remains missing.
The current build adds durable scoped telemetry, captured-plan consumers,
Databricks-only Apps AI authentication, enforced SRE read policy, independent
retention/revocation and sanitized deployment/source APIs.

Native SRE acceptance passed a real Gateway investigator (2 model calls,
4 transcript steps, 4,511 tokens), owned lock/idle and advancing-sequence diagnoses, and a
signed diagnostic runbook. Source policy classified 38 identities, including
19 denied/unexecuted sources. The final deployed App independently passed its
real investigator (2 calls, 3 new cited database probes, 4,558 tokens), evidence
hashes/event chain and idempotent lifecycle. Action/queue ledgers remained unchanged.

All ten resource-scoped telemetry SDK/schema reads passed against the API-verified
`<test-project>/<native-test-branch>` resource and actual database OID. A controlled workload with
verified query ID still yielded zero plan/log/DDL records. Native provider delivery,
masking and latency remain unverified. This is an acceptance gap, not successful
end-to-end auto-explain capture. See the updated telemetry and SRE reports.

Remaining functionality includes automatic historical logs/ASH/DDL integration
into RCA/migration/SRE, event-window SLO ingestion with honest coverage, and a
SRE-to-maintenance execution bridge beyond existing cancellation support. Native
coverage does not extend to every incident family. Provider limitations on
hints, HypoPG, host administration and OAuth-only broker logins remain.

Final artifacts, deployment and exact evidence composition are recorded in the
[dev.3 acceptance report](2026-10-29-v2.5-dev3-acceptance.md). Final deployment
`01f1d472793f135aa1802c4e4ef7c979` is RUNNING/ACTIVE. Both binaries retain
libpg_query; 2,107 production/embedded/package source fingerprints match.
The local full-suite-plus-reruns composite has 16,271 passing test events,
50 explained skips and zero remaining failures; native Lakebase has 46 passes
and three explicit telemetry-delivery skips. Frontend has 651 passes.

No broad UC/PII grant was introduced. The selected existing warehouse is attached
to the App; all six default metadata streams passed fresh App-principal reads,
with zero returned history rows. Databricks-only sign-in and pinned OAuth M2M,
the real Gateway call and final browser passed. Unavailable MCP/agent UI flows
are hidden with truthful direct-route guidance.

### Findings disposition for independent review

| Findings | Dev.3 disposition |
| --- | --- |
| F01 | Native database/planner/operator behavior preserved; restored full native run passed |
| F02 | All ten bounded readers, durable scoped history and normalized APIs implemented; native schemas passed, delivery still unverified |
| F03–F04 | Selected-resource capture/delivery remains zero despite enabled provider switches; standard auto_explain and provider capture remain separate |
| F05 | App warehouse/metadata reads verified; unmasked payload access and masking behavior remain unverified |
| F06 | Replay/reconciliation, event-time retention, source epochs and durable scope/policy fencing implemented and locally tested; no exact occurrence/completeness guarantee |
| F07, F09 | Historical logs remain diagnostic-only; automatic historical SRE/RCA and event-window error SLOs remain incomplete |
| F08 | Explicit scoped optimizer consumer implemented; unsafe legacy consumers exclude provider rows; automatic provider regression comparison withheld pending trusted context integration |
| F10 | Dedicated history runtime starts independently of managed mutation support; asynchronous metrics cannot admit live actions |
| F11 | Both native and deployed real-model investigator/evidence lifecycles passed; two native incident families and signed diagnostic runbook passed |
| F12 | Database read policy enforced below initial/model/background/runbook/resumed reads, with explicit denied/unexecuted sources |
| F13 | Sanitized effective/source status APIs and observed manual worker/collector execution verified; automatic investigations/SLO/runways/proposals/sinks remain disabled |
| F14 | Provider restrictions on hints/HypoPG/pgAudit/host administration remain; no private loader or broader role grant introduced |
| F15 | App OAuth M2M pinned and observed; AI keys/foreign origins rejected; optional unsupported protocols explicitly 501 after platform auth |
| F16 | Local, native, deployed, provider-refusal and skipped results recorded separately; initial failures retained |

This does not close every work item in the broader plan. The historical consumer,
SLO and maintenance bridges, provider-delivery acceptance and broader topology/
version/endurance coverage remain the next promotion gates, as detailed in the
acceptance report. No Claude review has been performed.

## 1. Objective and dev.2 baseline at plan approval

Close every feasible Lakebase, Databricks Apps and Unity AI Gateway integration and validation gap. Publish an evidence-backed capability matrix instead of calling a successful build or local suite “full Lakebase support.” Provider restrictions remain explicit where application changes cannot remove them.

Baseline installed/deployed version: **2.5.0-dev.2**. App: development App (deployment URL omitted). Workspace: `<deployment-url>`, explicitly selected CLI profile `<test-workspace-profile>`, workspace ID `<workspace-id>`. Existing project: `<test-project>`. Persistent App branch/database: `<development-resource>` / `pg_sage`. Native test branch/database: `<native-test-branch>` / `pg_sage_test`, originally created with a 48-hour TTL; revalidate existence/expiry before reuse.

Final dev.2 deployment: `01f1d3d41751196ea9b23119bf897d8c`, RUNNING/ACTIVE at acceptance. Linux SHA256: `7f6656d2244e644b2f3519c17613649ad78dfc7f7212bbf7ba01ebe28521ae85`. Installed macOS SHA256: `32f4fbaac3f0ab0e7aafa18271726055305195d723ab80bb48630fbb32c16ccf`. All 1,768 saved production/embedded source fingerprints matched that build. The original plan-preparation step added documentation only; the dev.3 implementation is recorded above.

Dev.2 uses Databricks sign-in for the browser and ordinary REST APIs, with no separate pg_sage password or local session authentication. Optional programmatic protocols still have the gaps in section 6. The original admin remains ID 1; its password is NULL and old sessions were removed. The task-created bootstrap password resource/secret was removed. Postgres, encryption and model resources remain. Observation trust and existing action approvals remain in force.

## 2. Baseline findings that drove the work

| ID | Finding and evidence | Consequence / work type |
| --- | --- | --- |
| F01 | Native PG18 migrations, durable collection/findings, OAuth/TLS/refresh, generic EXPLAIN, five Gateway advisor workflows and eight scoped executor contracts passed | Preserve working behavior; extend native regression coverage |
| F02 | No runtime reader for `system.lakebase` plans, logs, ASH, DDL or compute metrics | Missing implementation, not merely a missing test |
| F03 | Both documented exporter/log-capture switches and separate provider plan capture were already on; fresh account-wide telemetry exists, but selected `<test-project>`/pg_sage resource queries returned zero rows | Resolve selected-project coverage before claiming successful telemetry enablement |
| F04 | `auto_explain` is preloaded, standard `log_min_duration=-1`, and the tested role cannot SET it. Separate `neon_monitor` plan capture is on | Standard auto_explain logging and provider capture are different paths; do not prescribe ALTER SYSTEM or infer provider capture is off |
| F05 | System-table access is account-wide at schema scope; plan/query/parameter fields can be masked. App-principal UC/warehouse access has not been verified | Separate authorization and masking work; PostgreSQL/model grants do not provide this access |
| F06 | Provider delivery is asynchronous with no documented ingestion SLA; retention is seven days. Compute ID changes after resume | Durable late-arrival handling, event-time semantics, gap reporting and compute epochs are required |
| F07 | Recorded `neon_monitor.log_capture_min_level=warning` | A logs reader does not prove delivery of LOG-level checkpoint/temp-file/standard-auto_explain events |
| F08 | Current optimizer cached-plan gate/source list excludes a new Lakebase source; plan pairing selects newest plans without sufficient provenance separation | Reader-only work would leave consumers incomplete or create misleading plan comparisons |
| F09 | Current SLO errors proxy drains newly arrived log errors and divides by the current direct-SQL transaction delta | Reusing it for delayed logs would misalign numerator and denominator; requires event-window design |
| F10 | Cloud polling is coupled to the presence of a managed-parameter resolver | A read-only Lakebase telemetry source must start independently; it must not imply instance mutation support |
| F11 | No completed native SRE investigation through real Gateway with verified tool calls, evidence, citations and lifecycle readback | Native collector/advisor success does not establish investigator/SLO/runbook coverage |
| F12 | The 32-probe SRE catalog contains database-local, cluster-wide, filesystem, topology and log-derived sources. The initial operator plan already reads connection saturation; HTTP start has no enforced narrow scope | Enforce scope below every reader before running database-only acceptance; model instructions cannot provide isolation |
| F13 | Source defaults permit several automatic SRE/SLO/runway/change/proposal paths. Existing deployment evidence lacks a sanitized effective SRE configuration snapshot | Observation trust alone does not prove background readers/investigations are disabled; inspect effective state first |
| F14 | PG18 pg_hint_plan 1.8.0 installs but hook is inactive; LOAD returned 0A000 and session preload SET was unavailable. HypoPG is absent. pgAudit install returned 42501 | Provider/role restrictions; telemetry does not activate hints or hypothetical indexes |
| F15 | App/general AI uses Databricks unified auth with platform-supplied SP credentials, but the SDK provider is not explicitly pinned. Optimizer overrides can select API-key semantics. MCP still requires an inner credential | Pin/verify App OAuth and tighten Databricks-only optional AI/programmatic paths |
| F16 | Current evidence includes local scripted/fake-model tests and successful provider-refusal tests | Record these separately from native functionality and real-model acceptance |

At approximately 19:39 UTC, aggregate-only seven-day queries found 439,971 account-visible plan rows, 1,486,254 log rows and 3,198,561 compute-gauge rows, with zero matches for the selected project/branch identifiers. These counts prove the surface was receiving data elsewhere, not selected-project capture or App-principal access. No telemetry settings/grants were changed.

Coverage baseline: 1,154 local SRE pass events and five skips include scripted probes/fake models; there is no completed native investigator case. Final dev.2 frontend tests passed 646 tests; full changed auth/API/command packages passed separately, followed by focused host-format regressions and live App acceptance. The older 16,096-event Go composite predates that auth change. These are distinct evidence streams, not a fresh full native suite.

The existing detailed reports contain exact observations, SQLSTATEs, test counts and retained unsuccessful attempts; see section 12. No failed or skipped case is relabeled as native functional success.

## 3. AI authentication: direct answer and required invariant

**pg_sage is not using a Unity Gateway API key.** Deployed settings use `llm.auth=databricks`; the general `llm.api_key` was empty at live acceptance. Calls target:

```text
[deployment URL omitted]
model: system.ai.gpt-6-sol
```

- Local `<test-workspace-profile>`: Databricks CLI user OAuth, stored in the OS keyring. Profile metadata has `auth_type=databricks-cli`; it has no PAT/token/client-secret fields.
- Databricks App: platform injects App service-principal OAuth credentials, and pg_sage uses the official SDK unified-auth path. The App has model-service EXECUTE permission. AI is configured for the shared App principal, not user on-behalf-of authorization. The current client does not explicitly pin the SDK provider and acceptance did not record its resolved provider; add that verification/enforcement below. No evidence indicates a UG key was used.
- Browser sign-in, outbound Gateway auth, Lakebase database-token minting and future warehouse/UC authorization are separate flows. They can use Databricks identities without sharing permissions.
- Credentials are restricted to the configured HTTPS workspace origin, including port; native LLM redirects are refused. Retries call SDK authentication again, which may reuse a valid cached token rather than minting one. HTTP 401/403 currently remain terminal; they must not trigger a key/PAT fallback.
- Upstream model-provider credential configuration is separate from pg_sage client authentication; this work has not inspected or changed it.

Existing source: `sidecar/internal/databricksauth/auth.go`, `internal/llm/client.go`, `internal/llm/tools_wire.go`, `internal/config/llm_wire.go`, `cmd/pg_sage_sidecar/llm_client_registry.go`, `cmd/pg_sage_sidecar/sre_model_wiring.go`, and `deploy/databricks-apps/configure.py`.

**Target invariant:** the Databricks Apps deployment profile uses Databricks authentication for every supported AI and application caller path. No dedicated UG/model-provider API key or pg_sage login token is required. Stored RBAC, action approvals, protocol scopes, replay protection and message integrity remain authorization/integrity controls; they must not become alternate authentication bypasses. Standalone deployments can retain their existing provider options.

## 4. Delivery sequence and gates

| Workstream | Priority / dependency | Deliverable and gate |
| --- | --- | --- |
| A. Baseline, effective state, scope and capability inventory | P0, first | Reviewed fixture scope, complete source/probe inventory, explicit effective config, safe native harness |
| B. Databricks-only auth across optional AI/programmatic paths | P0; independent of telemetry visibility | No silent key fallback; real caller/routing acceptance or explicit unsupported caller classification |
| C. Telemetry coverage and permissions | P0 discovery; in parallel with A/B | Selected-resource event readback and local/App-principal permission/masking results |
| D. Durable telemetry reader and observed plans | P1; offline work can proceed before C passes | Crash-safe ingestion and truthful plan provenance; native promotion requires C |
| E. Logs, historical signals and SLO semantics | P1/P2; depends on D foundations | Each source/signal accepted independently; delayed data never becomes healthy live evidence |
| F. Native SRE tool loop, incidents and runbooks | P1; direct-SQL baseline follows A/B without waiting for telemetry | Real Gateway lifecycle, known incident diagnoses, source-aware abstention, no unintended actions |
| G. Lifecycle/version/topology/action breadth | P2, incrementally after preceding gates | Per-identity/version results, recovery/soak evidence and explicit remaining provider restrictions |
| H. Release and capability report | After required gates | New development revision, reproducible artifacts, deployment acceptance and honest supported matrix |

Do not let zero project telemetry block offline connector work or direct-SQL SRE acceptance. It does block claiming live telemetry success. Implement as small reviewable changes by workstream, rather than one unreviewable compatibility patch.

## 5. A — effective state, scope and native test foundation

1. Freeze the current source/artifact baseline and record sanitized effective configuration after persisted overrides: AI routing/auth; SRE enabled/automatic starts; runway investigation; SLO sources/opening; change readers; proposals/approval requests; external sinks; runtime component status. Do not infer disabled components from missing `/config/global` fields. Revalidate branch TTL and ownership.
2. Add trusted per-runtime read policy: registered database UUID, immutable provider binding, allowed source/probe scope, optional statement identity, policy version/hash. Requests may narrow it, never widen it. Persist the policy with investigations and enforce it again on resumed work.
3. Put enforcement beneath SQL acquisition/execution for `Runner.Run` and `RunBackground`, not solely in API decoding or the tool schema. Cover deterministic triage, model tools, statement EXPLAIN, model review, runbooks, background signals/runways/SLO/changefeed/pooler adapters and shared-history queries. Direct freeze/WAL custodian scans also execute outside `probes.Runner`; route them through the policy boundary. Inventory the independent action-verification runner and reject out-of-scope backend reads before observation, not after later database checks. Filter advertised tools too, with an execution-time check retained.
4. Classify every probe as database-local, cluster, filesystem/provider, history or external. A database predicate must not silently change a cluster-saturation probe's causal meaning. Split a valid local attribution probe from a cluster observation, or return explicit policy-denied evidence.
5. Before fixtures, explicitly disable unrelated background/external paths and verify runtime state; `automatic_start=false` alone is insufficient because runway investigation has a separate switch. No unintended action proposals, approval requests, ChatOps messages, notifications or execution are triggered by read-only acceptance. Diagnostic narrative/manual recommendations within the fixture investigation remain allowed.

Touchpoints: `internal/sre/probes/{types,registry,runner,classify}.go` plus new policy implementation/tests; `internal/sre/{investigator_tools,investigator_plan,investigator_explain,model_probe,plan,signals,worker,runbook_run}.go`; `cmd/pg_sage_sidecar/{database_runtime_sre,database_runtime_sre_actions,database_runtime_signals,database_runtime_runway,sre_signals,sre_runway_advisor}.go`; `internal/autonomy/{freeze_postgres,wal_postgres}.go`;  configuration lifecycle/generated metadata; durable request/record/create/scan/export paths; `internal/api/sre_operator_handlers.go`.

Acceptance: two synthetic local databases demonstrate no cross-database marker/session/history leakage; denied sources never execute in initial, model-selected, background, runbook or resumed paths. A policy change cannot silently widen a running case. Distinguish policy denial, provider refusal, unavailable privilege, timeout and successful empty data.

Add opt-in native files under `sidecar/tests/lakebaselive/`: `sre_preflight_test.go`, `sre_probes_test.go`, `sre_investigator_test.go`, `sre_incidents_test.go`, `sre_signals_runbooks_test.go`, `sre_recovery_test.go`. Reuse verified TLS and the production OAuth pool hook; do not point destructive raw `testdb.TestMain` fixtures at existing Lakebase databases. A fresh database on a cloned branch is not proof of cluster isolation.

## 6. B — finish the Databricks-only contract

- General chat, structured JSON, tools, retries, discovery, advisor, optimizer/tuning and SRE must each have explicit effective auth/routing metadata and an actual workflow test. `llm.auth=databricks` selects unified auth, not OAuth specifically: pin/verify SDK `oauth-m2m` in the App profile and retain local `databricks-cli`. The default SDK chain can try PAT/basic before M2M if not pinned. Test that stray PAT, basic/profile or API-key settings cannot change the App principal; report only safe provider/principal metadata.
- Replace the optimizer's implicit endpoint-override-to-API-key behavior with explicit routing/auth validation. In the Databricks-only App profile, reject API-key modes/keys and unauthorized origins through YAML, environment and persisted/API overrides; fail with a useful configuration error rather than silently falling back. Test inherited and dedicated optimizer routing separately. Preserve standalone configuration compatibility.
- SRE uses the general client when enabled; replace its API-key-only setup guidance. A successful general advisor call does not establish SRE tool-loop success.
- For supported programmatic App callers, authenticate once through verified platform identity, then explicitly map it to a stored agent principal, sponsor/OBO policy, permitted task/databases and protocol scopes. A user role alone is not a replacement for the existing MCP grant model. Preserve retired-principal refusal and audit attribution. Remove the mandatory inner pg_sage bearer/token layer only after equivalent authorization exists in the Databricks-only profile; disable local token issuance there. Standalone MCP retains its current contract.
- First establish the platform's service-principal identity/header and Authorization forwarding contract. Do not fabricate an email for a bot or accept a caller-supplied identity header. If a caller type cannot be securely represented by supported Apps ingress, classify it as unsupported; do not bypass ingress or quietly add a static key.
- Inventory MCP, specialist, signed ingest, SLO/change ingest and admin routes. The currently deployed MCP route has both the Apps identity gate and its own bearer contract; no live bot/SP compatibility claim exists yet. Current SLO/change-ingest HMAC is authentication and does not provide per-caller role/database authorization. Replacing it requires platform-principal-to-source/database permissions and explicit control of deployment-wide events, plus replay protection. Optional HMAC would still require a second shared secret; do not describe that as a Databricks-only path. Retain HMAC only for separately documented standalone/explicit-exception configurations.
- Test OAuth renewal and permission denial separately, including SDK refresh, cold start, temporary auth failure, terminal 401/403 and workspace-origin changes. The SDK client/error is cached with `sync.Once`; workspace/auth-source changes require restart unless deliberately redesigned. A historical 65-minute soak is not exact mint/expiry or revocation proof.

Touchpoints include `internal/api/{databricks_app_auth,mcp_oauth,router}.go`, the protocol handlers, `internal/config/llm_wire.go`, `internal/llm/`, `internal/databricksauth/`, client registry/SRE model wiring, Apps renderer/launcher and the safe effective-config API/UI. Keep the current owner binding, per-request roles, cross-origin checks and standalone auth regressions.

Gate: browser and each supported programmatic caller use Databricks auth without an inner login credential; each AI subsystem makes a billed real Gateway request with no provider/UG API key. Wrong-origin, revoked/denied and unsupported identities fail closed. Record App-SP versus user/OBO semantics explicitly.

## 7. C — selected-project telemetry and access preflight

1. Read current preview/project status and resolve the actual resource mapping. Known project UID: `<project-id>`. Verified branch identifiers: `<branch-id>` (test), `<branch-id>` (dev). Resolve endpoint/compute identifiers from current API metadata instead of guessing names.
2. Confirm table schema and filter columns. Do not depend on a populated `workspace_id` or project display name; bind the SDK workspace origin to verified project/branch/endpoint/database identity. Record any unmappable row as such, rather than broadening to account-wide payload reads.
3. Execute one harmless, externally tagged synthetic workload in the task-owned test database; record time, application tag, query identity and provider scope. Establish a matching aggregate, then retrieve only that synthetic plan/log payload. Compare both pg_sage branches. Capture/sampling may mean one execution is insufficient; use bounded attempts and retain negative observations.
4. Both documented GUC checks were already on. Do not blindly restart, ALTER SYSTEM, call private capture functions or reconfigure a legacy destination. If visibility remains zero, produce a sanitized diagnostic packet with resource IDs, query IDs/times, flags and results for provider investigation; no Slack/ticket is sent automatically.
5. Validate separately for local user and actual App principal: warehouse access, UC object access, masking, query result limits and SDK credential renewal. Default to metadata-only telemetry. Unmasking must be an explicit reviewed authorization choice, not automatic `databricks_pii_access` membership.
6. Prefer a governed resource-scoped read object if supported and validated. A SQL WHERE filter minimizes retrieved data but does not create UC authorization isolation when the principal can read the account-wide base schema. Record actual exposure before adding grants. Database/model permissions do not imply warehouse/UC permissions.

Gate: a controlled event is visible for the exact selected resource, access and masking behavior are known for both identities, and failure is classified as disabled capture, permission/masking, resource mapping, delivery gap or unresolved provider behavior. Do not infer enablement from account-wide rows.

## 8. D — durable reader and observed-plan integration

### Reader and persistence contract

Create a typed `internal/lakebaseobs` integration; do not force structured tables into the existing Supabase/Prometheus log-only API. Reuse official SDK unified auth, exposing only a narrow internal warehouse client from `databricksauth`. Use SQL Statement Execution with fixed verified identifiers, parameterized values, bounded selected columns/time windows/rows/bytes, polling, cancellation and a cost/query-rate budget. Do not follow arbitrary result URLs with workspace credentials.

Proposed configuration: optional `lakebase_observability` section with enablement, warehouse ID, fixed source object bindings, explicit registered-database/provider resource bindings, poll interval, page/byte/query budgets, replay/reconciliation policy and retention. No credentials in configuration. Initially restart-bound; register lifecycle metadata and safe effective status. Do not infer a project's telemetry binding solely from DNS or silently discover every account project.

Suggested interfaces, to finalize during review:

```text
Reader.ReadPage(context, stream, ResourceScope, TimeWindow, pageToken) -> Page
Store.CommitBatch(scope, stream, cursorBefore, events, cursorAfter) -> receipt
Consumer.Deliver(eventID, typedEvent) -> durable acknowledgement
```

Control/inbox/cursors live in the metadata/control pool, keyed by a stable installation UUID (persisted across Apps redeployments, distinct from the SNAPSHOT deployment ID), registered SRE database UUID, immutable provider scope hash, stream and schema version. Existing observed-plan cache remains in the monitored database initially. Make history placement explicit: `history.store=meta` currently moves snapshots/query store, not every explain-cache consumer. Do not partially move plan storage and strand reads/retention.

Commit inbox events and cursor atomically, then fan out with durable per-consumer acknowledgements. Use stable provider event ID if available; otherwise define a canonical identity including resource, compute/session, event time and payload identity. A hash cannot distinguish two genuinely identical events without a provider occurrence identifier: document the limit and do not claim exact error multiplicity. Use cursor compare-and-swap/fencing for concurrent workers. Sink-side uniqueness must use provider occurrence/event identity plus resource, including control-pool-to-monitored-plan-cache writes: a crash after sink commit and before inbox ACK must replay harmlessly. Existing query/time/JSON dedup is not the new structured-provider identity contract. Restarts, redeployments and concurrent workers must not double-count accepted events or lose committed cursors. Resource-binding changes create a separate scope; compatible schema upgrades explicitly migrate cursor state rather than silently resetting or reusing it for another resource.

Event-time cursors alone are insufficient. Handle equal timestamps, out-of-order/late rows, overlapping reads and old-compute delivery after resume. Add bounded rotating reconciliation across the available seven-day source window, not a fixed small overlap with silent late-event drops. A reconciliation budget/liveness test must show the full retained horizon can be swept before rows expire at the intended event rate. If backlog or cost limits prevent that, surface a coverage gap and unknown counts. Test equal-timestamp records split across page boundaries. An outage beyond retention records an irreversible gap. Query-through time is not a provider completeness watermark; a successful empty poll does not prove capture completeness.

Store `occurred_at`, `received_at`, provider scope/compute epoch, event/schema version and provenance. Keep raw query/plan/log payload retention at most seven days initially, configurable downward, with explicit size limits and deletion. Define expiry by original event time (not a fresh seven days on delayed receipt), with a receipt-time cap as well. Apply it to every managed payload copy: inbox, plan cache, subscriber evidence and export caches; review exports omit raw SQL by default. Retain payload-free dedup identities and consumer ACKs through at least the full replay/reconciliation horizon plus clock margin so payload deletion cannot cause replay double counting. Test retention/reconciliation boundary races. Longer-lived safe summaries have their own documented policy; do not inherit a 90-day raw SQL retention accidentally from another feed. Redact operational logs and review exports; never export tokens or customer SQL as routine evidence.

### Plans and all consumers

Normalize `plan_history` structurally, with source `lakebase_plan_history`; do not manufacture auto_explain log text. Verify actual object/array shape with a controlled native payload. Preserve query ID, provider fingerprint, duration provenance, original capture time, analyze/buffer/timing flags, resource/compute and available parameters only under approved data policy. Reject/classify masked, NULL, malformed, truncated, oversized, missing-ID and nonfinite values. Preserve signed/unsigned query-ID representation consistently. Exclude pg_sage monitoring by application identity and query tags.

Extend observed-plan storage/provenance rather than labeling every captured plan as actual analyzed execution. Update source selection/gates in optimizer, analyzer comparisons, query-store attribution, SRE regression evidence, retention and API/UI display together. Do not compare a generic estimate with an observed analyzed plan as a like-for-like regression. Do not attach a delayed plan to the newest counter sample or infer exact historical parameter bindings.

Touchpoints: new `internal/lakebaseobs/`; schema migrations; `internal/autoexplain/provider_log.go` plus structured-provider normalization; `internal/optimizer/plancapture.go`; `internal/analyzer/plan_pairs.go`; query-store collector/recorder and history-store routing; `internal/sre/probes/catalog_maintenance.go`; runtime provider wiring; `internal/firstlook/` and `internal/fleet/` capability reporting.

Gate: controlled tagged plan is read, persisted and consumed with correct provenance in pg_sage. Generic fallback remains available and clearly labeled. Crash/replay, scope switch, duplicate/late rows, masking, schema drift, cancellation and retention tests pass. No native telemetry claim until selected-resource readback passes.

## 9. E — logs, SLOs, history and resource evidence

**Logs/RCA:** map verified structured rows into `logwatch.LogEntry`, retaining event time, scope and SQLSTATE, then use typed durable fanout. The current provider log sink feeds RCA/plans; it does not automatically supply the SRE error counter or every migration subscriber. Wire each subscriber and acknowledgement explicitly through `database_runtime_logs.go` and `sre_signals.go`. Accept errors, deadlocks, migration signals, checkpoints and temp-file signals separately. At capture minimum `warning`, absent LOG events are unavailable coverage, not healthy absence. Do not change unsupported provider settings to force a green result.

**Error SLI:** replace arrival-time draining for this source with aligned event windows and a matching transaction denominator/epoch. Use provider transaction intervals or persisted SQL intervals only after their definitions and alignment are verified. Sampling, dropped log levels, outages, duplicates and unestablished completeness leave the window unknown/partial. Do not generate a healthy zero-error SLI from an empty asynchronous poll. Add availability/provenance gating to log-derived `autovacuum_cancellations` too.

**ASH/waits:** add bounded historical context with provider sample semantics and compute/session identity. Sampled history is not a live lock graph or complete session trace; PID reuse must not merge backends.

**DDL:** use a trusted internal provider connector for `lakebase_ddl_history`, preserving occurred/received time and bounded object/actor summaries. Do not enable arbitrary external submission of a privileged DDL event kind. Keep pg_sage's own action/config ledger; external/provider history supplements it. Surface documented resume/capture gaps.

**Metrics:** ingest compute/database counters and gauges with their documented semantics. Windowed counters must not be differenced as cumulative totals; gauges are point readings. Keep system-table CPU/memory/storage metrics **historical-only initially**. No documented ingestion SLA establishes suitability for current maintenance admission. Keep a separate historical runtime/status by default: placing delayed samples in `rt.cloud` can also feed `rt.hostMemory` into advisors/tuning even without a managed-parameter resolver. Test CPU, advisor/tuner memory and every capacity/withhold consumer remains unknown unless qualified for that specific purpose. Never wire these samples directly to `WithHostCPUReader` or free-space safety guards merely because they arrived recently. Qualify event age, provider semantics, compute identity and controlled delivery latency before any later guard proposal. Logical database size, cache and ephemeral storage are not durable free space.

Decouple telemetry startup from the managed-parameter resolver in `cloud_telemetry_wiring.go`. A `cloudtel.Source` may exist without a mutation resolver. Leave Lakebase instance managed-parameter actions unsupported. Test existing RDS/GCP/Supabase adapters for regressions.

Gate: each stream has source-specific freshness/coverage status, controlled native readback, idempotent subscriber delivery and negative access/masking tests. Unknown remains unknown. No delayed historical metric admits a live action.

## 10. F — native SRE acceptance and functional repairs

### Full probe inventory

Generate the matrix from `probes.Catalog()` and fail if the registry and checked inventory differ. Include these 32 probes, classified individually before execution:

| Group | Probe IDs |
| --- | --- |
| Locks/transactions | `lock_chains`, `lock_graph`, `long_transactions`, `prepared_xacts`, `backend_identity` |
| Connections | `connection_saturation` |
| Replication/WAL | `replication_lag`, `replication_slots`, `wal_checkpoint` |
| Maintenance/plans | `autovacuum_wraparound`, `vacuum_progress`, `plan_regressions` |
| Changes/archive | `archiver`, `sage_actions` |
| Checkpoint/temp/standby/waits | `checkpoint_activity`, `temp_file_activity`, `temp_file_holders`, `temp_spill_statements`, `standby_replay_state`, `lwlock_waits` |
| Runway | `xid_runway`, `wraparound_tables`, `xmin_horizon`, `autovacuum_cancellations`, `wal_runway`, `cluster_database_size`, `wal_directory`, `sequence_runway`, `runway_trends` |
| Statistics | `stat_database`, `stat_tables`, `stat_statements` |

The 32 IDs are the regular diagnostic catalog, not the complete read surface. Separately inventory and scope action probes `signal_target`/`recovery_sample` (including their independent runner), signal adapters `change_feed`/`slo_status`/`pooler_pools`, `explain_statement`, custodian reads and history/external sources.

For each identity/version record eligibility, whether SQL executed, policy reason, role/privilege facts, probe version, status/SQLSTATE, rows/bytes, duration, source scope/epoch and cleanup. Required database-local probes must return expected typed fixture data. “Any error accepted” is not a functional pass. Missing privilege, managed unsupported function, wrong topology, policy refusal and empty success are separate outcomes. Cluster/filesystem probes require verified authorized cluster scope; otherwise classify as unexecuted by policy.

### Real investigator, then known incidents

First instantiate the production coordinator, Postgres store, runner, statement explainer and real `llm.Client` in the native harness. Then repeat through deployed App runtime/API. Verify queued/claimed/collecting/evaluating/terminal lifecycle, real Gateway usage, at least two real model turns, at least one actual model-selected permitted read, durable tool transcript, evidence digest/content linkage, an accepted final claim citing that newly executed model-selected evidence ID/digest, event chain and idempotent request readback. Citations only to seeded deterministic evidence do not pass this gate. The action ledger remains unchanged for read-only tests. A valid inconclusive healthy-fixture result can pass lifecycle acceptance; deterministic-only fallback, rejected final output or missing transcript cannot satisfy the real tool-loop gate. Bound retries and retain unsuccessful attempts.

Add controlled fixtures incrementally: owned row-lock blocker/waiter; idle transaction; deliberately small sequence runway; bounded temp spill; then actual collected query/plan regression history, vacuum progress and bounded connection pressure. Use tagged sessions, expected object/backend identities and unconditional cleanup. Measure **detected**, **diagnosed** and **actionable** independently. Keep production causal thresholds; do not lower them merely to shorten fixtures. Dangerous wraparound, disk exhaustion, checkpoint storms or WAL retention remain replay-only until a suitable controlled topology is supplied.

### SLO, runbooks and remediation

Use signed synthetic application SLI input or a properly authenticated Databricks-only ingest path as designed in B; verify window/burn math, one scoped case, staleness, replay and missing-source semantics. Test application SLOs separately from database proxies. Exercise a typed, signed/versioned runbook with durable chosen path/evidence and rejection of unsigned/retired versions. Missing evidence must cause abstention.

Test stale evidence, reused PID, protected roles/apps, trust/policy denial, provider-disabled signals and recovery verification. Preserve the eight already validated executor contracts: `analyze_table`, `vacuum_table`, `create_index_concurrently`, `revert_created_index`, `reindex_concurrently`, `create_statistics`, `revert_created_statistics`, `alter_database_guc`. Current SRE action derivation supports `cancel_backend`, not those eight maintenance contracts. Treat an SRE-to-maintenance proposal bridge as a new, separately reviewed implementation deliverable with explicit identity, evidence, authorization, approval, execution, verification and rollback contracts. Until that bridge is accepted, keep maintenance executor acceptance separate and expect SRE cancel refusal on Lakebase. After the bridge and read-only loop pass, exercise one supported maintenance proposal end to end on owned fixtures. Do not enable cancel/terminate or other action families merely to satisfy a test.

Gate: at least one complete real-Gateway tool loop in both harness and App; expected diagnoses for supported synthetic families; explicit unknown/refused outcomes elsewhere; no unintended proposals, notifications, execution or customer-data reads.

## 11. G/H — breadth, operations and release criteria

- Run eligible native matrices on PG16/17/18 separately, with selected user and actual App principal. Include owned-object/schema visibility, quoted identifiers, partitions, arrays/enums, JSONB, numeric/time types and optional vector workloads where product paths consume them. Deep PG18 results do not certify other versioned views or datatypes.
- Extend vector acceptance beyond the existing three-query HNSW lab only with declared dataset/recall/performance baselines. Successful DDL does not prove workload benefit; HypoPG absence leaves hypothetical-index benefit validation unavailable.
- Accept pending/paused investigation restart, evidence/policy persistence, lease fencing, retry boundaries and duplicate-step prevention. Test Apps stop/start with the new telemetry reader and Databricks-only caller paths.
- Test direct endpoint idle/resume with source/server/compute epochs, counter resets, delayed old-epoch events and stale multi-sample invalidation. Existing reconnect/readback success is distinct from uninterrupted held-session survival.
- Repeat credential endurance for user and App principal, including object reads and uncached AI calls. Record exact mint/expiry where observable; otherwise label elapsed-time evidence accurately. Test denials/revocation separately. Record UC-claim-specific limitations.
- HA/failover requires supported topology and an authorized disruption mechanism; absent those, retain local/replay evidence and native-unverified status. OAuth through the built-in transaction pooler remains unsupported. Do not enable native passwords to bypass that boundary.
- Survey pg_hint_plan/HypoPG/pgAudit/pg_cron/pg_repack with current public documentation and selected-identity live behavior. If a documented supported activation path now exists, verify real forced-plan behavior—not extension listing. Otherwise retain provider restriction and a sanitized diagnostic; no private loader or superuser workaround.
- Keep direct filesystem/OS access, ALTER SYSTEM, brokered per-role SCRAM on OAuth-only projects, role governance and additional mutation families gated unless a separately reviewed supported path and native acceptance exist. Do not grant CREATEROLE or broad account permissions simply to make tests pass.

Before promoting the next development revision:

1. Every advertised feature/probe/caller has one evidence-backed status: verified, degraded/partial, permission-required, provider-unsupported, unverified or disabled. Include last tested version, principal, scope, source age and reason. Catalog installation alone cannot mark operational availability.
2. Core user workflows pass in App/browser/API with real Lakebase and Gateway: sign-in, collection/findings, estimates versus observed plans, investigation/evidence, supported proposal/approval/rollback and recovery. Optional unavailable sources are visibly unavailable.
3. Local regression/lint/build and necessary native suites pass; raw failures, skips and retries remain preserved. Re-run relevant full packages after changes; use a fresh broad run before release. Serialize embedded-asset generation and Go test/build enumeration to avoid the prior asset-file race. Count test events honestly; do not present a replacement composite as a green monolithic run.
4. Publish native JSON/Markdown matrices, fixture/cleanup inventory, source/binary fingerprints, resource/config changes and any unresolved provider findings. Include query/warehouse cost and polling/backlog health. Do not include credentials or customer query payloads in the review packet.
5. Deploy a new development version after implementation gates pass; verify actual UI/API/Gateway/telemetry/SRE behavior after restart. Keep encryption/state stable and a documented binary/source rollback. Rollback must account for new data/schema compatibility rather than assume a previous binary can read every new migration.

## 12. Evidence and source packet

Tracked findings and acceptance reports:

- [Advanced Telemetry findings](2026-10-29-lakebase-advanced-telemetry.md)
- [SRE functionality/probe/coverage audit](2026-10-29-lakebase-sre-coverage.md)
- [Native Lakebase results and fixture restrictions](2026-10-29-lakebase-native-tests.md)
- [Databricks Apps acceptance and auth change](2026-10-29-databricks-app-acceptance.md)
- [Full local suite, failed streams and corrected follow-ups](2026-10-29-full-test-suite.md)
- [Current development acceptance](2026-10-29-v2.5-dev-acceptance.md)
- [Initial compatibility survey](2026-10-29-lakebase-compatibility.md)
- [Lakebase configuration](../docs/lakebase.md) and [Apps configuration](../docs/databricks-apps.md)

Current public contract, checked against live observations:

- [Lakebase telemetry reference](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
- [Lakebase system tables](https://docs.databricks.com/aws/en/admin/system-tables/lakebase)
- [Lakebase release notes](https://docs.databricks.com/aws/en/release-notes/lakebase): old custom UC destinations no longer receive new telemetry; do not design a new integration around that retired path
- [Lakebase compatibility](https://docs.databricks.com/aws/en/oltp/projects/compatibility), [extensions](https://docs.databricks.com/aws/en/oltp/projects/extensions), [pooling](https://docs.databricks.com/aws/en/oltp/projects/connection-pooling)
- [Unity model-service query API](https://docs.databricks.com/aws/en/ai-gateway/query-model-services)
- [Apps authentication](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/auth), [forwarded headers](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/http-headers), [permissions](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/permissions)

Local raw evidence is ignored under `.local-test-results/`; selected telemetry evidence is aggregate-only. That directory also contains private auth artifacts: do not attach it wholesale to a review. Give a reviewer this plan, the tracked reports and relevant source/diff. Share only explicitly sanitized evidence needed to substantiate a finding.

## 13. Decisions and independent-review questions

The following design decisions remain exposed for Claude and human review against the implementation and its explicit remaining gaps:

1. **Scope:** Is enforcement below every reader complete, including deterministic/background/runbook/history paths? Does splitting a local probe preserve its causal meaning? How are policies pinned across resume and configuration changes?
2. **UC access:** Can a governed resource-scoped object provide useful unmasked synthetic evidence for the App without account-wide PII membership? What actual account exposure is acceptable if the Beta requires schema-wide reads? Lack of access must leave metadata-only/degraded capability.
3. **Persistence:** Is the proposed control-pool inbox/cursor plus monitored plan cache acceptable? If history centralization is required, update every reader/retention path atomically in the design. What raw-payload retention/data policy applies?
4. **Event identity/completeness:** Does the deployed schema provide a stable occurrence ID or ingestion watermark? If not, what multiplicity/late-arrival limitations must be exposed? Can any error-SLI window be called complete, or must this remain diagnostic historical evidence?
5. **Plan evidence:** Are query/resource/epoch/source/analyze flags sufficient for every comparison? Could a late provider plan be attributed to the wrong counter interval or mixed with a generic estimate?
6. **Live guards:** Keep asynchronous table metrics historical-only unless independently qualified. What supported fresh source, if any, can establish CPU/free-space/replica admission safely?
7. **Databricks-only callers:** What platform-verified identity represents service principals and MCP clients? Can agent/sponsor/task/database/protocol grants be mapped without a second login credential while retaining retired-principal refusal and audit identity? Does replacement ingest authorization bind callers to source/database/deployment scope? Which caller types must remain explicitly unsupported?
8. **AI routing:** Does the App profile pin/verify OAuth M2M and reject optimizer API-key semantics, validate optional optimizer-to-general fallback and SRE general-client inheritance, and preserve exact workspace containment? Are App-SP and user-OBO semantics clear? Which renewal/denial tests prove the actual pinned SDK path?
9. **Native acceptance:** Does the tool-loop gate prove model-selected execution and cited durable evidence, rather than deterministic fallback? Are every refusal, omission and source gap counted correctly? Is cluster/topology isolation actually established?
10. **Release language:** Challenge every “full,” “supported,” “actual plan,” “zero errors,” “current CPU” and “works on Lakebase” statement. Completion means a published supported matrix with explicit unavailable/unverified cells, not universal self-managed PostgreSQL parity.

Suggested review request: “Review this plan and linked findings against the current working tree. Prioritize correctness, missing integration consumers, scope/permission boundaries, async telemetry semantics, Databricks-only auth, native functional acceptance and rollback. Identify blockers and required tests with file references; do not infer compatibility from local test counts or extension installation.”

Review preparation: separate in-session auth, telemetry and SRE audits reviewed this plan; their material corrections are integrated. This does not represent a Claude review. That original plan-preparation step changed no runtime code, deployment, telemetry settings or grants; the subsequent authorized implementation/deployment is recorded in the dev.3 update.
