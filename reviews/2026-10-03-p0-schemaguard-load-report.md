# P0: decision ledger load (schema guard + policy gate)

Branch `claude/p0-schemaguard-load`, from master `49a088a9`. Not pushed.

## Problem (measured on lifeos, v1.8.1)

The database has 15k tables and 160 leaked `test_*` schemas. `sage.decision` grew from
2.2k to 41k rows/hour in 7 hours: 86.4k schema-guard `observe_only` rows in 3 hours, plus
22.5k `index` and 14.3k `fk_index` parked rows. At the 365-day retention that would be
about 1M rows/day and about 80 GB. Two writers caused it:

1. **The schema guard.** It wrote one row per invariant per cycle. For each invariant it
   also ran an unindexed `count(*) ... target_objects @> $2` over `sage.decision` (one
   core at about 85%), a `pg_stat_statements` ILIKE query, and a contract query.
2. **The standing policy gate** (perf audit `pg_sage-perf/.../static.md` F1). It inserted
   a row with a random `evidence_id` on every evaluation of every candidate, and it ran
   before the cheap skips. The `evidence_id` unique index therefore never caught a repeat.
   F8 adds that the `decision` foreign keys had no indexes, and that retention could only
   use `(database_id, created_at)` while `database_id` is NULL on every row.

## What was built

### Schema guard (`internal/schemaguard`, `internal/autonomy`)

| Concern | Change | Files |
|---|---|---|
| Change-only ledger | A row is written only when the decision for an invariant identity changes, or when the invariant reappears after a scan without it. The hash covers the route, disposition, reason and SQL of every member. Unchanged invariants are still planned and routed every cycle. | `schemaguard/dedupe.go`, `custodian.go`, `types.go`; `autonomy/schema_postgres.go` (recorder) |
| Batched reads | One contract query and one history query per cycle. `pg_stat_statements` is read once per cycle, scoped to the current database, and indexed by qualified name. | `autonomy/schema_sources_postgres.go`, `schema_statements.go`, `schema_detect_postgres.go`, `schema_detect_sql.go` |
| Clone families | Families use the analyzer's definition. Idle leftovers are parked once per identity and never planned or routed. Live families are remediated per member and recorded once per identity, with every member in `target_objects`. | `schemaguard/families.go`, `autonomy/schema_family_postgres.go` |
| Post-DDL debounce | `analyzer.schema_guard_ddl_debounce_seconds` (default 60; 0 means default; range 0-3600; restart-bound). A periodic scan satisfies a pending request. `TriggerSchemaGuard` is not debounced. | `autonomy/ddl_debounce.go`, `supervisor.go`, `types.go`; `config/schema_guard.go`; `cmd/.../autonomy_schema_guard.go` |

### Policy gate (`internal/ledger`, `internal/executor`)

| Concern | Change | Files |
|---|---|---|
| Upsert on fingerprint | Non-execute verdicts carry `fingerprint = sha256(database_id, feature, intent, sorted targets, verdict, policy_version, proposed SQL)`. A repeat of an open fingerprint runs `INSERT ... ON CONFLICT` against the partial unique index: `repeat_count + 1`, `last_seen_at = now()`, and the latest reason, risk, evidence and deadline (unindexed columns, so the update is HOT). The first row's id and `evidence_id` are returned. Execute verdicts get no fingerprint and keep one row each, because they back actions. A resolved row is never reused. If the unique index is missing or INVALID (42P10), the repository falls back to a plain insert. | `ledger/fingerprint.go`, `service.go`, `postgres.go`, `types.go`; `executor/standing_policy.go` |
| Skip order | `processFinding` now runs the read-only skips (cascade cooldown, no open finding, retry limit, oscillation limit) before `gate.Authorize`. | `executor/apply_finding.go` |

### Schema, retention, config

| Concern | Change | Files |
|---|---|---|
| Ledger migration | One DO block registered in `bootstrap.go` adds `fingerprint`, `repeat_count` and `last_seen_at`, then creates `idx_decision_fingerprint` (unique, partial), `idx_decision_schema_guard_targets` (same name and definition as the lifeos index; an existing one is kept), `idx_decision_created`, and an index leading every foreign key into or out of `sage.decision`: `action_log_id`, `queue_id`, `policy_id`, `verification` / `change_lease` / `incident_avoided` `.decision_id`, and `schema_baseline.last_authorized_decision_id`. `action_log.decision_id` was already indexed. Every step checks the catalog first, so a re-run locks nothing. An INVALID index is rebuilt unless a build is in progress. | `schema/decision_ledger_migration.go`, `bootstrap.go` (one line) |
| Retention | Non-execute decisions age out on `retention.decisions_days` (default 30), measured from `COALESCE(last_seen_at, created_at)` and bounded by `created_at`, so `idx_decision_created` serves the purge. A row is kept while an `action_log`, `verification`, active lease or `incident_avoided` row refers to it. Schema guard retention dry runs are also kept, as evidence for later deletes. Execute decisions keep `actions_days`. | `retention/cleanup.go`, `config/decision_retention.go`, `config.go` (field, default, one validate line) |
| Generated docs | `docs/generated/config-lifecycles.md` and `web/src/generated/config_meta.json` were regenerated for the two new keys. Both keys are YAML-only (registered as restart-bound in the store consistency test). | |

**Rows written per cycle (asserted).** On the synthetic catalog (2,000 tables in 40 live
copies plus 400 tables in 40 idle copies, 2,320 invariants), the schema guard writes 58
rows on the first scan (49 FK identities, 9 idle identities), 0 on an unchanged scan, and
exactly 1 after one FK is fixed. It issues at most 12 reads per cycle. On the gate path,
three `RunCycle` passes over a withheld candidate leave 1 row with `repeat_count` 1, 2
and then 3.

## Product decisions (and why)

1. **Ledger rows describe changes.** A schema guard row is the decision that held from
   its `created_at` until the next row for the same `invariant_key`. A gate row is a
   withheld verdict seen `repeat_count` times, from `created_at` to `last_seen_at`. A row
   can be updated in place only if it has no action behind it (execute verdicts are never
   upserted). The schema guard keeps its dedupe state in the ledger, so restarts do not
   re-record. Its only in-memory state is the set of identities seen in the last scan,
   which lets a reappeared invariant be recorded again.
2. **Gate fingerprint scope.** It is the coordinator's list (database, feature, intent,
   target, verdict, policy version) **plus the proposed SQL**. Without the SQL, two
   different candidates on one table (for example two different indexes) would collapse
   into one audit row. A change in reason updates the row's reason; it does not create a
   new row. That loses intermediate reasons, and is the cost of O(candidates) rows.
3. **Skip reorder is behaviour-neutral.** Every moved check returned before any action
   anyway. The gate no longer records verdicts for candidates that the skips would have
   dropped. Pending-approval and recently-rejected checks still run after the gate,
   because a queue verdict depends on them. A pending candidate's repeat queue verdicts
   now update one row instead of adding rows.
4. **`database_id` stays NULL in single-database and YAML-fleet modes. I chose not to
   "fix" it.** Those modes have no `sage.databases` identity, and the standing-policy
   scope, the earned-autonomy concurrency counts
   (`COALESCE(d.database_id,0) = COALESCE($2,0)`), leases and lease conflicts all key on
   the same NULL. Writing a non-NULL id only on decisions would break those equalities,
   which would be a safety regression. The operational problem the audit raised (the
   retention purge could only use the `(database_id, created_at)` index) is fixed with
   `idx_decision_created`. Two follow-ups for the coordinator: give standalone and
   YAML-fleet databases a registry identity everywhere at once, and fix a separate bug
   I found. YAML fleet registers `sage.databases` ids after the runtimes are built
   (`registerFleetDatabases`), so their executors keep a nil id even though an id
   exists.
5. **Indexes are built in Bootstrap, not CONCURRENTLY in the background.** My first
   version built them CONCURRENTLY at autonomy start. On PG18 that deadlocked a fleet
   reload's bootstrap: `CREATE INDEX IF NOT EXISTS` and `ALTER TABLE ... ADD COLUMN IF
   NOT EXISTS` take their lock even when nothing is to be done (verified on PG17), and a
   concurrent build waits for those sessions. The migration is now deadlock-free and
   takes no lock on a re-run. The cost: on first upgrade each missing index is built
   once, under the bootstrap's 30 s budget, pausing only pg_sage's own writes to that
   table. On lifeos (140k rows, 85 MB) that is about a second. **For a much larger
   ledger, pre-create the indexes `CONCURRENTLY` by hand with the same names; Bootstrap
   keeps existing ones.**
6. **Idle leftover families are not remediated** (no FK indexes or rehearsals in copies
   nothing uses). The exception is `unbounded_append`, which comes from an owner's
   declared contract; idleness never overrides explicit intent. "Idle" requires every
   signal: no activity within `analyzer.unused_index_window_days`, no statement and no
   session. "Unknown" counts as live, and the lookup error is kept in `family_reason`.
7. **Workload evidence matching uses qualified-name tokens, not ILIKE.** `public.orders`
   no longer matches `public.orders_archive`, and quoted names now match.
8. **Retention.** Withheld and observation rows follow `decisions_days` (30), not
   `actions_days` (365). A row that keeps repeating is kept, because the window is
   measured from `last_seen_at`. Schema guard dry runs are kept so that retention
   enforcement does not re-dry-run every month.

## Spec CHECKs

No AI-SRE-SPEC CHECK maps directly; this is a Phase 0 load fix. The ledger invariants are
preserved: execute decisions (which back actions) are never upserted or purged early,
`ledger.SelfAudit` is unchanged, and every action still goes through `Executor.Apply`.

## Test Results

**Commands** (Docker `golang:1.25`, `--cpus=2`, `-count=1`):
- full suite: `go test -p 2 -count=1 -cover ./...`, PG17 `pgsage-ag2` (:55472)
- touched packages: `go test -p 2 -count=1 -json`, PG17
- the same set on PG14 (:55414) and PG18 (:55418)
- the same set with `-race`, PG17
- e2e: `go test -tags=e2e -count=1 -timeout 900s ./e2e/`, PG17
- lint: `golangci-lint run ./...`

**Total (touched packages, PG17, `-json`):** 2,548 passed, 2 failed, 0 skipped. The 2
failures were intermittent load failures, each re-run green (see below).

**Full suite (PG17, final run):** 81 packages ok, 3 failing packages:
- `internal/api` `TestRouterTable_MatchesGolden`: CRLF checkout only. It passes with the
  golden file converted to LF; no `internal/api` file is touched by this branch.
- `cmd/pg_sage_sidecar` `TestEpisodeIncidents_StoreUnavailable`: "dial error: timeout"
  connecting to the test server under host load. It passes alone.
- `internal/executor` `TestApplyLockCeilingCapsCycleAnalyze`: the control wait was 1.7 s
  where 4 s was expected (timing under load). It passes 3/3 alone. The test uses a fake
  gate; no code it exercises changed.

**Cross-version (touched packages):**
- PG14: all ok.
- PG18: all ok except one instance of the same lock-ceiling timing test
  (`...CapsOperatorAnalyze`), which passes 3/3 alone.
- `-race` (PG17): all ok, no data races.

**e2e** (`-tags=e2e`, PG17): ok (155 s), run after both the first and the final change.

**Lint:** 0 issues.

**Coverage (touched packages; all at or above 70%):**

| package | coverage |
|---|---|
| internal/schemaguard | 94.1% |
| internal/config | 90.9% |
| internal/ledger | 88.2% |
| internal/executor | 84.4% |
| internal/autonomy | 83.6% |
| internal/schema | 82.3% |
| cmd/pg_sage_sidecar | 81.2% |
| internal/store (test registry only) | 74.5% |
| internal/retention | 97.9-100% |

**Skipped tests:** 0 in the touched packages.

**Failures attributable to this branch:** none in the final runs. During development, my
code caused a PG18 deadlock (bug 2) and a NULL-logic retention bug (bug 1); both are
fixed, with tests.

**Coverage gaps:** none. All packages meet their thresholds.

**Manual checks remaining:** none. No UI is involved.

## Mutation testing

39 mutations of the key logic were applied one at a time (schema guard 24, gate and
ledger 11, migration 4). All 39 were killed. M8 (the schema guard hash ignores the
reason) survived the first round; `TestChangedReasonAloneIsRecorded` was added.

## Bugs found

1. [BUG, mine, found by test] The retention dry-run keep `NOT (feature = ... AND
   evidence->>'disposition' = 'dry_run')` evaluated to NULL for rows without a
   disposition, so it kept every schema guard row. It now uses `IS NOT DISTINCT FROM`.
2. [BUG, mine, found on PG18] A background `CREATE INDEX CONCURRENTLY` deadlocked a
   reload's bootstrap (decision 5).
3. [BUG, pre-existing] `decision.policy_id` was an unindexed foreign key, in addition to
   the audit's list. It is indexed now.
4. [BUG, pre-existing, not fixed] YAML-fleet executors keep a nil `database_id` because
   ids are registered after the runtimes are built (decision 4).
5. [FINDING, pre-existing] `relatedQueryIDs` swallowed errors and matched other
   databases' statements and `LIKE` wildcards. Replaced.
6. [FIXTURES] Five phase-1 fixtures were logically wrong and were corrected, each
   explained in its commit:
   - Idle families built with primary keys: index builds count as scans.
   - A qualified `LOCK` statement was recorded by `pg_stat_statements`.
   - On PG14, statistics are never flushed while idle.
   - A parked verdict re-authorizes in Apply, so the gate is called twice by design.
   - The shared fixture's own execute decision was counted.

## Post-test audit

Per test file, these are the inputs, behaviours and fakes that remain uncovered, and
what was added after the audit.

- **Untested inputs:**
  - A clone family with mixed-case or quoted schema names, end to end. Statement mentions
    are matched case-insensitively and lock mentions exactly. Tokenization is unit-tested
    with quoted names, but the family check is not.
  - The `pg_stat_statements` permission-denied path (42501, "unknown") is not exercised
    on PostgreSQL.
  - The 42P10 fallback (the unique index missing) is tested only as error
    classification, not against a real database. Dropping the index in a shared test
    database would race other packages.
- **Behaviour not asserted:**
  - That a repeat upsert is a HOT update, which the audit suggested checking with an
    `n_tup_hot_upd` delta. The update touches only unindexed columns unless the
    deadline changes; this is not measured.
  - The executor work per routed schema invariant. Routing is intentionally unchanged
    and not bounded by this branch.
- **Assertions that could pass when broken:** the debounce tests rely on wall-clock
  sleeps. Mutations M12 and M13 (debounce off, pending run kept) both made them fail.
  The query bound (at most 12 reads per cycle) holds even if one batched read were
  issued twice. Its purpose is to rule out O(invariants) reads, which M1 and M15 would
  show as writes.
- **Fakes that hide real failures:**
  - `schemaguard` unit tests use `fakeLedger`. The real SQL path is covered by the
    2,400-table PostgreSQL test and by `TestHistoryCountsFamilyRowsPerMemberAndKeepsTheNewestHash`.
    That history test was added during this audit, because family rows' per-member
    counting had only been checked against the fake.
  - The executor skip-order test uses a counting fake gate. The real gate and ledger
    path is `TestRepeatedWithheldCandidateWritesOneDecisionRow`, which uses the real
    standing policy and the PostgreSQL ledger.

## What is left / for the coordinator

- **Decision 4** (`database_id` identity) and the YAML-fleet id ordering bug.
- **Decision 5**: on a very large existing ledger, pre-create the indexes concurrently.
- **The leftover-family classification lives on `claude/p0-tuning-correctness`
  (`380fffcc`), not master**, so the brief's premise was off. `schemaguard.CloneStem`,
  `GroupFamilies` and `ClassifyIdle` mirror the analyzer's rules. Once both land, share
  one implementation.
- **The post-DDL hook only fires for pg_sage's own DDL.** Application DDL never
  triggered a scan. The debounce is defensive.
- **Routing per cycle is still O(planned invariants)**, unchanged by the brief. Each FK
  apply is re-submitted through Apply every cycle. Its gate verdicts now upsert, but the
  executor work per route remains. Worth measuring after deploy.
- **Legacy lifeos rows.** The existing ~140k rows have no fingerprint or
  `invariant_key`. The first cycle after upgrade records one row per identity or
  candidate. Non-execute legacy rows older than 30 days are purged by the new rule.
- **Two runs failed for reasons unrelated to this branch.** `TestRouterTable_MatchesGolden`
  fails only because of the Windows CRLF checkout (it passes with the golden converted to
  LF). `TestOperatorQueueTimeoutRefuses` is timing-flaky under load and passes 3/3 alone.

## Pre-creating the ledger indexes on a large existing install

Run these before upgrading. Bootstrap keeps an index that already exists under the same
name. It adds the columns itself; the fingerprint index needs the column first, so run
the `ALTER` too. The `ALTER` adds nullable columns and one constant default, which
changes only the catalog.

```sql
ALTER TABLE sage.decision ADD COLUMN IF NOT EXISTS fingerprint text,
  ADD COLUMN IF NOT EXISTS repeat_count integer NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_fingerprint ON sage.decision
  (fingerprint) WHERE fingerprint IS NOT NULL AND resolved_at IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_schema_guard_targets ON sage.decision
  USING gin (target_objects) WHERE feature = 'schema_guard';
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_created ON sage.decision (created_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_action_log ON sage.decision
  (action_log_id) WHERE action_log_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_queue ON sage.decision (queue_id)
  WHERE queue_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_policy ON sage.decision (policy_id)
  WHERE policy_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_verification_decision
  ON sage.verification (decision_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_change_lease_decision
  ON sage.change_lease (decision_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_incident_avoided_decision
  ON sage.incident_avoided (decision_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_schema_baseline_decision
  ON sage.schema_baseline (last_authorized_decision_id)
  WHERE last_authorized_decision_id IS NOT NULL;
```
