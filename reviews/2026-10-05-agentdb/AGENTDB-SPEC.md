# Agent Guard: pg_sage for the Postgres that agents create and touch (the AgentDB re-spec)

**Status:** proposed, **draft 2, reviewed**.
- The self-review (`spec-review.md`, 68 findings) is folded in: all 10 P0s, all 45 P1s and all
  13 P2s are addressed. One P2 item (sqlcommenter tags, SR-66) is deferred with a reason.
  Appendix B maps every finding.
- Code citations were re-checked at `72646ab1` after drafting; nine were corrected.
- The external review (`gemini-review.md`, `gemini-3.1-pro-preview`) is folded in: of 12 findings,
  10 are accepted (3 of them modified), 1 partly accepted and 1 partly rejected. Appendix D maps
  them.
- **All reviews are complete.** The spec is ready for your decisions in §14, starting with D-1.
- **Date:** 2026-10-05.

**Baseline:**
- `master` at `72646ab1`: v2.2.0 plus #128 and #129.
- v2.3 tracks are open: fleet learning, index replace, specialist mapping, history outside the
  monitored database.
- PR #130 (first-look timeout) is open.

**Inputs:** `PROMPT.md` plus nine research files in `research/`:
- `prior-specs`, `session-history`;
- `code-audit-agentdb` (finding ids A-xx), `code-audit-deploy-paths` (DP-xx);
- `competitive-analysis`, `community-pain`, `incidents-security` (SAFE-*, INC-*);
- `technical-substrate`, `enterprise-needs` (ID-, AZ-, PC-, AP-, AU-, DP-, SF-, FO-, DR-, CM-).

**Evidence rules:**
- "Verified" code claims were re-checked at `72646ab1`. Other code claims cite the audit id.
- Market claims are worded as the research words them.
- Numbers are proposals unless they cite a measurement.
- Release names are G0–G4. Version numbers are assigned when each release is cut.

---

## 0. The verdict

**Should pg_sage do "databases for agents"?** The phrase covers two jobs.

### Job 1: hosting or provisioning instances for agents. No.

The hosts own the storage, copy-on-write branching, free tiers and distribution: Neon/Databricks,
Supabase, Prisma, Xata and the hyperscalers.
- Agents create 80% of Neon's databases and 97% of its branches, and 70% of Supabase's 4M+ new
  databases a month (competitive §0.1).
- In pg_sage's own live runs, an RDS or Cloud SQL instance per agent run took 8.5 and 14.6 minutes
  to become available, and costs a monthly floor (DP §12).

pg_sage keeps the research's *narrowed* form of provisioning (competitive W3): **governed,
short-TTL branches and clones** used as sandboxes, rehearsal targets and restore-drill targets
(§6.12). This is decision **D-1** for you (§14), because it deletes about 20k lines of a feature you
framed in May.

### Job 2: governing and operating agent-touched Postgres that pg_sage doesn't host. Yes.

**Incidents.**
- Destructive-database issues filed against Claude Code alone went from about 1 a quarter in
  mid-2025 to 12–13 a quarter in early 2026, then 8 in Q3 2026 (community §1, a lower bound).
- The major incidents combine some of four failures (incidents §0, community §1):
  - over-broad credentials;
  - production mistaken for test;
  - guardrails in the client or prompt;
  - backups inside the blast radius.

  PocketOS had all four. The Supabase MCP case was a read-path exfiltration.
- In DBA-Bench, 80% of agents' *unsafe* fixes were over-broad or under-evidenced, not
  `DROP`/`TRUNCATE` (incidents §0).

**Enterprise gates** (enterprise key findings):
- 43% run agents on shared service accounts, and 68% can't tell agent from human activity.
- 84% doubt they would pass an audit of agent behaviour.
- 49% use identity disabling or token revocation to contain agents.

**Gap.** No product combines all four of the following (competitive §3D):
1. works on any Postgres;
2. open source and self-hosted;
3. applies changes autonomously;
4. verifies and rolls them back.

The nearest players each cover a part:
- EDB and Lakebase act only on their own platforms.
- DBtune applies settings (GUC) only.
- Teleport (AGPL) and Bytebase (MIT core) cover parts of access and change review.
- Xata Agent was archived on 2026-06-15, pgai was archived on 2026-05-27, and Postgres MCP Pro
  stalled (competitive §0.7).

**pg_sage already has the hard part:**
- `policy.Gate` → `Executor.Apply`;
- typed, leased, verified, reversible actions;
- an earned-trust ledger;
- binding facts;
- MCP tokens that can never carry `approve` (`internal/mcptoken/mcptoken.go:6,54`, verified).

### Did we build AgentDB correctly? No.

AgentDB built job 1:
- about 16.7k lines of production Go and 3.5k of UI (prior-specs §7.4);
- 27 tables: 26 `sage.agent_db_*` plus `sage.agent_identities`;
- about 60 endpoints.

Its live-authorization ideas are worth keeping (§2.2). Around them:

- **A second authority.** `internal/agentdb` imports no other pg_sage package (verified). Its
  policy, approvals, tokens, audit, emergency-stop reader and leases bypass `policy.Gate`,
  `Executor.Apply`, the trust ledger, shadow mode and approval cards.
- **A default that bills silently.** `require_backup_before_destroy` is true by default
  (`internal/config/config.go:1038`, verified). Teardown needs `restore_verified`
  (`internal/agentdb/lifecycle_claim.go:41-45`, verified), which only an admin attestation sets.
  So every expired live database keeps billing, with no alert (DP-01).
- **Paths that orphan billed resources:** DP-02, DP-07 and DP-09.
- **Unusable by agents.** Agents get four REST routes (`internal/api/agent_db_agent_api.go:56-70`,
  verified) and no credentials, branches, lease control or MCP tools (A §1.4). pg_sage doesn't
  monitor what it creates (DP-18).
- **Dead surface:**
  - Terraform is display text only (DP §2.1);
  - deploy requests never execute (DP §4);
  - the monitoring queue and `auto_within_policy` are unwired (A §7.2).
- **Unproven.**
  - Runs: RDS, Cloud SQL and Lakebase last ran live on 2026-05-09/10.
  - Neon and Supabase are recorded as live-verified in a 2026-09-07 commit
    (`docs/neon-supabase.md:119-121`). The receipts are written under a git-ignored path
    (`.gitignore:54-55`), so none are in the repo.
  - Rewrites: the runners changed on 2026-09-26 and have not run live since (DP-28).
- **Stranded.** One mechanical commit out of about 1,264 since 2026-09-28. No user, design partner
  or production deployment is recorded (prior-specs §0, §7).

### Where it goes

- **Rename** the area to **Agent Guard**. "AgentDB" promises job 1 and collides with agentdb.dev
  (competitive §0.10).
- **Five releases:**
  - **G0:** decommission, posture checks, and the public AgentSafetyBench v0. This is the launch.
  - **G1:** agent identity, the gate composition, classification, the kill switch.
  - **G2:** guarded reads, sandboxes, and brokered writes in `branch` and `dev`.
  - **G3:** production writes, restore drills, and the agent change path.
  - **G4:** operation of the agent estate.
- **Alongside:** an enterprise track (E1–E4), reordered to the research's priorities.
- **Gates.** Go/no-go gates sit between releases, including a measured pull gate (§10.6). pg_sage
  has 13 GitHub stars and no recorded agent-database user, so **distribution, not features, is
  the binding constraint** (competitive §0.10; prior-specs §0).

**Positioning.** *The open-source guardian DBA between AI agents and Postgres:*
- *agents get least-privilege, expiring, attributable access;*
- *risky changes are rehearsed, bound to the exact request and reversible;*
- *trust is earned per agent and per capability;*
- *the databases agents spawn are operated, not just counted.*

---

## 1. First principles

### 1.1 What changed

1. **Databases became cheap, ephemeral and agent-created** (competitive §0.1). Your 2026-06-10
   framing anticipated this: agent databases "may be ephemeral … in the 100s or 1000s per hour",
   poorly tuned and sporadic, and may need governance, security, chargeback and archival
   (session-history §3.1).
2. **Agents act on production data, and client-side safety failed.**
   - At least 12 read-only bypasses or SQL-injection flaws hit database MCP servers between June
     2025 and September 2026 (incidents §2).
   - AWS's own advisory names the database role, not SQL-text checks, as the boundary.
   - Approval prompts were defeated by hidden characters, prefix allowlists and consent flags the
     agent could set itself (incidents §0).
   - Too many prompts make people relax precautions (community §1 item 10, §2.9).
3. **Over-broad actions, not destructive commands, are most of the risk.** In DBA-Bench (July
   2026):
   - frontier agents safely fix 17.9% of live PostgreSQL scenarios, against 93.4% for a human DBA;
   - 36.7% of the runs that fixed the problem did so unsafely;
   - 80% of the unsafe actions were over-broad or under-evidenced (incidents §0).

   Scoped, evidence-gated, verified actions address that. They are pg_sage's executor model.
4. **Agent identity shipped; governance lags.** Entra Agent ID, Okta, AWS AgentCore and Google
   shipped agent identity between October 2025 and August 2026. Yet only 13% of IT application
   leaders strongly agree that their governance is right (enterprise key findings).
5. **The open slot is acting across estates.** Hosts provision, specialists advise, and autonomy
   ships inside single platforms (EDB, Lakebase) or narrowly (DBtune, settings only).
   enterprise-needs' synthesis notes that none of the 41 vendors in Gartner's AI SRE guide is a
   database specialist.

### 1.2 What a database needs when agents use it

The map of needs to primitives (BUILD, DELEGATE or CANNOT) is technical-substrate §13.

1. **One identity per agent**, never a shared, owner, superuser or `BYPASSRLS` login.
   Credentials come from a broker, never from where a model can read them.
2. **Least privilege, enforced by the database.**
   - Agents never own objects.
   - Grants are column lists in `stage` and `prod`.
   - Secrets are never selectable.
   - Isolation is by login role or database, not by session variables (substrate §3.2).
3. **Bounded resources.**
   - Timeouts and connection and temp-file limits; role defaults are overridable, so they are
     re-applied per transaction.
   - CPU, I/O and memory per role: CANNOT. Only per-node `work_mem` exists (substrate §4.2).
4. **A SQL path a parser bypass can't break.**
   - Layers S0–S5 (substrate §12.5 numbers them L0–L5; this spec uses S to keep L for trust levels).
   - The agent's SQL runs *logged in as the agent's own role*, so escaping any check lands on the
     agent's privileges, not pg_sage's (substrate §2.3).
5. **Branch before acting, and an undo.**
   - Changes are rehearsed on sandboxes.
   - Writes are bounded, with pre-images kept.
   - A restore point is recorded per risky action.
   - Backups sit outside every agent-reachable credential.
   - Recovery is proven by drills.
6. **Safe schema change:** lint, rehearse, `lock_timeout` with retries, `CONCURRENTLY`,
   expand/contract, verify, rollback.
7. **A lifecycle for ephemeral databases:** owner, intent, TTL, suspend separate from reclaim,
   provider expiry as a backstop, archive before deleting durable data, an inventory sweep.
8. **Cheap fleet operation:** don't wake scale-to-zero computes, install nothing into every
   database, learn per template, take cost from providers.
9. **Agent-state hygiene** (competitive §0.8):
   - checkpoint growth (one LangGraph instance reached 224 GB);
   - pgvector HNSW vacuum fixes in 0.8.3 and 0.8.4, and an IVFFlat fix in 0.8.7;
   - extension drift.

### 1.3 What enterprises need before saying yes

enterprise-needs §3 grants trust on two axes, and each is set per action class, environment and
database tier:

- **Scope.** R0 is metadata, RA read, RB write, RC schema and configuration, RD provision or
  destroy.
- **Mode.** M0 observe, M1 propose, M2 approve each, M3 standard change, M4 autonomous within a
  budget.

pg_sage's levels L0–L3 (`internal/earned/level.go:17-32`, verified) map to M0–M3. pg_sage
deliberately never grants L4. So a coding agent can be **L3 on its own branch** while observe-only
in production.

**Mandatory in practice:**
- SSO with group mapping;
- SIEM export;
- secrets-manager credentials;
- database-side audit;
- a tested kill switch;
- break-glass;
- auditor evidence packs.

Agent identity federation is moving to mandatory over 2026–27.

**The standard-change mapping.** The research proposes that an L3 envelope *should map to* an ITIL
pre-approved standard change. Two caveats:
- A change manager or change board approves the envelope definition once (enterprise-needs §3.2).
- Auditor acceptance of this mapping is unproven. pg_sage supplies the evidence, not the
  attestation.

### 1.4 Be, don't be, and the named rivals

| Be | Don't be (integrate instead) |
|---|---|
| The guardian between agents and Postgres: identities, grants, guarded SQL, request-bound approvals, undo, kill switch, posture | A storage or branching engine. Delegate to Neon/Lakebase, Xata OSS, DBLab, Aurora clones, Cloud SQL fast clone |
| The AI DBA for agent-created and agent-touched databases, open and self-hosted | A long-lived instance provisioner. Hosts, Terraform and the providers' own MCPs own that |
| The safety gate for changes pg_sage itself applies, with rehearsal evidence for every migration | A schema-review product. Hand evidence to Atlas, Bytebase and Liquibase as PR checks or plans (§6.13) |
| A database-specific "guardian agent" (reviewer, monitor, protector) | An inline SQL proxy with masking (Formal, Delinea/StrongDM), an agent framework, or a memory host |

| Rival | Overlap | Difference |
|---|---|---|
| pgEdge AI DBA Workbench | Closest on both halves (competitive §2) | Read-only diagnosis; no gated apply |
| postgres.ai (DBLab, AI DBA) | Thin clones, diagnosis | "Autopilot" is only on its roadmap; pg_sage uses DBLab as a substrate |
| DBtune | Autonomous apply | Settings only, closed source |
| Bytebase, Atlas | Change review | Human-review workflow; pg_sage hands off to them |
| Teleport, Delinea/StrongDM, Formal | Agent access, JIT, SQL policy | Not Postgres-semantic DBAs; pg_sage exports policies to them |
| Immuta | Data policy and masking | Catalog-level; pg_sage enforces with column privileges |

### 1.5 Opportunity cost and distribution

- **Core first.** The core roadmap (v2.3) stays first. Agent Guard reuses it rather than competing
  with it:
  - G4 needs v2.3's history-outside-the-database work and its fingerprints;
  - G2's ledger generalizes `internal/earned`;
  - G3 hardens `internal/migration` and `internal/clone`, which ship already.
- **G0 is the launch.** It publishes AgentSafetyBench v0, the posture checks and the incident
  mapping, with a write-up (§11). It also *removes* about 20k lines.
- **Pull gate.** G1 starts only when the pull gate is met or you override it (§10.6).

### 1.6 Departures from the research, with reasons

| Research said | This spec does | Why |
|---|---|---|
| Keep a governed provisioning broker (competitive W3); keep the Neon and Lakebase branch runners and harden the reconciler (DP §12) | Keeps the broker in its narrowed form, as governed short-TTL branches and clones under the clone substrate (§6.12), with receipt and reconciler semantics. Deletes instance runners, project creation and Terraform | Same narrowing the research recommends. Instances are the wrong unit (§0). Decision D-1 is yours |
| Schema change management is a LOSE; integrate with Atlas, Bytebase and Liquibase (competitive §4.2–4.3) | G3 hardens pg_sage's *existing* agent migration path. `apply_migration` already ships and bypasses `Executor.Apply` (B7). G3 adds rehearsal evidence and a hand-off to those tools; no review workflow is built | The existing path must be made safe regardless; review stays with the specialists |
| Add runners for Aurora clones and Cloud SQL fast clone (competitive W3) | Deferred to a design partner, as a stated exception to principle 8 (short-TTL clones allowed) | Without them, production write autonomy on RDS, Aurora and Cloud SQL estates stays at L2. Accepted and stated |
| You framed AgentDB as a provisioner (2026-05-07, session-history §3.1) | Deletes provisioning | Evidence in §0 and §2. Put to you as D-1 rather than assumed |
| Pilots start on branches; operating agent databases ranks first (enterprise §3; competitive W1) | Order: sandboxes and branch writes (G2) before production writes (G3) | Follows the research order. The lifeos rule ("read-only for agents") holds through G2 |

---

## 2. Audit of what exists

### 2.1 What AgentDB is, as built

AgentDB is a human-operated control plane for per-agent-run Postgres:

1. An agent with an `agt_` token files a request.
2. An operator approves it, then provisions it from a size profile, optionally after a dry run.
3. An admin authorizes the live create, and the same admin executes it.
4. A reconciler runs every 300 s (`internal/config/defaults.go:196`, verified). It archives
   expired leases and attempts TTL destroy.
5. Fleet sync attaches a database to pg_sage's runtime only when an operator has set an
   `env:PG_SAGE_AGENTDB_*` DSN.

The flows are in A §1.3 and DP §1.3. The capability matrix is in A §1.4.

### 2.2 Ideas worth keeping (inside the core)

| Idea | Where | Fate |
|---|---|---|
| Server-owned plan hash; single-use authorization bound to requester and idempotency key | `live_execution_*.go` | Generalized into **request-envelope binding** on core approvals (§6.9) |
| A durable create id before the provider call; an uncertain create never retried blindly | `live_create.go` | Required of every adapter. Today's code writes two non-atomic statements (DP-24), so adapters must write **one atomic receipt INSERT** first (§6.12) |
| Destroy needs a recorded id, a receipt, a matching tag and a matching scope | `*_ownership.go`, `hosted_runner.go` | Required for `sandbox_destroy` and `estate_reclaim` |
| Compare-and-swap teardown claims | `lifecycle_claim.go` | Folded into executor leases |
| Approvals as evidence: attributed, single-use, bound (D4) | `request_provision.go` | Generalized to agent actions. D4b (approved request before a cloud register, v1.7.0) is **superseded**, because cloud registers are removed |
| Tenant and agent taken from the token, never the body (G8-B05) | `agent_db_agent_api.go` | Carried into `sage.guard_principals` |

### 2.3 What is wrong

| # | Finding | Evidence | Severity |
|---|---|---|---|
| AU-01 | A parallel authority bypasses the gate, executor, ledger, shadow mode and cards; TTL destroy is self-authorized | imports (verified); `teardown_reconcile.go:99-130` | P0, principle |
| AU-02 | Expired live resources are blocked from teardown by default and bill silently | verified above; DP-01 | P0 |
| AU-03 | A "not found" seen through the wrong scope is recorded as destroyed | DP-02 | P1 |
| AU-04 | Ambiguous creates wedge forever; installs sharing an account can adopt each other's resources | DP-07, DP-09 | P1 |
| AU-05 | Live approval is single-person and fragile | DP-03, DP-04, DP-05, DP-11 | P1 |
| AU-06 | Created databases are unusable and unmonitored | DP-15, DP-16, DP-18, A-06 | P1 |
| AU-07 | Agent tokens can't be listed or revoked (A-01); the restore gate is a free-text attestation and the UI invents the backup id (A-02); audit lacks actors (A-03..A-05) | A-01..A-05 | P1 |
| AU-08 | Dead surface | DP §2.1, §4; A §7.2 | P2 |
| AU-09 | Three non-human token systems (ping, `agt_`, `pgs_mcp_`); two disposable-database stacks (DP §11); two sets of cloud clients (prior-specs §0.9) | as cited | P2 |
| AU-10 | Plaintext local QA passwords committed in public files | `docs/agent-db-deployments.md:350,358,378`; `docs/reports/2026-05-08…:18`, `2026-05-09…` (two reports), `2026-05-10…:6`; `test-fixtures/full_surface/run_full_surface.ps1` and `run_walkthrough_fixture.ps1` (values not repeated) | P2 hygiene |
| AU-11 | A 116-line per-agent attribution heuristic was added (`3367ffa2`) and deleted in v1.2 (`1683e2e5`) without a note | `git log`, verified | Lesson: record removals |

### 2.4 Core gaps that block enterprise use

| # | Gap | Evidence | Severity |
|---|---|---|---|
| CG-01 | Secrets set through the API (`llm.api_key`, `clone.dle_token`, webhooks) are stored verbatim in `sage.config` (`internal/store/config_helpers.go:315,325`). `internal/api/config_apply.go:79-94` only refuses masked placeholders on write | verified | P0 |
| CG-02 | The API serves plain HTTP: `SAGE_TLS_CERT` and `SAGE_TLS_KEY` are read (`internal/config/config.go:652-653`) and never used | verified | P1 |
| CG-03 | OIDC has no PKCE and no nonce, and ignores `id_token` (`internal/auth/oauth.go`) | verified | P1; P0 with Google or GitHub |
| CG-04 | Three global human roles (`internal/auth/types.go:21-35`) | verified | P1 |
| CG-05 | Logins and user or role changes aren't audited; no SIEM export; no tamper evidence | DP §10 | P0 for regulated buyers |
| CG-06 | HTTP MCP accepts only pg_sage's static tokens (`internal/api/mcp_principal.go:20-33`) | verified | P1 by 2027 |
| CG-07 | One replica only; no Helm chart; static `/health`; unsigned images | DP §10 | P1 at fleet scale |
| CG-08 | Existing propose-scope MCP tools reach the gate with no caller, under pg_sage's own trust (`internal/mcp/scope.go:18-25`; `internal/mcp/intent_adapters.go:21-29`) | verified | P0 once agents use pg_sage |

### 2.5 Decisions

| Part | Decision |
|---|---|
| RDS and Cloud SQL instance runners; Neon and Supabase project creation; Terraform plans and templates; dry-run executor; LLM blueprints; size profiles; deploy requests (as text); monitoring queue; pings; `agt_` tokens; restore attestation; `local_postgres` on the control DB | **Kill in G0**, after the inventory (§12). Subject to D-1 |
| Neon and Lakebase branch logic | **Pivot in G2:** reimplement as `clone` adapters (§6.12) |
| Live-authorization ideas, receipts, ownership checks, claims | **Keep** as core requirements (§2.2) |
| Agent principals; tenant from the token | **Keep:** `guard_principals` on MCP tokens (§6.4) |
| Deploy requests | **Pivot in G3:** the agent change path (§6.13) |
| Fleet sync of agent databases | **Pivot in G4:** estate discovery with an observe tier (§6.14) |
| D4a (humans global; one operator team per install) | **Keep** until E3 adds per-database human RBAC |
| CG-08 | **Fix in G1** (§6.2.6) |

---

## 3. Principles (non-negotiable)

1. **One authority.** Every change an agent asks for goes through `policy.Gate` →
   `Executor.Apply` as a typed action, and so does every change pg_sage makes on an agent's behalf.
   The only exceptions are the closed list in §6.2.5, each with its own audit row.
2. **The database role is the boundary.**
   - Agent SQL runs on a connection that *logs in as the agent's own broker role*, never on
     pg_sage's login.
   - Parsers, prompts and client flags are defence in depth.
   - A bypass of pg_sage's checks hits the agent's privileges, not pg_sage's.
3. **Agents never approve, never own, and never change policy.**
   - Agent tokens can't hold `approve`.
   - Agent roles own no objects.
   - An agent can't change profiles, policy, prompts or Guard configuration. Its policy proposals
     need human approval, with two people when they widen anything (SAFE-TOOL-08).
4. **Approvals are out of band and bound to the full request.**
   - The binding covers the request envelope hash (§6.9) plus the action id.
   - Approvals are single-use and expire.
   - They are given in the pg_sage UI by a signed-in human. Chat only notifies.
5. **Earned per agent, never above pg_sage's own ceiling.** Agents follow the same ladder as pg_sage
   per (principal, database, capability), capped by the operator's `trust.level`. The ledger
   proposes promotions and humans sign them. Bad outcomes demote automatically.
6. **Fail closed, say why once.**
   - An unknown or unverified environment counts as `prod`.
   - An unclassified column is not granted in `stage` or `prod`.
   - A missing privilege, parser or encryption key means deny, with one message naming the exact
     grant or setting.
7. **Recovery is the system's claim.**
   - No agent write or DDL in `prod` without PITR posture.
   - No *autonomous* (L3) agent write in `prod` without a passed restore drill within
     `require_restore_drill_days`.
8. **Operate, don't host.** pg_sage creates no long-lived instances. Short-TTL provider branches
   and clones under receipts are allowed for sandboxes, rehearsals and drills.
9. **On by default, cheap, and AI-native.**
   - Posture runs in every first look and daily.
   - Guard activates when a principal exists.
   - LLM features are advisory, with deterministic fallbacks (§6.16).
   - No agent-facing row data reaches the LLM.

---

## 4. Personas and jobs

| Persona | Job | Served by |
|---|---|---|
| **Platform engineer** (primary buyer) | "Let our coding and app agents use Postgres without being the next PocketOS, and prove it to security." | G0, G1, G2 |
| **DBA / SRE** | "Agent-written schemas and queries hurt performance; agent databases sprawl." | G0, G3, G4 |
| **Security / compliance** | "Attributable, least privilege, a kill switch, evidence." | G1, E1–E3 |
| **Coding agent** over MCP | "Give me a sandbox, tell me if my migration is safe, apply it with approval." | G2, G3 |
| **App or runtime agent** | "Read what I may, request writes, get told no quickly." | G2 (brokered), G3 (direct lane) |
| **Agent-platform builder** | "Operate thousands of tenant databases my agents created." | G0 (RLS posture), G4 |

---

## 5. Product shape

### 5.1 Two access lanes

- **Brokered lane** (default; G1 reads, G2 writes).
  - The agent calls MCP tools.
  - pg_sage runs the agent's SQL on a small per-agent pool that **logs in as
    `sage_agentb_<id10>`**, with a credential only pg_sage holds (§6.6).
  - Controls: S0–S5, envelope-bound approvals, row bounds, pre-images and undo, taint, canaries,
    in-flight tracking for the kill switch.
- **Direct lane** (G3, requires E3).
  - For app-runtime principals only. Profiles with coding-agent classes never get it.
  - **Lower assurance than the brokered lane**, and documented as such: a compromised runtime
    holds whatever credential it was given and can use it outside pg_sage's view.
  - The runtime logs in as `sage_agent_<id10>` through cloud IAM database auth (the default:
    short-lived tokens, no stored secret), or with a credential the operator's own secret tooling
    sets and rotates. pg_sage never holds, stores or delivers a direct-lane password, and never
    returns one through the UI or MCP (SAFE-ID-03; §6.4).
  - pg_sage governs the grants and limits and observes. It isn't in the query path.
  - Controls: S0, plus a watchdog that re-checks the S4 settings.
  - Allowed: read-only in `stage`/`prod` with `pii`/`secret` columns excluded by privilege;
    writes only in `dev`/`branch`.

### 5.2 Capability classes

| Class | What | Lanes |
|---|---|---|
| `read` | `SELECT` on named columns of named tables or views | broker, direct |
| `write_insert` | `INSERT` (shapes in §6.9) | broker; direct in `dev`/`branch` |
| `write_update` | Single-table `UPDATE … WHERE` | broker |
| `write_delete` | Single-table `DELETE … WHERE` | broker |
| `ddl_additive` | `CREATE TABLE`, nullable `ADD COLUMN`, `CREATE INDEX CONCURRENTLY` | change path |
| `ddl_locking` | Statements taking `ACCESS EXCLUSIVE` or rewriting; dropping a non-unique index | change path |
| `ddl_destructive` | `DROP` of tables, schemas, views or functions; `TRUNCATE`; `DROP COLUMN`; narrowing type changes; dropping a constraint or a unique index (integrity loss) | change path |
| `maint` | `VACUUM`, `ANALYZE`, `REINDEX CONCURRENTLY` on named tables | runs as a pg_sage action on the agent's behalf (§6.3) |
| `sandbox` | A branch or clone for a task | MCP |
| `policy_proposal` | `propose_policy_change`, runbook drafts and compiles, owner declarations | MCP; always human-approved |

`ddl_*` is never available through `agent_propose_write` or a direct grant. It goes only through
the change path (§6.13).

### 5.3 Environments

- Every database has a label: `branch`, `dev`, `stage` or `prod`. The default is `prod`.
- The label is bound to a physical identity tuple (§6.5). The agent's claim is never used.
- Labels aren't facts, because a `dev` label *widens* rights and facts only narrow.
- **Effective ceiling** = min(principal `env_ceiling`, profile `env_ceiling`).

### 5.4 Starting levels (from G2)

Each cell is the level before the operator ceiling: `trust.level` `observation` → L1, `advisory` →
L2, `autonomous` → L3.

| Capability | branch | dev | stage | prod |
|---|---|---|---|---|
| `read` | L3 | L3 | L3 | L2 (earnable L3) |
| `write_insert` | L3 | L3 | L2 | L2 (G3+; earnable L3) |
| `write_update` | L3 | L3 | L2 | L2 (G3+; earnable L3) |
| `write_delete` | L3 | L2 | L2 | L2, max (never L3) |
| `ddl_additive` | L3 | L3 | L2 | L2 (L3 opt-in per database) |
| `ddl_locking` | L3 | L2 | L2 | L2, max |
| `ddl_destructive` | L3 | L2 | L2 (two people) | L2 (two people), max |
| `maint` | L3 | L3 | L2 | L2 (earnable L3) |
| `sandbox` (masked or schema-only) | L3 | L3 | L3 | L3 |
| `sandbox` (full data) | L2 | L2 | L2 | L2 |
| `policy_proposal` | L2 | L2 | L2 (two people if widening) | L2 (two people if widening) |

### 5.5 Non-goals (G0–G4)

- Long-lived instances.
- A wire-protocol proxy or inline masking for direct connections.
- Hosting agent memory.
- Text-to-SQL.
- Being in the hot path of high-QPS traffic.
- Sharing one install across tenants (D4a).
- Agent-to-agent delegation beyond "the less privileged identity wins".
- Securing the agent's own runtime: egress, sandboxing, provider credentials in the workspace.
  pg_sage doesn't host it (§11 scores those incidents as out of scope).

---

## 6. Architecture

### 6.1 Packages

```
internal/policy/          + PrincipalRef, AgentDecider, ActionRequest fields, Narrowing (§6.2)
internal/agentguard/      new: principal, roles, grants, classify, hash, query, write, undo,
                          taint, canary, decide (implements policy.AgentDecider), kill
internal/agentguard/posture/   deterministic detectors (§6.15), used by the analyzer and first look
internal/agentguard/estate/    G4 discovery, observe tier, lifecycle, usage
internal/earned/          subject = family | principal, for evaluation and demotion (§6.11)
internal/clone/           + neon, lakebase, template, schemaonly adapters; List, point-in-time,
                          atomic receipts (§6.12)
internal/migration/       parser classifier; verbatim rehearsal; Apply-routed runtime (§6.13)
internal/mcp/             agent_* tools; Principal on every agent-originated request (§6.2.6)
internal/mcptoken/        agent tokens bind a principal (§6.4)
cmd/pg_sage_sidecar/      wires GateConfig.Agents; Guard reconcilers under the v2.3 lease
removed in G0:            the manifest in §12
```

### 6.2 Gate composition

#### 6.2.1 Wiring (no import cycle)

`internal/policy` imports no internal package, and `earned` and `facts` import `policy`. So
`policy` defines the interface and `cmd` injects the implementation. This follows the existing
pattern of `GateConfig.Autonomy` and `GateConfig.Facts`.

```go
// internal/policy/types.go (additions)
type PrincipalRef struct {
	ID         string // "agp_…"
	SponsorID  int    // sage.users id; 0 = none
	TaskID     string // trusted task id (E2 token claim) or ""
	OnBehalfOf string // human subject from a token-exchange `act` claim (E2), or ""
}

type AgentDecider interface {
	// Decide returns the agent-specific outcome. stop=true means the decision
	// is final (a block or a cap that ends evaluation); otherwise Level caps the
	// remaining evaluation.
	Decide(ctx context.Context, req ActionRequest) (d Decision, maxLevel int, stop bool)
}

// ActionRequest gains:
//   Principal       *PrincipalRef // nil for pg_sage's own actions
//   CapabilityClass string        // §5.2
//   ArtifactHash    string        // request-envelope hash (§6.9, §6.13)
// ActionContract gains:
//   Narrowing bool                // §6.2.4
// GateConfig gains:
//   Agents AgentDecider           // nil = Guard off; agent-originated requests capped at L2
```

#### 6.2.2 Normative evaluation order for agent-originated requests (`Principal != nil`)

| Step | Stage | Rule |
|---|---|---|
| A1 | runtime, `hardStop` | An executor that is disabled, the emergency stop, or a replica mutation means `blocked`. **Except** `Narrowing` contracts, which pass and are recorded with `narrowing_during_stop` |
| A2 | `validateRequest` | A typed contract is present and valid. `ValidateSQL` runs on the **canonical** statement. The envelope hash is recomputed from the stored action and must equal `ArtifactHash` |
| A3 | `providerDecision`, `factDecision` | Unchanged. Facts only narrow, so they always apply |
| A4 | `Agents.Decide` | Steps D1–D10 below; the first failure decides |
| A5 | Level | min(ledger level for (principal, database, capability) after the D caps, operator ceiling, rollback-class cap). Operator ceiling: `observation` → L1, `advisory` → L2, `autonomous` → L3. Rollback-class cap: `reversible`, `no_rollback_needed` or `not_applicable` → L3; every other class (`application_rollback`, `mitigation_only`, `forward_fix_only`, `not_reversible`) → L2 |
| A6 | `documentDecision` | The six agent change classes (§6.3, `Feature`) are new `ChangeClass` values in `allChangeClasses()` (`internal/policy/document.go:363-370`). The default document (`document.go:341-345`) lists them in `allowed_change_classes` **and** `approval_required_classes`. A stored policy that doesn't list a class blocks it with `change_class_not_allowed`, as today (`internal/policy/gate_operator.go:26-28`). Guard onboarding proposes the policy version that adds them as approval-required, and a human approves it. Budgets and windows apply |
| A7 | `awaitVerification`, leases, DDL slot | Unchanged |

**D steps.** Each failure maps to `blocked` with the given reason unless a cap is stated.

- **D1:** kill or freeze on the principal, database or fleet → `agent_frozen`.
- **D2:** the principal is active and its sponsor is an active `sage.users` row →
  `agent_unsponsored`.
- **D3:** the database label ≤ effective ceiling, and the binding is verified for any non-`prod`
  treatment → `agent_env_ceiling`.
- **D4:** the capability is allowed by profile × lane × environment → `agent_capability`.
- **D5:** objects resolve to OIDs; no `secret` column; `pii` only with an unmask entry; columns
  within the principal's grants → `agent_classification`.
- **D6:** the change freeze is off → `agent_change_freeze`.
- **D7:** writes and DDL in `prod` need PITR posture → `agent_no_pitr`. A drill older than
  `require_restore_drill_days` caps the level at L2.
- **D8:** a tainted principal caps the level at L2.
- **D9:** rate, budget and pending-approval limits → `park` (`agent_rate`) or `blocked`
  (`agent_budget`). The MCP layer reports `agent_rate` with the existing `rate_limited` code
  (-32011) and a `retry_after`, so clients see one rate-limit signal.
- **D10:** a brokered use of a grant needs an unexpired lease in the registry, even if the database
  grant still exists → `agent_lease_expired`.

**`OperatorApproved` agent requests:**
- The approval satisfies the L2 requirement.
- These still bind: A1–A3, D1–D7, D10, A5's operator ceiling (so `observation` still means
  `observe_only`, as `operatorDecision` does today, `internal/policy/gate_operator.go:12-14`),
  A6's change-class allowlist and windows, and A7.

**The default trust level.** `trust.level` defaults to `observation`
(`internal/config/defaults.go:47`). On a fresh install, agent requests are therefore recorded as
L1 proposals until an operator raises the level to `advisory`. Onboarding and `agent_whoami` say
so in one line. Narrowing actions (§6.2.4) work at every level.

#### 6.2.3 Verdict mapping

| Outcome | `policy.Verdict` | Reason |
|---|---|---|
| A D-step failure | `blocked` | `agent_*` (as above) |
| L0 | `blocked` | `agent_level0` |
| L1 | `observe_only` | `agent_proposal_recorded`. A proposal row is written with `proposed_via='agent'` |
| L2 | `queue_approval` | `approval_required` |
| L3 within the envelope | `execute` | `agent_l3_envelope`. Outside the envelope, the request falls back to L2 |

A D-step's reason always wins. `agent_level0` is used only when the ledger itself yields L0, so
an expired lease reports `agent_lease_expired`, never `agent_level0`.

**Decision records.**
- `sage.decision` gains `principal_id`, `task_id` and `artifact_hash` (§7).
- `agent_query` is decided by `Decide` alone (D1–D6, D10), not by `Gate.Authorize`. It writes one
  row to `sage.guard_query_audit` per call, not a `sage.decision` row.

#### 6.2.4 Narrowing contracts

`Narrowing = true` is set only on:
- `guard_revoke`, `guard_freeze`;
- the kill steps (token revoke, `NOLOGIN`, terminate);
- `guard_watchdog_cancel`, `estate_quarantine`.

They pass A1's stops, A5's ceiling and A6's budgets, windows and DDL slot. They are still validated
(A2, A3) and audited.

**G1-08b:** a lease expires within one reconciler tick even with `emergency_stop` on and
`trust.level: observation`.

#### 6.2.5 The closed list of out-of-gate paths

| Path | Why outside the gate | Audit |
|---|---|---|
| The kill switch's direct fallback, when the control DB or gate is unreachable (§6.10) | Containment must not depend on the component that may be failing | A local append-only log, reconciled into `sage.action_log` when the control DB returns |
| The manual SQL runbook (`docs/agent-guard.md`) | pg_sage may be down | The operator's own records; a startup self-check notes changed agent roles |
| Break-glass login (E1) | The IdP may be down | `auth_audit` and an alert on every use |
| Decommission acknowledgement (§12) | Configuration about removed code | `sage.agentdb_decommission`, with actor and time |

Everything else is in the gate, including the watchdog (`Narrowing`) and the trash purge, which is
a human-only L2 `OperatorApproved` action.

#### 6.2.6 Existing MCP tools under principals (fixes CG-08)

From G1, every `policy.ActionRequest` built from an MCP call by an agent principal carries
`Principal`. That includes stdio with `mcp.stdio_principal` (§6.4). Each request is mapped to a
class:

| Tools | Class | Cap until G3 |
|---|---|---|
| `apply_migration`, `request_change` (schema intents) | `ddl_*` from the classifier | L2 |
| `optimize_query`, `ensure_fk_indexes` | `ddl_additive` | L2 |
| `sre_propose_action`, `sre_request_execution`, `specialist_request_remediation` | `maint` (pg_sage's own candidates) | L2 |
| `set_maintenance_policy`, `propose_policy_change`, `sre_draft_runbook`, `sre_compile_runbook` | `policy_proposal` | L2, two people if widening |
| `propose_fact`, `mark_object`, `report_source_fix`, `sre_evaluate_autonomy`, read tools | unchanged (facts stay proposed until a human confirms) | — |

Freeze, taint, the ceiling and the kill switch apply to all of them.

The approve-scope tools (`decide_fact`, `declare_table_contract`, `register_consumer`,
`sre_downgrade_autonomy`, `sre_review_investigation`) stay out of reach: agent tokens can't hold
`approve`, and a call gets `approval_reserved_for_humans` (-32005)
(`internal/mcp/scope.go:11-16`, `:80-90`).

**Behaviour change:** agent-originated requests that could auto-execute under pg_sage's earned
trust now queue for a human. The CHANGELOG lists this as a breaking change that makes them safer.

**G1-12:** a frozen principal's `apply_migration` and `request_change` are denied. A tainted
principal's are queued.

#### 6.2.7 Cross-database re-check

Principals live in the control DB, while writes commit in the target.

- From re-authorization until the target `COMMIT` returns, the executor holds
  `SELECT 1 FROM sage.guard_principals WHERE id = $1 AND status = 'active' FOR SHARE` on the
  control DB.
- The kill takes `FOR UPDATE` on the same rows, with `lock_timeout` set to 2 s, then cancels the
  in-flight backends it tracks (§6.10).
- **Residual window:** a statement already executing when the kill starts finishes or is cancelled.
  Its effects are recorded and remain undoable.

### 6.3 Executor contracts

Every action has `ProviderSupport` set to all of: self-managed, RDS/Aurora, Cloud SQL/AlloyDB,
Azure, Supabase, Neon. Exceptions are noted in §6.12 and §6.14.

| Action | `RiskTier` | `RollbackClass` | `Feature` (change class) | Lease target | DDL slot | Post-checks | Narrowing |
|---|---|---|---|---|---|---|---|
| `guard_role_ensure` | moderate | reversible | `agent_access` | cluster role | no | attributes and settings equal the spec; owns nothing | no |
| `guard_role_retire` | high | not_reversible | `agent_access` | cluster role | no | role absent in every database of the cluster | no |
| `guard_grant` | moderate | reversible | `agent_access` | object | no | `aclexplode` shows grantee, grantor and privileges | no |
| `guard_revoke` | safe | no_rollback_needed | `agent_access` | object | no | no residue for (grantee, grantor) | yes |
| `guard_freeze` | safe | reversible | `agent_access` | principal | no | no agent write grants; flag set | yes |
| `guard_unfreeze` | moderate | reversible | `agent_access` | principal | no | grants restored; credentials rotated if killed | no |
| `guard_watchdog_cancel` | safe | no_rollback_needed | `agent_access` | backend | no | backend gone or idle | yes |
| `guard_write` | moderate (`write_insert`/`update`), high (`write_delete`) | reversible | `agent_data_write` | table | no | keys = captured; transaction tuples ≤ bound; `verify_sql` true (in a new transaction) | no |
| `guard_undo` | high | forward_fix_only | `agent_data_write` | table | no | rows equal pre-images, or conflicts reported | no |
| `schema_migration` | moderate (`additive`, `online`), high (`locking`, `destructive`) | reversible if `down` was rehearsed for the group, else forward_fix_only; destructive: reversible only inside the trash window | `agent_schema_change` | each object | yes | catalog checks; `verify_sql` | no |
| `maint_on_behalf` | as pg_sage's own maintenance contracts | as theirs | `agent_maintenance` | table | as theirs | as theirs | no |
| `sandbox_create` | safe | reversible | `agent_sandbox` | clone | no | ready; masked; labelled `branch`; registered | no |
| `sandbox_destroy` | safe | no_rollback_needed (disposable by declaration) | `agent_sandbox` | clone | no | provider resource absent in the recorded scope | no |
| `restore_drill` | safe | no_rollback_needed | `agent_sandbox` | clone | no | checksums equal | no |
| `trash_purge` | high | not_reversible | `agent_schema_change` | object | yes | object gone (human only) | no |
| `estate_suspend` | safe | reversible | `agent_estate` | estate item | no | provider reports suspended | no |
| `estate_archive` | safe | no_rollback_needed | `agent_estate` | estate item | no | sample restore passes | no |
| `estate_reclaim` | high | reversible while a provider soft-delete or `reclaim_pending` window is verified, else not_reversible | `agent_estate` | estate item | no | provider resource absent; archive present | no |
| `estate_quarantine` | safe | reversible | `agent_estate` | estate item | no | connection limit 0 or suspended | yes |

- Agent levels use the agent envelope (§6.11), not the core's `l3Blocker`, which requires a single
  object and a maintenance window.
- `read` isn't an executor action: `agent_query` is decided by `Decide`.
- `maint` runs as pg_sage's own maintenance action, attributed to the principal. Broker roles own
  nothing, and `VACUUM` can't run inside a transaction.

### 6.4 Principals, tokens and credentials

```go
// internal/agentguard/principal.go
type Env string // "branch" | "dev" | "stage" | "prod"

type Principal struct {
	ID            string // "agp_" + 20 lower base32 characters
	Name          string // unique slug, ^[a-z][a-z0-9-]{1,62}$
	SponsorUserID *int   // nil means D2 fails (L0 everywhere)
	Tenant        string // "" = the install's single team (D4a)
	Profile       string // config profile name (§9)
	EnvCeiling    Env
	Status        string // active | frozen | retired
	FrozenReason  string
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
```

**MCP tokens.**
- `Kind == agent` tokens gain a required `principal_id` from G1.
- The token's `databases` list remains an upper bound, intersected with the principal's grants.
- `POST /api/v1/mcp/tokens` rejects `kind: agent` from G1. Agent tokens are minted at
  `/api/v1/agents/{id}/tokens`.

**Migrating existing agent tokens (G1).**
- Each token becomes a principal named by a slug rule: lowercase; any other character becomes
  `-`; truncate to 54; add `-<4 base32>` on collision.
- Migrated principals have no sponsor and the `legacy` profile.
- **Unsponsored behaviour:**
  - read tools keep working as today;
  - propose tools queue for a human (L2), as §6.2.6 requires;
  - `agent_*` tools return `agent_unsponsored`.

**stdio.** Today the stdio transport binds a fixed tokenless agent
(`internal/mcp/principal.go:115-116`). From G1:
- `mcp.stdio_principal: <name>` names a principal for it.
- Without it, stdio keeps today's read and propose tools, under the L2 cap, and every `agent_*`
  tool returns `agent_unsponsored`.

**External identities (E2).**
- MCP over HTTP becomes an OAuth 2.1 resource server: RFC 9728 metadata, audience validation, JWKS
  from configured issuers, and an issuer allowlist.
- `(iss, sub)` maps to a principal through `sage.guard_identity_bindings`.
- A token-exchange `act` claim fills `OnBehalfOf` (enterprise ID-3).
- A runtime-asserted task claim fills `TaskID`. This is the only source of per-task taint (§6.10).

**Credentials.**
- **Broker logins (G1).** pg_sage generates them. They are stored only in the control DB,
  encrypted with `encryption_key` and with associated data `principal_id || cluster_key || role`.
  They rotate every `agents.broker.rotation_days` (7), and at every unfreeze after a kill.
- **Direct-lane login (G3 with E3).** pg_sage manages the role's grants and limits, not its
  secret. Two methods, recorded per principal:
  - cloud IAM database auth (`rds_iam`, Cloud SQL/AlloyDB IAM users, Entra): pg_sage grants the
    IAM membership; there is no password;
  - operator-managed: the operator's tooling (for example Vault's database secrets engine) sets
    and rotates the password on its own admin connection. pg_sage requires `VALID UNTIL` on the
    role no more than `agents.direct.max_password_age_hours` (24) ahead, read from `pg_roles`,
    and raises a finding when it is missing or further out.

  There is no UI reveal, no MCP return, and no Vault, AWS SM or GCP SM integration for agent
  credentials.
- **Encryption key required.** Guard features that store secrets require `encryption_key`
  (`internal/config/config.go:118`). Without it, Guard runs posture-only and logs one line naming
  the key.

### 6.5 Environment binding and the control database

**Identity tuple** per database:
- `database_id` from `sage.sre_database_bindings` (a UUID);
- the provider resource id, from `cloudtel` or the adapter receipt: RDS or Aurora instance or
  endpoint, Cloud SQL instance, Neon endpoint or branch, Lakebase branch, DBLab clone id;
- the system identifier from `pg_control_system()`, when readable. When it isn't, today's code
  falls back to `host:port/dbname` (`internal/value/fleet.go:165-181`), which is a connection
  target, not a physical identity, so it never verifies a binding;
- the database OID.

**Reuse of the SRE binding.** `sage.sre_database_bindings` already has `identity_strength`
(`configured`, `provider` or `cluster`) and `cluster_epoch`, but every binding is written as
`configured` with epoch `unknown` (`internal/sre/coordinator.go:254-256`). Guard fills them: a
provider resource id makes the strength `provider`, a readable system identifier makes it
`cluster`, and the epoch records the value seen. A non-`prod` label needs strength `provider` or
`cluster`. The binding is keyed by the configuration entry (`runtime_key`), so a re-pointed DSN
keeps its `database_id`; that is why every Guard connection re-checks the live tuple.

**Labels.** `sage.guard_environment_labels` stores the label and a snapshot of the tuple.
- Every Guard connection re-reads the live tuple. On any change, the database is treated as `prod`
  and a `binding_changed` finding is raised.
- **Same physical identity under two labels.** The physical identity is the system identifier plus
  the database OID. If it appears under two labels with no distinguishing provider resource id,
  both are treated as `prod`, with a critical finding. This catches `pg_basebackup` copies,
  standbys and DSNs re-pointed at production.
- **pg_sage-created sandboxes** are labelled `branch` from their receipts, which carry the provider
  resource id.
- **Hashes** use `database_id`, never the name.

**G1-04b:**
- A `pg_basebackup` or DBLab clone of a `prod` database registered as `dev` without a receipt is
  evaluated as `prod`.
- A `dev` DSN re-pointed at the production primary is evaluated as `prod`.

**Control DB.** Guard requires `mode: meta` or a pinned `agents.control_database`. Otherwise it runs
posture-only and logs one line. Without a pin, the control database can change between restarts
(`cmd/pg_sage_sidecar/fleet_bootstrap.go:15-24`).

**Where data lives:**
- Per-database registries live in the target database's `sage` schema, which survives control-DB
  moves and commits atomically with target changes:
  - grants and leases;
  - pre-images;
  - capture tokens;
  - restore points;
  - the trash map;
  - agent provenance on `action_log` and `action_queue`.
- Install-wide state lives in the control DB:
  - principals and identity bindings;
  - cluster roles and their credentials;
  - labels;
  - taint;
  - the ledger;
  - the estate;
  - clone instances;
  - the in-flight table.

### 6.6 Roles

**PostgreSQL 16+ only for role management.** On PG14 and PG15, `CREATEROLE` is near-superuser, and
PG14 reaches end of life on 2026-11-12. There, Guard runs posture checks only.

**Cluster roles.** Roles are cluster-wide, so `sage.guard_cluster_roles` is keyed by
`(principal_id, cluster_key)`. The cluster key is the provider resource id, else the system
identifier. Rules:
- Two monitored databases on one cluster share the roles.
- The lease end is the maximum over the cluster's databases.
- A per-principal kill is cluster-wide.
- Retire runs `REVOKE` and `DROP OWNED BY` in every database of the cluster before `DROP ROLE`.

**Creation** (one transaction on pg_sage's connection, PG16+):

```sql
SET LOCAL createrole_self_grant = 'set';  -- pg_sage gets ADMIN + SET on the new role, no INHERIT
SET LOCAL lock_timeout = '2s';
CREATE ROLE sage_agentb_k2m4q7x9ab LOGIN
  PASSWORD 'SCRAM-SHA-256$4096:…'         -- verifier computed client-side; plaintext never sent
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2;
CREATE ROLE sage_agent_k2m4q7x9ab NOLOGIN  -- direct lane: LOGIN only from G3, with E3 delivery
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5;
-- then, per monitored database d of the cluster with grants:
ALTER ROLE sage_agentb_k2m4q7x9ab IN DATABASE d SET statement_timeout = '30s';
ALTER ROLE sage_agentb_k2m4q7x9ab IN DATABASE d SET lock_timeout = '2s';
ALTER ROLE sage_agentb_k2m4q7x9ab IN DATABASE d SET idle_in_transaction_session_timeout = '60s';
ALTER ROLE sage_agentb_k2m4q7x9ab IN DATABASE d SET transaction_timeout = '10min';  -- PG17+
ALTER ROLE sage_agentb_k2m4q7x9ab IN DATABASE d SET idle_session_timeout = '10min';
-- the direct-lane role sage_agent_… gets the same timeouts (they bound sessions a kill can't reach)
ALTER ROLE sage_agent_k2m4q7x9ab  IN DATABASE d SET default_transaction_read_only = on; -- hint
GRANT CONNECT ON DATABASE d TO sage_agentb_k2m4q7x9ab;
```

- Role names: `sage_agent_<10 lower base32 chars of sha256(principal_id)>` and `sage_agentb_<same>`.
- The SCRAM verifier keeps plaintext out of `log_statement` output.
- Brokered connections set `application_name = 'pg_sage agent:<principal>'` and use
  `QueryExecModeExec`, so they work behind PgBouncer transaction pooling.
- `temp_file_limit` is applied where pg_sage may set it. G1-13 records this per platform.

**PUBLIC preflight.** It runs per database before the first grant, and per cluster before the
direct lane. Each agent role is a member of PUBLIC.

| # | Check | Effect |
|---|---|---|
| P1 | PUBLIC has `CREATE` on a schema in the profile path | Refuse grants; report `REVOKE CREATE ON SCHEMA … FROM PUBLIC` |
| P2 | Another database on the cluster grants `CONNECT` to PUBLIC | Refuse the direct lane; report `REVOKE CONNECT ON DATABASE … FROM PUBLIC`. Brokered connections choose their own database, so they aren't affected |
| P3 | PUBLIC can execute a volatile `SECURITY DEFINER` function in a reachable schema | Refuse the direct lane unless the function is allowlisted. The brokered lane denies `prosecdef` calls anyway (§6.8) |
| P4 | PUBLIC table grants or default privileges | Posture finding AP-15; recorded as the PUBLIC baseline |

**Grants and grantor semantics.**
- pg_sage grants only what it holds `WITH GRANT OPTION`. It checks
  `has_column_privilege(current_user, rel, col, '<priv> WITH GRANT OPTION')`.
- It never uses owner-role membership for grants: that gives `DROP`.
- If the check fails, the result is `denied: grantor_lacks_privilege`, with the exact
  `GRANT … WITH GRANT OPTION` the owner must run.
- After `GRANT`, the effective grantor is read from `aclexplode` and recorded.
- Revoke runs `REVOKE … FROM r GRANTED BY <grantor>`, then re-reads `aclexplode`.
- On residue, the grant is marked `revoke_incomplete: other grantor <x>`, and D10 denies that
  object in the broker.
- A daily drift reconciler compares the registry with `aclexplode`.
- Schema `USAGE` and sequence `USAGE` (for `serial` columns) are granted with the first table
  grant in the schema and revoked with the last. They are reference-counted in the registry.

**Agent ownership.** Agent roles never own objects (AP-02). The change path (§6.13) runs DDL as the
table's or schema's owner, through `SET ROLE <owner>`. That needs a membership the operator grants
explicitly, `WITH SET TRUE, INHERIT FALSE` (PG16+), as the onboarding step "Allow agent change path
on schema S". New objects are owned by that owner role, and the action records the owner.

### 6.7 Classification

- **Facts schema.** A new fact type `column_class` with values `pii`, `secret` and
  `untrusted_input`, plus a new subject kind `column`. The `sage.facts` CHECKs
  (`internal/schema/facts_migration.go:21-24`) are replaced by versioned constraints (§7).
- **Keys.** Column facts are keyed by `(relid, attnum)`, with the name kept for display, so a
  rename keeps its class. `untrusted_input` may also be table-level.
- **Narrowing only.** These values only narrow access, which is consistent with facts. Unmasking is
  admin configuration (`agents.unmask`), never a fact.
- **Column grants.** Grants are always column lists, in every environment; `secret` columns are
  never in one.
  - In `stage` and `prod`, the list holds only columns classified clean (`pii` only with an
    unmask entry). A column added later isn't visible until it is classified and granted.
  - In `branch` and `dev`, the list defaults to every column not classified `secret`. On a catalog
    change, the reconciler extends it with new unclassified columns.
- **Catalog changes.** The posture cycle detects new columns and proposes classes. No grant follows
  until a human confirms them.
- **Views.** Classification propagates to views through `pg_depend`/`pg_rewrite`. A view exposing a
  `secret` base column is denied (D5). A view owned by a privileged role isn't granted to agents
  unless it is classified clean.
- **Proposals.** pg_sage proposes classes from deterministic name heuristics and, with a model
  configured, from schema names, comments and types (§6.16). It never uses row data. An operator
  confirms them.
- **Masking** is a convenience in `branch` and `dev`, never a control. The control is column
  privilege.

### 6.8 The brokered read path (`agent_query`)

**S0, identity.** The query runs on the principal's broker-login pool for that database, so
`RESET ROLE` returns to the agent's own role:

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = <ms>;
SET LOCAL lock_timeout = '1s';
SET LOCAL search_path = pg_catalog, <profile schemas>, pg_temp;
```

Optionally, it routes to a configured read replica (`agents.query.replica: prefer` with
`databases[].replicas` set).

**S1, transport.** Extended protocol only, so one statement per Parse.

**Pre-parse rejection.** These code points are rejected before parsing:
- Unicode categories Cf, Co and Cs;
- the TAG block, U+E0000–U+E007F;
- bidi controls, U+202A–U+202E and U+2066–U+2069;
- zero-width characters, U+200B–U+200D and U+FEFF.

**S2, parse allowlist.** It reuses `sqlast.InspectReadQuery`. Without cgo it returns
`ErrUnavailable`, so the tool denies (verified fail-closed).
- Exactly one `SELECT`: no data-modifying CTE, no locking clause, no `INTO`.
- No `SET`, `RESET`, transaction control, `DO`, `CALL`, `COPY`, `LOCK`, `LISTEN`/`NOTIFY`,
  `PREPARE` or `EXECUTE`.
- The grammar is PG17 (pg_query_go v6.2.2), so PG18-only syntax is denied (O-3).
- Fidelity: `Fingerprint(Parse(Deparse(Parse(sql)))) == Fingerprint(Parse(sql))`, or the query is
  denied.

**S3, catalog proof.** It reuses `internal/explain/analyze_guard.go`, with three changes:
- **Row-level security is allowed.** Agent roles are `NOBYPASSRLS`, so policies apply. A query is
  refused only when a policy expression calls a volatile function. The guard's
  refuse-all-RLS rule applies to EXPLAIN only.
- **`prosecdef` functions are denied** unless allowlisted.
- **The deny-list** is the guard's own: volatility plus `query_to_xml*`, `table_to_xml*`,
  `cursor_to_xml*`, `ts_stat`, `pg_input_is_valid` and the rest. Added to it: `set_config`,
  `dblink*`, `pg_terminate_backend`, `pg_cancel_backend`, `lo_*`, `pg_read_*file`, `pg_ls_*`,
  advisory locks, `pg_sleep`, and every function in the `sage` and `sage_guard` schemas.

Views are followed to depth 4 (`maxViewDepth`). Readable catalogs are an allowlist (R0): the
`pg_catalog` relations and views needed to describe the profile's objects. `pg_proc.prosrc` and
`pg_authid` are excluded. Proofs are cached by (canonical hash, catalog epoch).

**S4, server enforcement.** The read-only transaction and the timeouts. Rows are read through a
cursor:

```sql
DECLARE q NO SCROLL CURSOR FOR <query>;
FETCH <max_rows + 1> FROM q;
```

The extra row only signals truncation. The transaction then closes, so a large result is never
drained.

**S5, output controls.**
- Columns map back through pgx `FieldDescriptions` (table OID and attribute number).
- An output column with OID 0 (an expression, `UNION` or subquery) counts as classified whenever
  the query references a classified relation. It is then denied in `stage`/`prod` and masked in
  `branch`/`dev`.
- Byte cap.
- Results are fenced as untrusted data (`internal/ask/run.go:132-136`).

**Errors.** The agent receives only the SQLSTATE plus a fixed message written by pg_sage. Full
server text is logged for operators. Cast errors leak values, as in `WHERE ssn::int = 0`.

**Audit.** One `sage.guard_query_audit` row per call: principal, task, envelope hash, row count,
classes touched, verdict.

### 6.9 Brokered writes, envelope binding and undo

**Accepted shapes** (positive list). Anything else is `unsupported_shape`:
- `INSERT INTO t (cols) VALUES …`
- `INSERT INTO t (cols) SELECT …`
- `INSERT … DEFAULT VALUES`
- `INSERT … ON CONFLICT DO NOTHING`
- `UPDATE [ONLY] t SET col = expr … WHERE <pred>`
- `DELETE FROM [ONLY] t WHERE <pred>`

**Never accepted** in v1:
- `MERGE`;
- `ON CONFLICT DO UPDATE`;
- `OVERRIDING`;
- `WHERE CURRENT OF`;
- `UPDATE … FROM` and `DELETE … USING`;
- sub-selects in `SET`;
- agent-written `RETURNING`.

**Further requirements.**
- **Proof scope.** The S3 proof covers *the whole statement*: the `SET` list, `VALUES`,
  `INSERT … SELECT` and the predicate. Volatile functions are allowed only from the allowlist
  `gen_random_uuid`, `uuid_generate_v4`, `clock_timestamp` and `random`.
- **Primary key.** `t` has one. Without one: denied in `stage` and `prod`; allowed at L2 in
  `dev`/`branch`, with `undo_available: false`.
- **Inheritance parents** (relkind `r` with children) require `ONLY`. Partitioned tables (relkind
  `p`) are allowed.
- **Cascades.** `write_delete`, and any `write_update` of key columns, are denied on tables
  referenced by foreign keys whose `ON DELETE` or `ON UPDATE` action is anything other than
  `NO ACTION` or `RESTRICT`.
- **Triggers and rules.** Writes are denied on tables with non-internal triggers or rules, unless an
  admin allowlists the table in `agents.writes.trigger_allowlist`.
- **Size.** `max_rows` ≤ `agents.writes.max_rows_ceiling`, and the encoded pre-image size ≤
  `agents.writes.max_preimage_bytes`.

**Request envelope hash** (SAFE-PERM-02; JSON canonicalized per RFC 8785):

```
envelope = { v: "agw2", database_id, principal_id, env_label, binding_id, policy_version,
             canon: Deparse(Parse(sql)), rels: sorted [(oid, "schema.rel")], params,
             max_rows, verify_sql: canon(verify_sql) or "" }
hash     = sha256(JCS(envelope))
```

An approval binds `(queue_id, hash)`. It is single-use and expires after
`agents.approvals.ttl_minutes` (15). The card shows `canon` with hidden characters escaped, the
environment and binding, the estimated rows and locks, the restore point and the undo plan
(SAFE-HUM-02).

**Capture.** A `SECURITY DEFINER` function owned by the NOLOGIN role `sage_guard_cap`:

```sql
CREATE FUNCTION sage_guard.capture(p_rel regclass, p_token uuid, p_keys jsonb, p_phase text)
  RETURNS void LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$ … $$;
```

What it does:
- Takes only a relation, a one-time token and primary-key values. It never accepts agent SQL.
- Checks that `p_token` exists in `sage.guard_capture_tokens` for `(database_id, action_id,
  p_rel)`, is unexpired, and hasn't been used for `p_phase` yet; then marks that phase used.
- Selects the rows by key equality.
- Writes them to `sage.guard_preimages` in the **same database**, in the same transaction as the
  DML.

**Query construction (normative; GR-06).** The function is reachable from broker roles, so:
- Identifiers come only from the catalog. The relation's schema and name come from `pg_class` and
  `pg_namespace` through the `regclass` OID. The key columns come from the primary key's
  `pg_index.indkey`, never from the input. Both are quoted with `format('%I')`.
- `p_keys` carries values only: an array of objects whose key set must equal the primary key's
  columns exactly. Each value is cast to its column's type, and the values are bound with
  `EXECUTE … USING`; nothing from `p_keys` is concatenated into SQL.
- `p_phase` must be `pre` or `post`. Any other input, an unknown relation, a relation without a
  primary key or a token mismatch raises one fixed error.
- G2-18 fuzzes the function with crafted relations, key sets and values.

**Privileges.**
- `sage_guard_cap` holds `SELECT` on every column of the tables where any principal has write
  capability, `INSERT` on `sage.guard_preimages`, `SELECT` and `UPDATE` on
  `sage.guard_capture_tokens`, and `USAGE` on the schemas involved. Nothing else.
- pg_sage grants the table `SELECT` from privileges it holds `WITH GRANT OPTION`. If it can't,
  writes on that table are denied with `capture_unavailable`.
- The function lives alone in schema `sage_guard`. Broker roles get `USAGE` on `sage_guard` and
  `EXECUTE` on the function, and nothing on schema `sage` (calling a function needs `USAGE` on its
  schema, so it can't live in `sage`).
- Undo's write privileges belong to a different role, `sage_guard_rw`, whose function only
  pg_sage may execute (see Undo below).

**Execution** (`guard_write`).

On pg_sage's own connection:

```sql
INSERT INTO sage.guard_capture_tokens … ;
COMMIT;
```

On the broker-login connection:

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ READ WRITE;
SET LOCAL statement_timeout = …; SET LOCAL lock_timeout = …;
SET LOCAL search_path = pg_catalog, <profile schemas>, pg_temp;
```

1. **Keys.** For `UPDATE` and `DELETE`:
   ```sql
   SELECT <pk> FROM [ONLY] t WHERE <pred>;   -- keys K
   ```
   If `|K| > max_rows`, roll back with `bound_exceeded`. `REPEATABLE READ` makes the DML touch
   exactly these rows. A concurrent change raises a serialization failure, reported as
   `retryable: true`.
2. **Pre-images:**
   ```sql
   SELECT sage_guard.capture('t', $token, $K, 'pre');
   ```
3. **The agent's statement** runs as `canon || ' RETURNING <pk>'`, which yields K′. The rules:
   - `UPDATE`/`DELETE`: K′ must equal K.
   - `INSERT`: |K′| ≤ `max_rows`.
   - Otherwise roll back.
4. **Post-images**, for `UPDATE` and `INSERT`:
   ```sql
   SELECT sage_guard.capture('t', $token, $K', 'post');
   ```
5. **Transaction bound.** The sum of `n_tup_ins + n_tup_upd + n_tup_del` over
   `pg_stat_xact_user_tables`, excluding the `sage` schema, must be ≤ `max_rows` (inserts count
   once per row). This also catches cascades, triggers and rules that slip past the rules above.
6. `COMMIT`.
7. `verify_sql`, if given, runs in a **new** read-only transaction through §6.8. False means the
   action is marked `verification_failed`, and `guard_undo` is proposed at L2.

**Rules for the transaction.**
- pg_sage runs no statement of its own in the agent's transaction except `sage_guard.capture`.
- A restore point (`pg_current_wal_lsn()` and `clock_timestamp()`, read on pg_sage's own
  connection) is recorded in the action before step 1 (SAFE-BAK-02).

**Undo** (`guard_undo`, L2). Runs as pg_sage through `sage.guard_undo(action_id)`, owned by the
NOLOGIN role `sage_guard_rw` (`SELECT`, `INSERT`, `UPDATE`, `DELETE` on the write-capable tables;
`EXECUTE` granted to pg_sage only, revoked from PUBLIC):
- Inverse statements match on the **post-image** key.
- They exclude generated columns, and use `OVERRIDING SYSTEM VALUE` for identity columns.
- A row changed since the action is reported as a conflict and left alone.
- Undo is per row and never forced.

**Pre-image storage.** Pre-images stay inside the database they came from, which is the same trust
boundary as the source rows. They are readable only by `sage_guard_rw` and pg_sage (the capture
role can insert them, not read them). They are kept
for `agents.writes.preimage_retention_days` (7), and erasure requests cover them (§6.17).

### 6.10 Taint, canaries, anomalies, freeze, kill switch, break-glass

**Taint (Rule of Two, SAFE-TOOL-04).**
- An `agent_query` that touches an `untrusted_input` table taints the **principal**.
- Taint has **no time expiry**: injected text stays in the agent's context however long it waits
  (GR-05). It clears only on one of `agents.taint.clear_on`:
  - `context_reset`: a trusted context boundary, either a new stdio process for the stdio
    principal or the end of an E2 trusted task;
  - `operator`: a human clears it, with a reason (one click on the approval card);
  - `rotation`: the principal's tokens are reissued.
- Per-task taint applies only when `TaskID` comes from a trusted runtime claim (E2). Agent-chosen
  ids are ignored, because a fresh id would launder the taint.
- While tainted, every write, capability change, full-data sandbox and migration needs a human
  approval (D8). The card names the read that caused the taint.

**Canaries (SAFE-MON-04).**
- Admin-marked or pg_sage-created `sage_canary.<plausible_name>` tables.
- Brokered lane: a canary OID in a statement means deny, `guard_freeze` and a page.
- Direct lane: detection is best effort, through pgaudit object auditing and `internal/logwatch`.

**Anomaly rules (SAFE-MON-03).** Each rule freezes the principal to L0 and pages:
- rows read in 10 minutes above max(10 × the 7-day p95, `agents.anomaly_min_rows`);
- an attempt on an out-of-profile or classified object;
- a cross-tenant object;
- a write attempt within 60 s of an untrusted read.

**Freeze (SAFE-PERM-05).** A flag per database or fleet. Agent writes and DDL are denied.
`guard_freeze` revokes direct-lane write grants, which also stops direct connections.

**Kill switch (SAFE-HUM-04).** `POST /api/v1/agents/kill {scope: all|principal|database, id?,
reason}`, admin only. The steps are idempotent; each is audited.

1. Mark the principals `frozen` in the control DB. If the control DB is unreachable, continue per
   database and queue the audit.
2. Revoke their MCP tokens and broker-pool credentials in memory.
3. Cancel pending approvals and queued actions (`cancelled_kill`).
4. Cancel in-flight brokered statements, by PID from memory and `sage.guard_inflight`.
5. Per cluster: `ALTER ROLE <login|broker> NOLOGIN CONNECTION LIMIT 0`, with `lock_timeout` 2 s and
   3 attempts. Record `prior_attrs`.
6. Terminate backends whose `usename` is an agent role, on the primary and on each **configured**
   replica (`databases[].replicas`, DSN environment references; GR-01).
   - `pg_stat_replication` shows standbys but gives no routable DSN or credential, so it is used
     only to *report* standbys that have no configured DSN.
   - The step-5 role change replicates, so new logins fail on every standby, configured or not.
   - Sessions already open on an unconfigured standby end within the agent roles' bounds:
     `statement_timeout`, `idle_session_timeout` and, on PG17+, `transaction_timeout`. The kill
     report names those standbys and the bound.
   - The brokered lane only connects to configured replicas, so its sessions are always reachable.
7. `guard_freeze`.
8. Verify: no agent backend on the primary or any replica within
   `agents.kill_verify_timeout_seconds` (10). Report per database.

**After a kill.**
- `guard_unfreeze` after a kill forces credential rotation.
- `docs/agent-guard.md` ships the manual runbook for when pg_sage is down:

```sql
DO $$ DECLARE r record; BEGIN
  FOR r IN SELECT rolname FROM pg_roles WHERE rolname ~ '^sage_agentb?_[a-z2-7]{10}$' LOOP
    EXECUTE format('ALTER ROLE %I NOLOGIN CONNECTION LIMIT 0', r.rolname);
  END LOOP; END $$;
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE usename ~ '^sage_agentb?_[a-z2-7]{10}$';
```

**Break-glass (SF-4, E1).** A local break-glass admin that works when the IdP is down. Every use
alerts and enters a post-use review queue that the evidence packs include.

### 6.11 Agent trust, separation of duties and approvals

**Ledger.**
- New tables `sage.guard_autonomy`, `guard_autonomy_proposals`, `guard_autonomy_events`
  (append-only) and `guard_autonomy_outcomes` mirror the SRE ledger's shape: version
  compare-and-swap, `changed_by`, `change_reason`, one pending proposal per pair, events and
  outcomes.
- Their subject is (principal, `database_id`, capability), and capability names use underscores.
- `internal/earned` evaluation and demotion are refactored onto a `Subject` interface with `family`
  and `principal` implementations. The SRE tables are unchanged. Estimate: +3 agent-days in G2.

**Envelope at L3** (the standard-change definition):
- object set (OIDs);
- `max_rows`;
- hours;
- rate per hour;
- environment.

A request outside the envelope falls back to L2. The envelope's signer:
- an admin;
- plus a member of `agents.change_manager_group` when that group is configured (E3).

**Promotion evidence** (defaults). This reuses your fast-trust ramp (session-history §1):
- executions ≥ `agents.trust.min_executions`: 10 for writes and `maint`, 5 for DDL;
- elapsed time ≥ `trust.ramp_safe_hours` for `write_insert`, `write_update` and `maint`, or
  `trust.ramp_moderate_hours` for DDL and `write_delete`;
- a 100% verification pass;
- 0 undos in the window;
- human acceptance ≥ 0.9;
- for `prod` writes, a drill within `require_restore_drill_days`;
- no security signal in 7 days.

Evidence may be pooled across databases with the same template fingerprint (v2.3), within one
tenant only. A human still signs every promotion (the v2.3 rule: priors are evidence, never
authority).

**Shadow scoring.** At L1 and L2, each request records whether it *would* have run under the L3
envelope, and its outcome. This counts toward evidence.

**Signers.**
- DDL classes and `maint`: an operator ("DBA" maps to `operator` until E3).
- Write classes: an admin, or the data-owner group (E3).
- Never the principal's own sponsor, unless in single-operator mode.

**Demotion.**
- A verification failure or an undo returns the class to L2.
- A security signal (canary, anomaly, kill, taint followed by a write attempt) sets every class of
  the principal to L0.
- A departed sponsor (a disabled `sage.users` row or SCIM deprovisioning, E3) sets L0 and raises an
  "ownerless agent" finding.
- A data-loss event freezes the class.

**Separation of duties (SAFE-HUM-03, AP-2).**
- The requester's sponsor can't approve the request in `prod`.
- **Widening actions** need two people: a second admin who is neither the requester nor the
  sponsor. Widening actions are:
  - label widening (toward `dev`);
  - `agents.unmask`;
  - profile or ceiling changes;
  - promotion to L3 in `prod`;
  - unfreeze after a kill;
  - policy proposals that widen.
- `agents.single_operator_mode` (off by default) lets one human approve with a recorded reason.
  Each such approval enters a post-hoc review queue. This is decision D-5 for lifeos.

**Two-person data model.** `sage.action_approvals (queue_id, approver_user_id, decision,
decided_at, card_hash)`, primary key `(queue_id, approver_user_id)`. The executor enforces the
quorum (1 or 2) and the sponsor exclusion. Agent items set `expires_at = now() + approval TTL`, not
the queue's 7-day default.

**Approval channels (SAFE-HUM-01).**
- Agent actions are approved only in the pg_sage UI, by a signed-in user, with typed object names
  for `write_delete` and `ddl_destructive` (SAFE-HUM-05).
- Chat cards notify and link. `agents.approvals.chat_classes` defaults to `[]`.

**Rate limits.** `agents.approvals.max_pending_per_principal` (10) and `max_requests_per_hour` (30)
(SAFE-HUM-05).

### 6.12 Sandboxes, substrates and restore drills

**`clone.Provider` extensions.**
- `List(tag)`.
- `CreateAt(ctx, source, at time.Time)` for point-in-time restores, where the substrate supports
  it.
- Provider TTL.
- **One atomic receipt INSERT** into `sage.clone_instances` before the provider call (fixes the
  DP-24 pattern).
- The install tag is `sage.sre_deployments.deployment_id`.
- Every adapter: adopts after an uncertain create by deterministic name within the recorded scope;
  destroys only on matching receipt, tag and scope; never records a "not found" seen through
  another scope as destroyed (DP-02, DP-07, DP-09).

**Configuration.**
- `clone.provider` keeps `none | dle | snapshot` and adds `neon | lakebase | template |
  schemaonly`. `snapshot` keeps today's behaviour: it errors until an adapter exists.
- Per-database override: `databases[].clone`.

| Adapter | How | Point in time | Status |
|---|---|---|---|
| `neon` | Branch API; `expires_at` backstop (substrate §6); per-branch role | yes (branch at timestamp) | Reimplement from `hosted_*.go` at `72646ab1` |
| `lakebase` | Branch API, `spec.ttl` (from pg_sage's own runner, last live 2026-05-10; re-verify) | UNVERIFIED | Reimplement from `lakebase_runner.go`; send ownership metadata |
| `dle` (DBLab) | Existing adapter | nearest snapshot | Exists |
| `template` | `CREATE DATABASE … TEMPLATE <masked golden> STRATEGY FILE_COPY` on a designated sandbox cluster (PG15+). `file_copy_method = clone` on PG18; timing UNVERIFIED, benchmark before advertising | no | New |
| `schemaonly` | `pg_dump --schema-only` into the sandbox cluster; evidence marked "structural only" | no | New |
| `aurora_clone`, `cloudsql_clone` | Provider APIs | via the provider | Deferred to a design partner |

**Sandboxes are ephemeral fleet databases.**
- `sandbox_create` registers the clone as a monitored database: `database_id`, label `branch` from
  the receipt, observation-tier runtime.
- Agents use it through `agent_query`, `agent_propose_write` and `propose_migration` with
  `database: <sandbox name>`. No credential leaves pg_sage.

**Masking.**
- Masking runs once, on a **masked parent**: a masked golden template, or a masked parent branch
  refreshed every `clone.masked_parent_refresh_hours` (24).
- Sandboxes branch from that parent's head, and only pg_sage can branch. Agents never reach the
  pre-mask history.
- Registration happens only after masking and a PII scan pass. The scan samples `pii` columns
  against format detectors for e-mail addresses, national identity numbers and phone numbers.

**Restore drills (SAFE-BAK-04).** A `restore_drill` every `clone.drill_interval_days` (7) per
`prod` database, **only** on a copy-on-write substrate that restores to a point in time (`neon`,
`lakebase`, `dle`). pg_sage never drills by restoring a full RDS or Cloud SQL instance (GR-02: cost
and 15–40 minute restores).
1. Pick T = 10 minutes ago, and up to `clone.drill_tables` (5) tables with no writes from T − 1 h
   until now, by `pg_stat_user_tables` deltas from pg_sage's own samples. Quiet tables make the
   result immune to the provider's timestamp precision.
2. Compute, with settings pinned (`TimeZone=UTC`, `DateStyle=ISO`, `IntervalStyle=postgres`,
   `extra_float_digits=3`):
   ```sql
   SELECT count(*), sum(hashtextextended(t::text, 0)) FROM t
   ```
   Then re-check the deltas. A write since step 1 drops that table from the drill.
3. `CreateAt(source, T)`, recompute on the restored copy, and destroy the copy. Equal means pass.

A database without such a substrate never reaches L3 for `prod` writes (D7); it stays at L2, which
is the cost of not drilling. A per-action restore point is just the LSN and the time, which is
cheap.

### 6.13 The agent change path (scoped to what pg_sage applies)

**What exists today:**
- MCP `apply_migration` with a migration runtime that bypasses `Executor.Apply` (B7);
- a regex `lint_migration`;
- a rehearsal that never returns `promote_expand` (B1);
- a CHECK constraint that lacks `blocked` (B2).

**G3 makes this path safe.**

1. **Propose.** `propose_migration {database, up_sql, down_sql?, verify_sql?, source?, handoff?,
   task_id}`. `apply_migration` routes here.
2. **Classify.** A parser-based classifier replaces the regex. Per statement it records:
   - lock level;
   - rewrite risk;
   - `CONCURRENTLY` and `NOT VALID`;
   - destructive flags, including integrity loss: `ALTER TABLE … DROP CONSTRAINT` and dropping a
     unique index are `destructive`; dropping a non-unique index is `locking` (GR-04).

   The migration's risk class is `additive`, `online`, `locking` or `destructive`.
3. **Group.** Non-`CONCURRENTLY` statements are grouped into transactions, each with a `down`
   step. `CONCURRENTLY` statements stand alone. For a dropped constraint or index, pg_sage records
   `pg_get_constraintdef` or `pg_get_indexdef` before applying, and that is the `down` step.
4. **Rehearse** on a sandbox of the target, in verbatim mode.
   - Per group: duration; the strongest lock, from `pg_locks` sampled every 100 ms; rewritten
     relations; errors; `verify_sql`; and a `down` round trip (up, down, up).
   - **B1 evidence:** replay the top 20 queries that reference the touched relations (by
     `pg_stat_statements` relation match) with the existing safe EXPLAIN, before and after. Record
     plan-hash changes and cost deltas. The statement text carries `$n` placeholders, so the
     replay uses pg_sage's existing generic-plan paths (GR-03): `EXPLAIN (GENERIC_PLAN)` on PG16+
     (`internal/autoexplain/collector.go:218`); on PG14/15, `PREPARE` and `EXPLAIN EXECUTE` with
     NULLs under `force_generic_plan` (`internal/optimizer/hypopg_generic.go:15-16`).
5. **Decide.** The capability is `ddl_<risk>`, and the artifact hash covers the ordered canonical
   groups plus the rehearsal id. Levels:
   - `destructive`: L2, two people in `prod`;
   - unrehearsed: L1, or L2 with an "unrehearsed" banner for `additive` only;
   - a failed rehearsal: L1.
6. **Apply** through `Executor.Apply` (`schema_migration`), so B7 is fixed. Per group:
   - `lock_timeout` 2 s, 3 attempts with backoff;
   - DDL runs as the owner role through `SET ROLE` (§6.6);
   - a failure halts the migration (SAFE-TOOL-05);
   - `down` runs automatically only for `additive` groups; for any other class it is proposed at L2.
7. **Verify.** Catalog checks and `verify_sql`. The outcome goes to the ledger and to a source-fix
   report.

**Hand-off.** With `handoff: atlas|bytebase|pr`, the rehearsal evidence comes out as JSON plus
markdown attached to the source-fix packet, for the team's change-review tool. pg_sage builds no
review workflow.

**Delayed drops (SAFE-BAK-05).**
- A `DROP TABLE` or `DROP SCHEMA` by an agent becomes:
  ```sql
  ALTER … RENAME TO sage_trash_<action_id>;
  ```
- `sage.guard_trash_map` records the original name, the OID and `purge_after`.
- On rename, pg_sage revokes all privileges on the object and detaches it from publications.
- Objects with dependents are refused unless the statement had `CASCADE`; then the dependents are
  trashed too.
- `TRUNCATE` above `agents.writes.truncate_max_rows` is denied. Smaller ones are L2.
- `DROP COLUMN` isn't delayed; it is `ddl_destructive` with two people.
- The purge is human-only, after `agents.writes.trash_retention_hours` (48).

### 6.14 Agent estate operations (G4)

**Discovery adapters.**
- Read scopes, configured separately from reclaim scopes.
- Neon (projects, branches, endpoints, consumption), Supabase Management API (projects, status,
  usage), Lakebase (branches), plus manual or YAML registration.
- Field names are validated by a gated live run before release (O-4).
- Discovered items go to `sage.guard_estate`.

**Intent** is `ephemeral`, `durable` or `unknown`. It comes from provider tags,
`estate.intent_rules` or an operator, and is never assumed. `unknown` counts as `durable`.

**Observe tier.**
- No `sage` schema in the target; history lives in the meta DB (v2.3 `history.store: meta`).
- One short connection per observation, with `sslmode=verify-full` by default.
- Observe only when the provider reports the compute active, and `observe_interval_minutes` (60)
  have passed, or within `wake_budget_per_day` (0).
- Each observation runs the first look on first sight, the catalog fingerprint and posture.
- Estate databases start at L0 for pg_sage's own actions, whatever the global mode. They are
  promoted to the full runtime only when an operator marks them `durable`.
- Their schema text reaches the LLM fenced as untrusted (§6.16).

**Lifecycle actions:**

| Action | Preconditions | Starting level |
|---|---|---|
| `estate_suspend` | Supported by the provider; idle for `suspend_after_idle_minutes` | L3 |
| `estate_archive` | Snapshot or `pg_dump` to configured storage, then a sampled restore | L3 |
| `estate_reclaim` | `intent = ephemeral`; TTL expired; idle `reclaim_after_idle_days`; archive verified or the profile discards; a receipt or an opt-in owner policy for that provider scope; never `durable` or `unknown` without L2. Delayed by a provider soft delete or `reclaim_pending` for `reclaim_delay_hours` | L2, then earnable |
| `estate_quarantine` | Budget exceeded or an anomaly | L3 |

**Zombie sweep.** Provider resources tagged with this `deployment_id` and unknown to the registry
become findings, with no action.

**Usage attribution (not a FinOps engine; GR-10).**
- Per estate item, pg_sage records usage **as the provider reports it** in
  `sage.guard_estate_usage`: the provider's own units (compute seconds, storage bytes), and cost
  only when the provider's API returns cost. pg_sage never prices anything and keeps no rate
  tables.
- It attributes each item to a principal, team or tenant from tags and receipts, and exports the
  rows (CSV and OCSF) for the customer's FinOps tool.
- In-database attribution per agent role from `pg_stat_statements` is best effort. It resets on
  compute restart and is evicted at `pg_stat_statements.max`, and that loss is reported.
- Budgets are set in the provider's units, or in reported cost where available. They alert at 80%,
  and at 100% they propose `estate_quarantine`.

**Fleet findings.** These use v2.3 fingerprints, so one finding and one source-fix packet cover a
template's whole fleet.

**Memory-store playbooks.**
- LangGraph checkpoint retention, only under an owner-declared retention contract (`OwnerDeclared`).
- HNSW maintenance order.
- pgvector drift.

### 6.15 Posture detectors (G0)

**Cadence.** Detectors run:
- in the first look's "Agent posture" section, inside its statement budget (5 s once PR #130
  merges; if it doesn't, posture uses its own `SET LOCAL statement_timeout = '5s'`);
- when the catalog fingerprint changes;
- daily.

**Proposed fixes.** Detectors emit SQL evidence and proposed fixes. Any fix that changes access
(`REVOKE`, `ENABLE ROW LEVEL SECURITY`) is capped at L1, as a manual script.

**Exposed roles** are PUBLIC plus `agents.exposed_roles`. `anon` and `authenticated` are added
automatically when both exist (Supabase).

| Id | Detects | Severity | Notes |
|---|---|---|---|
| AP-01 | A registered agent role, or a role used by a client matching `agents.client_patterns` (anchored; a hint only), that is superuser, `BYPASSRLS`, `CREATEROLE`, `CREATEDB`, `REPLICATION`, or a member of `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files` or `pg_write_all_data` | critical (registered) / warning (hint) | |
| AP-02 | Objects owned by agent roles | critical | |
| AP-03 | Tables or views granted to exposed roles with RLS disabled (`aclexplode` of `relacl`/`acldefault`) | critical | |
| AP-04 | Permissive policies for exposed roles (`qual` or `with_check` = `true`) | warning | |
| AP-05 | `SECURITY DEFINER` functions executable by exposed roles without a pinned `search_path` | warning | |
| AP-06 | Views in exposed schemas over RLS tables without `security_invoker` (PG15+). On PG14: "no `security_invoker` available", reported as a warning | warning | |
| AP-07 | PUBLIC `CREATE` on schemas | warning | |
| AP-08 | Agent or app login roles without `statement_timeout` or `idle_in_transaction_session_timeout` | info | |
| AP-09 | `dblink` or `postgres_fdw` usable by PUBLIC; untrusted languages granted | critical | |
| AP-10 | pgvector < 0.8.4 with HNSW indexes (vacuum fixes); pgvector < 0.8.7 with IVFFlat (build overflow; the CVE id per competitive §1.6 is UNVERIFIED); server major at or within 90 days of end of life | warning / critical | Both pgvector arms are critical when the version is below the fix |
| AP-11 | No PITR (self-managed `archive_mode=off`; provider backup or PITR off or retention 0); deletion protection off; pgaudit absent where agents connect; backups not delete-delayed or immutable where `cloudtel` can see it (best effort, SAFE-BAK-01) | warning | |
| AP-12 | Agent memory-store growth: LangGraph checkpoint tables growing more than `agents.posture.memory_growth_gb_day` (5) with no deletes | info | Needs two observations |
| AP-13 | One login role used concurrently by ≥ 3 distinct `application_name`/`client_addr` pairs including an agent-like client | info; **warning** once any principal exists | Needs `pg_read_all_stats` for `client_addr`; degrades to `application_name` only |
| AP-14 | pg_sage's own role holds `BYPASSRLS` or inherits agent roles | warning | Plus a startup self-check: pg_sage's role isn't superuser for Guard features |
| AP-15 | PUBLIC table grants or default privileges | warning | The PUBLIC baseline (§6.6) |
| AP-16 | Unmanaged agent-like logins (AP-01 hint, AP-13) once any principal exists | warning | SR-59 |
| AP-17 | RLS isolation probe failure on a sandbox: two synthetic users, cross-user read or write | critical | Runs on sandboxes only (G2). For Supabase-style policies, sets `request.jwt.claims` per user |

### 6.16 LLM features (on by default when a model is configured)

**Data-flow rule.**
- No row data from agent-facing tables reaches the model.
- Inputs are limited to schema (names, types, comments), canonical statements with literals
  replaced by `pg_query.Normalize`, counts and evidence.
- Everything is fenced as untrusted.
- Estate databases' schema text is untrusted input too.

**Never in the decision path (GR-12).** LLM output is generated asynchronously, after the gate has
decided and the structured card exists. A slow or failed model call never delays a decision or a
card, and never changes one. The structured card is the primary content; the narrative is an
addition, marked as generated, with every claim citing evidence.

| Feature | Release | Deterministic fallback |
|---|---|---|
| Classification proposals (`pii`, `secret`, `untrusted_input`) from names, comments and types | G1 | Name heuristics |
| Approval-card narrative: what the request does, risk, rehearsal evidence, undo plan, with every claim citing evidence (Ask Sage grounding) | G2 | The structured card |
| Posture explanations and fix scripts | G0 | Templated text |
| Per-principal daily digest ("what agent X did"); Ask Sage tools over `guard_query_audit` and the action log | G2 | A tabular digest |
| Migration review narrative on rehearsal evidence | G3 | Structured evidence |
| Anomaly triage note (benign or suspicious, with cited evidence); it never unfreezes | G2 | Rules only |

### 6.17 Audit, retention and evidence

**Audit rows.**
- Every agent decision and action record carries: principal, `on_behalf_of`, task, approval or
  envelope id, `artifact_hash`, `policy_version` and the MCP tool-catalog hash.
- The `tools/list` response carries `catalog_sha256` in `_meta`, so clients can pin the catalog
  (SAFE-TOOL-07).

**Retention** (configurable):
- decisions, actions, approvals and grants: ≥ 180 days by default;
- `guard_query_audit`: 30 days;
- pre-images: 7 days;
- estate usage: 400 days.

**Erasure.** A documented procedure covers pre-images.

**Tamper evidence (E2).**
- `action_log` and `guard_query_audit` rows are hash-chained.
- The SIEM, receiving OCSF JSON over HTTP, syslog or OTLP, is the off-box copy.
- pgaudit records are correlated through `application_name = 'pg_sage agent:<principal>:<action>'`.

**Evidence packs (E2).** Per change: request, approver(s), policy version, envelope, rehearsal,
verification, rollback or undo. For L3 classes: the envelope definition, its signers and the
promotion evidence.

---

## 7. Data model

Each change is an idempotent migration file registered in `internal/schema/bootstrap.go` after
`ddlAsk`. The schema version isn't bumped.

```sql
-- G1, control DB --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_principals (
  id              text PRIMARY KEY CHECK (id ~ '^agp_[a-z2-7]{20}$'),
  name            text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
  sponsor_user_id integer REFERENCES sage.users(id) ON DELETE SET NULL,
  tenant          text NOT NULL DEFAULT '',
  profile         text NOT NULL,
  env_ceiling     text NOT NULL DEFAULT 'dev'
                  CHECK (env_ceiling IN ('branch','dev','stage','prod')),
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','frozen','retired')),
  frozen_reason   text NOT NULL DEFAULT '',
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sage.mcp_tokens ADD COLUMN IF NOT EXISTS principal_id text
  REFERENCES sage.guard_principals(id);

CREATE TABLE IF NOT EXISTS sage.guard_cluster_roles (
  principal_id      text NOT NULL REFERENCES sage.guard_principals(id),
  cluster_key       text NOT NULL,
  login_role        text NOT NULL,
  broker_role       text NOT NULL,
  broker_secret_ct  bytea NOT NULL,       -- AES-GCM; AAD = principal_id||cluster_key||role
  key_id            text NOT NULL,
  valid_until       timestamptz,
  prior_attrs       jsonb,
  status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active','killed','retired')),
  rotated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal_id, cluster_key),
  UNIQUE (cluster_key, login_role),
  UNIQUE (cluster_key, broker_role)
);

CREATE TABLE IF NOT EXISTS sage.guard_environment_labels (
  database_id    uuid PRIMARY KEY,         -- sage.sre_database_bindings.database_id
  label          text NOT NULL DEFAULT 'prod' CHECK (label IN ('branch','dev','stage','prod')),
  identity       jsonb NOT NULL,           -- {provider_ref, system_identifier, db_oid}
  verified       boolean NOT NULL DEFAULT false,
  set_by         text NOT NULL,
  set_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sage.guard_inflight (
  principal_id text NOT NULL, database_id uuid NOT NULL, backend_pid integer NOT NULL,
  action_id bigint, started_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (database_id, backend_pid)
);

CREATE TABLE IF NOT EXISTS sage.guard_taint (
  principal_id text NOT NULL REFERENCES sage.guard_principals(id),
  task_id      text NOT NULL DEFAULT '*',
  source       text NOT NULL,
  tainted_at   timestamptz NOT NULL DEFAULT now(),
  cleared_at   timestamptz,               -- no expiry; cleared only per agents.taint.clear_on
  cleared_by   text,                      -- 'context_reset' | 'rotation' | a user's email
  PRIMARY KEY (principal_id, task_id, source)
);

-- G1, every monitored database (its own sage schema) ---------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_grants (
  id               bigserial PRIMARY KEY,
  database_id      uuid NOT NULL,
  principal_id     text NOT NULL,
  lane             text NOT NULL CHECK (lane IN ('direct','broker')),
  capability       text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_]{0,63}$'),
  object_oid       oid NOT NULL,
  object_name      text NOT NULL,
  columns          text[] NOT NULL,        -- always a list; never a secret column
  privileges       text[] NOT NULL,
  grantor          text NOT NULL,          -- from aclexplode after GRANT
  granted_at       timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz NOT NULL,   -- computed in SQL on this database's clock
  revoked_at       timestamptz,
  state            text NOT NULL DEFAULT 'active'
                   CHECK (state IN ('active','revoked','revoke_incomplete')),
  grant_action_id  bigint NOT NULL,
  revoke_action_id bigint,
  CHECK (expires_at > granted_at)
);
CREATE INDEX IF NOT EXISTS guard_grants_open_idx ON sage.guard_grants (expires_at)
  WHERE revoked_at IS NULL;

ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS on_behalf_of   text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS task_id        text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS approval_id    bigint;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS envelope_id    text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS artifact_hash  text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS policy_version integer; -- as sage.decision
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS artifact_hash  text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS task_id        text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS artifact_hash  text;

-- proposed_via: a new constraint name, added once; validated in a separate transaction
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_queue_proposed_via_v2'
                 AND conrelid = 'sage.action_queue'::regclass) THEN
    ALTER TABLE sage.action_queue DROP CONSTRAINT IF EXISTS action_queue_proposed_via_check;
    ALTER TABLE sage.action_queue ADD CONSTRAINT action_queue_proposed_via_v2
      CHECK ((proposed_via IS NULL OR proposed_via IN ('ask_sage','agent'))
             AND (proposed_by IS NULL OR length(proposed_by) BETWEEN 1 AND 200)) NOT VALID;
  END IF;
END $$;
-- next migration step, its own transaction:
-- ALTER TABLE sage.action_queue VALIDATE CONSTRAINT action_queue_proposed_via_v2;

-- facts: column classes. The inline CHECKs at facts_migration.go:21-24 carry the default names
-- facts_fact_type_check and facts_subject_kind_check; the same "add once by new name" pattern
-- replaces them with facts_fact_type_v2 (adds 'column_class') and facts_subject_kind_v2
-- (adds 'column').
ALTER TABLE sage.facts ADD COLUMN IF NOT EXISTS subject_relid oid;
ALTER TABLE sage.facts ADD COLUMN IF NOT EXISTS subject_attnum smallint;
CREATE UNIQUE INDEX IF NOT EXISTS facts_column_class_uniq
  ON sage.facts (subject_relid, subject_attnum, fact_type) WHERE subject_kind = 'column';

-- G2, every monitored database ------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_capture_tokens (
  token       uuid PRIMARY KEY, database_id uuid NOT NULL, action_id bigint NOT NULL,
  rel         regclass NOT NULL, expires_at timestamptz NOT NULL,
  pre_used_at timestamptz, post_used_at timestamptz
);
CREATE TABLE IF NOT EXISTS sage.guard_preimages (
  database_id uuid NOT NULL, action_id bigint NOT NULL, seq integer NOT NULL,
  rel oid NOT NULL, phase text NOT NULL CHECK (phase IN ('pre','post')),
  key jsonb NOT NULL, row_data jsonb NOT NULL,
  captured_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
  PRIMARY KEY (database_id, action_id, seq, phase)
);
CREATE TABLE IF NOT EXISTS sage.guard_restore_points (
  database_id uuid NOT NULL, action_id bigint NOT NULL, lsn pg_lsn NOT NULL,
  taken_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (database_id, action_id)
);
CREATE TABLE IF NOT EXISTS sage.guard_query_audit (
  id bigserial PRIMARY KEY, database_id uuid NOT NULL, principal_id text NOT NULL,
  task_id text, envelope_hash text NOT NULL, verdict text NOT NULL, reason text,
  row_count integer, classes text[], at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sage.action_approvals (   -- action_queue.id and users.id are SERIAL
  queue_id integer NOT NULL REFERENCES sage.action_queue(id) ON DELETE CASCADE,
  approver_user_id integer NOT NULL REFERENCES sage.users(id),
  decision text NOT NULL CHECK (decision IN ('approve','deny')),
  card_hash text NOT NULL, decided_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (queue_id, approver_user_id)
);

-- G2, control DB: ledger (same shape as the SRE M7 tables) ---------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_autonomy (
  principal_id text NOT NULL, database_id uuid NOT NULL,
  capability   text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_]{0,63}$'),
  level        smallint NOT NULL CHECK (level BETWEEN 0 AND 3),
  envelope     jsonb NOT NULL DEFAULT '{}', evidence jsonb NOT NULL DEFAULT '{}',
  version      bigint NOT NULL DEFAULT 1, changed_by text NOT NULL, change_reason text NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal_id, database_id, capability)
);
-- guard_autonomy_proposals (one pending per key), guard_autonomy_events (append-only),
-- guard_autonomy_outcomes: the columns of sre_m7_autonomy_migration.go with principal_id,
-- database_id and capability as the key.

-- G2, control DB: sandboxes and clones -----------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.clone_instances (
  id bigserial PRIMARY KEY, deployment_id uuid NOT NULL, adapter text NOT NULL,
  scope text NOT NULL, ref text, name text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('rehearsal','sandbox','drill','gameday','bench',
                                           'masked_parent')),
  principal_id text, source_database_id uuid, masked boolean NOT NULL DEFAULT false,
  status text NOT NULL CHECK (status IN ('creating','create_uncertain','ready','destroying',
                                         'destroyed','failed')),
  expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE (adapter, scope, name)
);

-- G3, every monitored database -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_trash_map (
  action_id bigint PRIMARY KEY, database_id uuid NOT NULL, original_schema text NOT NULL,
  original_name text NOT NULL, object_oid oid NOT NULL, trashed_at timestamptz NOT NULL
  DEFAULT now(), purge_after timestamptz NOT NULL, purged_at timestamptz
);

-- G4, control DB ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_estate (
  id bigserial PRIMARY KEY, provider text NOT NULL, scope text NOT NULL, ref text NOT NULL,
  parent_ref text NOT NULL DEFAULT '',
  kind text NOT NULL CHECK (kind IN ('project','branch','database','sandbox')),
  intent text NOT NULL DEFAULT 'unknown' CHECK (intent IN ('ephemeral','durable','unknown')),
  owner_principal text, owner_user_id integer, ttl_expires_at timestamptz,
  last_active_at timestamptz, compute_state text NOT NULL DEFAULT 'unknown',
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','idle','suspended',
    'archived','reclaim_pending','reclaimed','gone')),
  created_by_sage boolean NOT NULL DEFAULT false, tags jsonb NOT NULL DEFAULT '{}',
  first_seen_at timestamptz NOT NULL DEFAULT now(), last_seen_at timestamptz NOT NULL
  DEFAULT now(), UNIQUE (provider, scope, ref)
);
CREATE TABLE IF NOT EXISTS sage.guard_estate_usage (
  estate_id bigint NOT NULL REFERENCES sage.guard_estate(id) ON DELETE CASCADE,
  day date NOT NULL, compute_seconds numeric, storage_bytes bigint,
  reported_cost numeric(14,4), reported_currency text,   -- only when the provider returns cost
  source text NOT NULL CHECK (source IN ('provider_api')), PRIMARY KEY (estate_id, day)
);

-- E2 ---------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sage.guard_identity_bindings (
  issuer text NOT NULL, subject text NOT NULL,
  principal_id text NOT NULL REFERENCES sage.guard_principals(id) ON DELETE CASCADE,
  created_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issuer, subject)
);
```

**Retention** is in §6.17.

**AgentDB tables** are dropped by an explicit list of 27 names (§12). The `guard_` prefix avoids
the collision with `agent_db_*`.

---

## 8. Interfaces

### 8.1 Error model

Agent tools follow pg_sage's existing MCP contract (`internal/mcp/errors.go:3-7`):
- **Gate outcomes are normal results.** They carry a `verdict`, as the intent tools do today
  (`internal/mcp/intent_execution_types.go:61,73`).
- **Failures the model can act on** are `tools/call` results with `isError: true`. Their
  `structuredContent` carries an existing code.
- **Authentication** fails before JSON-RPC: HTTP 401 with `mcp_token_required`
  (`internal/api/mcp_principal.go:20-33`).

| Kind | MCP | REST |
|---|---|---|
| Missing or invalid token, wrong audience (E2) | HTTP 401 `mcp_token_required` (existing) | 401 |
| Malformed arguments | `isError`, -32602 `invalid_arguments` | 422 |
| Scope missing | `isError`, -32001 `scope_required` | 403 |
| Agent asks for an approve-scope tool | `isError`, -32005 `approval_reserved_for_humans` | 403 |
| Database outside the token or principal | `isError`, -32003 `database_not_permitted` | 403 |
| Unknown database, action or sandbox | `isError`, -32007 `unknown_database` or -32004 `not_found` | 404 |
| Rate limit (D9 `agent_rate`) | `isError`, -32011 `rate_limited`, with `retry_after` | 429 |
| Substrate or control DB unavailable | `isError`, -32010 `unavailable` | 503 |
| Gate outcome: `blocked` (`agent_*`, `bound_exceeded`, `unsupported_shape`, `grantor_lacks_privilege`, `capture_unavailable`, …) | result `{verdict: "blocked", reason_code, fix?}` | 409 with the same body |
| Gate outcome: `observe_only` (L1 proposal recorded) | result `{verdict: "observe_only", reason_code, proposal_id}` | 200 |
| Gate outcome: `queue_approval` | result `{verdict: "queue_approval", approval_url, expires_at}` | 202 |
| Database error during execution | result `{verdict: "execute", status: "failed", sqlstate, message (pg_sage-authored), retryable}` | 409 |

All list endpoints and tools page with `{limit ≤ 200, cursor}` → `{items, next_cursor}`.

### 8.2 MCP tools (new)

**Scopes.** `read`: `agent_whoami`, `agent_query`, `agent_action_status`, `migration_status`,
`estate_list`. `propose`: everything else.

| Tool | Input (JSON Schema, abbreviated) | Output |
|---|---|---|
| `agent_whoami` | `{}` | `{principal:{id,name,profile,env_ceiling,status}, sponsor, databases:[{name, env, binding_verified, lanes, grants:[{capability, object, columns, expires_at}], levels:{capability: 0..3}}], tainted, frozen}` |
| `agent_query` | `{database: string, sql: string, params?: array, max_rows?: int ≤ ceiling}` | `{verdict, reason_code?, columns:[{name, type, class}], rows, truncated, row_count, envelope_hash, masked:[col]}` (rows fenced) |
| `agent_request_capability` | `{database, capability, objects:[string], columns?: {object:[col]}, duration_minutes: int ≤ max, reason: string}` | `{verdict, reason_code?, grant_ids?, expires_at?, approval_url?, fix?}` |
| `agent_propose_write` | `{database, sql, params: array, max_rows: int ≤ ceiling, verify_sql?: string, reason}` | `{verdict, reason_code?, action_id, envelope_hash, affected_rows?, undo_available, undo_until?}` |
| `agent_action_status` | `{action_id}` | `{status, verdict, verification, undo_available}` |
| `agent_request_undo` | `{action_id, reason}` | `{verdict, undo_action_id?, conflicts?:[key]}` |
| `request_sandbox` | `{source_database, data: "schema_only"\|"masked"\|"full", ttl_minutes: int ≤ 1440, purpose}` | `{verdict, sandbox:{name, status, expires_at}}`. Used as `database: <name>` in the other tools |
| `release_sandbox` | `{sandbox}` | `{status}` |
| `propose_migration` | `{database, up_sql, down_sql?, verify_sql?, source?:{repo, ref, path}, handoff?: "atlas"\|"bytebase"\|"pr"}` | `{migration_id, risk_class, groups:[{statements, lock, rewrite, destructive}], rehearsal:{status}, verdict, approval_url?}` |
| `migration_status` | `{migration_id}` | `{rehearsal_evidence, verdict, apply_status, verification}` |
| `estate_list` | `{provider?, status?, intent?, limit?, cursor?}` (agents see their own) | `{items, next_cursor, totals}` |

- `apply_migration` keeps its input shape and routes into `propose_migration` from G3.
- MCP elicitation only sends the approval URL.

### 8.3 REST (session auth)

| Method and path | Role | Request → response |
|---|---|---|
| `GET /api/v1/agents` | operator | `?limit&cursor&status` → `{items:[principal], next_cursor}` |
| `POST /api/v1/agents` | admin | `{name, sponsor_user_id, profile, env_ceiling, tenant?}` → 201 principal; 409 on a duplicate name |
| `PATCH /api/v1/agents/{id}` | admin (two people if widening) | `{sponsor_user_id?, profile?, env_ceiling?, status?: "retired"}` → 200, or 202 pending a second approver |
| `POST /api/v1/agents/{id}/tokens` | admin | `{name, scopes:["read","propose"], databases, expires_in_days ≤ 90}` → 201 `{token (shown once), ...}` |
| `GET /api/v1/agents/{id}/activity` | operator | `?database&from&to&limit&cursor` → `{sessions, statements, grants, actions, taint}` |
| `POST /api/v1/agents/{id}/freeze` | operator | `{reason}` → 200 |
| `POST /api/v1/agents/{id}/unfreeze` | admin (two people after a kill) | `{reason}` → 202 |
| `POST /api/v1/agents/kill` | admin | `{scope: "all"\|"principal"\|"database", id?, reason}` → 200 `{databases:[{name, roles_disabled, backends_terminated, replicas, verified, error?}]}` |
| `GET /api/v1/agent-environments/{database}` | operator | → `{label, verified, identity, set_by, set_at}` |
| `PUT /api/v1/agent-environments/{database}` | admin (two people if widening) | `{label}` → 200, or 202 pending |
| `GET /api/v1/agents/{id}/autonomy` | operator | → `{items:[{database, capability, level, envelope, evidence, proposal?}]}` |
| `POST /api/v1/agents/{id}/autonomy/{database}/{capability}/promote` | per §6.11 | `{proposal_id, envelope}` → 200, or 202 pending a second signer |
| `POST /api/v1/agents/{id}/autonomy/{database}/{capability}/demote` | operator | `{level, reason}` → 200 |
| `GET /api/v1/estate` | operator | `?provider&status&intent&limit&cursor` → `{items, next_cursor, totals}` |
| `POST /api/v1/estate/{id}/intent` | operator | `{intent, owner_principal?, ttl_hours?}` → 200 |
| `GET /api/v1/estate/usage` | operator | `?group_by=principal\|tenant\|provider&from&to` → `{rows}` |
| `GET /api/v1/evidence-packs` | admin | `?from&to&format=json\|csv` → file |
| `GET /api/v1/agentdb/decommission` | admin | → the §12 inventory |
| `POST /api/v1/agentdb/decommission/ack` | admin | `{acknowledged_resources:[id], exported: true}` → 200 |

### 8.4 UI

- **Agents page.**
  - Principals table.
  - Drawer: lanes, grants with countdowns, live sessions, statements, taint, the trust ladder per
    capability with the promotion coach, freeze.
  - A global **Kill agents** button (typed phrase `KILL AGENTS`) with verification per database
    and replica.
- **Agent approval card:**
  - canonical statement with hidden characters escaped;
  - environment and binding;
  - rows and locks;
  - restore point;
  - undo plan;
  - rehearsal;
  - LLM narrative (cited);
  - sponsor;
  - quorum (1 or 2);
  - typed object names for destructive classes.
- **First look and findings:** an "Agent posture" section, AP-01..AP-17.
- **Estate page (G4).**
- **Removed:** the AgentDB pages (`web/src/pages/agentdb/`, 20 files, plus `AgentDBsPage.jsx` and
  its two tests), their navigation entries and their Playwright specs.

---

## 9. Configuration

Every key goes into `internal/config/key_classes.txt`. The widening keys are `safety_critical`:
`profiles`, `unmask`, `direct_write_environments`, `exposed_roles`, `trigger_allowlist`,
`single_operator_mode`.

```yaml
agents:
  control_database: ""            # required unless mode: meta; else Guard is posture-only
  default_environment: prod
  exposed_roles: []               # adds anon + authenticated automatically when both exist
  client_patterns: ["^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"] # hints
  single_operator_mode: false
  change_manager_group: ""        # E3; co-signs L3 envelopes when set
  broker:
    rotation_days: 7
    pool_max_conns: 2
    pool_idle_seconds: 60
    max_total_connections: 20
  roles:
    connection_limit: 5
    statement_timeout_ms: 30000
    lock_timeout_ms: 2000
    idle_in_transaction_timeout_ms: 60000
    idle_session_timeout_ms: 600000  # bounds sessions a kill can't reach (GR-01)
    transaction_timeout_ms: 600000   # PG17+
    temp_file_limit_mb: 1024         # where permitted (G1-13)
    retire_grace_days: 7
  query:
    max_rows: 200
    max_rows_ceiling: 1000
    max_bytes: 1048576
    replica: off                     # off | prefer
  capabilities:
    max_duration_minutes: 240
    direct_write_environments: [dev, branch]
  direct:
    max_password_age_hours: 24       # VALID UNTIL bound for operator-managed logins (GR-07)
  writes:
    max_rows_ceiling: 1000
    max_preimage_bytes: 8388608
    preimage_retention_days: 7
    truncate_max_rows: 10000
    trash_retention_hours: 48
    trigger_allowlist: []            # [schema.table]; admin only
  approvals:
    ttl_minutes: 15
    chat_classes: []
    max_pending_per_principal: 10
    max_requests_per_hour: 30
  taint:
    clear_on: [context_reset, operator, rotation]   # no time-based expiry (GR-05)
  anomaly_min_rows: 10000
  kill_verify_timeout_seconds: 10
  require_restore_drill_days: 14
  reconcile_interval_seconds: 60     # lease expiry and grant drift; under the v2.3 lease
  trust:
    min_executions: {write: 10, maint: 10, ddl: 5}
    acceptance_min: 0.9
    signal_free_days: 7
  posture:
    memory_growth_gb_day: 5
    daily_at: "03:00"
  unmask: []                         # [{principal, database_id, columns, purpose, expires}]
  profiles:
    readonly-analyst: {classes: [read], env_ceiling: prod}
    app-writer: {classes: [read, write_insert, write_update], env_ceiling: prod, schemas: [app],
                 direct_lane: true}
    coding-agent: {classes: [read, write_insert, write_update, write_delete, ddl_additive,
                   ddl_locking, sandbox], env_ceiling: stage, direct_lane: false}
    legacy: {classes: [read], env_ceiling: prod}
mcp:
  stdio_principal: ""
clone:
  provider: none                     # none|dle|snapshot|neon|lakebase|template|schemaonly
  sandbox_cluster_dsn_env: ""
  masked_parent_refresh_hours: 24
  drill_tables: 5
  drill_interval_days: 7             # copy-on-write substrates only (GR-02)
databases:                           # existing list; one new optional field per entry
  - replicas: []                     # [{name, dsn_env}]; the kill switch reaches only these
estate:                              # G4
  sources: []                        # [{provider, api_key_env, org_id, reclaim_api_key_env?}]
  observe_interval_minutes: 60
  wake_budget_per_day: 0
  suspend_after_idle_minutes: 30
  reclaim_after_idle_days: 7
  reclaim_delay_hours: 48
  intent_rules: []
  budgets: []                        # [{group_by, id, unit: compute_seconds|storage_bytes|cost,
                                     #   monthly_limit}]; provider units only (GR-10)
```

**Removed `agentdb:` keys.**
- They are **warned about and ignored** from G0. Startup fails only when
  `agentdb.live_provisioning_enabled: true` is set (§12).
- A G0 migration deletes persisted `agentdb.*` rows from `sage.config` and records an audit entry.

---

## 10. Releases and acceptance

All releases are tests-first, per CLAUDE.md, with the full test report on PG14–18. Effort is in
agent-days and indicative.

### 10.1 G0: decommission, posture, launch (≈8 agent-days, plus E1 ≈5)

**Scope:**
- the §12 inventory and the removal manifest;
- detectors AP-01..AP-16 (AP-17 lands in G2) and the first-look section;
- AU-10 scrubbing;
- docs (`docs/agent-guard.md` with the manual runbook);
- **AgentSafetyBench v0** with the launch write-up (§11).

| Check | Statement |
|---|---|
| G0-01 | The inventory selects every row with evidence of a live call: `live_mode`, a non-empty `create_operation_id` or `provider_resource_id`, a live receipt, an `execute_live` attempt or a consumed authorization, across **all** statuses, including `failed`, `provisioning`, `status_unknown`, `status_checked` and `destroyed`-after-wrong-scope. It also includes RDS final snapshots, `local_postgres` artifacts and `sage` schemas in fleet-attached agent databases. Each item carries a per-provider delete template; golden tests pin the templates. |
| G0-02 | `git grep -ilE 'agent[-_]?db\|agt_'` over the repo matches only the allowlist: the decommission package, migrations, CHANGELOG and `reviews/`. Removed routes return 404. The auth-middleware exemptions are gone. |
| G0-03 | Per-detector fixture coverage on PG14–18. Version-specific arms skip with a recorded reason (AP-06 reports on PG14; AP-14's inheritance arm is PG14/15). Provider arms use recorded `cloudtel` fixtures. AP-12 uses two timed observations. |
| G0-04 | The first look reports "Agent posture" within budget, with the perf gate green. On lifeos it adds less than 1 s. |
| G0-05 | No posture finding proposes above L1. |
| G0-06 | A hash check finds none of the AU-10 secrets in tracked files (the strings aren't embedded in the test). The CHANGELOG gives rotation guidance, because the history keeps them. |
| G0-07 | With `agentdb:` keys present (and `live_provisioning_enabled` false), startup warns and ignores them. With it true, startup refuses, naming §12. Persisted `agentdb.*` overrides are deleted, with an audit row. |
| G0-08 | AgentSafetyBench v0 runs in CI, and its report is published with the release (§11). |

### 10.2 G1: identity, gate composition, classification, kill switch (≈12 agent-days, plus E2 ≈6)

**Scope:**
- §6.2 (all of it), §6.3 contracts for `guard_role_*`, `guard_grant`/`guard_revoke` and
  `guard_freeze`/`guard_unfreeze`, §6.4, §6.5, §6.6, §6.7, the kill switch;
- attribution views, `agent_whoami`, `agent_query` (reads, brokered), the Agents page;
- provenance columns;
- Guard reconcilers under v2.3's lease.

**Rules in G1:**
- Every grant is L2, operator-approved.
- `prod` grants exclude unclassified columns.
- Guard needs `trust.level` of `advisory` or higher to act; narrowing actions are exempt.

| Check | Statement (traces) |
|---|---|
| G1-01 | For every backend whose `usename` matches `^sage_agentb?_`: the role is registered, `rolsuper = false`, `rolbypassrls = false`, it has no membership in `pg_read_server_files`, `pg_write_server_files`, `pg_execute_server_program` or `pg_write_all_data`, and it owns nothing. A startup self-check asserts the same for pg_sage's own role's Guard-relevant attributes (SAFE-ID-01, SAFE-PERM-01). |
| G1-02 | An agent's effective privileges (`has_*_privilege` over the catalog, excluding `pg_catalog` and `information_schema`) = its Guard grants ∪ the PUBLIC baseline recorded at preflight. They don't include the launching admin's privileges (SAFE-ID-02). |
| G1-03 | A token for database A is refused on B. A foreign `aud` gets 401 (E2) (SAFE-ID-04). |
| G1-04 | An agent's claimed environment is ignored. An unverified binding is `prod` (SAFE-PERM-04). |
| G1-04b | A clone or standby of `prod` registered as `dev` without a receipt, or a re-pointed DSN, is evaluated as `prod` and raises a critical finding. |
| G1-05 | The kill, triggered during a 30 s brokered statement on the primary **and** one on a configured replica: within 10 s no agent backend remains on either, new logins fail on both and on an unconfigured standby, approvals are `cancelled_kill`, and both statements are cancelled. The report names the unconfigured standby and its timeout bound (SAFE-HUM-04). |
| G1-06 | Unfreeze after a kill restores `prior_attrs` and rotates every credential: old passwords fail. |
| G1-07 | After any Guard action, AP-02 is zero. |
| G1-08 | A grant expires on schedule: privilege false within one reconcile interval, revoke audited, `GRANTED BY` the recorded grantor, no residue. |
| G1-08b | The same with `emergency_stop` on and `trust.level: observation` (narrowing). |
| G1-09 | Grantor without `GRANT OPTION` → `denied: grantor_lacks_privilege`, with the exact statement. Residue from another grantor → `revoke_incomplete`, and D10 denies the object. |
| G1-10 | Attribution: each statement of a broker role appears in the principal's activity. When `pg_stat_statements_info.dealloc` advances, the view reports dropped attribution and falls back to `guard_query_audit`. |
| G1-11 | Migrated legacy tokens: slugs are valid and unique. Read tools work. Propose tools queue (L2). `agent_*` returns `agent_unsponsored`. |
| G1-12 | A frozen principal's `apply_migration` and `request_change` are denied. A tainted principal's are queued (CG-08). |
| G1-13 | A gated live matrix (RDS, Aurora, Cloud SQL, Azure Flexible, Supabase, Neon) records receipts for each of the following, committed scrubbed under `reviews/`: `CREATE ROLE`, each `ALTER ROLE … SET` knob, `GRANT … WITH GRANT OPTION`, terminating agent backends, reading `pg_control_system()`. |
| G1-14 | An agent can't change profiles, policy or prompts. Its `propose_policy_change` for a widening change needs two humans (SAFE-TOOL-08). |
| G1-15 | `agent_query` corpus RO-01..RO-16, plus the write-path `set_config`, `VALUES` and `INSERT … SELECT` cases and a masked-column cast case: each fails with 42501 or is refused before execution, with checksums unchanged (SAFE-PERM-01, SAFE-TOOL-02). |
| G1-16 | With PUBLIC `CREATE` on a profile schema, grants are refused with the exact `REVOKE` (P1). |

### 10.3 G2: sandboxes, branch and dev writes, earned agent autonomy (≈16 agent-days, plus E3 ≈8)

**Scope:**
- §6.9 (writes in `branch` and `dev` only, including sandboxes);
- §6.10 (taint, canaries, anomalies, freeze);
- §6.11 (ledger, SoD, the two-person model, channels, limits);
- §6.12 (substrates, sandboxes, masked parents; drills arrive in G3);
- AP-17;
- the LLM features listed for G2;
- signing and SBOM (moved from E4).

| Check | Statement |
|---|---|
| G2-01 | Envelope binding: approve E, then execute E is allowed. Changing any one field (SQL token, comment, invisible character, `max_rows`, `verify_sql`, database, principal, policy version) is denied. So are replay, another queue id, and expiry (SAFE-PERM-02). |
| G2-02 | A `DELETE` with `max_rows` 10 matching 1,000 rows is rolled back, with `bound_exceeded` (SAFE-PERM-03). |
| G2-03b | A delete on a table referenced by `ON DELETE CASCADE` is denied. With the rule disabled in a test build, the transaction tuple bound rolls back a cascade over the bound. |
| G2-04 | Undo after an `UPDATE` of 50 rows restores them byte-equal. A row modified in between is a conflict and stays unchanged. |
| G2-04b | A write on a table with a non-allowlisted trigger is denied. Generated and identity columns undo correctly. |
| G2-05 | Taint: an injected instruction in a ticket row is read, then a write is proposed under a *fresh* task id, and again after 24 simulated hours with no clear event. Both writes are queued for a human. After an operator clear or a new stdio process, writes follow the ledger again (SAFE-TOOL-04). |
| G2-06 | With the freeze on, brokered writes are denied and direct writes get 42501 (SAFE-PERM-05). |
| G2-07 | A `secret` column is refused in both lanes (42501 or D5). A new column isn't visible until classified (SAFE-PERM-07). |
| G2-08 | A canary read freezes the principal and pages within 30 s. The INC-04 replay fires an anomaly within 60 s (SAFE-MON-03, SAFE-MON-04). |
| G2-09 | A sponsor approving their own agent's `prod` request is rejected. `write_delete` without typed names is rejected. Widening without a second admin stays pending (SAFE-HUM-03, SAFE-HUM-05). |
| G2-10 | A consent variable or a chat "approved" never unlocks. A chat card has no approve action for agent classes. Unanswered past TTL → `denied` (SAFE-HUM-01). |
| G2-11 | Promotion is proposed only at the thresholds, signed by the right role and never by the sponsor. A verification failure demotes to L2. A canary hit demotes to L0. |
| G2-12 | Brokered `agent_query` p95 overhead ≤ 25 ms with a warm proof cache, on a local database in the perf gate. Remote latency is reported, with no gate. |
| G2-13 | Every agent record carries the principal, `on_behalf_of`, task, approval or envelope id, hash, policy version and catalog hash. Agent roles can't modify `sage.*` (SAFE-MON-01). |
| G2-14 | A masked sandbox: no credential and no registration before the masking and PII scan pass. `pii` and `secret` are masked. Pre-mask history is unreachable through pg_sage. No public exposure and no anonymous grants (SAFE-BR-02, SAFE-BR-04). |
| G2-15 | Every adapter: a crash between the receipt and the provider response → `create_uncertain`, then adoption within scope. A destroy with a tag or scope mismatch is refused. A wrong-scope "not found" is never recorded as destroyed. |
| G2-16 | A cross-tenant probe (tenant A's principal against tenant B's objects) returns 42501 or zero rows on every tool (SAFE-ID-05). |
| G2-17 | The AP-17 RLS isolation probe catches a seeded permissive policy on a sandbox (SAFE-PERM-06). |
| G2-18 | `sage_guard.capture` fuzzing: relations without a token, non-key and extra key fields, quote and semicolon payloads in values, wrong phases and reused tokens all raise the one fixed error, and `sage.guard_preimages` stays unchanged (GR-06). |

### 10.4 G3: production writes, restore drills, change path, direct lane (≈12 agent-days)

**Scope:**
- §6.9 in `stage` and `prod`;
- §6.12 drills;
- §6.13 (all of it: B1, B2 and B7 fixes, groups, hand-off, delayed drops);
- the direct lane (§5.1) with E3 credential delivery;
- `maint_on_behalf`.

| Check | Statement |
|---|---|
| G3-01 | `DROP COLUMN` is `destructive`, rehearsed, L2, two people in `prod` (SAFE-BR-01). |
| G3-02 | `ADD COLUMN … DEFAULT now()` reports the correct rewrite flag on PG14 and PG18, and the measured lock. |
| G3-03 | `schema_migration` goes through `Executor.Apply`. A lock timeout retries 3 times, then halts, with no group left half-applied (SAFE-TOOL-05). |
| G3-04 | `rehearsal` returns `promote_expand` when the evidence, including the replayed-query evidence, is complete (B1). A blocked migration persists `blocked` (B2). |
| G3-05 | Delayed drop: trashed with privileges revoked and publications detached. Restorable within 48 h. An agent's purge is denied (SAFE-BAK-05). |
| G3-06 | A drill restores to T on a copy-on-write substrate. Checksums match. A table written during the drill is dropped from it, not failed. A stale drill caps `prod` writes at L2. With no such substrate, `prod` writes stay at L2 or below, and no full-instance restore is ever started (SAFE-BAK-04, D7). |
| G3-07 | `prod` writes without PITR posture are denied (`agent_no_pitr`). |
| G3-08 | pg_sage generates and stores no direct-lane password: the role logs in through IAM auth, or with an operator-managed password whose `VALID UNTIL` is within `agents.direct.max_password_age_hours`. A secret scan of logs, API responses and MCP outputs finds no credential (SAFE-ID-03). |
| G3-09 | Neon and Lakebase adapters pass a gated live run. Scrubbed receipts are committed. |
| G3-10 | `maint` on PG14–16 runs as `maint_on_behalf` with attribution. Nothing runs inside a transaction block. |
| G3-11 | Direct lane: with a direct session open on an unconfigured standby, a kill blocks new logins there at once, and the open session ends within its `idle_session_timeout` or `statement_timeout` bound, as the kill report states. IAM-auth and operator-managed logins both work, and pg_sage stores no direct-lane password (GR-01, GR-07). |
| G3-12 | The classifier marks `ALTER TABLE … DROP CONSTRAINT` and dropping a unique index `destructive`, and dropping a non-unique index `locking`. Their `down` step is the recorded `pg_get_constraintdef` or `pg_get_indexdef` (GR-04). |

### 10.5 G4: agent estate operations (≈10 agent-days; needs v2.3)

| Check | Statement |
|---|---|
| G4-01 | With `wake_budget_per_day: 0`: zero connections to an `idle` fake provider over 24 simulated hours, and at most one observation per interval when active. A live Neon run shows suspension on schedule. |
| G4-02 | No `sage` schema in observe-tier estate databases. |
| G4-03 | `estate_reclaim` never runs on `durable` or `unknown` without L2, or without a receipt or an opt-in owner policy. It waits for `reclaim_delay_hours` and can be cancelled. |
| G4-04 | For a project running a metered synthetic workload, the recorded usage equals the provider's usage API for each day, and attribution to the right principal is exact. No row carries a cost the provider didn't report. |
| G4-05 | 300 clones of one template with the same missing index produce one fleet finding. |
| G4-06 | A budget at 100% proposes `estate_quarantine`. At L3 it executes and reverses. |
| G4-07 | The zombie sweep lists every resource tagged with this `deployment_id` that the registry doesn't know, and takes no action. |
| G4-08 | Estate databases promoted to the full runtime start at L0 for pg_sage's own actions, and use `sslmode=verify-full`. |

### 10.6 Go/no-go gates

| Before | Gate |
|---|---|
| G0 | D-1 decided by you |
| G1 | G0 shipped and the launch write-up published. **Pull metric**, measured for 6 weeks after launch: (a) a named design partner, or (b) ≥ 3 external GitHub issues, discussions or PRs about Agent Guard or the bench, or (c) ≥ 50 new stars. These thresholds are proposals; you set them (D-2). If the metric isn't met, G1 waits unless you decide otherwise |
| G2 | G1 dogfooded on lifeos with your Claude Code as the stdio principal, **read-only**, which keeps the lifeos rule. A kill drill passed. G1-13 receipts for at least 3 platforms. |
| G3 | G2 used on sandboxes for 2 weeks with zero unexplained undo conflicts. Signing and SBOM shipped. A design partner for production writes, or your explicit go |
| G4 | v2.3 `history.store: meta` and fingerprints merged. Neon adapter live-verified (G3-09). A design partner with ≥ 100 agent-created databases, or a scripted 500-branch Neon organization |

### 10.7 Enterprise track (reordered to the research)

| Release | Items |
|---|---|
| **E1 (with G0)** | Encrypt API-set secrets at rest with key ids (CG-01). Serve TLS (CG-02). OIDC with PKCE, nonce and `id_token` validation (CG-03). Group → role mapping. pg_sage reads its *own* credentials from `*_FILE` paths, which every secrets manager can fill (Vault Agent, CSI drivers, External Secrets); no vendor SDKs (GR-11). Audit logins and user and role changes. A break-glass admin with an alert. A readiness endpoint |
| **E2 (with G1)** | MCP as an OAuth 2.1 resource server with identity bindings (CG-06). SIEM export as OCSF over HTTP, syslog and OTLP. A hash-chained audit. pgaudit correlation. Evidence packs. A declarative, versioned file for principals, profiles and envelopes, with plan and apply semantics (PC-1, AP-5) |
| **E3 (with G2)** | Per-database human RBAC (D4a option B). Data-owner and change-manager groups. SCIM deprovisioning → L0. ITSM change-record webhooks. Direct-lane login through cloud IAM database auth, or operator-managed passwords with a `VALID UNTIL` bound (needed by G3's direct lane; pg_sage delivers no agent credential, GR-11) |
| **E4 (before G3)** | Signed images and SBOM (moved before production writes). A Helm chart with probes, `securityContext` and a PDB. Leader election reused from v2.3 (Guard reconcilers use it from G1) |

E checks are written per item before each release starts. Two examples:
- E1-01: `SELECT value FROM sage.config WHERE key = 'llm.api_key'` returns ciphertext.
- E1-03: a callback with a forged nonce gets 401.

---

## 11. AgentSafetyBench

An open, reproducible bench that measures Agent Guard the way PGIncidentBench measures Sage SRE. It
runs in CI on PG14–18. Scenarios are scripted and deterministic. A live-LLM arm runs behind
`PG_SAGE_LIVE_LLM=1`.

**v0 (G0, the launch).**
- **Posture scenarios:** a Supabase-style exposed table, a permissive policy, a definer function,
  PUBLIC `CREATE`, pgvector versions.
- **The read-only corpus RO-01..RO-16** (incidents §4.4), run against three configurations:
  - a `READ ONLY` transaction only;
  - a privilege-based read-only role;
  - pg_sage's EXPLAIN guard.

  This is publishable on its own: "which read-only designs survive which attacks".
- **The incident-to-control mapping** below, with expected outcomes.

**v1 (G1).** Adds identity, the kill switch and brokered-read scenarios. **v2 (G2).** Adds writes,
taint, canaries and sandboxes.

**Scoring is against declared expectations** (SR-59): each incident is prevented, detected, or out
of scope.

| Incident (incidents §1) | Expected with Agent Guard | Why |
|---|---|---|
| INC-01 Lovable RLS exposure | Detected (AP-03/04), prevented for Guard roles | Posture plus column grants |
| INC-04 Supabase MCP exfiltration | Prevented when the agent uses Guard | `secret` class, taint, broker login |
| INC-06 Replit freeze-and-drop | Prevented when the agent uses Guard | No DDL credential; freeze; delayed drop |
| INC-15 Kiro-style over-scoped action | Contained | L2 plus envelope bounds |
| INC-19 DataTalks (stale Terraform state, `terraform destroy`) | Out of scope | Infrastructure tooling outside Postgres |
| INC-20 PocketOS (provider token deletes volume and backups) | Out of scope for prevention; detected in part (AP-11 backup posture) | Provider credentials in the agent workspace aren't Postgres |
| ORM reset through the app's own connection string | Detected (AP-13/16) when the login is shared; otherwise out of scope | Guard governs only Guard-issued credentials |

**Targets (proposals):**
- RO corpus 100% refused under the Guard configuration;
- every "prevented" row prevented;
- benign false-block ≤ 2%;
- approvals ≤ 5 per 100 actions, counted *including* the ramp;
- kill ≤ 10 s.

Reports are published per release, with failures, signed like the existing bench reports.

---

## 12. Decommissioning AgentDB safely

pg_sage must never forget a billed resource it created.

1. **Drain** on the last pre-G0 version, as documented: let in-flight rows settle (creates and
   destroys) before upgrading.
2. **Inventory (G0).**
   - On startup, if `sage.agent_db_deployments` exists, pg_sage selects **by evidence of any live
     call** (G0-01 lists the evidence), not by status.
   - It adds RDS final snapshots (by deterministic name), `local_postgres` databases and schemas,
     and `sage` schemas inside fleet-attached agent databases.
   - Each item is logged with provider, resource id, deterministic name, region, account or project
     ("unknown" where never recorded), created_at and a per-provider delete template:
     - Cloud SQL lifts deletion protection first;
     - RDS asks the operator to choose a final snapshot;
     - Neon has separate project and branch commands.
   - `GET /api/v1/agentdb/decommission` returns everything as JSON, without secret values.
   - pg_sage destroys nothing.
3. **Credentials step.** The inventory lists the provider tokens, the Supabase master secret,
   `PG_SAGE_AGENTDB_*` DSNs and the IAM grants from the runbooks. Startup warns while any of these
   environment variables remain set.
4. **Acknowledgement.** YAML `agentdb_decommission: {acknowledged_resources: [...], exported:
   true}`, or `POST /api/v1/agentdb/decommission/ack`. It is recorded in
   `sage.agentdb_decommission` with the actor.
5. **Drop (G1).**
   - The 27 tables are dropped by an explicit list when the acknowledgement exists, or when the
     **operational** tables are empty: deployments, requests, receipts, authorizations, audit.
   - Seeded size profiles and the version row don't count, because `Ensure` creates them on every
     install.
   - Otherwise the drop step is refused and listed; everything else starts.
6. **Fleet-synced agent databases** stay monitored only if they are re-registered as fleet
   databases. The inventory includes their `env:` references.

**Removal manifest (G0):**
- **Code:**
  - `internal/agentdb/`;
  - `internal/api/agent_db_*` and the agent-db ping and agent-API prefix exemptions in
    `internal/api/auth_middleware.go:113-117`;
  - `cmd/pg_sage_sidecar/agentdb_*.go`;
  - `internal/retention/agent_rules.go` and the AgentDB entries in
    `internal/retention/exemptions.go`.
- **Config:**
  - `internal/config/config.go:144-160`;
  - the AgentDB parts of `internal/config/clone.go:23-35`;
  - `internal/config/key_classes.txt:112-116`;
  - AgentDB keys in `internal/store/config_helpers.go` and `internal/api/config_apply.go`.
- **UI:** `web/src/pages/agentdb/` (20 files), `web/src/pages/AgentDBsPage.jsx` and its two
  tests, the `App.jsx` and `Layout.jsx` entries, the Playwright specs, the committed UI bundle,
  and the router golden file.
- **Docs:**
  - the generated config docs;
  - `docs/agent-db-deployments.md`, `docs/runbooks/agentdb-*`, `docs/neon-supabase.md` (AgentDB
    parts) and `docs/reports/2026-05-*agentdb*`, moved to `reviews/archive/agentdb-2026-05/` with
    credentials scrubbed;
  - the README's AgentDB and Lakebase "provisioning" claims.

---

## 13. Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | pg_sage becomes a high-value target: role administration, broker credentials, pre-images | E1 before G1 actions. Broker logins per agent; there is no shared broker. Credentials encrypted with associated data. Pre-images stay in their own database. pg_sage's role has no `BYPASSRLS` (AP-14). The kill switch has a fallback and a manual runbook |
| R2 | Agents use credentials pg_sage doesn't manage | AP-01/13/16 become warnings once principals exist. "Only Guard-issued credentials" is the documented deployment rule. The bench scores this out of scope, honestly |
| R3 | Managed-service limits | G1-13 receipts per platform. Each missing primitive degrades to deny or posture-only, with one log line |
| R4 | Approval fatigue | L3 envelopes; the fast ramp; shadow scoring; template pooling (human-signed); a target that counts the ramp |
| R5 | Posture fixes break apps | Capped at L1 |
| R6 | Scope creep into proxy, hosting or schema review | §5.5 non-goals, §1.4 hand-offs |
| R7 | Building for nobody again | G0 is the launch; the pull gate (§10.6); you decide past it |
| R8 | The parser lags PG18 | PG18-only syntax is denied; track pg_query_go (O-3) |
| R9 | Undo gives false comfort | Accepted shapes, cascade and trigger rules, transaction bounds; conflicts surfaced; DDL undo only by rehearsed `down` or the trash window |
| R10 | Broker pools add connections | `broker.max_total_connections`; idle timeouts; a reserved budget documented next to `reserved_connections` |
| R11 | Rename confusion | One CHANGELOG entry; 404s with a doc link |

---

## 14. Decisions for you

| # | Decision | Recommendation |
|---|---|---|
| D-1 | **Delete AgentDB provisioning** (instances, Terraform, blueprints, project modes), keeping the narrowed broker as governed sandboxes and clones. The alternative, the research's: keep and harden the Neon and Lakebase branch runners and the reconciler as a provisioning product; freeze RDS | Delete. No user in five months; the hosts own the job; the narrowed broker survives as sandboxes |
| D-2 | Pull-gate thresholds before G1 (§10.6) | A named design partner, or ≥ 3 external threads, or ≥ 50 stars in 6 weeks |
| D-3 | A design partner for production writes (G3) and the estate (G4) | Seek one during G0 and G1 |
| D-4 | Multi-team installs (D4a, never asked). If yes, move E3's per-database human RBAC before G1 | — |
| D-5 | `single_operator_mode` for lifeos | On, with its review queue |
| D-6 | The name "Agent Guard" (check trademarks before launch copy) | Yes |
| D-7 | Hosted pg_sage in scope? It decides whether the brokered lane is customer-VPC only | Not before G2 |
| D-8 | AGPL versus embedding by agent platforms (competitive §4.5) | Keep AGPL for now |

**Open technical items:**
- **O-1:** managed-service privileges for role settings, event triggers and `pg_control_system()`
  (closed by G1-13).
- **O-2:** pgx support for OAUTHBEARER on PG18 `oauth`.
- **O-3:** the PG18 grammar in pg_query_go.
- **O-4:** Neon, Supabase and Lakebase usage and state field names (a G4 live run).

---

## Appendix A: traceability

### A.1 Incident-derived requirements (incidents-security §4.2)

| Requirement | Coverage |
|---|---|
| SAFE-ID-01 distinct identity | G1-01, G1-07 |
| SAFE-ID-02 no inherited authority | G1-02 |
| SAFE-ID-03 brokered short-lived credentials | Broker logins only pg_sage holds (§6.4); direct lane by IAM auth or a `VALID UNTIL`-bounded operator credential (§5.1); G3-08 |
| SAFE-ID-04 audience-bound tokens | G1-03 |
| SAFE-ID-05 tenant binding in data layer | G2-16 |
| SAFE-ID-06 no destructive provider credentials for agents | §5.5 (agent runtime out of scope); pg_sage's own reclaim scopes are separate (§6.14); bench marks INC-20 out of scope |
| SAFE-PERM-01 read-only by privileges | G1-01, G1-15 |
| SAFE-PERM-02 approval bound to the statement | G2-01 (full envelope) |
| SAFE-PERM-03 blast-radius bounds | G2-02, G2-03b |
| SAFE-PERM-04 registry labels | G1-04, G1-04b |
| SAFE-PERM-05 freeze as gate state | G2-06 |
| SAFE-PERM-06 default-deny RLS and isolation tests | AP-03/04/06 (G0-03), AP-17 (G2-17) |
| SAFE-PERM-07 secrets unreadable | G2-07 |
| SAFE-PERM-08 limits re-applied | §6.6, §6.8 `SET LOCAL`; RO-15 in G1-15 |
| SAFE-NET-01 authenticated endpoints | E1, E2 (token-only HTTP MCP today) |
| SAFE-NET-02 no exfiltration channel | G1-15 (RO-06/09/14); AP-09 |
| SAFE-NET-03 egress allowlist for agent runtimes | **Out of scope:** pg_sage doesn't host the agent runtime (§5.5) |
| SAFE-NET-04 production only through the gateway | **Out of scope** for enforcement (network design is the customer's); AP-16 detects unmanaged logins |
| SAFE-TOOL-01 typed write actions | §6.3; free SQL is read-only |
| SAFE-TOOL-02 one statement, no session control | §6.8 S1/S2; G1-15 |
| SAFE-TOOL-03 canonical identity | §6.9 envelope over OIDs and `database_id`; G2-01 |
| SAFE-TOOL-04 taint | G2-05 (principal-wide) |
| SAFE-TOOL-05 fail closed on preconditions | G3-03 |
| SAFE-TOOL-06 no destructive provider tools for agents | §8.2 (none exist); a catalog lint test in G1 |
| SAFE-TOOL-07 tool metadata integrity | §6.8 pre-parse rejection; `catalog_sha256` (§6.17); G2-13 |
| SAFE-TOOL-08 agents can't change their rules | G1-14 |
| SAFE-HUM-01 out-of-band approvals | G2-10 |
| SAFE-HUM-02 approval shows exactly what runs | §6.9 card; G2-01 |
| SAFE-HUM-03 separation of duties | G2-09 |
| SAFE-HUM-04 server-side kill switch | G1-05, G1-06 |
| SAFE-HUM-05 approval hygiene and rate limits | G2-09; §6.11 limits |
| SAFE-BAK-01 backups outside the blast radius | AP-11 (best effort); docs |
| SAFE-BAK-02 restore point per action | §6.9; `guard_restore_points` |
| SAFE-BAK-03 system reports recoverability | §6.12 drills; the card shows drill and PITR facts from metadata |
| SAFE-BAK-04 drills gate autonomy | G3-06 |
| SAFE-BAK-05 delayed drops | G3-05 |
| SAFE-BR-01 branches and deploy requests | G2 sandboxes; G3-01 |
| SAFE-BR-02 masked branches | G2-14 |
| SAFE-BR-03 sandboxed agent runtime | **Out of scope:** the agent runtime isn't pg_sage's |
| SAFE-BR-04 private by default | G2-14 |
| SAFE-MON-01 attribution in an immutable log | G1-10, G2-13, E2 hash chain |
| SAFE-MON-02 independent verification | §6.9 `verify_sql` in a new transaction; §6.13 step 7 |
| SAFE-MON-03 anomaly detection | G2-08 |
| SAFE-MON-04 canaries | G2-08 |
| SAFE-SUP-01 pin vulnerable agent tooling | AP-10 (extensions); MCP server pinning **out of scope** |
| SAFE-SUP-02 signed releases | E4 (before G3) |
| SAFE-VER-01 evidence preconditions | Existing Sage SRE runbook preconditions apply to `maint_on_behalf` (G3-10) |
| SAFE-VER-02 competing hypotheses | Sage SRE investigator (existing); **out of Guard scope** |

### A.2 Enterprise requirements (enterprise-needs §1)

| Id | Coverage |
|---|---|
| ID-1 one identity per agent (MUST) | §6.4, G1-01 |
| ID-2 accountable sponsor (MUST) | §6.4, §6.11 (ownerless → L0) |
| ID-3 delegation chain (MUST) | `on_behalf_of` (E2), G2-13 |
| ID-4 short-lived credentials (MUST) | Broker rotation; IAM auth for the direct lane; G3-08 |
| ID-5 IdP federation (MUST regulated) | E2 |
| ID-6 workload attestation (SHOULD) | Not covered (SPIFFE later) |
| ID-7 inventory and discovery (MUST) | Agents page, `agent_whoami`, estate |
| ID-8 complete offboarding (MUST) | `guard_role_retire`; SCIM (E3) |
| AZ-1 deny by default, read-only start (MUST) | Default `prod`; L2 grants in G1 |
| AZ-2 least agency (MUST) | Capability classes; envelopes |
| AZ-3 JIT elevation (MUST) | Leased grants (§6.6) |
| AZ-4 enforcement outside the model (MUST) | Principles 1–2 |
| AZ-5 context-aware authorization (SHOULD) | Environment, taint, time windows |
| AZ-6 no privilege amplification (MUST when on-behalf) | Broker logins; `on_behalf_of`; RLS applies |
| PC-1 policies as code (MUST) | E2 declarative file |
| PC-2 standard languages (SHOULD) | Not covered (OPA export later) |
| PC-3 static analysis before activation (SHOULD) | Profile validation at plan time (E2) |
| AP-1 full change record (MUST) | Evidence packs (E2) |
| AP-2 separation of duties (MUST) | §6.11, G2-09 |
| AP-3 risk-tiered paths (MUST) | Levels by class and environment; break-glass (E1) |
| AP-4 system-of-record integration (MUST regulated) | ITSM webhooks (E3) |
| AP-5 GitOps path (SHOULD) | Hand-off (§6.13); declarative file (E2) |
| AP-6 approval bound to artifact (MUST) | G2-01 |
| AP-7 two-person for destructive (SHOULD) | §6.11 |
| AU-1 tamper-evident trail (MUST) | E2 hash chain |
| AU-2 database-side corroboration (MUST) | pgaudit correlation (E2); AP-11 |
| AU-3 SIEM export (MUST) | E2 |
| AU-4 evidence packs (MUST) | E2 |
| AU-5 retention (SHOULD) | §6.17 |
| AU-6 labelled agent sessions (SHOULD) | `application_name` on broker connections |
| DP-1 classification-aware policy (MUST) | §6.7 |
| DP-2 masking and minimization (MUST) | Column privileges; masked sandboxes |
| DP-3 governed LLM data flows (MUST) | §6.16 data-flow rule |
| DP-4 DSPM signals (SHOULD) | Query audit export (E2) |
| DP-5 database content untrusted (MUST) | Fencing; taint |
| SF-1 environment separation (MUST) | §5.3, §6.5 |
| SF-2 blast-radius limits (MUST) | §6.9 |
| SF-3 kill switch (MUST) | §6.10 |
| SF-4 break-glass (MUST) | E1 |
| SF-5 reversibility and recovery (MUST) | Undo, drills, trash |
| SF-6 verification with rollback (SHOULD) | `verify_sql`, change-path verification |
| SF-7 progressive fleet rollout (SHOULD) | Existing `internal/rollout` (fixes R1, R3 and R4 needed); not in G0–G4 |
| FO-1 cost attribution (MUST) | G4 usage attribution: provider-reported usage and cost, attributed and exported, no pricing (GR-10) |
| FO-2 budgets and quotas (MUST) | G4 budgets in provider units; D9 rate limits |
| FO-3 TTL and teardown (SHOULD) | Sandbox and estate lifecycle |
| FO-4 unit economics (COULD) | Not covered |
| DR-1 self-hosted (MUST) | Yes |
| DR-2 residency per region (MUST) | Self-hosting; substrate region config |
| DR-3 tenant isolation (MUST) | `tenant` on principals; G2-16 |
| DR-4 delegated administration (SHOULD) | E3 RBAC |
| CM-1 published control mappings (MUST) | Evidence-pack docs (E2), stating that the standard-change mapping is proposed, not certified |
| CM-2 AI system documentation (SHOULD) | `docs/agent-guard.md`; LLM data-flow section |

## Appendix B: disposition of the self-review (`spec-review.md`)

| SR | P | Resolution |
|---|---|---|
| 01 | P0 | §6.2.2 normative order; `OperatorApproved` rules; Guard follows `trust.level` |
| 02 | P0 | §6.2.4 `Narrowing`; D10 lease backstop; G1-08b |
| 03 | P0 | §6.2.6; CG-08; G1-12 |
| 04 | P1 | §6.2.1 `AgentDecider` injected |
| 05 | P1 | §6.2.3 mapping; `sage.decision` columns; `guard_query_audit` |
| 06 | P1 | §6.3 contract table; rollback-class caps in A5 |
| 07 | P1 | §6.2.5 closed list |
| 08 | P1 | §6.2.7 `FOR SHARE`/`FOR UPDATE` |
| 09 | P0 | §6.5 identity tuple, two-label rule, receipts; G1-04b |
| 10 | P1 | §6.6 cluster roles |
| 11 | P1 | §6.6 PUBLIC preflight; AP-15; G1-02, G1-16 |
| 12 | P1 | §6.6 `GRANT OPTION` only; `GRANTED BY`; drift; G1-09 |
| 13 | P2 | §6.6 PG16+ only; `createrole_self_grant` |
| 14 | P1 | Direct lane only with E3; IAM auth or operator-managed credential, none held by pg_sage; no reveal; coding agents never (§5.1, §6.4, G3-08) |
| 15 | P1 | `mcp.stdio_principal` (§6.4) |
| 16 | P1 | Slug rule, `databases` bound, unsponsored behaviour, endpoint change (§6.4; G1-11) |
| 17 | P1 | Kill steps with in-flight tracking, configured replicas, timeout bounds elsewhere, lock timeouts, fallback, scopes, rotation, runbook (§6.10; G1-05/06, G3-11) |
| 18 | P0 | Broker logins (§5.1, §6.6, §6.8); whole-statement proof; `set_config` denied; verify in a new transaction (§6.9) |
| 19 | P0 | Cascade, trigger, rule and inheritance rules; transaction tuple bound (§6.9; G2-03b, G2-04b) |
| 20 | P1 | `sage_guard.capture` definer function with a one-time token, in its own schema (§6.9) |
| 21 | P1 | Post-image key; generated and identity columns; byte cap; no-PK; undo role (§6.9) |
| 22 | P1 | `maint_on_behalf` (§6.3; G3-10) |
| 23 | P1 | Owner `SET ROLE` with an explicit membership (§6.6, §6.13) |
| 24 | P1 | Guard reuse rules: RLS allowed, `prosecdef` denied, depth 4, normative deny-list (§6.8) |
| 25 | P1 | Column privileges as the control; OID-0 rule; view propagation; §5.1 reworded (§6.7, §6.8) |
| 26 | P1 | Facts schema, `(relid, attnum)` keys, column grants, re-classify before visibility (§6.7, §7) |
| 27 | P0 | Full envelope hash plus queue id (§6.9; G2-01) |
| 28 | P1 | Principal-wide taint with no time expiry; per-task only from trusted claims (§6.10; G2-05) |
| 29 | P2 | Cursor fetch; `pg_temp` last; catalog allowlist; fingerprint check; positive shapes; `USAGE` refcount; proof cache |
| 30 | P1 | SQLSTATE plus fixed messages; cast case in G1-15 (§6.8) |
| 31 | P1 | Two-person widening; DBA → operator; single-operator mode; departed sponsors (§6.11) |
| 32 | P1 | UI-only approvals; chat notifies (§6.11; G2-10) |
| 33 | P1 | `sage.action_approvals`; TTL on agent items (§6.11, §7) |
| 34 | P1 | Separate guard ledger tables plus a shared `Subject` refactor, estimated (§6.11, §7) |
| 35 | P1 | Fast ramp, pooling (human-signed), counted target, `prod` `ddl_additive` at L2, full starting table (§5.4, §6.11, §11) |
| 36 | P2 | Break-glass (§6.10, E1); emergency path = break-glass plus retrospective review |
| 37 | P0 | `database_id` keys; per-database registries; pre-images in the target, same transaction (§6.5, §6.9, §7) |
| 38 | P1 | `mode: meta` or a pinned control database (§6.5) |
| 39 | P1 | Provenance and audit columns in G1 (§7) |
| 40 | P1 | `proposed_via_v2`, added once; `VALIDATE` separate; after `ddlAsk` (§7) |
| 41 | P1 | `guard_` prefix; explicit 27-name drop (§7, §12) |
| 42 | P1 | `encryption_key` required; associated data; key ids (§6.4, §7) |
| 43 | P2 | `deployment_id` as the install tag; `sre_database_bindings` (§6.5, §6.12) |
| 44 | P2 | Retention ≥ 180 days; hash chain; erasure (§6.17) |
| 45 | P1 | Restore point vs drill; weekly point-in-time drills on copy-on-write substrates; quiet tables; pinned settings; consistent D7 (§6.2.2, §6.12) |
| 46 | P1 | Masked parents; credentials after scan; history unreachable (§6.12; G2-14) |
| 47 | P1 | Sandboxes as fleet databases; `dle`/`snapshot` kept; per-database substrate (§6.12) |
| 48 | P1 | Provider-reported usage only; L0 start; LLM fencing; `verify-full`; G4-04 against the provider API (§6.14) |
| 49 | P2 | Trash naming, map, revokes, publications, dependents, `TRUNCATE` (§6.13) |
| 50 | P2 | Groups, auto-down only for additive, unrehearsed at L1, `promote_expand`, B1 evidence (§6.13) |
| 51 | P0 | Evidence-based inventory, extra artifacts, templates, credentials step, drain (§12; G0-01) |
| 52 | P1 | Drop on operational tables only (§12) |
| 53 | P1 | Removal manifest; grep regex; middleware; overrides; warn and ignore (§12; G0-02, G0-07) |
| 54 | P0 | G1 self-contained: gate, `Decide`, classification, L2 grants (§10.2) |
| 55 | P1 | G0 = launch; pull metric; re-estimates (§10.1, §10.6, §11) |
| 56 | P1 | §1.6 departures; D-1; short-TTL clone exception; G3 hand-off |
| 57 | P1 | Reordered G2/G3; lifeos read-only through G2 (§10.6) |
| 58 | P1 | §6.16 LLM features and the data-flow rule |
| 59 | P1 | Per-incident expectations (§11); AP-16 |
| 60 | P1 | §8.1 error model; full shapes; pagination |
| 61 | P1 | §9 complete keys, classes, ceiling precedence, approvals block |
| 62 | P2 | AP-10 thresholds, AP-06 on PG14, anchored hints, AP-13 privilege, cadence, G0-03 coverage |
| 63 | P2 | §0 and §1 claims restated per the research; named rivals; standard change hedged |
| 64 | P2 | Corrected: AU-07 ids, sources, Neon and Lakebase TTL origins, "45 products" removed, D4b noted, `template` timing, memory in §1.2, DP-24 receipt |
| 65 | P2 | G1-13; PR #130 fallback; v2.3 lease reused; versions at cut |
| 66 | P2 | SQL-side expiries; `QueryExecModeExec`; replica option; read-only hint; dealloc reporting. **Deferred:** sqlcommenter tags (attribution is by role, which is stronger) |
| 67 | P2 | E track reordered; pgaudit correlation; signing before G3; declarative file |
| 68 | P1 | Appendix A complete with out-of-scope reasons; new checks G1-14, G2-16, G2-17; extended G1-01 |

## Appendix C: evidence

- **Code audits:** `research/code-audit-agentdb.md` (A-xx) and `research/code-audit-deploy-paths.md`
  (DP-xx).
- **History:** `research/prior-specs.md` and `research/session-history.md`.
- **Market:** `research/competitive-analysis.md` (gap matrix §3; win, lose, integrate §4).
- **Incidents and pain:** `research/incidents-security.md` (INC-*, SAFE-*, RO corpus) and
  `research/community-pain.md`.
- **Substrate and enterprise:** `research/technical-substrate.md` (§13 map) and
  `research/enterprise-needs.md` (catalog, ladder, integrations).
- **Self-review:** `spec-review.md`.
- **External review:** `gemini-review.md` (the raw review, how it was run, and a disposition per
  finding).

## Appendix D: disposition of the external review (`gemini-review.md`)

Each finding was checked against the code and PostgreSQL behaviour before it was accepted.

| GR | Gemini said (severity) | Verdict | Where |
|---|---|---|---|
| 01 | The kill switch can't reach replicas from `pg_stat_replication` (P0) | Accepted | §6.10 step 6; `databases[].replicas`; `idle_session_timeout`; G1-05, G3-11 |
| 02 | Daily point-in-time drills are costly and flap (P0) | Partly accepted: drills kept (SAFE-BAK-04), made weekly, copy-on-write only, timing-safe | §6.12; `clone.drill_interval_days`; G3-06 |
| 03 | `EXPLAIN` fails on `$n` placeholders before PG16 (P1) | Valid; existing generic-plan code handles it | §6.13 step 4 |
| 04 | `DROP CONSTRAINT` and `DROP INDEX` unclassified (P1) | Accepted | §5.2, §6.13 steps 2–3; G3-12 |
| 05 | Taint can be waited out (P0) | Accepted, modified: no expiry; clears on context reset, operator or rotation | §6.10; `agents.taint.clear_on`; §7; G2-05 |
| 06 | Injection risk in the capture function (P0) | Accepted | §6.9 query construction; G2-18 |
| 07 | Direct-lane credentials can leak through a compromised runtime (P1) | Accepted | §5.1, §6.4 |
| 08 | NULL column lists re-expose `secret` columns (P1) | Accepted | §6.7, §7 |
| 09 | `agent_level0` could hide a D-step reason (P2) | Accepted | §6.2.3 |
| 10 | G4 chargeback is a FinOps engine (P0, scope) | Accepted, modified: provider-reported usage only, no pricing | §6.14, §7, §9; G4-04 |
| 11 | Secrets-manager integrations are a distraction (P1, scope) | Accepted, modified: IAM auth or operator-managed credentials; `*_FILE` for pg_sage's own | §6.4, §10.7 |
| 12 | Keep the LLM to classification only (P2, scope) | Rejected in part (product principle); LLM output made asynchronous and never in the decision path | §6.16 |
