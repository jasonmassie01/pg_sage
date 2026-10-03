# P0 config safety: reversible, verified config changes and replica-safe index drops

Branch `claude/p0-config-safety` (from master v1.8.1). Phase 0 items 1 and 12 of
`pg_sage-review/reviews/2026-10-02-ai-next/ROADMAP.md`.

## What was built

### One source of truth: `internal/pgconf` (new)
| File | Content |
|---|---|
| `parse.go` | `ParseAlterSystem` (SET `=`/`TO`, RESET; RESET ALL refused) and `ParseAlterTableReloptions` (IF EXISTS, ONLY, quoted names, `toast.` keys, bare boolean options, quoted values with commas; trailing subcommands refused) |
| `restart.go` | The only restart-required list, now including `autovacuum_max_workers` (postmaster before PG18) and the executor's extra entries |
| `docs.go` | The GUC documentation prior and safe ranges (moved from advisor), plus `checkpoint_timeout`, `min_wal_size`, `autovacuum_naptime`, `autovacuum_max_workers`, `autovacuum_analyze_*`, `autovacuum_vacuum_insert_*` |
| `units.go` | Unit parsing (moved from `advisor/gucunits.go`), now honouring time base units (`checkpoint_timeout = 900` is seconds), and `ToBaseUnits` for read-back |
| `allowlist.go` | `ExecutableGUC` (the executor whitelist, moved from `executor/validate.go`), `AdvisorGUC` (documented), `AutonomousGUC` (documented and reload-only), `ValidateValue` |
| `reloptions.go` | Advisor reloption allowlist with ranges (autovacuum vacuum/analyze/insert thresholds and scale factors, cost delay/limit, `fillfactor` 50-100; `toast.` variants where PostgreSQL has them) and the executor rule `CheckExecutableReloption` (adds freeze ages and `autovacuum_enabled`, which may only be turned **on**) |

Removed duplicates: `advisor/validate.go` restart map, `advisor/docground.go` table and
parsers, `advisor/gucunits.go`, `executor/config_apply.go` restart map and parser,
`executor/validate.go` `safeAlterSystemParams`.

### Advisor
- `advisor/allowlist.go` (new), applied in `parseLLMFindings`: a GUC outside the
  documented list, or a reloption outside the allowlist, becomes an **advisory finding
  without SQL** (severity info, the LLM's rationale kept, a note why). Disabling autovacuum
  gets an explicit wraparound warning. Restart-required GUCs keep their SQL but carry
  `detail.approval_required`. A multi-key reloption SET is split into one finding per key.
- `docground.go`, `validate.go`, `llmcall.go`, `prompt.go` now use pgconf.

### Executor
- `config_change.go`, `config_prior.go` (new): at apply time, inside Apply and after the
  recommendation claim, the prior value is captured from `pg_settings` /
  `pg_class.reloptions` (+ TOAST) and the real rollback replaces whatever the LLM
  proposed. Recorded in `before_state.config_change` (prior row, requested value,
  rollback, outcome baseline) and `action_log.rollback_sql`.
- `config_readback.go`, `config_apply.go`: after `pg_reload_conf()` the setting is polled
  (bounded, 5 s default) until the requested value is live, compared in the setting's own
  unit and type, or `pending_restart` is set. Recorded in `after_state.config_readback`.
- `config_settle.go`: in effect -> outcome monitor; pending restart ->
  `applied_pending_restart` + verification `unverifiable` (never success); not in effect
  -> **reverted at once** and `failed`; unattributable -> `unverifiable`.
- `config_outcome.go`: the monitor credits `success` only when the targeted metric moved
  over the window, measured against a baseline stored at apply time (so resumed monitors
  judge the same baseline): temp-file spills for `work_mem` (at most half the pre-change
  rate predicts, at least 3 expected), dead-tuple ratio for `autovacuum_vacuum_*` GUCs and
  reloptions (autovacuum ran and the ratio fell >= 20%), HOT-update share for `fillfactor`
  (+5 points over >= 100 updates). Everything else is `unverifiable`.
- `rollback.go`: `regressionNone` goes through `settleConfigOutcome` for config actions.
- `manual.go`: the approval path now captures the rollback and reloads/reads back; before,
  an **approved ALTER SYSTEM was never reloaded** (bug).
- `validate.go`, `validate_ast.go`, `sqlast/*`: both validation layers refuse unknown
  storage parameters and `autovacuum_enabled=false` (any spelling, quoted, Unicode-escaped,
  `toast.`), from any producer.
- `approval_required.go`, `executor.go` (`findingRequest`, +1 line): the approval
  guardrail is added for findings marked `approval_required`, restart-required GUCs and
  executable-but-not-autonomous GUCs.

### Unused-index drops (item 12)
- `collector`: `IndexStats.LastIdxScan`, read as `to_jsonb(s)->>'last_idx_scan'` so PG14/15
  (and a mis-detected version) return NULL instead of failing; replication collection
  failure now marks the category unavailable.
- `analyzer/rules_index.go`, `rules_index_standby.go` (new): an index last scanned longer
  ago than the window is a drop candidate even with `idx_scan > 0` (durable evidence, no
  in-memory window needed); a recent scan restarts the window. With streaming replicas,
  physical slots, or unreadable replication state, the drop finding keeps its SQL but sets
  `approval_required`, `replica_index_usage=unknown`, replica/slot counts and an
  explanation in the recommendation.

## Product decisions (and why)
1. **Rollback rule.** RESET when the prior came from the built-in default, a computed
   value, an environment variable, or a `postgresql.conf` line (sourcefile visible); SET
   the prior effective value when it came from `postgresql.auto.conf` or the file is not
   visible. This is the brief's rule made exact: RESET restores precisely when the forward
   change merely shadowed a non-auto.conf value.
2. **Refuse what cannot be undone faithfully.** Command-line, database, role and session
   overrides (ALTER SYSTEM would not take effect there and the server-level prior cannot be
   read), a setting already pending restart, a missing table, and a reloption SET whose
   keys were partly set before (a faithful rollback needs two subcommands) are refused.
3. **Restart-required GUCs are approval-only.** pg_sage cannot restart or verify them; the
   earned-autonomy principle does not allow crediting an unverifiable action.
   Executable-but-not-autonomous GUCs (jit, huge_pages, ...) are approval-only for
   background findings too.
4. **`max_connections` joins the executor whitelist (approval-only).** The connection
   advisor proposed it with SQL, but the executor always refused it. Approval + restart
   flag is the honest middle ground; the advisor's connection floor guard still applies.
5. **A change that did not take effect is reverted immediately** so no unverified value
   lingers in `postgresql.auto.conf` to surface on the next restart.
6. **'unverified' is recorded as the existing `unverifiable` outcome.** The verification
   table's CHECK, recommendation reconcile and earned-autonomy reconcile already treat it
   as not credited; a new string would have been unknown to all of them.
7. **Unknown replica usage means approval, not advisory.** An operator who has checked the
   standbys can still approve the drop. Physical slots count as standbys (an offline
   replica returns); logical slots do not.
8. **Disallowed reloptions/GUCs from the LLM stay visible as advice**, including
   `autovacuum_enabled=false`, with a warning, rather than vanishing.

## Spec CHECKs
None of the AI-SRE spec CHECK-01..42 covers config actions; this work serves the product
principle (verified, reversible, earned). Verification checklist for this change:

```
CHECK-P0C-01: PASS  reload GUC: prior captured, RESET rollback, read-back in effect, rollback restores default (live PG)
CHECK-P0C-02: PASS  auto.conf prior restored exactly by SET (live PG)
CHECK-P0C-03: PASS  restart GUC: applied_pending_restart, not monitored, rollback clears pending_restart (live PG)
CHECK-P0C-04: PASS  reloption prior restored by SET; absent prior restored by RESET (live PG)
CHECK-P0C-05: PASS  autovacuum_enabled=false refused on autonomous and approval paths, table unchanged (live PG)
CHECK-P0C-06: PASS  unknown GUC refused by executor; advisory without SQL in advisor
CHECK-P0C-07: PASS  database-level override refused (live PG)
CHECK-P0C-08: PASS  not-in-effect change reverted and failed (live PG)
CHECK-P0C-09: PASS  Apply path: captured rollback replaces LLM guess, never blind success (live PG)
CHECK-P0C-10: PASS  approval path reloads and captures rollback (live PG)
CHECK-P0C-11: PASS  outcome credited only on measured temp-spill drop; no metric -> unverifiable (live PG)
CHECK-P0C-12: PASS  last_idx_scan collected on PG16+/NULL on PG14-15 (live PG, all versions)
CHECK-P0C-13: PASS  replicas / physical slots / unreadable replication -> approval_required
CHECK-P0C-14: PASS  14/14 mutations killed
```

## Test Results

**Commands:** `go test -p 2 -count=1 -cover` (Docker golang:1.25, `--cpus=2`), PG17
`pgsage-ag1` :55471; touched packages on PG14 :55414 and PG18 :55418; `-race` on touched
packages; full suite on PG17 from the repo root.

**Total:** full suite on PG17: 82 packages ok, 0 failed. Touched packages verbose: executor
931 passed (incl. subtests), pgconf/advisor/analyzer/collector/sqlast 1059 passed; 0 failed,
0 skipped. PG14 and PG18: all 6 touched packages ok. `-race`: all 6 touched packages ok, no
data races. Lint (`golangci-lint run ./...`): 0 issues.

**Coverage (PG17 full suite):**
| Package | Coverage | Floor |
|---|---|---|
| internal/pgconf (new) | 94.8% | 70% |
| internal/executor | 84.7% | 70% |
| internal/analyzer | 86.3% | 70% |
| internal/collector | 86.2% | 70% |
| internal/advisor | 79.6% | 70% |
| internal/sqlast | 89.6% | 70% |

All touched packages meet coverage thresholds.

### Skipped tests
None in the touched packages. `TestConfigRoundTrip_RestartGUCPendingThenRolledBack` skips
by design only where `shared_buffers` does not come from a superuser-visible
postgresql.conf; it ran on PG14, PG17 and PG18.

### Failures
None. During development: the first executor run hung 10 minutes because config actions
now stay monitored and a resume test waited out their windows (fixed by fixture
hygiene, see audit); `TestRunCycleActsOnDurableRecommendationC07` expected an autovacuum
reloption to be verified at once (now inconclusive/unverifiable by design);
`TestRunCycleFailedApplyMovesToFailedWithBackoff` showed the prior capture must run after
the recommendation claim so a refusal fails the attempt with backoff (implementation
fixed); `TestConfigOutcome_TempSpillsMeasuredAgainstBaseline` needed a real spill so its
baseline was consistent (test fixture fixed).

### Coverage gaps
None below threshold.

### Mutation testing (14/14 killed)
| # | Mutation | Killed by |
|---|---|---|
| M1 | auto.conf prior rolled back with RESET | TestGUCRollbackSQL, TestConfigRoundTrip_AutoConfPriorRestoredExactly |
| M2 | autovacuum_enabled=false allowed | TestCheckExecutableReloption, TestValidateExecutorSQL_ReloptionAllowlist, TestConfigApply_DisallowedReloptionRefused |
| M3 | autovacuum_max_workers not restart-required | TestRequiresRestart, TestRequiresRestart_AutovacuumMaxWorkers, TestFindingRequestApprovalRequirement |
| M4 | spill drop factor 0.5 -> 1.0 | TestJudgeTempSpills |
| M5 | last_idx_scan ignored | TestUnusedIndex_LastIdxScanOlderThanWindow, ..._WindowBoundary |
| M6 | physical slots ignored | TestUnusedIndex_PhysicalSlotCountsAsStandby |
| M7 | streaming replicas ignored | TestUnusedIndex_ReplicasRequireApproval |
| M8 | pending-restart change enters the success monitor | TestConfigRoundTrip_RestartGUCPendingThenRolledBack |
| M9 | outcome always credited | 4 live outcome/apply/manual tests |
| M10 | approval guardrail never added | TestFindingRequestApprovalRequirement |
| M11 | unknown GUC stays executable | TestApplyConfigAllowlist_UnknownGUCBecomesAdvisory, TestParseLLMFindings_AppliesConfigAllowlist |
| M12 | read-back always "in effect" | TestConfigRoundTrip_NotInEffect..., TestConfigRoundTrip_RestartGUC... |
| M13 | LLM rollback kept (no capture) | TestConfigApplyPath_GUCRecordedAndNeverBlindSuccess |
| M14 | database override not refused | TestGUCRollbackSQLRefusals, TestConfigPrepare_DatabaseOverrideRefused |

### Bugs found this session
1. [BUG] `executor/manual.go` — an approved (operator) ALTER SYSTEM was written to
   postgresql.auto.conf but never reloaded and had no captured rollback.
2. [BUG] `executor/apply_finding.go` — `monitorFinding` marked an ALTER SYSTEM with no
   rollback `success` immediately, overwriting `applied_pending_restart`.
3. [BUG] Advisor and executor restart lists disagreed; both missed `autovacuum_max_workers`.
4. [BUG] Unknown GUCs and reloptions (incl. `autovacuum_enabled=false`) passed the advisor
   and the executor's `ALTER TABLE SET (...)` check.
5. [BUG] `analyzer/rules_autovacuum_tuning.go` shipped `RESET` as rollback even when the
   table already had a value (now replaced by the captured rollback at apply time).
6. [BUG] `max_connections` was proposed with SQL by the connection advisor but always
   refused by the executor whitelist.
7. [LATENT] time GUC base units were ignored by the range parser (bare `checkpoint_timeout`
   would have been read as ms); fixed in pgconf.
8. [PG QUIRK] RESET of a postmaster GUC whose prior was a computed default (`wal_buffers=-1`)
   leaves `pending_restart=true` until the next restart; pg_sage then refuses further
   changes to it (pending change). The restart test uses `shared_buffers` to avoid
   polluting the shared servers; my PG17 container was restarted once to clear it.

### Post-test audit
- **Untested inputs:** non-superuser read-back (sourcefile hidden -> `unconfirmed`) is
  unit-tested only; the managed-provider read-back path is unchanged and not exercised
  live; dead-tuple and HOT outcome verdicts are judged by pure tests plus live counter
  capture, not by a real autovacuum run inside a window (too slow for CI).
- **Assertions that pass when broken:** covered by the 14 mutations above; every new test
  asserts SQL text, catalog state, outcome strings or JSON detail, not only `err == nil`.
- **Fakes hiding failures:** none for SQL; the outcome tests store a synthetic baseline in
  `before_state` but read real `pg_stat_database` counters.
- **Fixture hygiene added:** config actions are now monitored, so fixtures that leave them
  (`recommendation_cycle_test`, `lease_park_test`) close their monitors; otherwise a later
  `resumeOrphanedMonitors` test waits a full 15-minute window (this hung the first run).

## What is left
- Standby index usage is not collected (no standby DSN config), so every drop on a
  database with replicas needs approval. Collecting `pg_stat_user_indexes` from standbys
  would let it become autonomous again.
- `ALTER DATABASE ... SET` on managed providers (`TransformForCloud`) still ships a
  `RESET` rollback without prior capture.
- `applied_pending_restart` recommendations stay `verifying`; nothing re-checks after a
  restart.
- A measured-but-not-improved change is `unverifiable`, not rolled back; demotion on that
  signal belongs to the Phase 1 trust work.
- No outcome metric yet for WAL/checkpoint GUCs (`max_wal_size`, `checkpoint_*`), planner
  costs or `maintenance_work_mem`; they end `unverifiable`.
- `executor/executor.go` was already 938 lines (one line added here).

## Merge with master (#76 executor split, #78 snapshot dedupe)
- One unused-index decision path: `analyzer/rules_unused_index.go` (#78's reset-aware
  clock) now also treats a recent `last_idx_scan` as use, starts the clock at an old
  `last_idx_scan` (never before the relation stats epoch), and applies the standby gate to
  every drop finding.
- The live pre-drop check (`executor/unused_evidence.go`, autonomous and
  `ExecuteManual`) reads `last_idx_scan` version-agnostically and accepts an index last
  scanned a full window ago; any scan inside the window, or any scan without
  `last_idx_scan`, still refuses. Tests first (`unused_evidence_lastscan_test.go`).
- The approval guardrail call moved to `finding_policy.go`; `manualRun.prepareConfig`
  moved to `config_change.go` to keep `manual.go` under 500 lines.
- After the merge: touched packages pass on PG17 with `-race`, and on PG14 and PG18;
  executor coverage 85.0%; lint 0 issues. `internal/web` is unchanged, so `dist` was not
  rebuilt.
