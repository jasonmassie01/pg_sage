# Phase 3: five-minute time to value (report)

Branch `claude/p3-time-to-value` (from `release/v1.10.0`, merged with `master` before the push).
Roadmap: `reviews/2026-10-02-ai-next/ROADMAP.md`, Phase 3, "Five-minute time to value". Target
metric: time to the first useful finding below 5 min, and non-empty on any real database.
Before this work it was about 10 min and often empty.

## Result

| | Before | After |
|---|---|---|
| First collector snapshot | 1 interval after start (60 s) | at startup |
| First analyzer cycle | 1 interval after start (600 s) | on the first snapshot (polled every 1 s) |
| First finding | about 10 min, often none | **1.1 s** after process start (e2e, `pg_sage_time_to_first_finding_seconds` = 1.138) |
| First look over 11,786 relations | n/a | 298 ms warm, 1.27 s cold (perf gate, small) |
| Standalone without pg_stat_statements | exits | starts degraded; the first look gives the enablement steps |

## What was built

**Catalog-only first look** (`sidecar/internal/firstlook/`)
- It runs as one `READ ONLY, REPEATABLE READ` transaction over `pg_catalog` and the
  statistics views. Every statement has a `LIMIT` and carries the tag
  `/* pg_sage first_look */`. A failed check is degraded on its own: the transaction is
  reopened for the next check, and the note states the reason and the grant that fixes it.
- Statement timeout: it uses `safety.query_timeout_ms`, capped at 5 s, or the session's own
  `statement_timeout` when that is lower.
- Rules (pure functions, each item citing its catalog evidence):
  - invalid indexes;
  - exact duplicate indexes, comparing key and INCLUDE columns, operator classes,
    collations, expressions and predicate, and keeping the copy that backs a constraint;
  - redundant prefix btree indexes;
  - never-scanned indexes, with a caveat that states the statistics window (since the last
    reset, or since server start) and that replica scans are not counted;
  - unindexed foreign keys;
  - XID and multixact runway (thresholds = `analyzer.xid_wraparound_*`);
  - sequence runway, capped by the type of the owning column;
  - dead-tuple bloat estimate, sized from `relpages` (no lock);
  - test-named schemas: items, plus binding-fact proposals when they have been idle over a
    statistics window of 6 h or more;
  - pg_stat_statements, HypoPG and auto_explain, with exact steps per provider (RDS,
    Aurora, Cloud SQL, AlloyDB, Azure, Neon, Supabase, self-managed);
  - hidden query text.
- `Store`: `sage.first_look` keeps the last 10 reports per database.
- `Summarizer`: an optional model headline. Findings are passed as untrusted data, and the
  findings never depend on the summary.

**Onboarding** (`sidecar/internal/onboarding/`)
- Install kind is decided once per database, before the runtime collects or starts its
  trust ramp. It is `existing` if `trust_ramp_start` or any snapshot exists, otherwise
  `new`. The snapshot check reads the newest row through the `collected_at` index.
- Records the first look, and the first finding with its time to first finding (TTFF), once.
- `Tracker`: TTFF per runtime generation, for `/metrics`.
- `TrustGuide`: for each trust level, what it allows, what it never does without approval,
  what it still waits for (ramp hours left, `tier3_*`, execution mode), and which grants it
  needs.
- `CheckGrants`: a live, superuser-free check of `pg_monitor`, `pg_read_all_stats`, the
  sage schema, table ownership, `pg_maintain` (PostgreSQL 17+), `pg_signal_backend` and
  `ALTER SYSTEM` (PostgreSQL 15+ per-parameter grants).
- `Checklist`: connected, extensions, first look, MCP token, notifications, grant more.

**Wiring**
- `cmd/pg_sage_sidecar/database_runtime_firstlook.go`: every runtime records its install
  kind, then runs the first look at once on its worker group. It stores the report,
  proposes facts (fact cards go out as for any proposal), records TTFF and asks for the
  summary. An empty first look polls for the analyzer's first open finding (15 s, up to 1 h).
- Metrics: `pg_sage_time_to_first_finding_seconds`, `pg_sage_first_look_items` and
  `pg_sage_first_look_duration_seconds`, one series per database. A series appears only
  once its value is known.
- `collector.Run` collects at startup. `analyzer.Run` waits for the first snapshot (at most
  one interval) instead of skipping the cycle.
- `startup.ErrStatementsUnavailable`: a missing or unreadable pg_stat_statements degrades a
  database even when its checks are required (standalone). A wrong version or no
  connection still refuses.

**Database:** migration `ddlOnboarding` (`sage.first_look`, `sage.onboarding`), idempotent,
with one registration line in `bootstrap.go`.

**API** (`internal/api/onboarding_*.go`, one line in `router.go`)
- `GET /api/v1/onboarding`, `GET /api/v1/first-look` and `GET /api/v1/onboarding/trust` are
  open to every signed-in role.
- Whether an MCP token or a notification channel exists is shown to admins only.
- The trust guide names how this mode changes `trust.level`: `/api/v1/config/global`, the
  database's own config row in meta-db mode, or the YAML line in a YAML fleet.

**Web**
- `OnboardingPanel`, `FirstLookPanel` and `GrantMoreDialog`, shown on the landing route
  above Value.
- "Grant more" PUTs `trust.level` with `expected_generation` through the same config API
  the Settings page uses. The embedded dist was rebuilt.

**Docs**
- `docs/quickstart.md`: the role SQL, the docker one-liner and the binary, and what you see
  in minute 1, minute 5 and hour 1. It is linked from the README, the index and the nav.
- `TestQuickstart*` checks that every config key, CLI flag, environment variable, metric,
  API path and quoted UI label in the quickstart exists.
- The e2e test runs the quickstart's role SQL verbatim.

## Product calls (owner decisions implemented; further calls made here)

1. **An explicit trust level counts as the grant.** The default `trust.level` is already
   observation, so a new install only observes until someone grants more. If the operator
   sets `trust.level` in YAML or with `SAGE_TRUST_LEVEL`, that is honored from the first
   start. I chose this because IaC/Helm/fleet deployments configure trust up front, and the
   existing tests that start fresh databases with `advisory`/`autonomous` encode that
   contract. A hard cap would also have needed an opt-out flag everywhere. The landing page
   still shows "Grant more" until the level is above observation.
2. **"Read-only" means nothing changes outside the `sage` schema.** Shadow decisions, the
   first look, findings and facts are recorded in `sage` (allowed by the owner decision).
   HypoPG hypothetical indexes are session-local. The e2e test asserts zero `action_log`
   rows and unchanged user indexes on a new install.
3. **A missing pg_stat_statements no longer stops a standalone start.** It degrades with a
   stated reason. Without this, the most common "real database" fails before any value
   shows.
4. **Any first-look item counts as the first finding,** including info items such as
   "auto_explain not loaded, here is how". This matches "non-empty on any real DB". The
   analyzer's first open finding counts when the first look is empty.
5. **Fact proposals from the first look** are made only for test-named schemas with zero
   traffic over a statistics window of at least 6 h, the same quiet period as the facts
   detector. Subjects match the detector's family patterns, so proposals de-duplicate.
   They never bind until confirmed, and pg_sage never drops a schema.
6. **Thresholds:** XID runway uses the analyzer's `xid_wraparound_warning`/`critical`.
   Sequences: warning at 70 %, critical at 90 %. Bloat: tables of 100 MB or more with
   20 % or more dead tuples (warning at 50 %). Unindexed foreign keys: tables with 1,000 or
   more estimated rows, or never analyzed. Never-scanned indexes are a warning only over a
   window of 7 days or more, otherwise info.
7. **TTFF is measured from process start** for a database's first runtime, and from the
   runtime's start after a rebuild.
8. **The first look's statement timeout is `safety.query_timeout_ms`** (default 500 ms,
   the same as the collector), capped at 5 s. At scale the perf gate shows no check
   degraded.

## Test Results

**Commands** (all in `golang:1.25` with `--cpus=2`):
- `go test -p 2 -count=1 -cover ./...` on PG17 (`pgsage-ag5`, :55475);
- the touched packages on PG14 (:55414) and PG18 (:55418);
- `go test -tags=e2e -count=1 -timeout 900s ./e2e/`;
- `go test -tags=perfgate -run '^TestPerfGate' ./cmd/pg_sage_sidecar` with
  `PG_SAGE_PERF_SCALE=small`;
- `-race` on firstlook, onboarding, collector and facts, plus the onboarding, first-cycle
  and wiring tests in api, analyzer and cmd;
- `golangci-lint run ./...` (also with `--build-tags e2e,perfgate`);
- `npm ci && npm run lint && npm test && npm run build`.

**Total (PG17 full suite):**
- 101 packages ok, 2 failed. Neither failure is in a package this branch touches:
  - `internal/mcp` `TestToolReferenceDocsMatchSchemas`: environment only. The CRLF
    checkout on Windows breaks the `-->
` marker match in the unchanged `docs/mcp.md`.
  - `internal/sre/probes`: four probes exceeded their deadline while two test containers
    ran on the shared VM. The package re-run alone passed.
- An earlier run is void: another process emptied the shared `gobuildcache` volume halfway
  through and about 40 packages failed to build. Reruns used a private
  `gobuildcache-p3ttv` volume.

**New tests:** 116 Go tests and 16 vitest tests.
- Tests in firstlook, onboarding and api: 1,023 passed, 0 skipped.
- e2e: 21 passed, 13 skipped, 0 failed.
- web: 459 passed (78 files).

**Measured time to first finding (e2e, PG17):**
- 1.14 s (first run) and 1.45 s (second run), from `pg_sage_time_to_first_finding_seconds`.
- The first API poll that saw a non-empty first look came 2.7 s and 3.6 s after process
  start. That poll waits out login first.

**Perf gate (small):**
- `TestPerfGateFirstLook`: 11,786 relations, 102 items, in 298 ms (budget 5 s). No
  degraded check, zero user-table sequential scans, every first-look statement under
  500 ms.
- `TestPerfGate`: 0 offenders.

**Cross-version (touched packages):**
- PG14: all ok. The first run's `TestMetaReconcileConcurrentPassesPublishOneRuntime`
  timed out (30 s per item) while two suites ran at once; it passed on re-run, together
  with the whole cmd package.
- PG18: all ok except two load or connection flakes in cmd. `TestValueMetricsFleetMode...`
  could not dial :55418, and `TestFleetReloadRemovesDatabaseAndCleansUp` found no
  connection at the instant it sampled. Both pass in isolation.

**Race:** all ok, no data race. **Lint:** 0 issues (Go and eslint).

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/firstlook (new) | 87.8% |
| internal/onboarding (new) | 90.2% |
| internal/facts | 86.5% |
| internal/startup | 92.5% |
| internal/schema | 84.2% |
| internal/collector | 90.0% |
| internal/analyzer | 91.6% |
| internal/api | 78.8% |
| internal/config | 91.3% |
| internal/retention | 87.3% |
| cmd/pg_sage_sidecar | 79.8% |

All touched packages meet the 70% threshold. Below threshold, all untouched:
`cmd/create_admin` 51.5%, `cmd/reset_admin_for_test` 50.0%,
`cmd/sigstore_trusted_root` 56.1%, `internal/testsupport/pgssepoch` 58.3%,
`sre-bench` 65.7%.

### Skipped tests (must be zero or justified)

- e2e: 13 live-LLM tests (`TestLLM*`, `TestTunerLLM_*`, `TestOptimizerMultiQueryConsolidation`):
  `SAGE_LLM_API_KEY` is not set. Live LLM tests run only with a key, per the rules.
- New packages: none.

### Failures

None in touched packages. The two non-touched failures and the flakes are explained
above.

### Manual checks remaining

- CHECK-01: MANUAL. The landing page in a browser: checklist, first look and the "Grant
  more" dialog, light and dark themes. Vitest covers the behaviour.

## Mutation testing (first-look rules)

25 hand mutations, run with `go test` against the package each one touches:
- Round 1: 22 mutations, 17 killed.
- The 5 survivors led to new tests and to one real bug fix (below).
- Round 2: all killed except M25, which is an equivalent mutant: the guard before it makes
  `Columns[:n]` equal to `keys()[:n]`.

Killed mutants include:
- invalid flag flipped;
- keep-rank preference;
- prefix length;
- scan count;
- the 7-day boundary;
- foreign-key column sort;
- foreign-key minimum rows;
- XID boundary;
- sequence type cap, ascending and descending;
- sequence severity swap;
- cycle skip;
- bloat size boundary;
- schema activity;
- pg_stat_statements severity;
- preload de-duplication;
- fact idle window;
- checklist grant;
- unique skip;
- INCLUDE handling;
- tracker once-only;
- session timeout.

## Bugs found this session

1. [BUG] `firstlook/rules_index.go`: INCLUDE columns were treated as key columns. A plain
   index was not redundant to `(a) INCLUDE (b)`, and an INCLUDE column could look as if
   it served a foreign key. Found by mutation testing; now uses `indnkeyatts`.
2. [BUG] `onboarding/state.go`: the install-kind check used `EXISTS (SELECT 1 FROM
   sage.snapshots)`, a sequential scan of a large sage table (perf gate offender). It now
   reads the newest row by index.
3. [BUG, pre-existing behaviour] A standalone start without pg_stat_statements exited
   (fixed as product call 3).
4. Test fixtures fixed, reasons in commit `d2d6a5b5`:
   - the bloat fixture never analyzed its table;
   - the "invalid name" case used `;`, which `net/url` drops;
   - the perf test read its scan baseline before the catalog builder's late statistics
     arrived.

## Post-test audit

- **Untested inputs.** Partitioned parents with unindexed foreign keys on partitions (only
  the parent constraint is read, `conparentid = 0`). Exclusion-constraint duplicates.
  `pg_sequences` rows for sequences dropped mid-pass. The perf gate's large scale
  (35k relations), which was not run, only small.
- **Assertions that could pass when broken.** The provider steps are checked by keywords,
  not run against real RDS, Cloud SQL or Azure. They are written to the providers'
  documented mechanisms but are unverified live. The LLM summary is covered only with a
  fake server.
- **Fakes that hide failures.** The API handler tests use a fake reader. The real reader
  runs in the e2e test (`/api/v1/onboarding`, `/trust`, `/first-look` over HTTP with
  session auth) and in the composed runtime tests.

## Left open

- Large-scale perf gate (`PG_SAGE_PERF_SCALE=large`) for the first look: the budget is
  20 s; not run tonight.
- "Grant more" in a YAML fleet only shows the line to set. A per-database grant in a YAML
  fleet still means editing `databases[].trust_level`.
- The provider enablement steps should get one live check per provider when the managed
  test accounts are available.
