# Native Lakebase SRE source matrix — dev.5 follow-up

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

The regular catalog has 32 probes; the six original auxiliary sources follow them. This inventory uses the **final fixed-source complete native invocation** after the state-check upgrade and elapsed-clock correction: **79 passing test/subtest records, zero failures and two explicit opt-in skips in 435.223 seconds**, with source unchanged throughout. Its database-scoped matrix is combined only with the exact sixteen-source provider gate from that same invocation. The preceding dev.4 run and the initial red dev.5 old-state-check run remain preserved separately.

This is a query/source availability inventory, not a positive diagnosis test for every incident. For catalog probes, `OK` means typed rows returned and `empty` means SQL completed without observations. `explain_statement` instead stopped at `unknown_statement` before issuing EXPLAIN; it is explicitly identified below. A pg_sage policy refusal or unconfigured adapter is not a provider SQL failure.

Target: API-verified `<test-project>/<native-test-branch>`, database `pg_sage_test`, native the selected operator’s OAuth identity and verified TLS. Branch state/expiry were rechecked; source SHA256 before/after was `3df43110e9f5de27cf43b8793fd874b48258b326f937acafe16b52265e8d5f35`. The default deployed App keeps database-only policy. A test-only allowlist authorized exactly the sixteen cluster/filesystem follow-up sources and was not deployed. All sixteen statements completed (eleven `OK`, five valid `empty`) without new roles/grants or observed schema/function/privilege gaps. Positive lock/idle and sequence diagnoses come from separate owned incident fixtures in the same final invocation, not from the healthy/empty source reads.

| Source | Scope | Native query/source result | Rows | Evidence/version | Default database policy |
| --- | --- | --- | ---: | --- | --- |
| `lock_chains` | cluster | empty | 0 | final provider gate / v2 | withheld |
| `lock_graph` | database | empty | 0 | final native matrix / v3_database | permitted |
| `long_transactions` | database | empty | 0 | final native matrix / v2 | permitted |
| `prepared_xacts` | database | empty | 0 | final native matrix / v1 | permitted |
| `backend_identity` | database | OK | 1 | final native matrix / v1 | permitted |
| `connection_saturation` | cluster | OK | 6 | final provider gate / v3 | withheld |
| `replication_lag` | cluster | empty | 0 | final provider gate / v2 | withheld |
| `replication_slots` | cluster | OK | 2 | final provider gate / v2 | withheld |
| `wal_checkpoint` | cluster | OK | 1 | final provider gate / v2 | withheld |
| `autovacuum_wraparound` | database | OK | 393 | final native matrix / v1 | permitted |
| `vacuum_progress` | database | empty | 0 | final native matrix / v1 | permitted |
| `plan_regressions` | database | empty | 0 | final native matrix / v1 | permitted |
| `archiver` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `sage_actions` | database | empty | 0 | final native matrix / v1 | permitted |
| `checkpoint_activity` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `temp_file_activity` | database | OK | 1 | final native matrix / v1 | permitted |
| `temp_file_holders` | filesystem | empty | 0 | final provider gate / v1 | withheld |
| `temp_spill_statements` | database | empty | 0 | final native matrix / v3 | permitted |
| `standby_replay_state` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `lwlock_waits` | cluster | empty | 0 | final provider gate / v2 | withheld |
| `xid_runway` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `wraparound_tables` | database | OK | 50 | final native matrix / v2 | permitted |
| `xmin_horizon` | cluster | empty | 0 | final provider gate / v1 | withheld |
| `autovacuum_cancellations` | external | policy_scope_denied | 0 | final native matrix / v1 | withheld |
| `wal_runway` | cluster | OK | 1 | final provider gate / v2 | withheld |
| `cluster_database_size` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `wal_directory` | filesystem | OK | 1 | final provider gate / v1 | withheld |
| `sequence_runway` | database | OK | 14 | final native matrix / v3 | permitted |
| `runway_trends` | database | empty | 0 | final native matrix / v2 | permitted |
| `stat_database` | database | OK | 1 | final native matrix / v1 | permitted |
| `stat_tables` | database | OK | 11 | final native matrix / v3 | permitted |
| `stat_statements` | database | OK | 50 | final native matrix / v1 | permitted |

## Original auxiliary sources

| Source | Scope | Native query/source result | Rows | Evidence/version | Default database policy |
| --- | --- | --- | ---: | --- | --- |
| `signal_target` | cluster | OK | 1 | final provider gate / v1 | withheld |
| `recovery_sample` | database | empty | 0 | final native matrix / v1 | permitted |
| `change_feed` | database | source_not_configured | 0 | final native matrix / not executed | permitted |
| `slo_status` | external | policy_scope_denied | 0 | final native matrix / not executed | withheld |
| `pooler_pools` | external | policy_scope_denied | 0 | final native matrix / not executed | withheld |
| `explain_statement` | database | empty (`unknown_statement`) | 0 | preflight; EXPLAIN not executed / v1 | permitted |

## Final dev.5 native functional proof

Evidence prefix `.local-test-results/gap-dev5-native-post-upgrade`: one complete
frozen-source invocation, eleven passing roots, stderr empty, raw SHA256
`d2a1b16894f69b83942de12a35716030bf537703a2d76383df4b28ae64b778d3`.
The sixteen provider gate sources above are all from this same invocation.
Actual Gateway investigation `4dfc43a8-e51b-4c73-adc8-60ba98221985` used three
model calls/four probes/6,884 tokens and accepted final advisory/inconclusive
output. Owned blocker and sequence fixtures detected and diagnosed their actual
signals; action remained constrained by policy/manual requirements.

The strict telemetry root passed all 24 records, observed nonempty metadata
in all ten streams, and normalized eight controlled plans, one log and one DDL.
A fresh SDK reread of exact tag `lbo_4b4c66c4af2046678913bbe2` and the original
**18:10:24.663553–18:11:20.655008 UTC** window found nine valid plans and one
intentional self-monitoring exclusion, with no masked/malformed/other rejection.
StatementID `01f1d48d-6ad2-1daa-9312-1befcf1e3fad`; no payload persisted.
The actual native maintenance lifecycle also verified retention holds, typed
rollback, exact-object absence, terminal cascade and chain-head tombstone.
The validated canonical state check stayed OID270343 across Bootstrap. See
[maintenance details](2026-10-30-sre-maintenance.md). None of this establishes
current-incident authority, telemetry completeness, a provider-log SLO denominator,
App-principal maintenance execution or positive App DDL capture.

## Current dev.5 App functional evidence

Final SNAPSHOT `01f1d48e0748107d95b5bdd1ffad0e22`, unchanged Linux binary
`2ca61a1a3fa16ef28a4b63ccb92033538684b6d99462811934b39f89a56e70b0`, passed
source/API/browser/Gateway and both actual history tool loops. Databricks-only
admin identity, service-principal OAuth M2M, database-only policy and metadata-only
consent remained active. The earlier empty DDL result below is historical.

| Source/path | Fresh dev.5 App result |
| --- | --- |
| `lakebase_plan_history` | Positive/cited; exact controlled event `0b225c479a7d78b3ec1f77fc1dde4a7b229bc3bd0dd1abcce69fd35269bbf45c` retained in evidence `d35d27c1-2949-4235-be97-3b73cef3bd06` |
| `lakebase_log_history` | Positive/cited available evidence `ce801232-6ea4-4dc9-8846-af7df221921c`; partial history, no complete error denominator |
| `lakebase_session_history` | 100 positive/cited observations, evidence `97f1868c-63d1-472e-9f88-c8d6792f148d` |
| `lakebase_ddl_history` | One positive/cited observation, evidence `81112243-2df8-4459-8fc1-8cf3dfa59015`; exact owned event `6a9390f2d66c28d816fa612173d27533dbec996b574125a6b1b52205b40f19c8` verified through scheduler/API/evidence/citation |
| `lakebase_database_history` | 100 positive/cited observations, evidence `e1754d75-adcc-422c-95bd-f092c572c2ba`; no live host admission |
| `application_slo_status` | Same binary's temporary SLO snapshot passed 15 real-time samples, both 20x burn windows, a cited 3-call/8-tool/10,040-token case, actual staleness at 123.06s and exact cleanup; final objective/sample/status/transition counts zero |

Plan/log case `e0f50f43-5e45-46d0-9701-ae1f8187a53d`: four Gateway calls,
four tools, 14,816 tokens, verified 14-event chain. ASH/DDL/metrics case
`ea555ad2-3ca6-48a3-bbcc-69d48f18ed9c`: three calls, four tools, 13,563 tokens,
verified 17-event chain. Both concluded advisory inconclusive with idempotent
readback and eight unchanged action/approval ledgers. All five history sources
now have positive observations and accepted citations. The fresh owned DDL
fixture was removed; its earlier interrupted attempt and recovery are recorded
in the [current acceptance](2026-10-30-v2.5-dev5-acceptance.md#final-app-history-and-slo-evidence).
App raw-plan-cache payload ingestion and workload-table maintenance remain
unproven; historical completeness/current incident authority are not claimed.

## Historical dev.4 App sources and independent functional evidence

| Source or path | Disposition |
| --- | --- |
| `lakebase_plan_history` | Final fixed-source actual provider plan → durable adapter → SRE evidence/hash/chain verified, including negative query ID; final deployed App model selected this source, retained the exact controlled provider event and cited the hashed evidence |
| `lakebase_log_history` | Final controlled native log normalization and typed SQLSTATE classification; final deployed App model selected and cited positive hashed history; partial-history semantics, no complete error SLO |
| `lakebase_session_history` | Twelve actual native ASH rows normalized in the separately bounded metadata check; local scoped/retention/durable-source tests; query-before-limit saturation regression passed; final App model executed and cited 90 retained observations |
| `lakebase_ddl_history` | Final controlled native DDL normalization; final App model executed the adapter and cited zero observations/`unsupported` as unknown; positive App DDL capture unproven, no App DDL fixture created; not a complete migration feed |
| `lakebase_database_history` | Actual interval/gauge normalization; local retained structured historical evidence; final App model executed and cited 100 retained observations; no live host admission |
| `application_slo_status` | Final native scoped push burn/case/replay/restart/stale lifecycle passed; patched App binary passed fifteen real-time samples, both burn windows, cited Gateway case and actual stale-to-unknown at 124.71 seconds; exact cleanup preserved the case |
| App telemetry Runtime.Poll → history API | Separate actual controlled metadata plan event ingested/read back, 34.326 s event-to-receipt; payload unavailable by configured consent |
| App investigator history → Unity Gateway → durable citation | Final SNAPSHOT `01f1d4832bf714c4a7007762cfe71ed1`, case `13168b8f-5658-4033-93ca-22dee108408e`: positive model-selected plan/log history, exact controlled provider plan event, accepted hashed citations, actual `stat_database`/`lock_graph`, three model calls/five tools/9,661 tokens, final inconclusive, 17-event chain, idempotent readback and eight unchanged ledgers; metadata-only consent unchanged |
| App remaining history → Unity Gateway → durable citation | Same final SNAPSHOT, case `173eea86-0fc1-43cf-b0cc-a091b010fadb`: model executed/cited sessions (90 rows), database metrics (100 rows) and DDL (0 rows/`unsupported`, absence unknown); three model calls/four tools/13,265 tokens, final inconclusive, 17-event chain, idempotent readback and eight unchanged ledgers; no App DDL fixture or configuration changes |
| Operator statistics maintenance | Final native approval/execution/structural check/exact-object rollback verified; App-principal workload-table execution remains unproven |

`stat_tables` v3 includes physical database/table OIDs. The final native source matrix and maintenance acceptance exercise that version. The final optional query-ID history regression independently fails before the fix with 130 newer distractors for both plan/ASH sources and passes after parameterized signed/unsigned filtering before LIMIT, while preserving scope, retention and policy checks.

The fresh final strict telemetry root passed **all 24 records** and observed nonempty rows in **all ten metadata streams**. Controlled payload delivery returned eight valid plans, one valid log and one valid DDL. A new independent read of the **same final application/resource/database and original time window** found ten plans: **nine valid and one intentional self-monitoring exclusion**, no masked/malformed/other rejection. This is fresh final-run classification, not reuse of the earlier candidate. Capture/delivery remains asynchronous partial history without a completeness or latency SLA.

The separate final App history proof is `gap-dev4-app-sre-history-filter-final.json` (`checks.complete: true`). Its plan evidence is `4cdcaaef-03c3-499c-8ec2-451ab49de6ec`, SHA-256 `0b91bdaa0643c46e66e017b6b48f814193604cb8ad70cc54649a2e65f1af241f`, containing controlled event `0b225c479a7d78b3ec1f77fc1dde4a7b229bc3bd0dd1abcce69fd35269bbf45c`. This accepts retained structured history under `allow_payload: false`; it does not establish App raw plan/log cache access, complete history, a database cause, remediation or workload-table maintenance execution.

The additional `gap-dev4-app-sre-remaining-history-final.json` passed (`checks.complete: true`) on that same SNAPSHOT. Its cited evidence IDs are `a954af76-03dd-4830-9296-c89309a9a8d8` (sessions), `1415f9e0-49b4-423c-b278-9d4cdfb3433a` (database metrics) and `08b4082b-0bde-4253-a39f-d921c6b9760a` (DDL unknown/unsupported). Both cases together establish actual execution and accepted citations for all five history adapters, with positive observations for four. They do not establish positive App DDL capture; native controlled DDL delivery remains separate.

Evidence: `.local-test-results/gap-dev4-native-history-filter-sre-matrix.json`, `gap-dev4-native-history-filter-provider-probes.json`, `gap-dev4-native-history-filter-telemetry.json`, `gap-dev4-native-history-filter-plan-normalization.json`, `gap-dev4-provider-sink-history-filter.json`, plus separate `gap-followup-metadata-normalization.json` and `gap-dev4-app-scheduler.json`. See [current acceptance](2026-10-30-v2.5-dev5-acceptance.md), [provider findings](2026-10-30-provider-compatibility-gaps.md), [App SLO acceptance](2026-10-30-app-slo-acceptance.md) and [maintenance review](2026-10-30-sre-maintenance.md). Raw result directories contain private files and must not be attached wholesale.
