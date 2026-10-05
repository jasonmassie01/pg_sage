# Phase 3 "Managed clouds": host telemetry and parameter-group proposals (report)

Branch `claude/p3-managed-clouds` (from `origin/release/v2.1.0`). Roadmap:
`reviews/2026-10-02-ai-next/ROADMAP.md`, Phase 3 "Managed clouds": CloudWatch / Performance
Insights + parameter groups for RDS/Aurora, Cloud Monitoring + flags for Cloud SQL; "unlocks
autonomy that today is withheld for lack of host telemetry".

## What was built

### Host telemetry behind one interface (`internal/cloudtel`)

`Source` (`Provider()`, `Collect(ctx, now) (Sample, error)`) with two adapters and one
`Runtime` per database.

| File | What |
| --- | --- |
| `sample.go` | `Point` (value + provider timestamp), `Sample` (unknown = nil, never 0), distinguishable errors (`ErrUnavailable` > `ErrNoCredentials`, `ErrIdentity`; `ErrAuth`, `ErrThrottled`, `ErrClockSkew`, `ErrMalformed`, `ErrProvider`), freshness (10 min max age, 5 min future skew), `CPU`, `HostMemory`, `EffectiveFreeStorage` (free + autoscaling headroom). |
| `awsid.go` | `ParseRDSHost`: instance and Aurora cluster (writer/reader) endpoints, incl. `.cn`; proxies, custom endpoints, IPs and custom DNS never resolve (no discovery). |
| `awshttp.go` | SigV4 (`aws-sdk-go-v2/aws/signer/v4`, already a dependency) over plain HTTPS; credentials from the SDK default chain, cached, never in errors; server `Date` skew check; Query-XML and JSON error classification. |
| `cloudwatch.go`, `aws_metrics.go` | `GetMetricData` (Query protocol): CPU, FreeableMemory, FreeStorageSpace (RDS only), Read/Write IOPS and throughput, DatabaseConnections, `ReplicaLag` per read replica (max), `AuroraReplicaLagMaximum` (ms -> s). Newest fresh point per metric, invalid values are reported missing. |
| `pi.go` | Performance Insights `GetResourceMetrics` (JSON 1.1): `db.load.avg`, grouped by `db.wait_event_type`, and `os.memory.total.avg` (KB). PI is optional: its failures are partial data. |
| `rdsapi.go` | Official RDS SDK (already in go.mod): `DescribeDBInstances`, `DescribeDBClusters` (Aurora writer), `DescribeDBParameters` (paged); retries off (we poll every minute). |
| `classes.go` | Documented RAM per RDS instance class (t3/t4g, m5..m8g, r5..r8g, x2g); unknown = 0. |
| `aws_source.go` | `AWSSource`: credentials pre-check (nothing is sent without them), CloudWatch required, PI optional, memory from PI else the class table, storage capacity = max(allocated, autoscaling max), Aurora = auto-growing (no storage guard). `Target()` implements `managedparam.Resolver`. |
| `gcp_auth.go`, `gcp_auth_sa.go` | Google ADC chain without new dependencies: `GOOGLE_APPLICATION_CREDENTIALS` (service-account key: RS256 JWT bearer signed with the standard library; `authorized_user`: refresh grant), the gcloud ADC file, the metadata server (2 s probe). Tokens cached until a minute before expiry; `external_account` reported unsupported; token-endpoint errors classified (clock skew from `invalid_grant` iat/exp, auth, throttling). |
| `gcp_source.go`, `gcp_monitoring.go`, `gcp_apply.go`, `gcp_sqladmin.go` | Cloud Monitoring `timeSeries.list` (one `one_of` query for every metric of the instance and its replicas): CPU (fraction x100), memory quota/usage, disk quota/bytes_used, disk ops (DELTA / interval), backends (summed over databases), replica lag (max). Cloud SQL Admin `instances.get` / `instances.list` (match the connected IP within the project); tier memory fallback (`db-custom-*`, `db-n1-*`); auto-resize: unlimited = unbounded, a limit = the capacity. `Target()` returns the database flags. |
| `guards.go` | `Limits` (30 s lag, 10% free, 24 h runway, 5% memory; 0 disables), `StorageRunway` (least squares over 24 h, >= 6 points over >= 30 min), `Withhold` (reasons only; unknown or stale data adds none). |
| `runtime.go`, `status.go` | `Runtime`: polls every 60 s, fails closed on error (sample dropped), storage history restarts on a capacity change, implements `executor.HostCPUReader`, the new `executor.HostGuardReader`, `autonomy.DiskCapacityProvider`'s shape, `HostMemory()`; `Status()` (deep copy) and a registry for the API. `Unavailable(db, provider, reason)` for "unavailable: <reason>". |

### Managed parameter changes (`internal/managedparam`)

| File | What |
| --- | --- |
| `intent.go` | `Intent` (provider, parameter, value in PostgreSQL form, reason) carried in a finding's `detail.managed_change`; strict validation (no spaces, quotes or shell metacharacters reach a command). |
| `units.go` | `ProviderValue`: memory/time to the parameter's base unit (shared_buffers in 8kB pages, work_mem in kB, ...), whole units only, documented safe range, booleans as RDS `1/0` or Cloud SQL `on/off`. |
| `proposal.go`, `cli.go` | `Build`: typed `Proposal` with mechanism (`parameter_group` / `database_flag`), target, value and unit, current group/flag value and running value, apply method (`immediate` / `pending-reboot` from the group's ApplyType, else PostgreSQL's restart list), `RebootRequired`, `Rollback` (restore the user value or `reset-db-parameter-group`; Cloud SQL: the prior flag list or `--clear-database-flags`), exact CLI (+ `reboot-db-instance` when static), console URL, notes and blockers (default `default.*` group cannot be modified; Cloud SQL flags unknown -> `--database-flags` would clear the others). `RequiresApproval` is always true and `AutoApply` always false (`AutoApplyWithheld` says why). |
| `drift.go` | `DetectDrift`: operator-set group parameters / flags vs `pg_settings` -> `pending_reboot`, `overridden` (database/role/session source) or `mismatch`; formulas and engine defaults skipped. |
| `store.go`, `store_lifecycle.go` | `sage.managed_change_proposals` (new idempotent migration `internal/schema/managed_change_migration.go`, one registration line): upsert by fingerprint (refresh; supersede an older value for the same parameter), 7-day rejection cooldown, `Decide` (single winner under races), `SupersedeExcept`, `MarkApplied`. |
| `worker.go`, `worker_settings.go` | Every 5 min: open findings with an intent -> proposals (placeholders and a note when the target is unknown), "applied" when PostgreSQL runs the value, superseded when the finding is gone, drift against the resolved target (cached 15 min). |

### Where telemetry is used

- **Host memory guard** (`internal/advisor/host_memory.go`, `GateConfigFindingsHost`): live host
  memory reaches the configuration gates for both the advisor (`WithHostMemorySource`) and the
  tuning agent (`Settings.HostMemory`). `shared_buffers` is grounded (<= 40% of RAM, existing
  rule) instead of always advisory; `work_mem` above 5% of RAM is refused; while available
  memory is under 5% no memory setting may grow (lowering stays executable). Unknown memory
  keeps today's behavior exactly.
- **Managed forms** (`internal/advisor/managed_change.go`, two lines in `TransformForCloud`): on
  RDS, Aurora and Cloud SQL a restart-required or provider-restricted setting keeps its
  advisory finding and gains a typed intent plus `approval_required`. The tuning agent's
  managed `shared_buffers` proposal is `redirected` (like source-fix packets) instead of
  refused as "not executable here".
- **CPU headroom for maintenance windows**: the runtime is the executor's host CPU reader, so
  index-build load admission has measured CPU (unknown CPU is admitted only inside a window).
- **Replica lag / storage runway / memory pressure**: `verify.LoadEvidence.HostWithhold` (a
  string, so the struct stays comparable) and `ReasonHostTelemetry`; the executor reads it from
  a reader that implements `HostGuardReader`. It can only withhold.
- **Findings**: `managed_parameter_drift` (info; mismatch = warning) and
  `managed_storage_runway` (a week ahead; critical inside the 24 h floor), resolved when
  cleared, untouched when telemetry is unknown.
- **API** (`internal/api/managed_cloud_routes.go`, one router line): `GET
  /api/v1/managed-changes`, `POST /api/v1/managed-changes/{id}/approve|reject` (operator/admin;
  never applies; response says `applied_by_pg_sage: false`), `GET /api/v1/cloud-telemetry`.
- **UI** (`web/src/pages/actions/ManagedChanges.jsx` on the Actions page): approval cards with
  the command, reboot requirement, rollback, blockers, notes and console link; "I will apply it"
  / Reject for operators. Dist rebuilt.
- **Config** (`internal/config/cloud_telemetry.go`): `cloud_telemetry:` (enabled by default,
  poll interval, the four limits, `aws.region|db_instance_identifier|db_cluster_identifier`,
  `gcp.project|instance`; names are standalone-only) and `SAGE_*` env overrides.
- **Wiring** (`cmd/pg_sage_sidecar/cloud_telemetry_{runtime,wiring,findings}.go`, small hooks
  in `database_runtime*.go`).
- **Retention**: closed proposals age out with actions; open ones are kept.
- **Docs**: `docs/managed-clouds.md` (permissions, config, live check).

## Product calls (made by the lens "earns trust, then becomes autonomous")

1. **Managed parameter changes are never auto-applied in this PR.** Applying one needs provider
   write permissions (`rds:ModifyDBParameterGroup`, `cloudsql.instances.update`) pg_sage does
   not otherwise ask for; most of these settings need a reboot or Cloud SQL restarts the
   instance (an outage, not reversible in the "verify then roll back" sense); and the trust
   ledger has no `managed_parameter` action class with promotion evidence. So they are
   approval-only (structurally: no executor path can run them; `AutoApply` is pinned false and
   mutation-tested). Approval means "I will run it"; pg_sage verifies by reading the running
   value back and marks the proposal applied. A later release can add an adapter (like the
   Azure ARM one) and an action class that earns L1/L2 from these observed outcomes.
2. **Telemetry is on by default, credentials optional.** It is read-only; without credentials
   the status says `unavailable: <reason>` and every guard behaves as before.
3. **Unknown never withholds; known can.** Missing, stale or partial telemetry adds no withhold
   reason (status quo); fresh telemetry can only add reasons. Telemetry never touches a trust
   level, the policy gate or the ledger (enforced by an import-graph test and a randomized
   never-widen property against `verify.DecideAdmission`).
4. **Measured CPU admits index builds outside a window** under the existing 70% ceiling: that
   is the "autonomy withheld for lack of host telemetry" the roadmap targets. It changes the
   evidence, not the trust: a class still needs its earned level to run unattended.
5. **Memory guards**: `work_mem` <= 5% of RAM and a 5% available-memory floor (no growth of any
   memory setting). Chosen conservative; both disabled when memory is unknown.
6. **Identity from the connection only; no account-wide discovery.** RDS/Aurora from the
   endpoint host; Cloud SQL by matching the connected IP within one project. Reader endpoints,
   RDS Proxy and custom DNS report unavailable (standalone mode can name the resource).
7. **Storage**: RDS autoscaling headroom counts as free (capacity = MaxAllocatedStorage);
   Aurora and unlimited Cloud SQL auto-resize are unbounded, so no storage guard or runway.
8. **Cloud SQL flags**: proposals carry the full current flag list because `--database-flags`
   replaces it; without the current flags the proposal is blocked.
9. **Default parameter groups**: proposed with a blocker that explains creating and attaching a
   custom group (itself a reboot).
10. **Rejection cooldown 7 days**; a value already running is marked applied, never re-proposed.
11. **Polling 60 s** (CloudWatch bills per metric, about 10 per poll; configurable 30-3600 s).

## Spec CHECKs covered

No numbered CHECK of `reviews/2026-09-26/AI-SRE-SPEC.md` is specific to managed clouds. The
work follows its rules: unavailable telemetry is shown, not hidden (`GET
/api/v1/cloud-telemetry` says `unavailable: <reason>`; spec UX section); stale evidence fails
closed (a stale or failed sample is unknown CPU, so admission falls back to the maintenance
window; in the spirit of CHECK-40); every executed action still goes through `policy.Gate` /
`Executor.Apply` (managed changes are never executed); evidence carries provider timestamps;
outcomes are verified (applied = the running value observed); no unearned autonomy
(never-widen tests).

## Test results

**Command:** `go test -json -count=1 -p 2 -cover -timeout 60m ./...` (golang:1.25,
`--cpus=2`, PG17 `pgsage-ag3` :55473, after merging the latest `origin/release/v2.1.0`)
**Total:** 13848 passed, 1 failed, 22 skipped

**Coverage (touched packages, PG17):** cloudtel 87.1%, managedparam 89.3%, advisor 83.3%,
tuning 91.4%, verify 89.5%, executor 87.9%, config 91.7%, api 78.9%, schema 84.2%, retention
87.3%, store 76.1%, cmd/pg_sage_sidecar 78.5%.

**Other runs:**
- PG14 (:55414) and PG18 (:55418), touched packages (cloudtel, managedparam, advisor, tuning,
  verify, executor, config, api, schema, retention, store, cmd/pg_sage_sidecar): all pass,
  same coverage (PG14 cmd 78.6%, api 78.9%; PG18 api 79.0%).
- `-race` on cloudtel, managedparam, verify, advisor, tuning, config, plus the new executor,
  API and wiring tests: pass, no races.
- e2e (`-tags=e2e`, PG17): `ok github.com/pg-sage/sidecar/e2e 174.7s`.
- Perf gate (`-tags=perfgate`, `PG_SAGE_PERF_SCALE=small`, PG17): `--- PASS: TestPerfGate
  (164.60s)`.
- golangci-lint `./...`: 0 issues. ESLint on the changed web files: clean.
- vitest: 83 files, 484 tests passed (4 new ManagedChanges tests). `npm run build`: dist
  rebuilt and committed.
- Mutation testing: 35 of 35 mutants killed (see the audit).

### Skipped Tests (must be zero or justified)
22 skips, all pre-existing environment gates, none in a touched package: live provider and
LLM tests (`PG_SAGE_LIVE_*`: agentdb 6, azure 2, llm 2, rca 1), PgBouncer / standby /
restartable-server fixtures not provided (6), the bench and its live arm (2), plan-fixture
regeneration (1), an RCA helper process (1), a Windows-path case on Linux (1). The new
`cloudlive` tests are behind a build tag (not compiled in CI) and are owner-run.

### Failures
- `internal/mcp: TestToolReferenceDocsMatchSchemas` -- pre-existing and environmental: the
  test looks for a begin marker ending in `\n`, but this Windows checkout (`core.autocrlf=true`)
  has `docs/mcp.md` with CRLF. Untouched by this branch; passes on LF checkouts (CI).
- Flaky on the shared Docker VM during the first full run, not on reruns:
  `cmd/pg_sage_sidecar: TestFleetReloadHotSettingsApplyInPlace` and (second run)
  `TestFleetReloadUnreachableAddIsRegisteredFailed` -- both "dial error: timeout" creating a
  fixture database on :55473 while other agents loaded the VM. Both pass when rerun, and the
  whole package passes on PG14, PG18 and in the final PG17 run.

### Coverage Gaps (packages below threshold)
All packages meet coverage thresholds (lowest touched: store 76.1%, api 78.9%,
cmd/pg_sage_sidecar 78.5%; new packages 87.1% and 89.3%).

### Bugs Found This Session
See "Bugs found while testing" below (one design bug, three test-logic bugs, four weak
assertions found by mutation testing).

### Manual Checks Remaining
- CHECK-CLOUD-01..04: MANUAL -- owner-run live checks against a disposable RDS/Aurora and
  Cloud SQL instance (`-tags cloudlive`, see below).
- MANUAL: the Actions page's provider-change cards in a browser (vitest covers rendering and
  the approve/reject calls).

## Bugs found while testing

1. `verify.LoadEvidence` must stay comparable (an existing regression test compares it to its
   zero value): the first design carried withhold reasons as `[]string`; it is now a joined
   string (my phase-1 tests were adjusted for the type only, not the behavior).
2. The worker test assumed `max_connections` was not pending a restart on the shared test
   server and ignored a second drifting fixture parameter (`max_wal_size`); the test now uses a
   reloadable setting for the "already running" case and drops the extra fixture parameter.
3. `TestProposal...` used `p-1`, which is not a valid Google Cloud project ID (6-30 chars).
4. A pressure test proposed `work_mem = 2MB`, below the documented safe minimum, which the
   range gate drops before the memory guard; it now lowers 16MB -> 8MB.
5. Four mutants survived the first run (runway minimum points, Status deep copy, approval on a
   restricted-but-reloadable managed change, CLI value length bound); tests were added and all
   four are now killed.

## Post-test audit

Mutation testing of the guards and the code that consumes telemetry (35 mutants, each run
against its package's tests on PG17): 35 killed after 4 tests were added (31 on the first
run). Mutants covered every guard boundary (`>` vs `>=` on lag, free storage, runway, work_mem
cap, memory floor), freshness and clock-skew checks, error classification, PI optional vs
fatal, the credentials pre-check, Status deep copy, fail-closed poll, history reset on resize,
the verify and executor withhold paths, the advisor's managed intent and its approval, the
tuning redirect, telemetry being ignored by advisor or tuning, `AutoApply`, reboot inversion,
Cloud SQL flag list, drift kinds, unmodifiable parameters, single-winner decisions, applied
observation and value validation.

1. **Inputs not tested:** CloudWatch `PartialData` with a `NextToken` and Cloud Monitoring
   `nextPageToken` (one instance plus up to 20 replicas never paginates at a 15-minute window,
   but a very large `num_backends` label set could); real AWS Query XML namespaces beyond the
   ones the SDK and the fakes use; Cloud SQL tiers other than `db-custom-*` and `db-n1-*`
   (memory then comes from `memory/quota`, which every instance reports).
2. **Assertions that pass when broken:** the mutation run found four (now fixed). The
   never-widen property is checked two ways: structurally (the telemetry packages cannot import
   policy, trust, ledger, executor or store code) and behaviorally (2000 random samples through
   `verify.DecideAdmission`; 500 random host-memory readings through the configuration gates).
3. **Fakes that hide real failures:** the AWS fake checks that every request carries a SigV4
   header with the access key but does not verify the signature (the SDK signer is trusted; the
   live check covers it); the fake ignores `StartTime`/`EndTime`, so the adapter's own window
   filter is what is tested (a 40-minute-old point is dropped). The Google fake verifies the
   service-account JWT signature, issuer, audience, scope and lifetime for real. The proposal
   store, worker, findings, API and migration run against real PostgreSQL 14, 17 and 18.
4. **Not covered by CI by design:** real provider APIs (owner-run `cloudlive` tests).

## Live check (owner)

See `docs/managed-clouds.md`, "Live check": build tag `cloudlive`, `SAGE_TEST_AWS_REGION` +
`SAGE_TEST_RDS_INSTANCE` (`SAGE_TEST_RDS_IS_CLUSTER=1` for Aurora) or `SAGE_TEST_GCP_PROJECT` +
`SAGE_TEST_CLOUDSQL_INSTANCE`, read-only permissions listed there.

## What is left

- An apply adapter for RDS parameter groups / Cloud SQL flags plus a `managed_parameter`
  action class in the trust ledger, promoted from the observed outcomes recorded here.
- Feeding provider storage into the existing runway monitor and the WAL custodian's disk
  capacity (the runtime already implements the capacity shape; left unwired to keep the
  runway monitor's DB-size-based series unchanged).
- `sighup`-context settings on managed services are still rewritten to `ALTER DATABASE`
  (which PostgreSQL refuses for them); they should become managed intents too.
- Cloud Monitoring and CloudWatch pagination (not needed at one instance + replicas per call).
- Workload identity federation (`external_account`) credentials for Google.
