# P0 trust ledger: per-database earned autonomy, verified-only credit, review UI

2026-10-02 · branch `claude/p0-trust-ledger` (from master v1.8.1) · roadmap
`reviews/2026-10-02-ai-next/ROADMAP.md` Phase 0 items 5 and 6, Phase 1.1 (the part that
unblocks promotion). Evidence: `autonomy.md` §3.1, §3.5, I1, I4; `sre.md` #14, #21-26, I1-I2;
`interfaces.md` I1-I2.

## What was built

### P0-5 — the earned-autonomy ledger is per database

Before: `sage.sre_family_autonomy`, its proposals and every evidence query were keyed by
`deployment_id` only, and one `earned.Service` served every database of a control database.
In a fleet with a meta database, orders' shadow reviews could promote billing, and billing's
harmful outcome demoted orders.

Now every level, proposal, history entry, shadow review, live outcome, safety violation,
game day, failover cooldown and carried-over pair belongs to one database. PGIncidentBench
reports stay deployment-wide (they measure pg_sage, not a database).

- `internal/schema/sre_m7_database_scope_migration.go` (new, idempotent, registered with one
  line in `bootstrap.go`): adds `database_name` to `sre_family_autonomy` (new primary key
  `(deployment_id, database_name, family, action_class)`) and `sre_autonomy_proposals` (the
  one-pending index is redefined in place, same name, now per database); adds the
  `unverified` outcome result and the `database_scoped` history event; adds per-database
  indexes on events, outcomes and reviews; assigns legacy rows (below); supersedes legacy
  pending proposals. Constraint and index names the earlier M7 migrations re-create are
  redefined in place, so re-running the whole bootstrap never fights this migration.
- `internal/earned/store*.go`: `NewPostgresStore(pool, deploymentID, database)`; every
  query on levels, proposals, events, reviews, outcomes and game-day runs is filtered by
  the database; `LatestBench` stays deployment-wide. A write, review, outcome, game-day
  report, carry-over seed or history filter naming another database is refused
  (`ErrInvalidRequest`).
- `internal/earned/limiter.go`: a limiter bound to another database's ledger fails every
  authorization closed. HA role, failover cooldown, error budget and safety record were
  already per binding; the safety record now reads only the database's own outcomes.
- `internal/earned/legacy.go`: `AdoptLegacy` (see decision D1).
- `internal/earned/reconcile.go`: a reconciler records only into its own database's ledger.
- `cmd/pg_sage_sidecar/autonomy_wiring.go`: one ledger per (control pool, database);
  `install` adopts legacy levels, then seeds carry-over, for that database.

### P0-6 — only verified outcomes earn credit

`classifyOutcome` (`earned/reconcile.go`) counted `outcome='success'` with no verification
verdict as `verified_recovery`, the counter L3 promotion uses. Now:

| action_log outcome | verification verdict | ledger result |
|---|---|---|
| success | success | verified_recovery |
| success | none, executed < 24 h ago | pending (a verdict may still come) |
| success | none, executed >= 24 h ago | **unverified** (new) |
| success | unverifiable | **unverified** (was not_recovered) |
| success | pending / extended | pending |
| failed, or verdict failed | any | not_recovered |
| rolled_back / reverted / rollback_failed, or verdict revert | any | harmful |
| anything else | any | pending |

`unverified` outcomes are recorded (once per action) and counted in `Live.Unverified`; they
never count toward `live_l2_recoveries` and never demote.

### Phase 1.1 — promotion is reachable without curl

- `internal/earned/packetreview` (new): one review of a concluded or inconclusive
  investigation writes the shadow review (accepted / rejected) and the investigation outcome
  (confirmed / refuted). A causal-graph node id as the actual root cause becomes the
  outcome's `actual_node`; free text stays in the review note.
- API: `POST /api/v1/sre/autonomy/reviews` takes `actual_root_cause` and returns what it
  recorded (`investigation_outcome` or `investigation_outcome_skipped`).
  `POST /api/v1/databases/{db}/investigations/{id}/outcome` now records the matching review
  through the same path. `POST /api/v1/sre/autonomy/evaluate` returns
  `{created, not_proposed}`; each not-proposed pair has `reason` (`evidence_not_met`,
  `pending`, `at_cap`), `target`, the pending proposal id, and its unmet checks.
- Every unmet check (in `GET /sre/autonomy`, `/evaluate` and `EvidenceNotMetError`) carries
  `how` (a plain instruction with counts) and, where the rule implies one, `eta`
  (`internal/earned/guidance.go`), e.g. "Review 2 more concluded or inconclusive
  lock_blocking investigations in Cases (1 of 3 in the last 4 h)."
- MCP: `sre_review_investigation` and `sre_evaluate_autonomy` (operator or admin, strict
  schemas, arguments of other tools refused). There is still no approval tool.
- Web: Accept diagnosis / Reject diagnosis with note and actual root cause on every finished
  investigation in Cases (`web/src/pages/cases/InvestigationReview.jsx`); **Evaluate now**
  and **Path to next level** on the Earned autonomy page (`web/src/pages/autonomy/`);
  "All databases" in a fleet asks for one database instead of erroring (sre.md #26).
  `internal/api/dist` rebuilt and committed.
- Docs: `docs/configuration.md` earned-autonomy section; CHANGELOG `## Unreleased` bullet.

## Product decisions (made by the "earn trust, then autonomy" lens)

- **D1 Existing ledger rows.** A carried-over level moves to the database its
  `carried_over` event names (the database whose configuration seeded it). Every other
  existing level cannot be attributed (approvals and downgrades recorded no database), so it
  stays a deployment-wide legacy row and each database adopts it once, at its current level,
  when it binds, unless it already has its own row (event `database_scoped`). Legacy rows are
  kept for databases that bind later and are never written again. Safety: an adopted earned
  level is still capped at the gate by the adopting database's own evidence (supported
  level), so it grants nothing that database has not earned; an adopted restriction
  (an operator's downgrade) keeps holding. Legacy carried-over rows are not adopted: each
  database seeds its own carry-over from its own configuration. Legacy pending proposals are
  superseded (pg_sage re-evaluates each database). History recorded before the change (no
  database) is shown with every database's history; a database filter drops it.
- **D2 Bench stays shared, game days do not.** A bench report scores pg_sage's arms and is
  evidence for every database of the deployment. A game day runs on a clone of one database
  and counts only for that database.
- **D3 Unverified grace period: 24 h.** Post-action verification writes its verdict within
  minutes to hours; after 24 h without any verification row, none is coming. `unverifiable`
  is now "unverified", not "not recovered" (the action was not shown to fail either).
- **D4 Review and outcome stay in sync by sharing one path.** Both routes call
  `packetreview.Record`: the review first (idempotent upsert), then the outcome (append),
  so a retry after a failed outcome repeats safely. The outcome route refuses before any
  write when its outcome cannot be recorded (unknown node, confirming an inconclusive
  investigation without a node). The review route records the review and says why the
  outcome was skipped in that case (accepting an inconclusive packet is a valid review).
- **D5 MCP reviews count, approvals never.** `sre_review_investigation` is operator+ and the
  review is attributed `mcp:<actor>`; pg_sage, system actors and an unbound MCP agent are
  refused as reviewers (`reviewerAllowed`). Promotion approval stays human-only (no MCP
  tool, `humanActor` on Approve). **Coordinator:** if shadow evidence must be human-only,
  change `reviewerAllowed` to `humanActor` (one line) — the MCP tool then returns
  invalid_request.
- **D6 Evaluate explains itself.** Every applicable pair it does not propose is listed with a
  reason; a pair at its class cap is reported as `at_cap` (irreversible classes stay at L1).
  The instructions describe the spec bar (or the configured fast-elevation bar); they never
  lower it.

## Spec checks covered

- §7.3 promotion only from bench + shadow record + live evidence: now per database
  (`scope_db_test.go`, `autonomy_review_api_test.go` DatabasesHaveTheirOwnLedger).
- CHECK-40 (auto-downgrade on burn, failover, stale evidence, concurrent action): failover
  cooldown and safety regression proven per database (TestFleetFailoverCooldownIsPerDatabase,
  TestFleetLiveOutcomesAndViolationsStayInTheirDatabase); existing CHECK-40 tests pass.
- CHECK-39 (MCP role per tool): the two new tools refuse viewers and unbound callers.
- CHECK-09/26 (ids scoped to their database): a review of another database's investigation
  is not found; another database's proposal cannot be read, approved or rejected.

## Test Results

Two phases: all tests (Go and vitest) committed first in `7245604`, then implemented.

**Command (full suite, PG17 `pgsage-ag6` :55476, repo root mounted, after the coordinator's
load notice):** `docker run --cpus=2 ... golang:1.25 go test -p 2 -timeout 40m -count=1 -cover ./...`
**Total:** 81 packages ok, 1 failed: `internal/sre/probes` `TestRunner_DisablesJIT`, a
500 ms probe deadline exceeded under the shared VM's load (package not touched). Re-run
alone: ok (93.1%).

**Touched packages, verbose (`-v -count=1`, PG17):** earned, earned/hasource,
earned/packetreview, earned/slobudget, schema, mcp, gameday: **344 passed, 0 failed,
0 skipped** (`TestAdvisoryLock_ReleaseReportsClosedConnection` timed out once at its 2 s
deadline under load; pre-existing, untouched, passed on re-run and in every other run).
`internal/api` (847 top-level tests) and `cmd/pg_sage_sidecar` (285) ran non-verbose: ok.
No SKIP, TODO or PENDING in the verbose output.

**Cross-version (touched packages):** PG14 :55414: earned/..., schema, api, mcp, gameday
ok; cmd/pg_sage_sidecar timed out at the default 10 min while the VM was overloaded (in an
untouched meta-reconcile test), re-run with `--cpus=2 -p 2 -timeout 40m`: ok. PG18 :55418:
all touched packages ok.
**Race (`-race`, PG17, touched packages):** all ok, no data race reported.
**Lint:** `golangci-lint run ./...` from `sidecar/`: 0 issues. eslint on the changed web
files: 0 problems.
**Web:** `npm test` 49 files, **264 passed**, 0 failed; `npm run build` ok, `internal/api/dist`
rebuilt and committed.

**Coverage (touched packages, PG17):**

| Package | Coverage | Floor |
|---|---|---|
| internal/earned | 88.6% | 70% |
| internal/earned/packetreview | 100.0% | 70% |
| internal/earned/hasource | 100.0% | 50% |
| internal/earned/slobudget | 85.7% | 70% |
| internal/schema | 81.6% | 70% |
| internal/api | 76.2% | 70% |
| internal/mcp | 79.3% | 70% |
| internal/gameday | 87.6% | 70% |
| cmd/pg_sage_sidecar | 75.2% | 70% |

All touched packages meet the coverage thresholds.

### Skipped Tests
- None.

### Failures
- None in touched packages. Two load-induced timeouts in untouched tests (above), both
  passing on re-run.

### Coverage Gaps
- None below threshold.

### Bugs Found This Session
1. [BUG] earned/store*.go, cmd autonomy_wiring.go: one ledger per control database, so one
   fleet database's reviews/outcomes promoted or demoted another (P0-5). Fixed.
2. [BUG] earned/reconcile.go `classifyOutcome`: success without a verification verdict
   counted as `verified_recovery`, the L3 counter (P0-6). Fixed.
3. [BUG] `POST /sre/autonomy/evaluate` returned `{created: []}` with no reason; nothing in the
   UI or MCP recorded reviews (Phase 1.1). Fixed.
4. [BUG] The Autonomy page errored on "All databases" in a fleet (sre.md #26). Fixed.
5. [DESIGN] Re-running the earlier M7 migrations would have re-created the old one-pending
   index and the old event-type check after this migration replaced them (and failed on
   per-database data). Fixed by redefining them in place under the same names.

## Mutation tests

Each mutation was applied, the named tests run, then reverted.

| # | Mutation | Result |
|---|---|---|
| M1-M3 | shadow stats, live stats, safety record not filtered by database | killed |
| M4 | proposal read not scoped (cross-database approve) | killed |
| M5 | success without verification credited | killed |
| M6 | no 24 h grace period | killed |
| M7 | `unverifiable` counted as not_recovered | killed |
| M8 | database check disabled | killed |
| M9 | legacy carried-over rows adopted | killed |
| M10 | unbound MCP agent may review | killed |
| M11 | `acceptedNeeded` off by one | killed |
| M12 | game days not scoped | killed |
| M13 | pending pairs re-evaluated | killed |
| M14 | a downgrade supersedes every database's proposals | killed |
| M15 | limiter bound to any database | killed |
| M16 | review skips the investigation outcome | killed |
| M17 | strict outcome route not enforced | killed |
| M18 | migration leaves carried rows legacy | killed |
| M19 | outcome route skips the review | killed |
| M20 | evaluate hides its reasons | killed |
| M21 | MCP review not operator-only | killed |
| M22 | legacy (pre-scope) history hidden | survived at first, test added, killed |
| W1-W11 | web: wrong verdict, viewer sees review, root cause dropped, met checks listed, ETA hidden, evaluate for all databases or for viewers, reasons hidden, "All databases" error, panel without review, double submit | 10 killed; double submit survived (see audit) |

## Post-test audit

1. *Inputs not tested:* concurrent `AdoptLegacy` for one database from two processes
   (safe by `ON CONFLICT DO NOTHING`; events are written only for inserted rows, not
   tested); a legacy row later re-adopted by a database added months later (by design, see
   D1).
2. *Behaviour without an assertion:* the history clause that shows pre-scope
   (no-database) events had none; M22 survived. Added
   `TestHistoryShowsDeploymentWideLegacyEvents`.
3. *Assertions that pass when broken:* the double-submit web test passed with the in-flight
   ref removed (W4). The ref turned out redundant: React applies `disabled` before the next
   click is dispatched, and removing `disabled` fails the test. The ref was removed
   (`03d226d`).
4. *Fakes hiding real failures:* `packetreview` unit tests use fakes; the same path runs
   against Postgres with the real `sre.Service` in the API tests (review, outcome route,
   another database's investigation).

## What is left (for the coordinator)

- **D5:** confirm that MCP principals' reviews count as shadow evidence (currently yes,
  attributed `mcp:<actor>`). One line makes them human-only.
- Phase 1.1 parts not in this brief: Accept/Reject on *executed actions*, ChatOps review
  buttons, buttons per unmet check ("Upload bench", "Run bench on a clone"), and a signed CI
  bench report ingested at startup (`bench_present` still fails on every default install
  until a report is uploaded or `bench_results_path` is set; the coach now says so).
- Legacy rows stay in the table forever and are never written again. A cleanup can be added
  once every database of a deployment has bound once.
- During a rolling upgrade, an older binary's writes to levels fail, because its conflict
  target no longer exists. The ledger fails closed, which is safe.
- `router.go`, `bootstrap.go` and `startMCPRuntime` were already over the size limits before
  this branch. The edits here are single lines.
