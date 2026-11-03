# Provider compatibility follow-up — 2026-10-30

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

## Final dev.5 native acceptance after the state-check upgrade

A fresh complete native invocation after both reviewed corrections passed
**79 test/subtest records, zero failures and two intentional opt-in skips in
435.223 seconds** (wall **440.727s**) on **2026-10-30 18:03:58–18:11:21 UTC**.
All eleven enabled roots passed, including the actual Unity Gateway investigator,
all sixteen explicitly authorized provider probes, the statistics maintenance
retention/rollback lifecycle and strict telemetry. Sources were frozen and
unchanged: Go/module SHA256
`3df43110e9f5de27cf43b8793fd874b48258b326f937acafe16b52265e8d5f35`.
The original dev.5 whole run remains red: its old nine-state check rejected
`uncertain`. The new production migration upgraded that check and the fresh
whole run exercised it without suppressing the failure or expanding supported
states. The separate backward-clock elapsed-time regression was also fixed
and validated before this freeze.

Evidence prefix `.local-test-results/gap-dev5-native-post-upgrade` contains the
untouched event stream, invocation summary, verification, 38-source matrix,
16-probe inventory, incident results, Gateway transcript and telemetry report.
Raw JSON SHA256:
`d2a1b16894f69b83942de12a35716030bf537703a2d76383df4b28ae64b778d3`
(**219,474 bytes**); stderr empty. The only skips are the strict all-extension
compatibility gate and the separate prior-run attribution diagnostic. Known
HypoPG / active pg_hint_plan / standard preload limitations remain incompatibilities;
passing the configured suite does not declare those features supported.

All sixteen provider statements completed: **eleven OK and five valid empty**.
The actual Gateway loop made **three model calls, four probes, 6,884 tokens**,
accepted final advisory/inconclusive output and persisted its transcript
(investigation `4dfc43a8-e51b-4c73-adc8-60ba98221985`). Two isolated incident
fixtures detected/diagnosed their owned idle-transaction blocker and near-limit
sequence; policy/manual restrictions continued to prevent autonomous action.

Strict telemetry passed **all 24 records**, with nonempty final metadata reads
in all ten streams. Controlled delivery returned **eight valid plans, one valid
log and one valid DDL event**. Maximum observed event-to-reader delay was
**34.563964s / 32.005494s / 36.044101s** for plans/logs/DDL, respectively; these
are observations, not latency or completeness guarantees. Exact tag
`lbo_4b4c66c4af2046678913bbe2`, original window
**18:10:24.663553–18:11:20.655008 UTC**. The unique telemetry schema cleanup was
verified. A fresh SDK read/classification of that same exact tag/resource/db and
original window returned **ten plans: nine valid, one intentional self-monitoring
exclusion, zero masked/malformed/other rejections**, StatementID
`01f1d48d-6ad2-1daa-9312-1befcf1e3fad`; no payload was persisted. Its excluded
event is `1b59f89db3fe201785c964028a5d976892b6741d61d47944a0b1752478032eff`,
query ID `-2668566451894465446`.

Actual typed maintenance executed, retained recent and uncertain fixture
evidence, completed typed rollback, and cascaded only the terminal owned
timeline, with its chain-head tombstone verified. The canonical validated state-check OID stayed
**270343** across the fresh Bootstrap. See
[maintenance proof](2026-10-30-sre-maintenance.md) for exact native identities.
App-principal DDL scheduler/citation and maintenance execution are separate
claims; native proof is not relabeled as App proof.

## Separate final dev.5 App DDL proof

The positive App DDL gap is now closed on final SNAPSHOT
`01f1d48e0748107d95b5bdd1ffad0e22`, independently of native evidence. A fresh
owned CREATE TABLE event at **19:39:05.046082 UTC** reached the actual App
scheduler at **19:39:50.873637 UTC**, was read through the authenticated history
API, and appeared in available hashed `lakebase_ddl_history` evidence cited by
accepted final case `ea555ad2-3ca6-48a3-bbcc-69d48f18ed9c`. Exact resource,
database OID, short tag, PID, command, object and original window matched;
metadata-only consent remained active. Event ID
`6a9390f2d66c28d816fa612173d27533dbec996b574125a6b1b52205b40f19c8`.
The 45.828-second event-to-receipt interval is one observation, not a guarantee.
The unique table/schema were removed with ownership/OID checks and no CASCADE.

The first attempt timed out and its cleanup also failed during a transport
interruption. Its original red report remains preserved; independent exact
cleanup recovery succeeded before the fresh complete attempt. All five final
App history adapters now returned positive, accepted cited observations. These
results establish neither complete telemetry, raw App payload-cache ingestion,
live incident/action authority nor App workload-table maintenance execution.
See the [dev.5 acceptance](2026-10-30-v2.5-dev5-acceptance.md#final-app-history-and-slo-evidence)
for case/evidence hashes and the full local/native/deployed result boundaries.

## Historical dev.4 follow-up and earlier diagnostics

The preceding complete native package invocation against frozen dev4 sources
passed **79 test/subtest records, failed none and explicitly skipped two** in
**453.537 seconds** after the optional query-ID history fix. It enabled strict controlled telemetry delivery, the real
Unity Gateway loop and all sixteen approved provider probes in one invocation.
The earlier 46-pass/3-provider-delivery-skip result in 353.262 seconds remains
preserved as historical evidence. Separate strict compatibility assertions
record the unavailable planner extensions as failures; this full run does not
reclassify those known incompatibilities as supported features.

**Current disposition:** the corrected strict native telemetry gate passed
**24/24 records without skips**, verifying usable controlled plans, logs and
DDL plus delivery in all ten metadata streams. The original payload failures
came from the provider's observed **31-character plan/DDL application label**;
a 28-character unique fixture label resolved them while preserving exact
payload predicates. The sole rejected plan in that earlier short-tag run was
independently classified as excluded self-monitoring traffic. The complete
preceding candidate run also records one rejected plan without independently
classifying it. A new exact-window re-read of the final fixed-source run
independently found nine valid plans and one self-monitoring exclusion, with
no masked, malformed or other rejection. A
separate read-only check normalized all **490 actual
metadata events** from the other seven streams without rejection and validated
1,864 metric values. Direct native normalization now covers all ten streams;
this does not establish durable consumers for every stream. The deployed App's
real scheduler separately persisted one exact tagged synthetic plan's metadata
and returned its matching event through the authenticated history API, with
payload absent by consent. All sixteen explicitly authorized provider probes
also completed successfully. A subsequent actual-provider-to-durable-cache
test found and fixed a **pg_sage historical-read 500 ms whole-operation
deadline bug**. The final native chain verified two real plans through durable
cache and historical SRE evidence, replay, provenance, nullable flags, plan and
evidence hashes, event chain and exact cleanup. Its first historical read
completed in **1,430 ms** on the final fixed source under a five-second
whole-operation budget with individually bounded statements. A separate App
acceptance exposed query filtering after LIMIT; the final fix moves the
parameterized signed/unsigned query-ID predicate before LIMIT, preserving
policy and retention fences. pg_hint_plan activation and HypoPG remain
observed provider feature gaps.

The preview-off UI observation below is a bounded prerequisite-state snapshot,
not a proven telemetry emission root cause or proof of the state throughout
the later runs. Actual delivered events govern the functional findings.

## Final fixed-source native acceptance after the query-filter fix

The preceding candidate's full run below remains valid for its own snapshot.
A subsequent deployed history request exposed another application bug: the
historical store limited the newest 101 rows before applying the requested
query ID. Newer unrelated/self-monitoring rows could therefore hide a retained
matching event and yield partial no-observation/truncation metadata. The fix
applies a parameterized query-ID predicate before ordering and LIMIT for plans
and ASH, matching both signed PostgreSQL bigint and equivalent unsigned uint64
decimal representations. Binding, time/retention, registered-owner locking,
payload policy, normalizer validation and bounded cancellation remain in place.

Independent review found no scope or semantic blocker. The regression fails
on the old implementation for both plans and ASH, with 130 newer unrelated
rows crowding out older requested observations. Fixed-source tests cover both
representations, no-query behavior, the 100-row cap/truncation, retained-window
limits, no match, invalid/unsupported arguments, pinned query policy and
payload-policy revocation. The full lakebaseobs race/coverage/union-tag package
passed in **9.545 seconds**, 78.6% statement coverage; artifacts preserve the
expected pre-fix failure and subsequent pass under
`gap-closure-history-query-filter-before-fix.log` and
`gap-closure-history-query-filter-final.log`.

After a formal source freeze, the rebuilt exact-owned provider helper passed
**16:46:04–16:46:26 UTC**, using only the earlier 16:34 fixture's exact short
tag, verified resource/database and original window. Ten actual SDK plans
yielded two selected slow-query occurrences, with zero existing cache rows for
their query ID **-3648692323471975236**. Two durable cache/history rows,
idempotent ACK/sink replay, provenance, nullable flags, plan hashes and durable
SRE evidence/hash/event-chain checks passed. The first filtered historical read
took **1,430 ms**. Case `04ae46f8-5e27-4b35-80e7-d9d35f9dff95`, evidence
`d4ae03ee-f54c-4c75-993c-4425e05432ea` and verified SHA256
`40d24cae5a6562312f19b771b38c4bc7be6627253b07db67d8a786098837dd36`
were cleaned with all exact scope/cache/inbox/cursor/owner records; absence was
verified. Payload was never written to report files. The helper did not call
Runtime.Poll or a model tool. Evidence is
`.local-test-results/gap-dev4-provider-sink-history-filter.json`, with empty
stderr and its separate rebuild/run manifest.

A **fresh complete** fixed-source native package then ran the same full command
shown below, with the same exact fixture/probe/Gateway opt-ins and intentional
extension/attribution exclusions. It completed **16:46:53–16:54:27 UTC**:

| Assertion | Final fixed-source result |
| --- | --- |
| Full package | **79 pass, 0 fail, 2 explicit skips**; 11 root passes |
| Package / wall time | **453.537 / 456.763 seconds** |
| Source | Before/after Go/module SHA256 `0878423b465ddcf7e4671fbcd8e3e09918bafd76af43a9d5b8c17f29f750526c`; unchanged |
| Exit / stderr | Zero / empty test and runner stderr |
| Native maintenance/SLO/incidents | All passed; owned fixture/rollback/cleanup assertions preserved |
| Database source matrix | 38 entries: 8 `ok`, 10 `empty`, 20 `unsupported`; policy refusals remain distinct from platform gaps |
| Explicit provider probes | All sixteen completed: eleven `ok`, five valid `empty`; no grants |
| Real Gateway case | `354ff955-20d6-4ea5-8b1d-b479a333b959`: two model calls, four tool calls (three probes), 4,484 tokens; accepted citation/hash/event chain, `inconclusive`, `stop=final` |
| Strict telemetry | **All 24 records passed**, no delivery skips; eight usable owned plans, one valid log, one valid DDL |
| All ten metadata streams | Nonempty final scoped reads; schema/read/delivery gates passed |
| Owned telemetry cleanup | Exact unique schema dropped and absence verified |

The final telemetry fixture used startup tag
`lbo_d5f3fa3ec7ee436690280658`, original bounded window
**16:53:30.302767–16:54:26.148148 UTC**, and a **57.60-second** strict root.
Its first controlled read returned nine plans: eight valid and one rejected,
with no masked rows. Maximum observed event-to-reader delays were
**32.686466 seconds** for plans, **30.066714 seconds** for logs and
**34.249656 seconds** for DDL. All ten final metadata streams delivered in
this fresh invocation; earlier runs are not substituted for these gates.

A **new independent read-only normalization** used that final run's exact
application/resource/database and **the same original window**, without adding
time or broadening the payload selector. It found ten now-delivered owned
plans: **nine valid and one intentional self-monitoring exclusion**, with zero
masked, malformed or other rejection. The excluded event is
`5cce1474593dfe016d5706e79cfc70d145d17887eff6a18ca0955014fa0d3c18`,
query ID **-2668566451894465446**. The additional valid row reflects the later
re-read and is preserved as its own count. This classification does not reuse
the earlier run's result. No raw payload was persisted; exit was zero and
stderr empty. Evidence:
`.local-test-results/gap-dev4-native-history-filter-plan-normalization.json`.

The final complete untouched JSON, summary, empty stderr and five unique
reports share `.local-test-results/gap-dev4-native-history-filter`: suffixes
`-sre-matrix.json`, `-provider-probes.json`, `-sre-incidents.json`,
`-sre-transcript.json` and `-telemetry.json`. The source/probe inventory is
refreshed from this same final invocation in the
[per-source matrix](2026-10-30-native-sre-source-matrix.md). The new typed
maintenance proof is appended to the [maintenance review](2026-10-30-sre-maintenance.md).
Durable acceptance cases, evidence/audit records and native baseline fixtures
remain intentionally retained. No additional native tests followed this run
and exact-owned read-only classification.

## Preceding complete native candidate, before the query-filter fix

The preceding independent native package run completed **16:26:50–16:35:46 UTC**
on October 30, using the selected operator’s previously selected `<test-workspace-profile>` OAuth identity,
verified TLS and the exact isolated `<native-test-branch>` endpoint/database. Public
API preflight verified branch UID `<branch-id>`, endpoint UID
`<branch-id>`, branch state `READY`, and expiry **October 31,
16:52:26 UTC**. The endpoint was initially idle and woke through the normal
connection path; no endpoint configuration changed.

```sh
go test -p=1 -race -tags=integration,e2e -count=1 -timeout=1200s -json ./tests/lakebaselive
```

The one complete invocation enabled the existing mutation/SRE/live-Gateway
opt-ins, metadata and exact-owned telemetry payload checks with
`REQUIRE_DELIVERY=1`, short startup application identity, a 180-second wait
bound, and the exact sixteen-provider-probe allowlist documented below. The
Gateway used `system.ai.gpt-6-sol` at the selected workspace's
`/ai-gateway/mlflow/v1` endpoint, with OAuth and no API key. The strict
incompatible-extension gate and prior-run attribution diagnostic were
explicitly disabled. No source edits, grants or private setting changes were
made. SHA256 over all repository Go sources and module files matched before
and after:
`11833200348c613ba0c262615acfff6d667727f2b058b4385a8741a284a76e96`.
The toolchain was Go 1.27.1 on darwin/arm64.

| Result | Fresh complete invocation |
| --- | --- |
| Test/subtest records | **79 pass, 0 fail, 2 explicit skips** |
| Root tests | **11 pass, 0 fail, 2 explicit skips** |
| Package elapsed | **535.758 seconds**; stdout's rounded package line is 535.757 seconds |
| Wall time including command startup | **538.365 seconds** |
| Exit / stderr | Zero / empty test and runner stderr |
| Typed SRE statistics maintenance | Approved execution, replay/manual-bypass refusal, durable decision, exact statistics-object rollback and evidence chain passed |
| Native application SLO | Thirteen samples, one page, replay deduplication, conflict/wrong-scope refusal, restart persistence and stale-to-unknown passed |
| Controlled incidents | Owned lock/idle-transaction and advancing-sequence diagnosis passed; action restrictions preserved |
| SRE source matrix | 38 source identities: 8 `ok`, 10 `empty`, 20 `unsupported` (19 policy-denied, one unconfigured source) |
| Explicit provider matrix | All sixteen catalog reads completed: eleven `ok`, five valid `empty`; no new grants |
| Real Gateway investigator | Three model calls, eight tool calls, 9,749 tokens; accepted cited evidence and event-chain checks; healthy fixture concluded `inconclusive`, `stop=final` |
| Strict telemetry | All 24 records passed, no delivery skips; eight usable owned plans, one valid synthetic log and one valid DDL event |
| All ten metadata streams | Nonempty final reads; schema/read and delivery gates passed |
| Telemetry cleanup | Unique owned schema dropped and absence independently verified |

The two skipped root tests are exactly
`TestLakebaseProviderExtensionCompatibility` and
`TestLakebaseTelemetryAttribution`. These are intentional omissions of a
separately established strict provider incompatibility gate and a prior-run
diagnostic, respectively. Neither is a provider-delivery skip, and neither is
counted as successful feature support. The native extension prerequisite also
still observes restricted `pgaudit` creation; installing a catalog-advertised
extension does not prove server audit-log delivery.

The fresh telemetry fixture used verified startup tag
`lbo_6da2a754458a4d5e8df5b7db` and owned schema
`sage_telemetry_d6d6c1648c0748f7837a2222330ee87a`. Its strict root took
**67.99 seconds**, with nine returned plan rows: eight valid, one rejected,
none masked. The rejection is preserved as reported; this invocation does not
independently classify it as malformed or self-monitoring traffic. Observed
maximum event-to-reader intervals were **32.645534 seconds** for plans,
**30.090572 seconds** for logs and **34.281729 seconds** for DDL. This is one
successful bounded run, not a provider delivery/completeness SLA. The separate
seven-stream normalizer and durable/App scheduler checks below retain their
own earlier windows and are not combined into this invocation's counts.

The Gateway case is `21fe74a4-681e-4904-b580-4a298d63d340`. The native typed
maintenance case is `5e28ef75-2638-46ad-ba72-1866c099dbd5`, proposal
`4bcb4c4d-bc17-496c-b86d-42aa34880a2f`, evidence
`c0231dff-b734-42ed-a716-251e45748c0a`, queue 3, action 47 and decision 106.
Statistics OID 245775 was created and then verified absent after rollback;
ANALYZE statistics are not restored and workload benefit remains unproven.
The native SLO case is `1ff2bd4e-9960-4481-8e14-6fa95ec480ea`; its exact sample
series/status/transition cleanup assertions passed. Controlled incident
sessions were rolled back and their owned schema drop confirmed. Durable
acceptance cases, evidence, audit records, the native baseline workload and
persistence marker remain intentionally available in the test database; this
is not a claim that all acceptance history was deleted.

The untouched JSON event stream is
`.local-test-results/gap-dev4-native-final-complete.json`, with its empty
`.stderr` and `-summary.json`. Unique same-prefix reports are
`-sre-matrix.json`, `-sre-transcript.json`, `-sre-incidents.json`,
`-telemetry.json` and `-provider-probes.json`. The ignored runner preserves
the exact environment opt-ins and command without tokens or credential DSNs.
An initial setup-only invocation using the compact `-p1` spelling failed
before any native tests ran; it remains separately preserved under
`.local-test-results/gap-dev4-native-final.*`. The corrected invocation above
is fresh and complete, not a composite of filtered test results.

## Preview-state observation and telemetry chronology

Read-only inspection of the selected `<test-workspace-profile>` workspace's supported
Previews page (deployment URL omitted)
found **Lakebase Observability in System Tables off** and **Lakebase Insights
off**, while the separate **Lakebase OpenTelemetry Integration was on**.
The CUA accessibility state was cross-checked visually. A privacy-clean excerpt
with the exact URL, controls and bounded observation time is preserved in
`.local-test-results/gap-followup-preview-prerequisite.json`.

The [current public telemetry contract](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
requires the workspace preview. Capture starts automatically for active
projects after enablement; creating an old destination configuration is no
longer supported. `neon_monitor.log_capture_enabled=on` and
`databricks.o11y_exporter_enabled=on` are documented diagnostics, but their
values do **not** establish that the workspace preview is enabled. Account-wide
rows visible through Unity Catalog also do not establish this workspace's
capture. The earlier zero selected-resource reads established only that
delivery was then unverified. The separately observed off-state does not
establish why those reads were empty; later actual delivery contradicts a
claim of complete capture absence.

The supported preview control applies to all Lakebase projects in the workspace.
It has not been changed by this investigation. Workspace-wide capture needs
specific authorization; it cannot be isolated to the task-owned test branch
through a documented per-project system-table capture switch. No private API,
private GUC workaround, broad Unity Catalog grant or PII-group change was used.
No public SDK/API for reading this preview state was established, so readiness
must distinguish a verified UI prerequisite from PostgreSQL flags and table
permissions. Do not silently label the latter as preview readiness.

The strengthened native harness creates a unique owned durable schema/table,
executes a unique table-bound slow query and a safe timeout, keeps its compute
active, and checks actual selected-resource delivery after a bounded wait. Its
cleanup drops only that run's schema and separately verifies absence. Metadata
reads cover all ten tables; payload reads are restricted to the exact owned
application tag. Diagnostic aggregates check API UIDs, project slug/name,
branch/endpoint UIDs and plan-history Neon identifiers. An additional exact-app
aggregate omits resource filters to detect incorrectly mapped own events
without fetching another application's SQL, plan or logs.

The initial diagnostics returned zero for every alias and no own application
rows without resource filters. The 900-second strict run began during the
observed-off preview window and completed at **15:40:29 UTC**, with **55 polls
and 55 keepalives**. No preview was changed by this investigation, and its state
was not continuously reverified throughout the run. The final bounded reads
found selected-resource delivery in **all ten metadata streams**:

| Stream | Final returned metadata rows |
| --- | ---: |
| plan_history | 92 |
| postgres_logs | 2 |
| active_session_history | 12 |
| ddl_history | 219 |
| compute_counters | 22 |
| compute_gauges | 23 |
| database_counters | 13 |
| database_gauges | 23 |
| wait_event_counters | 319 |
| pg_stat_statements_counters | 498 |

One exact-owned synthetic log normalized successfully, with an observed
event-to-reader delivery interval of **289.006651 seconds**. Exact application
filters returned zero owned plans and DDL, including aggregate queries without
resource predicates. This establishes a remaining controlled-attribution gap;
it does not establish that no plan or DDL metadata was emitted or that payload
masking caused the absence. The strict run recorded **21 passing and three
failing test records, zero skips**: the two controlled plan/DDL subtests and
their root failed. All ten final metadata-delivery assertions passed. The owned
schema was dropped and absence independently verified; stderr was empty.
Artifacts are `.local-test-results/gap-followup-telemetry-strict.json` and its
`-test.json`/`.stderr` companions.

The preview was observed off during the stated inspection window, whereas
the public contract describes enablement as a prerequisite. This discrepancy
does not establish continuous state or causality for delivered or absent
events. A public-docs restart is recommended when either
documented flag is off after preview enablement. Both flags here are on, so a
restart is not an established remedy for the disabled preview.

### Owned-marker attribution follow-up

A separate read-only follow-up at **15:43:58 UTC** queried exact prior owned
markers inside the API-verified resource, database and bounded test interval,
without an application-name predicate. Only aggregate counts and timestamps
were returned; no statement, plan or DDL payload was fetched.

| Exact owned marker | Matching rows | Matching prior application tag | Masked statement rows |
| --- | ---: | ---: | ---: |
| Recorded EXPLAIN query ID | 0 | 0 | 0 |
| Unique owned schema/object name | 1 | 0 | 0 |

The one DDL event occurred at **15:40:29.429445 UTC**, when the verified cleanup
connection dropped the schema. This confirms capture of the owned cleanup,
whose ordinary pool application differs from the synthetic fixture session.
It does not establish capture of the initial CREATE statements, and column
masking is not an adequate explanation for an absent application-name row:
the [public schema](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
masks statement/plan/DDL text, while application names, schema names and query
IDs are metadata columns. `TestLakebaseTelemetryAttribution` passed; its
metadata-only report is `.local-test-results/gap-followup-telemetry-attribution.json`
with test/stderr companions; stderr is empty.

Read-only diagnostics found capture/exporter/plan
flags on, both query regexes empty, and the excluded application prefixes
`compute_ctl,postgres_exporter,sql_exporter,walproposer,pgbouncer`; the fixture's
`lakebaseobs_acceptance_` prefix matches none. No telemetry settings were written.

One narrower repeat uses a fresh verified-TLS connection with the unique
application name set in startup parameters, explicitly verifies its backend PID
and current application, commits new owned DDL, runs a unique 1.2-second query
and waits 180 seconds with keepalives. This separates startup attribution from
the earlier session `set_config` path without changing provider configuration.
It completed **15:45:04–15:48:55 UTC** with startup application and backend PID
verified, twelve polls and twelve keepalives, all ten metadata streams observed,
and owned schema cleanup independently verified. Its log normalized successfully
with **24.187296 seconds** of event-to-reader delay. Tagged plan and DDL payload
results remained zero: **21 passing and three failing test records, no skips**.
Startup attribution alone therefore did not resolve the gap. Artifacts use
`.local-test-results/gap-followup-telemetry-fresh-startup*`; stderr is empty.

The EXPLAIN-reported query identifier was identical in the two runs despite
different owned tables/schemas. It is therefore recorded as an owned query's
identifier, not asserted to uniquely identify the fixture. A follow-up also
checks the unique schema token and the explicitly verified backend PID, using
metadata-only aggregate results. The fresh result is separate from, and does
not rewrite, the prior 900-second observation.

### Attribution root cause established

The expanded read-only follow-up at **15:49:47 UTC** found two plans matching
the recorded query ID, three plans containing the exact unique schema token,
ten plan rows from the explicitly verified backend PID, and the owned initial
DDL plus cleanup. Their statement/plan fields were not masked. This establishes
actual controlled capture despite the earlier zero exact-application results.

Fetching only the application-name metadata for the intersection of the exact
owned schema and verified backend showed the same value in three plan rows and
one initial DDL row:

```text
Verified startup/session/log name (54 characters):
lakebaseobs_acceptance_e7a1b031aa4846ae883b87a81f4abab0
Captured plan/DDL name (31 characters):
lakebaseobs_acceptance_e7a1b031
```

The observed provider fields truncate application names to 31 characters. The
public schema describes them as application-name strings without specifying
this length behavior. The old strict payload failures were false negatives
from the long fixture selector, not evidence that the provider failed to
capture those controlled plans or DDL. Evidence is preserved separately in
`.local-test-results/gap-followup-telemetry-fresh-attribution.json` and
`gap-followup-telemetry-fresh-attribution-label.json`, with their passing test
and empty-stderr companions.

The harness now uses a 28-character `lbo_` name with 96 bits of random identity,
below the observed boundary, while retaining a full-UUID owned schema. It keeps
the exact application predicate; it never broadens a payload read to a shared
truncated prefix. The production adapter's optional application selector is
not automatically rewritten, and production database policy remains unchanged.
Live read-only settings show `plan_log_all_min_duration=100`,
`plan_log_min_duration=0` and `plan_dedup_cache_size=200`; no settings were changed.
The corrected strict run completed **15:51:40–15:52:32 UTC** in **51.36 seconds**:
**all 24 test records passed, none failed or skipped**. Its first payload poll
returned nine owned plan rows (eight normalized, one rejected), one valid
synthetic PostgreSQL log and one valid owned DDL event. Maximum observed
event-to-reader delivery intervals were **30.036154 seconds for plans**,
**27.514544 seconds for logs** and **31.76284 seconds for DDL**. All ten final
metadata streams were nonempty. The rejection is preserved rather than counted
as valid. A separate unchanged-production-reader/normalizer re-read found ten
exact-app plan rows: nine valid and **one intentionally excluded self-monitoring
query**, with no malformed, masked or other rejection. The original rejection
is an expected monitoring exclusion. Its read-only counts are preserved in
`.local-test-results/gap-followup-telemetry-short-tag-normalization.json`, with
empty stderr and no persisted payload. This does not establish acceptance of
every possible provider plan shape.

The unique schema was dropped and absence verified, and stderr is empty.
Artifacts are `.local-test-results/gap-followup-telemetry-short-tag.json`,
`gap-followup-telemetry-short-tag-test.json` and
`gap-followup-telemetry-short-tag.stderr`. The 180-second upper bound was not
exhausted because all three controlled streams succeeded on the first poll.
This establishes actual native controlled delivery under the corrected exact
selector. It does not establish a general latency/completeness SLA, unmasking
rights for other identities, preview enablement throughout the observed period,
or full historical SRE consumption. The subsequent scoped native cache test is
recorded below. No workspace preview, telemetry GUC or access grant was changed.

## All ten streams have direct native normalization coverage

A separately authorized read-only follow-up completed **16:15:13–16:15:31
UTC**, using the unchanged production SDK reader and normalizers on existing
events from the exact **15:51:40.81909–15:52:32.179274 UTC** fixture interval.
The SDK was configured with `AllowPayload=false`; database streams used the
API-verified test resource and database, and compute streams required explicit
`AllowCompute=true`. Each query was bounded to 1,000 rows, four MiB and twenty
seconds. No workload, fixture, setting, access grant or telemetry state changed.

| Stream | Actual events | Normalized | Rejected |
| --- | ---: | ---: | ---: |
| active_session_history | 12 | 12 | 0 |
| compute_counters | 2 | 2 | 0 |
| compute_gauges | 2 | 2 | 0 |
| database_counters | 2 | 2 | 0 |
| database_gauges | 2 | 2 | 0 |
| wait_event_counters | 33 | 33 | 0 |
| pg_stat_statements_counters | 437 | 437 | 0 |
| **Total** | **490** | **490** | **0** |

The check validated **1,864 numeric metric values** as finite and nonnegative,
with no unknown age fields, empty streams or invariant failures. Scope,
source, timestamps and compute identity matched the bounded query; each stream
contained one compute epoch. Counter intervals were valid and gauges retained
point-in-time semantics. Payload fields were absent. Together with the
controlled plan/log/DDL tests above, this establishes direct native reader and
normalizer acceptance for all ten streams, under the tested user identity.

The result remains explicitly **historical-only**, **incomplete** and outside
live admission. It does not exercise all seven durable consumers, scheduled
`Runtime.Poll`, an investigator's `ToolHistory` invocation/model citation, or
the App principal. The separate App metadata scheduler check follows below.
Metadata evidence is preserved in
`.local-test-results/gap-followup-metadata-normalization.json`; its separate
stderr file is empty and the helper exited zero. No raw SQL, plan or log
payload was retrieved or written.

## Deployed App scheduler persisted exact owned plan metadata

The running `<development-resource>` deployment
`01f1d47c59e517c1aa74b207a3c3d6ab` separately passed a bounded native scheduler
check on the dedicated `projects/<project>/branches/<branch>` branch and
`pg_sage` database. The helper verified the App's branch/database resource,
principal, active deployment and polling/metadata-only state, then verified the
endpoint host and project/branch/endpoint UIDs through public APIs. A read-only
database identity/owner check matched OID **180227**, registered database UUID
`1dc11514-b41c-4cf7-90c8-53dca4144f21`, current metadata-only owner policy and
recomputed scope hash
`225e1957cde9a262986f4fbc74cb10c380eecb5392230577f124c3c5e26ab936`.

Exactly one safe `SELECT pg_sleep(1.2)` query with an owned synthetic literal
ran in a read-only transaction, with a 28-character startup application tag
`lba_7df696e08a382c851f783854`. An `EXPLAIN (VERBOSE, FORMAT JSON)` of the same
synthetic query supplied query ID **8379109690210330486** without executing it.
The session's actual backend PID, tag, database and OID were verified. The
fixture ran **16:20:34.221388–16:20:35.653542 UTC**. No table, DDL, server
setting, App configuration, deployment, SLI sample or SRE case was changed.

The helper did not instantiate or call a telemetry Runtime. It waited for the
existing App scheduler, then read only inbox metadata matching the exact tag,
query ID, current verified owner scope and bounded fixture interval. Five
bounded polls found the real event and matched it to the platform-authenticated
history API:

| Metadata assertion | Observed result |
| --- | --- |
| Event ID | `0b225c479a7d78b3ec1f77fc1dde4a7b229bc3bd0dd1abcce69fd35269bbf45c` |
| Compute | `compute-hidden-art-d1oanjqs` |
| Occurred | `2026-10-30T16:20:35.577173Z` |
| App receipt | `2026-10-30T16:21:09.902749053Z` |
| Event-to-receipt interval | **34.325576 seconds** |
| Provider duration | **1202.230699 ms** |
| Metadata provenance | Exact tag/query ID, resource UIDs, OID, database UUID and scope matched |
| Authenticated history API | Same event ID, scope, compute and timestamps; HTTP 200 |
| Payload policy | `payload_allowed=false`, no SQL/plan/parameter payload in persisted event |
| API availability | `payload_unavailable`, expected under metadata-only consent |

The history API intentionally omits query ID/application fields for
payload-unavailable plans. The exact-owned inbox metadata supplies that
attribution, and matching event IDs link it to the authenticated API response.
The helper completed at **16:21:19 UTC**, exit zero with empty stderr; evidence
is `.local-test-results/gap-dev4-app-scheduler.json` and its ignored `.py`
helper. The API stayed historical-only, incomplete and without an exact
multiplicity claim. Its deployment/resource/polling guards remained unchanged.

This proves a positive metadata event through the deployed App's actual
`Runtime.Poll`, durable inbox and administrator history API under the App's
metadata-only configuration. It does not prove captured raw-plan cache
ingestion in the App: payload was deliberately absent by consent. The native
payload/cache/hash chain below is separate evidence. Neither this check nor
the native helper proves a model-selected `ToolHistory` call and citation;
the final App/Gateway investigator acceptance owns that assertion. Production
defaults and payload consent remain unchanged.

## Actual provider plans reached durable cache and hashed SRE evidence

An ignored local helper used the unchanged production SDK reader, normalizer,
registered coordinator scope, epoch-fenced owner policy, durable Store batch/
ACK methods and `autoexplain.StoreProviderPlan` consumer. Its SDK payload read
remained restricted to the exact 28-character synthetic application, API-
verified test resource/database and original fixture interval. It selected two
actual slow-query events containing the unique owned schema and required an
initial **zero existing cache rows for that query ID**. Cache inserts and
cleanup were also pinned to the unique scope hash and exact event IDs; no
unrelated row was overwritten or deleted.

The actual native chain verified two durable inbox/cache rows, duplicate ACK
and sink replay idempotency, source, scope, compute/event identity, capture/
receipt timestamps, plan fingerprints, duration provenance, plan hashes and
nullable analyze/buffer/timing flags. It constructed the production Runtime
for historical reads but deliberately did not invoke `Runtime.Poll`, whose
global expired-row purge would exceed this fixture's cleanup scope. This is
native delivery/consumer acceptance, not a scheduler-wide Poll test.

Before the fix, production `EvidenceSource.ReadHistory` failed under its
fixed **500 ms** deadline covering pool acquisition, database-identity read,
owner policy, retained events, cursor and normalization. Three bounded attempts
returned `history_read_failed` after **501 ms**, `history_read_failed` after
**500 ms**, and `history_identity_unavailable` after **500 ms**. This identifies
a pg_sage client budget failure on the local-to-Lakebase path, not missing
telemetry or a Lakebase SQL/schema restriction. At that stage no historical
evidence case or hash was claimed as verified.

The product fix allows **five seconds for the whole historical read**, keeps
individual backend statements and lock waits at **500 ms**, and bounds
cancellation/rollback independently. The final package race/coverage run with
union tags passed in 9.408 seconds, including cancellation regressions; see
`.local-test-results/gap-closure-history-budget-5s-final2.log`. Native rebuilding
used that unchanged final production code. The first post-fix historical read
succeeded in **1,478 ms**; that helper then requested an invalid investigation
state transition. This was corrected only in the ignored helper, not treated
as a provider/product failure. Its exact fixture cleanup succeeded and the
attempt remains preserved in `gap-followup-provider-sink-history-5s.json`.

The earlier scoped native chain completed **16:08:41–16:09:01 UTC**, exit code zero:

| Assertion | Observed result |
| --- | --- |
| SDK source | Ten exact-owned-application plan rows, two selected real slow-query occurrences |
| Existing cache collision preflight | Zero rows for the selected owned query ID |
| Durable cache + ACK | Two stored rows; replay did not duplicate either |
| Provenance | Source, unique scope, compute/event identity, capture/receipt timestamps, fingerprint and duration source verified |
| Context flags | `analyze_enabled=true`, `buffers_enabled=true`, `timing_enabled=null` preserved without inference |
| Plan hashes | Match normalization of the actual captured provider JSON |
| Historical read | Two actual plan observations on the first attempt in **1,230 ms** |
| Completeness | Explicitly historical-only and partial; `complete=false` |
| Durable SRE evidence | Committed, read back and SHA256 recomputed successfully |
| SRE event chain | Verified successfully |
| Payload separation | No raw SQL/plans in evidence or report files; long-lived evidence also excludes application/object names |
| Cleanup | Exact cache events, inbox/ACKs, cursors, owner policy, SRE case/evidence/events and coordinator registration removed and absence verified |

The task-owned investigation was
`3c89357f-ffc0-4289-9c32-d8c965ffe950`, with evidence
`bd2695bc-2fe8-455f-bddc-0b6160929552` and verified digest
`4af1a2a64b404fb4fe8badd8574f7e37c3e9ea5b881440b7c50aa0678d546dc1`.
These IDs are preserved for the validation record; their temporary database
fixtures were cleaned. Final metadata evidence is
`.local-test-results/gap-followup-provider-sink-history-5s-final.json`, with
empty stderr. No model call was needed for this ingestion/history/hash check;
the real App/Gateway loop is covered separately.

Each completed ingestion attempt verified exact fixture cleanup: cache events,
inbox/ACKs, cursors, unique owner policy and coordinator database registration.
The initial attempt found that the telemetry control tables had not yet been
created and stopped before payload ingestion (SQLSTATE `42P01`); its unique
coordinator binding was subsequently removed and verified. Production
`Store.EnsureSchema` then initialized its additive telemetry metadata tables.
The initial failure, successful cache/failed-history attempt and timed retry
are preserved respectively in `.local-test-results/gap-followup-provider-sink.json`,
`gap-followup-provider-sink-final.json` and
`gap-followup-provider-sink-history.json`, with empty stderr companions. Raw
provider payloads were never logged or written to report artifacts; the owned
temporary durable copies were cleaned. The compatibility agent made no
repository Go edits while this ignored helper ran; the regression owner
controlled the production deadline fix and final source freeze.

## Planner extension incompatibilities observed

| Operation on the isolated `pg_sage_test` database | Native result | Classification |
| --- | --- | --- |
| `CREATE EXTENSION IF NOT EXISTS pg_hint_plan` | Succeeds; planner hook remains absent | Installed catalog object does not establish usable hints |
| `LOAD 'pg_hint_plan'` | SQLSTATE `0A000` | Activation unavailable through this supported SQL path |
| `SET LOCAL session_preload_libraries='pg_hint_plan'` | SQLSTATE `42501` | Session activation setting denied |
| `ALTER DATABASE pg_sage_test SET session_preload_libraries='pg_hint_plan'` inside a rolled-back transaction | SQLSTATE `42501` | Database-scoped activation setting denied; no setting left behind |
| `CREATE EXTENSION IF NOT EXISTS hypopg` inside a rolled-back transaction | SQLSTATE `42501`; extension absent from `pg_available_extensions` | HypoPG what-if validation unavailable on this Lakebase image/role |

The [public Lakebase extensions list](https://docs.databricks.com/aws/en/oltp/projects/extensions)
lists `pg_hint_plan` 1.8.0 for PG18. The
[upstream activation instructions](https://pg-hint-plan.readthedocs.io/en/latest/installation.html)
require `LOAD` or a preload setting. The live results above establish the install-versus-activation gap for this tested endpoint and role. pg_sage already reports `installed_not_loaded`; it must continue to avoid
advertising working hint enforcement. Plain EXPLAIN and normal PostgreSQL
planner operation remain usable, but they do not replace a forced-hint feature.

HypoPG is not listed in the current public Lakebase extension table. The native installation attempt is the decisive observation for this tested Lakebase image and role. The production what-if path already
fails closed when absent. It must not invent a hypothetical benefit or treat
extension absence as a successful what-if validation.

## Tested fallback and its limits

On a task-owned temporary table of 20,000 rows, a real candidate index reduced
plan-only EXPLAIN estimated cost from **339 to 97.34**. Rolling back the index
savepoint restored the baseline cost exactly. The surrounding transaction
rolls back the temporary fixture. This proves a supported isolated physical
index experiment is possible; it is **not** an in-place replacement for
HypoPG, proof of workload latency improvement, or authorization to create
candidate indexes on customer production tables. A disposable branch or
reviewed real-index workflow can use this approach with its own cost/benefit
measurements and rollback evidence.

The strict extension test recorded two failed compatibility subtests and a
passing physical-index fallback, with zero skips. Evidence is preserved in
`.local-test-results/gap-followup-provider-extensions.json` and
`gap-followup-provider-extensions-test.json`; the separate stderr file is empty.
Source: `sidecar/tests/lakebaselive/provider_compatibility_test.go`.

No provider team message or ticket was sent. Existing internal issue links are
research evidence; no unsupported platform workaround was applied.

## Branch storage does not imply a shared PostgreSQL runtime

Copy-on-write storage does not establish a shared PostgreSQL runtime. Current public documentation states:

- [Branches](https://docs.databricks.com/aws/en/oltp/projects/branches):
  **"Each branch has its own compute that you can scale independently."**
  The same page explicitly isolates databases, roles, GRANT/REVOKE and role
  attribute changes between branches.
- [Manage computes](https://docs.databricks.com/aws/en/oltp/projects/manage-computes):
  **"To connect to a database in a branch, you must use a compute associated
  with that branch."** Each branch has one primary read-write compute and may
  have read replicas.
- [Architecture](https://docs.databricks.com/aws/en/oltp/projects/architecture)
  separates a stateless PostgreSQL compute layer from durable storage.
  Copy-on-write storage and multiple compute instances are separate concepts.

Public contracts and actual deployed behavior govern this assessment; a shared-storage diagram alone cannot establish runtime sharing. No new
cross-branch native probe was performed to justify this correction. Independent
branch computes also do not make a workspace-wide preview switch branch-local.

## Explicit provider-probe compatibility matrix

`sidecar/tests/lakebaselive/provider_probe_compatibility_test.go` adds a separate
opt-in read-only provider gate for the previously policy-denied cluster and
filesystem sources. It requires the exact test endpoint and API-verified
resource binding, plus `PG_SAGE_LIVE_LAKEBASE_PROVIDER_PROBES=1` and a nonempty
comma-separated `SAGE_PROVIDER_PROBE_IDS` allowlist. Unknown, duplicate,
database-only and external-adapter IDs are rejected before any connection.
The parser's boundary tests pass with the race detector.

The test constructs its own trusted cluster-scope policy restricted to those
IDs and the physical test pool. It leaves production's database policy intact.
The production runner uses bounded read-only transactions, fixed SQL, existing
role preconditions and row/byte limits. No roles are granted, settings changed,
actions invoked, or background workers started. Only metadata is persisted;
probe payload rows and raw error text are not logged.

`SAGE_PROVIDER_PROBE_REPORT` records status/reason, SQLSTATE when supplied,
runner attempts, failure phase, required role/extension names, probe-SQL
execution evidence, row counts and a classification that
separates policy failure, missing-role/extension preflight, platform schema or
function gaps, permission restrictions and execution-budget failures. Usable
empty results verify query support, not a positive incident detector. Missing
privileges are not converted into healthy emptiness or bypassed.

Runner attempts do not prove that catalog-probe SQL executed: a connection,
server-version read, transaction setting or prerequisite can fail first. The
report marks usable results `confirmed_completed`, known role/extension
preflights `not_reached_preflight`, and other failures `unverified`; it does not
infer exact statement execution from the broader server-execution phase.

The explicitly authorized read-only matrix ran after telemetry fixture cleanup,
using the existing verified-TLS/passwordless test DSN and selected `<test-workspace-profile>`
profile:

```sh
PG_SAGE_LIVE_LAKEBASE_MUTATE=1 \
PG_SAGE_LIVE_LAKEBASE_PROVIDER_PROBES=1 \
SAGE_PROVIDER_PROBE_IDS='lock_chains,connection_saturation,replication_lag,replication_slots,wal_checkpoint,archiver,checkpoint_activity,standby_replay_state,lwlock_waits,xid_runway,xmin_horizon,wal_runway,cluster_database_size,temp_file_holders,wal_directory,signal_target' \
SAGE_PROVIDER_PROBE_REPORT='<absolute metadata report path>' \
go test -json -race -count=1 -run '^TestLakebaseProviderProbeCompatibility$' ./tests/lakebaselive
```

Run from `sidecar/` with the endpoint/profile/DSN environment already used for
the isolated native suite. The existing fixture guard requires the mutation
opt-in even though this new root only performs reads; no fixture DDL or role
mutation is added. This compatibility matrix is not an incident-injection test.

The native matrix completed **15:40:59–15:41:08 UTC**: **all 16 probes passed**
(17 passing test records including the root, no failures or skips). Eleven
returned `ok`; `lock_chains`, `replication_lag`, `lwlock_waits`, `xmin_horizon`
and `temp_file_holders` returned valid `empty` results. Every catalog statement
completed under its bounded read-only production runner. Required
`pg_read_all_stats` role preconditions passed without new grants. No missing
role, extension, schema, function or privilege restriction was observed in this
matrix. This corrects the earlier policy-denied source interpretation: the
database-scoped production policy refused those sources; Lakebase did not
refuse these explicitly authorized test reads. The production policy remains
unchanged, and successful empty reads do not prove incident detection or
platform-specific WAL/replication semantics. Metadata evidence is preserved in
`.local-test-results/gap-followup-provider-probes.json` and its test/stderr
companions; stderr is empty.
