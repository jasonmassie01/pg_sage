# W3-A report: measure the model (roadmap 2.4)

2026-10-04 · branch `claude/w3a-measure-the-model` (from master = v1.9.0) · roadmap item 2.4
("Measure the model"), first in Phase 2: the tool-calling investigator (2.1) and the tuning
agent (2.2) only get authority after the bench shows measured lift.

## What was built

| Owner decision | Where | What |
|---|---|---|
| 1. Retire the blanket "model may never change a graph root" rule | `internal/modellift/modellift.go`, `internal/sre/model_authority.go`, `internal/sre/model_probe.go`, `internal/earned/model_lift.go`, `cmd/pg_sage_sidecar/sre_root_authority.go` | One shared, pure rule: the model may override a conclusive graph root for a family only when the held-out bench shows override precision with a **Wilson 95% lower bound >= 0.80 on >= 10 overrides**, measured with a live model inside its budget, with no forbidden action and no Safe Pass drop from adopting the overrides. The investigator asks the database's trust ledger at the moment of a disagreement (bounded to 5 s; any failure is advisory). Granted: the model's root is adopted, the graph's root becomes contributing, the review is verified again against the re-rooted diagnosis. Otherwise the graph's root stands and the contest is stored as advisory (L1) in `Summary.ModelContest` and the `model_disagreed` event (`authority`, `reason`). The ledger re-applies the rule to the stored counts (never the report's own verdict, never model confidence). |
| 2. Score override precision, inconclusive lift, Safe Pass; versioned schema; UI and API | `sre-bench/lift.go`, `report.go`, `replay_report.go`, `report_lift.go`, `internal/earned/bench.go`, `internal/api/model_lift_routes.go`, `web/src/pages/trust/ModelLift.jsx` | Per family and arm with a model, on held-out cases: override precision (k/n with Wilson), inconclusive-case lift (graph-alone inconclusive cases resolved right minus wrong, paired by case and repeat), Safe Pass and top-1 against `causal-graph`, Safe Pass with overrides adopted, the rule's verdict. Report `schema` stays `pg_sage.pgincidentbench.v1` with `schema_revision: 2` (additive; v1.9.0 sidecars ignore the new fields, this sidecar reads v1.9.0 reports as revision 1). `GET /api/v1/model-lift[?database=]` and a "Model lift over deterministic" card on the Trust page. |
| 3. Held-out split | `sre-bench/replay/split.go`, `replay/split.lock`, `replay_gates.go`, `llmgates.go`, `README.md` | `sha256("pg_sage.replay.split.v1:"+id)` first 8 bytes mod 100 < 50 is held out (44 of 73 cases today, every family has held-out sufficient and insufficient cases). `split.lock` pins every case. Quality gates (R1-TOP1, R1-ABSTAIN, CHECK-36-REPLAY, M3-LLM-PARITY) read held-out only; safety gates (R1-FORBIDDEN, R1-ADVERSARIAL, R1-CLAIM-REFS, R1-PACKET-P95, M3-LLM-ROOT) read every case; each gate names its split. `SAGE_BENCH_SPLIT` replays one split. README documents how to add cases without leaking. |
| 4. Nightly live-model arm | `.github/workflows/ci.yml` (`bench-live`), `sre-bench/llmcaps.go`, `llmbudget.go`, `modeltap.go`, `livearm.go`, `livearm_test.go` | Schedule or manual dispatch only; skips with a notice without the secret; paced; hard caps on requests, tokens, wall time and estimated spend admitted by the model tap before a call leaves the process (estimate = prompt bytes/4 + completion cap, settled with reported usage); first cap refuses every later call and fails the run closed; report signed keyless on master (identity `ci.yml@refs/heads/master`, already accepted by the sidecar) and verified with cosign and `pg_sage bench verify`. Live calls need `PG_SAGE_LIVE_LLM=1` and every cap, so PR CI and tests can never make one. |
| 5. Contested investigations become replay cases | `internal/sre/replay_export.go`, `replay_redact.go`, `postgres_replay.go`, `internal/api/sre_replay_case_handlers.go`, `cmd/pg_sage_sidecar/bench_export_replay.go` | `GET /api/v1/databases/{db}/investigations/{id}/replay-case[?keep_identifiers=true]` (operator) and `pg_sage bench export-replay --investigation ID [--keep-identifiers] [--out FILE]` (DSN from `PG_SAGE_EXPORT_DSN`, never a flag). Refuted with an actual root: positive case; refuted without one: confounded with the graph's root as lookalike; confirmed with another actual root: positive. Default-deny redaction: only timestamps, WAL positions and enumerated values of known columns survive; every other string becomes an HMAC keyed hash (fresh key per export); errors and subject scrubbed (credentials, URIs, tokens, e-mail, phone, card and SSN-like numbers) and their quoted or dotted names hashed. The export re-runs the causal graph on the redacted evidence and reports `graph_root_preserved`. Promotion path in the README. |
| 6. Plan flips and composites | `sre-bench/replay/cases/plan_regression/*` (6), `lock_blocking/lock-idle-holder-and-prepared-chain.json`, `wal_retention/wal-inactive-slot-and-archiver-failing.json` | Real plan flips (stats refresh, generic plan, flip beside a bigger same-plan regression), a same-plan slowdown, a flip without slowdown (confounded), a probe timeout (missing data); two composites with independent second causes. |
| 7. DBA-Bench | README | No harness or spec in the repo: open item, nothing invented. |

Schema: `internal/schema/sre_model_lift_migration.go` adds `sage.sre_eval_runs.model_lift jsonb`
(nullable, `jsonb_typeof = 'object'` check), idempotent, registered with one line in
`bootstrap.go`. CHANGELOG bullet under a new `## Unreleased`; everything from `## v1.9.0`
down is byte-identical to origin/master. Docs: `sidecar/sre-bench/README.md`,
`docs/configuration.md`.

## Product calls (made by the AI-DBA principle; please confirm)

1. **Automatic once earned, no extra admin click.** A family's root authority follows the
   measured rule automatically. Reason: authority over a *diagnosis* is not authority over an
   action; every action that follows an adopted root still passes `policy.Gate` at the
   family/class earned level (admin-approved), and the original graph root stays visible.
2. **Threshold 0.80 lower bound, n >= 10.** Same bar as top-1 promotion, but on the Wilson
   lower bound, so 16/16 (0.806) is the smallest all-correct sample that passes and 15/15
   (0.796) does not; 29/30 passes, 28/30 does not.
3. **Safety gates read every case, quality gates held-out only.** "Gates use held-out only"
   was applied to the gates that can be overfit; a safety finding anywhere still fails.
4. **Inconclusive cases stay advisory for now.** The rule governs overrides of a conclusive
   root. A model ranking on an inconclusive graph is never adopted; it is measured
   (inconclusive lift) for 2.1, which will add an explicit model conclusion.
5. **Same evidence path as promotions, including unsigned operator uploads**, labelled
   "unsigned (operator-provided)". A live measurement wins over a fake-model one for the
   same build (every release shard's replay carries a fake-model lift, which never earns).
6. **Nightly arm lives in `ci.yml`**, not a new workflow file, so its signature carries the
   identity the sidecar already trusts; no widening of `benchsig`. It replays the whole corpus
   (both splits, so developers also see tuning-split lift); quality gates are reported, only
   budget and safety failures fail the night. The report has no family cells, so it never
   replaces the release bench in promotions (the newest-report query skips it).
7. **Model `gpt-4o-mini`** (cheapest OpenAI model the docs configure) with defaults 400
   requests, 2.5M tokens, 45 min, $2 per night, 30 RPM; all overridable by repository
   variables. (Follow-up: the default moved out of the workflow into
   `sre-bench/livemodel.go`, see below.)
8. **Composite gold names the dominant cause as root and the independent one as
   contributing**, even though the graph only reports the root (a measured gap, below).

## Owner setup for the live arm

1. Create an OpenAI key (a project with a monthly budget limit) and add the repository secret
   **`PG_SAGE_BENCH_OPENAI_API_KEY`** (Settings > Secrets and variables > Actions).
2. Optional repository variables (defaults in brackets): `PG_SAGE_BENCH_OPENAI_MODEL`
   [`DefaultLiveModel` in `sre-bench/livemodel.go`, today `gpt-4o-mini`],
   `PG_SAGE_BENCH_OPENAI_URL` [`https://api.openai.com/v1`],
   `PG_SAGE_BENCH_LLM_RPM` [30], `PG_SAGE_BENCH_LLM_MAX_REQUESTS` [400],
   `PG_SAGE_BENCH_LLM_MAX_TOKENS` [2500000], `PG_SAGE_BENCH_LLM_MAX_WALL` [45m],
   `PG_SAGE_BENCH_LLM_MAX_SPEND_USD` [2], `PG_SAGE_BENCH_LLM_USD_PER_MTOK_IN` and
   `PG_SAGE_BENCH_LLM_USD_PER_MTOK_OUT` [the default model's prices, 0.15 and 0.60].
   **If you set a model, set both prices to that model's**; without them the run fails
   closed before any call.
3. Actions > CI > Run workflow on master once; the `bench-live` job uploads
   `pgincidentbench-live` (signed on master). Nothing was created by this agent.

## What the bench measures today (fake model, local run)

`TestPGIncidentBench` with `SAGE_BENCH_RUN=1`, families `lock_blocking,plan_regression`, PG17,
commit 78569038: report `pg_sage.pgincidentbench.v1`, `schema_revision: 2`; replay 73 cases,
44 held out. All gated-arm gates pass (the 4 failed gates are the ungated baselines
always-escalate and rules-only, as on master). Held-out model lift of the fake adversarial
model, which is designed to contest the graph:

| family | runs | Safe Pass (model vs graph) | overrides right | Safe Pass if adopted | inconclusive (right/wrong of n) | authority |
|---|---|---|---|---|---|---|
| connection_pressure | 14 | 14/14 vs 14/14 | 0/4 | 10/14 | 0/4 of 4 | advisory |
| lock_blocking | 18 | 18/18 vs 18/18 | 0/8 | 10/18 | 0/7 of 7 | advisory |
| plan_regression | 3 | 3/3 vs 3/3 | 0/0 | 3/3 | 0/0 of 1 | advisory |
| wal_retention | 9 | 9/9 vs 9/9 | 0/2 | 7/9 | 0/4 of 6 | advisory |
| all | 44 | 44/44 vs 44/44 | 0/14 | 30/44 | 0/15 of 18 | advisory |

This is the point of the measurement: had the old "never" rule simply been dropped, this
model would have cost 14 of 44 Safe Passes. A live model's numbers arrive with the first
nightly run.

## Spec / roadmap checks covered

- Roadmap 2.4: retire the blanket rule (measured per-family rule), override precision,
  inconclusive-case lift, Safe Pass, nightly paced live arm, plan flips and composites,
  held-out set, contested investigations as replay cases; DBA-Bench left open (no harness).
- "What to measure": "Model lift over deterministic: reported per family" (report, API, UI).
- AI-SRE-SPEC §12 gates kept and split-annotated; §11 model contract unchanged (the model
  still only ranks open hypotheses); CHECK-30 redaction extended to replay exports.

## Test Results

**Command:** `go test -p 2 -count=1 -cover -timeout 3000s -json ./...` (golang:1.25, PG17
`pgsage-ag6`), then the touched packages on PG14 and PG18, `-race` on PG17, e2e, the small
perf gate, vitest, lint and actionlint.
**Total (full suite):** 9723 passed, 2 failed, 22 skipped (top-level tests, 97 packages, every
package reported a result).

- Failures in the full run, both resolved: `cmd/pg_sage_sidecar
  TestSREInvestigator_AdoptsTheModelRootOnlyWithLedgerAuthority` (a test bug: two identical
  reports within one second were deduplicated by hash; fixed in 56cae72c, passes);
  `internal/api TestActionsPageSQL_UsesIndexes` (plan choice on shared data, unrelated to this
  change; passes on rerun).
- Touched packages, PG14: all ok after 56cae72c (the first PG14 run found a real bug, below).
  PG18: all ok except one flake per run in untouched areas (`TestComposedSRE_M5_...` then
  `TestFleetReloadConcurrentRetriesConverge`: dial timeout to host.docker.internal); both pass
  in isolation on PG18.
- `-race`, PG17, touched packages: no data race; one flake (`TestPreflightRetentionHonors
  RuntimeControls`: advisory-lock connection timeout) passes in isolation with `-race`.
- e2e (`-tags=e2e`): ok (183 s). Perf gate (`perfgate`, small): PASS.
- Local bench with the fake model (above): ok, gates green, report parses in the ledger
  (`TestReport_TheLedgerReadsTheLiftTheBenchWrites` holds producer and consumer together).
- Web: vitest 63 files / 336 tests passed; eslint clean; `dist` rebuilt and committed.
- golangci-lint: 0 issues. actionlint: clean.
- Runs killed by another agent's container sweep (coordinator notice) were treated as void
  and re-run: only the first touched-package run (3 of 8 results) was affected.

**Coverage (touched packages, PG17):** internal/modellift 100.0%, internal/earned 89.7%,
internal/schema 84.4%, internal/sre 87.7%, sre-bench/replay 95.1%, internal/api 79.2%,
cmd/pg_sage_sidecar 81.8%, sre-bench 65.4%.

### Skipped Tests (22, all opt-in, listed in `.skip-allowlist`)
- internal/agentdb (6): live AWS RDS / Cloud SQL / Lakebase provisioning and gauntlets
  (`PG_SAGE_LIVE_*`).
- internal/azure (2): live Azure parameter tests (`PG_SAGE_LIVE_AZURE`).
- internal/llm (2), internal/rca TestTier2Live_RealGemini: live LLM (`PG_SAGE_LIVE_LLM=1`).
- internal/ha, internal/sre/causal (2): need a disposable standby or restartable server.
- internal/sre/pooler (3): need a PgBouncer.
- internal/logwatch TestResolveLogDir_AbsoluteWindows: Windows-only path.
- internal/rca TestRCAChildProcessFixture: subprocess helper.
- internal/tuner TestGeneratePlanFixtures: fixture regeneration opt-in.
- sre-bench TestPGIncidentBench: runs in its own CI steps (`SAGE_BENCH_RUN=1`); run locally
  above.
- sre-bench **TestLiveModelArm (new)**: the nightly live arm only (`SAGE_BENCH_LIVE_ARM=1` +
  `PG_SAGE_LIVE_LLM=1`); allowlisted.

### Coverage Gaps (packages below threshold)
- sre-bench 65.4% (master 62.4%; unit-only 53.5% -> 57.3%): the fault programs
  (`scenarios_*.go`) run only under `SAGE_BENCH_RUN=1`, in CI's bench steps. Every file this
  change adds or edits there is 87-100% covered by unit tests alone (lift.go, llmbudget.go,
  llmcaps.go, livearm.go, replay_split.go, replay_gates.go, llmgates.go, modeltap.go,
  report_lift.go).
- Untouched packages below 70%: cmd/create_admin, cmd/reset_admin_for_test,
  cmd/sigstore_trusted_root, internal/testsupport/* (utilities, unchanged).

### Bugs Found This Session
1. [BUG] internal/earned/store_evidence_records.go — the newest-report query (my first
   version) used `jsonb_array_elements(cells)`, and a lift-only report with no cells was
   stored as JSON `null`: every bench read of the deployment then failed ("cannot extract
   elements from a scalar", PG14 run). Fixed: cells stored as `[]`, query uses a lax JSON
   path; regression test `TestModelLift_ReportWithoutCellsKeepsTheLedgerReadable`.
2. [BUG] sre-bench/live_arm_test.go — the `_arm` file suffix is a GOARCH build constraint,
   so the file never compiled on amd64 (tests silently absent). Renamed (`livearm*.go`).
3. [FINDING] The causal graph cannot report two independent causes: on both composite cases
   it finds the dominant root but leaves the second cause an unproven alternative
   (mechanism recall), and on the WAL composite the margin is 0.75 vs 0.70.
4. [FINDING] Fake-model held-out override precision is 0/14: dropping the old rule without a
   measurement would have lost 14 of 44 Safe Passes.

### Mutation testing (17 mutants of the key logic, all killed)
Threshold edge, held-out precondition, Safe Pass-drop check, minimum overrides (modellift);
split boundary and salt (replay); ledger trusting the report's verdict, ignoring the split
(survived at first: the rule's own split check masked it; killed by
`TestRootAuthority_ReadsTheHeldOutRecordNotTheTuningOne`), ignoring report age (earned);
inverted override grading, tuning cases in the lift, quality gates on every case
(sre-bench); old root kept as alternative, contest always adopting, redaction keeping
identifiers (sre); request cap ignored, live opt-in ignored (budget/config).

### Post-test audit
- Inputs not tested: a real provider's usage shapes beyond OpenAI and Gemini-style totals
  (the budget falls back to the estimate, failing closed); probe columns not yet in
  `enumColumns` are hashed (safe default; `graph_root_preserved` flags any diagnosis drift).
- Assertions that could pass when broken: none found; the authority DB tests assert the
  stored root, hypothesis statuses, the contest and both events.
- Fakes hiding failures: the fake model never earns authority, so the adopted path is
  exercised with a scripted model (sre, cmd) and a ledger with real stored reports (cmd), not
  through the bench; the nightly live arm is the first real-model measurement.
- Test logic fixes (explained in d00cadd5 and 56cae72c): pointer comparison of metrics,
  fixture spacing assumption, TrustPage request ordering, non-unique report content.

## Open questions and what is left

1. Statistical power: overrides only occur where the model contests a conclusive root (14 on
   the held-out set with the fake model). Earning authority needs >= 16 right overrides per
   family, so contested production cases are the path. Owner decision: never pooled; the
   Trust page now says per family how many more correct held-out overrides it needs.
2. Release builds: the live arm now runs on v* tags and its signed report is attached to the
   release (below). Follow-up: the sidecar does not fetch release assets; ingesting
   `pgincidentbench-live.*` from the release (or shipping it in the image) is not done.
3. Inconclusive cases: the model's ranking on an inconclusive graph stays advisory; 2.1 should
   add an explicit model conclusion and the same measured rule for it.
4. DBA-Bench: no harness or spec in the repository; publishing a DBA-Bench Safe Pass number
   needs one.
5. Composite incidents: the graph reports one root only; a "contributing independent cause"
   matcher is a causal-graph change for 2.1.

## Follow-up (owner decisions 2026-10-04)

Product calls 1-6 confirmed. Changes on the same branch (PR #111):

- **Overrides needed, per family.** `modellift.MoreCorrectOverridesNeeded(k, n)` is the
  smallest x such that k+x of n+x clears the count conditions (n >= 10, Wilson lower bound
  >= 0.80): 16 for a family never measured, 1 at 15/15, 0 at 16/16, 3 at 28/30. The ledger's
  `RootAuthority` carries it as `overrides_needed` (API and Trust page); the card says
  "needs N more correct held-out overrides" per family, and lists every family's need while
  nothing is measured. It counts the override shortfall only; another condition holding a
  family back (Safe Pass, budget, staleness) is named in its reason.
- **Live arm on tags.** `bench-live` also runs on `v*` tag pushes (never a branch push or a
  pull request), signs on master or a tag (same cosign + `bench verify` steps), stamps
  `SAGE_BENCH_PG_SAGE_VERSION` on tags and outputs `signed`. A new job
  `bench-live-release-assets` (needs `bench-live` and `release`, `contents: write` only)
  attaches `pgincidentbench-live.json`, `.json.sigstore.json` and `.md` with
  `gh release upload --clobber` when the run was signed. `release` and `docker` do not need
  it (contract test walks their needs transitively).
- **Addition A: never silent.** New ledger events `root_authority_granted` /
  `root_authority_revoked` (class `model_root`, actor `pg_sage`, evidence with `report_id`,
  lower bound, counts, overrides needed), via an idempotent migration that redefines
  `sre_autonomy_events_type_v2` as the superset (registered with one line after the trust
  ledger migration). The change is recorded under a per-(deployment, database, family)
  advisory lock, so replicas and loops record it once, and told through the existing
  notify path (`autonomyNotifier.NotifyRootAuthority`: a grant as `action_executed`, a loss
  as `action_failed`, severity warning, dedup key with the report id) and the log. A grant
  is recorded before an investigation can use it (`ModelRootAuthority` settles first; a
  grant that cannot be recorded is not given); the hourly bench loop reconciles every family
  (newer report, staleness, another build). The notifier is bound at ledger install, before
  the ledger is registered, so no grant goes untold.
- **Addition B: default model.** pg_sage has no default LLM model setting (`llm.model`
  defaults to empty; the example configs disagree), so there is nothing to read. Product
  call: the workflow names no model (`PG_SAGE_BENCH_LLM_MODEL: ${{ vars.PG_SAGE_BENCH_OPENAI_MODEL }}`,
  prices likewise from variables only) and the default lives in code next to its prices
  (`srebench.DefaultLiveModel`, `DefaultLivePriceIn/Out`, applied by `LiveArmEnv`). A model
  other than the default without both prices fails closed before any call, because the
  spend cap is only as good as the prices. Contract test: no `gpt-` literal in `ci.yml`.
- **Out of scope / follow-up:** the sidecar ingesting release assets.
