# pg_sage — Agent-Native Autonomy: Build Specification

**Status:** Prescriptive build spec (implementable). **Date:** 2026-07-22.
**Audience:** an implementing engineer/agent (GPT 5.6). Build to this; where it says "PRESCRIBED," it is a decision, not an option.
**Companion research:** `research/2026-07-22-agent-native-feature-spec.md` (evidence, competitor mechanisms, sources). This document is the *how to build it*.

---

## 0. How to read this / build contract

- **Prescriptions are decisions.** Open questions from the research report are resolved here. Default numbers are chosen; change them only via config, not by re-litigating.
- **Concrete artifacts included:** new `sage.*` table DDL, Go interface signatures, config YAML additions, MCP tool JSON schemas, and load-bearing algorithms as pseudocode. GPT writes the Go implementation; these are the contracts it must satisfy.
- **Conventions (match the repo):** Go 1.24, `pgxpool` only, parameterized queries (`$1`), no `database/sql`, `log/slog`, errors wrapped `fmt.Errorf("ctx: %w", err)`, config via YAML+env+`sage.config`, no new heavy deps. Every I/O function takes `ctx` first. Every autonomous action routes through the policy gate (§3). No SQL string-concatenation of untrusted input; identifiers via `pgx.Identifier{}.Sanitize()` or the existing `sanitize` package.
- **Test standard:** follow `CLAUDE.md` — two-phase tests, `-count=1`, per-package coverage ≥70% business logic, adversarial cases mandatory. Every feature section ends with acceptance criteria that must become tests.
- **Migration discipline:** new `sage.*` tables are added as new numbered bootstrap migrations in `internal/schema`; never edit existing migrations.
- **Safety-first ordering:** Phase 0 (§13) also fixes the three trust-gate defects and the High-severity SSRF from the prior audit *before* any new rung-4 autonomy ships.

---

## 1. Prescriptions (the decisions)

1. **Build order is fixed:** F6 (policy/gate/ledger + audit fixes) → F1 (verify-and-revert index) → F3 + F4 (custodian guarantees) → clone enabler + F2 (rehearsed migration) → F5 (schema custodian) → F7 (fleet). §13 details phases.
2. **Value metric is the headline. PRESCRIBED:** the primary product metric is **DBA-hours saved** (§4), computed from *verified-successful* actions via a calibratable toil model, plus a separate, conservatively-credited **incidents-avoided** counter for the guarantees. Observability is a *supporting* concern, not the deliverable — telemetry lives behind an **Advanced view** (§5).
3. **Verify-and-revert is coupled to autonomy. PRESCRIBED:** the executor must refuse to *auto-apply* any change it cannot *auto-verify*. Apply and measurement are owned by the same component (Azure's lesson). No "fire and forget" autonomous change.
4. **Thin-clone substrate. PRESCRIBED:** a pluggable `clone.Provider` interface (§9) with two reference adapters: (a) **Database Lab Engine (DLE)** for self-managed/co-located Postgres — primary reference; (b) **provider snapshot-restore-to-scratch** for managed (RDS/Aurora/Cloud SQL/AlloyDB) — restore latest snapshot/PITR to an ephemeral instance, use, destroy. Neon-branch adapter is optional/future. Features that need a clone degrade to **rung-2 recommend** when no provider is configured.
5. **MCP is re-introduced as an intent-level surface. PRESCRIBED:** a new `internal/mcp` server exposing intent tools (§10), gated through the *same* policy path as every other action. Caller claims are untrusted input, never authorization. The retired REST-only stance is reversed for the agent-to-agent lane; the web UI is scoped to the **decision/evidence review + value** surface.
6. **Standing policy replaces approval. PRESCRIBED:** a declarative `sage.policy` object (§6) is the sole source of autonomous authority. Trust level + a few booleans is replaced/augmented by this object. Unknown classification → fail closed.
7. **Deadline-override is policy-opt-in, defaulted ON for the "unattended" profile. PRESCRIBED:** custodians may act *outside* the maintenance window only to prevent an XID/disk deadline, only if `policy.deadline_overrides` grants it. Two default profiles ship: `staffed` (overrides off, windows narrow) and `unattended` (overrides on, windows wide).
8. **Default thresholds (all config-overridable):** index verify window 2h adaptive [30m,72h]; min-gain G=20%; regression R=15%; write-impact W=20%; unused-index window 90d (never drop unique); XID red buffer = 25% of `autovacuum_freeze_max_age` remaining; slot abandonment window 24h + retained-WAL > 10% disk; retention first-run = dry-run always.

---

## 2. System architecture & package layout

New/changed packages (all under `sidecar/internal/` unless noted):

| Package | Feature | Responsibility |
|---|---|---|
| `policy` | F6 | Policy object load/validate, the single authorization gate, DDL change-leases, drift reconciliation |
| `ledger` | F6/§4 | Evidence Ledger writes + reads; **toil/value accounting** (DBA-hours-saved) |
| `verify` | F1 | Per-query/per-invariant verification + auto-revert engine (replaces the global-cache-hit rollback check) |
| `clone` | F2/enabler | `clone.Provider` interface + DLE and snapshot-restore adapters; rehearsal orchestration |
| `migrate` (extend `migration`) | F2 | Lint rules (Squawk-equivalent), expand/contract planner, online apply + bounded auto-revert |
| `custodian` | F3/F4 | Invariant enforcers: `custodian/freeze` (wraparound+bloat), `custodian/wal` (slot/WAL) |
| `schemaguard` | F5 | Agent-pathology invariant catalog + remediation |
| `mcp` | §10 | Intent-level MCP server; every tool → policy gate |
| `value` (or in `api`) | §4/§5 | DBA-hours-saved rollup endpoint + Advanced-view telemetry endpoints |

Existing packages changed: `executor` (route all actions through `policy` gate; use `verify` for post-apply), `optimizer` (add candidate-set selection; feed `verify`), `analyzer`/`forecaster` (forecaster predictions become *silent triggers* into custodians, not a deliverable), `api` (new endpoints), `config` (new sections), `schema` (new tables).

**Control flow (every autonomous action):**
```
trigger (analyzer/custodian/mcp/schedule)
  → build candidate + decision artifact
  → policy.Gate.Authorize(contract, ctx)   // §3 single gate, fail-closed
      ├─ blocked/park → ledger.RecordDecision(parked)   // durable, no action
      └─ granted → change-lease (if DDL) → executor.Apply
                     → verify.Watch(window)             // §7 verify-and-revert
                         ├─ pass → ledger.RecordSuccess + toil credit  // §4
                         └─ fail → executor.Revert + ledger.RecordRevert (0 credit)
```

---

## 3. The single authorization gate (F6 core)

**PRESCRIBED:** exactly one authorization path. Delete the dead `ShouldExecute`/`shouldExecute` in `executor/trust.go`. `EvaluateActionPolicy` is extended into `policy.Gate`.

```go
// package policy
type Gate interface {
    // Authorize is the ONLY path to an autonomous mutation.
    Authorize(ctx context.Context, req ActionRequest) Decision
}

type ActionRequest struct {
    Contract   executor.ActionContract // typed contract (existing), risk tier from SQL shape
    SQL        string                  // the exact statement (already ValidateExecutorSQL'd upstream)
    TargetObjs []string                // schema-qualified objects (for lease + blast radius)
    Feature    string                  // "index"|"freeze"|"wal"|"migration"|"schema"|"config"
    Deadline   *DeadlineContext        // non-nil for custodians: {kind:"xid"|"disk", urgency, hard_at}
}

type Decision struct {
    Verdict     string   // "execute"|"queue_approval"|"park"|"blocked"|"observe_only"
    RiskTier    string
    Reason      string
    Guardrails  []string // ENFORCED, not decorative (see fix below)
    OffWindowOK bool     // true only if deadline-override applies
    EvidenceID  string   // ledger correlation id
}
```

**Gate evaluation order (fail-closed at every step):**
1. Executor disabled / emergency-stop / replica (non-read-only) → `blocked`.
2. Contract missing or `ValidateExecutorSQL` fails → `park` with reason `no_typed_contract` (never execute unclassified SQL). **Protected schemas now include `sage` itself** (audit fix).
3. Trust level `observation` or exec mode `manual` → `observe_only`.
4. Policy lookup (§6). If policy absent/invalid → `blocked` (fail closed).
5. **Guardrail enforcement (audit fix):** if the contract's guardrails include `approval_required`, the verdict is `queue_approval` regardless of tier — the guardrail is now consulted, not copied-and-ignored.
6. Budget/blast-radius/rate-limit checks (§6). Exceeded → `park` (or `blocked`).
7. **Maintenance window (audit fix):** parse using a *correct* cron/friendly parser that honors day-of-week AND day-of-month (the current parser ignores fields 3 and 5 — replace it; add a reproduction test that `"0 2 * * 0"` is in-window only on Sundays). If outside window:
   - moderate/high action without deadline → `blocked` (wait for window).
   - custodian action with `Deadline` and `policy.deadline_overrides[kind]==true` → `execute` with `OffWindowOK=true`, loudly evidenced.
   - otherwise → `blocked`/`park`.
8. Tier ladder (existing `read_only`/`safe`/`moderate`/`high`) → `execute`/`queue_approval`.

**Reauthorize immediately before Exec** (the executor already does this; keep it — runtime safety changes must stop an in-flight decision).

**Change-lease (DDL serialization, multi-writer):**
```go
// Acquire before ANY DDL; blocks/queues conflicting DDL on overlapping objects.
func (g *gate) AcquireLease(ctx, actor string, objs []string, intent string) (LeaseID, error)
func (g *gate) ReleaseLease(ctx, LeaseID) error
```
Implementation: `pg_advisory_xact_lock` keyed by hash(object) for the critical section + a `sage.change_lease` ledger row (TTL-reclaimable; advisory locks auto-release on connection death). Overlapping in-flight lease → the later request is `park`ed with reason `ddl_conflict` (or queued if `policy.serialize_mode=queue`).

**Drift reconciliation:** a periodic job diffs live schema vs. `sage.schema_baseline`; additive/safe drift → `schemaguard` reconciles; destructive/ambiguous → `ledger.RecordDecision(parked, reason=drift_destructive)`.

---

## 4. Value model — DBA-hours saved (the headline metric)

**PRESCRIBED.** This is the product's primary number. It replaces "insight" as the deliverable.

### 4.1 Toil model (calibratable)

Replace the flat 15/30-min heuristic (`cases/shadow.go:estimatedToilForAction`, `executor.go:estimatedToilForActionType`) with a table-driven model.

```sql
CREATE TABLE sage.toil_model (
  action_type      text PRIMARY KEY,
  base_minutes     numeric NOT NULL,      -- human minutes to do this task manually, end to end
  notes            text,
  model_version    int NOT NULL DEFAULT 1,
  updated_at       timestamptz NOT NULL DEFAULT now()
);
-- Seeded defaults (PRESCRIBED starting values; tune via config/UI later):
--  analyze_table            15
--  create_index_concurrently 45   -- analysis + build + verify + cleanup
--  drop_unused_index        20
--  reindex_concurrently     30
--  vacuum_table             20
--  freeze_table             20
--  set_table_autovacuum     30
--  alter_system_guc         25
--  create_statistics        30
--  apply_query_hint         25
--  online_migration         120   -- expand/contract, rehearse, apply, verify
--  fk_supporting_index      30
--  retention_policy_setup   60
--  slot_bound / slot_drop   30
```

### 4.2 Incidents-avoided credit (separate, conservative, labeled)

Guarantees (F3/F4) prevent outages, not routine toil. Credit them separately so the two are never conflated.

```sql
CREATE TABLE sage.incident_avoided (
  id           bigserial PRIMARY KEY,
  database_id  int,
  kind         text NOT NULL,          -- 'xid_wraparound'|'disk_full_slot'|'lock_storm'
  severity     text NOT NULL,          -- 'near_miss'|'prevented'
  credited_minutes numeric NOT NULL,   -- CONSERVATIVE. default: near_miss=120, prevented=480
  evidence_id  text NOT NULL,          -- ledger correlation
  occurred_at  timestamptz NOT NULL DEFAULT now()
);
```
Credit is granted **only** when the invariant demonstrably crossed a danger threshold and the agent's action moved it back (e.g., a table crossed the XID red buffer and a FREEZE returned it below amber). Routine green-state maintenance is toil credit, not incident credit. Never double-count.

### 4.3 Saved-minutes stamping (honesty rules)

- Stamp `toil_minutes_saved` on the `sage.action_log` row **at verification time**, only when `outcome='success'` (verified). Add columns:
```sql
ALTER TABLE sage.action_log
  ADD COLUMN toil_minutes_saved numeric,
  ADD COLUMN toil_model_version int;
```
- **Reverted actions credit 0.** The agent did work, but the user's net toil saved is zero (optionally track "agent effort minutes" separately for internal metrics — never surface it as "saved").
- **Parked/recommended items are "pipeline," not "saved."** Report them under a distinct "potential savings" figure derived from the toil model, clearly separated from realized savings.
- **Recurring-task multiplier:** for actions that would otherwise recur (e.g., a per-table autovacuum tuning that eliminates a monthly manual VACUUM), credit `base_minutes` once per application; do not multiply speculatively. Recurrence value shows up naturally as the same action_type recurs over time.

### 4.4 Value rollup + endpoint

```sql
CREATE VIEW sage.value_rollup AS
SELECT
  date_trunc('day', executed_at) AS day,
  database_id,
  action_type,
  count(*) FILTER (WHERE outcome='success')          AS actions_verified,
  coalesce(sum(toil_minutes_saved),0)                AS toil_minutes_saved
FROM sage.action_log
GROUP BY 1,2,3;
```

`GET /api/v1/value` (authenticated) returns:
```json
{
  "dba_hours_saved": { "all_time": 412.5, "this_month": 63.2, "this_week": 14.8 },
  "by_feature":  { "index": 180.0, "vacuum_freeze": 90.5, "migration": 60.0, "schema": 52.0, "wal": 30.0 },
  "by_database": [ { "name":"orders", "hours": 120.4 }, ... ],
  "incidents_avoided": { "count": 7, "credited_hours": 22.0, "detail": [ {"kind":"xid_wraparound","when":"..."} ] },
  "potential_hours_pending": 38.0,          // parked/recommended, NOT counted as saved
  "trend_daily": [ { "day":"2026-07-01", "hours": 2.1 }, ... ]
}
```

Prometheus gauges (input-only, for the Advanced view / Grafana): `pg_sage_toil_minutes_saved_total{database,feature}`, `pg_sage_incidents_avoided_total{kind}`.

---

## 5. Observability as a supporting view (the reframe)

**PRESCRIBED.** Observability is a preference, not an anti-goal — but it is *secondary* to action and value.

- **Primary surface (default landing):** the **Value** view — DBA-hours saved (headline card, all-time/month/week + trend), breakdown by feature and database, incidents avoided, and the **decision/evidence queue** (Cases/Shadow → the Evidence Ledger). This is "PR review + value," not telemetry.
- **Advanced view (a distinct, secondary tab, off by default in the nav):** the telemetry that previously "failed the filter" is *allowed here*, clearly framed as supporting detail: findings explorer, action history, snapshot/metric trends, plan-history diffing, config drift, cost attribution. These consume the existing endpoints (`/findings`, `/actions`, `/snapshots/*`, `/metrics`) plus new read-only telemetry endpoints as needed.
- **Rule that stays:** every telemetry item in the Advanced view must *also* be wired as a silent trigger where it implies an action (plan-regression → trigger verify/re-plan; disk trend → custodian; XID trend → freeze custodian). Telemetry is allowed to be *shown*; it is not allowed to be the *only* thing that happens.
- **Alerts** remain side-effects of an action/park/incident event (notify on "acted" / "parked a decision" / "incident avoided"), never a terminal deliverable on their own.

UI note: the Value view is the product story ("pg_sage saved you 63 hours this month and prevented 2 wraparound outages"); the Advanced view is for the operator who wants to look under the hood. Build the Value view first.

---

## 6. `sage.policy` — the standing policy object (F6)

```sql
CREATE TABLE sage.policy (
  id            int PRIMARY KEY DEFAULT 1,          -- singleton per database in standalone; per-db in fleet
  database_id   int,
  version       int NOT NULL,
  profile       text NOT NULL DEFAULT 'staffed',    -- 'staffed' | 'unattended'
  doc           jsonb NOT NULL,                     -- the policy object (schema below)
  updated_by    text NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (database_id, version)
);
```

**Policy `doc` shape (validated on write; ambiguity rejected):**
```jsonc
{
  "allowed_change_classes": ["index","analyze","vacuum","freeze","autovacuum_tuning",
                             "config_guc","retention","fk_index","online_migration"],
  "maintenance_windows": ["weekdays 01:00-05:00"],   // parsed with the CORRECT parser
  "lock_duration_ceiling_ms": 3000,
  "blast_radius": { "max_rows_rewritten": 5000000, "max_tables_per_window": 20 },
  "budgets": { "storage_bytes": 0, "spend_daily": null, "llm_tokens_daily": 500000 },
  "rate_limits": { "max_self_initiated_changes_per_window": 50 },
  "deadline_overrides": { "xid": true, "disk": true },   // 'unattended' default; 'staffed' default false
  "refusal_set": ["rls_change","grant_expansion","major_upgrade","non_dup_object_drop","unrollbackable"],
  "unknown_classification": "fail_closed",               // ONLY legal value; enforced
  "serialize_mode": "park"                                // "park" | "queue"
}
```

**Validation rules (PRESCRIBED — reject on violation, do not silently coerce):**
- `budgets.*` of `0` means **"disabled/none allowed," never "unlimited."** `null` means "no cap." (Fixes the audit's zero-means-unlimited class.) A missing key = fail closed for that dimension.
- `unknown_classification` must equal `"fail_closed"`.
- Windows must parse; an unparseable window is a validation error (not "never/always").
- A `propose_policy_change` returns a **dry-run impact**: which currently-pending actions the delta would newly allow/block, before it can be ratified. Ratification is operator/admin-gated (reuse v0.8.4 RBAC). LLM may draft, never ratify.

**Two shipped profiles:**
- `staffed`: `deadline_overrides` off, narrow windows, `online_migration` requires approval.
- `unattended`: `deadline_overrides` on, wide windows, guarantees (F3/F4) at rung-4, structural changes still recommend-only.

---

## 7. `verify` — verification & auto-revert engine (F1's core, reused everywhere)

Replaces the global-cache-hit regression check in `executor/rollback.go`.

```go
// package verify
type Criterion struct {
    Kind        string        // "per_query_latency" | "write_latency" | "invariant"
    TargetIDs   []int64       // queryids for per_query
    MinGainPct  float64       // revert if NO target improves by >= this (Azure "no-gain revert")
    RegressPct  float64       // revert if any target regresses > this
    WriteImpactPct float64    // revert if table write latency up > this
    Window      time.Duration // adaptive; see below
    HardMax     time.Duration // upper bound (default 72h)
}

// Watch blocks (in a detached goroutine tracked by the executor WaitGroup) for the
// window, then evaluates. Returns the verdict; the caller reverts on !ok.
func (e *Engine) Watch(ctx context.Context, actionID int64, c Criterion) (ok bool, reason string)
```

**Adaptive window:** start at `policy default` (2h). If the targeted queries have `< min_calls` (default 30) samples in `sage.query_store` at window end, extend up to `HardMax`; if still insufficient, **revert-to-safe** (drop the change) and re-queue rather than claim an unverifiable win. Never mark success without significance.

**Evaluation (pseudocode):**
```
before = query_store windowed latency for TargetIDs in [executed_at - baseline, executed_at]
after  = query_store windowed latency for TargetIDs in [executed_at, now]
if any target: after > before*(1+RegressPct/100) -> revert("regression")
if no target:  after < before*(1-MinGainPct/100) -> revert("no_gain")     // Azure rule
if write_latency(table) up > WriteImpactPct       -> revert("write_impact")
if index invalid                                   -> drop + revert("invalid")
else -> success, stamp toil_minutes_saved
```

**Coupling rule (PRESCRIBED):** if `verify` cannot be run for a given action (no query store data, no clone, no measurable criterion), the executor **must not auto-apply** at rung 3-4 — downgrade to `queue_approval`. Apply and verify are inseparable.

**Low-load gate:** apply only when instance CPU/Data-IO/Log-IO below `policy` ceilings (mirror Azure). The collector already samples load; expose a `verify.OKToApplyNow()` check.

---

## 8. Feature builds

Each feature below gives: trigger, the catalog/query it uses, the algorithm split (deterministic vs. LLM), the action, autonomy rung, and acceptance criteria (→ tests). All route through §3 and use §7.

### F1 — Verify-and-Revert Index Lifecycle *(upgrade; rung 3→4)*

- **Trigger:** analyzer missing-index / consolidation finding (existing path).
- **Candidate generation (deterministic):** decompose target queries' predicates; enumerate candidates; cost each via planner (generic plan if no live EXPLAIN); **candidate-set selection** — start with a *greedy* set that maximizes Σ(estimated read gain) − Σ(write-amplification cost), CP-SAT later. LLM only for "is this workload too write-heavy to bother?" judgment + rationale text. LLM never emits the executing DDL.
- **Pre-estimate gate (rung 2):** HypoPG-create candidate, re-plan targets; require estimated cost drop ≥ policy threshold and planner actually chooses it. Degrade to EXPLAIN-only if HypoPG absent.
- **Validate:** `ValidateExecutorSQL` (must be `CREATE INDEX CONCURRENTLY`; `sage` schema protected); risk tier from SQL shape; LLM self-rating discarded.
- **Apply (rung 3):** `verify.OKToApplyNow()` → change-lease → `CREATE INDEX CONCURRENTLY` on dedicated conn with `lock_timeout`+`statement_timeout`.
- **Verify:** `verify.Watch` with `per_query_latency` + `write_latency` criteria; before_state from `sage.query_store` (F1 machinery exists). Revert on regression / no-gain / write-impact / invalid.
- **Autonomy:** launch rung 3 at `advisory`+ for SAFE adds; rung 4 once track record (trust ramp promotes). Fold **extended-statistics autopilot** (F11) in here — same verify loop, criterion = row-estimate error.
- **Acceptance:** (happy) useful index built, targets improve ≥G%, retained. (adversarial) read-helping/write-hurting index auto-reverted within window; no-gain index auto-reverted (no-gain rule); INVALID auto-dropped; concurrent duplicate DDL does not double-build; low-sample targets never claim a verified win; with verify disabled, executor refuses auto-apply.

### F2 — Rehearse-on-Clone Migration Gate *(new; rung 3)*

- **Trigger:** MCP `apply_migration`, GitHub Actions PR check, or F6 drift reconciliation.
- **Lint (deterministic gate):** Squawk-equivalent rule set → each hazard maps to a safe rewrite: `ADD UNIQUE`→`CREATE UNIQUE INDEX CONCURRENTLY`+`ADD CONSTRAINT USING INDEX`; `SET NOT NULL`→`ADD CHECK NOT VALID`→`VALIDATE`→set; add-col-NOT-NULL-DEFAULT / type change → expand/contract. Build these rules in `internal/migrate/lint`.
- **Expand/contract planner:** pgroll-style dual-schema views + hidden column + batched backfill + `search_path` version selection. Contract phase is a *separate, later* step, never same-cycle as expand.
- **Rehearse (rung 2):** `clone.Provider.Create()` → apply rewritten migration → measure lock type/duration, backfill time at policy batch size, resulting plans for top affected queries (real data), disk delta; optional traffic replay. Destroy clone.
- **Apply (rung 3):** with `lock_timeout` (cancel-and-retry on lock, waiters proceed), throttled backfill (pause on replication-lag signal from F4), old schema stays live.
- **Bounded auto-revert window** (PlanetScale model, default 30m): monitor affected-query latency/errors; breach → cancel (old version live → cheap rollback).
- **Autonomy:** rung 3 for additive/expand under policy; contract phase requires higher tier/approval (destructive).
- **Degrade:** no clone provider configured → rung-2 recommend only (lint + estimate), never auto-apply.
- **Acceptance:** (happy) `ADD UNIQUE` auto-rewritten, rehearsed, applied lock-free. (adversarial) `SET NOT NULL` on 500M rows without prior validated CHECK is rewritten/blocked; plan-regressing-under-replay migration parked or reverted; stale clone (> policy age) downgrades to recommend; contract never auto-runs same cycle as expand.

### F3 — Wraparound & Bloat Custodian *(new/agent-native; rung 4)*

- **Trigger (continuous):** per-table `age(relfrozenxid)` graduated thresholds; dead-tuple ratio; bloat estimate; xmin-holding long txn.
- **Deadline math (deterministic):** `distance = autovacuum_freeze_max_age − age(relfrozenxid)`; time-to-deadline from XID rate (snapshots). Urgency = green/amber/red (red = within 25% buffer).
- **Response ladder:** green → autovacuum tuning via F1 verify path; amber → `VACUUM (FREEZE)` in window on dedicated conn; red → **deadline-override** (off-window, raised cost limits) + if xmin pinned by a live txn, act on the blocker (runaway tracker cancel-with-evidence); bloat-needs-rewrite → orchestrate **pg_repack** (online), never `VACUUM FULL` (allowlist blocks it anyway).
- **LLM split:** deterministic owns deadline math + action choice + escalation; LLM only narrates + judges write-hot-and-freeze-urgent trade-offs (output = validated parameter, not the decision).
- **Autonomy:** rung 4 for VACUUM/FREEZE/autovacuum-tuning (deterministic deadline, low-risk, and waiting risks an outage). pg_repack gated moderate/approval unless policy permits. Deadline-override per §1.7.
- **Incident credit:** when a table crosses red and an action returns it below amber, write `sage.incident_avoided(kind='xid_wraparound', severity='near_miss')`.
- **Acceptance:** (happy) aging table tuned→frozen, never reaches red. (adversarial) xmin pinned by 6h idle-in-txn → custodian acts on the blocker, not endless futile VACUUM; approaching hard limit with override off → loud escalating artifact + (per policy) protective action, never a silent deadline miss; over-aggressive autovacuum setting auto-reverted.

### F4 — Replication-Slot & WAL Disk Guardian *(agent-native; rung 3→4)*

- **Trigger (continuous):** per-slot retained WAL bytes (restart_lsn vs current LSN); `active=false` duration; retained WAL as fraction of disk; disk-exhaustion trend.
- **Classify (deterministic):** healthy-active / lagging-active / inactive-recent / abandoned (inactive > `policy.abandon_after` AND retaining > threshold).
- **Response:** lagging-active → set/confirm `max_slot_wal_keep_size` backstop (bounding beats deleting; an invalidated lagging slot is recoverable, disk-full is an outage) + throttle producers per policy; abandoned → drop **only** after long abandonment proof AND not in `sage.slot_consumer_registry`; disk-red → backstop + throttle + notify (side-effect), never a hard read-only stop.
- **Autonomy:** keep-size backstop rung 4 (safe, reversible). Slot **drop** rung 2-3 default, rung-4 only with explicit `policy` opt-in + abandonment proof + registry check (unrollbackable → hardest gate). Fail closed: unprovable abandonment → bound, don't drop.
- **Managed providers:** use provider disk metrics / `safe_wal_size`; if filesystem invisible, enforce WAL-bytes-vs-configured-max instead of disk %.
- **Incident credit:** bounding a slot that was on track to fill the disk → `incident_avoided(kind='disk_full_slot')`.
- **Acceptance:** flapping slot never dropped; registered-consumer slot never auto-dropped; imminent exhaustion with no provable drop → bound+throttle, not read-only; managed-no-filesystem still enforces WAL bounds.

### F5 — Agent-Native Schema Custodian *(Workstream C; rung 2→3)*

- **Trigger:** slow schema scan (hourly/daily) + on-DDL via F6 change ledger.
- **Invariant catalog (deterministic detectors, each with remediation-class + auto-remediable flag):** FK-without-supporting-index (auto: `CREATE INDEX CONCURRENTLY`, verified via F1); unbounded append table (auto-with-consent: retention policy — partition + scheduled drop, **first run dry-run always**); missing NOT NULL/CHECK (recommend/park — prove intent via `NOT VALID`→`VALIDATE`); everything-text (recommend + rehearse F2); random-UUID PK on write-hot table (recommend + rehearse, never auto — rewrites table); no PK (recommend); index-on-everything (hand to F1 for verified redundant-drop).
- **LLM split:** deterministic owns invariant detection; LLM judges likely intent (`status text` with 4 distinct values → propose CHECK/enum), synthesizes rationale, prioritizes parked items. Never decides an auto-applied structural change.
- **Table contracts:** agents declare intent via MCP → `sage.table_contract` (append_only?, retention?, expected_pk?, exemptions) so the custodian enforces the *right* invariants. A declaration constrains; it never authorizes destructive action.
- **Autonomy:** FK-index/redundant-drop rung 3-4; retention rung 3 with mandatory dry-run + declared window; structural rung 1-2 recommend/rehearse.
- **Acceptance:** new FK gets verified supporting index within a cycle; declared append-only table gets retention after dry-run; type tightening never auto-applied; first retention run dry-runs (no deletes); small-lookup FK index showing write-cost-no-gain reverted/exempted; externally-re-introduced pathology parked after N reversions (existing oscillation guard), not fought forever.

### F6 — Standing-Policy & Change-Serialization Control Plane *(foundation)*

Built in §3 (gate), §6 (policy object + validation), plus:
- **Evidence Ledger:** extend `sage.action_log` + a new `sage.decision` table for parked/recommended items:
```sql
CREATE TABLE sage.decision (
  id           bigserial PRIMARY KEY,
  database_id  int,
  feature      text NOT NULL,
  intent       text NOT NULL,
  evidence     jsonb NOT NULL,          -- trigger, metrics, alternatives considered
  proposed_sql text,
  verdict      text NOT NULL,           -- 'parked'|'recommended'|'blocked'
  reason       text NOT NULL,
  deadline_kind text,                   -- non-null if blocking an xid/disk deadline
  created_at   timestamptz NOT NULL DEFAULT now(),
  resolved_at  timestamptz
);
```
- **Degraded-state clock:** a parked `sage.decision` with non-null `deadline_kind` is polled by the relevant custodian; when urgency crosses red, the custodian triggers the protective override (F3/F4) rather than let the deadline pass. A parked decision without a deadline may hold indefinitely (safe-but-suboptimal) with its durable artifact.
- **Self-audit job:** verify every `action_log` row with `outcome!='failed'` has a matching ledger/evidence entry (no un-gated actions); verify past index/retention fixes still hold (index still used, retention still running) — re-open a `sage.decision` if an invariant regressed.
- **Acceptance:** in-policy action executes and is fully traced; out-of-window moderate DDL blocked until the *correctly-parsed* window; Sunday-only window honored only on Sunday (audit bug fixed via test); two conflicting migrations serialized; caller asserting elevated authority refused + logged as a request; zero/ambiguous budget rejected at validation; parked decision blocking XID deadline triggers override; every executed action has a ledger entry.

### F7 — Fleet Staged-Rollout & Cross-Instance Learning *(deferred; rung 4)*

Spec after F1/F3/F4 exist. Sketch: policy inheritance (fleet default → tag/class → instance); a validated fix on instance A becomes a *prior* for its cohort, **re-verified per instance** (never blind-applied); canary a fleet change on k instances, measure aggregate regression, halt on breach; blast-radius containment so one bad decision can't fan out. Depends on F6 policy inheritance + a stock of per-instance validated changes. Fix the audit's fleet-mode dead features (empty health-history, nil ANALYZE semaphore) before building on fleet.

---

## 9. `clone.Provider` (enabler)

```go
// package clone
type Provider interface {
    Create(ctx context.Context, spec CloneSpec) (Clone, error) // thin/branch/snapshot
    Destroy(ctx context.Context, c Clone) error
    SnapshotAge(ctx context.Context) (time.Duration, error)     // freshness of the source
}
type Clone struct { DSN string; ID string; CreatedFrom time.Time }
type CloneSpec struct { IncludeData bool; TargetSizeHintBytes int64 }
```
- **DLE adapter (reference):** talks to a Database Lab Engine instance (ZFS/LVM thin clones); `Create` ≈ seconds. Config: DLE endpoint + token.
- **Snapshot-restore adapter (managed):** restore latest automated snapshot / PITR to an ephemeral instance via the provider API (RDS/Aurora/Cloud SQL/AlloyDB), return its DSN, destroy on `Destroy`. Slower (minutes); cache/reuse within a rehearsal batch.
- **Freshness rule:** if `SnapshotAge > policy.max_clone_age` (default 24h), F2 downgrades to recommend-only (a stale clone can't validate current behavior).

---

## 10. MCP intent-level surface (re-introduced)

New `internal/mcp` server (stdio + optional HTTP). **Every tool routes through the §3 gate.** Caller claims are untrusted input; authority comes only from `sage.policy`.

Tools (JSON Schemas abbreviated):
```jsonc
optimize_query      { query_id|query_text, goal:"latency", constraints?:{max_write_impact_pct} }
                 -> { decision, candidate_ddl, pre_estimate, expected_effect, rollback_handle, evidence_id }
apply_migration     { ddl|intent, constraints?:{max_lock_ms, window_minutes} }
                 -> { decision, rewritten_plan, rehearsal_result, rollback_handle, evidence_id }
ensure_fk_indexes   { schema }                          -> { actions:[...], evidence_id }
declare_table_contract { table, append_only?, retention?, expected_pk?, exemptions? } -> { ok }
register_consumer   { slot_name, owner }                -> { ok }   // protects a slot; never authorizes a drop
set_maintenance_policy { scope, patch }                 -> { dry_run_impact }   // ratify = operator-gated
get_guarantee_status {}   -> { xid:{...}, wal:{...}, schema:{...} }  // machine-readable invariant state
get_value {}              -> same shape as GET /api/v1/value
get_ledger { filter }     -> [ decision/evidence rows ]
```
Rules: no tool can widen authority; `set_maintenance_policy` returns a dry-run and requires operator ratification (LLM/agent may propose, not ratify); results are machine-consumable (JSON), not prose.

---

## 11. Config additions (`config.yaml` / env / `sage.config`)

```yaml
policy:
  profile: unattended            # staffed | unattended
  # full policy doc may live in sage.policy; this is the bootstrap default
value:
  toil_model_version: 1          # sage.toil_model drives actual values
verify:
  window_minutes: 120
  window_max_minutes: 4320       # 72h
  min_gain_pct: 20
  regress_pct: 15
  write_impact_pct: 20
  min_samples: 30
clone:
  provider: dle                  # dle | snapshot | none
  dle_endpoint: "..."            # if dle
  max_clone_age_minutes: 1440
custodian:
  freeze: { red_buffer_pct: 25 }
  wal:    { abandon_after_minutes: 1440, retained_wal_disk_pct_ceiling: 10 }
mcp:
  enabled: true
  transport: stdio               # stdio | http
```
Config validation must reject `budgets.*: 0` as "unlimited" (it means none) and unparseable windows (§6).

---

## 12. Cross-cutting requirements

- **Every autonomous action** produces a ledger entry (§6) and, on verified success, a toil credit (§4). No exceptions — the self-audit enforces it.
- **Reuse existing safety:** `ValidateExecutorSQL` allowlist (extend to protect `sage`), dedicated-connection VACUUM/CONCURRENTLY, `lock_timeout`+`statement_timeout`, emergency-stop (fail-closed), runaway-tracker evidence revalidation, anti-oscillation guard.
- **Managed-provider awareness:** `ALTER SYSTEM`→parameter-group rewriting already exists; custodians and F4 must use it; features that need filesystem/superuser degrade gracefully.
- **No new heavy deps.** MCP server: prefer a small stdio JSON-RPC implementation over a framework.
- **Observability:** emit the value gauges (§4) and keep the existing Prometheus endpoint for the Advanced view.

---

## 13. Phased delivery (build this order)

- **Phase 0 — Foundation + audit fixes (F6 + value model).**
  - Fix from the prior audit *first*: the High SSRF (`POST /api/v1/llm/models` → add `adminOnly` + endpoint validation), the maintenance-window cron parser (honor DOW/DOM), delete the dead `ShouldExecute` gate, enforce guardrail strings, and (fleet) the empty health-history + nil ANALYZE semaphore.
  - Build `policy` (gate + policy object + validation + change-lease + drift), `ledger` (Evidence Ledger + `sage.decision`), and the **value model** (`sage.toil_model`, `sage.incident_avoided`, stamping, `/api/v1/value`, the Value view). Nothing rung-4 ships before this.
- **Phase 1 — First autonomy wins:** F1 (verify-and-revert index; reuses most scaffolding — cheapest high-value), then F3 + F4 (guarantees; need F6, no clone). Prove act→verify→revert / guarantee on production with contained blast radius. Start showing DBA-hours-saved.
- **Phase 2 — Rehearsal substrate:** `clone.Provider` (DLE adapter first, snapshot adapter second) + F2. Deepen F1's pre-prod verification.
- **Phase 3 — Agent-native schema + MCP:** F5 (needs F2 clone + F6 serialization) and the `internal/mcp` intent server. This lights up the agent-to-agent lane.
- **Phase 4 — Fleet:** F7 (needs F6 inheritance + a stock of validated single-instance changes).

**Definition of done per phase:** all new packages ≥70% coverage with adversarial tests; the value view shows non-zero, honest DBA-hours-saved from verified actions; no un-gated actions in the self-audit; every acceptance-criteria bullet in §8 has a passing test.

---

## 14. Resolved open questions (was "open" in the research report)

- **Thin-clone substrate:** DLE primary, snapshot-restore for managed, Neon later (§1.4, §9).
- **CP-SAT vs greedy index selection:** greedy first, CP-SAT later (§8 F1).
- **Verify window default:** 2h adaptive [30m, 72h], extend-then-revert-to-safe on insufficient samples (§7).
- **Deadline-override:** policy opt-in, default ON for `unattended` profile (§1.7, §6).
- **Intended-schema baseline for an ownerless DB:** infer from current state + agent `declare_table_contract` calls; treat the baseline as evolving evidence (§8 F5, §6 drift).
- **Where trend/plan-diff/cost dashboards go:** the Advanced view, as supporting telemetry that also fires silent triggers — not the primary deliverable (§5).
- **Headline metric:** DBA-hours saved (verified actions) + incidents avoided (conservative, separate) (§4).

---

## 15. Sources

See `research/2026-07-22-agent-native-feature-spec.md` §11 for the full cited source list (Azure/Oracle verify-and-revert mechanisms, pganalyze CP-SAT, Database Lab, pgroll, Squawk, HypoPG, PlanetScale, the OtterTune failure analysis, and the wraparound/slot outage postmortems), all retrieved 2026-07-22. Repository facts (MCP-retired, trust-gate defects, existing toil model in `cases/shadow.go`) are from direct code reads.
