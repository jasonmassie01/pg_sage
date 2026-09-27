# pg_sage — Master Review, Improvement Plan & AI SRE Spec (2026-09-26)

**Baseline:** v1.5.0, `master` @ `b396595`. **Branch:** `claude/full-review-ai-sre-2026-09-26`.
**Inputs:** 10 Claude feature-group reviews (`group-01..10`), 3 Claude research reports
(`research-ai-sre-*`), and Codex's parallel audit (`codex/`, integrated in §8).
**Status legend:** ✅ fixed on this branch · 🟡 partially fixed · ⏳ deferred (with reason) ·
❌ not a bug. Fix status is filled in §4 after the fix phase.

---

## 0. Executive summary

1. **pg_sage has a lot of real, working machinery, and an equally large layer of
   "almost".** Collection, rules, LLM advisors, typed action contracts, a standing-policy
   gate, durable index verification, AgentDB live runners, MCP and a React UI are all real.
   But the review found **~300 defects** (Claude 294 + Codex 48, heavily overlapping) and
   **~200 unreachable/half-wired items**. The common pattern is a well-tested package whose
   production caller or data contract doesn't line up with it: tests construct the
   intermediate struct by hand, so the seam is never exercised.
2. **19 distinct P0s** (unsafe mutation of a monitored DB, cloud cost/ownership loss, or a
   security hole). Themes: the standing-policy gate silently dropped protections
   (approval guardrail spelling, tier/ramp/window ignored); side paths that bypass the gate
   entirely (retention DELETE, `/explain`, AgentDB mutations, rollback-by-wrong-id); and
   LLM output trusted where it must be validated (memory units, `drop_ddl`, `IF NOT EXISTS`).
3. **Default installs are safe; autonomous installs are not yet.** With the defaults
   (`trust.level: observation`, MCP stdio, OAuth off, retention contracts undeclared,
   migration LLM off) most P0s are latent. They fire once an operator turns on
   `autonomous` + `auto`, declares a table contract, exposes HTTP MCP, or uses AgentDB live
   mode. That's exactly the path the product is selling, so **do not expand autonomous
   authority until the Wave-0 fixes in §4 are merged and pass their regression tests.**
4. **Recommended new feature: *Sage SRE*.** A Postgres-deep AI SRE that
   investigates incidents with a fixed catalog of read-only probes, tests competing
   hypotheses against a hand-built Postgres causal graph, abstains honestly, hands actions
   to the existing policy gate, and verifies recovery independently. Autonomy is earned
   **per incident family** from a public replay benchmark. It is also exposed over MCP so
   the horizontal AI SREs (Datadog Bits, Azure SRE Agent, AWS DevOps Agent, PagerDuty) can
   call pg_sage as their Postgres specialist. See §6.
5. **Market timing is favorable.** The open-source Postgres SRE agent (Xata Agent) was
   archived 2026-06-15. No Postgres-specific AI SRE both diagnoses and acts. Horizontal
   agents only act with generic moves (restart/scale/rollback/PR). The best independent
   benchmark (DBA-Bench, Jul 2026) shows frontier agents at **17.9% Safe Pass vs 93.4% for
   a human DBA**. A product that is honest about that gap and closes it family by family
   with measured evidence is differentiated.

---

## 1. The enriched brief (what you asked + what you should have asked)

**Asked:** review every feature group → no bugs / dead paths / half-wired features →
improve every feature → deep-research and spec an AI SRE feature → write it down →
integrate Codex → fix the bugs.

**Added by the meta-prompt** (see `00-review-brief.md`):
- Ground truth over docs: the June REVERSE_SPEC was re-verified; several of its "unwired"
  claims are now stale (HA safe mode wired, FleetBudget constructed, notify senders
  registered, optimizer ConfidenceThreshold consumed). Others remain true.
- Objective dead-code signal (`deadcode`: 245 prod-unreachable funcs, 150 excluding test
  helpers) + manual hunt for *unwired ≠ unreachable* (config keys with no consumer, tables
  written but never read, UI pages not routed, endpoints with no caller).
- Proactive hunt for pg_sage's known failure classes (default masking, fenced LLM JSON,
  tx-scope, fleet leaks, threshold boundaries, e-stop bypass, unbounded growth).
- Evaluation as a first-class requirement for the AI SRE ("how do we know it's right?").

**Questions you aren't asking (consolidated; per-group lists in each file):**
1. *Why does every mode build its own runtime?* Standalone, YAML-fleet, meta-db and AgentDB
   each wire components separately in `cmd/` (main.go is 2,838 lines). That is the root cause
   of ~25 findings (retention, alerting, WAL checks, notification pools, budgets missing
   in some modes). One `DatabaseRuntime` constructor is the highest-leverage refactor.
2. *Should pg_sage ever connect as superuser?* Nearly every blast-radius finding is amplified
   by it. A least-privilege role plus a separate DDL role would cap the damage of any future bug.
3. *What counts as "done"?* Package tests passed while seams were broken (cache units, field
   names `mean_exec_time_ms`, payload shapes). Add **composed contract tests** that run
   collector SQL → snapshot → rule → finding → policy → action → verify → API → UI.
4. *Where is the source of truth?* action_log vs action_queue vs decision vs verification vs
   value vs incidents vs cases, split across target and meta DBs. The UI invents weaker
   definitions ("verified" = SQL returned). Define one immutable chain per action.
5. *Is `sage.*` in the monitored DB acceptable?* ~1–2 MB/min of snapshots, never pruned in
   fleet mode, written into customer databases. Consider meta-DB-only storage.
6. *Who may delegate authority?* Human roles, standing policy, agent tokens and tenant scope
   are separate checks that are currently conflated (MCP, AgentDB).
7. *Is the frozen C extension a liability?* It ships known SQL injection, is still built by
   the default compose file, and causes the "default mode = extension" quick-start trap.
   Recommendation: delete from master (tag an archive).
8. *Are you measuring value honestly?* "Toil minutes saved" and "incidents avoided" are
   models or counterfactuals; the Value page currently can 500 and credits unverified work.

---

## 2. Method & evidence

| Evidence | Result |
|---|---|
| `go build`, `go vet` | clean |
| `staticcheck` (prod code) | clean (24 test-only nits) |
| `deadcode` (prod-unreachable) | 245 funcs (150 excl. `testsupport`/`testdb`) → classified per group |
| Baseline unit (PG17 fixture) | 7,144 pass / 1 fail (clock-skew + order-dependent test) / 11 skip |
| Baseline integration | 7,292 pass / 0 fail / 11 skip (cloud-live + pg_hint_plan) |
| Lead re-verification | Every P0 and headline P1 re-read by the lead before inclusion (marked *LV*) |
| Codex | 48 findings, 3 reproduced with probes; cross-review of its own P0/P1s |

Coverage below the CLAUDE.md floors at baseline: api 40%, auth 48%, autoexplain 30%,
autonomy 33%, collector 10%, executor 48%, ha 33%, ledger 47%, migration/runtime 29%,
policy 57%, querystore 12%, retention 17%, schema 7%, schema/lint 23%, startup 17%,
store 28%, value 24%, cmd/pg_sage_sidecar 43%. (Low numbers in DB-backed packages are partly
because most of their tests are integration-tagged.)

---

## 3. Unified defect register

### 3.1 P0 — unsafe mutation / cost-ownership loss / security (19)

| # | Finding | Sources | Area | Enabling condition | Fix |
|---|---|---|---|---|---|
| P0-01 | `/explain` parameterized path runs `PREPARE … AS <input>` over the simple protocol, so a multi-statement body leaves the READ ONLY tx (*LV*) | G6-B01, G1-B01 | api/explain | operator session | ✅ §10.1 |
| P0-02 | Retention enforcer deletes by `ctid` only: a partitioned parent deletes rows in *other* partitions, incl. future rows (Codex reproduced DELETE 2 for LIMIT 1) (*LV*) | Codex R01, G4-B04 | autonomy | declared table contract + retention class allowed | ✅ §10.1 |
| P0-03 | Retention DELETE bypasses the policy gate: emergency stop, executor disabled, observation, manual mode, window, timeouts; dry-run evidence not bound to column/window/relation | Codex R02/R03, G4-B04, G2-B12 | autonomy | same | ✅ §10.1 |
| P0-04 | Action contracts spell the guardrail "approval required"; the gate matches `approval_required` → drop index, cancel backend, reindex, hints etc. run without approval (*LV*) | G4-B01 | executor | trust autonomous + auto | ✅ §10.1 |
| P0-05 | Standing gate ignores `tier3_*` flags, the 8/31-day ramp and `trust.maintenance_window`; default window `always` | G4-B02 | policy | autonomous | ✅ §10.1 |
| P0-06 | Autonomous `pg_cancel_backend(pid)` from a previous cycle, with no identity recheck; freeze custodian targets the oldest xact *cluster-wide* (pg_dump, walsender) every minute | G4-B03 | executor/custodian | autonomous | ✅ §10.1 |
| P0-07 | WAL custodian sets `max_slot_wal_keep_size` to the threshold the slot already exceeds → slot invalidated at next checkpoint (breaks CDC/replica) | G4-B05 | autonomy | slot > threshold + autonomous | ✅ §10.1 |
| P0-08 | DROP INDEX rollback is plain `CREATE INDEX` (blocks writes) with no lock_timeout; approved-path auto-rollback skips authorization | G4-B06, G4-B11 | executor | rollback fires | ✅ §10.1 |
| P0-09 | SQL whitelist bypass: multi-subcommand `ALTER TABLE … SET(…), DROP COLUMN` passes; double-space `VACUUM  FULL` classified safe | G4-B07 | executor | LLM advisor emits it | ✅ §10.1 |
| P0-10 | Verified-index revert drops by LLM-authored name: `CREATE INDEX IF NOT EXISTS` on a name collision → "no gain" → drops the user's pre-existing index | G4-B08, G3-B05 | executor/optimizer | LLM name collision | ✅ §10.1 |
| P0-11 | Advisor validator reads unitless memory GUCs as **bytes**; PG uses kB/8kB/MB → ~1000× oversized `work_mem` passes into autonomous `ALTER SYSTEM` (*LV*) | G3-B01 | advisor | LLM emits unitless value | ✅ §10.1 |
| P0-12 | Migration advisor sends unclassified DDL verbatim to the LLM, incl. `ALTER ROLE … PASSWORD`, user-mapping and subscription connection strings | G7-B01 | migration | migration + LLM + log_statement=ddl | ✅ §10.1 |
| P0-13 | Actions UI shows "Roll Back" on *queued* proposals; the server resolves the queue id against `action_log` and runs an unrelated action's rollback SQL (*LV*) | G9-B01, G6-B05 | api/web | operator click | ✅ §10.1 |
| P0-14 | Fleet emergency stop aborts at the first DB whose flag can't be written; the rest (random order) keep executing; any agent DB triggers it | G5-B01 | fleet | fleet/meta mode | ✅ §10.1 |
| P0-15 | AgentDB ping token (unauthenticated route) can set any deployment status → hides a live cloud DB from list/TTL/destroy; heartbeats un-archive and clear budget state | G8-B01 | agentdb | agent token | ✅ §10.1 |
| P0-16 | Archived deployments are never revisited by TTL; restore-required default blocks destroy → running, billing forever; one provider error drops the batch | G8-B02 | agentdb | any expiry | ✅ §10.1 |
| P0-17 | Re-register with an existing `deployment_id` overwrites the row and wipes provider resource identity → orphaned live instance; can move tenant | G8-B03 | agentdb | retry / re-provision | ✅ §10.1 |
| P0-18 | Live destroy falls back to a derived name `pgsage-<id>` with no ownership/tag check → can destroy another tenant's or install's instance | G8-B04 | agentdb | missing provider_resource_id | ✅ §10.1 |
| P0-19 | No tenant isolation (tenant from request body); ambiguous create marked `failed` and never reconciled → orphan/duplicate billed resources | G8-B05, G8-B06 | agentdb | multi-tenant / provider error | ✅ §10.1 |

### 3.2 P1 — silently broken features / wrong answers (headline set)

Grouped by theme; full rows with file:line are in the group files. Dedupe mapping to Codex IDs
in §8.

**Safety & authority (executor/policy/API)**
- Gate usage (rate limit, blast radius, storage budget) never enforced; deadline override skips
  trust/mode/tier; change class defaults to `index` for every action (G4-B15/17/18).
- Rollback cooldown keyed on a finding id that changes → rolled-back actions return; monitor
  overwrites operator rollback with `success`; GUC rollback never reloads (G4-B10/12/13).
- Custodians ignore replica/failover safe mode; no backoff; unlocked shared map →
  `concurrent map writes` crash (G4-B14/19/20).
- Verification looks up new indexes by unqualified name via search_path → drops/recreates
  forever outside `public` (G4-B09). Missing evidence = success (Codex C14). Verdict persisted
  before revert effect (Codex C15).
- Manual DDL bound to the 30 s HTTP deadline → INVALID index, no action_log row (G6-B03).
- HTTP MCP without role check (SURF-01/G6-B02); OIDC links by unverified email (SURF-02/G6-B04).

**Findings lifecycle & detection truth (analyzer/collector)**
- Suppression resurrected every cycle (G2-B01 = C02, Codex reproduced).
- Last finding in a category never resolves (G2-B02 = C03).
- Cache-hit unit mismatch: rule never fires; RCA same (G2-B04 = G1-B08 = C01, reproduced).
- `xid_wraparound` finding filtered as self-monitoring → never persisted (G2-B03).
- `query_regression` reads `mean_exec_time_ms`; collector writes `mean_exec_time` → never fires (G1-B06).
- pg_stat_statements not aggregated per queryid → duplicate query_store samples, unreliable
  verify windows (G1-B05, R10).
- Real PostgreSQL jsonlog lines all dropped; csvlog multi-line records dropped (G1-B02/03).
- Critical findings re-paged every cycle (G2-B06 = G7-B07).
- Only one table ever gets autovacuum tuning; unused-index timer never reset; FK-index rule
  guesses schema; in-progress CIC flagged invalid; lint suggests dropping UNIQUE/PK (G2-B05/07/08/09/10).
- New forward SQL keeps the old inverse (C04, reproduced).

**Runtime parity (cmd/)**
- Retention not run in fleet/meta modes → `sage.*` grows forever inside customer DBs
  (C08 = G1-B04 = G5-B08).
- WAL/plan-time prerequisite checks only in standalone (G1-B07 = G5-B13).
- LLM reconfigure reaches only the shared client, so "disable LLM" doesn't stop egress
  (G3-B02 = G5-B04). Runtime-added DBs get no LLM budget (G3-B03 = G5-B06).
- Notification rules/policy edits written to control DB but read from each monitored DB
  (G7-B05, G5-B10/B11). Alerting never built in fleet (G7-B09).
- Default mode `extension` → documented quick start starts only the API (G10-B01).
- Legacy-encrypted passwords undecryptable after upgrade (G5).

**LLM features**
- Daily briefing fires on ~36/365 days (G3-B04). HypoPG "no gain" doesn't lower confidence
  (G3-B06). DB content/PII reaches the LLM unredacted in tuner/RCA/narrator/explain (G3-B07).
  shared_buffers not checked against RAM (G3-B08). Retired hints stay installed (C11).

**RCA / incidents**
- Manual resolve overwritten next cycle; no hydration after restart; reason dropped (R04, SURF-19).
- Tier-2 LLM correlation can never fire in production; incidents never notify; self-action
  correlation family names mismatch (substrate report).

**Notify / migration**
- PG regex `\b` is backspace → live DDL polling never matches (G7-B02, *LV*). Risk formula
  hides most rules (B03). Volatile defaults marked safe (B04). Email default port/TLS
  mismatch (B08). Default rule can't fire (B06).

**AgentDB (14 P1s)**: region bypass, cost cap bypass, static GCP token, approve overrides
deny, self-attested restore gate, UI live actions always 400, `secret_ref` dropped, provider
readiness always "missing", `Ensure` re-runs DDL with exclusive locks on every call incl.
unauthenticated ping, local provisioning on monitored DB, no e-stop, shared Supabase
password, approved Terraform ≠ what runner creates (G8-B07..B20).

**UI**: token banner never renders, "Restart now" always 415, action history mislabels
queued/verified (G9-B02/03/04). Cases replaced the only suppress/resolve/execute UIs (SURF-10).

### 3.3 P2/P3 counts by group

| Group | P0 | P1 | P2 | P3 | Dead/unwired |
|---|---|---|---|---|---|
| G1 Collection | 1 (dup) | 9 | 18 | 9 | 14 |
| G2 Analysis | 0 | 12 | 10 | 6 | 18 |
| G3 LLM | 1 | 7 | 13 | 7 | 24 |
| G4 Executor/safety | 8 | 15 | 11 | 5 | 20 |
| G5 Runtime/config | 1 | 14 | ~10 | ~5 | 18 |
| G6 API/auth/MCP | 1 | 4 | 5 | 2 | 7 |
| G7 Notify/migration | 1 | 8 | 19 | 10 | 17 |
| G8 AgentDB | 6 | 14 | 9 | 1 | 13 |
| G9 Web | 1 | 3 | 12 | 14 | 16 |
| G10 Tests/CI/docs | 0 | 2 | 10 | 10 | 8 |
| Codex (unique after dedupe) | — | C07, C11, C15, R06-R08 | C09, C10, C12, C13, C17, R05, R09, SURF-05..08, 13, 14 | SURF-18, 20 | — |

---

## 4. Fix plan & status (this branch)

Fixes were executed in 9 parallel, package-partitioned worktrees (`fix/2026-09-26-<area>`),
each following the CLAUDE.md two-phase process (failing regression tests committed first,
then the fix), then merged into this branch by the lead. Per-area reports are in
`fixes-<area>.md`. The consolidated status table and final test report are in
**§10 (Fix results)**.

| Wave | Scope | Areas |
|---|---|---|
| **0 — authority & data safety** | All 19 P0s + authority P1s (MCP role, OIDC, gate usage, change class, manual ctx, verify tri-state, revert durability) | executor, api-web, llm, notify-migration, agentdb, runtime |
| **1 — identity & truth** | Suppression, resolution, cache units, rollback_sql refresh, optimizer identity, queryid precision, verified labels, incident durability, log parsers | analysis, collection, rca, api-web |
| **2 — runtime parity** | Retention/WAL checks/alerting/notify/policy pools/LLM reconfigure & budgets in every mode; default mode; example config | runtime, collection |
| **3 — cheap P2s** | Per area as listed in each fixes file | all |
| **Deferred** | Structural refactors (single runtime constructor, single `Executor.Apply`, AST SQL validation, notify/alerting convergence, AgentDB principals); designs in §5 | — |

---

## 5. Making every existing feature better

Ranked (impact × effort) and deduplicated across Claude and Codex. Full rationale lives in the
group files (`G*-I*`) and Codex matrices (`core-audit.md`, `surface-audit.md`).

### 5.1 Cross-cutting (do these first; each removes a *class* of bugs)
1. **One `DatabaseRuntime` constructor** for standalone / YAML-fleet / meta-db / AgentDB
   (G5-I07, Codex runtime §maintainability). It kills the parity drift behind ~25 findings.
2. **One `Executor.Apply(ctx, ActionIntent)` pipeline**: authorize → lease → re-authorize →
   semaphore → exec with mandatory timeouts → log → durable verify (G4-I07). Five pipelines
   exist today (RunCycle, manual, custodian, verified-index, retention).
3. **The gate is the only authority** (G4-I01): move ramp/tier/window/provider support into
   `policy.Gate`, delete the legacy engine, and expose a "policy explain" API for the UI.
4. **AST-based SQL validation** with `pg_query_go`, shared by the validator and the classifier
   (G4-I05, G7-I12). Regex classification is the root of the whitelist and migration misses.
5. **Durable recommendation state machine**, separate from findings/notifications:
   proposed → approved → applying → applied → verifying → verified/reverted/inconclusive, with
   forward+inverse+evidence+policy version versioned atomically (Codex core, C04/C07/C15).
6. **Composed contract tests** from collector SQL to the UI, plus Go↔UI schema contract tests
   for every mocked fixture (G9-I12, Codex surface Q2). The unit suites missed every seam bug.
7. **Retention catalog**: every append-only `sage.*` table has an owner, window, batch budget,
   and a CI tripwire fires when a table is added without one (G5-I14, Codex C08).
8. **Least-privilege connection model**: a monitoring role (pg_monitor + pg_read_all_stats),
   a separate DDL role and an `explain` role. Stop requiring superuser.
9. **Capability manifest** generated from runtime registration. It drives docs, readiness
   endpoints and "unsupported / unavailable / unconfigured / degraded" UI states (Codex).

### 5.2 Per feature group

| Feature | Top improvements |
|---|---|
| **Collection** | Per-category freshness/completeness plus a stale-data state that blocks mutation eligibility (Codex); stats-reset epoch and userid/toplevel aggregation (R10); collect `indkey/indnkeyatts/indclass/indpred` facts (G2-I05); snapshots to the meta DB only, with per-category sampling (G1-B23); pg_stat_io and pg_stat_checkpointer (PG16/17) |
| **Query store / plans** | Write `plan_hash` to get plan-change history; interval-delta regression detection instead of lifetime means (G2-I12); parameter cohorts |
| **Logs** | Real jsonlog/csvlog parsing (fixed here); durable cursor; loss/freshness metrics; per-database fanout |
| **Index rules** | Index bloat/REINDEX rule (G2-I06, still missing since June); "unused" means unused over an observed window, with coverage shown (G2-I07); one owner for FK indexes (G2-I08); portfolio reasoning (INCLUDE, predicates, opclass) |
| **Vacuum / freeze** | Per-table reloption tuning that reads existing reloptions (G2-I09); physical vs dead-tuple bloat as distinct signals (G2-I10); one wraparound owner with deadline estimation from XID burn (G2-I11, Codex) |
| **Forecaster** | Wire storage growth (C09/G2-I15); reset-aware interval rates (C10); uncertainty bands and backtest error; object identity on every forecast |
| **Schema lint** | Uniqueness-aware overlap rule; stable resolution semantics when a rule errors; wire `bloated_table` |
| **LLM layer** | LLM registry owning every client with fan-out reload and a persisted per-feature usage ledger `sage.llm_calls` (G3-I01/I04); `ChatJSON` vs `ChatText` plus `temperature: 0` (G3-I02); `llm.Untrusted()` prompt boundary with a lint test on call sites (G3-I05); prompt/response replay eval harness (G3-I08) |
| **Index optimizer** | Parse LLM DDL into a typed `IndexSpec` and regenerate DDL/DropDDL (G3-I09); HypoPG as a hard gate when present (G3-I10); **LLM-free HypoPG candidate enumeration**, so the optimizer works with the LLM off (G3-I13) |
| **Config advisor** | Validator driven by `pg_settings.unit/min_val/max_val`, plus a max-step guard (G3-I15); restart-required GUCs always advisory (G3-I16); verify applied GUCs with the durable engine (G3-I18) |
| **Query tuner / hints** | One hint state machine with installed-effect proof and a retirement executor (C11); hint-table readiness per application session (C13); validate referenced indexes/aliases (G3-I21) |
| **Executor / trust** | Everything in 5.1 #2–#5; OID-bound verification (G4-I11); reverts withheld by policy become approval items (G4-I12); async approved DDL (G4-I09) |
| **Custodians (freeze/WAL/schema)** | Evidence-matched backend signals only (G4-I08); slot-owner heartbeat and retained-WAL runway, never auto-drop; schema remediation ends in a visible terminal disposition, not a silent no-op (R07) |
| **Migration safety** | `pg_query_go` parser; rule-first scoring with intrinsic severity; `pg_proc.provolatile` for defaults; lock-queue watchdog; finish rehearsal with real affected-query replay (G7-I09..I19, R08) |
| **RCA / incidents** | Durable incident state owner (fixed here); evidence-referenced causal claims with calibrated confidence instead of a fixed 0.6 (R05); becomes the substrate of §6 |
| **Notifications** | Converge on `notify`: port throttle/escalation, add an async bounded outbox with retry and dead-letter, one control-plane dispatcher, resolve lifecycle (G7-I01..I05, SURF-15) |
| **Fleet** | Connectivity state machine with staleness-decayed health (G5-I08); fleet-level e-stop latch in the control DB (G5-I10); budget rebalance and metrics (G5-I09) |
| **Config** | Register the missing reconfigure owners for collector/analyzer/alerting intervals (G5-I01); generate the key registry from struct tags (G5-I04); versioned `sage.schema_migrations` (G5-I12) |
| **API / auth** | Route capability manifest plus generated authz tests (G6 C1); per-database RBAC; principal-bound MCP; issuer+subject OIDC |
| **Dashboard** | Split Queue vs Executed; show before/after/verification/policy per action (G9-I01/I02); actionable Cases (G9-I03); a computed "what will pg_sage do right now" panel (G9-I04); scoped e-stop in the header (G9-I05); one `apiFetch` (G9-I06); honest partial-fleet states (G9-I07) |
| **Value / ROI** | One ledger topology; observed, modeled and counterfactual value kept separate; every claim links to evidence (SURF-03/14/18) |
| **AgentDB** | Resource identity plus install-tag verification (closes 3 P0s); durable teardown state machine with blocked-teardown alerts and live burn; agent principals with tenant scope; heartbeat-only ping; commissioning gates (provisioned ≠ connected ≠ monitored ≠ backed up) (G8 C1–C4, SURF-07) |
| **Vector lab** | Persist evidence; holdout sets; cardinality sensitivity (Codex) |
| **Tests / CI / release** | `-race`, a PG 14–18 matrix and pg_hint_plan in CI; skip-budget gate; assertion lint; documented-path smoke e2e; versioned images; govulncheck/gitleaks (G10-I01..I10) |
| **Repo** | Delete the frozen C extension from master (tag an archive); remove tracked logs and personal config; publish `docs/` without internal review files |

---

## 6. New feature: **Sage SRE** (AI-driven SRE for Postgres fleets)

Full build spec: **[`AI-SRE-SPEC.md`](./AI-SRE-SPEC.md)**. Summary:

- **What:** persistent, bounded incident investigations that gather fresh evidence through a
  fixed catalog of read-only probes, generate hypotheses from a **hand-built Postgres causal
  graph** (the LLM re-ranks and picks the next probe; it never writes SQL), test competing
  hypotheses with refutation probes, abstain when evidence is insufficient, hand actions to the
  existing policy gate with a **repair contract**, and verify recovery independently.
- **R1 (read-only, Codex's Investigator):** blocking, connection pressure, WAL/replication
  retention, plus a change feed that treats pg_sage's own actions as changes, MCP read tools
  and an evidence-first postmortem draft.
- **R1.1:** approved, evidence-matched backend cancel; ChatOps approval; SLO burn-rate; signed
  change events.
- **R2:** eight more families; **pre-incident investigations** from forecaster runways
  (wraparound, disk/WAL, sequences); typed runbooks with Xata-playbook import; incident memory.
- **R3:** per-family earned autonomy (L0–L4) gated by **PGIncidentBench** (open, reproducible
  replay + Docker fault programs with decoys and noise, Safe Pass as the headline metric) plus a
  shadow record; game days on clones; fleet canarying.
- **Distribution:** an MCP "specialist" surface so Datadog Bits / Azure SRE Agent / AWS DevOps
  Agent / PagerDuty / Claude Code call pg_sage for Postgres problems, while execution stays
  behind pg_sage's gate.
- **Prerequisites:** the Wave-0/1 fixes in this branch (incident durability, identity, units,
  gate authority, e-stop latch, MCP principal), plus `plan_hash`, a 60 s lock-chain tick and
  `ChatWithTools`.

---

## 7. Dead / unwired code decisions

Verdicts from the group files, reconciled with Codex. **WIRE** = valuable, finish it.
**DELETE** = superseded/duplicate. **TEST-ONLY** = move into a test helper.

| Item | Verdict | Owner |
|---|---|---|
| All `stripToJSON`/`stripMarkdownFences`/`stripToJSONObject` copies (advisor, explain, optimizer, tuner, migration, rca) | DELETE (one shared `llm.ParseJSON`) | llm, api-web |
| `advisor.ValidateConfigRecommendation`, `parseNumericValue` | DELETE (superseded by docground) | llm |
| `optimizer/decay.go`, `DetectBloatedIndexes` | DELETE | llm |
| `optimizer/cost.go`, `detection.go` (INCLUDE/partial/matview/BRIN) | WIRE later as deterministic prompt evidence (fix 100× cost units first) | deferred |
| `llm/models.go` unreachable wrappers | DELETE | llm |
| `rollout` runtime factory/scheduler (R06) | DELETE the unwired runtime stub; keep the engine only if §R3 fleet canary proceeds | executor |
| `executor.NewRollbackMonitor`, `logAction`; duplicate `PG_CANCEL_BACKEND` case | DELETE | executor |
| `custodian/freeze.Scanner`, `wal.DefaultPolicy`, `Supervisor.TriggerSchemaGuard`, `clone.SnapshotProvider` | DELETE unless wired by the executor agent; managed snapshot clone must report "unavailable" | executor |
| `policy.ValidateContract`, `LeaseKey` | WIRE `ValidateContract` at init (G4-I03) | executor |
| `querystore.WindowedLatencyMs` | WIRE (regression baseline) | analysis/collection |
| `querystore.Prune` | DELETE once retention covers query_store | collection |
| `ha.Monitor.IsReplica`, `autoexplain.ConfigureSessionBatch`, `explain.New`, `retention.cleanStaleFirstSeen` | DELETE | collection, api-web |
| `forecaster.ForecastGrowth` / `RecordSizeHistory` (C09) | WIRE (storage runway = R2 pre-incident trigger) | deferred |
| `value.RecordIncident` (SURF-14) | WIRE from verified incident recoveries (Sage SRE R1) | deferred |
| `analyzer.DedupFindings`, `cases.ResolveIfEvidenceMissing` | DELETE | analysis |
| `schema/lint` unused/duplicate/invalid index rules | DELETE after porting the stats-reset guard | analysis |
| `schema/lint` `bloated_table` | WIRE (physical bloat) | analysis |
| `config` watcher wrappers + `applyHotReload`, `NewConfigController`, `schema.EnsureDatabasesTable`, `fleet.BuildActionFamilyReadiness` | DELETE | runtime |
| `crypto.DecryptWithMigration` (+ V1/V2Legacy) | WIRE (legacy passwords are undecryptable after upgrade) | runtime |
| `api.NewRouter*`, `registerConfigRoutes`, `configUpdateHandler`, `checkHost`, timeline helpers, `auth.DeleteUser/UpdateUserRole/GetUserByID/CountAdmins` | DELETE / TEST-ONLY | api-web |
| `EventBroker.Stop/SubscriberCount` | WIRE into shutdown + metric | api-web |
| `alerting.Manager.Throttle`, `Throttle.IsQuietHours/Reset` | TEST-ONLY wrappers (quiet hours work via `ShouldAlert`) | notify |
| `migration.LogDetector.ProcessLogEntry` | DELETE (log detection is wired another way) | notify |
| `agentdb` monitoring claims/schedule (SURF-08) | WIRE later (adaptive monitoring; fix starvation ordering first) | deferred |
| `agentdb.Store.DestroyProvisionLive` | DELETE (authorized path is `ExecuteDestroyProvisionLive`) | agentdb |
| `agentdb.BudgetGate` | WIRE before provisioning | agentdb |
| `agentdb.HeuristicBlueprintGenerator` | TEST-ONLY (product must fail closed without an LLM) | agentdb |
| Legacy UI pages (`Findings.jsx`, `IncidentsPage`, `ForecastsPage`, `QueryHintsPage`, `SchemaHealthPage`, `DatabaseSettingsPage`) | DELETE after their workflows exist in Cases (SURF-10) | api-web |
| `tuner` verification controls (C12) | DELETE inert keys (or implement); no silent no-op config | llm |
| Frozen C extension (`src/`, `sql/`, `include/`, root Makefile/Dockerfile, `META.json`, `pg_sage.control`) | DELETE from master (tag archive) | ✅ deleted; archive at local tag `c-extension-final`; sidecar `extension` mode removed |

---

## 8. Codex integration ledger

Codex's work was high quality. Its findings were source-traced with file:line, three were
reproduced with probes, and its own cross-review challenged its P0/P1s. **Every Codex finding was
accepted**, with the adjustments below. Its AI SRE spec is the R1 core of §6.

| Codex ID | Claude equivalent | Decision |
|---|---|---|
| R01 retention partition delete (P0) | G4-B04 (partial) | **Accepted as P0-02**. Codex found the partition mechanism; Claude found the gate bypass. Lead verified. |
| R02/R03 retention gate bypass, unbound dry run | G4-B04, G2-B12 | Accepted → P0-03 |
| R04 incident durability | substrate B-list | Accepted (P1), fixed by the rca agent |
| R05 RCA identity/provenance | substrate | Accepted (P2); calibration moves into Sage SRE |
| R06 rollout never activated | G4 lead | Accepted; decision: delete the unwired runtime stub |
| R07 schema remediation silent no-op | — (Codex only) | Accepted (P1) → executor agent |
| R08 rehearsal promotes on empty workload | G7-B22 | Accepted (P1) |
| R09 FK coverage accepts INCLUDE/partial | G2-B08 | Accepted, merged |
| R10 query evidence reset identity | G1-B05 | Accepted, merged |
| C01 cache units | G1-B08, G2-B04 | Accepted (reproduced by Codex) |
| C02 suppression resurrects | G2-B01 | Accepted (reproduced) |
| C03 last finding never resolves | G2-B02 | Accepted |
| C04 new forward SQL, old inverse | — (Codex only) | Accepted (reproduced) → analysis agent |
| C05/C06 optimizer identity/dedup | G2-B19, G3-B12/B13 | Accepted, merged |
| C07 advisor proposals stranded after first cycle | G3-B18 (related) | Accepted (P1); structural fix = durable recommendation state machine (§5.1 #5) → deferred with partial mitigation |
| C08 fleet retention | G1-B04, G5-B08 | Accepted |
| C09 storage forecast orphaned | G2 D07 | Accepted (P2) → deferred WIRE |
| C10 query-volume growth from lifetime counters | — (Codex only) | Accepted (P2) → analysis agent |
| C11 retired hints stay installed | — (Codex only) | Accepted (P1) → llm agent |
| C12 inert tuner controls | — | Accepted (P2) |
| C13 hint readiness per app session | — | Accepted (P2), deferred (needs live pg_hint_plan) |
| C14 missing evidence → success | G4-B28, G1-B26 | Accepted (P1) |
| C15 verdict before revert effect | — (Codex only) | Accepted (P1) → executor agent |
| C16 unquoted identifiers | G2-B22, G4-B23 | Accepted, merged |
| C17 cross-DB rollback metrics | G4 | Accepted (P2) |
| C18 descending sequences | G1-B35 | Accepted |
| SURF-01 MCP role | G6-B02 | Accepted (P1) |
| SURF-02 OIDC | G6-B04 | Accepted (P1, issuer-dependent) |
| SURF-03 Value attribution | G2-B11 | Accepted, **downgraded to P2** per Codex's own cross-review |
| SURF-04 provider readiness | G8-B15 | Accepted |
| SURF-05 Terraform content unused | G8-B20 (related) | Accepted (P2) |
| SURF-06 dry-run backup "verified" | G8-B11 (related) | Accepted (P2) |
| SURF-07 AgentDB monitoring partial | G8-B21 | Accepted |
| SURF-08 adaptive monitoring uncalled | G8 D | Accepted, deferred |
| SURF-09 queryid float64 | G6-B09 | Accepted |
| SURF-10 Cases removed workflows | G9 D01-D04 | Accepted (P2, high user impact) |
| SURF-11 ranking/freshness | G9-B07, G2-B24 | Accepted |
| SURF-12 success ≠ verified | G6-B10, G9-B04 | Accepted |
| SURF-13 shadow ignores policy | — | Accepted (P2) |
| SURF-14 dead ledger link / no incident credit | G9-B20 | Accepted |
| SURF-15 alert delivery watermark | G7-B18 | Accepted |
| SURF-16 login limiter | G6-B06 | Accepted |
| SURF-17 approval identities from JSON | G8-B26, G6-B07 | Accepted |
| SURF-18 counter that decreases | — | Accepted (P3) |
| SURF-19 resolution reason dropped | G6-B08 | Accepted |
| SURF-20 static GCP token | G8-B09 | Accepted |

**Codex testing pass (15:41–17:41).** Codex turned its findings into staged, failing contract
tests (`pre-remediation-validation-2026-09-26.md`, `preflight-*.md`) and documented its replay
and verification (`preflight-replay.md`, `preflight-verification.md`). On the original b396595
it measured: baseline 7,148 pass / 0 fail / 8 skip; real metadata-fleet + Chromium journey 14/14;
Linux binary smoke/fleet 38/0. Its combined unit + subprocess coverage lifts
`cmd/pg_sage_sidecar` from 43.5% to 73.8%, which shows the gap is mostly missing subprocess
instrumentation. It also added new defects: retention audit atomicity, concurrent verification
claim, stale-snapshot refresh, reset-after-regrowth, interior resets, cross-process RCA identity,
and AgentDB orphan schema. **All of them were run against this branch and now pass** (§10.2). Two
were closed by further product fixes on this branch; the rest were already fixed or needed
fixture adaptation.

**Found by Claude and not in Codex's reports** (the highest-severity ones): P0-01 `/explain`
statement escape, P0-04 approval guardrail spelling, P0-05 gate ignores tier/ramp/window, P0-06
backend cancel by stale PID, P0-07 WAL custodian invalidates slots, P0-08 non-concurrent
rollback, P0-09 whitelist bypass, P0-10 IF NOT EXISTS revert, P0-11 memory units, P0-12 DDL
secrets to LLM, P0-13 rollback by queue id, P0-14 fleet e-stop abort, P0-15..19 AgentDB. Also
jsonlog/csvlog parsing, regression field-name drift, PG regex `\b`, briefing cron, and the
default-mode quick-start trap.

**Codex spec vs Claude spec.** Adopted from Codex: the R1 scope, data model, API, state machine,
budgets, probe rules, 35 acceptance checks and staged rollout. Added by Claude: market
positioning, the hand-built causal graph, typed runbooks and repair contracts, per-family earned
autonomy with automatic downgrade, the MCP specialist surface, pre-incident runways, SLO
burn-rate, PGIncidentBench as an open benchmark with baselines, and 7 more acceptance checks.
Rejected: nothing. Where the two differed on tone (Codex: "do not expand autonomous authority
yet"), the master spec agrees and makes it explicit in §0.

---

## 9. Roadmap order (after this branch)

1. **Merge this branch** after review. It carries Wave 0–2 fixes and their regression tests.
2. **Structural safety refactors** (§5.1 #1–#5): unified runtime constructor, single
   `Executor.Apply`, gate as sole authority, AST SQL validation, durable recommendation state
   machine. These remove the *classes* of bugs found here.
3. **CI hardening** (G10-I01..I03): race, PG 14–18 matrix, pg_hint_plan, skip budget,
   documented-path smoke.
4. **Sage SRE M0–M4** (R1 GA), with PGIncidentBench published alongside.
5. **AgentDB principals + teardown state machine** before advertising multi-tenant AgentDB.
6. **Sage SRE R1.1 → R2**, pulling pre-incident runways forward if you agree (§6 Q4).

---

## 10. Fix results (this branch)

**Branch:** `claude/full-review-ai-sre-2026-09-26` (local only, not pushed). 12 fix branches
merged: executor, analysis, collection, llm, rca, api-web, notify-migration, agentdb, runtime,
contracts-exec, contracts-evidence, integration. Each followed the two-phase process
(failing regression tests committed first). Per-area detail with commit hashes and test names
is in `fixes-<area>.md`.

**Tally across the fix reports:** ≈ 290 items FIXED, ≈ 25 PARTIAL, ≈ 50 DEFERRED (with
reasons), 2 NOT A BUG. The fix phase itself found **≈ 40 additional bugs** (listed under "Bugs
Found This Session" in each report). Among them: explain's prepared-statement leak, the
autoexplain pool deadlock, the optimizer client ignoring `llm.enabled=false`, verify
comparing only first/last samples, retention delete and audit not being atomic, two workers
reverting the same watch, stale snapshots re-analyzed, the `+Inf` planning ratio, the
briefing double-fire, and the standalone DSN registered as `localhost`.

### 10.1 P0 status

| # | Status | Where |
|---|---|---|
| P0-01 explain statement escape | ✅ | api-web `5489570` (PREPARE via `PgConn().ExecParams`; single read-statement allowlist) |
| P0-02 retention cross-partition delete | ✅ | executor + contracts-exec: `(tableoid, ctid)` join, cutoff re-checked in DELETE, strict batch bound, delete and audit in one tx |
| P0-03 retention gate bypass / unbound dry run | ✅ (column choice partial) | routed through the gate as a moderate action; dry run bound to relation+column+window and aged 24 h–7 d. An explicit owner-chosen retention column needs a schema change (deferred) |
| P0-04 approval guardrail spelling | ✅ | executor `isApprovalRequiredGuardrail` |
| P0-05 gate ignores tier3/ramp/window | ✅ | executor/policy; with default config moderate actions never auto-run |
| P0-06 stale-PID backend cancel | ✅ | pid + backend_start + query identity rechecked; lock-chain findings now record backend identity (analysis) |
| P0-07 WAL custodian invalidates slots | ✅ | keep-size never below retained WAL + headroom; otherwise refuse + escalate |
| P0-08 non-concurrent index rollback | ✅ | `CREATE INDEX CONCURRENTLY` with lock_timeout; approved-path rollback authorizes |
| P0-09 whitelist bypass | ✅ | single ALTER subcommand; whitespace-normalized classification |
| P0-10 IF NOT EXISTS revert drops user index | ✅ | `revert_created_index` only drops a recorded OID; LLM `drop_ddl` ignored and rollback synthesized from parsed DDL (llm) |
| P0-11 memory GUC units | ✅ | llm: base units per GUC; table test over every documented GUC |
| P0-12 DDL secrets to LLM | ✅ | notify-migration: literals redacted; role/user-mapping/subscription DDL never sent; unclassified DDL not sent |
| P0-13 rollback by queue id | ✅ | api-web: `record_kind` + unique `ledger_key`; rollback only for executed rows |
| P0-14 fleet e-stop aborts | ✅ | runtime: every DB stopped in memory first, then per-DB persistence with named failures |
| P0-15 ping mutates status | ✅ | agentdb: ping is liveness-only |
| P0-16 archived never revisited | ✅ | agentdb: reconciler revisits live archived deployments; per-row isolation; visible block reason |
| P0-17 re-register orphans resources | ✅ | agentdb: idempotent same-request; 409 for other tenant/agent |
| P0-18 destroy by derived name | ✅ | agentdb: requires recorded resource id + creation receipt; verifies AWS tags / GCP labels |
| P0-19 tenant isolation / ambiguous create | ✅ (agent path) | tenant-bound agent tokens on `/api/v1/agent-api/`; `create_uncertain` state with operation id. Human-operator tenant scoping is a product decision |

### 10.2 Codex contract tests on the fixed branch

All of Codex's staged regression tests were run against this branch. Where our fixes had
made a contract intentionally stricter, fixtures were adapted to the production path with
every behavioral assertion kept; each adaptation is explained in its commit.

| Suite | Result |
|---|---|
| Core probes (C01/C02/C04) | pass |
| Retention (partition, changed contract, audit atomicity, non-partitioned, runtime controls) | pass (after 1 real fix: atomic audit) |
| Durable revert (happy path, crash before effect, connection loss, process crash, concurrent claim) | pass (after 1 real fix: durable claim) |
| RCA persistence (restart identity, restart clear, manual resolution vs stale flush, two OS processes) | pass |
| Evidence (stale snapshot, +Inf planning, reset-after-regrowth, role/top-level identity, interior reset, HA unknown) | pass (after 3 real fixes + verify interior-reset fix) |
| Surface (MCP viewer denial, stop controls, OIDC unverified/verified) | pass, adopted permanently in `internal/api` |
| Surface AgentDB (role/live boundary, invalid registration leaves no schema, spoofed approver) | pass. `SchemaLifecycle` fixture changed because the original encoded two bugs fixed here (self-attested restore, local provisioning without opt-in). **Needs your sign-off.** |
| Diagnostics SQL artifact test | N/A (external artifact) |

### 10.3 Behaviour changes operators must know

- **Trust gate is stricter.** Safe actions need `tier3_safe` plus 8 ramp days. Moderate actions
  need `tier3_moderate`, 31 days, and both the policy window and `trust.maintenance_window`
  open; otherwise they queue. Backend signals always queue. Existing policy documents lack the
  new change classes (`backend_signal`, `query_hint`, `schema_change`), so those actions stay
  blocked until the documents are updated.
- **Approval guardrails now bind.** `alter_table`, `reindex_concurrently`, cancel/terminate
  backend and `apply_query_hint` always queue for approval. Unused, duplicate and invalid index
  drops and per-table autovacuum tuning run unattended only once moderate trust is earned.
- **Retention deletes** need a matching dry run 24 h–7 d old and go through the gate. A
  withheld delete returns a policy error.
- **Crash recovery of a pending revert** resumes after the 5-minute claim lease expires.
- **Modes:** the default is **standalone** (the quick start works, and with no DSN it targets
  localhost). `--meta-db` with no mode infers `meta`. `mode: extension` fails at startup
  because the C extension was removed. `pg_sage_info{mode}` reports `meta` where it used to
  report `extension`; the `pg_sage_mode` gauge value (0) is unchanged.
- **vectorlab** bounds idle-in-transaction time by the run budget, not the 25 ms-scale
  statement timeout.
- **LLM kill switch** now also disables the optimizer client; LLM hot reload reaches every
  client.
- **OIDC** requires `email_verified` and matches issuer+subject. Existing password accounts are
  refused at SSO login until an admin linking flow exists (deferred).
- **HTTP MCP:** viewers get read tools only; the actor is persisted.
- **Notifications:** private/metadata targets are refused unless
  `notification_policy.allow_private_targets: true` (YAML-only). Channel secrets are
  encrypted at rest. Critical alerts bypass quiet hours; others are deferred, not dropped.
  Default rule severities are per event.
- **Migrations never auto-promote** until workload capture exists (rehearsal without affected
  queries is inconclusive).
- **Metrics:** `pg_sage_toil_minutes_saved_total` (counter) became the gauge
  `pg_sage_toil_minutes_saved`, plus `pg_sage_value_metrics_up`. Per-DB LLM budget metrics added.
- **AgentDB:** agent API tokens are tenant-bound; `secret_ref` must be `env:PG_SAGE_AGENTDB_*`;
  Supabase passwords are derived per project from a master secret; local provisioning needs
  `PG_SAGE_AGENTDB_LOCAL_PROVISIONING`; new GCP token-source env vars.
- **Meta-db trust:** the global `trust.level` is a ceiling. A downgrade applies to every DB; a
  raise never escalates a DB and returns a warning.
- **Repo:** `local_monitor_config.yaml` and the fixture logs are untracked. **Back up
  `local_monitor_config.yaml` in your main checkout before pulling this branch**, or git will
  delete it. Live LLM tests need `PG_SAGE_LIVE_LLM=1`.

### 10.3a Final verification (final HEAD of this branch)

```
## Test Results

**Commands:** go build ./... ; go vet ./... ; go vet -tags=integration ./...
  go test -v -cover -count=1 ./...                      (Windows, PG17 fixture + hypopg)
  go test -v -cover -count=1 -tags=integration ./...
  go test -race -count=1 ./...                          (golang:1.25 Linux container)
  go test ./e2e -v -tags=e2e -count=1                   (golang:1.25 Linux container)
  golangci-lint v2.11.4 run ./...  (CI-pinned)          web: eslint; vitest; vite build
**Total:** unit 7,541 passed / 0 failed / 13 skipped; integration 7,695 / 0 / 13;
  race: 58 packages ok, 0 data races; e2e 73 / 0 / 13 (baseline master e2e: 73 / 0 / 13);
  lint 0 issues; web 134 tests passed, lint clean, build reproduces committed dist.
  Baseline for comparison (b396595): unit 7,144 / 1 (env) / 11; integration 7,292 / 0 / 11.
**Coverage:** every internal business package ≥ 70% (lowest: sanitize 71.4%, store 73.4/77.6,
  api 73.6/74.1, agentdb 74.0, selfmonitor 75.0). Raised from baseline: collector 9.7→85.5,
  querystore 11.9→89.1, retention 16.7→100, schema 6.9→81.5, schema/lint 23.4→77.1,
  value 24.4→91.5, executor 47.5→79.6, autonomy 33.2→80.1, api 40.1→73.6, auth 47.9→81.0.

### Skipped Tests (all justified)
- Live cloud: AWS RDS, Cloud SQL, Lakebase, 3× AgentDB gauntlet (need PG_SAGE_LIVE_* + creds)
- Live LLM: rca TestTier2Live_RealGemini (PG_SAGE_LIVE_LLM=1); e2e LLM/tuner suites (API key)
- pg_hint_plan not installed on the fixture: 4 hint_verify tests (CI gap G10-B21)
- OS: logwatch TestResolveLogDir_AbsoluteUnix on Windows
- Helper: rca TestRCAChildProcessFixture (runs only under its parent tests, which pass)

### Coverage Gaps
- cmd/pg_sage_sidecar 51.0% (baseline 43.5%): startup wiring; Codex's subprocess-instrumented
  measurement puts it at 73.8%. Utility commands meet the 50% floor. All other packages meet
  thresholds.

### Manual Checks Remaining
- MANUAL: browser pass over Cases suppress/resolve, Actions queue vs executed, AgentDB
  authorize-live + restore-drill prompts.
- MANUAL: docker compose "Restart now" returns (restart policy), meta-db trust ceiling in the UI.
- MANUAL: pg_hint_plan live checks (C11 retired-hint removal); live provider/LLM suites.
```

### 10.4 Decisions (resolved 2026-09-26)

1. **Index drops and per-table autovacuum tuning: autonomous once trust is earned.** The
   `drop_unused_index` and `set_table_autovacuum` contracts no longer carry the "approval
   required" guardrail. They run unattended only at trust `autonomous` with `tier3_moderate`,
   the 31-day ramp, and open windows; otherwise the standing gate queues them for approval
   (`TestEarnedAutonomyExecutesOnlyAfterTrustIsEarned`). The e2e checks A01/A05/A08 and
   B06/B10/B14 again assert auto-execution. `alter_table`, `reindex_concurrently`,
   cancel/terminate backend and `apply_query_hint` stay approval-guarded.
2. **Codex `SchemaLifecycle` change and contract-agent fixtures:** kept. A withheld retention
   delete returns a policy error, and crash recovery waits for the 5-minute claim lease.
3. **Meta-db global trust as a ceiling, and YAML-only `allow_private_targets`:** kept. A
   fleet-wide setting must not escalate one database, and private notification targets are an
   operator-host decision that should not be toggled from the dashboard.
4. **Frozen C extension:** deleted from master (see §7).
5. **Publishing:** the review folder moved from `docs/reviews/` to `reviews/` at the repository
   root, so `docs.yml` and mkdocs no longer publish it.

### 10.5 Deferred (with reason)

| Item | Why deferred |
|---|---|
| Unified `DatabaseRuntime` constructor; single `Executor.Apply`; AST SQL validation; notify/alerting convergence; durable recommendation state machine (C07 fully) | Structural refactors. Designs in §5.1 and the fix reports; best done with the new regression suite as a net |
| RCA/lock-chain 60 s fast path (substrate B1, G1-B13) | Design in `fixes-rca.md`; it's Sage SRE M0 |
| Explicit retention column in table contracts | Schema/API change + product decision |
| G1-B27 provider disk/WAL IO as capacity | Product decision |
| G4-B24 unify maintenance-window grammars | Would change the meaning of existing policy documents |
| G4-B17 `RefusalSet`/`LockDurationCeilingMS`/`SerializeMode` | Enforcing RefusalSet as written would ban common actions; product decision |
| SURF-03 Value ledger topology across fleets | Needs a decision on where the ledger lives |
| Human-operator tenant scoping in AgentDB; approved-request-before-cloud-register | Product decisions |
| SURF-08 adaptive monitoring worker; C09 storage growth wiring; SURF-14 incident credit | Feature work, best folded into Sage SRE (pre-incident runways, verified-recovery credit) |
| C13 hint readiness per app session | Needs a live pg_hint_plan environment |
| G9-B13 operator header e-stop control | Needs state plumbing + UX decision |
| Two concurrent sidecars can duplicate an open incident; partial pg_stat_statements resets; "keep" verdict completion before credit | Residual risks noted by the contract agents; low likelihood |
| ~11 G5 P2/P3s, `inst.DatabaseID` publish race, G7-D05/D06, lint `bloated_table` | Low value or blocked on the runtime refactor |

### 10.6 After the decisions: CI hardening and the version matrix (2026-09-26)

Roadmap §9 step 3 (G10-I01..I03) is done:

- **I02 matrix:** `integration-matrix` runs the race-enabled integration suite on PG 14, 15,
  16 and 18; the test job covers 17. `scripts/ci/setup-test-postgres.sh` preloads
  pg_hint_plan everywhere, so the hint verification suite runs in CI.
- **I03 skip budget:** `cmd/skipbudget` fails CI on any skip not justified in
  `sidecar/.skip-allowlist`.
- **I01 documented path:** `TestDocumentedQuickStart` runs `--pg-url` only and
  `SAGE_DATABASE_URL` only, as a least-privileged role created by the installation docs'
  SQL block, which the test runs verbatim.

The first matrix run found product bugs that only show on non-17 servers. All are fixed with
regression tests:

| Bug | Versions | Impact |
|---|---|---|
| HypoPG validation EXPLAINs normalized `$n` workload text without GENERIC_PLAN | PG14, PG15 | Index validation failed for almost every real workload |
| `Actual Rows` parsed as int64; PG18 emits two decimals | PG18 | EXPLAIN node breakdown empty; tuner missed every ANALYZE-plan symptom; optimizer plan summary blank |
| Hint table treated as ready without `query_id` (pg_hint_plan < 1.7) | PG14-16 | Every persisted hint INSERT failed |
| Documented setup never created `pg_stat_statements` and relied on a schema bootstrap the role cannot perform | all | The quick start as written exited at startup |
| `vectorlab` idle-in-transaction timeout = statement timeout | all | Session killed on a client pause (flaky) |

Other changes: the extension mode was removed with the C extension (`meta` label for
meta-db, `standalone` default); tests assuming PG17 were made version-aware, with
`testdb.RequireServerVersion`. The PG14 advisor fixture waits for the async stats collector
instead of skipping.

Verification (Linux container, PG17 + pg_hint_plan, as CI runs it):

| Suite | Passed | Failed | Skipped |
|---|---:|---:|---:|
| Unit, race | 7,551 | 0 | 9 |
| Integration | 7,705 | 0 | 9 |
| e2e | 73 | 0 | 13 |

There were 0 data races. The skip budget allowed all 31 skips. The per-version integration
runs reach 0 failures after the fixes, re-verified per package on PG14 and PG18.

