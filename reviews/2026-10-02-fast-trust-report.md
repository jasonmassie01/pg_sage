# Fast trust elevation: report (2026-10-02)

Branch `claude/fast-trust` (from master `fce3674`). Commits: `3a00ab1` (tests first),
`cbaf3e7` (implementation, docs, generated files, web dist) and this report.

The product owner asked that every trust level elevate in hours, not days or weeks, for
the `lifeos` dogfood database. pg_sage's principle is to earn trust, then become
autonomous. The change makes how fast trust is earned configurable. It does not change
what earned trust allows, and it never makes the speed-up silent.

## Settings found that gate elevation

| Gate | Before | Now |
|---|---|---|
| Trust ramp, SAFE | `safeRampAge = 8d` hard-coded (`policy/gate.go`) | `trust.ramp_safe_hours` (default 192, 1-8760) |
| Trust ramp, MODERATE | `moderateRampAge = 31d` hard-coded | `trust.ramp_moderate_hours` (default 744, 1-8760, at least the safe ramp) |
| D6 learned IO baseline | `verify.io_baseline_days` (int days, 0 disables) | plus `verify.io_baseline_hours` (1-8760, wins over days when set, must fit `io_sample_retention_days`) |
| M7 shadow window | 30 days, spec constant | `sre.autonomy.promotion.shadow_window_hours` (720, 1-8760) |
| M7 shadow volume | 20 packets, constant | `shadow_min_reviewed` (20, 3-1000) |
| M7 shadow acceptance | 95%, constant | `shadow_min_accepted_pct` (95, 50-100) |
| M7 bench top-1 | 80%, constant | `bench_min_top1_pct` (80, 50-100) |
| M7 factual precision | 90%, constant | `bench_min_precision_pct` (90, 50-100) |
| M7 Safe Pass (bench and game days) | 95%, constant | `min_safe_pass_pct` (95, 50-100) |
| M7 live L2 recoveries for L3 | 50, constant | `min_live_recoveries` (50, 1-10000) |
| M7 evaluation interval | `evaluate_interval_minutes` (60, 5-1440), already configurable | unchanged; reported when lowered |
| M7 safety window | `safety_window_days` (30, 1-365), already configurable | unchanged; reported when lowered |

Reviewed and deliberately left alone:
- Rollback cooldown days (`trust.rollback_cooldown_days`, already configurable, min 1 day):
  this is anti-flap hysteresis after a failure, not an elevation gate.
- The retention-delete dry-run review window (24 h min, 7 d max), the handoff reject
  cooldown and handoff TTL (24 h). These gate irreversible deletes or operator-rejected
  handoffs, not elevation.
- Bench report max age (30 d) and minimum bench run counts (n >= 10). These are evidence
  freshness and quality, not time to elevate.
- Unused-index and other analysis windows. They are not trust gates.

## Hard invariants (not configurable, tested under the fastest profile)

- **Irreversible actions keep the spec ramp.** `policy.rampAge` uses the configured ramp
  only when the contract's reversibility cap allows L3 (`reversible`,
  `no_rollback_needed`). Anything else (`not_reversible`, `forward_fix_only`,
  `application_rollback`, `mitigation_only`, `not_applicable`, empty, unknown) waits
  `max(configured, spec)`. This includes the irreversible retention delete. A longer
  configured ramp still slows everything.
- Irreversible classes never go above L1, mitigation-only classes never above L2, and L4 is
  never proposed or met. The class caps are unchanged; the DB test approves every proposal
  for four rounds under the fastest bar.
- The emergency stop always wins. The trust level, execution mode, tier3 flags, windows and
  the standing policy remain the outer bound. The M7 combinatorial matrix now also runs
  with the fastest profile and an e-stop dimension: 36,288 combinations.
- An admin still approves every promotion. pg_sage, `system` and `mcp:` actors are refused
  (DB test).
- Downgrade signals still apply. A harmful outcome still demotes the family and blocks
  re-promotion inside `safety_window_days` (DB test).
- No knob accepts 0 to mean skip. Config validation enforces the minimums, the gate takes
  the spec ramp for a zero or negative age and floors any configured ramp at 1 h, and
  `earned.Thresholds.Normalized` replaces any zero, negative, NaN or out-of-range threshold
  with the spec value. Before this change, a partly zero `Thresholds` would have disabled
  checks.

## Never silent

- Startup prints one `WARN [startup] FAST ELEVATION: ...` headline, then one line per
  lowered value with its spec default. This was verified end to end by running the built
  sidecar against PG17 with the dogfood profile: 8 WARN lines.
- `GET /api/v1/sre/autonomy` returns `fast_elevation: {active, lowered: [{key, value,
  default, unit}]}`. The list is never null.
- **Advanced > Earned autonomy** shows a yellow "Fast elevation" badge that lists each value
  with its spec default.
- The keys are YAML-only and restart-bound, so the WARN always reflects what runs. The
  config API refuses them; verified end to end: `PUT /api/v1/config/global
  {"trust.ramp_safe_hours":"1"}` returns `unknown config key`.

## YAML for the lifeos dogfood database

Merge this into the lifeos sidecar config (restart required). The trust lines at the top
are what let pg_sage act at all: `trust.level`, `tier3_*` and the maintenance window are the
operator bound, and fast elevation does not change them.

```yaml
trust:
  level: autonomous               # needed for MODERATE actions; advisory = SAFE only
  tier3_safe: true
  tier3_moderate: true
  maintenance_window: always      # or e.g. "nights" if lifeos has busy hours
  ramp_safe_hours: 1              # SAFE actions after 1 hour (spec: 192)
  ramp_moderate_hours: 4          # MODERATE actions after 4 hours (spec: 744)
verify:
  io_baseline_hours: 2            # learned IO baseline after 2 hours (spec: 7 days)
sre:
  autonomy:
    evaluate_interval_minutes: 5  # propose promotions every 5 minutes (default 60)
    promotion:
      shadow_window_hours: 4      # 4 h of shadow reviews (spec: 720)
      shadow_min_reviewed: 3      # 3 reviewed packets in that window (spec: 20)
      min_live_recoveries: 3      # 3 verified L2 recoveries for L3 (spec: 50)
```

Product decision: the profile lowers time and volume, never accuracy. The 80% top-1, 90%
precision, 95% acceptance and 95% Safe Pass stay at the spec. Speed is not a reason to trust
a less accurate diagnosis.

Notes for lifeos:
- The trust ramp counts from `trust_ramp_start` in `sage.config`, which is written on first
  start. On an existing lifeos install that started more than 4 hours ago, the new ramps are
  already satisfied at restart.
- L2 needs a PGIncidentBench report no more than 30 days old that covers the family. Set
  `sre.autonomy.bench_results_path` to the CI `pgincidentbench/` artifact, or upload one (see
  below). Without one, nothing is proposed above L1, whatever the timings.
- Shadow volume is counted inside the window. With the profile, a family needs its first
  review at least 4 h ago and at least 3 reviewed packets, 95% or more of them accepted, in
  the last 4 h.

## Approving promotions quickly (API)

```bash
# log in (admin)
curl -c c.txt -H 'Content-Type: application/json' -X POST \
  http://HOST:8080/api/v1/auth/login --data '{"email":"admin@pg-sage.local","password":"..."}'
# (optional) bench evidence, if bench_results_path is not set; admin
curl -b c.txt -H 'Content-Type: application/json' -X POST \
  'http://HOST:8080/api/v1/sre/autonomy/bench-results?database=lifeos' --data @report.json
# shadow review of a concluded investigation; operator
curl -b c.txt -H 'Content-Type: application/json' -X POST \
  'http://HOST:8080/api/v1/sre/autonomy/reviews?database=lifeos' \
  --data '{"investigation_id":"<uuid>","verdict":"accepted"}'
# propose now instead of waiting for the 5-minute loop; operator
curl -b c.txt -X POST -H 'Content-Type: application/json' --data '{}' \
  'http://HOST:8080/api/v1/sre/autonomy/evaluate?database=lifeos'
# list pending proposals and their evidence
curl -b c.txt 'http://HOST:8080/api/v1/sre/autonomy/proposals?database=lifeos'
# approve one (admin only; evidence is re-checked at approval)
curl -b c.txt -H 'Content-Type: application/json' -X POST \
  'http://HOST:8080/api/v1/sre/autonomy/proposals/<id>/approve?database=lifeos' \
  --data '{"note":"lifeos dogfood"}'
```

The same Approve button is on **Advanced > Earned autonomy**. Promotion is one level at a
time (L1 to L2, then L2 to L3 after `min_live_recoveries` verified L2 recoveries), and each
step needs its own approval. A proposal expires after `proposal_ttl_hours` (168).

## Files

- New: `sidecar/internal/config/elevation.go` (promotion config, ramp and baseline
  accessors, validation), `sidecar/internal/config/elevation_lowered.go`
  (`LoweredElevation`) and `sidecar/cmd/pg_sage_sidecar/fast_elevation.go` (threshold
  mapping, startup WARN).
- Changed: `internal/config/{config.go,agent_native.go,sre_autonomy.go}` (fields, defaults,
  one validation hook), `internal/policy/{gate.go,types.go}` (`rampAge`, exported spec ramp
  constants, `RuntimeState.SafeRampAge` and `ModerateRampAge`),
  `internal/executor/{policy_runtime.go,index_verification_runtime.go,load_admission.go}`,
  `internal/verify/io_admission.go` (sub-day detail in hours),
  `internal/earned/{evidence.go,service.go}` (`Thresholds.Normalized`),
  `internal/api/autonomy_handlers.go` (`fast_elevation`),
  `cmd/pg_sage_sidecar/{main.go,autonomy_wiring.go,autonomy_fleet.go}` and
  `web/src/pages/AutonomyPage.jsx` (badge).
- Docs: `docs/configuration.md` (trust and load-admission rows, promotion rows, the new
  "Fast elevation (dogfood databases)" section with the profile between markers that a test
  loads), `config.example.yaml` (commented profile that a test uncomments and loads),
  `CHANGELOG.md`.
- Generated: `web/src/generated/config_meta.json`, `docs/generated/config-lifecycles.md`,
  `internal/api/dist`.
- Tests: `internal/config/elevation_{config,lowered}_test.go`,
  `internal/policy/gate_ramp_test.go` (the M7 matrix refactored into `runPrematureMatrix` in
  `gate_autonomy_test.go`), `internal/earned/{thresholds_fast_test.go,fast_promotion_db_test.go}`,
  `internal/executor/elevation_runtime_test.go`, `internal/verify/io_admission_hours_test.go`,
  `internal/api/autonomy_fast_elevation_test.go`, `internal/store/elevation_override_test.go`,
  `cmd/pg_sage_sidecar/fast_elevation_test.go` and
  `web/src/pages/AutonomyFastElevation.test.jsx`.

## Spec CHECKs covered

- CHECK-40 (autonomy auto-downgrades): harm demotion and the downgrade matrix are
  re-asserted under the fastest bar.
- §7.3 invariants (irreversible ≤ L1, L4 reserved, human approval) are re-asserted under the
  fastest profile.

## Test Results

**Command:** `go test -count=1 -cover -v -timeout=40m ./...` in `golang:1.25`, repo root
mounted, against `pgsage-ag2` (PG17, :55472). Touched packages were also run with `-race`
on PG17, and on PG14 (:55414) and PG18 (:55418). Web: `npm test` and `npm run build`. Lint:
`golangci-lint run ./...`.

**Total (PG17 full suite):** 10,252 passed, 6 failed, 13 skipped. All 6 failures were
`context deadline exceeded` and fixture `DROP DATABASE` timeouts in packages this branch
does not touch: `cmd/pg_sage_sidecar` (meta bootstrap), `notify`, `recommendation`,
`retention`, `runway`, `sre/probes` and `sre-bench`. They happened while several agents
were running suites on the shared Docker host. All 7 packages pass when re-run alone on PG17
(`rerun17`: exit 0). An earlier full run had one `internal/api` 10-minute timeout under the
same load; `internal/api` passes in the re-runs, including with `-race`. Treat these as
flakes from shared-host load, not regressions.

**Touched packages (PG17 / PG17 -race / PG14 / PG18):** all pass.

| Package | Coverage (PG17) | Threshold |
|---|---|---|
| internal/config | 89.7% | 70% |
| internal/policy | 89.7% | 70% |
| internal/earned | 87.6% | 70% |
| internal/executor | 83.2% | 70% |
| internal/verify | 86.8% | 70% |
| internal/api | 76.1% | 70% |
| internal/store | 74.5% | 70% |
| cmd/pg_sage_sidecar | 72.6% | 70% |
| cmd/gen_config_meta | 86.4% | 50% (tool) |

All touched packages meet their coverage thresholds.

Web: 47 files and 249 tests pass, including the 3 new tests in
`AutonomyFastElevation.test.jsx`. The build succeeds. Lint: 0 issues.

### Skipped Tests (13, all pre-existing opt-in live/env tests)
- TestAWSRDSLiveProvisioning, TestCloudSQLLiveProvisioning, TestLakebaseLiveProvisioning:
  need `PG_SAGE_LIVE_*` cloud credentials.
- TestAgentDBLiveGauntlet*: three tests that need live cloud gauntlet flags.
- TestAzureLiveServerParameter, TestAzureLiveRestartBoundParameter: need
  `PG_SAGE_LIVE_AZURE`.
- TestChatWithToolsLive_RealProvider, TestTier2Live_RealGemini: need `PG_SAGE_LIVE_LLM`.
  GEMINI_API_KEY was unset, as the rules require.
- TestResolveLogDir_AbsoluteWindows: Windows-only path test, running on Linux.
- TestRCAChildProcessFixture: a helper process that runs only under its parent test.
- TestPGIncidentBench: needs `SAGE_BENCH_RUN=1`; CI runs it in its own step.

### Failures
None in touched packages. The shared-host timeouts are listed above.

### Coverage Gaps
None below threshold.

### Bugs Found This Session
1. [BUG] `earned.NewService` replaced thresholds only when the whole struct was zero. A
   partly zero `Thresholds` (for example `MinL2Recoveries: 0`) would have silently skipped
   that check. Fixed with `Thresholds.Normalized`.
2. [TEST BUG, own] The fast-shadow fixture put the first of 3 reviews outside the 1-hour
   window. Shadow volume is counted inside the window, so this was a test logic error, not
   an implementation one. The fixture now records 4 reviews.
3. [TEST WEAKNESS, own] `TestZeroThresholdsTakeTheSpecBar` first passed only because a
   zero bench age failed freshness. Mutation testing caught it. The test now pins the
   report age, and the mutation is killed.

### End-to-end (manual, programmatic)
- CHECK-01: PASS. The built sidecar with the dogfood profile against PG17 logs 1 headline
  and 7 per-value `FAST ELEVATION` WARN lines.
- CHECK-02: PASS. `GET /api/v1/sre/autonomy` returns `fast_elevation.active=true` with 7
  lowered values, and `enforced=true`.
- CHECK-03: PASS. `PUT /api/v1/config/global {"trust.ramp_safe_hours":"1"}` is refused
  (`unknown config key`).
- CHECK-04: PASS. `POST /api/v1/sre/autonomy/evaluate` works with the fast bar (no evidence,
  so `created: []`).
- CHECK-05: MANUAL. The badge rendering in a real browser. It is covered by component tests,
  but nobody has looked at it in a browser.

## Post-test audit

- Mutation testing: 22 mutations across the gate ramp floors, the irreversible branch,
  `LoweredElevation`, validation, baseline precedence, `Normalized`, the service hook,
  threshold mapping, the WARN gate, the runtime wiring, admission evidence, the API
  `active` flag and the hours wording. All 22 are killed (one only after the test fix
  above).
- Untested input: the call to `warnFastElevation` in `main.go` has no unit test. The e2e
  run checked it (CHECK-01).
- Fakes: the gate tests use a fake limiter. The promotion invariants are also asserted
  against real PostgreSQL through the ledger (approval refusals, caps over 4 approve
  rounds, harm demotion).
- Fleet mode: the elevation keys are global (`trust.*`, `sre.autonomy.*`). Per-database
  overrides of the ramp or bar are not supported, by design. There is one ledger per
  control database.

## Product decisions

1. Fast elevation shortens how long trust takes, never what trust allows. Actions that
   cannot be rolled back keep the spec ramp. This is the trust-ramp counterpart of
   "irreversible never above L1".
2. Minimums: ramps and windows are at least 1 hour, shadow volume at least 3 packets,
   recoveries at least 1, accuracy percentages 50-100. Below 50% pg_sage is wrong more often
   than right, which earns nothing.
3. The moderate ramp must be at least the safe ramp.
4. The keys are YAML-only and restart-bound, so lowering a gate is a deliberate deploy-time
   act that the startup WARN records. An API admin cannot shorten ramps at runtime.
5. The dogfood profile lowers only time and volume. The accuracy bar stays at the spec.
6. Lowering is reported against the spec. A shorter `io_baseline_days`,
   `safety_window_days` or `evaluate_interval_minutes` (already configurable) now also
   warns. Disabling the baseline (`io_baseline_days: 0`) does not warn, because it is
   stricter.
7. Rollback cooldown, the retention dry-run window and the handoff timers are not elevation
   gates, and they stay as they are.

## What is left / for the coordinator

- Merge note: `internal/api/dist` was rebuilt and is tracked, so expect conflicts with other
  branches that also rebuilt it. Rebuild after merging.
- The MCP `sre_get_autonomy` tool does not yet include `fast_elevation`. The API and UI do.
- Existing observation: backdating `trust.ramp_start` on a fresh install already skipped the
  ramp before this change, and it is not reported as fast elevation. Consider reporting a
  `ramp_start` in the past at first persist.
- UI copy in `TrustBadge.jsx` and `SettingsPage.jsx` still says "8-day ramp" and "31-day
  ramp". Those are the defaults, and they are accurate unless the profile is used.
