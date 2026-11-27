# pg_sage Documentation

Agentic Postgres DBA — a Go sidecar that monitors, analyzes, and optimizes
PostgreSQL 14+ databases with trust-ramped safety controls.

**Verified and reversible.** Every change is verified against its prediction, every change
that can be undone keeps a rollback, and the record can be published:
[Publishing outcomes](publish-outcomes.md) writes a signed bundle of changes verified
improved, no change, inconclusive and reverted, with the verifier's false-positive rate ([sample bundle](outcomes-sample/index.html)). Databases are
graded against the open, versioned [rubric](rubric.md).

v1 centers the product around an autonomous DBA workflow instead of a
traditional observability dashboard:

- **Overview** shows fleet health, provider readiness, and agent state.
- **Cases** is the primary work queue for findings, incidents, and proposed
  actions. Schema-health, forecast, query-hint, and incident routes open Cases
  with the matching source filter selected; the legacy `#/findings` route
  aliases here.
- **Actions** separates proposed changes, needs your review, in progress and
  history. Approval, completed SQL and verified outcomes remain separate states.
- **Fleet** manages database connections and per-database runtime state.
- **Settings** includes system configuration, emergency controls, and Shadow
  Mode: per action class, what pg_sage would have done below its earned trust
  level, how those decisions scored, and their potential toil minutes.

The web UI and JSON API are authenticated by default. On first standalone start, pg_sage
prints a one-time password for `admin@pg-sage.local`; API clients authenticate
through `/api/v1/auth/login` and reuse the `sage_session` cookie. Databricks Apps
uses platform sign-in and pg_sage roles, with no separate pg_sage password.

Migration-safety cases are non-executing by default for high-risk DDL. pg_sage
generates deterministic lock/rewrite and live-risk preflight evidence,
migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI
metadata so teams can review schema work before anything touches production.
A type tightening also gets a recommend-only online column swap script that changes
the type without a table rewrite ([Migration safety](migration-safety.md)).

Incident and maintenance cases now carry richer automation candidates. Lock
blockers, runaway queries, connection exhaustion, standby conflicts,
WAL/replication pressure, autovacuum pressure, and sequence exhaustion route to
typed playbooks with safe diagnostics or reviewed scripts. Vacuum, bloat, and
freeze findings project guarded `VACUUM`, concurrent reindex, bloat-remediation
plans, freeze diagnostics, and per-table autovacuum tuning actions with
explicit verification plans and IO-saturation gates.

Query tuning cases are no longer limited to planner hints. pg_sage can turn
suggested rewrites into PR-ready artifacts, retire broken hints through a safe
metadata action, recommend `CREATE STATISTICS`, draft parameterization changes,
and promote repeated per-query `work_mem` hints into reviewed role-level
settings when the blast radius is explicit.

Index recommendations are measured with HypoPG hypothetical indexes when the
`hypopg` extension is installed; pg_sage notices an extension installed while it
runs within 10 minutes, without a restart. Only a complete measurement verifies
or rejects an index. HypoPG models btree, BRIN, hash and bloom indexes; a GIN,
GiST, SP-GiST, HNSW or IVFFlat recommendation stays unverified with the method
named and needs operator approval (measure HNSW and IVFFlat with `pg_sage
vector-lab`); so does a candidate whose table has such an index in flight, since
its baseline cannot be modelled. When some workload statements cannot be
planned, the verdict is unverified and the reason and approval card name the
planned improvement and the unplanned queryids (`what_if_measured`,
`what_if_unplanned_queryids`). Lowering `optimizer.hypopg_min_improvement_pct`
re-measures remembered rejections the new minimum would admit and stops listing
them to the tuning agent as rejected.

The tuning agent sees each column by its type name (`vector`, `geometry`, an
enum's name) rather than `USER-DEFINED`. A statement is hinted as a vector ANN
query only when a `<->`, `<=>` or `<#>` operand is a `vector`, `halfvec` or
`sparsevec` column or is cast to one: the same operators on PostGIS, pg_trgm or
btree_gist columns are not vector queries, and a column of unknown type gets no
hint. A filtered ANN query is pointed at measuring its recall (pgvector iterative
scans or a higher `ef_search`) with [`pg_sage vector-lab`](vector-lab.md).

A table's workload is the application statements that name it after FROM, JOIN,
UPDATE or INTO (every relation of a comma list, `ONLY` and quoted mixed-case
names included; literals, comments and CTE names never count), resolved the way
the tuning agent resolves them. A schema-qualified name counts only for its own
schema. An unqualified name counts for the only schema holding a table of that
name, so an application on `search_path=app` has its statements on `app.orders`
measured. pg_stat_statements does not record the search_path a statement ran
under, so a name held by `public` and another schema counts for `public`, and a
name held by two schemas other than `public` counts for neither; qualify the
statements to have them measured against the other table. HypoPG plans the
statements under pg_sage's own search_path, so an unqualified statement is
measured only when pg_sage's role or database resolves the name the same way
(an unplanned statement leaves the verdict unverified).

On a write-heavy table (index-maintaining writes, that is inserts and non-HOT
updates, above `optimizer.write_heavy_ratio_pct` of those writes plus the tuples
read) a measured improvement must also reach
`optimizer.write_impact_threshold_pct`; below it the candidate is rejected and
remembered, and once the table stops being write-heavy the idea is measured
again. An unmeasured candidate on such a table stays unverified ("write-heavy
table; improvement unmeasured"). When the table's columns could not be read
this cycle, column existence is left to HypoPG and PostgreSQL and an unverified
reason says "column metadata unavailable this cycle". The per-table index cap
counts only valid indexes, and a replacement does not count the index it drops.

A replacement (`replace_index`) is measured as the swap it is: HypoPG plans the
workload with the old index, creates the wider one, hides the old one with
`hypopg_hide_index` and plans again, so the improvement is judged without the
index that will be dropped. Every statement whose plan used the old index is
guarded: one whose planned cost rises more than 10% once it is hidden rejects
the replacement, and the reason names its queryid. A covered drop proposed by
the tuning agent is measured the same way (the index hidden): a guarded
statement rising more than 10% rejects the drop, and otherwise the measured
change is the drop's prediction (`expected_change_pct`, method `model`) for
verify-and-revert. Hiding needs HypoPG 1.4 or later, detected per instance in
`pg_proc` because providers ship different versions; with an older HypoPG a
replacement is measured with the old index present and its reason says
"HypoPG without hypopg_hide_index (1.4+)", and a drop keeps the rule's
"reads unchanged" prediction. Hidden indexes are backend-local: pg_sage unhides
them before the what-if connection goes back to the pool, and a connection whose
hide failed is closed instead.

Provider readiness is adapter-driven for Lakebase, self-managed Postgres, Cloud SQL,
AlloyDB, RDS, and Aurora. The matrix shows extension enablement paths, log
access, provider limitations, and action-family readiness so automation policy
can differ by managed-service constraints.

---

## Getting Started

- [Databricks-native implementation](databricks-native.md) — architecture, Lakebase/App setup, OAuth, compatibility and evidence
- [Lakebase and Unity Gateway](lakebase.md) — native connections, telemetry, SRE, supported actions and restrictions
- [Databricks Apps](databricks-apps.md) — build, resource bindings, platform authentication and deployment
- [Five-minute quickstart](quickstart.md) — minimal role, read-only first look in minute 1
- [Installation](installation.md) — Download, prerequisites, database user setup
- [Try It Out](try-it-out.md) — local v1 smoke path and UI checklist
- [Walkthroughs](walkthrough.md) — Platform-specific getting started guides
  - [Linux / macOS](walkthrough-linux.md)
  - [Windows](walkthrough-windows.md)
  - [Fleet Mode](walkthrough-fleet.md)

## Reference

- [Glossary](glossary.md) — the five nouns (Issue, Plan, Change, Decision, Autonomy) and the terms the dashboard defines
- [Architecture](architecture.md) — Tier 1 rules engine, Tier 2 LLM analysis, Tier 3 executor
- [Fleet Mode](fleet.md) — control database, tenant boundaries, fleet learning, leader election, fleet routes
- [Configuration](configuration.md) — YAML config, environment variables, runtime overrides
- [Command-line client](cli.md) — `plan`, `approve`, `apply`, `explain`, `ledger`, `doctor` and their exit codes
- [Finding Types](findings.md) — All diagnostic findings and severity levels
- [Owner Checks](custom-checks.md) — Read-only house rules you write as one SELECT
- [SQL Schema](sql-reference.md) — `sage.*` tables, views, and functions
- [LLM Costs & Budgets](llm-costing.md) — Token usage, model routing, per-database budgets

## Operations

- [Databricks deployment templates](https://github.com/jasonmassie01/pg_sage/blob/master/deploy/databricks-apps/README.md) — package/configure commands and operator handoff
- [Deployment](deployment.md) — Docker, systemd, Kubernetes
- [Security](security.md) — Least-privilege roles, network policies, audit logging
- [Agent governance](agent-governance.md) — agent principals, tokens and OAuth, grants, two-person rules, posture and the kill switch
- [Decommissioning the removed database provisioner](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/decommission/README.md) -- inventory, delete templates and acknowledgement for resources an earlier version created
