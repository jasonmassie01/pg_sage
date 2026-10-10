# Agent Guard spec: baseline refresh, `72646ab1` → `62d27f6e`

**Date:** 2026-10-09. **Spec:** `AGENTDB-SPEC.md` (now "decided, draft 3").

## Baselines

| | Commit | What it is |
|---|---|---|
| Old | `72646ab1` (2026-10-05) | v2.2.0 plus #128 and #129. The spec was drafted and first verified here |
| New | `62d27f6e` (2026-10-09) | `origin/master` when the branch was rebased: v2.3.1 (`c5ef2b24`) plus #149–#153, #155 and #157 (performance and CI follow-ups) |

- 133 commits and 329 changed files lie between the two.
- The spec commit was rebased from `518d1c58` to `5328af9b` without conflicts; it only adds files
  under `reviews/2026-10-05-agentdb/`.
- Releases in between: v2.2.1, v2.3.0 and v2.3.1, all on 2026-10-07. v2.2.0 was already in the
  old baseline.

## Method

- Every citation was read at `62d27f6e` and diffed against `72646ab1`. Paths are relative to
  `sidecar/` unless they start with `docs/`, `.gitignore`, `test-fixtures/` or `.github/`.
- Line citations were re-read at the cited lines, not only diffed.
- The AgentDB code is byte-identical at both baselines: `internal/agentdb`,
  `internal/api/agent_db_*`, `web/src/pages/agentdb/`, `AgentDBsPage.jsx`, `App.jsx` and
  `Layout.jsx`. The only AgentDB change is three lines in `cmd/pg_sage_sidecar/agentdb_reconciler.go`.
  Claims that cite audit ids (A-xx, DP-xx, B1/B2/B7) therefore carry over unchanged.
- Counts were recomputed the way `research/prior-specs.md` §7.1 and §7.4 computed them.
- No database, container or test run was used. This is a documentation refresh.

## Citations

Status: **ok** (still exists and means what the spec says), **fixed** (stale path, line, name,
number or behaviour corrected in place: old → new), **gone** (no longer exists; what replaced it).

| # | Spec | Citation | Status | Note |
|---|---|---|---|---|
| 1 | §0 | `internal/mcptoken/mcptoken.go:6,54`: agent tokens never carry `approve` | ok | |
| 2 | §0 | `internal/agentdb` imports no other pg_sage package | ok | Only `internal/testdb`, in `testmain_test.go` |
| 3 | §0 | `internal/config/config.go:1038`: `require_backup_before_destroy` true by default | fixed | `:1038` → `:1047` |
| 4 | §0 | `internal/agentdb/lifecycle_claim.go:41-45`: teardown needs `restore_verified` | ok | |
| 5 | §0 | `internal/api/agent_db_agent_api.go:56-70`: four agent routes | ok | |
| 6 | §0 | `docs/neon-supabase.md:119-121`: Neon and Supabase live-verified | ok | |
| 7 | §0 | `.gitignore:54-55`: receipts under ignored `test-output/` | ok | |
| 8 | §0, §1.5 | About 16.7k production Go lines (13,689 + 2,638 + 375) and 3.5k UI lines; "about 20k" | ok | Now 13,689 + 2,638 + 378 = 16,705; UI 3,557 |
| 9 | §0, §7, §12 | 27 tables: 26 `sage.agent_db_*` plus `sage.agent_identities` | ok | 27 `CREATE TABLE`s in `internal/agentdb` |
| 10 | §0 | About 60 endpoints | ok | Route files unchanged; see "Not verified" |
| 11 | §0 | Live runs: RDS, Cloud SQL, Lakebase 2026-05-09/10; runners changed 2026-09-26, not run live since | ok | No AgentDB code change and no new receipt since |
| 12 | §0 | "One mechanical commit out of about 1,264 since 2026-09-28" | fixed | Two of about 1,397 to `62d27f6e`: `86230885` (file split) and `e9b4d663` (v2.3 leader check on the reconciler, 3 lines) |
| 13 | §0 | "pg_sage has 13 GitHub stars" | fixed | Not code. Kept as the drafting figure, with 17 on 2026-10-09 (GitHub API) |
| 14 | header | "v2.3 tracks are open" | fixed | Shipped in v2.3.0 (2026-10-07) |
| 15 | header, §6.15, App. B SR-65 | PR #130 (first-look timeout) open; posture's `SET LOCAL` fallback if it doesn't merge | gone | #130 closed unmerged on 2026-10-07; its branch shipped in v2.2.1 through #136 (`internal/firstlook/runner.go:22`, 5 s per statement). Fallback removed |
| 16 | §1.3 | `internal/earned/level.go:17-32`: L0–L3, L4 never granted | ok | |
| 17 | §1.5 | `internal/migration` and `internal/clone` ship already | ok | Both unchanged |
| 18 | §2.1 | `internal/config/defaults.go:196`: reconciler every 300 s | ok | Since v2.3 it runs on the leader only (`cmd/pg_sage_sidecar/agentdb_reconciler.go:47`); added to §2.1 |
| 19 | §2.2 | `live_execution_*.go`, `live_create.go`, `*_ownership.go`, `hosted_runner.go`, `lifecycle_claim.go`, `request_provision.go`, `agent_db_agent_api.go` | ok | |
| 20 | §2.3 | `teardown_reconcile.go:99-130`: TTL destroy self-authorized | ok | |
| 21 | §2.3 AU-10 | `docs/agent-db-deployments.md:350,358,378`; four May reports; two `test-fixtures/full_surface/*.ps1` | ok | Files unchanged; the credentials are still there (values not repeated) |
| 22 | §2.3 AU-11 | Commits `3367ffa2` and `1683e2e5` | ok | |
| 23 | §2.4 CG-01 | `internal/store/config_helpers.go:315,325`; `internal/api/config_apply.go:79-94` | ok | |
| 24 | §2.4 CG-02 | `internal/config/config.go:652-653`: TLS variables read, never used | fixed | `:652-653` → `:655-656`; still no TLS listener |
| 25 | §2.4 CG-03 | `internal/auth/oauth.go`: no PKCE, nonce or `id_token` | ok | |
| 26 | §2.4 CG-04 | `internal/auth/types.go:21-35`: three global roles | ok | |
| 27 | §2.4 CG-06, §8.1 | `internal/api/mcp_principal.go:20-33`: static tokens only, 401 `mcp_token_required` | ok | |
| 28 | §2.4 CG-07 | One replica only (DP §10) | ok | Note added: v2.3's leader lease moves only fleet-wide jobs (fleet learning, approval-card follow-ups, the AgentDB reconciler); per-database work isn't gated on it |
| 29 | §2.4 CG-08 | `internal/mcp/scope.go:18-25`; `internal/mcp/intent_adapters.go:21-29` | ok | |
| 30 | §6.2.1 | `internal/policy` imports no internal package; `earned` and `facts` import `policy`; `GateConfig.Autonomy`, `GateConfig.Facts` | ok | `internal/policy` unchanged |
| 31 | §6.2.2 | `hardStop`, `validateRequest`, `ValidateSQL`, `providerDecision`, `factDecision`, `documentDecision`, `awaitVerification`, `OperatorApproved` | ok | |
| 32 | §6.2.2 | `internal/policy/document.go:363-370` (`allChangeClasses`) and `:341-345` (default document) | ok | |
| 33 | §6.2.2 | `internal/policy/gate_operator.go:26-28` (`change_class_not_allowed`) and `:12-14` (observation → `observe_only`) | ok | |
| 34 | §6.2.2 | `internal/config/defaults.go:47`: `trust.level` defaults to `observation` | ok | |
| 35 | §6.3 | `l3Blocker` needs one object and a maintenance window | ok | `internal/policy/autonomy.go:186-199` |
| 36 | §6.2.2 | Rollback classes | ok | `internal/policy/types.go:70-78` |
| 37 | §6.2.2, §8.1 | MCP codes -32602, -32001, -32003, -32004, -32005, -32007, -32010, -32011 and their names | ok | `internal/mcp/errors.go:12-39` |
| 38 | §6.2.6 | `internal/mcp/scope.go:11-16` and `:80-90` (approve scope, -32005) | ok | |
| 39 | §6.2.6 | The 20 tool names in the class table | ok | All registered. New since: `fleet_findings`, `specialist_investigation_transcript`, both read scope |
| 40 | §6.4 | `internal/mcp/principal.go:115-116`: fixed tokenless stdio agent | ok | |
| 41 | §6.4 | `POST /api/v1/mcp/tokens` | ok | `internal/api/mcp_token_routes.go:32` |
| 42 | §6.4 | `internal/config/config.go:118`: `encryption_key` | fixed | `:118` → `:121` |
| 43 | §6.5 | `sage.sre_database_bindings` (`identity_strength`, `cluster_epoch`, `runtime_key`); `internal/sre/coordinator.go:254-256` | ok | |
| 44 | §6.5 | `internal/value/fleet.go:165-181`: `host:port/dbname` fallback | ok | |
| 45 | §6.5 | `cmd/pg_sage_sidecar/fleet_bootstrap.go:15-24`: control DB can move | ok | |
| 46 | §6.7, §7 | `internal/schema/facts_migration.go:21-24`: inline CHECKs | ok | |
| 47 | §6.8 | `sqlast.InspectReadQuery`, `ErrUnavailable` without cgo; pg_query_go v6.2.2 | ok | |
| 48 | §6.8 | `internal/explain/analyze_guard.go`: `maxViewDepth` 4, deny-list | ok | |
| 49 | §6.8 | `internal/ask/run.go:132-136`: untrusted fencing | ok | |
| 50 | §6.10 | `internal/logwatch` | ok | |
| 51 | §6.11 | `trust.ramp_safe_hours`, `trust.ramp_moderate_hours` | ok | `internal/config/config.go:227-228` |
| 52 | §6.11 | The queue's 7-day default expiry | ok | `internal/schema/bootstrap.go:642` |
| 53 | §6.11 | "Same template fingerprint (v2.3)" for pooled evidence | fixed | v2.3's fingerprint is shape sets plus a similarity score. Now: identical table and index shape sets in `sage.fleet_fingerprint`, not look-alikes; tenant from `fleetlearn.Boundary` |
| 54 | §6.12 | `clone.Provider`; `clone.provider` `none`/`dle`/`snapshot`; `snapshot` errors | ok | `internal/clone/provider.go:19-23`; `cmd/pg_sage_sidecar/mcp_migration_runtime.go:23-43` |
| 55 | §6.12 | `sage.sre_deployments.deployment_id` | ok | |
| 56 | §6.12 | Reimplement from `hosted_*.go` and `lakebase_runner.go` "at `72646ab1`" | fixed | Now "at `62d27f6e`"; the files are byte-identical |
| 57 | §6.13 | B1, B2, B7 on the migration path | ok | Code unchanged. B1 re-read: `judge` never promotes without affected-query evidence (`internal/migration/rehearsal/orchestrator.go:117-125`) |
| 58 | §6.13 | `internal/autoexplain/collector.go:218` (`GENERIC_PLAN`) | ok | |
| 59 | §6.13 | `internal/optimizer/hypopg_generic.go:15-16` (`force_generic_plan`) | ok | |
| 60 | §6.14, §10.6 | v2.3 `history.store: meta` leaves no `sage` schema in observe-tier databases | fixed | It moves only snapshots and the query store; findings, first-look reports and the action log stay in the monitored database. New open item O-5 |
| 61 | §6.14 | "Fleet findings use v2.3 fingerprints" | fixed | v2.3 groups open findings by category and object (`internal/fleetlearn/findings.go`), at 3+ databases by default. The outcome (one finding per template fleet) holds |
| 62 | §6.14, §6.15 | "Catalog fingerprint" | ok | Design, not code, at both baselines. Clarified as Guard's own hash; v2.3's fleet fingerprint holds no grants or policies |
| 63 | §6.16 | `pg_query.Normalize` | ok | pg_query_go API |
| 64 | §7 | `internal/schema/bootstrap.go`, registered after `ddlAsk` | ok | `ddlAsk` at `:422`; v2.3 appended its migrations after it |
| 65 | §7 | `sage.mcp_tokens` (`kind`, `scopes`, `databases`) | ok | `internal/schema/mcp_v2_migration.go:22-25` |
| 66 | §7 | `sage.users.id` and `sage.action_queue.id` are SERIAL | ok | |
| 67 | §7 | `action_queue_proposed_via_check` | ok | `internal/schema/ask_migration.go:55-59` |
| 68 | §7 | `sage.decision.policy_version` | ok | `internal/schema/ddl_agent_ledger.go:11` |
| 69 | §7 | `sre_m7_autonomy_migration.go` | ok | |
| 70 | §8.1 | `internal/mcp/errors.go:3-7` | ok | |
| 71 | §8.1 | `internal/mcp/intent_execution_types.go:61,73` | ok | |
| 72 | §8.4, §12 | `web/src/pages/agentdb/` (20 files), `AgentDBsPage.jsx` and two tests | ok | |
| 73 | §9 | `internal/config/key_classes.txt`, `safety_critical` | ok | |
| 74 | §9 | New keys `agents.*`, `estate.*`, `mcp.stdio_principal`, `databases[].replicas` | ok | None exists yet and none collides; v2.3 added `history` and `fleet_learning` |
| 75 | §10.1 G0-04, §10.3 G2-12 | "The perf gate" | ok | Exists (`internal/testsupport/perfgate`, gates A–G). Gates now named (delta 6) |
| 76 | §6.1, §9, §10.2, §10.7 | "The v2.3 lease" / "leader election reused from v2.3" | ok | Exists now: `internal/leader`, `sage.fleet_leader_lease` (delta 1) |
| 77 | §12 | `internal/api/auth_middleware.go:113-117` | ok | |
| 78 | §12 | `cmd/pg_sage_sidecar/agentdb_*.go` | ok | Three production files; `agentdb_reconciler.go` gained the 3-line leader check |
| 79 | §12 | `internal/retention/agent_rules.go`; AgentDB entries in `internal/retention/exemptions.go` | ok | `exemptions.go` changed (fleet-learning entries added); the AgentDB map is unchanged |
| 80 | §12 | `internal/config/config.go:144-160` | fixed | `:144-160` → `:147-163` |
| 81 | §12 | `internal/config/clone.go:23-35` | fixed | `:23-35` → `:23-37`; the block ended at 37 at `72646ab1` too |
| 82 | §12 | `internal/config/key_classes.txt:112-116` | ok | |
| 83 | §12 | AgentDB keys in `internal/store/config_helpers.go` and `internal/api/config_apply.go` | ok | |
| 84 | §12 | `App.jsx` and `Layout.jsx` entries, Playwright specs, committed UI bundle, router golden | ok | Bundle renamed (`index-C4-F-grm.js` → `index-DAQ8L4Nj.js`) |
| 85 | §12 | Docs: generated config docs, `docs/agent-db-deployments.md`, `docs/runbooks/agentdb-*`, `docs/neon-supabase.md`, `docs/reports/2026-05-*agentdb*`, README claims | ok | |
| 86 | §10.1 G0-02, §12 | `git grep -ilE 'agent[-_]?db\|agt_'` matches only the allowlist after the manifest | fixed | v2.3 added matches: `internal/fleetlearn/shape.go` (`Boundary`'s `agentdb:` arm) and three tests. Added to the manifest |
| 87 | App. A.2 | `internal/rollout` | ok | |

**Totals:** 87 citations checked: 73 ok, 13 fixed, 1 gone.

## Deltas folded in

What shipped between the baselines and bears on the design. Each was checked in code first.

| # | Delta | Shipped | Code | Spec changes |
|---|---|---|---|---|
| 1 | **Leader lease.** Sidecars sharing a control database elect one leader; only it runs fleet-wide jobs, and writes are fenced by the lease epoch | v2.3.0 | `internal/leader/elector.go`, `postgres.go`; `sage.fleet_leader_lease`; `fleetLeaderAllows` (`cmd/pg_sage_sidecar/fleet_learning_wiring.go:74`); lease in the control pool (`wire.go:57-65`); `fleet_learning.leader_lease_seconds` (30) | §1.5; §2.1 step 4; §2.4 CG-07 note; §6.1; §6.5 (lease in the pinned control database); §9 comment; §10.2 scope; G1-08 (failover bound); §10.7 E4 |
| 2 | **History outside the monitored database.** `history.store: meta` moves snapshots and the query store only | v2.3.0 | `internal/histstore`; `internal/config/history.go` | §1.5; §6.14 observe tier; §10.5 heading; §10.6 G4 row (first clause met); O-5 |
| 3 | **Fleet fingerprints and fleet findings.** Shape-hash fingerprints with a tenant boundary; findings open on 3+ databases grouped by category and object | v2.3.0 | `internal/fleetlearn` (`shape.go`, `similarity.go`, `findings.go`); `sage.fleet_fingerprint` | §6.11 pooled evidence; §6.12 sandbox `tenant=` tag; §6.14 fleet findings; §6.15 catalog fingerprint clarified |
| 4 | **Index replace.** `replace_index`: create a wider index, then drop the subsumed one, both `CONCURRENTLY`, behind a durable state machine with crash resume, a lease across both steps, OID and definition re-checks and a soft drop; always approval-gated | v2.3.0 | `internal/executor/index_replace*.go`, `contract_replace.go`; `sage.index_replace` | §1.5; §6.13 (what exists; step 3 routes an exact replacement to it; step 6 reuses its crash rules); §7 comment; G3-03 |
| 5 | **Specialist query scope.** Plan-regression investigations probe only their statement (`QueryScoped`; `query_id`/`query_hash` bound as parameters) | v2.3.0 | `internal/sre/probes/query_scope.go`; `internal/sre/plan_query.go`; `internal/specialist/query.go` | §6.13 step 7: post-apply plan regressions are left to Sage SRE's statement-scoped investigations; Guard adds no reader |
| 6 | **CI performance gate.** Gates A–G, small scale on pull requests and large nightly, on a clean server since v2.3.1; an on-demand workflow | gate existed at `72646ab1`; changes in v2.3.1, #151, #155 | `internal/testsupport/perfgate`; `.github/workflows/ci.yml`, `perfgate.yml`; `cmd/pg_sage_sidecar/perfgate_first_look_test.go` | §10 intro (new tables: A, C, F; list endpoints: E); G0-04 (warm-up: A, D; steady: B, C, G; `TestPerfGateFirstLook`, which CI doesn't run today, added in G0) |
| 7 | **The first look's own timeout.** 5 s per statement, one retry of a check a transient error degraded | v2.2.1 (#136, PR #130's branch) | `internal/firstlook/runner.go:22`, `retry.go` | Header; §6.15 cadence; App. B SR-65 |
| 8 | **AgentDB inside fleet learning.** `fleetlearn.Boundary` gives `agentdb:` databases a tenant or isolated boundary | v2.3.0 | `internal/fleetlearn/shape.go:64-69`, `:79-84`; `similarity.go:87` | §12 removal manifest (and G0-02's grep) |

**Checked and not folded in** (no bearing on the design):
- v2.2.1: sequence runway through column defaults, grant warnings at `observation`, the startup
  line, the `sage_footprint` threshold.
- v2.3.0: the specialist investigator section and transcripts, the need-based fleet LLM budget,
  look-alike priors on approval cards.
- v2.3.1: CPU and catalog fixes (snapshot encoding, tuner reads, the change-feed index, the
  first look with JIT off, incremental decision purge, sampled self-configuration). Posture
  inherits the first look's JIT-off setting without a spec change.
- "Grant more" (onboarding) predates `72646ab1`.
- Unmerged branches (for example `claude/grant-more-create-schema`) are not part of the baseline
  and are not cited.

## Decisions recorded (2026-10-09)

- **Decided by the owner:** D-1 (delete AgentDB provisioning; keep the narrowed broker as
  sandboxes and clones), D-2 (the pull-gate thresholds), D-4 (no multi-team installs; E3 not
  moved before G1), D-7 (no hosted pg_sage before G2; brokered lane customer-VPC only; revisit
  at G2).
- **Defaults, reversible, under the owner's delegation of product calls:** D-3 (seek a design
  partner during G0/G1), D-5 (`single_operator_mode` on for lifeos, with its review queue), D-6
  ("Agent Guard", pending a trademark check before launch copy), D-8 (keep AGPL for now). §14
  gives each reason.
- **Updated:** the status line, the header summary, §0 (the D-1 paragraph and the rename bullet),
  §1.5, §1.6 (both D-1 rows), §2.5 (D-1 and D-4), §5.1 (D-7), §6.11 (D-5), §9 (D-5 comment),
  §10.1 (D-6 trademark check), §10.6 (G0 gate met; G1 thresholds decided; D-3 on G3 and G4), §14.
- No recommendation's substance changed.

## Open items added

- **O-5: where the observe tier keeps its results.** The G4 gate assumed v2.3's
  `history.store: meta` would take the `sage` schema out of estate databases. It doesn't: findings,
  first-look reports and the action log stay in each monitored database, and bootstrap creates a
  `sage` schema in every one. The design still holds, but G4 must also keep observe-tier results
  in the meta DB, and its ≈10 agent-day effort is re-estimated before G4 starts.

## Not verified

- **"About 60 endpoints"** (§0). AgentDB routes dispatch through a sub-router
  (`internal/api/agent_db_handlers.go`), so the count can't be read off route registrations. The
  files are unchanged since `72646ab1`, so the figure is carried from the audit (prior-specs
  §7.4: 62 of about 139 method and path pairs in July).
- **Live-provider claims** (§0). The live runs' receipts are git-ignored and weren't re-run.
- **Performance.** Neither `TestPerfGate` nor `TestPerfGateFirstLook` was run (no database was
  used). The spec names the gates; it doesn't assert that today's numbers pass them.
- **External facts:** PostgreSQL 14's end-of-life date (§6.6) and market or research figures
  other than the star count weren't re-checked; they aren't code citations.
