# W3-C: Ownership and memory as binding facts (roadmap 2.3)

Branch `claude/w3c-binding-facts` (from `origin/master` = v1.9.0). Agent report, 2026-10-04.

## What was built

pg_sage now keeps **typed facts** about each monitored database in one store
(`sage.facts`). Detectors and the model propose them with cited evidence; an operator
confirms or rejects each once (UI, API, MCP, Slack/Telegram card); only confirmed facts
bind, and they can only narrow or redirect what pg_sage does.

| Fact type | Subject kinds | Confirmed effect (gate route) |
|---|---|---|
| `owned_by_app_migrations` | index, table, schema (pattern) | No create/drop/alter DDL on it, also for operator approvals → `source_fix`: the finding's SQL becomes a source-fix packet (migration text + down) |
| `test_fixture` | schema pattern | Removed from the analyzer snapshot (no findings, no optimizer/LLM work, no budget spent); self-initiated actions blocked → `excluded`; one `test_fixture_cleanup` finding per fact with a reviewed `DROP SCHEMA ... CASCADE` batch the operator runs |
| `slot_consumer` | slot (pattern) | Never dropped, advanced or WAL-bounded (any request touching `slot:<name>`), operator approvals included → `alert`; a confirmed single slot is also registered in `sage.slot_consumer_registry` so the WAL custodian escalates instead of bounding/dropping |
| `append_only` | table | No row deletes, truncates, `VACUUM FULL`/bloat rewrites or index drops on it → `keep` |
| `table_window` | table | Self-initiated work held outside a `maintenance` window / inside a `batch` window → `wait_for_window` |

Every narrowing names the fact: `bound_by_fact` with detail
`fact #12: index public.idx_x is owned by the application's migrations (confirmed by alice@example.com on 2026-10-04); route: source_fix`.

### Files

Go (`sidecar/`):
- `internal/facts/` (new package): `types.go` (types, errors, Describe/Provenance/Hash),
  `pattern.go` (identifier folding, quoting, `*` globs, protected-schema refusal, LIKE
  conversion), `validate.go`, `refs.go` (SQL object extraction), `bind.go` (typed binding
  rules and routes), `binder.go` (`policy.FactBinder` + catalog index→table resolver),
  `store.go`/`store_decide.go` (dedupe upsert, declare, decide with content hash, expire,
  slot-registry sync, import of `register_consumer`/`declare_table_contract` declarations),
  `verify.go` (re-verification with absence grace), `detect.go`/`detect_fixtures.go`
  (detectors), `model.go`/`model_evidence.go` (LLM proposals from cited catalog evidence),
  `prompt.go` (bounded prompt lines, `Matching`), `sourcefix.go`, `filter.go` (analyzer
  filter + cleanup batch), `worker.go`.
- `internal/policy/facts.go` + one field in `types.go` + three lines in `gate.go`
  (`factDecision` after hard stops/validation/provider, before the operator path).
- `internal/executor/facts.go` (+ field, `Facts: executorFacts{e}` in both standing gates).
- `internal/analyzer/fact_filter.go` (+ field, two lines in `cycle.go`), exported
  `AppManagedIndexes` in `app_managed_index.go`.
- `internal/optimizer/facts.go`, `internal/advisor/facts.go`, `internal/sre/facts.go`
  (+ one line each where prompts are built).
- `internal/chatops/fact_card.go` (+ parse branches), `internal/notify/fact_card.go`
  (+ dispatcher/Slack/Telegram branches), `internal/api/fact_routes.go`,
  `internal/api/chatops_fact_decide.go`, `internal/mcp/fact_tools.go`,
  `internal/mcp/production_facts.go`.
- `internal/schema/facts_migration.go` (one registration line in `bootstrap.go`),
  retention rules in `internal/retention/rules.go`.
- `cmd/pg_sage_sidecar/database_runtime_facts.go`, `facts_mcp.go` (+ one-line hooks).

Web (`sidecar/web`, built by a sub-agent, tests first): Facts page (filters, evidence,
provenance, confirm/reject/expire, declare form), `FactBadges` inline on approval cards and
finding cases (Cases page), source-fix packet and cleanup batch rendering in case evidence;
rebuilt `internal/api/dist`.

Docs: `docs/facts.md` (+ mkdocs nav), CHANGELOG bullet under `## Unreleased`.

## Product calls (made by the "earns trust, then autonomous; facts never widen" lens)

1. **Facts bind operator approvals for ownership, CDC slots and archives**, not for test
   fixtures or windows. Rationale: the operator already told pg_sage the object is the
   app's / CDC's / an archive; an approval that contradicts a confirmed fact is more likely
   a mistake than intent. Undoing it is one recorded click (reject/expire the fact). Test
   fixtures and windows are about pg_sage's own initiative only.
2. **Bound actions are `blocked` (not parked)**, checked early, so a bound action never
   spends budget, never produces a shadow "would execute" record, and the reason is the fact.
3. **Read-only diagnostics and rollbacks of pg_sage's own changes are never bound**
   (`revert_created_index` too): undoing pg_sage's change restores what the owner had.
4. **Unreadable facts fail closed** (`facts_unavailable`) for mutations; the analyzer
   filter and prompts degrade open (findings unchanged, no facts in prompt) because the gate
   still binds.
5. **When unsure, a fact binds**: an unqualified target matches any schema, a relation of
   unknown kind matches table and index patterns. Over-narrowing is the safe direction.
6. **pg_sage never runs `DROP SCHEMA`.** The executor cannot (no allowed prefix), and a fact
   must not widen what it can do; the "single operator-approved batch" is a reviewed script
   attached to one finding per fixture fact.
7. **Operator declarations are confirmed by the declaration** (API/UI declare). Proposals
   via MCP are recorded as `source=operator`, `proposed_by=mcp:<actor>` and stay *proposed*
   until someone confirms (an MCP client may be an agent). MCP `decide_fact` is allowed for
   operator/admin principals: confirming can only narrow.
8. **A rejected fact stays rejected** when re-proposed (proposal count grows) so the model
   cannot nag; an expired fact reopens on new evidence. Re-proposals never change a
   confirmed fact's value.
9. **Expiry**: a fact whose subject names nothing for 1 h (grace for migrations recreating
   it) expires; facts re-proposed in the same pass count as freshly observed. Confirmed and
   proposed facts are retention-exempt; rejected/expired age out after `actions_days`.
10. **Fact cards go to the `approval_needed` channels** (no new notification rule needed)
    with Confirm/Reject buttons on the v1.9.0 approval-card token rules, bound to the fact's
    content hash (a stale card is refused with `content_changed`).
11. **Unification**: `register_consumer` slots and `declare_table_contract` append-only
    tables are imported as confirmed facts (never overriding an existing fact); a confirmed
    single-slot fact writes the slot registry the WAL custodian already reads. The v1.8.1
    app-managed index handling stays (it suppresses re-drops before any fact exists) and is
    now also the evidence for an `owned_by_app_migrations` proposal. No new config: facts
    only narrow, so there is nothing to switch off; the model proposer runs at most once a
    day on the existing LLM budget.
12. **Detector thresholds**: test schemas quiet ≥ 6 h (families of ≥ 2 copies proposed as one
    pattern, and only once every member is quiet); insert-only tables ≥ 1,000,000 inserts.

## lifeos read-only check (what the detectors would propose)

Run against lifeos (port 5440) with `default_transaction_read_only=on` (verified `on`),
reading only catalog/statistics views and `sage.action_log`:

| Detector | Would propose |
|---|---|
| app-managed index | **`owned_by_app_migrations` index `public.idx_thesis_allocation_run`**: dropped by pg_sage 8 times (last 2026-06-12), recreated with the same definition each time. (44 drops of 37 distinct indexes in `action_log`; only this one came back.) |
| test fixtures | nothing: lifeos now has only `public` (97 tables) and `sage`; the ~160 leaked `test_*` schemas are gone |
| slot consumer | nothing: no replication slots |
| append-only | nothing at the 1 M-insert threshold (2 insert-only tables above 10 k inserts) |
| model | would see 1 schema summary (`public`) and the app-managed evidence; no new facts expected |

Once confirmed, that fact turns any pg_sage DDL on the index into a source-fix packet and
the gate blocks it with the fact named; declared at table or schema level (`public`, if the
app's migrations own the whole schema) it would have prevented the fight before the first
drop.

## Test Results

**Command:** `go test -count=1 -p 2 -cover -v ./...` (Docker `golang:1.25`, own PG17 `pgsage-ag7`
:55477, repo root mounted)
**Total:** 12,500 passed, 0 failed, 21 skipped (96 packages ok)
**Coverage (touched packages, PG17):**

| Package | Coverage | PG14 | PG18 |
|---|---|---|---|
| internal/facts (new) | 86.1% | ok 86.1% | ok 86.1% |
| internal/policy | 91.0% | ok | ok |
| internal/executor | 87.4% | ok | ok |
| internal/analyzer | 91.5% | ok | ok |
| internal/optimizer | 92.4% | | |
| internal/advisor | 79.7% | | |
| internal/sre | 87.7% | | |
| internal/chatops | 89.0% | ok | ok |
| internal/notify | 90.7% | ok | ok |
| internal/mcp | 79.3% | ok | ok |
| internal/schema | 84.4% | ok | ok |
| internal/retention | 87.3% | ok | ok |
| internal/api | 79.1% | | |
| cmd/pg_sage_sidecar | 80.4% | | |

Also: `go test -tags=e2e -count=1 -timeout 900s ./e2e/` ok (189 s);
`PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$' ./cmd/pg_sage_sidecar`
ok (172 s); `-race` on facts, policy, chatops, notify, mcp, optimizer, advisor ok;
`golangci-lint run ./...` 0 issues (after fixing three De Morgan findings in new code);
web `npm test` 71 files, 393 passed, 0 failed, 0 skipped; `npm run lint` clean; dist rebuilt.

### Skipped Tests (must be zero or justified)
None in touched packages. The 21 skips are pre-existing and environment-gated: live cloud
provisioning (AWS RDS, Cloud SQL, Lakebase, AgentDB gauntlets: 6), Azure live parameters (2),
live LLM providers (`PG_SAGE_LIVE_LLM`, 3), container failover/restart fixtures (3),
PgBouncer fixtures (3), a Windows-only path test, the RCA child-process fixture, the plan
fixture generator and `TestPGIncidentBench` (opt-in bench). The facts slot-detector test
would skip without `wal_level=logical`; it ran on PG17, PG14 and PG18.

### Failures
None in the final runs. One earlier run of `internal/sre`
`TestDurability_OutageBlocksHandoffUntilVerified` failed with a store-ping deadline while
other agents loaded the Docker VM; it passed 3/3 on rerun and in the full suite.

### Coverage Gaps (packages below threshold)
All packages meet coverage thresholds (lowest: api 79.1%, mcp 79.3%, advisor 79.7%).

### Bugs Found This Session
1. [BUG, test] `facts/model_test.go`: the timeout fake never drained the request body, so
   `httptest.Server.Close` hung for 10 minutes (fixed in the test, explained in its commit).
2. [BUG, test] never-widen property asserted that bindings block read-only/rollback
   requests, which are exempt by design (assertion narrowed; never-widen check unchanged).
3. [BUG] `verify.go`: the schema existence query took one parameter but was passed two
   (caught by review before the first run; fixed).
4. [LINT] three De Morgan simplifications in new identifier helpers.

### Manual Checks Remaining
- CHECK-M1: MANUAL: Slack/Telegram fact cards against a real workspace (covered by signed
  callback tests with fake providers).
- CHECK-M2: MANUAL: Facts page and case badges in a browser (covered by vitest).

## Post-test audit

- **Untested inputs**: multi-statement SQL in one request (the reader names the objects of
  the leading statement only; the targets still match);
  `CREATE INDEX` on a partition child whose parent is app-owned (child matched by its own
  name only); non-ASCII unquoted identifiers (accepted, folded with `strings.ToLower`, which
  matches PostgreSQL only for ASCII).
- **Assertions that could pass when broken**: the 429 test asserts an error and no proposals,
  not the exact error class (the llm client turns a 429 retry into a context deadline when
  the caller's deadline is shorter than the backoff); the fake-chatter test asserts that
  the 429 error propagates unwrapped-to-`errors.Is`.
- **Fakes hiding real failures**: binder unit tests use a fake catalog resolver; covered by
  the live `TestCatalogResolverFindsIndexParents` (quoted, mixed-case names) and the
  executor gate tests use a stub binder; the live store/binder path is exercised in
  `verify_db_test.go`.
- **Mutation testing** (each mutation applied, tests run, then restored), all killed:
  gate ignores bindings; a binding returns execute (widening); gate skips operator
  approvals; binder error passes; binding ignores fact status; fixtures bind operator
  approvals; protected-schema check removed; app-migration facts ignore the DDL class;
  table patterns ignore an index's parent; the executor drops its binder; window logic
  inverted (11/11). The web sub-agent ran 13 + 7 UI mutations, all killed.

## What is left / open questions

1. `Findings.jsx` (the old finding detail view, which also got the fact sections) is not
   routed in `App.jsx`; findings are shown through the Cases page, which now has the badges
   and sections. Schema-lint and migration cases get no badges (their identity key does not
   name a database object).
2. Pre-existing over-limit functions I added a line or two to (not refactored, shared
   files): `Advisor.Analyze`, `FormatPrompt`, `mcp.Server.callTool`, `purgeRules`,
   `startMCPRuntime`; `bootstrap.go` is over 500 lines.
3. Should a confirmed `owned_by_app_migrations` fact also open a PR (Phase 3 source-fix
   packets through MCP)? Today the packet is attached to the finding and served by the API.
4. Should the WAL custodian *unregister* a slot when its fact is rejected? Today the
   registry is left as is (conservative).
5. Flaky under host load: `internal/sre TestDurability_OutageBlocksHandoffUntilVerified`
   failed once (store ping deadline at 8 s while other agents loaded the Docker VM); passed
   3/3 on rerun. Unrelated to this change.
