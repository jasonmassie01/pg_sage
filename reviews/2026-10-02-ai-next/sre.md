# Sage SRE — AI-next review (2026-10-02)

Scope: `internal/{sre,rca,runway,chatops,notify,cases,earned}`, `sre-bench`, AI-SRE-SPEC.
Paths are relative to `sidecar/` (or `internal/`) unless noted.

**Verdict:** a well-engineered deterministic expert system with an LLM *reviewer* attached.
The model cannot form a hypothesis, pick more than one probe, overrule the graph or act, and
its value over the graph has never been measured. Right for R1; not "AI first". The trust
ramp meant to graduate it is unreachable from the UI.

---

## 1. Inventory

| Feature | What it does today | Where | LLM? how | Maturity | Evidence |
|---|---|---|---|---|---|
| Coordinator / worker | Durable investigations under fenced leases; fixed per-family probe plan, then diagnose, runbook, model turn, conclude | `internal/sre/coordinator.go`, `worker.go`, `plan.go`, `plan_m6.go` | No | production | ~820 `Test*` in `internal/sre`; DB tests |
| Causal graph `causal-v4` | ~50 hand-built mechanism nodes in 13 families; hand-weighted scoring (`h.add(0.55,…)`); root = most confident supported non-amplifier | `internal/sre/causal/*.go` (graph.go:15, match.go:104) | No | solid | Replay 59/60 Safe Pass; live 30/30 (`reviews/2026-10-02-sre-m4-ga-report.md:65,90`) |
| Probe catalog | ~36 fixed read-only probes; typed results `ok/empty/error/no_privilege/unsupported`; args limited to pid, backend_start, window | `internal/sre/probes/types.go:21-43,113-117` | No | production | runner/catalog DB tests |
| Model turn | ≤2 turns, 16k in/4k out; may permute the *open* hypotheses, ask for 1 catalog probe *only when inconclusive*, write ≤5 cited claims | `model_turn.go`, `model_contract.go:200-206`, `model_prompt.go` | Yes: one `submit_review` tool (OpenAI tools or JSON fallback) | solid (narrow) | One live run: gold root ranked first 14/14, 2 probe requests (GA report :93-97) |
| Claim validator + verifier | Claims must cite in-scope, hash-intact evidence; every number must appear in cited evidence; graph wins any disagreement and *all* model output is dropped | `claims.go`, `model_verify.go:191-207` | Validates LLM | production | 108/108 and 89/89 claims verified (GA report) |
| Incident memory | Up to 3 similar past investigations of the *same database* offered to the model as fenced context; leakage-guarded | `memory.go:15-24`, `postgres_memory.go:49` | Context only | solid | memory DB tests |
| Typed runbooks | Signed DAGs: probe → decision → proposal; English playbook → LLM-compiled draft → human sign | `internal/sre/runbook/`, `runbook_run.go` | Compile only | solid | runbook tests; proposals never consumed (see §2) |
| Approved cancel | Evidence-matched `pg_cancel_backend` of one backend, approval-queued, identity recheck ≤5 s, recovery verified over fresh samples | `internal/sre/action/` (derive.go, request.go, recovery.go) | No | solid | action DB tests; no dogfood use |
| SLO / burn rate | App SLIs (Prometheus/push) + labeled proxy SLIs; multi-window burn; feeds autonomy downgrade | `internal/sre/slo/` | No | solid | unit/DB tests; no bench family |
| Change feed | sage actions, config audit, migration findings, pgss resets, restarts, role, timeline, extensions; signed external events | `changefeed/sources.go:15-26`, `api/sre_signal_handlers.go:28` | No | solid | DB tests |
| Reactive detector | Checkpoint storm / temp-file / LWLock episodes from counter deltas | `detect.go:13-38` | No | solid | bench families |
| Pre-incident runways | Wraparound, disk/WAL, sequence runway investigations + custodian proposals | `runway.go`, `internal/runway/` | No | solid | bench (compressed time); trends cap bug §2.17e |
| Earned autonomy | L0–L4 per family × class; proposes L1→L2 only with bench report + 30-day shadow of ≥20 reviews at ≥95% accepted | `internal/earned/evidence.go:104-111`, `service.go:178` | No | prototype in practice (unreachable) | dogfood: user could not promote |
| Legacy RCA + Tier-2 | Rule trees open/dedup/auto-resolve/stale-resolve incidents; Tier-2 (one plain `Chat`) writes free-text root cause + one `recommended_sql`; narration is a 2-turn tool-calling pass over in-memory evidence | `internal/rca/tier2.go:16-24,258-284`, `narrate.go`, `narrate_tools.go` | Yes: free text (Tier-2), cited narration | solid (old) | rca tests, many DB-fixture only |
| Cases | Read-time projection of findings/incidents/hints into Cases; "shadow" toil-saved report | `internal/cases/case.go`, `shadow.go` | No | solid | projector tests |
| Notify | Slack/email/PagerDuty/Telegram; rules on type + severity; no grouping/digest | `internal/notify/dispatcher.go` | No | solid | tests (PagerDuty resolve path untested) |
| ChatOps | Signed Slack/Telegram approve/deny of queued SRE cancels only | `internal/chatops`, `notify/approval_chatops.go` | No | solid | tests; identity mapping API-only |
| MCP | 16 `sre_*` tools (read, propose, request execution, runbook draft/compile, downgrade) | `internal/mcp/sre_*.go` | Caller is an LLM | solid | principal tests |
| PGIncidentBench | 75 fault scenarios (32/16/16/11 positive/noise/decoy/benign), 65 synthetic replay cases (3 families), Wilson intervals, gates | `sre-bench/` | Fake model in CI; live opt-in | solid harness, thin LLM eval | reports in `reviews/2026-10-0*` |

---

## 2. What is weak

**The model does not drive anything.**
1. *Fixed plans, not planning.* Every family runs a hard-coded probe list
   (`plan.go:42-62`, `plan_m6.go:13-34`); spec §6's "next probe with the highest expected
   discrimination" is not implemented. The only adaptive step is one model probe, offered only
   when the graph is inconclusive (`model_contract.go:433-436`); live: 2 requests in 35 calls.
2. *The model can't name a cause the graph lacks.* `checkRanking` rejects any node not
   already open (`model_contract.go:400-425`); the spec's `unmodeled` hypothesis does not
   exist. Anything outside ~50 nodes is "inconclusive" forever.
3. *Disagreement erases the model.* When the graph is conclusive and the model ranks another
   root first, its claims are discarded too (`model_verify.go:197-201`). "The graph may be
   wrong" becomes one timeline event that reaches neither review nor the bench.
4. *Open-ended investigation is broken.* `operator` is an accepted kind (`request.go:31-32`,
   `api/sre_operator_handlers.go:47-49`) with no plan, so it always fails `no_probe_plan`, as
   a test asserts (`coordinator_db_test.go:273-281`). "Investigate why it's slow" is impossible.
5. *Probe args are too narrow for a planner.* Only `pid`, `backend_start`, `window`
   (`probes/types.go:113-117`): no relation, queryid, role, application or slot argument.
6. *Hard ceilings are code constants mirrored by DB CHECKs* — 2 turns, 12 probes, 16k tokens,
   120 s (`limits.go:10-19`); a multi-step agent needs a migration, not config.

**Coverage gaps.**
7. *Families missing:* CPU/IO saturation by a query (no pgss-delta probe), bloat/vacuum
   falling behind, disk full, crash/OOM restart, auth/connection-refusal storms, log error
   storms (timeouts, deadlocks), cloud throttling (`providerobs` is not a probe). No wait-event
   histogram or `pg_stat_io` probe (`probes/types.go:21-43`).
8. *pg_sage as the cause of load.* `sage_own_action` only covers executed actions
   (`causal/graph_v2.go:71-79`). The dogfood overload by pg_sage's own forecaster query
   (`internal/forecaster/datasource.go`, since bounded) is not a modelable mechanism. The
   index re-drop fight is now caught outside SRE (`analyzer/app_managed_index.go:14-18`,
   executor oscillation guard), but investigations can't name it.
9. *SLO-burn triage only looks at lock, connection and plan* (`causal/triage.go:15-17`), so a
   burn from checkpoints, temp spill, LWLock or replication is reported as "maybe outside
   PostgreSQL".
10. *Hand-tuned weights shown as scores.* Confidence is a sum of literals (`lock.go:197-282`,
    `connection.go:213-268`), rendered as "score 0.85" (`web/src/pages/cases/InvestigationPanel.jsx:180`)
    though the spec says confidence appears only where calibrated (§9).

**Dead ends and duplicated reasoning.**
11. *Runbook action proposals go nowhere.* A runbook may end in `action` with family and
    autonomy class (`runbook_run.go:166-173`), labeled "runbook proposal (not executed)"
    (`runbook_types.go:40`); nothing in `api`, `cmd` or `sre/action` reads it. Only
    `cancel_backend` derived from lock/connection roots is actionable (`action/derive.go:76-79`).
12. *The only action misses the most common root.* Idle-in-transaction holders are explicitly
    ineligible (`action/derive.go:94-95,104-109`); `terminate_backend` exists in the runbook
    vocabulary (`runbook/proposal.go:42`) but has no SRE action path.
13. *Two diagnoses, two verdicts.* Pages carry the RCA incident's `RootCause`
    (`rca/events.go:81`), which Tier-2 fills with free LLM text plus `recommended_sql`
    (`rca/tier2.go:278-281`); the validated SRE conclusion is never notified (no
    "investigation" in `internal/notify`). Slack gets the weaker answer.
14. *Two unconnected operator feedback channels.* `POST /sre/autonomy/reviews`
    (accepted/rejected → promotion, `earned/service_decide.go:323`) and
    `POST .../investigations/{id}/outcome` (confirmed/refuted → memory only, `memory.go:261`).
    Neither has UI; neither updates graph weights or the bench.
15. *No learning across databases.* Memory is scoped to one database
    (`postgres_memory.go:49`). Fleet mode learns nothing from its siblings.
16. *No postmortem.* R1 promised an evidence-first postmortem (spec §4, CHECK-41); there is an
    export (`export.go`) but no postmortem generator anywhere (`grep -ri postmortem` → nothing).
17. *Incident plumbing bugs feeding SRE.* (a) The trigger source reads the 50 *oldest* open
    incidents before filtering by family (`sre/triggers.go:70-75`): old open incidents can
    starve new ones. (b) Auto-resolve matches signal IDs only,
    not the object (`rca/lifecycle.go:179`), so one chronic signal pins every incident that
    carries it. (c) PagerDuty never resolves: incident events set no `DedupKey`/`Resolve`
    (`notify/incident_events.go:52-81`), the key embeds the event type
    (`notify/events.go:127-137`), so "resolved" opens a second PD incident. (d) No grouping
    or digest in the dispatcher. (e) Runway trends are capped at 200 rows ordered by kind
    (`sre/probes/catalog_runway.go:225-226,248`): >~195 sequence series push WAL/XID trends out
    silently (inferred from ORDER BY/LIMIT). (f) Case projector WAL/replication predicates use
    names RCA never emits (`cases/incident_projector.go:358-365` vs `rca/signals.go:178,277`).
18. *Token budget:* `DeploymentDailyTokens = DatabaseDailyTokens = llm.token_budget_daily`
    (`cmd/pg_sage_sidecar/sre_model_wiring.go:42-43`): one noisy DB can spend the fleet's budget.

**Evaluation rigor.**
19. *The bench cannot show the LLM helps.* `M3-LLM-ROOT` fails the LLM arm if it changes any
    graph-concluded root (`sre-bench/llmgates.go:11-14`); CI uses a fake model
    (`sre-bench/fakemodel.go:11-40`; no `PG_SAGE_BENCH_LLM_*` in `.github/workflows/ci.yml`).
    The one live run had 63/64 replay calls rate-limited (GA report :106-112). Measured value is
    graph over rules-only, never model over graph.
20. *Corpus is self-authored and synthetic.* All 65 replay cases say "synthetic … not a
    customer incident"; no held-out split (README :308-311); replay covers only 3 families;
    plan scenarios insert rows into `sage.query_store` instead of causing a plan flip
    (`sre-bench/scenarios_plan.go:11-14`); no composite faults (README :317); factual-precision
    gate never evaluated (`gates.go:68-71`). Safety snapshot misses ALTER SYSTEM, DDL and kills
    of untracked sessions (`safety.go:82-105`).

**Operator UX (why "promote" did nothing).**
21. L2 requires an ingested PGIncidentBench report (`earned/evidence.go:209-219`);
    `bench_results_path` defaults to empty and upload is admin API only
    (`config/sre_autonomy.go:19`). So `bench_present` fails for every family in every default
    install — nothing is ever proposable, regardless of reviews.
22. No UI records packet reviews; nothing in `web/src` calls `/sre/autonomy/reviews`. Shadow
    count stays 0.
23. No "Promote" button by design; the pending card renders `null` when empty
    (`web/src/pages/AutonomyPage.jsx:140`). The only explanation is the per-class "Next level"
    column of unmet checks.
24. Fast elevation (4 h / 3 reviews) cannot help: bench presence/age stay at spec
    (`cmd/pg_sage_sidecar/fast_elevation.go:14-28`), and `shadow_duration` counts from the
    first review ever (`earned/evidence.go:323-332`).
25. Promotion doesn't govern what users care about: index/tuning actions carry no
    `IncidentFamily` (`executor/autonomy.go:49-57`, `policy/autonomy.go:63-67`), and the SRE
    cancel is operator-approved, so the ledger gates only custodian freeze/WAL work.
26. The Autonomy page errors on "All databases" in fleet mode (`api/autonomy_handlers.go:83-88`)
    and Runbooks needs a single database; ChatOps identity mapping is API-only
    (`api/chatops_identity_handlers.go:18-20`).

---

## 3. Iterate

| # | Change | Value | Effort | Risk | Guardrail |
|---|---|---|---|---|---|
| I1 | **Review buttons on the Cases panel**: "Accept packet / Reject (why)" + "Root was: [node ▼]", writing *both* `/autonomy/reviews` and `/outcome` in one call | Unblocks shadow evidence; one feedback concept | S | Low | Operator role; family from investigation, not caller (already) |
| I2 | **Ship a signed bench report in the release**, ingest at startup; replace the empty pending card with an "L2 needs: N more reviews, X h" checklist | Makes promotion reachable and legible | S | Low | Report signed by CI key (`internal/sre/signed`); still admin approves |
| I3 | **Notify with the SRE conclusion** (root, cited facts, operator step, review buttons); demote Tier-2 text | Operators see the validated answer where they work | M | Low | Only graph-root and verified claims; redaction as today |
| I4 | **Keep model output on disagreement**: store ranking + claims labeled "model disagrees", auto-open a review request and write a replay candidate | Turns disagreements into training/eval data | S | Low | Graph root stays authoritative for actions |
| I5 | **Operator-kind plan**: broad triage probes, and SLO-burn triage across *all* matchers | "Investigate now" works; SLO triage covers M6 families | M | Low (read-only) | 12-probe ceiling, background budget |
| I6 | **`terminate_backend` for idle-in-tx holders** as a second SRE action (L1 script → L2 approval; never above L2) | Covers the most common lock root | M | Med (session kill) | Identity recheck, protected roles/apps, waiters ≥ 1 |
| I7 | **Route runbook `action` proposals into `action.ActionService`** with the family/class already computed | Makes signed runbooks useful | M | Med | Policy gate + ledger level; identity only from evidence |
| I8 | **pg_sage-self-load node**: probe of pg_sage's own sessions/pgss rows by `application_name`; surface `app_managed_index` as a change-feed source | Investigations can say "pg_sage did this" | S | Low | Read-only |
| I9 | **Per-DB vs fleet token split** (`deployment ≥ N × database`) | Fair budget | S | Low | Existing durable reservation |
| I10 | **Bench hygiene**: real plan flips, composites, paced nightly live-LLM arm, held-out cases | Credible numbers | M | Low | Pre-registered thresholds |
| I11 | Show evidence-strength labels instead of "score 0.85" until per-family calibration exists | Honesty | S | Low | — |
| I12 | **Incident plumbing**: newest-first trigger query filtered by mapped signals; object-aware auto-resolve; PagerDuty resolve with the detect dedup key; incident grouping/digest; order runway trends so WAL/XID are never truncated | Fewer stale incidents, starved investigations and duplicate pages | S–M | Low | Tests for >500 rows, foreign `database_name`, PD resolve |

---

## 4. Expand

- **Missing families** (§2.7) as graph nodes *and* bench programs, with `providerobs` as a probe
  for cloud throttling.
- **Remediation beyond cancel**, via `policy.Gate`/`Executor.Apply` with repair contracts:
  terminate idle-in-tx (I6); per-role `idle_in_transaction_session_timeout` and
  `CONNECTION LIMIT` (reversible, L2 cap); `ANALYZE` after a plan flip; reviewed query hint
  (`cases/query_hint_projector.go`). Slot drops and global GUCs stay manual scripts.
- **Proactive hunting**: a daily untriggered triage investigation per database that files
  recurring patterns as pre-incidents ("app X held xmin 3 h at 02:00 five nights running").
- **Postmortem draft** from the event hash chain + verified facts + labeled narrative;
  operator edits become outcome labels.
- **Cross-incident learning**: fleet-scoped memory with per-database opt-out; recurrence
  detection ("4th time this month; last fix was X").
- **MCP parity with the spec**: no `sre_investigate`, `sre_record_external_change` or outcome
  tool exists among the 16 `sre_*` tools, so external AI SREs cannot ask pg_sage to look.

---

## 5. AI-first redesign

**Principle:** the graph becomes prior and safety net; the model becomes the investigator.
Probes stay catalog-only, numbers come from code, actions go through the gate.

**Loop (per investigation):**
1. *Observe.* Deterministic detectors still open investigations (spec §2.1). The graph runs
   first and yields scored hypotheses `H₀`: cheap, and the fallback.
2. *Hypothesize.* The model receives `H₀`, the triage evidence, the change feed, similar
   incidents (fleet-wide, leakage-guarded) and the catalog. It may add hypotheses from the graph
   **or** `unmodeled:<slug>` with predicted observations and a refutation probe from the catalog.
3. *Probe (tool calls).* A real agent loop over `llm.ChatWithTools`
   (`internal/llm/tools.go`): each catalog probe is a tool with a typed schema (extend
   `probes.Args` with `relation_oid`, `queryid`, `role`, `application`, `slot`). Each call is
   checked by `probes.Catalog().CheckArgs`, counted against a per-investigation probe budget,
   and committed as immutable evidence before the model sees it. Ceilings become config with
   hard maxima (e.g. 8 turns, 24 probes, 64k tokens, 5 min).
4. *Conclude.* Model emits `root` (graph node or `unmodeled`), contributing, ruled-out with
   contradicting evidence IDs, missing evidence, operator step. Validation as today
   (`claims.go`, `model_verify.go`), plus: every supported hypothesis must cite ≥1 evidence whose
   probe the node's `Predicted` names; `unmodeled` roots are shown, never actioned.
5. *Arbitrate.* Graph conclusive + model agrees → concluded. Graph conclusive + model disagrees
   → concluded with the graph root, model root shown as "contested", review request opened.
   Graph inconclusive + model conclusive on a graph node with validated support → concluded
   with `source=model`, actions capped at L1 for that family until the bench shows the model
   arm's top-1 on inconclusive cases ≥ the threshold. `unmodeled` → inconclusive + narrative.
6. *Act.* The model may *select* a typed action from the family's repair vocabulary; target
   identity comes only from evidence IDs (`action/target.go` pattern); the gate and ledger level
   decide. The model never authors SQL (retire Tier-2 `recommended_sql`).
7. *Verify.* Existing recovery predicates (`action/recovery.go`); code judges.
8. *Learn.* One **outcome ledger** row per investigation: graph root, model root, operator
   verdict, actual node, action, recovery verdict. Feeds (a) memory, (b) the shadow record,
   (c) a nightly job turning refuted/contested/unmodeled cases into replay cases (the export is
   nearly one already), (d) model-drafted graph-weight/node changes that a human signs (the
   runbook compile/sign pattern).

**Evaluation:**
- Replace `M3-LLM-ROOT` with a two-sided gate: the model arm may change a graph root only if,
  on the replay corpus, its *override precision* is ≥ 0.95 and it lifts top-1 on inconclusive
  cases. Report graph, graph+model, model-only, always-escalate side by side per family.
- Nightly paced live bench on a cheap (gpt-6-luna) and a frontier model, ≥3 repeats.
- **Production shadow mode**: run the agentic loop beside the deterministic one, store both;
  promotion counts agreement with operator verdicts.
- Grow the corpus from reviewed production investigations; hold out 30%.

**Cost control:** graph-first short-circuit (concluded + high-strength → one narration turn
only); cheap model for triage/narration, frontier model only on inconclusive or contested
cases; durable per-DB and per-fleet daily budgets (fix §2.18); cache probe results per
investigation; record $/investigation in the outcome ledger and bench report.

---

## 6. Top 5

**1. Make promotion reachable and the review loop one click (I1 + I2).** Today no default
install can ever propose L2 because no bench report is ingested and no UI records reviews
(`earned/evidence.go:209-219`, `config/sre_autonomy.go:19`, no caller of `/autonomy/reviews`
in `web/src`). Ship a CI-signed bench report with each release and ingest it at startup; add
Accept/Reject + "actual root" on every concluded investigation that writes both feedback
tables; replace the empty pending card with a checklist of unmet checks and ETA. Effort S–M;
this is the precondition for "earns trust, then becomes autonomous" meaning anything.

**2. Turn the model turn into a bounded tool-calling investigator with an `unmodeled` escape
hatch (§5 steps 2–5).** The graph stays the prior and fallback; the model plans probes over the
catalog, may conclude when the graph cannot, and may contest when it disagrees. Every
validator stays; model-sourced roots stay at L1 until the bench shows lift. Effort L; this
makes Sage SRE AI-driven rather than AI-annotated.

**3. Fix the bench so it can measure the model (I10 + new gate).** Retire the
"model may never change a root" gate in favour of override precision and inconclusive-case
lift; run a paced live-model arm nightly; add real plan flips, composites and a held-out set;
convert every contested/refuted production investigation into a replay case. Without this,
recommendation 2 cannot be promoted safely and no claim of model value is defensible.

**4. Close the open-ended and coverage gaps (I5, I8, §4 families).** Give the `operator`
trigger a broad triage plan so "investigate now" works; extend SLO-burn triage to all
matchers; add top-statement, wait-histogram and `pg_stat_io` probes; let investigations name
pg_sage's own load and app-managed indexes, both seen in dogfood. Fix the trigger starvation
and PagerDuty resolve bugs (I12) while there.

**5. Deliver the validated answer where operators are, and widen remediation (I3, I6, I7).**
Page with the SRE conclusion (root, cited facts, operator step, review buttons) instead of
Tier-2 free text; add `terminate_backend` for idle-in-transaction holders (capped at L2) and
route signed-runbook action proposals into the action service. Today the only executable
action skips the most common lock root, and runbook proposals are dead ends.
