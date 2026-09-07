# pg_sage Roadmap — 2026 H2 (research-backed)

*Synthesized 2026-06-10 from the as-built [REVERSE_SPEC](./REVERSE_SPEC.md) + five fresh
research reports under [`research/v2_*`](../research/). Every feature ends in an **autonomous
action**, not a chart. Full citations live in the research files; the load-bearing ones are
inline here.*

---

## 1. Thesis update

The original thesis — *an autonomous Postgres DBA that takes action* — is **confirmed and
sharpened by the research into a defensible wedge**, with one reframing:

> **Reversibility, not autonomy, is the product.** The community is openly hostile to LLMs with
> write access to production ("*Giving LLM agents direct, autonomous access to a real production
> database with write access seems insane*" — top-voted 2026 HN comment, cited in
> [v2_user_pain](../research/v2_user_pain_autonomous.md)). The market has answered this hostility
> by converging on **advise-only**: pganalyze *explicitly refuses* auto-apply on accountability
> grounds ("The Dilemma of the AI DBA", Jan 2026); Postgres.ai's "Self-Driving Postgres" ships
> only at SAE Levels 0–1. The **auto-apply-for-tuning rung in open, portable Postgres is empty.**
> The only players who actually auto-apply physical/plan changes — **Oracle Autonomous Database**
> and **Azure SQL automatic tuning** — are proprietary, single-vendor, and locked to their cloud.

pg_sage's wedge is the intersection nobody occupies: **(a) no-extension sidecar** (passes the
security review that killed OtterTune's SaaS), **(b) LLM-native reasoning** (root cause,
doc-grounded tuning, migration generation), and **(c) a gated, reversible executor** that can
actually *fire* the change. The job for 2026 H2 is to **earn trust by making every action
verifiable and reversible**, copying the verify-and-revert mechanics Oracle/Azure proved, and to
**claim the AgentDB-at-scale theater** before the serverless-Postgres vendors build a DBA layer on top.

Market context that shapes positioning (from [v2_competitive](../research/v2_competitive_postgres_autonomy.md)):
managed Postgres pivoted in 2025–26 toward *Postgres-as-substrate-for-AI* (vectors, MCP, agent
branching — Neon reports **97% of branches are now agent-created**; Crunchy was acquired by
Snowflake ~$250M; Timescale rebranded to Tiger Data). The vendors are racing to *host* agent
workloads; **none of them operates the resulting databases.** That is our opening.

---

## 2. Confirmed-by-research findings

1. **Verify-and-revert is the transferable IP, and pg_sage does it wrong today.** Every engine
   that auto-applies physical changes ties the revert decision to the **specific queries** the
   change targeted and verifies over a **frequency-scaled window** (Oracle SQL Performance
   Analyzer; Azure's 30-min–72-hr per-query window; SQL Server's >3σ + >10 CPU-sec gate). pg_sage's
   executor instead rolls back on **coarse global metrics after a fixed window** (REVERSE_SPEC §4),
   which masks per-table/per-query regressions. **This is the #1 foundation fix** — it unlocks
   trustworthy index and config auto-apply.
2. **LLMs don't hallucinate indexes much — validation cost is the real barrier.** The 2026
   Microsoft "LLM vs DTA" study found LLM index picks are rarely invalid but vary 4× run-to-run.
   Conclusion (from [v2_llm_prior_art](../research/v2_llm_dba_prior_art.md)): **HypoPG what-if
   validation is load-bearing**, and the LLM must *steer, not replace* the deterministic validator
   ("guilty until proven innocent", per Oracle SPM). pg_sage's tier split already encodes this.
3. **The most-automatable pain is surgical and reversible, not dramatic.** Top of the
   recurrence × LLM-closeability ranking: **per-table autovacuum reloptions tuning** (`ALTER TABLE
   … SET (autovacuum_vacuum_scale_factor=…)`) — not "run VACUUM." Then ANALYZE/extended-stats
   creation, TXID-wraparound freeze, and dead-replication-slot cleanup (a cited 2025 postmortem
   went **1.06 TB → 40 GB** by dropping two dead slots).
4. **AgentDB's biggest win is nearly free.** The lifecycle reconciler that bounds ephemeral-DB
   sprawl is **already built and tested but unscheduled** (REVERSE_SPEC §5). Wiring it is a config
   change, not a feature.
5. **Nobody solves multi-tenant noisy-neighbor isolation cleanly** (Nile: quotas "not yet
   implemented"; Turso: isolation "not perfect"). An observe-then-throttle auto-quarantine loop
   would exceed what any serverless-Postgres platform offers.

---

## 3. Foundation fixes (do these FIRST — they gate everything below)

These are *finish/fix* items from the reverse-spec, not new features. Several roadmap items are
blocked on them.

| # | Item | Type | Why it gates the roadmap | Effort |
|---|---|---|---|---|
| F1 | **Per-queryid verify-and-revert window** (replace global-metric rollback) | FIX | Prerequisite for *any* trustworthy index/plan auto-apply (Theme A). Copy Azure's model. | M |
| F2 | **Postgres "mini Query Store"** — snapshot per-queryid plan hash + latency over time | BUILD | Substrate for F1, plan-regression detection, and "why did the plan change" narratives. No extension needed (`pg_stat_statements` + plan sampling). | M–L |
| F3 | **Wire HA safe-mode into the executor** (`ha.InSafeMode()` is dead) + **replica gating** in fleet executors (`RunCycle(ctx,false)`) | FINISH | Without it, autonomous DDL can fire against a node whose role is changing, or a replica. Blocks turning autonomy on safely. | S |
| F4 | **Schedule the AgentDB lifecycle reconciler** (built, tested, unwired) | FINISH | Unlocks TTL/archival/sprawl control (Theme B) at ~zero build cost. | S |
| F5 | **Wire `fleet.FleetBudget`** (per-DB LLM token budget — built, never constructed) | FINISH | Required before running LLM tuning across thousands of AgentDBs without cost blowups. | S |
| F6 | **Auto-promoting trust ramp** (today day-8/31 thresholds gate a *manually-set* string; no auto-promotion) + **per-action-class trust** | FIX/BUILD | The whole "earn trust over time" story is currently manual. Research flags time-based ramp conflates operator-confidence with intrinsic-action-risk — make trust per-action-class. | M |
| F7 | **Register event-path notification senders** (executor/analyzer notifications silently no-op) + **emergency-stop re-check in the rollback goroutine** | FIX | Safety/telemetry integrity for autonomous actions. | S |

---

## 4. Theme A — Autonomous Core DBA

*Each: problem (cited) → LLM call → autonomous action → safety/gating → competitor → effort.*

### A1. Per-table autovacuum tuner  ⭐ flagship quick-win
- **Problem:** Global autovacuum settings are wrong for skewed tables; practitioners hand-tune
  `autovacuum_vacuum_scale_factor` per hot table and beg for it to be automatic
  ([v2_user_pain](../research/v2_user_pain_autonomous.md), multiple dba.SE/r/PostgreSQL threads).
- **LLM call:** given a table's dead-tuple rate, size, write pattern, and current reloptions →
  recommend per-table reloptions with a rationale.
- **Autonomous action:** `ALTER TABLE … SET (autovacuum_vacuum_scale_factor=…, autovacuum_vacuum_cost_limit=…)`.
  Fully reversible (store prior reloptions as rollback SQL).
- **Safety:** SAFE tier; rollback = restore prior reloptions; verify dead-tuple ratio improves over
  N cycles else revert. Deterministic rule decides *whether*; LLM decides *which values/why*.
- **Competitor:** nobody in open Postgres auto-applies this; Azure does the equivalent for its knobs.
- **Effort:** **S–M.** Reuses advisor + executor. **Highest ROI item on the board.**

### A2. Index lifecycle: auto-create → verify → auto-drop (Oracle-model)
- **Problem:** Missing indexes and unused/duplicate index bloat are the perennial #1/#2 DBA chores.
- **LLM call:** propose candidate indexes from slow-query + seq-scan evidence (already exists in the
  optimizer); **HypoPG validates** the benefit before anything is built.
- **Autonomous action:** `CREATE INDEX CONCURRENTLY` (exists) → **F1 per-queryid verify** → keep or
  `DROP INDEX CONCURRENTLY`; separately, **measured-non-use auto-drop** of unused indexes after a
  configurable observation window (copy Oracle's 373-day / Atlas's 7-day pattern).
- **Safety:** MODERATE; never drop unique/PK/constraint-backing indexes; rejection blocklist for
  candidates that didn't help; per-table index cap (Atlas caps at 4) to prevent write-amp.
- **Competitor:** Oracle ADB gold standard; Azure SQL; pganalyze/AlloyDB advise only. **This is the
  white space.**
- **Effort:** **M** (create exists; needs F1+F2 and the drop/verify loop).

### A3. Version-aware, doc-grounded config tuning (GPTuner-style)  ⭐ LLM-native moat
- **Problem:** `work_mem`, `shared_buffers`, `checkpoint_*`, WAL settings — endlessly asked, version-
  and workload-dependent, easy to get wrong.
- **LLM call:** the **defensible** shape from GPTuner/λ-Tune research: the LLM reads the *Postgres
  manual + release notes for the detected major version* as a **prior**, proposes a knob region; a
  deterministic/BO validator picks the value. The LLM is a prior generator, not the optimizer.
- **Autonomous action:** `ALTER SYSTEM SET …` (or `ALTER DATABASE` per the cloud-transform that
  already exists) + reload; rollback = prior value.
- **Safety:** SAFE for session-scoped/reloadable knobs, MODERATE for restart-requiring; verify on
  workload metrics over a window; never auto-apply restart-requiring changes outside a maintenance window.
- **Competitor:** OtterTune (dead) did ML knobs; **nobody ships LLM-doc-grounded tuning for Postgres.**
- **Effort:** **M–L.** Advisor scaffolding exists; the doc-RAG + version-awareness is new and is a moat.

### A4. ANALYZE / extended-statistics autopilot
- **Problem:** Bad row estimates from missing/stale stats and absent multi-column stats cause plan
  disasters; "run ANALYZE" and "create extended statistics" are constant advice.
- **LLM call:** from estimate-vs-actual skew + correlated-column detection → recommend
  `CREATE STATISTICS` (ndistinct/dependencies) and targeted `ANALYZE`.
- **Autonomous action:** `ANALYZE table` (SAFE, exists partially) and `CREATE STATISTICS …` (SAFE,
  reversible via DROP).
- **Safety:** SAFE; cheap and reversible; process-wide ANALYZE semaphore already exists.
- **Effort:** **S.** Quick win.

### A5. Plan-regression detection + (advise-first) remediation
- **Problem:** "The query was fast yesterday, slow today" — plan flips after a stats change or
  version upgrade. Most-requested, but **auto-remediation is dangerous** (research explicitly
  down-ranks auto plan-forcing).
- **LLM call:** on F2's plan store detecting a regressed queryid → explain the likely cause and
  propose a fix (index, stats, rewrite, or — last resort — a `pg_hint_plan` pin).
- **Autonomous action (staged):** start **advisory** (open a finding/case with the diagnosis); only
  auto-apply the *non-plan-forcing* fixes (stats/index) under trust. Plan-forcing stays manual until
  F2 is mature, and likely requires relaxing the no-extension stance for `pg_hint_plan` (open question).
- **Safety:** advisory → SAFE sub-actions only; never auto-force a plan in v1.
- **Effort:** **M** (depends on F2). Big differentiator: **"why did the plan change" narrative** is
  LLM-native and nobody explains it well.

### A6. Dead-replication-slot & wraparound guardian
- **Problem:** Inactive replication slots silently block autovacuum cluster-wide → bloat →
  TXID-wraparound emergency (cited postmortem: 1.06 TB → 40 GB after dropping two dead slots).
- **LLM call:** classify whether a slot is genuinely abandoned vs a briefly-disconnected real
  consumer (this judgment is exactly what an LLM + context is good at).
- **Autonomous action:** escalating — **advise** for slot drop (irreversible! high bar), but
  **auto-fire** the reversible mitigations: aggressive freeze/`VACUUM (FREEZE)` on at-risk tables,
  per-table autovacuum cost-limit bumps as wraparound age climbs.
- **Safety:** slot drop stays HIGH/manual (irreversible); freeze actions SAFE.
- **Effort:** **S–M.** High-impact incident prevention.

**Deliberately NOT building (research-driven cuts):**
- **Partition maintenance** — `pg_partman`'s background worker already owns it; only future-partition
  pre-creation *where pg_partman is absent* is a clean slice. Don't re-implement.
- **Connection pooling auto-fix** — the real fix lives outside the DB (PgBouncer); stay advisory.
- **Text-to-SQL** — out of scope; that's human-to-data, pg_sage is database-to-database.

---

## 5. Theme B — AgentDB at scale (the new theater)

*Serving databases deployed BY agents: ephemeral, 100s–1000s/hour, poorly tuned, sporadic,
multi-tenant. Today AgentDB provisions but does not operate them (REVERSE_SPEC §5).*

### B1. Bring AgentDBs into the fleet + schedule the reconciler  ⭐ near-free
- **Problem:** Provisioned agent DBs aren't monitored or tuned, and orphaned DBs sprawl (Neon's
  "2,847 branches vs two" problem).
- **Action:** register agent-provisioned DBs into `fleet.DatabaseManager` so the existing
  collector/analyzer/executor pipeline applies; **schedule the built-but-unwired lifecycle
  reconciler** (F4) to enforce TTL/lease expiry/archival.
- **Effort:** **S.** Mostly wiring existing parts. Unlocks everything else in Theme B.

### B2. Zero-touch tuning of freshly-spawned DBs
- **Problem:** Agent-created DBs are "often poorly tuned" — default config, no indexes, no stats.
- **LLM call + action:** on first workload observation, apply a **config template** sized to the
  instance + the Theme-A tuners (A1/A3/A4) automatically at a higher default trust for ephemeral
  DBs (lower blast radius than production).
- **Safety:** ephemeral/non-production DBs can ramp trust faster; per-tenant FleetBudget (F5) caps LLM cost.
- **Effort:** **M** (composes A1/A3/A4 + B1).

### B3. Auto-rightsizing & scale-to-zero for sporadic usage
- **Problem:** Agent workloads are bursty; idle DBs waste money. Neon/Aurora Serverless v2 scale to
  zero; most platforms don't.
- **LLM call:** classify a DB's usage pattern (steady / bursty / abandoned) from activity history →
  recommend rightsizing or pause/archive.
- **Autonomous action:** trigger provider rightsizing (the AgentDB runners already create/destroy);
  pause-or-archive idle DBs; resume on demand.
- **Safety:** reversible (archive then restore); chargeback (B5) makes savings visible.
- **Effort:** **M–L** (provider-specific; reuses runners).

### B4. Noisy-neighbor auto-quarantine  ⭐ nobody does this
- **Problem:** One tenant's runaway query starves shared compute; **no serverless-Postgres platform
  isolates this cleanly** (Nile/Turso admit it).
- **LLM call:** detect a tenant exceeding its share → classify malicious/buggy/legitimate-spike.
- **Autonomous action:** observe-then-throttle — set per-role `statement_timeout`/`work_mem` caps,
  cancel runaway backends, rate-limit the tenant's connections; escalate to quarantine.
- **Safety:** soft caps on shared compute (flagged honestly — memory/IO caps stay soft without
  cgroups); reversible role-setting changes.
- **Effort:** **M.** Genuine differentiator.

### B5. Usage metering & chargeback/showback
- **Problem:** Chargeback today is **agent-self-reported** (REVERSE_SPEC §5) — untrustworthy. Finance
  needs real per-tenant cost.
- **Action:** meter real resource use (connections, storage, query time, LLM tokens) per tenant from
  the collector; produce showback/chargeback; auto-enforce budgets (auto-`budget_exceeded` exists —
  make it metered, not self-reported).
- **Effort:** **M.** Mostly deterministic; high enterprise value.

### B6. Fleet-wide policy & drift enforcement at scale
- **Problem:** Thousands of DBs drift from their config template; security defaults (least-privilege,
  SSL, no public IP) need to hold across the fleet.
- **LLM call:** explain *why* a DB drifted and the blast radius of correcting it.
- **Autonomous action:** bulk-apply config templates; auto-correct drift; enforce per-tenant
  least-privilege role defaults at provision time.
- **Safety:** uses F5 budgets + trust ramp; dry-run preview for bulk ops.
- **Effort:** **M–L.**

---

## 6. Theme C — LLM-native differentiators (defensibility-ranked)

*These are hard for advise-only competitors to copy and compound the moat. From
[v2_llm_prior_art](../research/v2_llm_dba_prior_art.md).*

| Rank | Capability | What it is | Autonomous payoff |
|---|---|---|---|
| C1 | **Natural-language root cause across correlated signals** | D-Bot-style tool-grounded diagnosis over the RCA signals pg_sage already has | Turns an incident into a ranked, cited diagnosis → drives the right auto-action |
| C2 | **Version-aware doc-grounded tuning** (= A3) | LLM reads PG manual/release-notes as a prior | Safe, explainable config auto-tuning nobody else has |
| C3 | **Migration generation + risk classification** | Generate online-DDL migrations + classify risk (exists partially) | Auto-stage safe migrations; gate risky ones |
| C4 | **Plain-English justification of every autonomous action** | LLM writes the "why" into the audit log | The accountability answer to pganalyze's "AI DBA dilemma" — *this is what makes auto-apply acceptable* |
| C5 | **Cross-database fleet learning** | What worked on DB X informs DB Y | Compounds with AgentDB scale (thousands of DBs = training signal) |
| C6 | **"Why did the plan change" narratives** (= A5) | Explain plan flips from the F2 plan store | Differentiated incident UX → drives remediation |

**Grounding discipline (non-negotiable, from research):** the LLM never emits DDL that fires
unvalidated. HypoPG/what-if or the deterministic validator gates physical changes; rollback SQL must
be a stored statement or the action **does not auto-fire** (proposed hard boundary).

---

## 7. Quick wins vs big bets

**Quick wins (S, ship in 2026 H2):** F3 (HA/replica gating), F4 (schedule reconciler), F5 (wire
FleetBudget), F7 (notification senders + estop re-check), A1 (autovacuum tuner), A4 (ANALYZE/stats
autopilot), B1 (AgentDBs into fleet). *These earn trust and unlock the rest at low risk.*

**Big bets (M–L, start H2, land H1'27):** F1+F2 (per-queryid verify + plan store) → A2 (index
lifecycle) and A5 (plan regression); A3/C2 (doc-grounded tuning); B3/B4/B5 (rightsizing,
quarantine, chargeback). *These are the moat.*

**Sequencing (dependency-ordered):**
1. **Foundation:** F3, F4, F5, F7 → then F6 (trust auto-promotion) and F1+F2 (verify + plan store).
2. **Core DBA quick wins:** A1, A4 (ride existing executor) → A2 once F1+F2 land → A6.
3. **LLM moat:** A3/C2, then C1 (NL root cause), C4 (action justification — ship *with* the first
   auto-apply, it's the trust unlock), C6.
4. **AgentDB theater:** B1 (now) → B2, B5 → B3, B4, B6.

---

## 8. Cross-DB scorecard

| Capability | Table-stakes | Differentiator for pg_sage | Don't bother |
|---|---|---|---|
| Index advice | ✅ (everyone has it) | **Auto-create-verify-drop** loop (open-PG white space) | |
| Plan management | | **NL plan-change narrative**; advise-first remediation | Auto plan-forcing in v1 (unsafe) |
| Config tuning | partial | **Doc-grounded, version-aware, LLM-prior + validator** | Pure-ML knob tuning (OtterTune died) |
| Verify-and-revert | (Oracle/Azure only) | **Per-queryid window on open Postgres, no extension** | |
| Autovacuum/stats | | **Per-table reloptions auto-tuning** | Re-implementing pg_partman |
| Multi-tenant isolation | (nobody clean) | **Observe-then-throttle auto-quarantine** | Hard cgroup isolation on shared compute |
| Ephemeral DB ops | (Neon hosts, doesn't operate) | **Autonomous DBA layer for agent DBs** (tune/rightsize/archive/chargeback) | Building our own serverless storage engine |
| Failover/HA | ✅ (Patroni/pg_auto_failover) | wire HA safe-mode for *action safety*, not failover itself | Competing with Patroni |
| Text-to-SQL | | — | **Out of scope** |

---

## 9. Questions you should be asking (that you mostly aren't)

1. **Is the no-extension stance a moat or a ceiling?** It's why pg_sage passes security review
   (OtterTune's killer), but it blocks `pg_hint_plan` plan-forcing and a real plan store. Where
   exactly does "no extension" stop being worth it — and should there be an *optional* extension tier?
2. **Should trust be per-action-class instead of one calendar-gated level?** Research says time-based
   ramp conflates *operator confidence over time* with *intrinsic action risk* — they're orthogonal.
   Auto-applying an ANALYZE on day 1 is fine; dropping an index on day 40 may not be.
3. **What is the hard line for "may auto-fire"?** Proposed: *no autonomous action unless its rollback
   is a single stored SQL statement.* That cleanly excludes irreversible ops (slot drop, partition
   detach) from autonomy forever. Do you accept that as a product invariant?
4. **Who is the buyer — the DBA, the platform team, or the AI-agent platform?** The AgentDB theater
   implies a *platform* buyer (someone running 10k agent DBs), which is a different GTM, pricing, and
   security posture than a single-DB DBA tool. Picking one sharpens everything.
5. **Does the AgentDB theater cannibalize or complement the core DBA product?** They share an engine
   but serve opposite customers (one big precious DB vs thousands of disposable ones). Are these one
   product or two?
6. **If Neon/Supabase/Aurora ship a built-in autonomous DBA layer, what's left?** They host 97% of
   agent DBs already. Is pg_sage's defensibility the *cross-provider, provider-neutral* control plane
   — and does that survive a vendor building it in-house for their own platform?
7. **How do you prove an autonomous action helped without a thin-clone?** Postgres.ai's DBLab is the
   "verify before apply" substrate. Should pg_sage consume it (adapter) rather than verify in prod?
8. **What's the liability/audit story when an auto-action causes an outage?** pganalyze refuses
   auto-apply *specifically* to avoid this. C4 (plain-English justification + rollback) is the answer
   — but is it enough for an enterprise to accept? What contractual/audit artifacts are needed?
9. **Is "LLM reads the docs to tune" durable as base models get better at SQL natively?** The moat is
   the *grounded validator loop*, not the LLM — make sure the defensibility is in the
   verify/HypoPG/plan-store machinery, not the prompt.
10. **Are you measuring the right success metric?** Not "findings surfaced" (observability) but
    "**DBA-hours saved** and **incidents prevented**, with zero auto-action-caused regressions." If
    the metric is still dashboard engagement, the product will drift back to observability.
11. **Can the executor actually verify per-query without an INVISIBLE index flag?** Postgres has no
    Oracle-style invisible index; building-then-measuring exposes the index workload-wide. Is HypoPG
    what-if enough, or do you need a thin-clone (Q7)?
12. **What happens at 10k databases to the LLM cost curve?** F5 (FleetBudget) caps it, but per-DB LLM
    tuning at agent scale needs a *cheap deterministic default* with LLM only on the exceptions.
    Where's the LLM/deterministic boundary at scale?

---

## 10. How this maps to the existing code (finish / build / cut)

- **FINISH (built but dead — high leverage):** HA safe-mode (F3), FleetBudget (F5), lifecycle
  reconciler (F4), event-path notification senders (F7), optimizer confidence-threshold gate,
  cases→executor connection.
- **FIX:** global→per-queryid verify-and-revert (F1), replica gating in fleet executors (F3),
  emergency-stop re-check in rollback goroutine (F7), trust auto-promotion + per-action-class (F6).
- **BUILD (new):** F2 plan store; A1/A2/A3/A4/A5/A6; B2/B3/B4/B5/B6; C1/C4.
- **CUT / DEPRIORITIZE:** partition maintenance (pg_partman owns it), auto plan-forcing v1,
  text-to-SQL, Terraform execution (finish-or-cut the rendered-but-never-run Terraform path).

*Every recommendation above is grounded in the research files in [`research/v2_*`](../research/) and
the as-built [REVERSE_SPEC](./REVERSE_SPEC.md). Citations are inline in those source files.*
