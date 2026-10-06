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

- [x] P0 plan (this file)
- [x] P1 tests first (committed before implementation, not run):
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
- [x] P2 implement, run, fix
- [x] P3 wiring in `cmd/pg_sage_sidecar` (`fleet_learning_wiring.go`)
- [x] P4 config metadata regenerate, key classes, CHANGELOG `## Unreleased`
- [x] P5 lint, full touched packages `-race -count=1` on PG17, DB tests on PG14, e2e
- [x] P6 report, PR

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

## What shipped

| Piece | Where |
|---|---|
| Schema fingerprints (types, index shapes, literal/name-free query shapes; names only with `include_names`) | `internal/fleetlearn/{shape,normalize,keywords,catalog}.go` |
| Similarity, look-alikes, tenant boundary | `internal/fleetlearn/similarity.go`, `shape.go` (`Boundary`) |
| Outcome digests and look-alike priors | `internal/fleetlearn/{digest,prior,service}.go` |
| Control-DB store with leader fencing | `internal/fleetlearn/store.go`, `internal/schema/fleet_learning_migration.go` |
| Priors on tuning proposals and approval cards | `internal/tuning/lookalike.go`, `rank.go`; `internal/approvalcard/calibration.go` |
| Fleet findings: API, MCP `fleet_findings`, Fleet page | `internal/fleetlearn/findings.go`, `internal/api/fleet_learning_routes.go`, `internal/mcp/fleet_tools.go`, `web/src/pages/databases/FleetFindings.jsx` |
| Need-based LLM budget split | `internal/fleet/budget_need.go`, `budget.go` |
| Leader election | `internal/leader/{elector,postgres}.go` |
| Wiring, leader-gated jobs | `cmd/pg_sage_sidecar/fleet_learning_{wiring,adapters}.go` |
| Config, key classes, generated metadata | `internal/config/fleet_learning.go`, `key_classes.txt`, `docs/generated/config-lifecycles.md`, `web/src/generated/config_meta.json` |

## Bugs found while testing

1. Extension-owned tables (pg_hint_plan's `hint_plan.hints`) were fingerprinted, making
   unrelated databases look alike. Fixed: `pg_depend` deptype `e` members are excluded.
2. A cycle that read no database (first cycle before the fleet registers, or an outage)
   pruned every stored fingerprint. Fixed: no prune without a successful read; the first
   cycle starts two minutes after startup.
3. The retention guard and the config-override consistency guard caught the new tables and
   keys: fleet tables are exempt (bounded current state), `fleet_learning.*` is YAML-only.
4. Query-shape filter `sage.` matched application tables like `message.` (found in review,
   fixed before the first run with a word boundary; catalog reads `pg_*` are also excluded).

## Test results

**Command:** `go test -cover -count=1 -p 2 ./...` (Docker golang:1.25, `--cpus=2`, own PG17
container), touched packages also with `-race` on PG17 and without on PG14;
`go test -tags=e2e -count=1 ./e2e/`; `npx vitest run`; golangci-lint `run ./...`.

**Total:** full suite 113 packages: 111 ok, 2 failed (retention guard and config-override
guard, both fixed in 9a0f3553 and re-run green); touched packages on PG14: 12/12 ok;
`-race` on PG17: 10/12 ok, 2 failed on `host.docker.internal` dial timeouts under load
(`TestSnapshotAPI_OrphanDeltaReadsNull`, `TestFleetReloadAddRemoveCyclesLeakNothing`), both
green on re-run with `-race`; e2e ok; web 93 files / 619 tests ok; lint 0 issues.
125 new Go tests, 6 new web tests.

**Coverage (touched packages, PG17):** fleetlearn 88.9%, leader 86.8%, fleet 84.7%, tuning
91.6%, approvalcard 90.7%, config 92.8%, api 79.2%, mcp 84.7%, schema 84.5%, retention 87.3%,
store 76.2%, cmd/pg_sage_sidecar 78.4% (PG14 run). All meet thresholds.

### Skipped tests

- e2e: `TestLLM*` and `TestTuningAgentLive`: live-LLM tests, need `PG_SAGE_LIVE_LLM=1` and a
  key (by design). No skips in the touched packages.

### Mutation testing

15 mutants on the key rules (boundary check, isolated tenants, caution tie, safety margin,
budget cap rounding, ceiling, caution application, cautioned ranking, identifier redaction,
per-database recurrence, scoped MCP tokens, lease WHERE clause, fence check, scope filter on
read, prune on empty cycle): all killed. One deliberately equivalent mutant survived.

### Post-test audit

- Added after the audit: MCP backend scope filter and nil-service adapters
  (`fleet_learning_adapters_test.go`).
- Not covered by an automated test: the two-sidecar process-level handover (covered at the
  store and elector level with real Postgres, contention and expiry); the five-minute need
  loop's scheduling (its measurement and application are tested).
- Fakes: the elector's in-memory store mirrors the Postgres semantics; the same scenarios
  run against Postgres in `postgres_db_test.go`.

## Deferred / remainder

- Fleet findings group by category + object identifier, so they recur when tenants share
  names (the common tenant-per-database case). Grouping differently named objects by shape
  is not built.
- Priors reach the tuning agent's proposals (index, GUC, reloption, statistics, query hint)
  and their approval cards. SRE actions and older advisor paths do not use them yet.
- Leader-only jobs: fleet learning, approval-card follow-ups, agent-database reconciler.
  The specialist outbound worker and per-database loops are not leader-gated (per-database
  work already has its own locks); running several sidecars on the same databases is
  otherwise unchanged.
- The fleet LLM budget is still per process: each sidecar enforces its own copy of the cap.
  A shared, control-database budget ledger is not built.
- Look-alikes and the leader status are API-only (no UI beyond fleet findings).
