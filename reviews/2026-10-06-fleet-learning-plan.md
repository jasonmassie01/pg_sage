# Fleet learning (roadmap P3, target v2.3.0) — plan

Branch `claude/v23-fleet-learning` (worktree `pg_sage-fleetlearn`, from master 72646ab1).
Roadmap line: "schema fingerprints, look-alike priors, fleet findings, need-based LLM
budgets, leader election".

## Product calls (AI DBA lens: evidence first, authority never widened)

- **Fleet = one control database.** Learning data lives in the control DB (meta DB, else
  the fleet's primary/auth pool), keyed by a *fleet scope*: `meta` in meta-DB mode,
  `fleet:<hash of the configured database names>` in YAML fleet mode,
  `standalone:<database>` otherwise. Nothing is read across scopes.
- **Tenant boundary.** Every database gets a *boundary*: an agent database belongs to
  `agentdb-tenant:<tenant id>` (no tenant: isolated, no peers); a database tagged
  `tenant=<x>` belongs to `tenant:<x>`; the rest share the operator's own boundary `""`.
  Priors only flow between databases of the same scope **and** the same boundary.
- **Privacy.** Fingerprints hold only hashes of shapes: column type lists, index shapes
  (method, uniqueness, partial flag, key column *positions and types*), and query shapes
  (pg_stat_statements text with literals redacted and every identifier replaced). No data,
  no literals, no names. `fleet_learning.include_names: true` (operator opt-in) also stores
  table labels for drill-down.
- **Look-alike priors are evidence, never authority.** They are attached to tuning
  proposals as `detail.lookalike_prior` labelled "from look-alike databases" and on the
  approval card. They never set or raise `confidence_score`, never remove an approval
  requirement, never change trust levels or the policy gate. They *may add caution*: when
  look-alikes mostly regressed for the same action shape, the proposal requires approval
  (with the look-alike reason). Within the uncalibrated tier they order proposals (a prior
  is better evidence than none) but never move a proposal above a locally calibrated one.
- **Fleet findings are read models.** Computed on demand by fanning out over the fleet's
  pools (open findings grouped by category + object), shown when the same problem is open
  on at least `fleet_learning.fleet_finding_min_databases` databases, with per-database
  drill-down. No new write path, so nothing can act fleet-wide on them.
- **Need-based LLM budget.** `fleet_learning.budget_split: need` (default) splits
  `llm.fleet_token_budget_daily` by measured need (open findings, critical findings,
  unresolved incidents) with a floor and ceiling expressed as a percentage of the even
  share. The sum of allocations never exceeds the cap (integer floor division; rounding
  remainder is left unallocated). `even` restores the old split.
- **Leader election.** A lease row per scope in `sage.fleet_leader_lease`, acquired and
  renewed with one conditional UPSERT on the database clock (now()), epoch incremented on
  takeover (fencing token). A sidecar counts itself leader only until its *local*
  deadline (renew start + TTL − 20% margin), so a partitioned leader stops before a
  successor can take over. Leader-only jobs: the fleet-learning cycle (its writes are
  fenced by holder + epoch inside the write transaction), the approval-card follow-up
  loop and the agent-DB lifecycle reconciler. Lease release on shutdown for fast
  failover. `fleet_learning.leader_lease_seconds: 0` disables election (every sidecar
  runs the jobs, today's behaviour).

## Work items

- [ ] P0 plan (this file)
- [ ] P1 tests first (committed before implementation, not run):
  - `internal/fleetlearn`: fingerprint normalization/hashing (privacy: no names/literals),
    similarity, boundary, prior aggregation, fleet-finding grouping, need score,
    catalog + digest + store integration tests on real Postgres
  - `internal/leader`: elector state machine with fake store (acquire, renew, expiry,
    local deadline, failover, release), Postgres lease store split-brain test with
    concurrent contenders
  - `internal/fleet`: need-based allocation (floors, ceilings, cap invariant, even mode)
  - `internal/tuning`: look-alike prior never raises confidence/removes approval, adds
    caution on regression, orders uncalibrated tier
  - `internal/config`: defaults, validation, key classification
  - `internal/api`, `internal/mcp`: routes + `fleet_findings` tool
  - web: Fleet page "Recurring across the fleet" section
- [ ] P2 implement, run, fix
- [ ] P3 wiring in `cmd/pg_sage_sidecar` (`fleet_learning_wiring.go`)
- [ ] P4 config metadata regenerate, key classes, CHANGELOG `## Unreleased`
- [ ] P5 lint, full touched packages `-race -count=1` on PG17, DB tests on PG14, e2e
- [ ] P6 report, PR

## New config keys (all `operator_preference`; none widens authority or spend)

| Key | Default | Meaning |
|---|---|---|
| `fleet_learning.enabled` | true | fingerprints, priors and fleet findings |
| `fleet_learning.include_names` | false | store table labels with fingerprints |
| `fleet_learning.interval_minutes` | 60 | fingerprint/digest cycle |
| `fleet_learning.lookalike_min_similarity` | 0.6 | similarity for a look-alike |
| `fleet_learning.min_prior_outcomes` | 3 | outcomes before a prior is shown |
| `fleet_learning.fleet_finding_min_databases` | 3 | recurrence for a fleet finding |
| `fleet_learning.budget_split` | need | `need` or `even` |
| `fleet_learning.budget_floor_pct` | 50 | floor, % of the even share |
| `fleet_learning.budget_ceiling_pct` | 300 | ceiling, % of the even share |
| `fleet_learning.leader_lease_seconds` | 30 | lease TTL; 0 disables election |

## New tables (control DB; created everywhere by the idempotent migration)

`sage.fleet_fingerprint`, `sage.fleet_outcome_digest`, `sage.fleet_leader_lease`.

## Deferred / remainder

(filled in at the end)
