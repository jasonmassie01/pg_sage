# Sage SRE — AI-driven SRE for PostgreSQL fleets (build spec)

**Status:** proposed. **Date:** 2026-09-26. **Baseline:** pg_sage v1.5.0 + the fixes on branch
`claude/full-review-ai-sre-2026-09-26`.
**Lineage:** Codex's *Sage Incident Investigator* spec (`codex/ai-sre-spec.md` +
`codex/ai-sre-contracts.md`) is adopted as the **R1 core**, nearly verbatim in scope, data model
and acceptance checks. This document adds positioning, the Postgres causal graph, typed runbooks,
per-family earned autonomy, the MCP "specialist" surface, proactive (pre-incident) detection,
SLO burn-rate, evidence-first postmortems and an open benchmark. Research:
`research-ai-sre-market.md`, `research-ai-sre-prior-art.md`, `research-ai-sre-substrate.md`,
`codex/sre-research.md`. All numeric targets are proposals, not measurements.

---

## 1. Why this, why now

- **White space.** Every Postgres-specific AI tool stops at read/propose (pganalyze MCP,
  postgresai, pgEdge, Azure PG autonomous tuning in REPORT). The only Postgres tools that act are
  DBtune (GUCs only) and Google's preview DB agent (GCP-only, approval-gated). Horizontal AI SREs
  (Datadog Bits, Resolve, Azure SRE Agent, AWS DevOps Agent, Komodor) act only with generic moves:
  restart, scale, rollback, PR. **Nobody ships a Postgres-semantic action vocabulary with
  verification.** pg_sage already has one.
- **Vacuum.** Xata Agent, the one recognizable open-source Postgres SRE agent, was archived
  2026-06-15.
- **Category legitimacy.** Gartner's first AI SRE market guide came out Jan 2026. All three
  hyperscalers ship SRE agents, and all accept MCP tools.
- **The honesty gap is the opportunity.** DBA-Bench (Jul 2026, 106 live PostgreSQL scenarios)
  scores frontier agents at **17.9% Safe Pass vs 93.4% for a human DBA**. 62% of correct
  diagnoses still failed to repair; 37% of repairs violated a safety rule, and 80% of those
  violations were *over-scoped* actions, not obviously destructive ones. Practitioners distrust
  agents with write access (Replit 2025, PocketOS 2026). A product that measures itself
  publicly, abstains when unsure, and earns authority per incident family is differentiated.

**Positioning:** *The open-source Postgres responder your AI SRE calls: self-hosted,
deterministic detection, measured accuracy, and a reversible executor the LLM can't bypass.*

**Explicit non-goals:** cross-service microservice RCA, Kubernetes, on-call scheduling, incident
coordination. Integrate with those tools via webhooks, PagerDuty/Slack and MCP; don't compete.

---

## 2. Principles (non-negotiable)

1. **Deterministic first.** Detection, paging and the canonical SLI never depend on the LLM.
   With the LLM off, Sage SRE still produces evidence packets and deterministic hypotheses.
2. **Catalog-only tools.** The model picks probes from a versioned, fixed catalog with typed
   arguments. It never emits SQL, never grants permissions, never computes numeric facts
   (numbers are computed by code and bound as typed evidence).
3. **Every claim cites evidence.** Claims reference immutable evidence IDs in scope. Unresolvable
   references are rejected before display. Tool results are typed `ok | empty | error |
   no_privilege | unsupported`, so an error is never read as "healthy" (the #1 prior-art
   failure mode: misreading data, 71%).
4. **Competing hypotheses + falsification.** Each hypothesis carries predicted observations and a
   refutation probe. Output keeps root cause, symptoms and contributing factors separate, and
   lists rejected hypotheses with reasons.
5. **Abstention is a feature.** "Investigation complete, cause inconclusive" is a first-class,
   non-failure outcome.
6. **Authority lives outside the model.** Any action goes through the existing policy gate +
   executor with re-authorization at execution time, emergency stop, replica/HA gates, leases
   and approvals. Every action carries a **repair contract** (§7.2).
7. **Recovery is verified independently** of the action's success return, over fresh samples.
8. **Autonomy is earned per incident family** from replay + shadow evidence, never from
   elapsed time, and is automatically downgraded on error-budget burn, failover or evidence staleness.
9. **Untrusted text is data.** Query text, comments, identifiers, `application_name`, log lines,
   archiver errors, notes and runbooks are fenced and redacted, and they never become
   instructions or action parameters. Action parameters come only from structured IDs
   (pid+backend_start, OID, queryid). (AIOpsDoom, USENIX Sec '26.)
10. **pg_sage's own actions are change events.** "Did we cause this?" is always a hypothesis.

---

## 3. Personas & jobs

| Persona | Job | Sage SRE output |
|---|---|---|
| App/platform engineer on call for several PG DBs, not a DBA | "Postgres is on fire. What is it, is it getting worse, what do I do?" | Evidence packet in < 2 min: impact, current state, likely cause + alternatives, next check, safe action (or manual script), recovery predicate |
| DBA | Reusable, trustworthy evidence; fewer 3am pages | Timeline, lock graphs, horizon holders, change correlation, exported packet, postmortem draft |
| External AI SRE (Datadog Bits, Azure SRE Agent, AWS DevOps Agent, PagerDuty, Claude Code) | "Is the problem in Postgres, and what's safe to do?" | MCP tools returning evidence-bundled diagnoses and policy-checked proposals; execution stays behind pg_sage's gate |
| Platform owner / security reviewer | Prove it can't do something stupid | Capability manifest, autonomy ledger per family, benchmark results, audit trail |

---

## 4. Scope and releases

### R0 — substrate repairs (prerequisites; most are done on this branch)
Incident durability and hydration (R04), database identity on incidents (R05), Tier-2
reachability, incident notifications, self-action family mapping, cache-hit units, jsonlog/csvlog
parsing, per-queryid aggregation, fleet retention, gate authority fixes (P0-04/05), e-stop fleet
latch (P0-14), MCP principal binding (SURF-01). See MASTER-SPEC §10 for status.
**Additional R0 items (not yet done):** write `query_store.plan_hash`; run lock-chain/RCA on the
collector tick (60 s) instead of the 600 s analyzer tick; add `ChatWithTools` to the LLM client.

### R1 — Investigator (read-only) — *Codex R1, adopted*
Persistent, bounded investigations for three families:
**(a) lock/transaction blocking, (b) connection pressure, (c) WAL/replication retention.**
Automatic start off by default; start from a Case or by operator. Competing hypotheses,
timeline, evidence links, supported-action *proposals* (not execution), recovery verification
after an operator reports an external intervention, and redacted export. **Additions to Codex R1:**
- **Postgres causal graph** (§6) for deterministic hypothesis generation.
- **Change feed v0:** pg_sage's own actions, config changes (`sage.config_audit`), DDL seen by the
  migration detector, `pg_stat_statements` resets, restarts/failovers (timeline id), extension
  version changes.
- **MCP read tools** (§9) so external agents can start and read investigations.
- **Evidence-first postmortem draft** (timeline + verified facts; LLM narrative labeled).

### R1.1 — Approved actions + SLOs — *Codex R1.1, extended*
- Opt-in, always-approved **cancel of an evidence-matched backend** (one backend/action, identity
  = pid + backend_start + database + user + query hash, ≤5 s evidence age, immediate recheck).
- **ChatOps approval** through the existing approval API (Slack/Telegram interactive approve with
  signed callbacks). The approver identity is the authenticated chat user mapped to a pg_sage user.
- **SLO/burn-rate** (§8), one Prometheus-compatible SLI connector, signed change-event ingestion
  (`POST /sre/change-events`: deploys, migrations, feature flags).

### R2 — Breadth + proactive
- Families: **autovacuum starvation / wraparound runway, plan regression (needs plan_hash),
  checkpoint storm, disk/WAL runway, temp-file explosion, sequence exhaustion, replication
  lag, LWLock contention** (taxonomy in prior-art §4).
- **Pre-incident investigations:** the forecaster opens an investigation when a runway crosses a
  horizon (time-to-wraparound, time-to-disk-full from WAL/slot growth, sequence runway, RDS
  autoscaling cooldown horizon). This is where autonomy is safest and most valuable.
- **Typed runbooks** (§7.1) with Xata-playbook import (English → typed DAG, reviewed by a human).
- Incident-library retrieval (similar past pg_sage incidents + verified outcomes as in-context
  examples; leakage-guarded in eval).
- Vector-quality incidents via Vector Lab evidence (Codex R2).

### R3 — Earned autonomy + fleet learning
- Per-family autonomy levels L0–L4 (§7.3) promoted only by the benchmark gate + shadow record.
- Fleet canarying of remediations (finish the rollout engine, R06) and cross-DB learning.
- **Game days:** fault injection on disposable clones (existing DLE clone provider) to exercise
  investigators against the customer's own schema, with results feeding family calibration.

**Excluded through R2** (Codex list, kept): arbitrary SQL, shell/K8s, failover/promotion,
restarts, `max_connections` changes, global GUC changes, slot drops, VACUUM FULL, index
rebuilds, app deploys, cloud provisioning, automatic PR publication, storage growth on providers
where it is irreversible (Cloud SQL). These may appear as reviewable *manual scripts* only.

---

## 5. Architecture

```
 collector(60s) ─┐          logwatch ─┐        providerobs ─┐     change feed ─┐   SLI connector ─┐
                 ▼                    ▼                     ▼                  ▼                  ▼
          ┌──────────────── typed trigger + evidence normalizer (freshness, epoch, identity) ────────────┐
          │  deterministic detectors: rca trees, lock chain, forecaster runways, burn-rate alerts       │
          └──────────────────────────────┬────────────────────────────────────────────────────────────────┘
                                         ▼ (bounded queue, coalesced by scoped fingerprint)
                    sre coordinator (durable investigation + lease/fence + budget reservation)
                                         ▼
     hypothesis engine: causal-graph matcher (deterministic) ⊕ optional LLM ranker/next-probe selector
                                         ▼
          probe registry (fixed, versioned, read-only SQL/API; typed results; caps) → immutable evidence
                                         ▼
          claim validator (refs in scope, fresh, typed) + verifier pass (DiagGuard-style challenge)
                                         ▼
     Cases panel / MCP / export  ─────► typed action proposal ─► existing policy gate ─► executor
                                         ▼
                           recovery evaluator (fresh samples, predicates) → timeline/ledger
                                         ▼
                     autonomy ledger per family ◄── benchmark + shadow results
```

**Code placement** (Codex table, extended):

| Area | Location | Change |
|---|---|---|
| Coordinator, worker, leases | new `internal/sre/{coordinator,worker}.go` | durable bounded work, stop/resume, fencing |
| Causal graph + matcher | new `internal/sre/causal/` | static DAG + symptom signatures (YAML embedded, versioned) |
| Hypotheses/claims | `internal/sre/{hypothesis,claims}.go` | structured facts vs inferences; refutation probes |
| Probes | `internal/sre/probes/` | fixed queries per PG major; typed results; row/byte/time caps |
| Runbooks | `internal/sre/runbook/` | typed DAG runbooks + repair contracts (R2) |
| Recovery | `internal/verify` + `internal/sre/recovery.go` | incident predicates separate from latency gain |
| LLM | `internal/llm` | `ChatWithTools` (OpenAI-compatible tool calling) + per-investigation budget |
| RCA | `internal/rca` | trigger adapter; fixed incident identity/hydration (done) |
| Cases/UI | `internal/cases`, `web/src/pages/CasesPage.jsx` | investigation panel |
| MCP | `internal/mcp` | read tools + proposal tools, principal-bound (§9) |
| Runtime | `cmd/pg_sage_sidecar` | constructed once per DB via the unified runtime constructor |
| Eval | new `sidecar/sre-bench/` + `e2e/sre/` | scenarios, fault programs, graders (§10) |

Rules: never call the LLM while holding `rca.Engine.mu` (fixed in R0). Investigation failures
can never block deterministic RCA, policy or e-stop. Coordination state lives in the meta DB when
configured. In single-DB installs it lives in the monitored DB, and that limitation is documented.
If metadata durability fails, Sage SRE degrades to read-only reporting and blocks action handoff.

---

## 6. The Postgres causal graph (deterministic hypothesis engine)

Prior art: generic causal discovery (PC/Granger/LiNGAM) performs near random on microservices,
and "pick the largest anomaly" scores 75.8% on RCAEval. So **hand-build the Postgres causal
model** (Causely-style): nodes are mechanisms, edges carry *symptom signatures* (which probes
should show what), and each hypothesis has a **discriminating probe**.

Excerpt (R1 families):

| Hypothesis (mechanism) | Predicted observations (support) | Refutation probe | Common confounders |
|---|---|---|---|
| Root blocker is an **idle-in-transaction session** | chain head `state='idle in transaction'`, `xact_start` old, waiters on same relation | `blocker_graph` shows head active or no shared object | long analytics query that is active, not idle |
| Root blocker is **DDL queued behind a long xact** (lock-queue amplification) | head = long xact; next = ACCESS EXCLUSIVE waiter; many waiters behind it | no AE waiter in queue | migration itself long-running (not waiting) |
| **Prepared transaction** holds locks/horizon | `pg_prepared_xacts` row old, locks by virtual xid | no prepared xacts | — |
| **Hot-row contention** | many `transactionid` waits on same tuple; short xacts | waits spread across many rows | FK-check contention |
| **Pool fan-out** (connection pressure) | many idle conns per `application_name`×`client_addr` proportional to replica count; no blockers | blockers present or active ratio high | traffic increase |
| **Blocked-query backlog** (connection pressure) | active conns waiting on locks; rising `wait_event_type='Lock'` | no lock waits | slow queries (not blocked) |
| **Connection leak** | monotonic growth of idle conns from one app since deploy X | growth flat or cyclical | autoscaling |
| **Inactive logical slot** retains WAL | `active=false`, `restart_lsn` stale, `wal_status` extended→unreserved, consumer last-seen old | slot active and advancing | write surge |
| **Slow consumer** | slot active, confirmed_flush advancing slower than WAL rate | lag stable/shrinking | network |
| **Archiver failure** | `pg_stat_archiver.failed_count` rising, `.ready` count rising | failed_count flat | slots |
| **Write surge** | WAL rate step change; slot lag grows only proportionally | WAL rate flat | bulk load (expected) |

The matcher scores hypotheses by signature fit (support − contradiction, weighted by evidence
freshness/strength), then picks the next probe with the highest expected discrimination among
the top-k. The LLM is optional. When enabled, it may re-rank, propose a next probe from the
catalog, and write the narrative. It may not add hypotheses outside the graph without labeling
them `unmodeled` (shown, never actioned).

---

## 7. Actions, runbooks, autonomy

### 7.1 Typed runbooks (R2)
A runbook is a versioned DAG: `trigger signature → probe steps → decision nodes → proposal`.
Steps can only reference catalog probes and typed actions. English playbooks (e.g. imported from
Xata) are compiled by the LLM into a *draft* DAG that a human reviews and signs. Unsigned
runbooks never run. (StepFly/Flow-of-Action show ordered, constrained tool use roughly doubles
success over free ReAct.)

### 7.2 Repair contract (every action class)
From DBA-Bench's failure analysis, where over-scoped actions caused 80% of unsafe repairs:
`preconditions` (fresh evidence predicates) · `scope derived from evidence IDs only` ·
`lock impact + timeouts` · `reversibility class` (reversible / mitigation-only / irreversible) ·
`rollback trigger + inverse` · `post-conditions` (recovery predicate) · `blast-radius budget` ·
`never-do list`. Contracts extend the existing `executor.ActionContract`; the gate checks them.

### 7.3 Autonomy levels (per incident family × action class)
| Level | Behavior | Promotion evidence |
|---|---|---|
| L0 | Evidence packet only | default |
| L1 | + hypotheses + proposal (manual script) | family ships |
| L2 | + one-click approval handoff | ≥ 90% factual precision and ≥ 80% top-1 on replay; 0 safety violations; 30-day shadow with ≥ 95% operator-accepted packets |
| L3 | auto-execute *reversible, bounded* actions in window; human notified | L2 metrics + ≥ 95% Safe Pass on family fault programs + ≥ 50 verified live L2 recoveries, 0 harmful |
| L4 | auto-execute + auto-rollback without window | reserved; not in R1–R3 |

**Automatic downgrade** to ≤ L1 while: an error budget is burning (fast window), failover in
progress or HA role unknown, evidence stale, concurrent pg_sage action on the same object, or any
family safety regression in the last N days. Irreversible classes are never above L1.

---

## 8. SLOs and burn-rate (R1.1)

- Canonical SLI `bad_events / eligible_events`, owned by a registered app SLI (Prometheus query or
  pushed counters). Only a registered app SLI earns a customer-impact claim. PG counters are
  proxies and are labeled that way.
- Multi-window, multi-burn-rate (Google SRE workbook defaults, adjustable): page at 14.4× over
  1h & 5m, or 6× over 6h & 30m; ticket at 1× over 3d & 6h. Low traffic / zero denominator /
  absent data = **unknown**.
- Database proxy SLIs when no app SLI exists: p95 latency of top-N queryids by total time,
  error-class log rate, connection-refusal rate, replication-lag budget.
- Error-budget state feeds autonomy downgrade (§7.3) and the recovery predicate.

---

## 9. API, MCP and UX

**REST** (Codex table adopted): `POST/GET /databases/{db}/investigations`, `GET …/{id}`,
`GET …/{id}/events`, `POST …/{id}/{stop|resume|notes|external-changes|proposals}`,
`GET …/{id}/export`, `POST /sre/change-events` (HMAC/mTLS). Idempotency keys; `If-Match`
versions; canonical error codes (permission, stale_evidence, missing_capability, budget,
expired, metadata_unavailable). Explicit database UUID in fleet writes.

**MCP (the distribution surface)**, principal-bound, per-tool role:
| Tool | Role | Returns |
|---|---|---|
| `sre.list_incidents(db?)` | viewer | active incidents/cases with severity, age, family |
| `sre.investigate(case_id \| trigger)` | operator | investigation id (async) |
| `sre.get_investigation(id)` | viewer | evidence-bundled summary: observed, likely, alternatives, missing, next check, recovery |
| `sre.get_evidence(id, evidence_id)` | viewer | typed, redacted evidence payload |
| `sre.propose_action(id, hypothesis_id)` | operator | typed proposal + repair contract + policy verdict (no execution) |
| `sre.request_execution(proposal_id)` | operator | queued for the existing approval flow; never executes directly |
| `sre.record_external_change(id, …)` | operator | attaches an external intervention and starts recovery verification |

The calling agent never holds database credentials. All authority stays in pg_sage.

**UX (Cases panel):** Impact & state → Observed → Likely explanation → Other explanations
(rejected with reasons) → Missing evidence → Next check → Proposed action (with repair contract
and policy verdict) → Recovery (predicate, samples, verdict). Each claim opens the exact evidence
sample with timestamp and freshness. Failed probes and unavailable telemetry are shown, not hidden.
"Inconclusive" is visually distinct from "resolved". Confidence numbers appear only for families
with calibrated confidence; otherwise evidence-strength labels are used.

---

## 10. Data model (Codex §5 adopted + additions)

Adopted: `sre_investigations`, `sre_evidence` (immutable, hashed, typed status),
`sre_hypotheses` (revisioned, support/contradiction refs), `sre_steps` (idempotent, attempt
identity), `sre_recovery_checks`, `sre_events` (append-only hash chain), `sre_service_slos`
(R1.1), `sre_change_events` (R1.1). All keyed by deployment + stable database UUID.
**Additions:** `sre_causal_model_versions` (graph version/hash pinned per investigation),
`sre_runbooks` (DAG, version, signer, status), `sre_family_autonomy` (family × action class
→ level, evidence refs, last change, actor), `sre_eval_runs` (benchmark results per build/model).
Retention: redacted evidence 30 d, timelines 90 d, audit per policy. Evidence referenced by an open
approval/recovery is pinned. Deletes leave tombstones.

---

## 11. Budgets & model contract

Codex ceilings adopted as initial caps: 1 probe/DB concurrently, 4/sidecar; 500 ms per probe
statement, 100 ms lock_timeout; 500 rows / 256 KiB per probe; 12 probes, 2 model turns, 16k in /
4k out tokens, 120 s wall per investigation; trigger queue 100/sidecar. Reservations are atomic
and survive crashes. Model output schema: `hypotheses[]` (graph node ids + support/contradiction
evidence ids + missing facts), `next_probe` (catalog id + typed args + discrimination rationale),
`conclusion` (observed facts, inference, limitations, next operator step). Validation rejects
unknown/out-of-scope/stale ids, unknown probes/args and oversized output, with one repair attempt.
Tool calling uses OpenAI-compatible `tools`/`tool_choice`, and falls back to JSON-schema prompting
for providers without tool support. Local/VPC models are first-class.

---

## 12. Evaluation — **PGIncidentBench** (open, reproducible)

**Why open:** no public Postgres incident benchmark with safety scoring exists apart from
DBA-Bench's research harness. Publishing one is both the quality gate and distribution.

- **Scenario sources:** (1) 60 redacted replay scenarios (Codex: 30 positive across R1 families,
  15 benign/confounded lookalikes, 15 missing-data/adversarial), frozen at detection time with
  no future leakage; (2) **Docker fault programs**, each with a *manifestation predicate* (fault is
  present before the run), a *post-fix verifier*, **decoy** and **background-noise** variants,
  and PG 14–18 matrix; (3) DBA-Bench-style composite faults (e.g., idle-in-tx blocker + migration +
  pool saturation).
- **Metrics (per family, never only pooled):** Safe Pass (headline), top-1/top-3 diagnosis,
  factual precision of claims, false-action rate, abstention rate + selective accuracy,
  time-to-first-evidence, time-to-mitigation, run-to-run consistency, probe load, tokens and
  $/investigation.
- **Baselines (always reported side by side):** deterministic-rules-only, causal-graph-only
  (LLM off), current RCA, "always escalate", human DBA panel (subset).
- **Graders:** deterministic predicates for facts/actions; two human reviewers for disputed
  narratives; LLM-as-judge only as a secondary aid.
- **Release gates:** R1 GA requires 0 forbidden tool/mutation/tenant leaks on the adversarial set;
  100% machine-resolvable claim refs; ≥ 90% factual precision; ≥ 80% top-1 when evidence is
  sufficient; ≥ 95% abstention on insufficient cases; p95 packet < 2 min. Pre-register thresholds;
  publish denominators and intervals.
- **Acceptance checks:** Codex CHECK-01..35 adopted verbatim, plus:
  - CHECK-36 causal-graph matcher alone reaches the R1 top-1 target on positive replay cases (LLM off).
  - CHECK-37 each hypothesis in a packet lists at least one refutation probe or `none_available`.
  - CHECK-38 pg_sage's own action within the window always appears as a hypothesis.
  - CHECK-39 MCP tools enforce role per tool on the real mounted router; `request_execution`
    creates exactly one approval item and never executes directly.
  - CHECK-40 autonomy auto-downgrades on burn, failover, stale evidence and concurrent action.
  - CHECK-41 postmortem export contains only evidence-backed statements; narrative is labeled.
  - CHECK-42 decoy/noise variants do not reduce top-1 by more than 10 points vs clean variants.

---

## 13. Rollout & milestones (indicative effort)

| Milestone | Content | Exit criteria | Size |
|---|---|---|---|
| M0 | R0 leftovers: plan_hash, 60 s lock-chain/RCA tick, `ChatWithTools`, unified runtime constructor for SRE | composed tests from collector→incident→notify pass in all modes | M |
| M1 | Store/lease/budget foundation + probe registry (R1 probes) + causal graph v1 | CHECK-13..16, 21, 24, 28, 31 | M |
| M2 | Investigator loop (deterministic) + Cases panel + export + MCP read tools | CHECK-01..09, 29, 30, 33, 36, 37, 38, 39 (read) | L |
| M3 | LLM ranker/next-probe + claim validator + verifier pass + PGIncidentBench v1 (R1 families) | CHECK-10..12, 42; R1 gates | M |
| M4 | R1 GA (read-only, opt-in auto-start) | all R1 checks; docs on permissions/data flow | S |
| M5 | R1.1: approved cancel, ChatOps approval, SLI connector, change events | CHECK-17..20, 22, 23, 32, 34, 40 | M |
| M6 | R2 families + pre-incident runways + runbooks + incident memory | per-family gates | L |
| M7 | R3 earned autonomy + game days + fleet canary | autonomy ledger + benchmark gates | L |

**Success measures in beta:** median active operator minutes from incident open to accepted
evidence and to recovery (sampled directly, not modeled), target −30% without worse p90 or any
harmful intervention; packet acceptance rate; unsupported-claim rate; abstention; verified
recovery rate; $/investigation.

---

## 14. Open questions for you

1. Is the MCP "specialist inside other AI SREs" channel the primary GTM, or is the pg_sage UI?
   It changes whether M2's MCP tools or the Cases panel come first.
2. Should PGIncidentBench live in this repo (AGPL) or a separate permissively-licensed repo to
   maximize adoption by other vendors?
3. Which LLMs must be supported for tool calling on day one (OpenAI-compatible only, or also
   native Anthropic/Gemini)? Local models (Ollama) as a first-class target?
4. Do you want pre-incident runways (R2) pulled forward? They are the safest autonomy story and
   reuse the forecaster (whose storage-growth path must be wired first).
5. Is a hosted "Sage SRE cloud" (metadata + benchmark dashboards, no customer data) in scope, or
   strictly self-hosted?
