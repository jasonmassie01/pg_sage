# Phase 3: self-configuration (derived settings), report (2026-10-04)

Branch `claude/p3-self-config` (from `origin/master` = v1.10.0). Roadmap phase 3,
"Self-configuration": *"Most of the 350 knobs (85% need a restart) become derived values with
a derivation ledger (value, evidence, bounds), shadow-tested and pinnable."* This is about
pg_sage's own configuration (intervals, thresholds, timeouts), not Postgres GUC tuning.

## Design

```
 config key ──► class (key_classes.txt): safety_critical | operator_preference | derivable
                                                                   │ rule registered?
 per database, startup (before any worker) and every hour          ▼
   evidence (catalog scan, statements read, sequence scan, max_connections, temp rate)
        │ rule: derive → round up → clamp to bounds (never past the default in the
        │       direction that spends more or widens authority; one fixed ceiling)
        ▼
   operator set it? (YAML leaf, fleet defaults/databases[] alias, API override) ──► operator
   pinned? ──► pinned value
   candidate ≠ active ──► shadow (soak self_config.soak_hours, default 24 h, samples)
        │ soak over: compare against the active value's measured outcomes
        ├─ not worse ──► promoted: live key applied now; restart-bound key pending restart
        └─ worse / too few samples ──► held in shadow, reason recorded once
        ▼
   sage.config_derived_setting (state) + sage.config_derivation (ledger: value, previous,
   cited evidence, bounds, rule + version, reason, actor, time) ── one transaction
        ▼
   in-force values written into the database's runtime config (config hot-reload lock),
   only after the commit, only if the whole config still validates
```

### 1. Classification as data (`internal/config/key_classes.txt`, `key_class.go`)

All 371 keys (every `Config` field path plus every `databases[]` field) are classified,
embedded with `go:embed`: 172 safety-critical, 61 operator-preference, 138 derivable. Tests:
every key classified (a new key fails until classified), no stale entries, the parser rejects
duplicates/unknown classes, authority/credential/endpoint patterns are safety-critical, any key
an environment variable can set is not derivable (found one: `clone.max_clone_age_minutes`,
reclassified), operator-declared routes and windows are preferences. The generated lifecycle
reference gained a "Self-config class" column and `config_meta.json` a `class` field.

### 2. Initial derived keys (`internal/selfconfig/rules*.go`) and why

| Key | Lifecycle | Evidence | Bounds | Shadow comparison |
|---|---|---|---|---|
| `collector.interval_seconds` | live | catalog scan + statements read per cycle | 60-600 s (never below the default) | mean cycle cost vs a 1% budget of one backend |
| `safety.query_timeout_ms` | restart | catalog scan time | 500-5000 ms (fixed ceiling, 10x) | slowest scan in the soak needs 2x headroom |
| `sre.runways.sequence_interval_seconds` | restart | sequence scan time, sequence count | 600 s to min(3600, lookback/(min_samples-1)) | mean scan vs a 0.1% budget |
| `sre.detectors.temp_file_mb` | restart | temp-file rate per detector window | 1024-65536 MiB (never below the default) | share of soak windows over each threshold (raise only if >=10% cross the active and <=5% the candidate) |
| `sre.detectors.lwlock_waiters` | restart | max_connections | 8-64 (never below the default) | none measurable: stability over the soak |

Why these: each one costs or fails on real databases in a way the default cannot know.
lifeos (18 GB, large catalog) could not collect its catalog inside the 500 ms read deadline;
pg_sage's own cycle cost and sequence sampling grow with catalog and sequence counts; the
temp-file threshold is noise on an analytics workload that normally writes GBs of temp files;
8 LWLock waiters is contention on a 100-connection server and noise on a 2000-connection one.
All five are consumed through the database's runtime config (`rt.cfg`: collector, catalog
reads, runways, SRE detectors), so a derived value really reaches its consumer in every mode.
Adding a key is one `Rule` value (key, bounds, direction, derive, observe, compare, get/set);
`ValidateRules` refuses a rule whose key is not classified derivable.

### 3. State machine (`promote.go`) and engine (`engine.go`)

`Step` is pure and holds the promotion rule; `Engine.Reconcile` runs every rule under one
advisory lock per database, commits state and ledger, then writes in-force values.

### 4. Surfaces

- API (new route file `internal/api/derived_settings_routes.go`):
  `GET /api/v1/derived-settings?database=` (every role), `POST
  /api/v1/derived-settings/{key}/pin|unpin?database=` (admin).
- Configuration page: "Derived settings" section on the General tab
  (`web/src/pages/settings/DerivedSettings.jsx`): value, status, default, bounds, lifecycle,
  shadow (value, since, samples, reason), pending restart, pin, evidence, history, Pin
  current / Unpin.
- Docs: `docs/generated/derived-settings.md` (generated by `cmd/gen_config_meta
  -derived-out`, drift-tested), the class column in `docs/generated/config-lifecycles.md`,
  `docs/configuration.md` "Derived settings (self-configuration)", `config.example.yaml`.
- Startup log: one `db "<name>": derived settings: key=value (status) ...` line per database.
- Config: `self_config.enabled` (default true), `self_config.soak_hours` (default 24, 1-720),
  both safety-critical and restart-bound.

### Schema

`internal/schema/self_config_migration.go` (`ddlSelfConfig`, registered after `ddlMCPv2`):
`sage.config_derived_setting` (one row per key; checks tie status to its values) and
`sage.config_derivation` (append-only ledger, index on key and time). Idempotent. Both are
exempt from age-based retention (rows only on change).

## Product calls (made under the product principle; please confirm)

1. **On by default, shadow first.** `self_config.enabled` defaults to true: nothing changes
   for 24 h after a candidate appears, and only a not-worse comparison promotes it.
2. **Bounds never cross the product default in the direction that spends more** (shorter
   intervals, lower incident thresholds). The one exception is the catalog read timeout,
   which may grow to 10x the default (5 s): a deadline shorter than the catalog scan fails
   every cycle and wastes the work done; a longer one only lets the same read finish. It is
   recorded as a fixed ceiling with that justification, and the comparison promotes a raise
   only when observed scans leave less than 2x headroom.
3. **What counts as operator-set:** any leaf the operator wrote in the YAML file (even one
   equal to the default), the fleet `defaults.*`/`databases[].*_interval_seconds` aliases,
   and API overrides (global and per database). Environment overlays only reach
   safety-critical and preference keys (tested). A pass that cannot read these fails closed.
4. **Pins live in the derivation state, per database, not in the config.** "Pin current"
   freezes the value in force; a key set in the configuration cannot be pinned or unpinned
   from the UI (409, "change it there"). Pin/unpin are admin-only; every role reads.
5. **Derivation never writes a value it did not derive.** Without a promoted value the
   runtime keeps its own configured value.
6. **Restart-bound keys** keep the value they started with; a live promotion is reported
   "pending restart" and applied at the next start (ledger `applied`). If a whole-config
   reload resets the runtime config, the startup value is restored on the next pass with no
   ledger entry.
7. **Soak samples are required for measurable rules** (3); a rule with no measurable outcome
   (`lwlock_waiters`) promotes when the candidate held for the whole soak. A hold is recorded
   once per outcome/reason, so the ledger grows only on change.
8. **The collector interval is sized from one cycle's measured cost** (catalog scan +
   statements read), not from pg_sage's total self-cost (see bug 1).
9. **Hourly live pass** (constant); the startup pass is bounded at 1 minute, each evidence
   read is a 5 s read-only transaction.
10. **Probe timeouts are left to the probe-deadline track**: this change derives only
    `safety.query_timeout_ms` (the catalog read deadline read through the config) and does
    not touch probe internals.

## Test Results

**Command:** `go test -cover -count=1 -p 2 ./...` (sidecar; PG17 `pgsage-ag2` :55472;
golang:1.25, `--cpus=2`)
**Total:** 102 packages. First full run: 97 ok, 5 failed: 3 fixed on this branch
(`cmd/pg_sage_sidecar`: derivation reset a programmatic interval, bug 2; `retention` and
`store` registry tests for the new tables and keys), 1 load flake (`sre/probes` 500 ms
probe deadlines while three containers ran; green on rerun and on PG14/PG18), 1
pre-existing on this Windows checkout (`internal/mcp` `TestToolReferenceDocsMatchSchemas`:
the CRLF checkout of docs/mcp.md breaks its marker match; untouched here, identical on
origin/master). All reruns green.
New tests: 108 Go test functions and 7 vitest cases. Verbose run of the new/most-touched
packages (selfconfig, config, gen_config_meta): 474 passed, 0 failed, 0 skipped.

- e2e: `go test -tags=e2e -count=1 ./e2e/` ok (244 s).
- Perf gate: `PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$'
  ./cmd/pg_sage_sidecar` **PASS**.
- PG14 (:55414) touched packages: all ok. PG18 (:55418) touched packages with `-race`: all ok.
- Lint: `golangci-lint run ./...` 0 issues.
- Web: `vitest run` 76 files, 450 tests passed; eslint clean on changed files; `npm run
  build` done and dist committed; node_modules deleted.
- Mutation test (30 mutants: caps both directions, clamp, rounding, empty and cross-key
  bounds, sample minimum, soak wait, promotion outcome, restart gating, operator and pin
  precedence, fail-closed operator set, validity guard, every comparison threshold, pin
  semantics, the derivable-class check): **30 killed, 0 survived**.

| Package | Coverage |
|---|---|
| internal/selfconfig | 92.5% |
| internal/config | 92.3% |
| internal/schema | 84.2% |
| internal/retention | 86.9% |
| internal/store | 76.1% |
| internal/api | 79.5% |
| cmd/gen_config_meta | 87.2% |
| cmd/pg_sage_sidecar | 79.9% |

All touched packages meet the 70% threshold.

### Skipped Tests
None.

### Failures
None remaining on this branch (the mcp docs-marker failure is pre-existing, Windows CRLF).

### Bugs Found This Session
1. [BUG] design: deriving the collector interval from pg_sage's total self-cost per cycle
   ratchets it to the ceiling (selfcost normalizes all of pg_sage's DB time to the collector
   cycle) — replaced by the measured per-cycle catalog + statements cost; tests corrected,
   reason in the commit.
2. [BUG] engine: with no promoted value the engine wrote the product default over the
   runtime's own value (caught by `TestFleetCollectionStatusUsesManagedCollector`) — fixed,
   regression test added.
3. [BUG] classification: `clone.max_clone_age_minutes` is settable by an environment variable
   but was classified derivable — reclassified operator-preference (caught by the env test).

### Post-test audit
- Cap-at-default in the "higher spends more" direction had no registered rule exercising
  it; synthetic-rule tests added (mutant M01 killed).
- Assertions check values, states, ledger rows and runtime config fields, never only errors.
- No fakes over Postgres: evidence, ledger, pins, concurrency, API and runtime run on real
  Postgres (14/17/18). Runtime tests inject evidence only where a small database cannot
  produce large-catalog numbers; a separate test runs real evidence and asserts nothing
  changes on a small database.
- Not covered: real multi-day soaks (a controllable clock covers them) and a genuinely large
  catalog (lifeos is off-limits; the perf gate's fixture exercised startup derivation).

### Manual Checks Remaining
- CHECK-SC-UI: MANUAL — the Derived settings section in a browser (vitest only).

## What is left / open questions

- Coordinator: other tracks that add config keys fail `TestEveryConfigKeyIsClassified`
  until their keys are classified in `internal/config/key_classes.txt` (by design).
- `databases[].collector_interval_seconds` / `defaults.collector_interval_seconds` are
  normalized but never applied to the collector (pre-existing); self-config treats them as
  operator-set so it never overrides them, but the per-database value still has no effect.
- In standalone mode the Configuration page's main settings show the configured value; the
  derived value in force appears only in the Derived settings section.
- Next derived keys (one Rule each): `analyzer.*` thresholds once the analyzer reads the
  per-database runtime config in fleet mode, `retention.*_days` from measured growth,
  `llm.token_budget_daily` below the fleet cap from measured need, probe timeouts once the
  probe-deadline track exposes deadline-error counts.
