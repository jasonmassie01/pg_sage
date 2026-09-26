# Research: Prior Art for an AI-Driven SRE Feature for Postgres Fleets

Date: 2026-09-26. Scope: technical prior art and design patterns to make the pg_sage "AI SRE" spec
state-of-the-art and measurable. Method: web search plus primary-source reads. For the most
spec-relevant 2026 papers (DBA-Bench, SREGym, ORCA-bench, Kim et al., Lu et al.) and D-Bot, the
PDFs were downloaded and the quoted numbers were checked against the extracted text.

Legend:
- **[V]** means the number or claim was checked against the primary source text (PDF or official doc).
- **[S]** means it comes from the abstract, a search snippet, or the vendor/author page, and was
  not checked in the full text.
- **UNVERIFIED** means an inference, recollection, or claim I could not confirm from a primary
  source. Treat it as a hypothesis to check before it goes into the spec.

Model names such as "GPT-5.5", "Claude Opus 4.8" and "Claude Fable 5" are copied exactly as the
cited papers wrote them.

---

## 0. Executive summary

1. **Independent benchmarks are much harsher than the results papers report for themselves.**
   D-Bot reports Acc 0.754 (single-cause) and 0.655 (multi-cause) on its own PostgreSQL
   micro-benchmark [V]. On the independent DBA-Bench (PostgreSQL, 106 scenarios, July 2026),
   D-Bot reaches **5.7% Safe Pass** at **$7.16 per run**. The best automated system reaches
   17.9% and a human DBA reaches **93.4%** [V].
2. **Diagnosis is not remediation.** On DBA-Bench, 62.1% of runs that pass Diagnosis fail
   Outcome, and 36.7% of runs that restore the outcome do it unsafely [V]. Of the unsafe cases,
   80% were unscoped interventions or missing safeguards, not "destructive" commands [V].
3. **The dominant failure mode is hallucinated interpretation of real data, not fabricated
   data.** Kim et al. ran 1,675 OpenRCA runs. "Hallucination in interpretation" appeared in
   71.2% of runs, "incomplete exploration" in 63.9%, "symptom-as-cause" in 39.9% and "no
   cross-validation" in 18.6% [V]. Prompt engineering did not fix these.
4. **Procedure-guided agents beat free-form ReAct.** Flow-of-Action (SOP-guided multi-agent)
   scored 64.01% against ReAct's 35.50% [S]. StepFly (TSG to DAG execution) reports about 94%
   success [S]. For pg_sage this favors deterministic runbooks with LLM gap-filling over an
   open-ended agent.
5. **Classic statistics are hard to beat.** On RCAEval, simply "picking the largest anomaly"
   scores 75.8% [S]. Pham et al. (ASE 2024) found that PC, FCI, Granger, LiNGAM and random-walk
   causal RCA performed "similarly to Dummy/random". NSigma, BARO and CIRCA did well once the
   failure time is known [S]. Any AI SRE must beat these baselines in its evaluation report.

---

## 1. LLM and agent RCA research

### 1.1 Systems (architectures and reported accuracy)

| System (venue) | Architecture | Evaluation and result | Notes for pg_sage |
|---|---|---|---|
| **D-Bot / "LLM as DBA"** (Tsinghua, VLDB 2024; arXiv 2312.01454, 2308.05481) | Offline knowledge extraction from diagnosis docs. Tool retrieval. **Tree-search** reasoning. **Multi-agent** "expert roles" (8) with asynchronous cross-review. Built on PostgreSQL 12.5 + pg_stat_statements + HypoPG. | 539 anomalies (254 single-cause, 285 multi-cause) from 6 apps. D-Bot(GPT-4) Acc 0.754 / 0.655, HEval 0.500 / 0.669. Human DBA (2 years experience) Acc 0.955 / 0.487. Vanilla GPT-4 Acc 0.351 / 0.105. Ablations: no knowledge costs -19.2 to -64.1% accuracy; no tree search costs >35.85%. About 5 min per diagnosis, about $1.8 per anomaly [V]. | Knowledge retrieval matters a lot. Self-built benchmark dominated by "missing index" and "many updates". **Collapses on the independent DBA-Bench (5.7% Safe Pass)** [V]. |
| **Panda** (AWS, CIDR 2024) | Four components: Grounding, Verification (with citations), Affordance (impact assessment), Feedback [S]. | Design paper, no large benchmark [S]. | This is essentially the pg_sage thesis: cite evidence, estimate impact, close the feedback loop. |
| **RCACopilot** (Microsoft, EuroSys 2024; arXiv 2305.15778) | Alert-type-matched **incident handlers** (deterministic diagnostic collection) followed by LLM root-cause *category* prediction and explanation. | 1 year of Microsoft incidents, Micro-F1 0.766. Collection stage in production for more than 4 years across 30+ teams [S]. | Validates "deterministic collectors first, LLM classifies and explains". |
| **In-context RCA with GPT-4** (Microsoft, arXiv 2401.13810) | Retrieval of similar **historical incidents** as in-context examples. | 100k incidents. +24.8% over fine-tuned GPT-3, +49.7% over zero-shot. Human-rated correctness +43.5% [S]. | Supports an incident-memory retrieval store in the `incidents`/`cases` substrate. |
| **Ahmed et al.** (ICSE 2023; arXiv 2301.03797) | Fine-tuned LLMs recommend root cause and mitigation. | 40k+ incidents, lexical and semantic metrics plus owner evaluation [S]. | Early evidence. Lexical metrics are weak judges of correctness. |
| **RCAgent** (Alibaba, CIKM 2024; arXiv 2310.16340) | Tool-augmented agent on an internally deployed model (privacy). **Self-consistency over action trajectories**. Observation Snapshot Key (keeps an observation head in context and stores the full observation in a KV store). | Beats ReAct on root cause, solution, evidence and responsibility (automated plus human metrics). Deployed on Alibaba Flink [S]. No single headline accuracy found. | The OBSK pattern maps to large query results such as pg_stat_statements dumps. |
| **mABC** (EMNLP 2024 Findings; arXiv 2404.12135) | Seven role agents with **blockchain-inspired weighted voting** to reduce hallucination. Bounded steps. | AIOps challenge dataset plus Train-Ticket. "Superior" [S]. | Voting is costly. DBA-Bench shows elaborate architectures do not pay off automatically. |
| **Flow-of-Action** (WWW 2025 Companion; arXiv 2502.08224) | **SOP knowledge base**. An SOP flow constrains the order of tool calls. Multi-agent. | 64.01% against ReAct 35.50% [S]. | Strongest evidence for runbook-constrained investigation. |
| **Nissist** (Microsoft, ECAI 2024; arXiv 2402.17531) | Turns unstructured troubleshooting guides (TSGs) plus mitigation history into a node-level KB. Multi-agent copilot. | Reports reduced time to mitigate. No headline accuracy found [S]. | |
| **StepFly** (Microsoft, FSE 2026; arXiv 2510.10074) | TSG quality tooling, then offline LLM extraction of an **execution DAG** plus query plugins, then an online DAG scheduler that runs independent steps in parallel. | About 94% success on GPT-4.1, 32.9–70.4% faster on parallelizable TSGs [S]. | Runbooks should compile to typed DAGs, not stay free text. |
| **Meta RCA** (eng blog, 2024-06-24) | Heuristic retriever (ownership, code graph) narrows thousands of changes to a few hundred. An LLM ranker picks the top 5. | 42% of investigations had the root cause in the top 5 at investigation creation [S]. | **Change correlation plus LLM ranking**. They stress confidence measurement and explainability. |
| **DBAIOps** (arXiv 2508.01136) | Reasoning LLM plus a **knowledge graph** (symptoms, metrics, log patterns, causes, knobs, operations, risks, verification signals). | On DBA-Bench: 14.2% Safe Pass at $0.315 against GPT-5.5 ReAct 17.9% at $1.02 [V]. | The KG schema (with "requires-evidence" and "unsafe-under" edges) is a good template for pg_sage rule metadata. |
| **DiagGuard** (Lu et al., arXiv 2608.21310, Aug 2026) | Two-stage defense: **ground observations before localization**, then **verify the diagnosis against evidence**. | Acc@1 43.5% → 52.5% on an independent model and benchmark [V]. | Direct recipe for a verify-then-conclude stage. |
| **Google AI Operator** (sre.google "AI in SRE") | Parallel investigation modules, deterministic enrichers, sub-agents, strict token-per-step budget, escalates if root cause cannot be identified. L2/L3 autonomy. | About 44% time-to-mitigate reduction for supported incidents (investigation dashboards). 10% time-to-mitigate reduction for L1 "Incident Hypothesis" [S]. | Most complete industrial safety architecture (see §5). |
| **LLM postmortem generation** | Little rigorous public work was found specifically on DB or SRE postmortem generation. The adjacent work is Microsoft's incident-summarization and RCA recommendation papers above. | **UNVERIFIED gap.** No benchmark for postmortem quality was found. | pg_sage should generate postmortems deterministically from the evidence ledger (§2), using the LLM only for prose. |

### 1.2 Benchmarks

| Benchmark | Setting | Headline result |
|---|---|---|
| **OpenRCA** (Microsoft, ICLR 2025) | 335 failures, 3 enterprise systems, 68 GB of logs, metrics and traces. Scored on time, component and reason. RCA-agent uses a controller plus a Python executor. | Best model solved **11.34%** [S]. Kim et al. re-ran the full benchmark on 5 newer models: 3.9–12.5% [V]. |
| **ITBench** (IBM, ICML 2025) | 94 scenarios (SRE, CISO, FinOps) on Kubernetes. | SRE 13.8%, CISO 25.2%, FinOps 0% [S]. |
| **ITBench-AA** (IBM plus Artificial Analysis, 2026-05-27) | 59 SRE RCA tasks × 3 repeats. "Average precision at full recall": a task scores 0 if any true cause is missed, and **submitting co-occurring symptoms or upstream mechanisms is penalized**. | Claude Opus 4.7 47%, GPT-5.5 46%, Qwen3.7 Max 42%. More turns did not mean more accuracy (Gemini 3.1 Pro: 83 turns at 30%) [S]. |
| **AIOpsLab** (Microsoft, 2025; arXiv 2501.06706) | Live microservices with fault injection. Tasks: detection, localization, RCA, mitigation. 48 problems. | Detection up to 100% (FLASH). Localization top-1 up to 61.5%. RCA up to 45.5%. **Mitigation up to 54.6%** [S]. |
| **SREGym** (arXiv 2605.07161, May–Jul 2026) | 90 problems, 47 fault primitives, **ambient noise injection** (two 2-minute noises every 5 minutes), metastable and correlated failures. Backends include MongoDB. **No PostgreSQL faults were found in the text** [V]. | Diagnosis 38.1–72.6%, mitigation 40.4–78.5% across agent/model pairs. Up to 40-point E2E gaps by failure layer. Noise measurably lowers diagnosis. Agents "draw partial conclusions" on compound failures [V]. |
| **ORCA-bench** (arXiv 2607.28545, Jul 2026) | 1,079 RCA tasks, varying report specificity and detection timing. Human re-score κw = 0.90. | Best **25.3% (Medium)** and **10.0% (Hard)**. Hallucinated implausible root cause in **7–40%** of reports, depending on model. **26–40% of telemetry tool calls error out or return empty**. Removing source-code access hurts every model [V]. |
| **DBA-Bench** (UESTC, arXiv 2607.22165, Jul 2026). **Most relevant.** | PostgreSQL under live OLTP/OLAP load. 106 scenarios in 7 categories (query tuning, system failure, periodic health check, business change, resource governance, **composite faults**, **misleading alerts**). Environment restored from snapshots, with **manifestation predicates** checked before each run and a **post-fix verifier**. | Primary metric is **Safe Pass** (outcome restored **and** zero safety violations). Best 17.9% (GPT-5.5, Claude Opus 4.8). Human DBA 93.4%. Easy scenarios 19.6% Safe Pass, Hard 7.6% [V]. |
| **RCAEval** (WWW 2025 / FSE 2026) | 735 failure cases across 3 microservice systems. 15 baselines. | Best Avg@5 is 0.54 (RCD) and 0.46 (CIRCA) [S]. |
| **AIOps2025 + RCA100** (Cai et al., arXiv 2606.29193) | 503 expert-labeled cases. Scores **reasoning/evidence** (20–30% weight), not only the answer. | Recommends moving from "final-answer matching" to evidence-grounded scoring [S]. |
| **Hu, Liu, Fu** (arXiv 2606.29159) | Audit of OpenRCA, RCAEval and PetShop reporting practice. | **Pooled leaderboards hide system-specific winners.** Leave-one-system-out regret reaches 24.8 points [S]. |
| SRE-skills-bench (Rootly) and ReliabilityBench (arXiv 2601.06112) | Cloud SRE knowledge tasks; consistency, robustness and fault-tolerance surface. | A benchmark literally named "SRE-bench" was **not found** (UNVERIFIED that one exists). |

### 1.3 Cross-cutting findings

- **Architecture does not buy accuracy by itself.** Holding the backbone fixed on DBA-Bench,
  ReAct (17.9%) beat the knowledge-graph agent DBAIOps (14.2%) and the tree-search agent D-Bot
  (5.7%) [V]. DBA-Bench also reports that no automated baseline leads in every category.
- **What does help:** retrieval of domain knowledge and past incidents (D-Bot ablation;
  Microsoft ICL), SOP/TSG-constrained tool order (Flow-of-Action, StepFly), evidence
  verification (DiagGuard), self-consistency over trajectories (RCAgent), and deterministic
  data collectors (RCACopilot).
- **Failure-mode taxonomy to design against** (DBA-Bench [V]; percentages are the dominant
  mode within each category):
  - **Noise/decoy anchoring**: 68.3% of misleading-alert failures.
  - **Causal-chain truncation**: 45.9% of composite-fault failures.
  - **Wrong remediation target**: 34.7% of system-failure failures.
  - **Decisive evidence omission**: 33.0% of health-check failures.
  - **Unclosed verification loop**: 24.4% of query-tuning failures.

  Kim et al. adds symptom-as-cause, timestamp errors and no cross-validation. ORCA-bench adds
  the finding that empty or failed tool calls are common, and agents silently treat them as
  "no problem".
- **The authors' own design lessons** (DBA-Bench) are to make hypothesis state explicit, to
  carry a repair contract through the whole loop ("a final safety filter is too late"), and to
  evaluate on end-to-end Safe Pass per category together with cost.

---

## 2. Grounding and hallucination control for ops agents

These are patterns with evidence behind them. The spec should require each one.

1. **Tool-call-only facts (evidence ledger).**
   - Every factual claim in a diagnosis must reference an evidence item ID. An evidence item is
     a stored tool result: query, timestamp, instance, a row hash, and the value.
   - Claims without a reference are dropped or marked "conjecture".
   - Sources: Panda's "Verification with citation" [S]. Cai et al.'s evidence-coverage scoring
     [S]. Google's transparency requirement to log reasoning chains, hypotheses and confidence
     [S].
2. **Empty result is not negative evidence.**
   - ORCA-bench found 26–40% of telemetry calls error or return empty [V].
   - Tools must return typed outcomes: `ok`, `empty`, `error`, `not_collected`,
     `insufficient_privilege`. The reasoner must not read `error` as "healthy".
   - For pg_sage this matters because `pg_stat_statements` may be missing or `pg_read_all_stats`
     may not be granted.
3. **Explicit hypothesis ledger with falsification tests.**
   - Keep competing hypotheses, each with its predicted observations and a **discriminating
     test** (a query whose result would refute it).
   - Rank hypotheses by how many predictions survived.
   - Sources: DBA-Bench ("seek disconfirming observations, and record which causal links remain
     unverified") [V]. Causely's codebook, where each root cause has a symptom-signature column
     [S].
   - This also counters the symptom-as-cause and decoy-anchoring failures.
4. **Symptom vs cause discipline.** ITBench-AA penalizes listing co-occurring symptoms or
   upstream mechanisms [S]. The output schema should separate `root_cause`,
   `contributing_factors`, `symptoms` and `blast_radius`.
5. **Verify the diagnosis, not just the action.** DiagGuard (ground first, verify second)
   raised Acc@1 by 9 points [V]. Add a separate verifier pass, with a different prompt and
   ideally a different model, that checks each claim against the ledger.
6. **Self-consistency and voting.** Run N independent investigations and use agreement as a
   confidence signal (RCAgent trajectory self-consistency; mABC voting). This multiplies cost,
   so reserve it for high-severity incidents.
7. **Calibrated confidence and abstention.**
   - PACE-LM (Microsoft, arXiv 2309.05833) estimates confidence in two stages: groundedness in
     retrieved history, then a rating of the predicted cause against historical references
     [S].
   - A pre-registered 2026 study (risk-adjusted-abstention, GitHub) found that **cost-derived
     abstention thresholds were 39.6–45% worse than a fixed tuned constant**, and the optimal
     gate on RCAEval was "never abstain" (a benchmark artifact) [S].
   - Lesson: calibrate thresholds empirically on replayed pg_sage incidents, not from theory,
     and report risk–coverage curves.
   - Google escalates immediately when a root cause cannot be identified or the situation is
     outside safe bounds [S].
8. **Counterfactual and temporal checks.**
   - A candidate cause must **precede** the symptom (change-point ordering). Timestamp errors
     appear in 23.3% of OpenRCA runs [V].
   - Where possible it must be **sufficient**: a what-if check. HypoPG for index hypotheses and
     EXPLAIN (not ANALYZE) of the regressed query under the old and new plan are cheap,
     side-effect-free counterfactuals. D-Bot uses HypoPG [V].
9. **Verify then act, and verify after acting.**
   - Before acting, re-check preconditions against live state, because telemetry can be stale.
   - After acting, run the scenario's post-condition predicate.
   - DBA-Bench's "unclosed verification loop" is a top failure mode [V]. The pg_sage `verify`
     package is the natural home.
10. **Untrusted text is data.** Query text, `application_name`, log lines, comments, and table
    or column names are attacker-influenced (see §5.8). They must never be read as
    instructions or be the source of action parameters.

---

## 3. Classic SRE methods to embed

### 3.1 SLIs and SLOs for databases

Suggested SLIs (UNVERIFIED as a canonical list; derived from SRE practice):

- **Availability**: successful connections and queries ÷ attempts, measured from the client or
  pooler.
- **Latency**: p95/p99 of `pg_stat_statements` per-query mean is only an approximation.
  Prefer pooler- or app-side histograms.
- **Freshness**: replica replay lag in seconds.
- **Durability/recoverability**: seconds since last successful WAL archive, and
  `failed_count` delta in `pg_stat_archiver`.
- **Correctness headroom**: XID and MXID age as % of the limit.
- **Capacity**: connections used ÷ usable, and disk free.

Error budgets and an error-budget policy follow the SRE workbook
(<https://sre.google/workbook/error-budget-policy/>). The AI SRE should **gate autonomous
actions on remaining error budget**, as Google's real-time risk evaluation does [S].

### 3.2 Multi-window, multi-burn-rate alerting

From the SRE Workbook, "Alerting on SLOs", Table 5-8, for a 99.9% SLO [V]:

| Severity | Long window | Short window | Burn rate | Budget consumed |
|---|---|---|---|---|
| Page | 1 h | 5 min | 14.4 | 2% |
| Page | 6 h | 30 min | 6 | 5% |
| Ticket | 3 d | 6 h | 1 | 10% |

- Alerting approaches should be judged on **precision, recall, detection time and reset time**
  [V].
- For low-traffic services, generate synthetic traffic [V]. pg_sage can issue a synthetic probe
  query per database.

### 3.3 Anomaly detection suited to DB metrics

- **Seasonal baselines.** GitLab's Prometheus approach uses z-scores against multi-week
  same-hour offsets, with short-term, long-term and margin bands (2019) [S]. Grafana's
  `promql-anomaly-detection` framework uses the same band composition [S].
- **Robust statistics.** For heavy-tailed DB metrics (lock waits, temp bytes), use median/MAD
  and EWMA rather than mean/σ. UNVERIFIED as benchmarked for PostgreSQL specifically.
- **Change-point detection.**
  - BOCPD (Adams & MacKay 2007, arXiv 0710.3742) keeps a run-length posterior [S].
  - BARO applies **multivariate BOCPD** to RCA (arXiv 2405.09330) and did well on RCAEval's
    resource faults but poorly on network faults [S].
  - CUSUM is cheap and is tuned by average run length (false alarms) against expected detection
    delay [S].
  - Use change points to **timestamp** onset, which feeds temporal-precedence checks (§2.8).
- **Simple baselines win often.** NSigma, BARO and CIRCA perform well with short data once the
  failure time is known [S]. "Largest anomaly" scores 75.8% on RCAEval [S].

### 3.4 Change and deploy correlation

- About **70% of outages are due to changes in a live system** (SRE book, Introduction) [S].
- Meta's approach (retrieve candidate changes by heuristics, then rank with an LLM) reached 42%
  top-5 [S].
- The Postgres change sources pg_sage should ingest:
  - DDL: event triggers or log `log_statement=ddl`.
  - GUC changes: `pg_settings` diff and `pg_file_settings`.
  - Extension versions.
  - Planner statistics refresh: `last_analyze` jumps.
  - New queryids in `pg_stat_statements`.
  - Role and pooler config.
  - Cloud maintenance events.
  - pg_sage's own executor actions. This one matters: **the agent is a change source too.**

### 3.5 Causal inference over metric graphs

- **Evidence against generic causal discovery.** Pham et al., "RCA for Microservice System
  based on Causal Inference: How Far Are We?" (ASE 2024; arXiv 2408.13729) found PC, FCI,
  Granger, LiNGAM, fGES and random-walk methods "mostly perform similarly to Dummy" [S].
- **Prefer a hand-built causal model of Postgres internals.** Knowledge-based structure works
  better: CIRCA builds its graph from operator knowledge [S]. Causely uses a Bayesian causality
  graph and a "codebook" of symptom signatures per root cause over the discovered topology [S].
- **pg_sage's advantage.** Postgres has a small, well-known causal structure, for example:
  - long transaction → xmin horizon pinned → dead tuples → bloat and autovacuum churn →
    latency;
  - slot inactive → WAL retention → disk.

  Encode this as a static causal DAG with symptom signatures. Do not learn it from data.

### 3.6 Alert grouping and dedup

- COLA (ICSE-SEIP 2024; arXiv 2403.06485) combines correlation mining (temporal plus spatial)
  with LLM reasoning for ambiguous cases, reaching F1 0.901–0.930 [S].
- iPACK (ICSE 2023) aggregates duplicate tickets [S].
- For a fleet, group by `(cluster, primary/replica topology, time window, causal-DAG ancestor)`.
  Inhibit child symptoms when the ancestor fires (Alertmanager-style inhibition).

### 3.7 Toil, severity, postmortems, runbooks, game days

- **Toil** is manual, repetitive, automatable, tactical, with no enduring value, and scaling
  linearly (<https://sre.google/sre-book/eliminating-toil/>). An AI SRE should *measure* the
  toil it removes (actions per week by class, human minutes saved), not just claim it.
- **Severity model.** Use a SEV1–SEV4 scale keyed to SLO impact and data-risk. The data-risk
  axis is Postgres-specific: wraparound proximity or archive failure is a durability SEV even
  at zero user impact. UNVERIFIED as an industry standard; this is a recommendation.
- **Blameless postmortems** (<https://sre.google/sre-book/postmortem-culture/>). Generate them
  from the evidence ledger: timeline, detection, diagnosis path including discarded
  hypotheses, actions and their verification, and follow-ups.
- **Runbook automation.** Compile runbooks to typed DAGs (StepFly) with preconditions and
  postconditions and SOP-constrained ordering (Flow-of-Action).
- **Game days.** Periodically inject Postgres faults (§6.2) in staging and score the agent.
  Google's "Silver/Gold" data tiers and nightly evaluations are the model [S].

---

## 4. PostgreSQL incident taxonomy

Views and GUCs are standard PostgreSQL. Version notes are marked. "Safe first actions" are
read-only or narrowly scoped and reversible. "Dangerous actions" should be forbidden to the
agent or need two-person approval.

| # | Incident | Detection signals | Typical root causes | Safe first actions | Dangerous actions |
|---|---|---|---|---|---|
| 1 | **Connection exhaustion** | `count(*)` in `pg_stat_activity` vs `max_connections − superuser_reserved_connections` (and `reserved_connections`, PG16+). Log `FATAL: sorry, too many clients already` / `remaining connection slots are reserved`. Many sessions with `state='idle in transaction'`. | App pool misconfiguration or leaks, retry storms, missing pooler, slow queries holding connections. | Attribute by `usename/application_name/client_addr`. Terminate **only** idle-in-transaction sessions older than N minutes from the offending client (`pg_terminate_backend`, scoped). Set per-role `idle_in_transaction_session_timeout`. | Raising `max_connections` (needs restart and multiplies memory). Mass termination. Restarting the server. |
| 2 | **Lock contention / blocking chains** | `wait_event_type='Lock'`. `pg_blocking_pids()` gives the chain head. `log_lock_waits`. Deadlock log lines. | DDL (ACCESS EXCLUSIVE) queued behind a long transaction, which then blocks everyone behind it in the lock queue. Hot-row updates. FK checks. | Find the **root blocker**. If it is a DDL or idle-in-transaction session, `pg_cancel_backend` it before considering terminate. Recommend `lock_timeout` for migrations. | Killing waiters instead of the root. Terminating a large writer, which triggers a long rollback. Killing anti-wraparound autovacuum. |
| 3 | **Long transactions / xmin horizon pinned** | `pg_stat_activity.backend_xmin` age, `xact_start`. `pg_prepared_xacts`. `pg_replication_slots.xmin/catalog_xmin`. `pg_stat_replication.backend_xmin` (`hot_standby_feedback`). Rising `n_dead_tup` while vacuum runs "successfully". | Forgotten sessions, analytics on primary, orphaned 2PC, stale logical slot, standby feedback from a long replica query. | Identify **which of the four horizon holders** it is (session, prepared xact, slot, standby). Notify the owner. Cancel or terminate a session past policy. PG17+ `transaction_timeout` as a guard. | `ROLLBACK PREPARED` (2PC data semantics). Dropping a logical slot (breaks the CDC consumer). |
| 4 | **XID wraparound** | `age(datfrozenxid)`, `age(relfrozenxid)` vs `autovacuum_freeze_max_age` (200M default) and `vacuum_failsafe_age` (1.6B, PG14+). Log "must be vacuumed within N transactions". At about 3M remaining, XID-assigning commands are refused. | Horizon holders (#3), autovacuum starvation, huge tables, cancelled anti-wraparound vacuums. | Clear horizon holders **first**. Manual `VACUUM` (not FULL) on the oldest-`relfrozenxid` tables. Do not cancel "to prevent wraparound" autovacuums. | **Single-user mode is outdated advice.** It removes the safety guard on modern PG [S]. `VACUUM FULL`. Raising freeze ages to "buy time". |
| 5 | **MultiXact exhaustion** | `mxid_age(datminmxid)`. **Member space** (size of `pg_multixact/members`), which age monitoring does not show. Writes fail with "would create a multixact". | Many concurrent `SELECT … FOR SHARE/KEY SHARE`, FK checks on hot parents. O(n) multixacts with O(n²) members. | Aggressive vacuum of high-`mxid_age` tables. Find the lock-sharing workload. Monitor members, not only IDs. | Assuming <50% ID usage is safe. **Metronome, May 2025**: 4 outages with IDs under 50% [S]. PG19 widens `MultiXactOffset` to 64-bit [S]. Whether PG19 is GA as of today is UNVERIFIED. |
| 6 | **Replication lag / slot bloat** | `pg_stat_replication` `write/flush/replay_lag`. Replica `now() − pg_last_xact_replay_timestamp()`. `pg_replication_slots.active=false`, `wal_status` (`extended/unreserved/lost`), `safe_wal_size`. `pg_wal` size growth. | Replica I/O or CPU saturation, long replica queries with `max_standby_*_delay`, network, dead CDC consumer. | Classify as physical vs logical and active vs inactive. Alert on retained bytes. Recommend `max_slot_wal_keep_size` (PG13+) **and** `idle_replication_slot_timeout` (PG18). They complement each other [S]. | Dropping slots automatically. Failing over to a lagging replica. |
| 7 | **WAL archiving failure / disk full** | `pg_stat_archiver.failed_count` delta, `last_failed_wal/time`. Count of `.ready` files (`pg_ls_archive_statusdir()`). Disk-free trend (forecaster). | Bad credentials or bucket for `archive_command`/`archive_library`, destination full, slow archiver. Slots (#6). Temp files (#11). | Surface the archiver error text as quoted data. Page a human. Forecast time-to-full. Extend storage via cloud API if policy allows (see #16). | **Deleting files in `pg_wal`**, `pg_resetwal`, `archive_command='true'` (silently breaks PITR). |
| 8 | **Checkpoint storms** | `pg_stat_checkpointer` (PG17+; `num_requested` ≫ `num_timed`), earlier `pg_stat_bgwriter`. Log "checkpoints are occurring too frequently". I/O and latency sawtooth. | `max_wal_size` too small for write bursts, bulk loads, full-page-image amplification after checkpoints. | Raise `max_wal_size` (reload, reversible). `checkpoint_completion_target=0.9`. | `fsync=off`, `full_page_writes=off`, `synchronous_commit=off` globally. |
| 9 | **Autovacuum starvation** | `n_dead_tup`, `last_autovacuum` staleness, `pg_stat_progress_vacuum`, all workers busy. Cost-delay throttling. PG18: `autovacuum_worker_slots` vs `autovacuum_max_workers` (reloadable) [S]. | Too few workers, cost limit too low, huge tables with default scale factors, horizon pinned (#3). | Per-table `autovacuum_vacuum_scale_factor/threshold` (PG18 adds `autovacuum_vacuum_max_threshold`, UNVERIFIED name). Raise `autovacuum_vacuum_cost_limit` (reload). Manual VACUUM on a dedicated connection outside a transaction. | `VACUUM FULL` on hot tables. Disabling autovacuum. |
| 10 | **Plan regressions / plan flips** | Per-`queryid` step change in `mean_exec_time`/`shared_blks_read` (change-point). `auto_explain` plan hash changes. Stats refresh (`last_analyze`) or GUC change just before. | Stale or skewed statistics, generic vs custom plan switch (`plan_cache_mode`), dropped or invalid index, data growth crossing a cost threshold, upgrade losing stats. | `ANALYZE` the involved tables (cheap). HypoPG or EXPLAIN counterfactual. Per-role/function `plan_cache_mode`. **PG19 `pg_plan_advice`** contrib (committed 2026-03-12) to pin a known-good plan [S]. | Global `enable_*=off`. Dropping or creating indexes non-concurrently. |
| 11 | **OOM / `work_mem` blowups** | Log `server process (PID n) was terminated by signal 9` then crash-recovery restart. Host OOM-killer logs. `pg_backend_memory_contexts`. Cloud FreeableMemory. | `work_mem` × sort/hash nodes × parallel workers × connections. `hash_mem_multiplier`. Memory-heavy extensions. | Lower `work_mem` per role or query class. Cap parallel workers. Recommend `vm.overcommit_memory=2` on self-managed hosts. | Raising `work_mem` globally. Auto-restart loops. |
| 12 | **Temp-file explosions** | `pg_stat_database.temp_bytes` rate. `log_temp_files`. `pg_stat_statements.temp_blks_written` per query. Disk-free drop. | Spilling sorts/hashes, missing indexes, cartesian joins. | Cancel the single runaway query (by PID and queryid). Set `temp_file_limit`. Tune that query. | Raising `work_mem` fleet-wide as a "fix". |
| 13 | **PgBouncer saturation** | `SHOW POOLS`: `cl_waiting>0` sustained, `maxwait>1s`. `SHOW STATS` wait time [S]. | Pool smaller than concurrency, slow queries holding server connections, session-mode pinning. | Find what holds server connections (slow queries, #2, #3). Temporary `pool_size` bump only within `max_connections` headroom. | Switching `pool_mode` live (prepared-statement breakage). Pool size above backend capacity. |
| 14 | **Failover events** | `pg_is_in_recovery()` flips, timeline change, cloud event stream, client error spikes. | HA manager decision, host failure, maintenance. | Re-discover topology. Verify replicas re-attached and slots present on the new primary (PG17 `sync_replication_slots` for failover slots). Re-baseline anomaly detectors, whose cold caches cause false alarms. | **The agent initiating failover or promotion** (split-brain risk). Leave that to the HA manager or a human. |
| 15 | **Extension / upgrade issues** | `pg_extension.extversion` vs `pg_available_extensions.default_version`. Post-upgrade plan regressions (missing stats). Deprecated GUCs. | Version skew across the fleet, missing `ANALYZE` after `pg_upgrade`. PG18's `pg_upgrade` retains planner stats (UNVERIFIED detail). | `ANALYZE` / `vacuumdb --analyze-in-stages`. Report skew. | `ALTER EXTENSION … UPDATE` or `DROP EXTENSION` by the agent. |
| 16 | **Sequence exhaustion** | `pg_sequences.last_value / max_value`. Also `int4` columns fed by bigint sequences. Forecast time-to-exhaustion. | `int4` serial primary keys on high-insert tables. | Early warning (months ahead). Draft a migration plan to bigint for humans. | `setval` backwards, `CYCLE`, changing a column type on a live hot table. |
| 17 | **LWLock contention** | Sampled `wait_event_type='LWLock'` with `wait_event` = `LockManager`, `SubtransSLRU`/`SubtransBuffer`, `MultiXactOffsetSLRU`, `WALWrite`, `BufferMapping`. | **LockManager**: queries touching many relations or partitions exceed the 16 fast-path slots (PG ≤17). PG18 scales fast-path slots with `max_locks_per_transaction` [S]. **SubtransSLRU**: more than 64 subtransactions per transaction (suboverflow), SAVEPOINT-heavy ORMs; GitLab removed subtransactions in 2021 [S]. PG17 makes SLRU buffers configurable (`subtransaction_buffers` etc.) [S]. | Attribute to the queryid or ORM pattern. Recommend partition pruning, fewer indexes per hot table, removing savepoints. On PG18, raise `max_locks_per_transaction` (restart). | Restart as the "fix". Blindly killing sessions. |
| 18 | **Bloat** | `pgstattuple` or estimates. Table/index size vs live rows. Index-only-scan heap fetches. | #3 and #9 historically. Update-heavy tables without HOT (fillfactor). | Schedule online `pg_repack`/`pg_squeeze`. PG19 adds concurrent repack (InfoQ headline: "Concurrent Table Repacking") [S]; exact command UNVERIFIED. `REINDEX CONCURRENTLY` for indexes. | `VACUUM FULL` / `CLUSTER` (ACCESS EXCLUSIVE) on production tables. |

### Cloud-provider specifics

- **RDS storage autoscaling** triggers when free space is ≤10% for at least 5 minutes, with at
  most 4 modifications per 24h and a **6-hour cooldown** (or until storage optimization
  finishes) [S]. The AI SRE must forecast *past* the cooldown: a second fill inside 6h is not
  rescued.
- **EBS gp2 `BurstBalance`**: I/O credits deplete toward baseline IOPS [S]. Latency incidents
  with healthy DB internals often trace to credit exhaustion. T-class CPU credits are the
  analog.
- **Aurora `IO:XactSync`** is an Aurora-only commit-acknowledgement wait event. It is the key
  write-saturation signal, often fixed by batching commits [S]. Aurora storage auto-grows, so
  disk-full looks different there. **Freeable memory** exhaustion leads to OS-level kill and
  restart [S].
- **Cloud SQL automatic storage increase** checks every 30 seconds and grows in 5–25 GB steps
  up to 64 TB. **Increases are permanent and cannot be reduced** [S]. An agent that "fixes"
  disk-full by growing storage creates an irreversible cost change. Treat it as
  non-reversible and require approval.

---

## 5. Agent safety patterns for production action

Primary sources: Google, "AI in SRE" (sre.google) [S]; DBA-Bench [V]; Bilal et al. survey
(arXiv 2605.12729) [S]; OWASP Top 10 for Agentic Applications (2026) [S]; Google, *Building
Secure and Reliable Systems* ch. 5 [S].

1. **Autonomy levels mapped to the trust ramp.**
   - Google L0–L4: Manual; Assisted (automation monitors and investigates); Partial (acts after
     explicit approval); High (bounded scenarios autonomous); Full [S].
   - Promotion only after "sustained success against human-verified evaluation data" [S].
   - **Real-time downgrade**: an L3 request is intercepted and turned into an approval request
     if the risk score is elevated or production state is anomalous [S].
   - This maps onto pg_sage's existing trust ramp and `autonomy`/`policy` packages.
2. **Repair contract, not a final filter.** Each proposed action carries:
   - preconditions, affected objects, evidence-supported scope;
   - lock impact and reversibility;
   - rollback conditions and expected state transitions;
   - a post-action verifier.

   Source: DBA-Bench [V]. 80% of unsafe repairs were scope or safeguard failures [V], so
   **scope must be derived from evidence** (specific PIDs, OIDs, queryids), never from free
   text.
3. **Dry-run everywhere.** Google requires `dry_run=true` on all production-affecting systems
   [S]. Postgres equivalents:
   - `EXPLAIN` without ANALYZE;
   - HypoPG;
   - `BEGIN; …; ROLLBACK` only for transactional DDL, and never for CONCURRENTLY or VACUUM;
   - render and store the exact SQL plus the target set (for example the PIDs a terminate
     would hit) before approval.
4. **Zero-trust actuation.** Agents call a **curated action registry**, not raw SQL or scripts
   [S]. OWASP "least agency": minimum tools, least-privilege credentials, allowlists, policy
   enforcement per call [S]. The agent's DB role should be a distinct, audited principal ("no
   ambient access") [S].
5. **Blast-radius limits and fleet canarying.**
   - Per-action caps: maximum sessions terminated, maximum tables touched, maximum GUCs
     changed.
   - Fleet caps: at most N instances per hour. Apply first to a canary instance, verify, then
     widen. Standard progressive rollout (SRE workbook, "Canarying releases").
   - Concurrent-action detection: no two actions on the same instance or object at once [S].
6. **Two-person rule / multi-party authorization.** MPA for actions against a small,
   well-defined API lets approvers know exactly what they authorize. Broad MPA is a breakglass
   [S]. Use MPA for irreversible classes: Cloud SQL storage increase, slot drop, restart,
   failover, extension change.
7. **Time-boxed leases, circuit breakers, kill switch.**
   - Autonomy grants and GUC overrides expire (`ALTER ROLE … SET` with a scheduled revert).
   - Agent-specific rate limits and circuit breakers stop runaway loops [S].
   - A global "Red Button" pauses or revokes L3 [S]. This maps to pg_sage's emergency stop,
     which must also be checked at actuation time, not only at planning time.
8. **Prompt injection via telemetry.**
   - AIOpsDoom (Pasquini et al., arXiv 2508.06394; USENIX Security '26) showed that attackers
     can **inject telemetry via error-inducing requests** so that agents take harmful actions.
     No prior knowledge of the target is required [S]. The widely repeated "90% success rate"
     figure is UNVERIFIED (not in the abstract).
   - The AIOpsShield defense sanitizes telemetry using its structure and the small share of
     user-generated content [S].
   - Postgres injection surfaces include:
     - `pg_stat_statements.query`, `pg_stat_activity.query` and `application_name`;
     - SQL comments;
     - object names and log messages that echo user input;
     - `RAISE` text and `pg_stat_archiver` error text.
   - Mitigations:
     - (a) Pass untrusted strings only inside delimited, typed fields.
     - (b) Normalize or truncate them. Prefer `queryid` over text.
     - (c) Take action parameters only from structured tool outputs (PID, OID, queryid), never
       from model prose.
     - (d) Never let the LLM choose an action outside the registry.
     - (e) Red-team with injected `application_name`, comments and log strings.
9. **Audit logs.** Keep an immutable per-action record: evidence IDs, hypotheses (including
   rejected ones), confidence, approver, the exact SQL, pre/post metrics, and the verifier
   result. Google stores every execution trace and uses it to generate golden data [S].
10. **Automatic rollback.** Rollback is only meaningful for reversible classes (GUC reload,
    per-role settings, `DROP INDEX CONCURRENTLY` of an agent-created index). The verifier must
    define a failure predicate and a time window. Irreversible classes, such as terminated
    sessions and storage growth, must be labeled as such in the registry.

---

## 6. How to evaluate an AI SRE

### 6.1 Offline replay of historical incidents

- **Snapshot evidence at detection time.** Freeze every tool response as of the incident start
  (or detection) and replay the agent against the recorded responses. This avoids hindsight
  leakage, for example post-fix metrics or postmortem text sitting in the retrieval store.
  UNVERIFIED as published methodology; it is standard ML hygiene. Meta evaluated "at
  investigation creation time" [S].
- **Gold labels** come from human-verified root cause, symptoms and correct action. Use
  Google's Gold/Silver/Bronze tiers, and harvest labels at incident closure by asking the
  on-call to accept, modify or reject the agent's suggestion [S].
- **Vendor-style quick check** (incident.io): 5–10 closed incidents, precision above 80% and
  recall around 60%, plus probes with non-existent services and runbooks to test whether the
  agent admits ignorance [S]. This is too small for statistics but a useful smoke test.

### 6.2 Fault-injection benchmark (reproduce Postgres faults in Docker)

Follow DBA-Bench's methodology [V]:

- Each scenario has a **database spec, workload spec and fault program**.
- The fault program has preconditions, an activation step, **manifestation predicates** (only
  start the run when the causal state and symptoms are present) and a **post-fix verifier**.
- Restore from a dirty snapshot before each run and discard the environment afterwards.
- Include **misleading-alert** and **composite** scenarios, plus SREGym-style **ambient noise**
  (transient, unrelated disturbances) [V].

Concrete Postgres fault programs (UNVERIFIED as a set; derived from the §4 taxonomy):

- idle-in-transaction holder;
- DDL behind a long transaction;
- inactive logical slot plus a write burst;
- broken `archive_command`;
- `max_wal_size` too small plus bulk load;
- stale stats after a bulk load (plan flip);
- a SAVEPOINT loop exceeding 64 subtransactions;
- a 200-partition fan-out query at high concurrency (LockManager);
- a `work_mem` bomb under a cgroup memory limit;
- a cartesian temp-file query with `temp_file_limit` unset;
- a PgBouncer pool smaller than concurrency;
- XID-age acceleration with `xid_wraparound`-style test tooling, or by consuming XIDs;
- a sequence near `max_value`;
- network latency and loss via Toxiproxy/Pumba/tc-netem [S];
- disk-full via a small tmpfs or loop device.

### 6.3 Metrics (report all, per category, with cost)

| Metric | Definition | Source precedent |
|---|---|---|
| **Safe Pass** (primary) | Outcome restored **and** zero safety violations | DBA-Bench [V] |
| Diagnosis Pass / Acc@1 / Acc@3 | Root-cause set matches gold (DBA-Bench uses F1 ≥ 0.8 plus no contradictions) | DBA-Bench, AIOpsLab, RCAEval |
| Precision at full recall | Penalizes symptom padding | ITBench-AA [S] |
| Evidence / reasoning score | Share of key evidence cited, causal-chain checkpoints hit | Cai et al. [S], Lu et al. trajectory eval [V] |
| Hallucination rate | Share of reports with an implausible or unsupported cause | ORCA-bench [V] |
| **False action rate** | Actions taken where the gold says "no action" or a different target | Recommended (UNVERIFIED as a named metric elsewhere) |
| Abstention rate plus selective accuracy | Risk–coverage curve, ECE/Brier on confidence | PACE-LM, abstention study [S] |
| TTD / TTM | Time to diagnose and to mitigate (wall clock) | SREGym [V], AIOpsLab |
| Cost per investigation | Tokens × price plus tool calls. DBA-Bench shows a 90× spread ($0.08 to $7.16) [V] | DBA-Bench |
| Consistency | Pass^k over repeated runs (ITBench-AA uses 3 repeats) | ITBench-AA [S], ReliabilityBench [S] |
| Noise robustness | Drop in diagnosis with vs without ambient noise | SREGym [V] |

Reporting rules:

- Report **per category and per instance type**. Pooled leaderboards hide winners and losers
  [S] (Hu et al.).
- Always include **trivial baselines**: largest-anomaly, the deterministic rules alone, and
  "always escalate".
- Include a **human-DBA reference** on a subset.
- Pre-register the promotion thresholds that move an action class from L1 → L2 → L3.

---

## 7. Implications for the pg_sage AI SRE spec (build on existing substrate)

1. **Deterministic first, LLM second.** Rules and forecaster detect. A static Postgres causal
   DAG with symptom signatures (§3.5) proposes hypotheses. The LLM ranks, explains, and fills
   gaps only when signatures are ambiguous. This follows RCACopilot, Causely and Pham et al.
2. **Evidence ledger in `rca`/`incidents`.** Every claim links to a stored tool result. Tool
   results are typed (ok/empty/error/no-privilege). The postmortem is rendered from the ledger.
3. **Hypothesis ledger with discriminating tests.** Each hypothesis carries predicted
   observations and a refutation query. The final report lists rejected hypotheses and why.
   This targets decoy anchoring and chain truncation.
4. **Runbooks as typed DAGs** (StepFly and Flow-of-Action) in `policy`/`cases`. They
   constrain tool order and carry a repair contract (§5.2).
5. **Separate verifier pass** (DiagGuard) before any action, and the `verify` package
   post-condition after it. Close the loop or auto-escalate.
6. **Autonomy.** Keep L1/L2 for anything not in a bounded, reversible, evidence-scoped
   registry class. Downgrade on error-budget burn, active failover, or concurrent actions.
   Irreversible classes (cloud storage growth, slot drop, restart, failover, extension change)
   are never autonomous.
7. **Telemetry-injection hardening.** Untrusted strings are fenced and typed. Action
   parameters come only from structured IDs. Red-team this in CI.
8. **Change feed.** Include pg_sage's own actions as first-class change events.
9. **Evaluation harness.** Docker fault programs with manifestation predicates and post-fix
   verifiers, noise and decoy variants, and replay of pg_sage's own historical incidents.
   Safe Pass is the headline metric, with the §6.3 metrics per category and cost. Promotion
   gates are pre-registered.
10. **Incident memory.** Retrieve similar past pg_sage incidents with their outcomes as
    in-context examples (Microsoft ICL: +49.7% over zero-shot [S]). Guard retrieval against
    leakage in evaluation.

---

## 8. Explicitly unverified items

- Whether PostgreSQL 19 is GA as of 2026-09-26. Beta 1 was on 2026-06-04 [S]. The 64-bit
  MultiXactOffset and `pg_plan_advice` are reported as PG19 features [S].
- The PG18 GUC name `autovacuum_vacuum_max_threshold`, and PG18 `pg_upgrade` retaining planner
  statistics (from memory).
- The exact PG19 concurrent-repack command syntax.
- AIOpsDoom's "90% success rate" (seen in a search snippet, not in the abstract).
- ITBench-AA scores are from the IBM/Hugging Face blog (primary author source), not
  independently re-run.
- No rigorous benchmark for LLM **postmortem generation** quality was found.
- No benchmark literally named "SRE-bench" was found.
- Flow-of-Action, StepFly, RCACopilot, COLA and RCAEval numbers are from abstracts or
  snippets [S], not full-text reads.
- The SLI list (§3.1), the severity model (§3.7), the fault-program list (§6.2) and the
  "false action rate" metric are this report's recommendations, not cited standards.
- SREGym: a secondary summary claimed PostgreSQL faults were included. **The full text
  mentions MongoDB but not PostgreSQL**, so that claim is rejected.

---

## 9. Sources

LLM/agent RCA and DB diagnosis
- D-Bot, VLDB 2024: <https://www.vldb.org/pvldb/vol17/p2514-li.pdf>; arXiv
  <https://arxiv.org/abs/2312.01454>; LLM As DBA <https://arxiv.org/abs/2308.05481>
- Panda (CIDR 2024): <https://www.cidrdb.org/cidr2024/papers/p6-singh.pdf>
- RCACopilot (EuroSys 2024): <https://arxiv.org/abs/2305.15778>
- In-context RCA with GPT-4: <https://arxiv.org/abs/2401.13810>
- Ahmed et al. ICSE 2023: <https://arxiv.org/abs/2301.03797>
- RCAgent (CIKM 2024): <https://arxiv.org/abs/2310.16340>
- mABC (EMNLP 2024 Findings): <https://aclanthology.org/2024.findings-emnlp.232/>
- Flow-of-Action (WWW 2025): <https://arxiv.org/abs/2502.08224>
- Nissist: <https://arxiv.org/abs/2402.17531>
- StepFly (FSE 2026): <https://arxiv.org/abs/2510.10074>
- DBAIOps: <https://arxiv.org/abs/2508.01136>
- Meta RCA blog:
  <https://engineering.fb.com/2024/06/24/data-infrastructure/leveraging-ai-for-efficient-incident-response/>
- PACE-LM: <https://arxiv.org/abs/2309.05833>
- Risk-adjusted abstention study: <https://github.com/KarthikeyaMogulluri/risk-adjusted-abstention>
- Kim et al., "Why Do AI Agents Systematically Fail at Cloud RCA?": <https://arxiv.org/abs/2602.09937>
- Lu et al., trajectory-level RCA / DiagGuard: <https://arxiv.org/abs/2608.21310>

Benchmarks
- OpenRCA (ICLR 2025): <https://github.com/microsoft/OpenRCA>,
  <https://iclr.cc/virtual/2025/poster/32093>
- ITBench: <https://arxiv.org/abs/2502.05352>; ITBench-AA:
  <https://huggingface.co/blog/ibm-research/itbench-aa>
- AIOpsLab: <https://arxiv.org/abs/2501.06706>
- SREGym: <https://arxiv.org/abs/2605.07161>
- ORCA-bench: <https://arxiv.org/abs/2607.28545>
- DBA-Bench: <https://arxiv.org/abs/2607.22165>, code <https://github.com/TanJI-C/DBA-Bench>
- RCAEval: <https://arxiv.org/abs/2412.17015>
- AIOps2025/RCA100 multi-dataset benchmark: <https://arxiv.org/html/2606.29193v1>
- Pooled-leaderboard audit: <https://arxiv.org/abs/2606.29159>
- SRE-skills-bench: <https://github.com/Rootly-AI-Labs/SRE-skills-bench>;
  ReliabilityBench: <https://arxiv.org/html/2601.06112>

SRE methods
- SRE Workbook, Alerting on SLOs: <https://sre.google/workbook/alerting-on-slos/>
- Error budget policy: <https://sre.google/workbook/error-budget-policy/>
- SRE book intro (70% changes): <https://sre.google/sre-book/introduction/>
- Toil: <https://sre.google/sre-book/eliminating-toil/>;
  Postmortems: <https://sre.google/sre-book/postmortem-culture/>
- Canarying: <https://sre.google/workbook/canarying-releases/>
- Google "AI in SRE":
  <https://sre.google/resources/practices-and-processes/ai-engineering-reliable-operations/>
- GitLab anomaly detection: <https://about.gitlab.com/blog/anomaly-detection-using-prometheus/>;
  Grafana framework <https://github.com/grafana/promql-anomaly-detection>
- BOCPD: <https://arxiv.org/abs/0710.3742>; BARO: <https://arxiv.org/abs/2405.09330>
- Causal-inference RCA "How far are we?": <https://arxiv.org/abs/2408.13729>
- Causely: <https://docs.causely.ai/getting-started/how-causely-works/>
- COLA alert aggregation: <https://arxiv.org/abs/2403.06485>

PostgreSQL and cloud
- GitLab subtransactions:
  <https://about.gitlab.com/blog/why-we-spent-the-last-month-eliminating-postgresql-subtransactions/>
- PG17 SLRU sizes: <https://pganalyze.com/blog/5mins-postgres-17-configurable-slru-cache>
- Fast-path locking / PG18: <https://postgres.ai/blog/20251008-postgres-marathon-2-004>;
  <https://pganalyze.com/blog/5mins-postgres-LWLock-lock-manager-contention>
- Metronome MultiXact RCA:
  <https://metronome.com/blog/root-cause-analysis-postgresql-multixact-member-exhaustion-incidents-may-2025>
- PG19 64-bit MultiXactOffset:
  <https://www.bytebase.com/blog/postgres-19-feature-preview-64bit-multixactoffset/>
- pg_plan_advice:
  <https://www.depesz.com/2026/03/22/waiting-for-postgresql-19-add-pg_plan_advice-contrib-module/>
- PG19 beta / repack: <https://www.infoq.com/news/2026/06/postgresql-19-graph-queries/>
- PG18 release notes: <https://www.postgresql.org/docs/release/18.0/>
- idle_replication_slot_timeout:
  <https://thebuild.com/blog/all-your-gucs-in-a-row-idle_replication_slot_timeout/>
- Wraparound / single-user mode: <https://www.cybertec-postgresql.com/en/database-is-not-accepting-commands/>
- PgBouncer runbook: <https://runbooks.gitlab.com/pgbouncer/pgbouncer-connections/>
- RDS storage autoscaling:
  <https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html>
- gp2 burst: <https://aws.amazon.com/blogs/database/understanding-burst-vs-baseline-performance-with-amazon-rds-and-gp2/>
- Aurora IO:XactSync: <https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/apg-waits.xactsync.html>
- Cloud SQL storage: <https://cloud.google.com/sql/docs/postgres/storage-options-overview>,
  <https://cloud.google.com/sql/docs/mysql/instance-settings>

Safety
- AIOpsDoom / AIOpsShield: <https://arxiv.org/abs/2508.06394>;
  <https://www.usenix.org/conference/usenixsecurity26/presentation/pasquini>
- Agentic NetOps/AIOps safety survey: <https://arxiv.org/abs/2605.12729>
- OWASP Top 10 Agentic (2026): <https://www.promptfoo.dev/docs/red-team/owasp-agentic-ai/>
- Building Secure and Reliable Systems ch. 5:
  <https://google.github.io/building-secure-and-reliable-systems/raw/ch05.html>
- Toxiproxy/Pumba chaos:
  <https://oneuptime.com/blog/post/2026-02-08-how-to-use-docker-for-chaos-engineering-with-toxiproxy/view>
