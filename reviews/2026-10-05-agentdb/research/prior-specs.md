# AgentDB: review of the prior specs, plans, reports and decisions (2026-10-05)

**Purpose.** This is research input for `AGENTDB-SPEC.md`. It reviews every earlier document about
AgentDB (agent databases) and the roadmaps around it. It dates each one, traces how the thesis
changed, and checks each promise against the code with quick `git grep` checks. A separate agent
does the deep code audit, so any item not settled here is marked **UNSURE** and listed in §8.4.

**Repo state.** Worktree `C:/Users/jmass/pg_sage-agentdb-spec`, branch `claude/agentdb-spec` =
`origin/master` @ `72646ab1` (after the v2.2.0 tag, 2026-10-05). Paths are relative to the repo
root, and code paths start with `sidecar/`. Dates come from `git log --follow`. Where a document
states its own date, it is shown with the commit date in brackets.

**Status vocabulary (promise ledger, §3).**
- **BUILT:** present and wired in production code.
- **PARTIAL:** present but narrowed, unwired, or missing a material part of the promise.
- **NOT BUILT:** no code found.
- **ABANDONED:** decided against, or built and then removed.
- **CONTRADICTED:** the code (or a later binding decision) does the opposite of the promise.
- **UNSURE:** a quick check could not settle it; it is handed to the code audit.

**Safety note.** Some committed reports contain plaintext local QA admin passwords (§5.4). They
are not reproduced here.

---

## 0. Key findings

1. **The thesis changed six times in five months, and AgentDB never had a user.**
   - April: an *observer* of databases that agents create (the "Agent Database Guard").
   - May 7: a *governed provisioner* (schema and external modes only).
   - May 7–10: a *multi-cloud provisioning control plane* with Terraform and LLM blueprints.
   - June: an *autonomous operations layer for thousands of agent databases*.
   - July: absorbed into the core "no-human database" lane, where AgentDB itself is never mentioned.
   - September: a "lifecycle contract", then a hardening target.
   - October: absent from the roadmap.

   No document records a user, a design partner or a production deployment. The 2026-09-26 review
   asks outright whether 12k LOC is right "for a feature with no production users"
   (`reviews/2026-09-26/group-08-agentdb.md:470-473`).
2. **The observer half was built, then deleted without a note.** The April research rated per-agent
   identity as the foundation of everything else (`research/v1_agent_created_databases.md:129`). It
   was built on 2026-05-08 as `sidecar/internal/analyzer/agent_workload.go` (commit `3367ffa2`). v1.2
   deleted it on 2026-06-11 (commit `1683e2e5`), and neither the commit message nor the CHANGELOG
   mentions the removal. The May 7 spec still calls it "current behavior"
   (`docs/superpowers/specs/2026-05-07-agent-database-deployment-module.md:106-118`).
3. **Scope grew past every boundary the specs set.**
   - The May 7 spec says AgentDB "should not turn pg_sage into a full AgentDB product". Its v1 was
     `schema` + `external`, with `database` and `branch` returning `defer` (`...module.md:13-30,
     749-750`).
   - The same day, the expansion spec executed local `CREATE DATABASE` and planned RDS, Cloud SQL and
     Lakebase instances.
   - By May 10 there was live cloud execution, Terraform upload and LLM blueprints. By September 7,
     Neon and Supabase had been added.
   - Today the code is about 16.7k lines of production Go (`agentdb` 13,689, `api/agent_db_*` 2,638,
     `cmd` 375), 3.5k lines of UI and 27 `sage.agent_*` tables.
4. **The authorization layer is the best part. The lifecycle around it was unsafe until 2026-09-26.**
   - G8 found 6 P0s: a ping could rewrite lifecycle state, archived resources were never torn down,
     re-registering orphaned a live resource, destroy went by a derived name with no ownership check,
     there was no tenant isolation, and an ambiguous create was orphaned.
   - All P0 fixes landed, and every named regression test exists on master (§6.2).
   - About 20 deferred items are still open, checked against code (§6.3).
5. **AgentDB has its own authority stack that never touches `policy.Gate` or `Executor.Apply`.**
   - `sidecar/internal/agentdb` imports neither `internal/policy` nor `internal/executor`.
   - The TTL reconciler destroys live cloud resources every 300 s by default. It authorizes itself
     through a durable claim (`teardown_reconcile.go:99-130`), with no shadow mode, earned trust,
     approval card or notification.
   - This contradicts the product principle (§5.3), even though databases *inside* the fleet have
     gone through the gate since v1.7.0.
6. **The main value promised to agents cannot be reached by agents.**
   - After the G8-B05 fix, agent tokens reach only four routes: list, get, create request and list
     requests (`sidecar/internal/api/agent_db_agent_api.go:62-69`).
   - Recommendations, feedback, cost samples and lease extension are operator-session only
     (`agent_db_handlers.go:24-38,180-189,233`).
   - Nothing produces recommendations except an operator POST (`agent_db_handlers.go:479-482`).
   - The README still advertises "agent-facing query recommendations" (`README.md:73`).
7. **Evidence has decayed.**
   - Live cloud runs: RDS, Cloud SQL and Lakebase on 2026-05-09/10. Neon and Supabase are documented
     as verified in v1.5.0 (2026-09-07).
   - The runners were rewritten on 2026-09-26 with ownership checks, unprotect-before-destroy and
     `create_uncertain`, and have not run against a real cloud since ("No real cloud APIs were
     called", `reviews/2026-09-26/fixes-agentdb.md:7`).
   - CI permanently allow-lists the live tests as skips (`sidecar/.skip-allowlist`).
   - The UI's live create and destroy were broken from v1.3 (2026-07-19, when `authorize-live` was
     added) until 2026-09-26, and nobody noticed.
8. **AgentDB is stranded.** About 1,264 commits landed between 2026-09-28 and 2026-10-05. One
   touched an AgentDB path, and it was a mechanical `main.go` split. The 2026-10-02 ROADMAP never
   mentions AgentDB. Coding agents now get a separate MCP v2 interface with separate `pgs_mcp_`
   tokens, and managed-cloud APIs went into a separate `internal/cloudtel` (§7).
9. **Parallel infrastructure stacks grew without reuse.**
   - Disposable-database stacks: AgentDB `ProviderRunner` (five clouds) and `internal/clone`, whose
     managed snapshot adapter returns "unavailable" (`cmd/pg_sage_sidecar/mcp_migration_runtime.go:23-27`).
   - GCP/RDS clients: AgentDB's own, and the `internal/cloudtel` ADC chain and RDS client built
     2026-10-04/05.
   - Non-human credentials: ping tokens, `agt_` agent tokens and `pgs_mcp_` MCP tokens.
10. **D4 is the only formal AgentDB product decision.** Humans are global, with one operator team
    per install. Approvals are attributed and single-use. A cloud register needs an approved,
    unused request. D4 defers per-human tenant scoping to "roadmap AgentDB principals"
    (`reviews/decisions/LEDGER.md:22`), and that item is on no current roadmap.

---

## 1. Timeline

### 1.1 Documents

| # | Date (commit; later edits) | Document | Purpose | Key decisions | Status claims, and reality |
|---|---|---|---|---|---|
| 1 | 2026-04-27 | `specs/autonomous-dba-product-spec-2026-04-27.md` | Product spec for the autonomous DBA agent (Observe → Diagnose → Decide → Act → Verify → Remember) | "Agent" means pg_sage itself. No agent databases. | Frames the product principle; predates AgentDB |
| 2 | 2026-04-13; 2026-04-07 | `research/ROADMAP_SYNTHESIS.md`, `roadmap.md` | Early roadmaps | None about AgentDB | Public `roadmap.md` never mentions AgentDB (stale) |
| 3 | 2026-04-29 (commit 05-08) | `research/v1_agent_created_databases.md` | Explores agent-created databases: 5 kinds, workload signature, failure modes, gaps | Top 5 bets: identity first, DDL gate, workload fingerprint, memory hygiene ("the moat"), public dogfooding (`:127-137`). Asks whether to offer a write API (`:145`) and how a kill switch should work (`:163`) | None |
| 4 | 2026-04-30 (commit 05-08) | `research/2026-04-29-product-roadmap-review.md` | Roadmap review. Ranks **Agent Database Guard** P1 (`:24`) | Rejects the `pg_sage.remember()` write API (`:225`). Disposable lab DBs allowed; creating production DBs needs explicit intent; teardown audited (`:266-270`) | None |
| 5 | 2026-04-30 (commit 05-08) | `docs/superpowers/plans/2026-04-30-roadmap-slices.md` | Slices | Guard = attribution before autonomy | Built 05-08, deleted 06-11 |
| 6 | ~2026-04-29 (commit 05-08) | `docs/ROADMAP_v1.x_addendum.md` | Opus 4.7 synthesis. Theater B "Agent-Native Operation", items A1–A6 (`:151-186`) | v1.2 = A1/A3/A4/A5 plus deploy requests (`:229-230`). Revisit the write API after 6 months of A1–A6 data (`:186`). Buyer may be "platform team that runs agents" (`:255`) | No A1–A6 shipped as specified |
| 7 | 2026-05-07 (commit 05-08; never edited) | `docs/superpowers/specs/2026-05-07-agent-database-deployment-module.md` (795 lines) | **Pivot:** "Agents are becoming database deployers" (`:7`). pg_sage becomes the DBA layer that provisions, tunes, budgets, backs up and cleans up | v1 = `schema` + `external`; `database`/`branch` return `defer` (`:186-204`). MCP out (`:759`). Never drop immediately (`:753`). 12 resolved decisions (`:747-769`), 5 left open (`:771-783`) | Spec only. Describes the Guard as current behaviour (stale since 06-11) |
| 8 | 2026-05-07 (commit 05-08) | `docs/superpowers/plans/2026-05-07-agent-database-deployment-module.md` | Slice list | Downgrades "scoped role" to "recorded credential scope metadata" (`:17-19`) | All 10 slices ticked by 05-08 |
| 9 | 2026-05-07 (commit 05-08; edit 05-10) | `docs/superpowers/specs/2026-05-07-agentdb-provider-provisioning-expansion.md` (+ plan) | **Expansion:** "provider-aware provisioning control plane" (`:5-8`). Local `schema`/`database` execute; RDS, Cloud SQL and Lakebase plan at instance level | Cloud is instance-only. CLIs are never the execution path (`:40-47`) | Plan checklist never ticked |
| 10 | 2026-05-08/09 (commit 05-08) | 7 slice plans: cloud dry-run executor, audit export, backup assurance, identity/ping tokens, lifecycle controller, promotion deploy requests, recommendation contract | TDD slice plans | Restore drills must never claim verified. Deploy requests are review-only. Tokens are bound to one deployment | 0 of 129 checkboxes ticked, although the code shipped |
| 11 | 2026-05-08 | `docs/reports/2026-05-08-agentdb-work-report.md` | Day report | — | Claims the whole control plane was "implemented" in one day (`:68-83`) |
| 12 | 2026-05-09 (commit 05-10) | `docs/superpowers/specs/2026-05-09-agentdb-blueprint-builder-design.md` (+ plan) | English intent → typed blueprint → draft Terraform | The LLM can't apply anything. Deterministic fallback when no LLM (`:50-53`) | Fallback removed later (G8-D01) |
| 13 | 2026-05-09 (commit 05-10) | `docs/superpowers/specs/2026-05-09-agentdb-live-provisioning-ga.md` (822) + plan (872) | Make live create/destroy GA on RDS, Cloud SQL and Lakebase | 10 principles (`:97-115`). State machine, receipts, typed errors, rate limits, production safety mode, emergency destroy, TTL from `available`. Single-tenant MVP (`:803-805`). Real restore drills out of MVP (`:817-818`) | Plan: 11 coverage boxes ticked, 60 task boxes not |
| 14 | 2026-05-09 (commit 05-10) | `docs/reports/2026-05-09-claude-opus-agentdb-live-provisioning-review.md` | External review folded into GA spec | Single `execute` route with `mode=live` + `cost_estimate_id`. Empty allowlist denies. Dual-control emergency destroy (`:15-37`) | The execute shape was replaced in v1.3; emergency destroy never built |
| 15 | 2026-05-09/10 | `docs/reports/2026-05-09-agentdb-release-readiness-report.md`, `docs/reports/2026-05-09-cloud-provisioning-live-validation.md` | Release evidence | — | Live create/status/delete receipts for RDS, Cloud SQL and Lakebase. Product-chain gauntlets on 05-10: blueprint → RDS, template → Cloud SQL, request → Lakebase (`:324-384`) |
| 16 | 2026-05-10 | `docs/reports/2026-05-10-agentdb-release-hardening-report.md` | Hardening | — | "Functionally strong" for local, UI, API, dry-run and GCP live (`:140-148`) |
| 17 | 2026-05-10 (edit 07-19) | `docs/runbooks/agentdb-live-provisioning.md`, `docs/runbooks/agentdb-cloud-provider-setup.md` | Operator runbooks | Credentials stay outside pg_sage. Cleanup sweeps. Emergency stop by turning live config off | Stale (§5.2) |
| 18 | 2026-05-10 | `CHANGELOG.md:2131-2170` (v1.1) | "AgentDB Cloud Provisioning Release" | LLM-required blueprints | Released |
| 19 | 2026-05-08 (edits 05-10, 07-19, 09-26, 09-27 ×2) | `docs/agent-db-deployments.md` | User doc | Principal model added 09-26; D4 rules added 09-27 | Partly stale (§5.2) |
| 20 | 2026-06-10 (commit 06-11) | `research/v2_agentdb_at_scale.md` | The control plane around thousands of agent DBs | Top 8: schedule the reconciler, spawn-time tuning, metering, quarantine, verify-and-revert indexing, drift, secure defaults, rightsizing (`:101-108`) | Self-diagnosis: "AgentDB is a provisioning ledger, not an autonomous DBA" (`:11`) |
| 21 | 2026-06-10 (commit 06-11) | `docs/ROADMAP_2026H2.md` | H2 roadmap. Theme B "AgentDB at scale (the new theater)" (`:170-224`) | "Claim the AgentDB-at-scale theater" (`:30`). B1 now, then B2/B5, then B3/B4/B6 (`:265`). Finish or cut Terraform execution (`:334`) | Only B1 shipped; Terraform neither finished nor cut |
| 22 | 2026-06-11 (edit 07-19) | `docs/reverse_spec/05-agentdb.md`, `docs/REVERSE_SPEC.md` | As-built reverse spec: LIVE / scaffolding / absent | — | Accurate in July; 3 statements now stale |
| 23 | 2026-06-11 | `CHANGELOG.md:2026-2027` (v1.2) | Fleet integration (B1) and reconciler scheduling (F4) | Silently deletes the Guard (`agent_workload.go`) | — |
| 24 | 2026-07-18 (commit 07-19) | `docs/superpowers/specs/2026-07-18-wave-0-safety-contract.md` | Safety contract | AgentDB authority = intersection of 4 layers. Renewal beats expiry. Adaptive monitoring is the target. "MCP is retired" (`:45-70`) | Built, except monitoring |
| 25 | 2026-07-19 | `CHANGELOG.md:1983-1990` (v1.3) | Remediation | Server-owned plans, exact-operation authorization, leased monitoring claims, "JIT credentials" | The UI was not updated, so live actions broke until 09-26 |
| 26 | 2026-07-22 (commit 07-23) | `research/2026-07-22-agent-native-feature-spec.md` | Agent-native autonomy research | "No-human database" lane = guarantees + standing policy (`:13`). Thin-clone enabler "not yet integrated (no evidence in the repo)" (`:33`) | Never mentions the AgentDB package |
| 27 | 2026-07-22 (commit 07-23; edit 09-27) | `specs/agent-native-autonomy-build-spec.md` | Build spec | `clone.Provider` with DLE and snapshot-restore adapters (`:25`). MCP re-introduced (`:26`). Profiles `staffed`/`unattended` (`:28`) | Snapshot adapter still unavailable |
| 28 | 2026-09-04 (commit 09-07) | `research/2026-09-04-product/report.md` | Product research | "Agent database lifecycle contract" ranked 5th of 5 (`:34`). Outcome contracts (`:174-205`) | None |
| 29 | 2026-09-07 | `docs/neon-supabase.md`, `CHANGELOG.md:1819-1852` (v1.5.0) | Neon and Supabase, including AgentDB project/branch runners (`:68-109`) | Record the server ID, verify ownership, refuse default branches, don't assume native expiry | "Verified live" on Free tiers. The `secret_ref` example is now invalid |
| 30 | 2026-09-26 | `reviews/2026-09-26/group-08-agentdb.md` | Full review | 6 P0, 14 P1, 9 P2, 1 P3, 13 dead or unwired items | Could "leak live cloud resources indefinitely" and "destroy a resource it does not own". The UI could not do a live create or destroy (`:21-27`) |
| 31 | 2026-09-26 | `reviews/2026-09-26/fixes-agentdb.md` | Fix report | Fixes and deferrals (`:12-50`) | All P0s fixed; several P1s partial |
| 32 | 2026-09-26/27 | `reviews/2026-09-26/MASTER-SPEC.md` | Master review | P0-15..19 are AgentDB (`:129-133`). "AgentDB principals + teardown state machine before advertising multi-tenant AgentDB" (`:459`) | None |
| 33 | 2026-09-26 (edit 10-01) | `reviews/2026-09-26/AI-SRE-SPEC.md` | Sage SRE spec | Cloud provisioning excluded from SRE autonomy through R2 (`:131-135`). Game days on DLE clones (`:128-129`) | None |
| 34 | 2026-09-27/28 | `reviews/decisions/D4-agentdb-tenancy-and-registration.md`, `LEDGER.md`, `MORNING-2026-09-28.md` | Decision memo and ledger | D4a: humans global, approver recorded. D4b: approved request before a cloud register (`D4:6-11`) | Done and merged. Owner kept it on 09-28 (`LEDGER.md:74-80`) |
| 35 | 2026-09-27/30 | `CHANGELOG.md:1768-1801` (v1.6.0), `:1480-1530` (v1.7.0) | Releases | Agent DBs get the **full runtime**, with the sage schema bootstrapped into each agent DB (`:1484-1486`). D4 shipped | None |
| 36 | 2026-10-02 | `reviews/2026-10-02-ai-next/ROADMAP.md` (+ `platform.md`, `landscape.md`, `interfaces.md`) | AI-first roadmap | **AgentDB is in no phase.** Landscape: "operating agent-spawned fleets cheaply" is open white space (`landscape.md:147-151`) | `platform.md:27`: "Prototype … no run evidence". `:123`: resolve `secret_ref` (I12) |
| 37 | 2026-10-03 | `reviews/2026-10-03-perf-storage-report.md`, `CHANGELOG.md:598` (v1.8.3) | Retention | Retention rules for `agent_db_*` tables (`sidecar/internal/retention/agent_rules.go`) | None |

### 1.2 Releases that changed AgentDB

| Release | Date | AgentDB change |
|---|---|---|
| v1 | 2026-04-28 | None (no AgentDB) |
| v1.1 | 2026-05-10 | First release: local provisioning, gated live RDS/Cloud SQL/Lakebase, blueprints, Terraform upload, UI |
| v1.2 | 2026-06-11 | Reconciler scheduled; partial fleet sync; **Guard deleted** |
| v1.3 | 2026-07-19 | Four-layer authority, `authorize-live`, plan/estimate/authorization tuple. **Broke the UI live flow** |
| v1.5.0 | 2026-09-07 | Neon and Supabase runners; env `secret_ref` fleet connections; serialized schema init |
| v1.6.0 | 2026-09-27 | G8 fixes (P0-15..19 and most P1s) |
| v1.7.0 | 2026-09-30 | D4; agent DBs get the full `DatabaseRuntime` |
| v1.8.3 | 2026-10-03 | Retention for agent tables (`internal/retention`) |
| v1.8.x – v2.2.0 | 2026-10-02..05 | No AgentDB feature or behaviour change |

---

## 2. How the AgentDB thesis changed

### 2.1 Phases

**Phase 0 (up to 2026-04-28): pg_sage is the agent.** The April 27 product spec defines "Agent" as
pg_sage's own control loop (`specs/autonomous-dba-product-spec-2026-04-27.md:61`). Databases created
by agents are not in scope.

**Phase 1 (2026-04-29/30): the Guard observes databases that agents create.**
- **User.** The team whose Postgres is being shaped by AI agents: vibe-coded schemas, agent memory
  stores, a branch per task. The addendum says the buyer may be "another agent" or a "platform team
  that runs agents" (`docs/ROADMAP_v1.x_addendum.md:37,255`).
- **Problem.** The median agent database "has no human DBA and never had one"
  (`research/v1_agent_created_databases.md:15`). Its schemas are almost right; it suffers memory
  pollution, runaway agents and blurred identity.
- **pg_sage's role.** An agent-aware operational DBA: attribution, workload fingerprint, DDL scoring,
  memory hygiene. The white space is the link between runtime operations and agent context (`:97`).
- **Boundary.** No write API: it "would turn pg_sage from a sidecar/operator into an agent runtime
  dependency" (`research/2026-04-29-product-roadmap-review.md:225`). Lab databases are allowed;
  creating production databases needs explicit intent (`:266-270`).

**Phase 2 (2026-05-07): Agent Database Deployments.**
- **User.** Agents with API tokens and humans in sessions, both requesting databases through one
  policy path. Humans or higher-trust accounts approve, archive and delete (`...module.md:174-178`).
- **Problem.** Agents will ask for temporary schemas, sandboxes, vector stores and preview
  environments (`:7-11`).
- **pg_sage's role.** The DBA layer that provisions, tunes, tracks, budgets, backs up and cleans up,
  but "not a full AgentDB product" (`:13-30`). v1 has only `schema` + `external` modes, and MCP is
  out (`:749,759`).
- **Why it changed.** No reason is recorded. The spec refers to external "AgentDB drafts" (`:13`):
  Forge workspaces, a memory vault, SDKs and hosted AgentDB. Those drafts are not in this repo and
  were not reviewed.

**Phase 3 (2026-05-07 to 05-10): multi-cloud provisioning control plane, then GA.**
- **User.** Platform or operator teams that let agents create real cloud databases. The GA spec lists
  the enterprise buyer's questions: which agent created what, caps on spend, VPCs, SIEM, rotation,
  masking (`docs/superpowers/specs/2026-05-09-agentdb-live-provisioning-ga.md:759-776`).
- **Problem.** "Creating the database is the easy part; safely connecting, governing, proving
  backup/restore, reconciling drift, and bounding cost are the product" (`:39-41`, paraphrased
  closely).
- **pg_sage's role.** A provisioner: five tabs in the UI, Terraform upload, and an LLM that writes
  blueprints. The narrow May 7 scope was dropped within hours, and no document records the reversal
  as a decision.

**Phase 4 (2026-06-10): AgentDB at scale.**
- **User.** A platform buyer running about 10,000 agent databases (`docs/ROADMAP_2026H2.md:296-298`).
- **Problem.** The databases are ephemeral, poorly tuned, used in bursts and multi-tenant, and
  "vendors are racing to *host* agent workloads; none of them operates the resulting databases"
  (`:36-37`).
- **pg_sage's role.** The autonomous operations layer: tune at spawn, rightsize, archive,
  auto-quarantine noisy tenants, meter cost and fix drift.
- **Self-critique.** "AgentDB is a provisioning ledger, not an autonomous DBA"
  (`research/v2_agentdb_at_scale.md:11`). The doc also asks whether pg_sage should be a thin control
  plane over Neon, Aurora Serverless or Lakebase rather than a provisioner (`:118`). Nothing answered
  that question.

**Phase 5 (2026-07-18 to 07-23): the no-human lane moves into the core.**
- The July research defines the "no-human database" lane: databases agents create and nobody
  watches, served by guarantees and fail-closed standing policy
  (`research/2026-07-22-agent-native-feature-spec.md:13`).
- That lane was built in the core: custodians, and a policy profile `unattended` that is the default
  (`sidecar/internal/config/defaults.go:198`). It does not reference AgentDB.
- The same doc says no thin-clone enabler exists (`:33`), although AgentDB had been creating Lakebase
  branches since May. `internal/clone` was built separately.
- MCP was declared "retired" on 07-19 (`docs/superpowers/specs/2026-07-18-wave-0-safety-contract.md:66-70`)
  and re-introduced on 07-23 (`specs/agent-native-autonomy-build-spec.md:26`).
- AgentDB got only safety remediation in this phase.

**Phase 6 (2026-09-04 to 09-07): lifecycle contract, then more providers.**
- The September research reframes the idea as an "agent database lifecycle contract": owner, TTL,
  spend limit, teardown evidence, drift. It ranks it last of five (`research/2026-09-04-product/report.md:34`).
- It asks "Is the first buyer an overloaded DBA, an application developer, or an agent platform?"
  (`:221`), the same question asked in April.
- In practice, v1.5.0 added two more provisioners: Neon and Supabase.

**Phase 7 (2026-09-26 to 09-28): hardening and D4.**
- G8 shows the lifecycle was unsafe and asks whether to "cut to one provider done end-to-end"
  (`group-08-agentdb.md:470-473`).
- D4 settles the user model: one operator team per install, agents as principals bound to a tenant,
  and approvals as evidence for spending money (`reviews/decisions/LEDGER.md:58-61`).
- The runtime refactor gives agent databases the full AI-DBA runtime (`CHANGELOG.md:1484-1486`). This
  is the first time pg_sage *operates* databases it provisioned, but only for those whose
  credentials can be resolved (§3, P-62).

**Phase 8 (2026-10-02 to 10-05): stranded.** The AI-next ROADMAP leaves AgentDB out.
- Coding agents get MCP v2 with scoped `pgs_mcp_` tokens and source-fix packets instead.
- The landscape review names the April white space again: Tiger Ghost and AlloyDB for agents
  "provision and scale those databases; none of them operates them"
  (`reviews/2026-10-02-ai-next/landscape.md:26-29`).
- `platform.md:27` rates AgentDB "Prototype".

### 2.2 The thesis at each phase

| Phase | Who is the user | What job | What pg_sage is | Scope boundary |
|---|---|---|---|---|
| 1 (Apr 29) | DBA or platform team whose databases agents create | "Tell me which agent did this and stop the stereotyped mistakes" | Observer and advisor inside the database | No write API; lab DBs only |
| 2 (May 7) | Agents (tokens) and humans (approvers) | "Give my agent a governed workspace and clean it up" | Governed provisioner (schema/external) | Not a full AgentDB product; no MCP |
| 3 (May 9) | Platform team; enterprise buyer | "Let agents create cloud databases without leaks or unbounded spend" | Multi-cloud provisioning control plane | Cloud = instance only; no direct `terraform apply` |
| 4 (Jun 10) | Agent-platform buyer at 10k DBs | "Operate my fleet of ephemeral agent databases cheaply" | Autonomous operations layer | Don't build a storage engine |
| 5 (Jul 22) | Owner of unattended databases (core) | "Guarantee no XID or disk deadline is missed" | Core autonomy; AgentDB unused | Clone substrate separate |
| 6 (Sep 4) | Undecided | "Owner, TTL, spend, teardown evidence" | Lifecycle contract | Ranked last |
| 7 (Sep 27) | One operator team per install; agents as tenant principals | Same as 3 | Provisioner + AI DBA runtime for connectable DBs | Humans global |
| 8 (Oct 2) | Coding agents via MCP v2 (not AgentDB) | "Fix this query at its source" | MCP specialist; AgentDB absent | None |

### 2.3 What stayed constant and what drifted

- **Constant:** operate, don't host (`ROADMAP_2026H2.md:279`, `landscape.md:147-151`). Never drop on
  the first sign of abandonment. Destroy only with backup evidence. Approval and attribution for
  spend. `application_name` is a hint, never authorization (`...module.md:102-104`).
- **Drifted:** observe → provision → operate at scale → contract → nothing. The user went from DBA
  to agent to platform team to a single operator team, and is now unstated. Scope grew from 2
  modes to 5 clouds plus Terraform plus an LLM. Every phase *planned* to operate agent databases;
  only provisioning was ever finished.

---

## 3. Promise ledger

Code evidence uses `sidecar/` paths. "grep: none" means `git grep -i` over `sidecar/internal` and
`sidecar/cmd`, excluding tests, found no match.

### A. Observing agent-created databases (April 29–30, addendum A1–A6, May 7 Guard section)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-01 | Per-agent identity correlation (`application_name` `agent:<id>`, SQL comment `agent_id`), findings for unattributed or ephemeral workloads | v1:109,129; addendum:164; module:106-118 | **ABANDONED** | Built in `analyzer/agent_workload.go` (`3367ffa2`, 05-08); deleted in v1.2 (`1683e2e5`) with no stated reason. Partial revival elsewhere: MCP v2 `query_sources` and source-fix packets read sqlcommenter tags and `application_name` (`internal/mcp/agent_tools.go:48,73`) to attribute code, not agents |
| P-02 | Agent-flavored workload fingerprint | v1:105,133; addendum:166 | **NOT BUILT** | grep: none |
| P-03 | DDL gate for agent DDL (`ddl_command_end` scoring, block or auto-fix) | v1:117,131; addendum:165 | **NOT BUILT** | No event trigger. The core has a migration linter (MCP `lint_migration`), which is not agent-scoped |
| P-04 | Memory hygiene: stale embeddings, orphan rows, duplicates | v1:113,135; addendum:167 | **NOT BUILT** | grep: none. `vectorlab` is read-only HNSW evidence, not hygiene |
| P-05 | Memory-poisoning detection | v1:119 | **NOT BUILT** | grep: none |
| P-06 | Per-agent cost guardrails from `pg_stat_statements` | v1:115; addendum:168 | **NOT BUILT** | Only cost samples the agent reports itself (P-23) |
| P-07 | Runaway-agent isolation per `application_name` | v1:111; addendum:169 | **NOT BUILT** | Agent-scoped isolation absent. The core's general runaway-query handling is out of scope here |
| P-08 | Orphan-branch detection | v1:123 | **NOT BUILT** | grep: none |
| P-09 | `pg_sage.remember()` agent write API | v1:145 (question) | **ABANDONED** (decision) | Rejected (`roadmap-review:225`), "revisit in 6 months" (`addendum:186`); never revisited |
| P-10 | pg_sage tags and observes its own queries | v1:121,137 | **BUILT** (core) | `/* pg_sage */` tag in v1.2; `selfmonitor`, `selfcost`, `selfbudget` |
| P-11 | Agent-readable database manifest | roadmap-review:365 (question) | **UNSURE** | Not an AgentDB feature. MCP `list_facts`/`top_queries` may cover part of it |

### B. Deployment module (May 7)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-12 | Request API with allow/review/deny/defer decisions | module:137-164 | **BUILT** | `agentdb/policy.go:12-44` |
| P-13 | Idempotency per (tenant, key): same body replays, different body returns 409, a **missing key returns 400**, keys kept 24 h | module:166-172 | **PARTIAL** | Replay and 409 built (`store.go:29-30`). The key is optional, with no 400 and no 24 h expiry |
| P-14 | v1 modes `schema` + `external`; `database`/`branch` return `defer` until an adapter proves create, tag, backup and cleanup | module:186-204,749-750 | **CONTRADICTED** | `database` executes locally; cloud `instance` is live; branches are created through `instance` + `provider_params.mode=branch`, while `branch` isolation still returns `defer` (`policy.go:39-40`). `external` is only a review decision with no provisioning level (`register.go:92-95`) |
| P-15 | Scoped role and credential per schema workspace; role cleanup | module:208-235,751-752 | **NOT BUILT** | G8-B17 deferral; grep `CREATE ROLE`/`GRANT`: none in `agentdb` |
| P-16 | DDL allowlist and required `application_name` for workspaces | module:211-214 | **NOT BUILT** | grep: none |
| P-17 | Lease states active → stale → quarantined → archiving → archived → restore_verified → dropping → dropped | module:347-356 | **PARTIAL** | `status` ∈ {active, archived, deleted, budget_exceeded} plus `provisioning_status`; no stale or quarantined state |
| P-18 | Heartbeat 15 min, stale after 2 misses, quarantine at 24 h, archive at 48 h | module:358-366 | **NOT BUILT** | Ping is liveness-only (G8-B01 fix); abandonment is lease expiry only |
| P-19 | Agents may extend their own lease within policy | module:174-178,369-370 | **NOT BUILT** for agents | `extend-lease` is operator-only (`api/agent_db_handlers.go:182`); the agent API has 4 routes (`agent_db_agent_api.go:62-69`) |
| P-20 | Cleanup ladder: notify → disable or read-only → backup → verify → drop; drop needs ownership tag, tenant match, e-stop clear, fleet drop-rate limit | module:372-392 | **PARTIAL** | Archive, restore gate, ownership check and e-stop gate built. No notify, read-only or drop-rate limit |
| P-21 | Backup modes, daily readiness, `pg_dump` schema archive with manifest and checksum, restore into a throwaway schema; "file exists" is not verification | module:394-458 | **NOT BUILT** | Backup records and checks exist. The local backup is a *planned* `pg_dump --schema-only` command, never run (`providers.go:332-341`). `restore_verified` is an admin attestation with evidence (fixes:24) |
| P-22 | Encrypted backups with recorded storage location | module:449-451 | **NOT BUILT** | No archives are produced |
| P-23 | Database-observable cost signals: size, bloat, WAL, query time | module:462-485 | **NOT BUILT** | Cost samples are self-reported (`reverse_spec/05:262-264`) |
| P-24 | Hard and soft budgets; read-only quarantine at the hard limit | module:487-496,763 | **PARTIAL** | `budget_exceeded` status; `BudgetGate` wired into live issuance (fixes:49); no quarantine or provider action (G8-B29 deferred) |
| P-25 | Chargeback labels, top-N spend, archive cost forecast | module:493-496 | **NOT BUILT** | grep: none |
| P-26 | Promotion: schema diff, generated migration, data movement plan, RLS review, plan comparison, apply with backup and verify | module:498-528 | **PARTIAL** | Review-only records of SQL that operators supply (`deploy_requests.go`; `reverse_spec/05:285-290`). Nothing generated or executed. The UI still says "Promotion deploy requests" (`web/src/pages/agentdb/PromotionPanel.jsx:48`) |
| P-27 | Machine-readable recommendations for agents: patch hints, supersession, feedback | module:258-341 | **PARTIAL** | Store and API exist (`operations.go:12`). The only producer is an operator POST (`api/agent_db_handlers.go:479-482`); no analyzer or optimizer feeds it. Agent tokens can't reach it (`:184-189`) |
| P-28 | Tuning packs for vector, PostGIS, JSONB and extension configuration | module:237-256 | **PARTIAL** | Static text keyed on self-declared `metadata.workload_types`/`extensions` (`tuning.go:5-22`); no observation |
| P-29 | Agent DBs UI: queue, deployments, lifecycle, cost, backups, recommendations | module:530-550 | **BUILT** | `web/src/components/Layout.jsx:40`; about 3.5k LOC |
| P-30 | DDL/object audit (`agent_db_objects`) and an "objects and DDL audit" tab | module:542,598 | **NOT BUILT** | Table absent from the 27 `sage.agent_*` tables |
| P-31 | Audit is append-only (app roles can't UPDATE/DELETE); JSONL export | module:638-640,769 | **PARTIAL** | JSONL export built. No trigger or grant enforcement (`schema_statements.go:317-323`). 21 discarded audit-write errors (`_ = s.audit`) |
| P-32a | Enterprise: SSO/OIDC for humans; workload identity federation for agents | module:556-557 | **PARTIAL** | Human OIDC built in core (D7); agents use static `agt_` bearer tokens |
| P-32b | Enterprise: secrets integration (Vault, cloud secret managers, rotation) | module:558-560 | **PARTIAL** | AWS-managed master secret ARN only; `env:PG_SAGE_AGENTDB_*` references; no resolver for secret managers (P-82) |
| P-32c | Enterprise: data classification and masking | module:561-562,757-758,765-766 | **PARTIAL** | A masking policy *ID* is required for sensitive classes (`policy.go:23-27`); no masking anywhere (`reverse_spec/05:243-244`) |
| P-32d | Enterprise: egress controls (FDW, `COPY PROGRAM`, exports) | module:563-564 | **NOT BUILT** | Not in `agentdb` |
| P-32e | Enterprise: kill switch per agent, team or deployment; quarantine; forensic timeline | module:571-572 | **PARTIAL** | Fleet e-stop gates AgentDB mutations (fixes:31); tokens can be revoked; no per-agent kill switch or quarantine |
| P-32f | Enterprise: supply-chain fields (client version, prompt pack, repo SHA) | module:573-574 | **NOT BUILT** | grep: none (UNSURE whether `metadata` is used for this informally) |
| P-32g | Enterprise: approval SLAs, stale-owner fallback, owner paging | module:575-576,782-783 | **NOT BUILT** | `approval_sla_seconds` is stored as a policy-reason string only (`docs/agent-db-deployments.md:243`) |
| P-32h | Enterprise: operational metrics (allow/deny ratios, quarantines, restore failures, stale counts) | module:584-586 | **NOT BUILT** | No AgentDB Prometheus metrics (grep over metrics/runtime files: none) |
| P-33 | Deployment-bound ping tokens, hashed, rotate and revoke, rate-limited | module:180-182 | **BUILT** | `identity.go`; per-deployment failure cap since G8-B24 |

### C. Provider expansion (May 7–10)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-34 | Local `schema` and `database` provisioning that executes | expansion:12-31,40 | **PARTIAL** | Built, but off by default since G8-B17 (`PG_SAGE_AGENTDB_LOCAL_PROVISIONING`). No role, credentials or teardown DROP |
| P-35 | Cloud instance plans for RDS, Cloud SQL and Lakebase | expansion:12-15,31-36 | **BUILT** | Plus Neon and Supabase (v1.5.0) |
| P-36 | Custom size profiles (CRUD and seeded defaults) | expansion:49-62 | **BUILT** | `profiles.go` |
| P-37 | Provider readiness | expansion:66 | **BUILT** | Reported "missing" for every provider until G8-B15 was fixed |
| P-38 | Provider CLIs are never the execution path | expansion:40-47 | **BUILT** | SDK and REST runners |

### D. Blueprint builder (May 9)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-39 | English intent → typed blueprint → Terraform draft → policy → approve → provision | blueprint:30-40 | **BUILT** | Since D4, provisioning also consumes an approved request (`CHANGELOG.md:1517-1521`) |
| P-40 | Deterministic parser when no LLM is available | blueprint:50-53 | **CONTRADICTED** | Production returns `ErrBlueprintLLMRequired`; the heuristic generator is test-only (G8-D01, `92bd7999`). This matches the product principle (LLM on by default) |
| P-41 | The approved Terraform is what gets created | GA:666-669 | **PARTIAL** | G8-B20 fix: `multi_az`/engine version honoured; subnet group and security groups deferred |

### E. Live provisioning GA (May 9)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-42 | Runner interface including `RestoreDrill` | GA:254-296 | **PARTIAL** | No `RestoreDrill` method (`provider_runner.go:39-47`) |
| P-43 | Live runners for RDS, Cloud SQL and Lakebase branches; full Lakebase instances behind a flag | GA:298-303,800-801 | **BUILT** | Lakebase instance mode hard-disabled (`runtime_registry.go:78`) |
| P-44 | Creation receipt written before the provider call; crash recovery | GA:224-231 | **BUILT** | `create_operation_id` + `create_uncertain` (fixes:19) |
| P-45 | Derived name `pgs-` + sha1(id)[:20] | GA:199-215 | **CONTRADICTED** | `pgsage-<normalized>`, hashed only for long IDs (`provider_naming.go:10-27`); ownership verification used instead (fixes:17) |
| P-46 | No plaintext secrets in rows, logs, audit or UI; recursive redaction | GA:308-380 | **BUILT** | `provider_redaction.go`; secret-shaped settings rejected |
| P-47 | Credentials to connect: AWS Secrets Manager master, GCP IAM or Secret Manager, Lakebase credential generation | GA:341-363 | **PARTIAL** | AWS only; GCP and Lakebase deliver none (G8-B09 deferred) |
| P-48 | Config for rate limits (`max_live_creates_per_hour/day`, dimensions), `require_private_network_for_production`, production safety mode | GA:386-446 | **NOT BUILT** | `AgentDBConfig` has 4 keys plus per-provider allowlist/TTL/cost (`internal/config/config.go:144-160`); grep: none |
| P-49 | An empty allowlist denies; `"*"` allows all | GA:425-426 | **BUILT** | `effective_policy.go:251-271` |
| P-50 | The TTL countdown starts when the resource reaches `available` | GA:430-431 | **CONTRADICTED** | The lease is set at register (`queries.go:51`, default 3600 s at `register.go:104-105`); no reset found at `available` (code audit to confirm) |
| P-51 | Cost estimate expires after 15 min | GA:595-596 | **BUILT** | `live_execution_issue.go:132` |
| P-52 | Low-confidence estimates doubled; unknown instance class fails closed | GA:557-559 | **BUILT** | After G8-B08 |
| P-53 | Networking: RDS subnet group and security groups; Cloud SQL public IP only via a verified proxy; block `0.0.0.0/0` | GA:452-485 | **PARTIAL** | Public IP and `0.0.0.0/0` blocked; subnet/SG not built; proxy verification UNSURE |
| P-54 | Emergency destroy: admin-only, dual control, `unsafe_destroy` audit | GA:528-537 | **NOT BUILT** | grep `force_destroy`/`unsafe_destroy`: none |
| P-55 | In-flight cancel for `queued`/`provisioning` | GA:756 | **NOT BUILT** | States are declared only (`provider_runner.go:197-199`; G8-D06 never deleted) |
| P-56 | UI tabs, disabled-reason text, confirm before live | GA:598-630 | **BUILT** | Live flow broken from v1.3 (07-19) to 09-26 (G8-B13) |
| P-57a | Terraform upload with static policy (no tfvars, state, provisioners or unpinned modules) | GA:632-662 | **BUILT** | `terraform_policy.go` |
| P-57b | Parse `terraform validate` / `show -json` | GA:663-665 | **NOT BUILT** | Regex manifest (`reverse_spec/05:203`) |
| P-57c | Templates run through the same gates as built-in runners | GA:666-669 | **CONTRADICTED** | Relabelled review-only and bound by hash (SURF-05, fixes:34); the H2 roadmap's "finish or cut" (`:334`) was not done |
| P-58 | Reconcile handles stale state and partial deletes; no double destroy across sidecars | GA:695-696,740-741 | **BUILT** | Advisory lock and claims; G8-B02/B23 fixes |
| P-59 | Live E2E tests behind flags (RDS, Cloud SQL, Lakebase, gauntlet) | GA:698-711 | **PARTIAL** | The tests exist. Last run 2026-05-10 (Neon/Supabase ~09-07). Runners changed on 09-26. Permanently skip-allowlisted in CI (`sidecar/.skip-allowlist`) |
| P-60 | Operator runbook | GA:751 | **PARTIAL** | Exists but stale (§5.2) |
| P-61 | Failure messages an agent can act on; typed errors | GA:233-247,757 | **PARTIAL** | Typed kinds exist (`provider_errors.go:12-19`); quality UNSURE |
| P-62 | Agents can request deployments, poll status and fetch recommendations; cannot bypass gates | GA:624-630 | **PARTIAL** | Request and poll built (agent API). Recommendations unreachable (P-27). Gates hold |

### F. At scale and H2 Theme B (June 10)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-63 | B1/F4: schedule the reconciler; bring AgentDBs into the fleet so the collector, analyzer and executor apply | v2:101; H2:175-181 | **PARTIAL** | Reconciler scheduled (`cmd/pg_sage_sidecar/agentdb_reconciler.go:13-39`, every 300 s). Full runtime since v1.7.0 (`agentdb_fleet.go:176-206`), but only for deployments with an `env:PG_SAGE_AGENTDB_*` DSN or inline credentials. Cloud-created DBs can't join without a manual env DSN (`platform.md:76`) |
| P-64 | B2: zero-touch tuning at spawn | v2:23,102; H2:183-189 | **NOT BUILT** | grep: none |
| P-65 | B3: rightsizing or scale-to-zero | v2:43,108; H2:191-198 | **NOT BUILT** | grep: none |
| P-66 | B4: auto-quarantine noisy neighbours | v2:63,104; H2:200-207 | **NOT BUILT** | grep `quarantine`: none |
| P-67 | B5: measured metering and chargeback | v2:73,103; H2:209-214 | **NOT BUILT** | Self-reported samples only |
| P-68 | B6: config templates per profile and drift fixing | v2:83,106; H2:216-224 | **NOT BUILT** | grep: none |
| P-69 | Secure spawn defaults (`REVOKE` on public, split roles) | v2:93,107 | **NOT BUILT** | grep: none |
| P-70 | Verify-and-revert indexing on agent DBs | v2:33,105 | **UNSURE** | Agent DBs get the full runtime with a "fresh trust ramp" (`CHANGELOG.md:1486`); effective trust level and outcomes unverified |
| P-71 | Ladder: idle → suspend → export to S3 → destroy; archive moves data | v2:53 | **NOT BUILT** | "Archive" is a status (`reverse_spec/05:362-366`) |

### G. Wave-0 contract and the July programme

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-72 | Authority = intersection of runtime, global, provider and operation layers; fail closed | wave-0:45-58 | **BUILT** | `effective_policy.go`, `agent_db_live_authority.go` |
| P-73 | Renewal-safe, single-winner expiry | wave-0:56-58,93 | **BUILT** | `lifecycle_claim.go`, `extendLeaseSQL` (`register.go:120-140`) |
| P-74 | Tiered adaptive monitoring | wave-0:60-65 | **PARTIAL** | Schedule and claim contracts exist; no production caller of `ScheduleMonitoring`/`ClaimMonitoringWork` (`monitoring_schedule.go:17`, `monitoring_claims.go:46`) |
| P-75 | `clone.Provider` managed snapshot-restore adapter | build-spec:25 | **NOT BUILT** | Factory returns "managed snapshot clone adapter is unavailable" (`cmd/pg_sage_sidecar/mcp_migration_runtime.go:23-27`); AgentDB runners not reused |

### H. G8 improvement proposals beyond the bug table

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-76 | `pg_sage_install_id` tag | G8:146-149,456-457 | **NOT BUILT** | grep: none; declined (fixes:17) |
| P-77 | Durable teardown state machine with blocked reason and backoff | G8:372-378 | **PARTIAL** | `teardown_blocked_reason/at` exist; no `next_attempt_at`/attempts/backoff |
| P-78 | Blocked-teardown alerts and live burn metrics | G8:377,466-467 | **NOT BUILT** | `agentdb` imports neither notify, alerting, value nor metrics |
| P-79 | Provider-native TTL backstop (EventBridge delete, expiry tags) | G8:444-447 | **NOT BUILT** | The `ttl` tag is set at create only (`aws_rds_runner.go:146,169`); only Lakebase branches self-expire |
| P-80 | Documented IAM policy conditioned on tags | G8:452-455 | **NOT BUILT** | Runbook IAM lists unconditioned actions (`agentdb-cloud-provider-setup.md:107-113`) |
| P-81 | Commissioning gates: provisioned ≠ connected ≠ monitored ≠ backed up | MASTER-SPEC:290 | **NOT BUILT** | `available` is the end state |
| P-82 | One `SecretResolver` (AWS and GCP Secret Manager, Lakebase OAuth) to hand off credentials | G8:411-416; platform.md:123 (I12) | **NOT BUILT** | `env:` only (`cmd/pg_sage_sidecar/agentdb_secret_ref.go:22-61`) |
| P-83 | Store agent snapshots in the meta DB; scale-to-zero-aware cadence; analyzer advisory-only | G8:313-314,418-423 | **CONTRADICTED** | v1.7.0 bootstraps the sage schema *into each agent DB* and runs the standard runtime (`CHANGELOG.md:1484-1486`); scale-to-zero cadence deferred (fixes:37) |
| P-84 | MCP tools for agents using the same principal | G8:386 | **NOT BUILT** | The MCP tool list has no AgentDB tool (`internal/mcp`) |

### I. D4 (September 27)

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-85 | Record the approver or denier (`decided_by`, `decided_at`) | D4:137-143 | **BUILT** | `queries.go:7,16,23`; `TestRequestApprovalRecordsSessionActor` |
| P-86 | A cloud register needs an approved, unused request; blueprints and templates consume one | D4:152-161 | **BUILT** | `agentdb/types.go:23`; `api/agent_db_register_handler.go:74`; `TestRegisterCloudInstanceWithoutRequestRejected` |
| P-87 | Additive per-user tenant grants for humans | D4:145-150 | **NOT BUILT** | Deferred by decision |

### J. October 2 roadmap reviews

| ID | Promise | Source | Status | Evidence / notes |
|---|---|---|---|---|
| P-88 | Resolve `secret_ref` so agent DBs join the fleet (I12) | platform.md:123 | **NOT BUILT** | Same as P-82 |
| P-89 | Operate agent-spawned fleets cheaply: policy templates, per-tenant LLM budgets, abandoned-object detection, TTL reconciliation at thousands of DBs | landscape.md:147-151 | **PARTIAL** | TTL reconciliation only (bounded at 100 per pass); `FleetBudget` exists in the core |

### Ledger totals

| Status | Count (of 98 rows) | IDs |
|---|---|---|
| BUILT | 22 | P-10, 12, 29, 33, 35, 36, 37, 38, 39, 43, 44, 46, 49, 51, 52, 56, 57a, 58, 72, 73, 85, 86 |
| PARTIAL | 25 | P-13, 17, 20, 24, 26, 27, 28, 31, 32a, 32b, 32c, 32e, 34, 41, 42, 47, 53, 59, 60, 61, 62, 63, 74, 77, 89 |
| NOT BUILT | 41 | P-02–08, 15, 16, 18, 19, 21, 22, 23, 25, 30, 32d, 32f, 32g, 32h, 48, 54, 55, 57b, 64–69, 71, 75, 76, 78–82, 84, 87, 88 |
| ABANDONED | 2 | P-01 (silently), P-09 (by decision) |
| CONTRADICTED | 6 | P-14, 40, 45, 50, 57c, 83 |
| UNSURE | 2 | P-11, P-70 (sub-points of P-53 and P-61 are also UNSURE; see §8.4) |

Each row is counted once, under the status in its Status column.

**Pattern.** Everything that governs a create or destroy was built and hardened: authority,
receipts, ownership, idempotency, approvals. Almost everything that would make pg_sage *operate*
agent databases was not built: attribution, cost truth, real backups and restores, hygiene,
spawn-time tuning, quarantine, metering, credential handoff, agent-reachable recommendations.

---

## 4. Decisions made and questions left open

### 4.1 Decisions

| ID | Date | Decision | Source | Rationale given | Status today |
|---|---|---|---|---|---|
| DEC-01 | 04-30 | No agent write API (`pg_sage.remember`) in v1.x; revisit after 6 months of A1–A6 data | roadmap-review:225; addendum:7-8,186 | It would make pg_sage an agent runtime dependency | Held. The A1–A6 data was never collected because the Guard was deleted. The revisit date (~2026-10/11) is arriving with nothing to go on |
| DEC-02 | 05-07 | AgentDB stays narrow: deployment, tuning, lifecycle, cost, safety. Memory vault, tracing, SDKs, MCP gateway, HNSW planner, hosted AgentDB, pricing and full IAM belong elsewhere | module:13-30,642-665 | Protect the DBA mission | Memory, tracing and SDKs stayed out. The *narrowness* was abandoned (5 providers, Terraform, LLM blueprints) |
| DEC-03 | 05-07 | v1 = `schema` + `external`; true DB creation and branches gated by adapters | module:749-750 | Safest first | Reversed the same day by the expansion spec; never recorded as a decision |
| DEC-04 | 05-07 | Never drop immediately: notify → quarantine/read-only → archive → verified restore → optional drop; drop needs human approval | module:753-754,763-764 | — | Partial. The TTL reconciler destroys live resources with no per-drop approval once a restore attestation exists (`teardown_reconcile.go:99-130`) |
| DEC-05 | 05-07 | Recommendations expose patch hints; no repo PRs | module:755-756 | — | Held for AgentDB. MCP v2 source-fix packets now let a coding agent open the PR (other subsystem) |
| DEC-06 | 05-07 | MCP out of scope for this module | module:759 | — | Held for AgentDB. The product removed MCP on 07-19, brought it back on 07-23 and shipped v2 on 10-04 |
| DEC-07 | 05-07 | HNSW autotuning is a separate product | module:760-761 | — | Held |
| DEC-08 | 05-07 | Production data only through masked copies; masking needed for sensitive data | module:757-758,765-766 | — | A gate on an ID only; no copying or masking exists |
| DEC-09 | 05-07 | JSONL is the minimum audit export | module:769 | — | Built |
| DEC-10 | 05-07 | Cloud = instance only; plans, not CLIs | expansion:35-47 | Avoid paid resources from one click | Held |
| DEC-11 | 05-09 | Dry-run by default; live needs explicit global and provider enablement; no plaintext secrets; every paid resource tagged; delete is harder than create; reconcile is first-class | GA:97-115 | Adoption risks per provider docs (`:37-66`) | Held |
| DEC-12 | 05-09 | MVP tenancy: a single admin/operator team | GA:755,803-805 | Defer multi-tenant RBAC | Became D4a |
| DEC-13 | 05-09 | Real restore drills, SIEM, credential rotation, branch promotion and DR are post-GA | GA:807-822; Opus:39-50 | Scope | Still post-GA; no follow-up |
| DEC-14 | 05-09 | The LLM proposes blueprints; pg_sage validates; drafts are never applied | blueprint:5-8,55-66 | Authority stays deterministic | Held. Blueprints now require an LLM |
| DEC-15 | 07-18 | Four-layer authority intersection; renewal beats expiry; monitoring cadence grants no authority | wave-0:45-65 | Fail closed | Built |
| DEC-16 | 09-26 | Fix-lead choices: ownership verification instead of collision-free names; `restore_verified` only by admin attestation with evidence; Terraform templates review-only; Supabase per-project passwords derived by HMAC; local DDL opt-in; no new `oauth2` dependency | fixes:17,22,24,30,32,34 | Smallest safe change | Built. The no-oauth2 choice is undercut by `cloudtel` building an ADC chain a week later (§5.1) |
| DEC-17 | 09-27 | **D4a:** humans global (one operator team per install); approver recorded | D4:6-8; LEDGER:22,58-61 | Tenant-scoping humans "adds ceremony without a user"; an approval that isn't linked, used or attributed "is not evidence" | Built; the owner kept it 09-28 |
| DEC-18 | 09-27 | **D4b:** a cloud register needs an approved, unused request, and so do blueprint and template provisioning | D4:8-9; CHANGELOG:1509-1521 | Approval is how pg_sage earns the right to spend cloud money | Built. A cloud-instance request with any positive budget is auto-approved by policy (`policy.go:31-35`), so the human gate is the admin `authorize-live` |
| DEC-19 | 09-27 | (Implicit, in the refactor) agent DBs get the full `DatabaseRuntime`, including the sage schema bootstrap | MORNING:63-67; CHANGELOG:1480-1499 | Parity across modes | Built. Conflicts with G8's advice (meta-DB storage, scale-to-zero cadence) and the v2 "fleet-lite" question |

### 4.2 Open questions (deduplicated)

| ID | Question | First raised, later raised again | Status |
|---|---|---|---|
| Q-1 | Who is the user: DBA, platform team, agent platform or app developer? | roadmap-review:354; addendum:255; H2:296-298; Sep-report:221 | **Open.** D4 answers tenancy, not the market |
| Q-2 | Is AgentDB one product with the core DBA or a second product? | H2:299-301 | **Open.** In practice: same repo, separate authority |
| Q-3 | Provisioner, or thin layer over Neon, Lakebase and Aurora Serverless branches? What is left if the hosts build the DBA layer? | v2:118; H2:302-305 | **Open** |
| Q-4 | Which provider first, or cut to one end to end? | module:773-775; G8:470-473 | **Open.** All five were built |
| Q-5 | Should schema backup verification always test a restore? | module:778-779 | **Open.** No archives exist |
| Q-6 | Approval SLA behaviour (escalate, auto-deny, leave pending) | module:782-783 | **Open.** SLA is a reason string |
| Q-7 | One row per branch, or one logical DB with a branch pivot? | v1:149; v2:116 | **Open** |
| Q-8 | Full runtime or "fleet-lite" for thousands of DBs? | v2:114 | **Settled implicitly** (full runtime, DEC-19), never analysed |
| Q-9 | How aggressive should idle → destroy be, and who can veto (kill-switch UX)? | v1:163; v2:120 | **Open** |
| Q-10 | Metering unit of record, and whether to use FOCUS | v2:122 | **Open** |
| Q-11 | Who pays when pg_sage dies or the controller crashes (provider-native backstop)? | G8:444-447; Sep-report:231 | **Open** |
| Q-12 | Is `restore_verified` ever true? | G8:448-451 | **Partly.** Admin attestation with evidence; still no executed restore |
| Q-13 | How far can one sidecar's cloud credentials reach (tag-conditioned IAM)? | G8:452-455 | **Open** |
| Q-14 | Two installs in one account (install ID)? | G8:456-457 | **Open.** Declined in fixes. The adopt-by-tag path for an uncertain create could adopt another install's same-ID resource (UNSURE; §8.4) |
| Q-15 | Multi-tenant SaaS or a single team? | G8:458-459; D4:192-196 | **Settled:** single team per install "until multi-team installs exist" |
| Q-16 | What does an agent receive to connect? | G8:460-462 | **Open.** An AWS secret ARN only |
| Q-17 | Should the TTL reconciler be autonomous by default? | G8:463-465 | **Open.** E-stop gate only; trust ramp deferred |
| Q-18 | Is 12k LOC right for a feature with no users? | G8:470-473 | **Open.** Now about 16.7k LOC in production |
| Q-19 | What does trust mean for a DB no human understands? | roadmap-review:364 | Settled for the core (standing policy, earned trust); **open for AgentDB** |
| Q-20 | Revisit the write API (DEC-01) | addendum:186 | **Due** |

---

## 5. Contradictions and drift

### 5.1 Between documents

1. **The boundary was broken on the day it was drawn.** The May 7 module spec sets v1 to
   `schema` + `external` and "not a full AgentDB product" (`:13,749`). The May 7 expansion spec
   executes `database` and plans cloud instances (`:12-36`). Two days later came GA for live cloud,
   Terraform and LLM blueprints. No document reconciles these.
2. **Branch semantics.** The module spec treats `branch` as an isolation mode deferred to an adapter
   (`:191,204`). The code creates Neon, Supabase and Lakebase branches as `instance` with
   `provider_params.mode=branch`, while request policy still returns `defer` for `branch`
   (`policy.go:39-40`). An agent asking for a branch the documented way is deferred; the working path
   hides branches under "instance".
3. **MCP flip-flops.**
   - "MCP remains out of pg_sage scope for this module" (module:759).
   - "MCP is retired" (wave-0:66-70, July 19).
   - "MCP is re-introduced" (build-spec:26, July 23).
   - MCP v2 for coding agents (October 4).

   AgentDB never followed the reversal.
4. **The thin-clone enabler was rebuilt instead of reused.** The July research says no clone
   enabler exists (`research/2026-07-22-agent-native-feature-spec.md:33`), and the build spec
   prescribes `clone.Provider` with a managed snapshot adapter. AgentDB had created and deleted
   Lakebase branches live since May, and Neon/Supabase branches since September. The snapshot adapter
   is still a stub that returns an error.
5. **Cloud authentication was rebuilt instead of reused.** The G8-B09 fix declined Google ADC because
   it "would change go.mod" (fixes:22). One week later, `internal/cloudtel` implemented an ADC chain
   without new dependencies, plus its own RDS and Cloud SQL Admin clients
   (`reviews/2026-10-05-p3-managed-clouds-report.md:15-30`). Nothing is shared with `agentdb`.
6. **Non-human identity is split three ways.** Deployment ping tokens, tenant-bound `agt_` agent
   tokens (09-26) and scoped `pgs_mcp_` MCP tokens (10-04; `internal/mcptoken/mcptoken.go:23`). The
   May spec wanted one "agent API token" model (module:102,675-681).
7. **Release claims against the review.** v1.1 announced the "AgentDB Cloud Provisioning Release",
   and the 05-10 report called it "functionally strong" (`:140-148`). G8 then found 6 P0s, including
   destroying a resource pg_sage does not own. The 05-09 report says live reconcile "isolates bad or
   stale deployment rows" (`:203-204`). The TTL path (`ReconcileAbandonedDeployments`) still aborted
   the whole batch on one provider error until G8-B02. The receipts proved the happy path through
   test harnesses, not the failure paths or the UI.
8. **Who the user is changes with every document.** DBA → agent → enterprise platform team →
   "10k agent DBs" platform → undecided (September) → a single operator team (D4) → coding agents
   on MCP (October). See §2.2.
9. **The roadmap points at a missing item.** D4 defers human tenant scoping to "roadmap AgentDB
   principals" (`LEDGER.md:22`). MASTER-SPEC requires principals and a teardown state machine
   "before advertising multi-tenant AgentDB" (`:459`). The 2026-10-02 ROADMAP has no AgentDB item.
10. **Different answers to "should agent DBs be operated?"**
    - H2 and v2: register them in the fleet so analyzer and executor apply (B1).
    - G8: attach the analyzer in advisory-only mode, store snapshots in the meta DB, use a
      scale-to-zero cadence.
    - The refactor: full runtime and sage schema inside the agent DB.
    - The October roadmap: keep history outside the monitored DB (Phase 3).

    These four positions were never reconciled.

### 5.2 Docs that no longer match the code

| Doc and lines | Claim | Code reality |
|---|---|---|
| `docs/agent-db-deployments.md:314-319` | The AgentDB fleet path "collects snapshots but does not attach the analyzer or executor"; deployments backed only by `secret_ref` are skipped | Full `DatabaseRuntime` since v1.7.0 (`cmd/pg_sage_sidecar/agentdb_fleet.go:176-206`); env `secret_ref` DSNs resolved (`agentdb_secret_ref.go:22-61`) |
| `docs/reverse_spec/05-agentdb.md:169-185` | `HeuristicBlueprintGenerator` is "LIVE" | Test-only (G8-D01) |
| `docs/reverse_spec/05-agentdb.md:307-314`, `docs/REVERSE_SPEC.md:72` | Analyzer and executor not attached; `secret_ref` skipped | As above |
| `docs/neon-supabase.md:91-93` | Set `secret_ref` to `env:AGENT_DATABASE_URL` | Rejected: must be `env:PG_SAGE_AGENTDB_<NAME>` (`agentdb/credentials.go:25-29`) |
| `docs/runbooks/agentdb-cloud-provider-setup.md:56-62`, `agentdb-live-provisioning.md:44-47` | AWS RDS needs only `PG_SAGE_LIVE_PROVISIONING=1` | Also needs `PG_SAGE_ENABLE_AWS_RDS_RUNNER=1` (`runtime_registry.go:24-27`) |
| `docs/runbooks/agentdb-cloud-provider-setup.md:126-143`, `agentdb-live-provisioning.md:60-69` | GCP needs a static access token and a restart | `PG_SAGE_GCP_TOKEN_SOURCE=metadata` refreshes (`credentials.go:71-92`); undocumented |
| `docs/runbooks/agentdb-cloud-provider-setup.md:197-203` | "Operator marks restore verified" | Admin-only `POST /backups/restore-drill` with evidence; operators get 403 (fixes:24,62-64) |
| `docs/runbooks/agentdb-live-provisioning.md:192-197` | Emergency stop = turn off config and providers | The fleet e-stop now gates AgentDB mutations (fixes:31); undocumented there |
| Both runbooks | Cover three providers | Neon and Supabase covered only in `docs/neon-supabase.md` |
| `docs/superpowers/specs/2026-05-07-agent-database-deployment-module.md:106-118` | The Guard is current behaviour | Deleted 2026-06-11 |
| `README.md:73` | "Agent-facing query recommendations" | Agents can't reach them; no producer (P-27) |
| 12 AgentDB plan files under `docs/superpowers/plans/` | 10 have no ticked box; the GA plan ticks only its coverage list (11 ticked, 60 not) | The code shipped; plans are not status records |

### 5.3 Against the product principle

The principle: pg_sage is an AI DBA; LLM features on by default; it earns trust, then autonomy;
every action goes through `policy.Gate` / `Executor.Apply`, is evidence-backed, verified, and
reversible where possible.

1. **A second authority.** AgentDB control-plane mutations (live create and destroy, TTL teardown,
   local `CREATE SCHEMA`/`CREATE DATABASE`) use their own four-layer policy and authorization records.
   `agentdb` imports neither `internal/policy` nor `internal/executor`. MASTER-SPEC listed AgentDB
   among "side paths that bypass the gate entirely" (`:21-23`). The G8 fixes added an emergency-stop
   gate but left the trust ramp out (fixes:31).
2. **Autonomy without earned trust.**
   - The TTL reconciler runs every 300 s by default (`internal/config/defaults.go:196`) and destroys
     live resources through a self-issued claim (`teardown_reconcile.go:99-130`).
   - It has no shadow mode, no earned level, no approval card and no notification (`agentdb` imports
     no notify, alerting, approval-card, value, earned or shadow package).
   - The core makes similar actions earn L0–L4. The AI-SRE spec excludes cloud provisioning from
     autonomy through R2 (`AI-SRE-SPEC.md:131-135`).
3. **Not reversible, and weakly verified.**
   - Destroy is irreversible; the only safety net is an RDS final snapshot for non-disposable DBs.
   - "Restore verified" is an admin's attestation, not an executed restore.
   - Provisioning counts as done at `available`, with no commissioning check (P-81).
4. **Evidence-backed only in name.**
   - Cost truth is whatever the agent reports (P-23).
   - Masking is an ID check (P-32c).
   - Tuning packs echo metadata the agent declared (P-28).
5. **Approvals bypass the shared trust surfaces.** AgentDB requests and `authorize-live` are separate
   from the Actions queue, approval cards (UI, Slack, Telegram) and the trust ledger. The 2026-10-02
   ROADMAP wants approval cards "for every action type" (Phase 1.5).
6. **LLM on by default: consistent.** Blueprints require an LLM and fail closed without one (P-40).
   The LLM cannot apply infrastructure (DEC-14).

### 5.4 Hygiene

- **Plaintext local QA admin passwords are committed.** In
  `docs/reports/2026-05-08-agentdb-work-report.md:17-18`,
  `docs/reports/2026-05-09-agentdb-release-readiness-report.md:8` and
  `docs/reports/2026-05-10-agentdb-release-hardening-report.md:6,154`. The fixture password appears in
  `docs/agent-db-deployments.md:350,358,378`. These are local test credentials, but the repo is
  public, and the 05-09 report itself says "do not commit credentials" (`:390`). Values are not
  repeated here.
- Live receipts (`sidecar/test-output/agentdb-live-receipts.jsonl`) are git-ignored, so the release
  evidence the reports cite is not in the repo.
- The plan checkboxes (§5.2) and the stale Guard description make the planning record unreliable as
  a status source.

---

## 6. The 2026-09-26 review and whether its fixes landed

### 6.1 What it found

From `reviews/2026-09-26/group-08-agentdb.md`:
- **Bugs:** 6 P0, 14 P1, 9 P2 and 1 P3, plus 13 dead or unwired items (D01–D13) and 12 ranked
  improvements.
- **Headline (`:21-27`):** live-mutation authorization was "carefully built", but almost every P0
  sat around it: cloud resource identity, lifecycle states that dropped out of sweeps, and a ping
  token that could rewrite status.
- **Escalation:** MASTER-SPEC took G8-B01..B06 as global P0-15..19 (`:129-133`) and the P1s as the
  AgentDB block (`:193-197`).
- **Codex:** reported overlapping items SURF-04..08, 17 and 20 (`codex/surface-audit.md`).

### 6.2 Fix status on master

Every fix claimed in `fixes-agentdb.md` is on master: the commits are in the `sidecar/internal/agentdb`
log (2026-09-26/27), and each named regression test exists (`git grep "func <Test>"` found one
definition for each of the 35 sampled).

| ID | Issue (short) | Fix status (fixes doc) | On master |
|---|---|---|---|
| B01 (P0) | A ping could set any lifecycle status | Fixed: liveness-only, `agent_status` enum | Yes: `TestAgentPingCannotChangeLifecycleStatus` |
| B02 (P0) | Archived live deployments never torn down; batch aborted | Fixed: re-claim each pass, block reasons, per-row isolation | Yes: `TestArchivedBlockedDeploymentIsRetriedAfterRestoreVerified` |
| B03 (P0) | Re-register orphaned live resources and could move the tenant | Fixed: replay idempotently, 409 for a different owner | Yes: `TestRegisterSameIDDoesNotResetLiveDeployment` |
| B04 (P0) | Destroy by derived name without an ownership check | Fixed: recorded ID, receipt, tag/label check | Yes: `TestDestroyRequiresLiveCreationReceipt` |
| B05 (P0) | No tenant isolation | Fixed for agents (tenant-bound tokens); humans global by D4 | Yes: `TestAgentPrincipalCannotReadOtherTenant` |
| B06 (P0) | Ambiguous create became `failed` and was orphaned | Fixed: `create_uncertain`, adopt by tag (AWS); hosted and Lakebase reconciled by hand | Yes: `TestAmbiguousCreateStaysUncertainAndIsReconciled` |
| B07 | AWS region allowlist not bound to the SDK region | Fixed: refuse on mismatch | Yes: `TestAWSCreateRefusesRegionMismatch` |
| B08 | Cost ceiling bypass | Fixed: unknown class refused, doubling, extend capped; two-person review deferred | Yes: `TestUnknownInstanceClassDeniedLive` |
| B09 | GCP token decays; destroy impossible; no credentials | Partial: metadata token source, unprotect; agent credentials deferred | Yes: `TestGCPTokenSourcesReportExpiryAndRefresh` |
| B10 | Policy gate bypasses | Partial, then completed by D4b | Yes: `TestOperatorApproveCannotOverridePolicyDeny`, `TestRegisterCloudInstanceWithoutRequestRejected` |
| B11 | Restore verification self-attested; cross-deployment tampering | Fixed: admin drill attestation, scoped upsert | Yes |
| B12 | `require_backup_before_destroy:false` ignored | Fixed | Yes |
| B13 | UI live create/destroy always returned 400 | Fixed (UI calls `authorize-live`) | Yes (web tests) |
| B14 | `secret_ref` couldn't be set | Fixed on register; PATCH deferred | Yes: `TestRegisterRejectsSecretRefOutsideAllowlist` |
| B15 | Provider readiness always "missing" | Fixed | Yes |
| B16 | `Ensure` DDL on every call | Fixed: memoized, version row | Yes: `TestEnsureIsMemoizedPerPool` |
| B17 | Local DDL on a monitored DB adopted existing objects | Partial: opt-in, no adoption; roles and DROP deferred | Yes: `TestLocalProvisioningDisabledByDefault` |
| B18 | No e-stop gate | Partial: store and reconciler gate; trust ramp and API in-memory gate deferred | Yes: `TestEmergencyStopBlocksAgentDBMutations` |
| B19 | Shared Supabase password | Fixed: per-project HMAC | Yes |
| B20 | Approved artifact differed from what was created | Partial: AWS subnet group and SG deferred | Yes: `TestAWSCreateHonorsApprovedSettings` |
| B21 / SURF-07 | Fleet integration one-way and broken | Partial: desired-state sync, `sslmode=require` | Yes: `TestAgentFleetPlanRemovesInactiveAndReplacesChanged` |
| B22 | Substring error classification; snapshot collisions | Partial: snapshot retention deferred | Yes |
| B23 | Reconcile edge cases | Fixed | Yes |
| B24 | Ping endpoint abuse; no retention | Partial: per-deployment cap; per-IP limiter deferred | Yes (retention later in v1.8.3) |
| B25 | UI showed only 100 deployments | Fixed | Yes |
| B26 / SURF-17 | Spoofable audit identity; audit errors discarded | Actor fixed; error surfacing deferred | Partly: 21 `_ = s.audit` sites remain |
| B27 | Authorization consumed before validation | Fixed | Yes |
| B28 | HCL injection; zip size | Fixed | Yes |
| B29 | `budget_exceeded` takes no action; negative samples | Partial: negative samples rejected; stop/destroy proposal deferred | Yes |
| D01, D02, D04, D05 | Heuristic generator, unauthorized destroy, BudgetGate, test helpers | Fixed | Yes |
| SURF-05 / SURF-06 | Terraform unused; dry-run backup marked "verified" | Relabelled review-only; dry-run records `planned` | Yes |

### 6.3 Deferred or never addressed: still open on master

| ID | Item | Evidence it is still open (2026-10-05) |
|---|---|---|
| D03 / SURF-08 | Adaptive monitoring scheduler: wire it or delete about 600 LOC and 3 tables | No production caller of `ScheduleMonitoring`/`ClaimMonitoringWork` |
| D06 | Delete the `queued` / `cancel_requested` / `cancelling` transitions | Still present at `provider_runner.go:197-199` |
| D08 | `agent_db_pings` has no reader | Retention added; the comment says "Heartbeats have no reader" (`internal/retention/agent_rules.go:12`) |
| D09 | Inline-credential fleet path is dead | Still at `agentdb_fleet.go:27-37` |
| D10 | Local provisioning half-built (roles, credentials, teardown) | Opt-in only (P-34) |
| D11 | Relabel promotion as review-only | Not relabelled (`PromotionPanel.jsx:48`) |
| B08 (part) | Reviewer ≠ requester; RDS `ttl` tag updated on extend | Not built; `ttl` set at create only (`aws_rds_runner.go:146`) |
| B09 (part) | Database credentials for the agent on GCP | Not built |
| B14 (part) | `PATCH /secret-ref` | No route |
| B18 (part) | Trust ramp; in-memory fleet e-stop on API routes | Only the reconciler gets `SetMutationGate` (`agentdb_reconciler.go:21`) |
| B20 (part) | AWS subnet group and security groups | Not built |
| B21 (part) | Meta-DB snapshots, RDS ARN resolution, scale-to-zero cadence | Not built; contradicted by DEC-19 |
| B22 (part) | RDS final-snapshot retention sweep | Not built |
| B24 (part) | Per-IP limiter on the unauthenticated ping route | Not built |
| B26 (part) | Surface audit write errors | 21 sites discard them |
| B29 (part) | Budget hard limit → stop/destroy proposal | Not built |
| G8 C1 | `pg_sage_install_id` | Declined |
| G8 C2 | Blocked-teardown alerts and live burn | Not built (P-78) |
| G8 C7 | `SecretResolver` for credential handoff | Not built (P-82) |
| G8 C8 | Monitoring integration as designed | Contradicted (P-83) |

### 6.4 G8's "questions you aren't asking" (`:442-473`), status today

1. Who pays when pg_sage dies? **Open** (Q-11).
2. Is restore ever verified? **Partly** (Q-12).
3. Blast radius of the cloud credentials? **Open** (Q-13).
4. Two installs in one account? **Open** (Q-14).
5. SaaS or single team? **Settled by D4** (Q-15).
6. What does the agent receive to connect? **Open** (Q-16).
7. Should the TTL reconciler be autonomous by default? **Open** (Q-17).
8. Do we alert on orphans? **Open.**
9. Snapshot cost? **Partly.**
10. Is 12k LOC right? **Open**; it has grown.

---

## 7. Activity

### 7.1 Commits by month

| Month | All repo commits | AgentDB-path commits (pkg + api + web + cmd + docs) | `internal/agentdb` only | Share |
|---|---|---|---|---|
| 2026-03 | 104 | 0 | 0 | 0% |
| 2026-04 | 112 | 0 | 0 | 0% |
| 2026-05 | 15 | 10 | 5 | 67% |
| 2026-06 | 31 | 3 | 3 | 10% |
| 2026-07 | 6 | 2 | 1 | 33% |
| 2026-08 | 0 | 0 | 0 | — |
| 2026-09 | 460 | 36 | 30 | 8% |
| 2026-10 (to 10-05) | 1,260 | 1 | 0 | 0.08% |

From 2026-09-28 to 2026-10-05 there were 1,264 commits (1,087 non-merge). One touched an AgentDB path:
`86230885` (2026-10-02, "split main.go by responsibility", mechanical). `internal/agentdb` has had no
diff since 2026-09-28.

### 7.2 Last meaningful changes

- **Last new capability:** 2026-09-07 (Neon/Supabase runners, v1.5.0).
- **Last behaviour change:** 2026-09-27 (D4: attributed approvals; approved request before a cloud
  register; blueprint and template provisioning consume a request).
- **Last indirect change:** 2026-09-27/30. The runtime refactor gave agent DBs the full runtime, and
  v1.8.3 added retention rules, both made outside `agentdb`.
- **Since then:** nine releases (v1.8.0 → v2.2.0) with no AgentDB change.

### 7.3 Evidence decay

- Live cloud runs: 2026-05-09/10 for RDS, Cloud SQL and Lakebase (receipts in reports, the files
  themselves git-ignored); about 09-07 for Neon and Supabase (`docs/neon-supabase.md:111-121`).
- The runners were then changed on 09-26 (ownership verification, unprotect-before-destroy, uncertain
  create) without a live run (fixes:7). Every live test is skip-allowlisted in CI. The 2026-10-02
  platform review rates AgentDB "no run evidence" (`platform.md:27`).
- The UI live path was broken from 2026-07-19 (v1.3 added `authorize-live` and left the UI untouched;
  commit `492cd7b3` has no web diff) until 2026-09-26. Ten weeks without anyone noticing is strong
  evidence that nobody used it.

### 7.4 Footprint

- **Code:** about 16.7k lines of production Go (13,689 `agentdb` + 2,638 `api/agent_db_*` + 375
  `cmd`), about 7% of the sidecar's 231k. 10.8k lines of `agentdb` tests plus 4.0k of API tests.
- **UI and schema:** about 3.5k lines of UI and 27 `sage.agent_*` tables.
- **API:** 62 of about 139 REST method+path pairs in July (`docs/REVERSE_SPEC.md:63`).
- **Config:** 4 YAML keys plus per-provider allowlists, and about 15 environment variables across five
  providers.

### 7.5 Verdict

AgentDB is **stranded and maintained only when a review forces it.**
- Its development came in two bursts, May 7–10 and September 26–27. The second was a correctness
  review, not product work.
- It sits on the main navigation and in the README, but no current roadmap phase includes it.
- The products it was meant to feed were built separately: the agent-facing interface (MCP v2),
  disposable clones (`internal/clone`) and the cloud clients (`internal/cloudtel`).
- It is safe to keep only because live provisioning is off by default and gated behind several flags.

---

## 8. Assessment

### 8.1 Strongest ideas, judged by the documents' own arguments

1. **The live-mutation authority model.** A server-owned plan hash, cost estimate, policy generation
   and single-use authorization bound to requester and idempotency key. Provider policy can narrow
   global policy but never widen it, and missing layers fail closed (GA spec; wave-0 contract;
   `docs/agent-db-deployments.md:245-275`). G8 calls it "carefully built". It is the deterministic
   gate that an LLM or agent can't talk past, which matches the product principle.
2. **"Delete is harder than create," made concrete.**
   - A creation receipt is written before the provider call.
   - An uncertain create is never retried blindly.
   - Destroy needs a recorded ID, a live receipt and a matching provider tag.
   - Teardown is a durable claim, so renewal beats expiry with a single winner.

   These generalize to any agent-created resource.
3. **Approval as evidence, not a crutch (D4).** An approved *design* is not permission to spend.
   Approvals are attributed, single-use and bound to the exact request. This is the clearest
   statement of how "earn trust" applies to spending.
4. **Least-privilege agent principals.** Tenant and agent come from the token, never the body.
   Agents can request and read their own deployments but never approve, authorize or destroy.
   `application_name` is a correlation hint, not authorization (module:102-104).
5. **The original white space: operate, don't host.** Raised in April, June and October: no vendor
   operates the databases agents create (v1:97; H2:36-37; landscape:26-29,147-151). It is the only
   AgentDB thesis that plays to pg_sage's core strength, the gated, verified AI DBA.
6. **The v2 lifecycle insights.** Separate *suspend* (cheap, reversible) from *reclaim*. Run a GC
   sweep *plus* a provider-native TTL backstop. Use intent tags so the sweep can tell ephemeral from
   durable (v2:49-53).
7. **G8's discipline.** Commissioning gates (provisioned ≠ connected ≠ monitored ≠ backed up), and
   cutting to "one provider done end-to-end" before expanding.

### 8.2 Weakest ideas, judged by the documents' own arguments

1. **pg_sage as a five-cloud provisioner with Terraform and LLM blueprints.**
   - It contradicts DEC-02 and the June question "provisioner or thin layer?" (v2:118).
   - It competes with the hosts that own provisioning (landscape:26-29).
   - The Terraform path never runs: it was relabelled review-only, neither finished nor cut
     (H2:334).
   - Most of the code went here, and most of the P0s were found here.
2. **Trusting the agent's own numbers.** Self-reported cost samples as chargeback, masking-policy IDs
   as masking, and declared metadata as tuning input. Each one contradicts "evidence-backed".
3. **A restore gate that never restores.** First a UI checkbox, now an admin attestation. The May
   spec itself says "'File exists' is not a valid backup verification" (module:446-447). The local
   backup plan is `--schema-only`.
4. **A heartbeat that changes nothing.** Pings don't extend leases, agents can't extend leases, and
   ping rows have no reader. The "stale → quarantine" ladder (module:347-366) has no input.
5. **Recommendations for agents with no producer and no agent access** (P-27). The product's real
   agent loop (MCP v2 source-fix packets) was built separately.
6. **Bootstrapping the full sage schema and 60-second collection into every agent database.** It
   runs against the scale story (thousands of ephemeral, scale-to-zero databases) and the October
   roadmap's own lesson: keep history outside the monitored database.
7. **Silent scope moves.** The narrow v1 reversed within a day, and the Guard deleted without a note.
   Neither was recorded as a decision, so later documents reasoned from stale premises.

### 8.3 Five questions the new spec must answer

1. **Who is the user, and should pg_sage provision at all?**
   - Name the persona and the job. For example: a platform team handing coding or app agents
     disposable Postgres; or the AI DBA operating databases agents created elsewhere (Neon, Supabase,
     Lakebase, Tiger, AlloyDB).
   - Decide build or delegate: keep pg_sage's own runners, or govern and operate on top of the hosts'
     branch and project APIs? This settles Q-1 to Q-4 and the keep/narrow/kill list for the five
     runners, Terraform, blueprints, size profiles and local provisioning.
2. **Will AgentDB mutations go through the one authority?** Do create, destroy, TTL teardown and
   local DDL become typed actions through `policy.Gate` / `Executor.Apply`, earning trust per
   (database, action class) with shadow mode, approval cards, notifications and the value ledger? Or
   does the parallel four-layer authority stay, and if so, why? Also define "verified" and
   "reversible" for create and destroy: commissioning gates, real restore drills, final snapshots,
   suspend before reclaim, and a provider-native backstop for when pg_sage is down.
3. **What is the single non-human identity, and what may it do?** Merge ping tokens, `agt_` tokens
   and MCP tokens into one scoped principal. Decide the scopes: request, read own deployments, extend
   lease within policy, consume recommendations, report outcomes. Decide whether MCP is AgentDB's
   agent interface, and how the tenant is enforced if installs become multi-team (D4a-B).
4. **How are agent databases operated at scale, on true evidence?**
   - Monitoring model: full runtime versus a lighter tier, where history lives, and a cadence that
     respects scale-to-zero.
   - Credential handoff, so created DBs actually join the fleet (I12).
   - Cost measured, not self-reported.
   - Which DBA behaviours fit agent databases: spawn defaults, quarantine, drift fixes, cleanup of
     abandoned objects.
5. **What is the one substrate for disposable databases, and what proves it works?**
   - Unify the AgentDB runners, `clone.Provider` (rehearsal, game days) and `cloudtel` auth behind
     one provider layer, or say why not.
   - Set the gates before anything is advertised again: nightly or release-gated live runs, one
     provider proven end to end, a design partner, and the docs, runbooks and README made to match.

### 8.4 Handoff to the code-audit and synthesis agents (UNSURE items)

- **P-50:** confirm that no code path resets `lease_expires_at` when a resource reaches `available`
  (lease set in `queries.go:51`).
- **Q-14:** can AWS adopt-by-tag after an uncertain create adopt another install's resource that
  carries the same `pg_sage_deployment_id`, given there is no install ID?
- **P-70:** effective trust level and executor behaviour for `agentdb:*` runtimes (exec mode from
  `resolveStaticFleetExecMode`, `agentdb_fleet.go:197`).
- **Store location:** with no meta DB, does the AgentDB store still live on `authPool`, the first
  monitored DB (G8-B17 context)?
- **SSL defaults:** the env `secret_ref` path defaults to `sslmode=prefer` for non-hosted providers
  (`agentdb_secret_ref.go:53-55`), while the inline path defaults to `require` (`agentdb_fleet.go:57-61`).
- **P-53:** Cloud SQL proxy verification in preflight.
- **P-61:** quality of agent-actionable error messages.
- **D4b auto-approval:** a cloud-instance request with any positive budget is auto-approved by
  policy (`policy.go:31-35`), so the only human gate is the admin `authorize-live`. Confirm that is
  the intended evidence chain.
- **External "AgentDB drafts"** (module:13,642-665: Forge workspaces, memory vault): not in this repo;
  ask the owner whether they still matter.

---

## Appendix A: Documents reviewed

| Path | Lines | First commit | Last edit |
|---|---|---|---|
| `docs/agent-db-deployments.md` | 382 | 2026-05-08 | 2026-09-27 |
| `docs/reverse_spec/05-agentdb.md` | 391 | 2026-06-11 | 2026-07-19 |
| `docs/REVERSE_SPEC.md` (AgentDB parts) | — | 2026-06-11 | 2026-07-19 |
| `docs/reports/2026-05-08-agentdb-work-report.md` | 199 | 2026-05-08 | — |
| `docs/reports/2026-05-09-agentdb-release-readiness-report.md` | 421 | 2026-05-10 | — |
| `docs/reports/2026-05-09-claude-opus-agentdb-live-provisioning-review.md` | 55 | 2026-05-10 | — |
| `docs/reports/2026-05-09-cloud-provisioning-live-validation.md` | — | 2026-05-10 | — |
| `docs/reports/2026-05-10-agentdb-release-hardening-report.md` | 167 | 2026-05-10 | — |
| `docs/runbooks/agentdb-cloud-provider-setup.md` | ~240 | 2026-05-10 | 2026-07-19 |
| `docs/runbooks/agentdb-live-provisioning.md` | 214 | 2026-05-10 | 2026-07-19 |
| `docs/superpowers/specs/2026-05-07-agent-database-deployment-module.md` | 795 | 2026-05-08 | — |
| `docs/superpowers/specs/2026-05-07-agentdb-provider-provisioning-expansion.md` | 91 | 2026-05-08 | 2026-05-10 |
| `docs/superpowers/specs/2026-05-09-agentdb-blueprint-builder-design.md` | 109 | 2026-05-10 | — |
| `docs/superpowers/specs/2026-05-09-agentdb-live-provisioning-ga.md` | 822 | 2026-05-10 | — |
| `docs/superpowers/specs/2026-07-18-wave-0-safety-contract.md` (AgentDB parts) | — | 2026-07-19 | — |
| `docs/superpowers/plans/*agent*` (12 files) and `2026-04-30-roadmap-slices.md` | 16–872 | 2026-05-08/10 | — |
| `docs/neon-supabase.md` (AgentDB section) | 172 | 2026-09-07 | — |
| `docs/ROADMAP_2026H2.md` | 337 | 2026-06-11 | — |
| `docs/ROADMAP_v1.x_addendum.md` | 296 | 2026-05-08 | — |
| `roadmap.md`, `research/ROADMAP_SYNTHESIS.md` | 138, 164 | 2026-03/04 | 2026-07-19 |
| `research/v1_agent_created_databases.md` | 198 | 2026-05-08 | — |
| `research/v2_agentdb_at_scale.md` | 128 | 2026-06-11 | — |
| `research/2026-04-29-product-roadmap-review.md` | 385 | 2026-05-08 | — |
| `research/2026-07-22-agent-native-feature-spec.md` | 597 | 2026-07-23 | — |
| `research/2026-09-04-product/report.md` | — | 2026-09-07 | — |
| `specs/agent-native-autonomy-build-spec.md` | 503 | 2026-07-23 | 2026-09-27 |
| `specs/autonomous-dba-product-spec-2026-04-27.md` | 1,374 | 2026-04-27 | — |
| `reviews/2026-09-26/group-08-agentdb.md` | 505 | 2026-09-26 | — |
| `reviews/2026-09-26/fixes-agentdb.md` | 141 | 2026-09-26 | — |
| `reviews/2026-09-26/MASTER-SPEC.md` (AgentDB parts) | 795 | 2026-09-26 | 2026-09-27 |
| `reviews/2026-09-26/AI-SRE-SPEC.md` (skimmed) | 389 | 2026-09-26 | 2026-10-01 |
| `reviews/2026-09-26/group-05`, `group-10`, `codex/surface-audit.md` (AgentDB lines) | — | 2026-09-26 | — |
| `reviews/decisions/D4-agentdb-tenancy-and-registration.md`, `LEDGER.md`, `MORNING-2026-09-28.md` | 196, — , — | 2026-09-27 | 2026-10-01 |
| `reviews/2026-10-02-ai-next/ROADMAP.md`, `platform.md`, `landscape.md`, `interfaces.md` | 175, — | 2026-10-02 | — |
| `reviews/2026-10-03-perf-storage-report.md`, `reviews/2026-10-05-p3-managed-clouds-report.md` (AgentDB lines) | — | 2026-10-03/04 | — |
| `CHANGELOG.md` (v1.1 – v2.2.0 entries), `README.md` | — | — | 2026-10-05 |

## Appendix B: How the checks were done

- **Dating:** `git log --follow --format='%h %ad %s' --date=short -- <file>` for each document.
- **Activity:** `git log --format=%ad --date=format:%Y-%m -- <paths> | sort | uniq -c`, and
  `git log --since=2026-09-28` for the recent counts.
- **Promise checks:** `git grep -l -i -E '<pattern>' -- 'internal/**/*.go' 'cmd/**/*.go'
  ':!**/*_test.go'`. Files were read where a grep alone could not decide.
- **Fix landing:** for each test named in `fixes-agentdb.md`, `git grep "func <Test>"` found exactly
  one definition.
- **Guard deletion:** `git log -S agent_workload` shows it added in `3367ffa2` and removed in
  `1683e2e5`.
- **UI break window:** `git show 492cd7b3` adds `authorize-live` in `api/agent_db_handlers.go` with no
  change under `web/src/pages/agentdb*`.
- No files other than this report were modified, and nothing was committed.
