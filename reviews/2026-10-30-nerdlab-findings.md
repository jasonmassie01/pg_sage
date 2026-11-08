# Lakebase finding scenarios and remaining coverage

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

Two native runs against the isolated `<test-project>/<native-test-branch>` branch on PostgreSQL
18 now positively exercise **nine real finding families with normal recovery**.
The eight-condition catalog/work_mem run passed in 99.92 seconds; the separate
production-default slow-query/recovery run passed in 65.86 seconds. Both used the
actual collector, analyzer and durable findings path. No findings were inserted,
no generated actions enabled, no global SRE poller run, and no provider settings
changed. Rule thresholds stayed at production defaults; the fixture processes
used 2 second collection/analysis intervals, a bounded 10 second catalog timeout,
and disabled model advisors. This is positive functional proof of those
conditions, not a certification of every PostgreSQL incident.

Native evidence: `.local-test-results/finding-scenarios-native.log` and
`.local-test-results/default-slow-query-native-final.log`.
The focused local race run also passed 12 evidence cases and a durable lifecycle
test with seven unknown/ongoing subcases, zero skips:
`.local-test-results/temp-spill-focused-live.log`.

## Positive fixture proof

| Category | Actual input | Correction/control proved |
| --- | --- | --- |
| `duplicate_index` | Two ordinary btree indexes on the same column | Drop one fixture index; finding resolves |
| `missing_fk_index` | 12000 child rows, FK with no leading-column supporting index | Create supporting index; finding resolves |
| `stale_statistics` | 12000 rows, never analyzed; autovacuum disabled only on the owned test table | ANALYZE; finding resolves |
| `table_bloat` | 12000 rows then 9000 deletes, autovacuum disabled only on owned table | Non-FULL VACUUM/ANALYZE; finding resolves |
| `seq_scan_heavy` | 110000 rows and 105 selective sequential scans without an index | Fixture table removed; finding resolves. This does not demonstrate that an index improved a real workload |
| `sequence_exhaustion` | Owned ascending integer sequence at 950 of 1000 | Expand only that sequence's maximum; finding resolves |
| `invalid_index` | Owned concurrent unique-index build fails on actual duplicate values | Invalid catalog state observed across analyzer cycles; drop fixture index, finding resolves |
| `slow_query` | One constant-result pg_sleep(1.2), fresh queryID preflight and measured mean 1202.24 ms against the unmodified 1000 ms threshold | One zero-sleep call of the same normalized statement lowers its cumulative mean; production analyzer resolves the finding, then exact own finding/sample cleanup |
| `temp_spill` | Real sort spills under session-local 64 kB work_mem; actual EXPLAIN reports disk and pg_stat_statements temp counters increase | Same fixture query under 64 MB does not spill; a new observed non-spilling execution resolves its finding |

The 64 MB control is a measurement on this small synthetic workload, not a proposed
production setting. A lower work_mem can be appropriate under concurrency.
No recommendation was automatically executed.

## Deployed App proof

The deployed dev.6 App's own collector/analyzer also produced all eight open
finding families at **20:20:22 UTC**, using the same controlled fixture families:
duplicate index (id 6), missing FK index (7), bloat (8), stale statistics (9),
sequential scans (10), invalid index (11), sequence pressure (12), and the new
temp-spill/work_mem advice (13). These are actual durable analyzer findings, not
seeded metadata or fabricated SRE cases. The sequence became visible after the
explicit narrow fixture grant described below.

A separate one-shot constant-result App query subsequently crossed the unchanged
1000 ms slow-query threshold: one call, mean **1201.920908 ms**, durable finding
**id 14**. The actual Cases API projects it as
`finding:pg_sage:slow_query:query:queryid:-5191732810777595558`. At that captured readback, **nine new fixture families were visible among 13
Cases work items**. Current totals are dynamic and must be taken from a fresh
readback; these counts describe that proof, not a permanent visibility guarantee. Pre-existing
slow-query id 4 was preserved; its similar mean is not a leaked native result.
Direct endpoint/database findings and scoped Cases readbacks confirm the App
contains only its own controlled label, while the native endpoint has separate
query statistics. Cloned branches can share a physical database OID; scope must
also include endpoint/branch and database identity.

The slow query used no workload table scan or statistics reset, with a five-second
statement cap and only 1.2 seconds of requested sleep. Non-ANALYZE EXPLAIN obtained
the exact queryID beforehand; existing pg_stat_statements/query_store collisions
would abort before execution. Output aliases do not determine queryID, so the
repeatable native proof also uses a harmless distinct constant-expression shape.
Private additional evidence: `nerdlab-app-slow-query.json`,
`slow-query-scope-api.json` and `slow-query-scope-diagnosis.json`, under
`.local-test-results/`. The App slow-query counter/finding remains for UI review;
no active workload generator remains.

Private evidence: `.local-test-results/nerdlab-app-findings-mid-exercise.json`,
`nerdlab-app-exercise.log`, `nerdlab-app-narrow-grants.json` and the fixture
manifest. Four paced low-memory queries completed with no errors; the cumulative
110 scan budget is exhausted. The App fixtures remain for browser/operator review
and later guarded cleanup. This does **not** claim App-SP workload maintenance or
App recovery from executing generated recommendations; the successful correction
and resolution experiments above ran on the isolated native test branch. The
initial native slow-query probe was also neutralized with a zero-sleep call,
leaving a non-slow mean of 601.12 ms and zero own slow-query findings; neither
global nor selective resets were used.

## Restart coverage gap found and corrected

A later dev.7 restart exposed a real lifecycle bug. The exact owned index
`nerdlab_20261030.invalid_a` remained invalid (OID 201289, indisvalid=false,
indisready=false, indislive=true, no active build), but finding id 11 was resolved
at 20:37:50.400132 UTC after its last pre-restart observation at 20:37:16.480978.
All eight inspected collector cycles from 20:35:16 through 20:41:43 persisted an
indexes category. This was not an index correction or missing catalog input.

The invalid-index detector deliberately waits for two observations and keeps
first-seen state in memory. On restart, its first observation withheld output,
but category-wide resolution incorrectly interpreted that warmup absence as
clearance. Later ordinary detection reopened the same index as new finding
id 18 (last_seen 20:41:43.998451), so this was a false-clear/row-split gap rather
than permanent loss. No finding was manually reinserted or repaired.

The narrowly scoped fix marks only invalid_index resolution unknown while a
current invalid index is warming up or its table has an active build. It runs
before the rule records its first observation. Positive findings for other
confirmed indexes can still emit; new detections still require two observations;
the existing failed-build-probe guard remains intact. A local durable race test
passed restart warmup→same open identity→second confirmation→observed valid
index recovery, plus active-build and failed-probe uncertainty. Independent
review and the existing invalid-index/catalog tests also passed. Final
[dev.8 deployed acceptance](2026-10-30-v2.5-dev8-acceptance.md) also passed:
same row id 18 remained open across 17 read observations and the restarted
analyzer refreshed its occurrence count from 10 to 11 without a false clear or
row split. The final captured App read includes all nine fixture families among
13 Cases work items; model-produced bloat advice is visible separately and was
not executed.

Evidence: `.local-test-results/invalid-index-restart-lifecycle.log`,
`nerdlab-dev7-invalid-index-diagnosis-initial.txt` and
`nerdlab-dev7-invalid-index-snapshot-availability.json`.

## Work_mem gap closed, with explicit limits

Previously, temp-block evidence fed a hint-dependent tuner path. Lakebase can
install `pg_hint_plan` while its hook remains unavailable, so that path did not
provide a working native work_mem recommendation. The new Tier 1 `temp_spill`
finding needs no hint extension, plan cache, standard auto_explain, or model call.
It reports at least 128 new temporary blocks in comparable consecutive snapshots,
with fresh calls and known matching global statistics epochs.

The advice asks for controlled EXPLAIN/BUFFERS and bounded SET LOCAL experiments,
considers index/query alternatives, and explains memory budgets per operation,
parallel worker and concurrent session. It supplies **no executable SQL/value**
and explicitly says that no active hint/value was validated. Lakebase permits
user-context settings at session, role and database scope; changing a broader
scope still requires separate workload validation and an exact restore plan.

Generic category-wide absence never resolves these findings. A query must
remain observable, have newer calls and unchanged temp read/write counters to
clear. Missing/topN-truncated/evicted samples, inactivity, unknown epochs,
changed epochs, visible individual counter decreases, and small continuing
spills stay unknown/open. Local durable lifecycle tests cover those observable
conditions. **Counter comparisons infer an interval; they do not prove
per-entry lineage.** A selective reset/eviction followed by regrowth between
samples can escape the visible guards. Complete selective-reset safety would
require per-entry lineage metadata; it is not claimed here.

Table/sequence/index findings use object names backed by catalog metadata; query findings
use `queryid:<id>`. Filtering every finding by fixture schema prefix therefore
does not find every query finding. The harness also checks the exact queryID
and controlled query text.

## Complete analyzer inventory: what the nine fixtures do not prove

These rows inventory the registered deterministic rules plus database/late
checks. "Not induced" means the input may be available, but this run did not
create that incident. It does not mean the rule is unsupported by Lakebase.

| Category/path | Lakebase input and current evidence | Remaining limitation or deliberate boundary |
| --- | --- | --- |
| `unused_index` | Catalog/counter reads work; local rule/reset/standby tests exist | Default 7 day clean evidence window not accelerated; no fresh positive native default-window claim. Unknown standby usage withholds automatic DROP |
| `duplicate_index` | Exact duplicates positive above | Subset/covering/constraint-protection cases have local tests; not all variants newly induced natively |
| `missing_fk_index` | Positive above | General non-FK missing indexes use seqscan/optimizer paths. FK table-name ambiguity fails closed |
| `invalid_index` | Positive above | Active-build/failed-probe exclusions preserved; not every cancellation/failure mode injected |
| `table_bloat` | Positive above | Dead-tuple heuristic is not physical bloat measurement. pgstattuple separately works on owned fixtures |
| `autovacuum_tuning` | Per-table reloption SQL previously verified on native owned fixtures | Default needs at least 1 million live rows plus churn; not induced inside this 150000 row demonstration budget |
| `stale_statistics` | Positive never-analyzed branch above | Seven-day age branch and correlation/extended-statistics tuning need independent evidence |
| `wraparound_freeze` | Table XID-age source readable | Default 150 million XID age intentionally not created; no transaction-flood test |
| `xid_wraparound` | Database/table XID/runway sources readable | 500 million warning/1 billion critical not induced; no attempt to endanger the database |
| `slow_query` | Fresh native default 1000 ms positive and normal recovery; new App finding and Cases work item id 14 | Constant-result sleeping statement proves the latency rule without CPU/table load. It does not reproduce every real query performance cause |
| `high_plan_time` | pg_stat_statements planning fields are conditional | Requires populated planning counters and sufficient calls; no privileged track_planning activation or new positive native claim |
| `temp_spill` | New positive interval finding and recovery above | Interval inference/lineage caveat above; no automatic safe memory value inferred |
| `seq_scan_heavy` | Positive above at default 100000 row threshold | Advisory heuristic, not proof that every seqscan needs an index |
| `high_total_time` / first-cycle supplement | Counter snapshots and deltas available | New sustained high-frequency incident not induced; local rule coverage is separate from positive live workload diagnosis |
| `query_regression` | Current and retained historical query samples available | Needs comparable history over configured lookback; no new default 7 day regression experiment |
| `sort_without_index` | Requires suitable captured local EXPLAIN plan | Provider plan-history rows are intentionally excluded; metadata-only App history is not raw-plan parity |
| `plan_regression` | Local plan-pair/diff logic tested; native provider capture bridge separately verified | Needs comparable eligible local plans; do not treat asynchronous provider metadata as a tested automatic plan-regression feed |
| `cache_hit_ratio` | Database counter source available | No artificial cache-thrashing test; read ratio alone does not prove current host memory pressure |
| `checkpoint_pressure` | Provider gate can read checkpoint/WAL statistics | No forced checkpoint storm; provider-managed instance settings cannot be tuned with ALTER SYSTEM |
| `stat_statements_pressure` | Extension capacity/statistics sources available | No query-cache saturation/eviction flood injected |
| `connection_leak` | Prior controlled idle transaction/SRE source verified | Default 30 minute analyzer finding not held for 30 minutes here |
| `lock_chain` | Prior scoped native blocker/idle investigation verified | No new broad/global lock poll; App default policy stays database-only |
| `sequence_exhaustion` | Positive ascending integer sequence above | Custom descending ranges are incompletely represented by the collector; no universal sequence-range claim |
| `replication_lag` | Readable native source, healthy empty observations in provider gate | No replica outage/lag injection; cluster-scoped source withheld by default App read policy |
| `inactive_slot` / `slow_replication_slot` | Native replication-slot source readable | No task-created orphan/retention-pressure slot; never drop provider/user slots to force resolution |
| `work_mem_promotion` | Existing role-promotion rule joins active Set(work_mem) hints | Active hint prerequisite remains unavailable on this Lakebase configuration. New hint-free temp_spill advice is the supported alternative, not active-hint promotion parity |
| `extension_drift` | Catalog/config comparison path available | No removal/change of another extension to trigger a demonstration |
| `sage_footprint`, `sage_self_cost`, `sage_self_budget` | Agent metadata/query accounting paths have local coverage | No metadata/disk/CPU pressure injection merely to fire these safeguards |
| Clone-family advisories | Catalog/table-clone heuristics and local tests | No new clone-fanout demonstration; unrelated existing schemas are excluded from fixtures |
| Model advisors/forecasters/tuning producers | Earlier real Unity Gateway/native/App tool and advisor workflows verified | Optional, separately budgeted/configured producers; not exhaustive new forecasting, partition, rewrite, agent-posture or vector-candidate scenario coverage |

Real disposable index trials and their rollback work. HypoPG remains unavailable;
this is not hypothetical-index equivalence. Standard session auto_explain remains
unavailable through the tested role/configuration. Provider advanced telemetry
is a separate working asynchronous history path; it does not turn on the
standard auto_explain hook or complete every plan-dependent tuning feature.

## SRE inventory and cases

An analyzer finding is not automatically an SRE case. This fixture run proves
durable findings and their normal corrections. It does not manufacture operator
cases or imply that every finding family has an automatic case trigger.

The exhaustive 32 catalog + 6 auxiliary query/source table and source-by-source row
counts are retained in [native SRE source matrix](2026-10-30-native-sre-source-matrix.md).
Every family remains explicitly accounted for:

| Source family | Current positive evidence | Remaining boundary |
| --- | --- | --- |
| Database identity/statistics/tables/statements | Native typed sources available | Source reads do not alone prove an incident |
| Database lock graph, long transactions, prepared transactions | Positive owned blocker/idle diagnosis previously verified | Healthy/empty prepared-xact reads are not a positive prepared-transaction incident |
| Temp-file activity/statements/holders | Database activity readable; new actual spill fixture above | Filesystem holder source is outside default App policy; no live current-incident claim from provider history |
| Vacuum progress, wraparound tables, autovacuum age, XID/xmin runway | Typed age/runway/table sources readable | Dangerous wraparound/vacuum-starvation/flood conditions not injected; cluster xmin sources withheld by default |
| Sequence runway/trends | Positive owned near-limit sequence diagnosis previously verified | Long-horizon trends need genuine retained history; custom-range caveat remains |
| WAL/checkpoint/archive/directory/runway | Exact native provider-gate reads completed | Cluster/filesystem sources withheld by default App policy; no failover/WAL exhaustion or storage-pressure injection |
| Replication lag/slots/standby replay | Exact native provider-gate reads completed | No controlled replica failure, inactive-user-slot deletion or HA certification |
| Connection saturation/LWLock/signal target | Exact native provider-gate reads completed | Cluster sources withheld by default; no saturation or broad cancellation/termination test |
| Plan regressions/sage actions/recovery sample | Native queries available; some healthy empty | Positive plan regression/action recovery not inferred from empty sources |
| Change feed, pooler pools, autovacuum cancellations | Adapter/policy state explicitly reported | Unconfigured or external/policy-denied inputs are not supported positive feeds |
| explain_statement | Guarded preflight verified | Earlier unknown_statement result did not execute EXPLAIN; owned EXPLAIN fixtures above are separate evidence |
| Advanced plan/log/ASH/DDL/database history | All five App adapters previously returned positive, model-cited observations | Asynchronous partial evidence, metadata-only App consent, no completeness/latency SLA, no live host-admission authority |
| Application SLO | Actual counter ingestion→burn→case→stale-to-unknown lifecycle previously verified | Provider logs cannot supply a complete aligned error denominator; default-hour endurance not newly certified |
| Typed maintenance | Native owned-table statistics proposal/approval/execution/rollback verified | App remains observation-only; user-owned fixture catalog visibility is not App-SP workload maintenance authority |

See [provider restrictions](2026-10-30-provider-compatibility-gaps.md),
[maintenance evidence](2026-10-30-sre-maintenance.md), and
[dev.5 acceptance](2026-10-30-v2.5-dev5-acceptance.md) for the separate complete
history, SLO, Gateway, SQL-family and retention tests. Current UI/deployment
acceptance must be read separately from those retained proofs.

## App demonstration scope and cleanup

The reviewed reproducible harness is `scripts/lakebase-nerdlab.py`. It uses
the selected operator’s existing Databricks profile/OAuth, verified TLS, and only schema
`nerdlab_20261030` in the isolated `<project>/<development-branch>`/`pg_sage` database.
The similarly named `pg_sage_...` schema was avoided because query self-monitor
filters intentionally exclude that application marker. The actual seeded fixture
occupies 13,787,136 bytes (about 13.15 MiB), including its indexes. All objects are
owned by the selected operator. To expose the controlled sequence value, only schema USAGE and
SELECT on that single sequence were granted to the existing App service
principal; table SELECT, sequence USAGE/UPDATE and ownership were not granted.
Those narrow grants disappear with the disposable schema/sequence.

The harness refuses reuse, records database/schema OIDs and all relation owners
and OIDs, and stays at or below 110 read scans cumulatively: 105 selective seqscans, one
non-spilling baseline sort, and at most four low-memory sorts. The four-sort
exercise is paced over its requested window (180 seconds gives 0/45/90/135 seconds)
after an observed deployed collector baseline. It reserves budget before each
call and holds a process lock. No workload generator remains in the background. The separate one-shot slow
query does not read a workload table or consume that exhausted scan budget. Table autovacuum is disabled only for these disposable stale/bloat
fixtures. Statements are capped at 20 seconds with 5 second lock timeout.

Private inventory/evidence lives in
`.local-test-results/nerdlab-app-fixtures.json`. The cleanup command is:

```sh
python3 scripts/lakebase-nerdlab.py cleanup
```

Cleanup locks the six exact tables and sequence in a transaction, rechecks the
database/schema/object identity and ownership, then uses plain DROP TABLE,
DROP SEQUENCE and DROP SCHEMA with RESTRICT. A changed object or external
dependent aborts cleanup. It does not delete retained findings/cases/evidence or reset query statistics;
the constant-result slow-query counter is separate from the disposable schema.
It grants no broad privileges, changes no execution trust, and runs no global
retention cleanup.

## Documentation research

The public [Lakebase compatibility documentation](https://docs.databricks.com/aws/en/oltp/projects/compatibility)
confirms that user-context parameters can be set at session/database/role scope,
instance settings are managed, superuser/host access is unavailable, and
pg_stat_statements counters do not survive compute restart/scale-to-zero.
The positive fixture observations above validate only the recorded shipped behavior; they do not establish unavailable hooks or complete telemetry.
