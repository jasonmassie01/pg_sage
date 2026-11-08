# Native Lakebase SRE acceptance — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

## Verdict and scope

**Latest complete dev4 native run:** the frozen-source full package passed
79 test/subtest records, failed none and explicitly skipped two in 453.537
seconds after the query-filter fix. The preceding 535.758-second candidate
remains separately preserved. The final real Gateway case
`354ff955-20d6-4ea5-8b1d-b479a333b959` made
two model calls and four tool calls, used 4,484 tokens, verified cited
evidence/event-chain assertions and ended `inconclusive` on the healthy fixture.
Native typed maintenance, application SLO lifecycle, controlled incidents,
source matrix, all sixteen authorized provider probes and strict telemetry
delivery passed in that same invocation. The two skips are the separate strict
extension incompatibility test and prior-run attribution diagnostic. See the
[provider follow-up](2026-10-30-provider-compatibility-gaps.md) for unchanged-source
digest, artifacts and cleanup; earlier acceptance counts remain historical.

**October 30 follow-up:** a separately authorized exact-allowlist read-only
matrix executed all sixteen previously policy-denied cluster/filesystem probe
statements successfully on the isolated branch: eleven `ok`, five valid `empty`,
no missing-role/schema/function or privilege gap observed. This leaves the
database-scoped production policy intact and proves query support, not positive
incident detection or every platform-specific WAL/replication semantic. The
corrected strict provider telemetry gate separately passed 24/24 records with
usable plans/log/DDL and all ten metadata streams delivered. See the
[provider follow-up](2026-10-30-provider-compatibility-gaps.md) for exact artifacts,
31-character application-label behavior and remaining planner extension gaps.
Two actual captured provider plans also passed durable-cache replay and
historical SRE evidence/hash/event-chain readback after a bounded historical
read deadline fix; all temporary scope/case/cache/history records were cleaned.
The final first historical read took 1,230 ms. This separate check made no model
calls and does not replace the real Gateway investigator acceptance below.
A final read-only metadata check normalized 490 actual events from the other
seven telemetry streams without rejection, completing direct native
reader/normalizer coverage of all ten streams. It did not exercise all durable
consumers or investigator historical-tool selection. A separate deployed-App
check subsequently verified one exact synthetic plan event through the real
App scheduler, durable metadata inbox and authenticated history API. Payload
was absent by consent; the raw-plan cache chain remains separate native
evidence. See the linked provider follow-up for exact attribution and limits.

The production SRE coordinator/store/probe runner completed a real Unity
Gateway investigator loop on the explicitly isolated
`projects/<project>/branches/<branch>/endpoints/<endpoint>`, database
`pg_sage_test`, using user OAuth from the selected `<test-workspace-profile>` workspace profile,
verified TLS and PostgreSQL 18. The fixture is task-owned. This establishes
the tested database-scoped paths; it does **not** certify all SRE functionality,
all PostgreSQL versions, the App principal or cluster-wide observation.

The App principal was subsequently accepted through the deployed API on final
deployment `01f1d472793f135aa1802c4e4ef7c979`: case
`98b61710-0cdd-4bd1-8503-47c00f0c3332`, two real model calls, three newly
selected/cited database reads, 4,558 tokens, verified evidence hashes and
14-event chain, idempotent lifecycle and unchanged action/queue ledgers. Its
healthy-fixture result was inconclusive. See the
[final dev.3 report](2026-10-29-v2.5-dev3-acceptance.md) for the separate
App/browser/Gateway acceptance and precise limits.

The restored full native `go test -json -race -count=1 ./tests/lakebaselive`
run completed on October 30 in **353.262 seconds: 46 passing test/subtest
records, 3 skipped, 0 failed**, with empty standard error. All three SRE roots
and both controlled incident subtests passed. The three historical skips concern then-unverified
provider plan/log/DDL telemetry delivery; they do not weaken or extend the
separately verified direct-SQL and Gateway paths.

The read policy defaults to database scope for Lakebase and preserves legacy
standalone behavior elsewhere. Trusted configuration may narrow sources;
request/model data cannot widen it. The policy binds the physical connection
target and database, is hashed into the creation event, and is checked before
reads on resume. Changed/missing policy prevents an existing case from reading.
The scoped lock graph only returns local session identities; an outside-scope
blocker makes the result unavailable rather than disclosing it or claiming
healthy absence. `lock_chains` remains denied in database scope.

## Evidence

The opt-in roots are in `sidecar/tests/lakebaselive/sre_test.go` and
`sre_incidents_test.go`. They require both the existing exact native
endpoint/database/TLS guard and `PG_SAGE_LIVE_LAKEBASE_SRE=1`. The Gateway root
additionally requires explicit Databricks OAuth/model opt-in without an API key.
No background investigator loop, action service, notification dispatcher,
cluster probe, server setting change or statistics reset is started.

Preserved local artifacts (excluded from version control):

- `.local-test-results/gap-dev3-native-restored.json` and separate empty
  `.stderr`: restored full-native test events and exact outcomes.
- `.local-test-results/gap-dev3-native-restored-sre-matrix.json`: current
  metadata-only results for all 38 source identities.
- `.local-test-results/gap-dev3-native-restored-sre-transcript.json`: current
  redacted real Gateway transcript, evidence IDs/digests and token usage.
- `.local-test-results/gap-dev3-native-restored-sre-incidents.json`: current
  two controlled incident results and cleanup contract.
- `.local-test-results/lakebase-sre-native-final.log`: earlier combined native run.
- `.local-test-results/lakebase-sre-native-matrix.json`: metadata-only outcomes
  for all 38 catalog/auxiliary identities.
- `.local-test-results/lakebase-sre-native-transcript.json`: redacted production
  transcript with tool evidence IDs/digests and accepted claims.
- `.local-test-results/lakebase-sre-native-incidents.json`: owned incident
  outcomes, policy fingerprint and cleanup contract.
- `.local-test-results/sre-policy-runtime-focused.log`: current-source focused
  race tests across probes, coordinator and runtime wiring.

The initial `.local-test-results/lakebase-sre-native.log` failed only at final
report export after matrix and Gateway acceptance assertions; report directories
are now created with owner-only permissions. The first combined rerun preserved
a lock fixture identity failure. Its fixture used the protocol-handshake PID;
Lakebase's SQL backend PID differed. The corrected fixture reads
`pg_backend_pid()` from each owned session and asserts the exact identities in
the observed graph. Neither production probe budgets nor causal thresholds
were relaxed. Earlier unsuccessful runs remain preserved.

The earlier combined `-race` invocation passed all three SRE roots and both
incident subtests in **111.500 seconds**. `git diff --check` also passed after
these changes.

Two later full-native attempts also remain preserved.
`gap-dev3-native-all.json` recorded 35 passes and one telemetry setup failure
because its warehouse environment variable was omitted.
`gap-dev3-native-final.json` recorded 15 passes and 12 failures after workspace
IP-ACL access was lost mid-run, including failures before SRE target binding.
Those setup/access results are not removed or reclassified as successes. The
restored run used the full configuration after read-only workspace/branch/
endpoint verification and was the latest full-native dev.3 acceptance result
at that checkpoint; the final dev.4 result is linked above.

## Real Gateway investigator

The restored healthy-fixture case `58fddabd-4efa-46e5-a60d-389b9f972c82`
completed `inconclusive` with **2 real model calls, 3 executed diagnostic tool
calls, 4,511 tokens** and `stop=final`. Its four transcript steps include
`read_stat_view`, `run_probe`, `graph_state` and final `submit_conclusion`;
two probes consumed the probe budget. This ran through the selected workspace's
`/ai-gateway/mlflow/v1` endpoint with `system.ai.gpt-6-sol` and Databricks OAuth,
without an API key. The final claims cite committed evidence; this is protocol
and evidence-lifecycle acceptance, not a positive incident diagnosis.

The earlier healthy-fixture case `af3d5bea-4778-436f-b547-8520ad9fdf14` completed
`inconclusive` with **2 real model calls, 4 tool calls, 4,378 tokens** and
`stop=final`. A previous passing case
`dab96cbd-530d-499d-a024-78009f0e2aa5` used 4,363 tokens. An earlier case
`938d2e1d-3d64-4622-8ac0-9fd2b47ee15e` also met the substantive acceptance
checks (3 calls / 6 tools / 7,139 tokens) before the report-write failure.

Assertions cover a nonfailed terminal lifecycle, at least two real model turns,
billed token usage, an actual model-selected permitted read committed as
evidence, matching stored evidence digest, an accepted final claim citing that
fresh model-selected evidence, a verified event chain, idempotent request
readback and an unchanged `sage.action_log` count. A deterministic fallback or
final answer citing only seeded probe evidence would fail this test.

An inconclusive healthy-fixture conclusion is appropriate: no incident is
asserted. It proves the bounded protocol/lifecycle, not accuracy across every
incident family. The preserved cases/evidence intentionally remain durable.

## Controlled known incidents

| Owned fixture | Detection and diagnosis | Remediation status |
| --- | --- | --- |
| One row holder left idle in transaction, one blocked waiter | Exact SQL backend identities found in database-scoped `lock_graph`; production coordinator concluded `idle_in_tx_holder` | Backend actions refused by database read policy; no execution |
| Signed/versioned local runbook on that lock case | v1 content hash, signer, extra `lock_graph` evidence and path `read_graph,is_idle,operator_step` verified; `completed` | Operator instruction only; no action queued or executed |
| Advancing sequence with explicit MAXVALUE 1,000, starting at 900 | Production two-sample plan and unchanged nearness/consumption thresholds concluded `explicit_maxvalue_limit` | Reviewed manual migration only |

These are **two controlled incident families**. The signed-runbook result
belongs to the row-lock case and is not a third independently covered incident.
Restored signed-runbook lock case:
`60a8ce0b-ac0b-485b-b1d3-e6635604165f` (5 durable evidence records).
Restored sequence case: `5cd9b478-ed5b-49f1-a02d-8f444067e8f5`
(4 durable evidence records). Both were detected and diagnosed; neither was
actionable (`policy_scope_denied` and `manual_migration_only`, respectively).
The lock runbook completed. The policy fingerprint was
`b86f77ee94ff91caa5f49055c38f6d4ddcb489a17db2e66e2f038d6ae58ec92f`.
Earlier passing lock case `d9080e74-8c3a-433f-8348-51f307bc2245` and sequence
case `aef95c03-e5a3-4d5c-9bc2-eefcfe241e0a` remain preserved.
The test verifies evidence hashes/event chains and unchanged action ledger.
Transactions unconditionally roll back, the bounded waiter exits, and the
uniquely named owned schema is dropped with cleanup confirmation in the log.
The restored log explicitly confirms removal of
`sage_sre_acceptance_1793371262612765000`.
No existing workload objects are removed.

## Complete native source matrix

There are 32 regular diagnostics, 2 independent action probes, 3 independent
signal adapters and 1 EXPLAIN source. The registry count is asserted so a new
probe cannot silently escape review. Metadata includes scope, eligibility,
whether SQL ran, version, status/reason, rows and duration. In the healthy native
matrix, 17 sources executed SQL: 8 returned `ok`, 9 returned successful empty
results. Nineteen were refused before SQL by policy, one adapter was not
configured, and the EXPLAIN refusal used an intentionally unknown query ID.
Successful emptiness does not certify an incident detector.

The restored matrix reproduced those counts exactly: **38 sources, 19 policy
eligible, 17 SQL executions, 8 `ok`, 9 executed `empty/no_rows`, 19 refused
before SQL, 1 `source_not_configured`, and 1 `empty/unknown_statement` EXPLAIN
refusal**. Thus the raw status totals are 8 `ok`, 10 `empty` and 20
`unsupported`; they are not 38 functional incident successes. The policy hash
matches the controlled incident report above.

| Source group | Native outcome |
| --- | --- |
| `backend_identity`, `stat_database`, `stat_tables`, `stat_statements` | Required identity/counter paths returned usable data |
| `autovacuum_wraparound`, `wraparound_tables`, `sequence_runway`, `temp_file_activity` | Usable current-database evidence; dangerous wraparound/temp-exhaustion scenarios were not induced |
| `lock_graph`, `long_transactions`, `prepared_xacts`, `vacuum_progress`, `plan_regressions`, `sage_actions`, `temp_spill_statements`, `runway_trends`, `recovery_sample` | SQL executed successfully; healthy-fixture matrix was empty. Lock/idle and sequence have separate positive fixtures above |
| `lock_chains`, `connection_saturation`, `replication_lag`, `replication_slots`, `wal_checkpoint`, `archiver`, `checkpoint_activity`, `standby_replay_state`, `lwlock_waits`, `xid_runway`, `xmin_horizon`, `wal_runway`, `cluster_database_size`, `signal_target` | Cluster scope: `policy_scope_denied`, zero execution attempts |
| `temp_file_holders`, `wal_directory` | Filesystem scope: `policy_scope_denied`, zero execution attempts |
| `autovacuum_cancellations`, `slo_status`, `pooler_pools` | External provenance/binding not authorized: `policy_scope_denied`, unexecuted |
| `change_feed` | Database-local source allowed, independent fixture adapter not configured: `source_not_configured`; not counted tested |
| `explain_statement` | Deliberately absent synthetic query ID: `empty/unknown_statement`; no workload EXPLAIN executed |

Policy denial is distinct from managed provider incompatibility. These denied
sources were not executed and cannot be labeled provider-unsupported from this
run. The full regular/auxiliary matrix is the JSON artifact, not a claim that
all 38 sources are operationally available.

## Local policy regression coverage

Focused `-race` tests pass for `internal/sre/probes`, `internal/sre` and
`cmd/pg_sage_sidecar`. They verify the complete source inventory, canonical
policy hash/cloning, explicit empty allowlist, nested policy intersection,
pre-acquisition Run/RunBackground refusal, two synthetic databases and wrong
physical binding, durable creation snapshot, changed-policy restart refusal,
deterministic/runbook/signal wrapper enforcement, filtered tool schema and a
forged model tool call refused even when it bypasses that schema.

Runtime checks verify strict scope suppresses cluster runway startup, backend
action startup, cluster change poller, external SLO engine/log subscription and
custodian scans before any independent callback/dial. A narrowed cluster
allowlist authorizes only the named custodian. Missing live log input produces
`source_unavailable` with zero eligible SLI events on repeated samples; it
cannot create a healthy zero-error window. Broader local run counts belong in
the root full-suite report, not a sum of these focused reruns.

## Remaining acceptance gates

- Keep the native and deployed App investigator results separate; both paths
  passed, but they do not establish all incident families or action execution.
- Signed runbook unsigned/retired/missing-evidence rejection has local DB
  coverage; the native fixture verifies the valid signed branch only.
- External signed application SLO input, burn/staleness/replay and one scoped
  SLO investigation remain unaccepted natively. Database proxy SLOs and
  asynchronous provider logs do not certify application SLO completeness.
- Captured query/plan regression history, positive vacuum progress, bounded
  temp spill and connection pressure require separate controlled native
  fixtures. Dangerous exhaustion/retention/storm scenarios remain replay-only.
- Historical telemetry ingestion/fanout and selected-resource delivery remain
  incomplete. App-principal metadata query access passed independently.
  No historical sample is treated as a live
  CPU/memory/free-space safety input or complete log-derived SLI.
  The restored native run successfully read all ten scoped `system.lakebase`
  schemas but found zero selected-resource rows, including the controlled
  application's plan/log/DDL reads after an additional 30-second wait.
  Their three delivery subtests remain skipped/unverified. Native plan payload
  shape, masking, delivery latency, capture completeness and unmasked payload
  access cannot be established from those empty reads; see the advanced
  telemetry report and `gap-dev3-native-restored-telemetry.json`.
- The eight supported maintenance executor contracts remain separate from
  SRE. Current SRE derivation only proposes backend cancel; no SRE-to-supported
  maintenance proposal/approval/verification/rollback bridge is implemented.
- Other PG major versions, broader topology, App identity credential endurance,
  HA/failover and provider-specific role/extension paths remain separately
  qualified by their own evidence.
