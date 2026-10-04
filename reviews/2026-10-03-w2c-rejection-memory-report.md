# W2-C: optimizer what-if rejection memory

Branch `claude/w2c-rejection-memory` (cut from `origin/release/wave1`).

## Problem

On lifeos (2026-10-03) the LLM optimizer proposed one index on `public.ai_claims` 18 times in
about three hours, each time under a new name and a slightly different INCLUDE list
(`ai_claims_evidence_pattern_idx ... INCLUDE (id, status)`, `..._event_ids_pattern_idx ...
INCLUDE (id)`, `..._prefix_idx ... INCLUDE (status, id)`). HypoPG rejected each one with
"call-weighted improvement 0.0% is below the 10.0% minimum". Nothing remembered the
measurement, so every cycle spent an LLM call and a what-if on an idea already measured.

## What was built

| File | What |
|---|---|
| `sidecar/internal/schema/optimizer_rejection_migration.go` | New idempotent table `sage.optimizer_rejection` (one row per database, table, shape hash; CHECKs bound sizes; index on `measured_at`). Registered with one line in `bootstrap.go`. |
| `sidecar/internal/retention/rules.go` | Retention rule: rows age out on `retention.findings_days` from `measured_at`. |
| `sidecar/internal/config/optimizer_rejection_memory.go` | `llm.optimizer.rejection_memory.*` (enabled, max_age_days, call_volume_ratio, mean_time_ratio, row_estimate_ratio, prompt_max_shapes) with defaults and range validation; one field + one default + one validate line in `config.go`. |
| `sidecar/internal/optimizer/rejection_shape.go`, `rejection_sql.go` | Shape normalizer and matcher: access method; ordered keys with opclass (+parameters), collation, non-default order (ASC / default NULLS dropped); canonical expressions (outer parens, spacing, quoting, `!=`); predicate with top-level AND terms sorted (kept as written when there is a top-level OR or BETWEEN); INCLUDE as a sorted set. Name and table qualification ignored. `sameIdea` = same method, keys, predicate, and one INCLUDE set contains the other. |
| `sidecar/internal/optimizer/rejection_memory.go` | Material-change rules, per-cycle table view (suppress / learn / prompt lines), `remember`. |
| `sidecar/internal/optimizer/rejection_store.go` | Postgres store: `recent` (current database, table, max age, newest first, limit 100) and an upserting `record` (bumps `measure_count`, replaces evidence). |
| `sidecar/internal/optimizer/optimizer.go`, `optimizer_cycle.go`, `openrecs.go`, `prompt.go`, `types.go` | Wiring: the memory is built when a pool exists and memory is enabled; `admit` checks memory just before the what-if and remembers a what-if rejection; the open-recommendation re-check remembers but is never suppressed; the prompt gets an "Already measured" section (bounded) and system-prompt rule 14; `Result.MemorySkips`; one DEBUG summary line per cycle. The per-table loop moved to `optimizer_cycle.go` (keeps `optimizer.go` under 500 lines and `Analyze` from 95 to 57 lines). |
| Docs | `CHANGELOG.md` (Unreleased), `docs/configuration.md`, `docs/architecture.md`, `sidecar/config.example.yaml`, regenerated `docs/generated/config-lifecycles.md` and `sidecar/web/src/generated/config_meta.json`. |

## Design

1. Analyze -> per table: open recommendations re-emitted (unchanged) -> otherwise load the
   table's memory view (one indexed query) -> keep only rejections not materially changed ->
   prompt lists up to 5 newest shapes -> LLM -> each candidate: canonicalize -> validator ->
   **memory match? skip the what-if** -> what-if -> on a complete rejection, upsert the
   rejection and add it to this cycle's view (a second copy in the same reply is skipped too).
2. Material change (any one re-opens the idea): age >= `max_age_days` (7); the row estimate
   (`n_live_tup`) changed by >= `row_estimate_ratio` (2x, up or down); a target query's calls
   or mean time changed by >= its ratio (2x); a target query appeared or disappeared.
   Ratios are symmetric and floored (calls and rows at 1, mean time at 0.001 ms) so zero is a
   large change, never a division by zero. Calls are pg_stat_statements' cumulative counts,
   so a skipped idea is re-measured after the workload doubles, and the new measurement
   becomes the baseline: a natural geometric backoff.
3. Fail open everywhere: a memory read error is logged at WARN and every candidate is
   measured; a write error is logged at WARN; an unparseable or oversized (>8 KB) DDL is
   measured, not remembered. Memory saves work; it never decides admission.

## Product calls (made under the AI-DBA lens; please confirm)

1. **One row per shape, query set stored, not keyed.** The brief says "keyed by ... the
   fingerprint set of the target queries". I key the row by database, table and shape hash,
   and store the queryids (with calls and mean time) in the row; a different query set is a
   material change on lookup. Same matching behaviour, but the table cannot grow one row per
   workload variant of the same idea.
2. **A vanished target query is a material change too**, not only a new one: if the heavy
   query that kept the weighted improvement at 0% stops running, the idea may now pass.
3. **Ratios are symmetric** (a halving counts like a doubling) and the boundary is inclusive
   (exactly 2.0x is material, 1.99x is not; exactly 7 days is expired).
4. **Only complete what-if rejections are remembered.** Unverified outcomes (HypoPG missing,
   some queries unplannable, errors) and confidence-threshold rejections are not.
5. **Re-checks of open recommendations are never suppressed**, but their what-if rejections
   are remembered so the model hears about them. There is no operator-triggered what-if path
   in the optimizer today; memory is consulted only in the LLM admission path, so any such
   path built on `enrichWithHypoPG` / `reverify` is unaffected by construction.
6. **Deterministic detectors are out of scope by construction**: the missing-FK-index rule
   lives in `internal/analyzer/rules_fk_index.go` and never passes through the optimizer.
7. **The LLM is still called every cycle** (as specified: memory is fed to the model). The
   what-if is what is saved; the model is told not to repeat. See open question 1.
8. **Prompt shapes are sanitized like all prompt text**: string and numeric literals in a
   predicate are redacted to `?` (existing `llm.SanitizeForLLM` policy, to keep row values out
   of the prompt). Matching itself uses the unredacted shape, so `status = 'open'` and
   `status = 'closed'` remain different ideas.
9. **Opclass equivalence is literal**: `x text_ops` and `x` (default opclass) are different
   ideas, because resolving default opclasses needs the catalog. Worst case is one extra
   measurement.
10. **Retention**: rows purge on `findings_days` (default 180) from the last measurement;
    rows older than `max_age_days` are never read again, so purging only frees space.
11. Memory scope is `current_database()` on the monitored database's own `sage` schema, so
    fleet databases never share rejections.

## Test Results

**Command:** `go test -count=1 -p 2 -cover -v ./...` (Docker `golang:1.25`, own PG17 `pgsage-ag8`
:55478, HypoPG 1.4.3 installed)
**Total:** 12062 passed, 2 failed, 21 skipped (final full run). Both failures are in packages
this branch does not touch and pass on re-run (see Failures).
**Coverage (touched packages, PG17):** optimizer 91.4%, config 91.1%, schema 83.8%,
retention 86.9%, store 76.1%. New optimizer code: mean per-function coverage 93.5% over 43
functions (lowest: `opclass` 72.7%, `hash` 80%, `truncateBytes` 80%).

Other runs:
- `go test -tags=e2e -count=1 -timeout 900s ./e2e/` (PG17): ok, 20 passed, 13 skipped (all
  live-LLM tests: `SAGE_LLM_API_KEY not set`).
- Perf gate `PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run TestPerfGate
  ./cmd/pg_sage_sidecar/` (PG17): PASS (167 s).
- `-race` on optimizer, config, schema, retention (PG17): ok.
- PG14 (:55414) touched packages: ok, 1794 passed, 0 failed, 2 skipped (pre-existing:
  `GENERIC_PLAN` needs PG16). PG18 (:55418): ok, 1796 passed, 0 failed, 0 skipped. HypoPG is
  available on both matrix servers, so the real-HypoPG end-to-end test ran on PG14, 17 and 18.
- golangci-lint `./...`: 0 issues. gofmt clean on every changed file.

### Skipped Tests (must be zero or justified)
None in touched packages on PG17/PG18. Full-suite skips are all pre-existing environment gates:
live LLM (`PG_SAGE_LIVE_LLM`/`SAGE_LLM_API_KEY`, 3), live cloud provisioning (AWS RDS, Cloud
SQL, Lakebase, Agent DB gauntlet, Azure: 8), PgBouncer URLs (3), disposable standby/restart
servers (3), Windows-only path (1), child-process fixture (1), plan fixture regeneration (1),
full bench `SAGE_BENCH_RUN` (1). PG14: `TestGenericPlanCapturesUnboundParameters`,
`TestPurgeGenericPlansProbeLargeTablesByIndex` (need PG16 `GENERIC_PLAN`).

### Failures (if any)
- internal/partition: `TestConvert_ConcurrentKeyBuildWaitsOutLongTransactions` — "conversion
  took 2.76s: the key build did not wait" (timing under full-suite load); passes on re-run.
- internal/sre/slo: `TestLatencyProxy_QueriesIdleInThePreviousCaptureStillCount` — bootstrap
  advisory-lock connection deadline (30 s) under load; passes on re-run.
- The first full run also had two load flakes, analyzer (fixture DB connect timeout) and
  retention `TestRun_DoesNotWaitForTheConversion` (4.49 s), both green on re-run; and one real
  failure in my own audit test (below), fixed.

### Coverage Gaps (packages below threshold)
All touched packages meet thresholds: optimizer 91.4, config 91.1, schema 83.8, retention 86.9,
store 76.1 (all business >= 70%).

### Bugs Found This Session
1. [TEST BUG] `rejection_audit_test.go` — the first UTF-8 truncation test used a long string
   literal, which the prompt sanitizer redacts to `'?'`, so nothing was truncated. Rewritten
   with a long quoted identifier. (Also surfaced product call 8: literals are redacted in the
   prompt section.)
2. [DESIGN BUG caught before code] Sorting a predicate's AND terms after stripping each term's
   parentheses would have made `(a=1 OR b=2) AND c=3` equal to `a=1 OR b=2 AND c=3`; a term
   that is a disjunction keeps its parentheses (`TestCandidateShape_DifferentIdeas`, "or/and
   grouping").
3. [PRE-EXISTING, not fixed] wave-1 merge lost `## Unreleased` in `CHANGELOG.md`; one gofmt
   misalignment in `internal/store/config_consistency_test.go`.

### Mutation testing (matcher, material change, wiring)
24 mutants, all killed: predicate ignored; INCLUDE always same / must be equal; opclass
dropped; DESC dropped; default NULLS kept; AND terms unsorted; OR ignored when splitting;
quotes always dropped; operators fused (`- -1` -> `--1`); each ratio and the age check made
strict (`>` for `>=`, 4 mutants); vanished query ignored; new query ignored; material filter
removed; prompt bound removed; INCLUDE unsorted; suppression removed; admission not
remembered; reverify not remembered; other databases visible; no in-cycle learning.

### Post-test audit
- Untested inputs found and added: function-call keys with commas, non-ASCII identifiers,
  UTF-8 truncation, and the moved per-table branches (budget exhausted, model failure, open
  circuit).
- Assertions that would pass if broken: none found; every flow test asserts what-if call
  counts, stored rows/counts and prompt content, not only `err == nil`.
- Fakes that hide failure: `memStore` does not filter by age (deliberately, so the Go-side age
  check is tested); the real store's age window, ordering, scoping, upsert and concurrency are
  tested on Postgres. The counting what-if fake is backed by an end-to-end test with real
  HypoPG on PG14/17/18. Not testable offline: whether a real model obeys the "Already
  measured" list (live-LLM tests need a key); the memory works regardless, since the
  what-if is skipped either way.

### Manual Checks Remaining
- MANUAL: after deploying to lifeos (not touched by this work), confirm `sage.optimizer_rejection`
  gets one `ai_claims` row and the optimizer logs one DEBUG summary per cycle instead of an
  INFO rejection per cycle.

## Code limits
New functions <= 50 lines, new files <= 500 lines, new lines <= 100 chars except the config
`doc:"..."` struct tags (a Go struct tag cannot be split; same convention as `config.go`).
Pre-existing over-length functions touched: `Analyze` went from 95 to 57 lines;
`FormatPrompt` (+1 line) and `scoreConfidence` are unchanged otherwise.

## Open questions

1. Should the optimizer skip the LLM call for a table whose last N replies were all memory
   hits on an unchanged workload? That would save the remaining token cost (the 18 lifeos
   calls); today only the what-if is saved and the model is asked not to repeat.
2. `Result.MemorySkips` is not exported as a metric; worth a Prometheus counter?
3. Two pre-existing issues seen while working (not changed): the wave-1 merge dropped the
   `## Unreleased` heading in `CHANGELOG.md`, so the wave-1 bullets now sit under v1.8.5 (I
   added a new `## Unreleased` at the top with this bullet only); and
   `internal/store/config_consistency_test.go` has one gofmt misalignment
   (`verify.drop_window_hours`).
4. The bundled UI (`internal/api/dist`) was not rebuilt; the six new keys get tooltips at the
   next dist rebuild (the coordinator rebuilt dist at the wave-1 merge).

## Follow-up (owner decisions on the open questions)

1. **LLM calls are skipped too.** `llm.optimizer.rejection_memory.skip_llm_after` (default 3,
   range 1-100): after that many consecutive *wasted* proposals for a table (a reply whose
   every candidate was a memory hit or a fresh what-if rejection) with no material change
   since the streak began, the model is not asked about the table until a material change or
   `max_age_days` from the streak's start (same rules as the rejection memory). Files:
   `internal/optimizer/rejection_streak.go`, `optimizer_cycle.go`.
   Calls made:
   - A *proposal* is one model reply for a table, not one candidate, so one reply with three
     duplicate candidates does not skip the model at once.
   - An empty reply (or a failed call) is neutral: it neither extends nor breaks the streak.
     Any other outcome (an admitted or unverified candidate, a validator rejection) breaks it.
   - Streaks live in process memory (one entry per table). After a restart a table is asked
     up to `skip_llm_after` more times before it is skipped again; this avoids a second table
     and costs at most three calls per table per restart.
   - Operator-requested runs (`optimizer.WithOperatorRequest(ctx)`) always ask the model and
     measure every candidate (no what-if suppression either). No operator trigger for the
     optimizer exists yet; this is the hook for one.
   - The cycle's one DEBUG summary line now also names the tables whose model call was
     skipped (`... and the model for N table(s) (...)`); `Result.LLMCallsSkipped` counts them.
2. **Prometheus counters** (process-lifetime, per database, same hand-written exposition and
   per-instance iteration as the self-cost metrics):
   `pg_sage_optimizer_whatif_skipped_total{database}` and
   `pg_sage_optimizer_llm_calls_skipped_total{database}`. Read through
   `Analyzer.OptimizerMemoryStats()`; databases without an optimizer export nothing.
   Files: `cmd/pg_sage_sidecar/optimizer_memory_metrics.go` (+1 line in `prometheus.go`),
   `internal/analyzer/optimizer_stats.go`.
3. **CHANGELOG**: merged `origin/release/wave1` at cbed8d07; `## Unreleased` holds the wave 1
   entries plus this bullet; everything from `## v1.8.5` down is byte-identical to
   `origin/master`.
4. Web dist and the pre-existing gofmt misalignment left alone.

Tests first (commit d76c3c3d), then the implementation. New: boundary at N-1/N, configured N,
useful proposal resets, validator rejection resets, empty reply neutral, material change
resets (at and before N), max age lifts the skip (7 d - 1 s still skips, 7 d asks), operator
request bypasses the skip and the what-if suppression, no memory never skips, per-table
streaks, one DEBUG summary line, 8 concurrent cycles under `-race`, counters and their
exposition (sorted, quoted labels, nothing without optimizers), analyzer accessor, config
default/range. Mutation check of the streak: 12 mutants (threshold off by one, material change
ignored, useful never resets, streak never restarts, empty reply resets, each operator bypass
removed, each counter not incremented, rejections not counted, never skip, streak not per
table); all killed. The "never restarts" mutant first survived; I added
`TestModelSkip_MaterialChangeMidStreakRestartsCount`, which kills it.
