# pg_sage — taking it to the next level (AI-first)

2026-10-02 · review of every feature on master (v1.8.1) by six parallel reviewers, plus a week of
dogfood on a real 18 GB database (lifeos). Area reports, all with `file:line` evidence:
[tuning](tuning.md) · [sre](sre.md) · [autonomy](autonomy.md) · [platform](platform.md) ·
[interfaces](interfaces.md) · [landscape](landscape.md). Key claims were re-checked in the code by
the coordinator.

## The verdict

pg_sage already has the hard, unglamorous part that nobody else ships for Postgres: a gated execution
path (`policy.Gate` → `Executor.Apply`), typed action contracts, leases, verify-and-revert, an
evidence ledger, and an earned-autonomy model. The market scan confirms the gap it can own: every
big-vendor "AI DBA/SRE" agent (AWS, Google, Datadog, Azure) still stops at recommendations, and the
best general LLM agent safely fixes ~18% of live Postgres incidents (DBA-Bench, Jul 2026) against 93%
for a human DBA. **Safe remediation is the open problem, and safe remediation is what pg_sage is
built around.**

But today the AI is an annotator, not the driver:

- **The model reviews; it does not decide.** Sage SRE runs fixed probe lists per family; the model
  may reorder the graph's hypotheses, ask for one probe, and narrate. It cannot name a cause outside
  ~50 graph nodes, and "Investigate now" ends `no_probe_plan`. The optimizer/advisor are single-shot
  prompts with static confidence weights that never learn from outcomes.
- **Trust cannot be earned through the product.** Earning L2 needs a bench report and accepted
  reviews; reviews are API-only (no UI button, no MCP tool), "evaluate" returns `[]` with no reason,
  and the empty proposals card renders nothing. That is exactly what happened when you tried to
  promote.
- **Two trust systems that never meet.** The things that actually changed lifeos (19 index drops, a
  GUC change) ran on the old time-only trust ramp, which ignores outcomes. The evidence-based L0–L4
  ledger only governs custodians and SRE actions — and it is keyed per deployment, not per database.
- **Verification is thinner than it looks.** Drops and GUC changes are "verified" by a 15-minute
  compare of cumulative counters that barely move; LLM `ALTER SYSTEM` changes carry no rollback SQL
  and no read-back; "success without verification" counts as a verified recovery.
- **It can hurt the database it guards.** lifeos: forecaster overload (fixed in 1.8.1), a catalog it
  could not collect, 9.3 GB of its own history in the production DB, and the same 7-day-history
  pattern still live in the analyzer.

The next level is therefore not "more features". It is: **make the model the driver of one closed
loop — observe → hypothesize → act → verify → learn — inside the guardrails pg_sage already has, and
measure it well enough that autonomy expands on evidence instead of on a timer.**

## Target architecture: one Sage agent loop

Replace the per-feature pipelines (analyzer rules → optimizer prompt → advisor prompt → tuner
heuristics → SRE graph) with one runtime that every domain plugs into as a *skill*:

```
 signals ──► case ──► Sage agent (tool-calling, budgeted)
                        │  tools: catalog/probe library, pg_stat_statements top-N, EXPLAIN,
                        │         HypoPG what-if, clone/branch rehearsal, change feed, memory
                        ▼
                  typed proposal (action contract + predicted effect + cited evidence)
                        │  deterministic validators (AST, allowlists, blast radius, ownership)
                        ▼
                  policy.Gate ── earned level for (database, family, action class)
                        │                 └── below level: shadow / approval card
                        ▼
                  Executor.Apply ──► verifier (call-weighted before/after, business cycle)
                        ▼
                  outcome ledger: predicted vs observed ──► calibration, promotion/demotion,
                                                            memory, replay corpus
```

What stays deterministic (and must): the gate, AST/allowlist validation, leases, rollback,
verification math, budgets, and the human approval of promotions. What becomes model-driven: which
cases matter, which probes to run, the hypothesis (including "not in the graph"), the candidate fix,
the predicted effect, ownership/purpose classification, the explanation, and the config the system
derives for itself. The graph and the rules become the model's **prior and fallback**, not its cage.

## Phase 0 — Safety and correctness (now, ~1 week)

All verified in code; each is small. Ship as v1.8.2 before any expansion.

| # | Issue | Where | Fix |
|---|---|---|---|
| 1 | LLM config changes run with no prior-value capture, rollback SQL, read-back or outcome metric; unknown GUCs/reloptions pass (e.g. `autovacuum_enabled=false`) | advisor/docground.go:155-159, executor/apply_finding.go:318, config_apply.go | capture prior value → rollback SQL; read back `pg_settings`/`pending_restart`; GUC + reloption allowlists; restart-required list incl. `autovacuum_max_workers` |
| 2 | `/explain` ANALYZE runs inside a READ ONLY tx — volatile functions (e.g. `pg_terminate_backend`, `dblink`) still execute | explain/explain.go:289-315 | reject queries calling volatile/unsafe functions (AST + `pg_proc.provolatile`); ANALYZE only for proven-safe SELECTs; `timeout_ms=0` must not mean unlimited |
| 3 | Prompt-injection guard indexes original text with lower-cased offsets → panic / tag escape | llm/untrusted.go:56-72 | case-insensitive scan on the original string; fuzz test |
| 4 | Unqualified `DROP INDEX` skips the protected-schema check | sqlast/check_cgo.go:82 | resolve via catalog or require schema-qualified names |
| 5 | Earned-autonomy ledger keyed per deployment, not database → A's outcomes promote B | schema/sre_m7_autonomy_migration.go | add database scope to ledger, evidence and proposals |
| 6 | "Success without verification" counts as a verified recovery | earned/reconcile.go:127 | only verified outcomes earn credit |
| 7 | HypoPG errors are neutral → index admitted without the what-if gate; unweighted mean rejects real wins; expression indexes always rejected | optimizer/optimizer.go:300, hypopg.go:79-92, validate.go:242 | per-query isolation (savepoints), call-weighted improvement, real expression-index parsing; an error is "unverified", not "pass" |
| 8 | Analyzer reads 7 days of query snapshots every cycle; optimizer `COUNT(*)` over all snapshots | analyzer/analyzer_checks.go:97-133 | bounded reads (as done for the forecaster); merge the snapshot-dedupe branch |
| 9 | Default MCP stdio + 06:00 briefing to stdout corrupt the MCP stream | config defaults, briefing.go:369 | briefing never writes to stdout when MCP is on stdio |
| 10 | Clone-schema collapse treats live tenants (`tenant_000123`) as leftover copies and hides their findings | analyzer/clone_schemas.go | "leftover" only when the family is idle; live families become one fan-out finding per issue |
| 11 | Tuner hints: every scan gets a parallel hint, every seq scan an index hint, `HashJoin()` with empty alias, empty hints inserted | tuner/planscan.go:168-233, rules.go:98-107 | fix the heuristics against real plan JSON fixtures (current fixtures hide the bugs) |
| 12 | Unused-index drops ignore replicas and `last_idx_scan` | analyzer/rules_index.go | include standby usage where reachable; refuse drops when replica usage is unknown and the DB has replicas |

## Phase 1 — A trust loop you can actually use (~2 weeks)

1. **Review and promote from the UI and MCP.** Accept / Reject / "actual root cause" on every
   concluded investigation and on every executed action (writes the shadow-review and outcome
   tables). An "Evaluate now" button. Replace the empty proposals card with a **promotion coach**:
   each unmet check as an instruction with a button ("Run bench on a clone", "Review 2 more
   investigations", "ETA 3 h"). Ship a CI-signed bench report with every release and ingest it at
   startup; allow a local bench run on a clone to count for the families it covered.
2. **One trust system.** Every self-initiated action class (index create/drop, GUC, autovacuum,
   retention, hints, freeze, cancel) gets a ledger pair per database. The time ramp becomes a floor,
   not a grant. Rollbacks, regressions and operator rejections demote automatically. One **Trust**
   page shows both, per database × family × class, with the evidence.
3. **Real verification.** Call-weighted before/after deltas for the queries the action targets,
   windows that cover a business cycle for drops (soft-drop: keep the definition, re-create on
   first miss), a variance-aware test, and predicted-vs-observed recorded for every action.
4. **Shadow mode as the default way to earn trust.** Below the earned level, pg_sage records every
   action it *would* take with its predicted effect and scores it later (HypoPG/replay, what the
   human did, what happened). Thousands of scored shadow decisions instead of 3–20 hand reviews.
5. **Approval cards with the why.** Title, cited evidence, model rationale, predicted effect,
   rollback, blast radius — one click in UI, Slack and Telegram for *every* action type (today only
   SRE cancel has chat buttons), with a follow-up message carrying the verification verdict.

## Phase 2 — The model drives (~4–6 weeks)

1. **Tool-calling investigator.** The model plans probes over the catalog, may conclude when the
   graph cannot, may contest when it disagrees, and has an `unmodeled` cause with cited evidence.
   Operator-started and SLO-burn triage use a broad plan. Model-sourced roots stay at L1 until the
   bench shows lift.
2. **Tuning agent.** One case-driven agent per database replaces optimizer/advisor/tuner prompts:
   tools = top statements, EXPLAIN (safe), HypoPG what-if, clone rehearsal, write-cost model,
   extended statistics; output = typed proposal with predicted effect; learns from the outcome
   ledger (calibrated confidence instead of fixed weights). Workload classification first: app vs
   test vs tenant schemas, migration-managed objects, batch windows.
3. **Ownership and memory as binding facts.** The model proposes typed facts ("index owned by the
   app's migrations", "schemas are test fixtures", "slot belongs to CDC"); the operator confirms once;
   confirmed facts can only narrow or redirect action (route to a PR instead of DDL), never widen
   autonomy. This fixes the lifeos index fight at the root, before the first drop.
4. **Measure the model.** Retire the "model may never change a graph root" gate; score override
   precision, inconclusive-case lift and Safe Pass; nightly paced live-model arm; real plan flips and
   composites; a held-out set; every contested production investigation becomes a replay case. Run
   DBA-Bench and publish the Safe Pass number with failures.

## Phase 3 — Reach (parallel tracks)

- **MCP v2 for coding agents.** Conform to the protocol (`ping`, `content[]`, notifications), scoped
  API tokens, database argument on every tool, fix `apply_migration`'s schema. New tools: top
  queries + plans, safe EXPLAIN, HypoPG what-if, migration lint (expose the existing DDL classifier),
  mark-owned/exempt, query→source attribution (sqlcommenter/`application_name`). **Source-fix
  packets**: pg_sage hands Claude Code/Cursor a cited packet; the agent opens the PR in the app repo;
  pg_sage verifies after deploy. Docs + a one-line `claude mcp add` setup.
- **The Postgres specialist other agents call.** Stable `sre_*` investigation contract for AWS
  DevOps Agent, PagerDuty and Datadog: cited causal chain, confidence, missing evidence, and a gated
  remediation they can request but not force.
- **Ask Sage.** A conversational surface on the same agent loop and tools, answers grounded in cited
  evidence, able to open a case or a proposal — never to bypass the gate.
- **Self-configuration.** Most of the 350 knobs (85% need a restart) become derived values with a
  derivation ledger (value, evidence, bounds), shadow-tested and pinnable.
- **Managed clouds.** CloudWatch/Performance Insights + parameter groups for RDS/Aurora, Cloud
  Monitoring + flags for Cloud SQL. Unlocks autonomy that today is withheld for lack of host
  telemetry.
- **Five-minute time to value.** Collect immediately; catalog-only first look (invalid/duplicate/
  never-scanned indexes, wraparound, sequence runway, leaked schemas); read-only first, "grant more"
  later; onboarding on the landing page; docs that match the UI.
- **Cheap, self-aware observer.** Change-only catalog storage (branch ready: 10.9× less growth),
  partitioned history, history optionally outside the monitored DB, a declared CPU/IO/storage budget
  for pg_sage itself with a self-cost metric.
- **Fleet learning.** Schema fingerprints, priors from look-alike databases, fleet findings ("the
  same missing index on 30 tenants"), need-based LLM budgets, leader election.

## What to measure

| Metric | Today | Target |
|---|---|---|
| Safe Pass (DBA-Bench + PGIncidentBench, live model) | unmeasured; CI uses a fake model | published, above frontier baselines |
| Verified outcomes (predicted vs observed within tolerance) | not recorded | ≥ 80% per action class before L3 |
| Time to first useful finding | ~10 min, often empty | < 5 min, non-empty on any real DB |
| pg_sage self-cost | invisible | ≤ 1% CPU, bounded storage, reported |
| Promotion path without curl | impossible | every check actionable in the UI |
| Model lift over deterministic | not measurable | reported per family |

## Recommended next step

Do Phase 0 as v1.8.2 this week (12 small, verified fixes, tests first), together with Phase 1.1
(review buttons + promotion coach) and Phase 1.2's ledger re-scoping, because those are what block
the product promise "earns trust, then becomes autonomous" from being true in practice. Then start
Phase 2.1 and 2.4 together: the tool-calling investigator only matters if the bench can prove it.
