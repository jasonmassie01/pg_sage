# Spec review: Agent Guard (`AGENTDB-SPEC.md`, draft 1)

**Reviewer:** fresh-context staff-engineer review, 2026-10-05.
**Baseline:** the worktree at `72646ab1`.
**Method:**
- Code claims were checked in `sidecar/` at that commit with `go list`, `git grep`, `git log` and file reads.
- Research claims were cross-checked line by line against the nine research files.
- Anything I could not check is marked UNVERIFIED.
- No database or cloud API was touched.

**Scope:** the repo's `/write-spec` self-review checklist (ambiguity, edge cases, contradictions, interfaces, dependencies, version behaviour, implementation gaps), plus feasibility against the code, security of the design, and product strategy.

**Citations used below:**
- Code paths are relative to `sidecar/internal/`, except `cmd/…`, `web/…` and `go.mod` (relative to `sidecar/`), `docs/…`, `.gitignore` and `reviews/…` (repo root), and `research/…` (this review's folder).
- "Spec L*n*" is a line of `AGENTDB-SPEC.md`.
- "Cross-check" means one of the research verification passes, which read every research file in full.

---

## 1. Verdict

**Not ready to build past G0.**

The direction holds up: stop provisioning, keep AgentDB's careful ideas, and govern agent access in estates pg_sage doesn't host. The spec is specific in many places. But it is not yet specific *where safety is decided*, and several mechanisms don't fit the code they claim to reuse.

**G0 can start after two fixes:**
- **SR-51.** The decommission inventory would miss live, billed resources.
- **SR-53.** The removal manifest is incomplete, and G0-02 cannot pass as written.

**G1 and G2 cannot start yet:**
- **The gate integration.** "The gate delegates to `agentguard.Decide` before pg_sage's own trust logic" is an import cycle as written (SR-04). It also leaves undefined which existing gate stages bind agent requests (SR-01). Both natural readings are unsafe:
  - In one, agents bypass the emergency stop and the operator's trust ceiling.
  - In the other, on a default install (`trust.level: observation`) no Guard action executes at all, even with a human's approval, and expired grants are never revoked (SR-02).
- **G1 depends on G2.** G1's scope needs G2's gate branch, ledger and classification (SR-54).

**Five design holes would ship as security bugs if built literally:**
- The environment binding cannot tell production from its own clone or standby (SR-09).
- The brokered lane runs agent SQL on pg_sage's own privileged login, against the spec's own research. A `set_config('role', …)` in a write's SET list switches back to pg_sage's privileges mid-transaction (SR-18).
- FK cascades and triggers escape row bounds and undo (SR-19).
- Approvals don't bind the bounds they approve (SR-27).
- Existing MCP tools already let agents act under pg_sage's own earned trust (SR-03).

**Strategy:**
- "Govern, don't host" is reasonable, and the research supports it.
- But the spec overrides its own research in several places without saying so (SR-56).
- It names distribution as the binding constraint and then plans none (SR-55).
- It ships brokered production writes before sandboxes, against the research's order and a recorded user rule about lifeos (SR-57).
- It defines no LLM feature, despite the decided product principle (SR-58).

**Counts:** 68 findings. **P0: 10. P1: 45. P2: 13.**

**Must change before G1 starts, in order:**
1. Write the normative evaluation order for agent requests, and the exemption for narrowing actions (SR-01, SR-02, SR-04, SR-05, SR-06).
2. Bring the existing propose-scope MCP tools and the stdio client under principals (SR-03, SR-15).
3. Redesign the environment binding (SR-09) and the role model for cluster-wide roles and PUBLIC (SR-10, SR-11, SR-12). Fix the credential fallback (SR-14), the kill switch (SR-17), the token migration (SR-16) and G1-01 (SR-68).
4. Make G1 self-contained: minimal `Decide`, `column_class`, column-level grants and provenance columns (SR-54, SR-26, SR-39).
5. Fix the data-model keys for fleets (SR-37, SR-38).
6. Add a distribution plan and a measurable pull gate (SR-55).

**Before G2 starts:**
- Fix the brokered write design (SR-18, SR-19, SR-20, SR-21, SR-27).
- Fix taint (SR-28) and separation of duties (SR-31).
- Fix the approval data model (SR-32, SR-33) and the ledger design (SR-34).

---

## 2. Citation check

| Spec citation | Result |
|---|---|
| `mcptoken/mcptoken.go:6,54`: agent tokens can never approve | Verified |
| `internal/agentdb` imports no other pg_sage package | Verified (`go list`) |
| `config.go:1038`: `require_backup_before_destroy` defaults to true | Verified. The file is `internal/config/config.go`; say so |
| `lifecycle_claim.go:43-44`: teardown needs `restore_verified` | Verified (lines 42-43) |
| `api/agent_db_agent_api.go:56-70`: four agent routes | Verified |
| `config/defaults.go:196`: 300 s reconciler | Verified |
| `teardown_reconcile.go:99-130`, `:165` | Verified |
| `docs/neon-supabase.md:118-121` | Verified (119-121). Receipts are git-ignored (`.gitignore:55`, cross-check) |
| `store/config_helpers.go` raw INSERT; `api/config_apply.go:79-94` | The INSERT is verified (`:315`, `:325`). Lines 79-94 are the masked-value *write* guard, not read masking |
| `config/config.go:652-653`: TLS env read; no `ListenAndServeTLS` | Verified |
| `auth/oauth.go`: no PKCE, nonce or `id_token` | Verified |
| `auth/types.go:21-35`: three global roles | Verified |
| `api/mcp_principal.go:20-33`: static tokens only | Verified |
| `earned/level.go:17-32`: L0–L4 | Verified |
| `earned/carryover.go:145-149`: `sre_family_autonomy` | Verified as an INSERT into that table. "Mirrors" it is not (SR-34) |
| `schema/ask_migration.go`: CHECK allows only `'ask_sage'` | Verified (lines 52-62) |
| `sqlast.InspectReadQuery` fails closed without cgo | Verified (`sqlast/readquery_nocgo.go`) |
| pg_query_go v6.2.2, PG17 grammar | Verified (`go.mod:13`) |
| `explain/analyze_guard.go` already does L2–L4; views followed to depth 3; the deny-list | **Partly wrong.** `maxViewDepth = 4`. The guard refuses all RLS tables and foreign tables. Its deny-list differs (SR-24) |
| B1: `migration/runtime/postgres_rehearsal.go:27-63`, `rehearsal/orchestrator.go:117-131` | Verified (cross-check). The verdict constant is `promote_expand`, not `promote` |
| B2 (CHECK lacks `blocked`), B7 (runtime bypasses `Apply`) | Verified (cross-check: `schema/ddl_agent_features.go:45`; `migration/runtime/postgres.go:30-50`) |
| `clone/dle_provider.go` exists | Verified. But `clone.provider` accepts `none`, `dle`, `snapshot` (SR-47) |
| `docs/agent-db-deployments.md:350,358`: committed passwords | Line 358 verified. Line 350 is not a password line in my grep (UNVERIFIED). Line 378 and fixture scripts are missed (cross-check) |
| "the existing 'Grant more' onboarding step, extended" | **Wrong.** That step is about pg_sage's trust level (`onboarding/checklist.go:143-158`), not database privileges |
| First look "its own 5 s once PR #130 merges" | Verified as pending. Branch `claude/firstlook-own-timeout` is not merged into `72646ab1` |
| 27 AgentDB tables | Verified: 26 `agent_db_*` plus `agent_identities` |
| AU-11: `3367ffa2` added and `1683e2e5` deleted per-agent attribution | Verified (cross-check). The deleted code was a 116-line heuristic, so "the observer half" overstates it |
| ~16.7k lines of Go and ~3.5k of UI | Verified (cross-check: 16,702 and 3,523) |
| `OwnerDeclared` path | Verified (`policy/types.go:140-142`) |
| Ask Sage fences data as untrusted | Verified (`ask/run.go:132-136`) |
| Table name `sage.mcp_tokens` | Verified (`schema/mcp_v2_migration.go`) |

Market and incident figures mostly trace to the research. SR-63 and SR-64 list those that don't.

---

## 3. Findings index

| ID | P | Spec section | Finding |
|---|---|---|---|
| SR-01 | P0 | §6.4 | Which existing gate stages bind agent requests is undefined; both readings are unsafe |
| SR-02 | P0 | §6.4, §6.7 | Narrowing actions (revoke at expiry, freeze, quarantine) are blocked by the gate's hard stops and observation mode |
| SR-03 | P0 | §3, §8.1 | Existing MCP tools let agents act under pg_sage's own trust; Guard never covers them |
| SR-04 | P1 | §6.1, §6.4 | `policy` → `agentguard` is an import cycle; delegation must be an injected interface |
| SR-05 | P1 | §6.4 | The decision vocabulary ("deny", "L1: proposal") doesn't exist in `policy` or `sage.decision` |
| SR-06 | P1 | §6.4, §6.8 | Typed actions have no executor contracts; several contradict the core's caps |
| SR-07 | P1 | §3.1, §6.7 | The out-of-gate paths (kill, watchdog, purge) aren't enumerated |
| SR-08 | P1 | §3.4, §6.6 | "Re-check in the same transaction" can't hold across control and target databases |
| SR-09 | P0 | §5.3, §6.3, §6.6 | The environment binding can't tell production from its own clone or standby |
| SR-10 | P1 | §6.3, §7 | Roles are cluster-wide but modeled per database |
| SR-11 | P1 | §1.2, §6.3 | PUBLIC privileges defeat least privilege and cross database boundaries |
| SR-12 | P1 | §6.3 | Grantor semantics: revokes can silently no-op; owner membership gives pg_sage `DROP` |
| SR-13 | P2 | §6.3 | PG16+ membership mechanics; PG14/15 role management after PG14's end of life |
| SR-14 | P1 | §6.2 | The pre-E3 credential fallback recreates the PocketOS pattern |
| SR-15 | P1 | §6.2 | The stdio MCP client has a fixed agent principal with no token |
| SR-16 | P1 | §6.2, G1-11 | The token→principal migration violates the new CHECKs; its effect on existing tools is undefined |
| SR-17 | P1 | §6.7 | The kill switch misses brokered sessions and standbys, and has unhandled failure modes |
| SR-18 | P0 | §5.1, §6.5, §6.6 | The brokered lane runs agent SQL on pg_sage's login; `set_config('role', …)` escapes to pg_sage's privileges |
| SR-19 | P0 | §6.6 | FK cascades, triggers, rules and inheritance escape bounds and undo |
| SR-20 | P1 | §6.6 | Pre-image capture needs privileges the broker must not have |
| SR-21 | P1 | §6.6 | Undo breaks on generated, identity, large, no-PK and partitioned rows |
| SR-22 | P1 | §5.2 | `maint`, CIC and `REINDEX CONCURRENTLY` can't run in the brokered transaction; PG14–16 need ownership |
| SR-23 | P1 | §5.2, §6.9 | Who owns objects an agent's migration creates is undefined |
| SR-24 | P1 | §6.5 | The EXPLAIN guard can't be reused as is (RLS refusals, `SECURITY DEFINER`, depth) |
| SR-25 | P1 | §5.1, §6.5, §6.7 | Output masking is bypassable; "masked direct lane" contradicts a non-goal |
| SR-26 | P1 | §6.7, §7 | `column_class` needs facts schema changes, and fails open on rename and new columns |
| SR-27 | P0 | §6.6 | The approval hash omits `max_rows`, `verify_sql`, environment, principal and policy |
| SR-28 | P1 | §6.7 | Taint is laundered by choosing a new `task_id` |
| SR-29 | P2 | §6.5, §6.6 | Statement mechanics: row cap, `pg_temp`, deparse fidelity, sequences and schema `USAGE` |
| SR-30 | P1 | §6.5, §6.6, §8.1 | Error text and casts leak masked and escalated data |
| SR-31 | P1 | §3.3, §6.8 | Separation of duties has holes; single-admin installs can't approve; no "DBA" role exists |
| SR-32 | P1 | §8.1, §8.2 | Approval channel contradiction: UI-only vs chat approval cards |
| SR-33 | P1 | §7 | Two-person approval has no data model; queue TTL defaults to 7 days |
| SR-34 | P1 | §6.8, §7 | The agent ledger is a refactor of `internal/earned`, not a mirror |
| SR-35 | P1 | §6.8 | Promotion defaults contradict the user's fast-trust decision and the fatigue evidence |
| SR-36 | P2 | §6.7 | No break-glass and no emergency change path |
| SR-37 | P0 | §7 | Install-wide tables keyed by per-database action ids; cross-database write order undefined |
| SR-38 | P1 | §7 | Without a meta DB, the control database can move between restarts |
| SR-39 | P1 | §7, G2-13 | Columns the checks need don't exist (`action_log`, `decision`, provenance in G1) |
| SR-40 | P1 | §7 | The `proposed_via` migration re-runs and re-locks on every boot |
| SR-41 | P1 | §7, §12 | `sage.agent_db_roles` reuses the prefix of the tables G1 drops |
| SR-42 | P1 | §6.2, §6.6 | Encryption depends on the optional `encryption_key` |
| SR-43 | P2 | §6.10, §7 | `pg_sage_install_id` and the environment registry duplicate existing identities |
| SR-44 | P2 | §7 | Retention and tamper evidence fall short of the enterprise research |
| SR-45 | P1 | §6.10, §6.4 | Restore points and drills don't work as designed; prod-write rules contradict |
| SR-46 | P1 | §6.10 | Masking sandboxes after copying leaks history and costs |
| SR-47 | P1 | §6.10, §9 | The sandbox interface is incomplete and breaks existing `clone.provider` values |
| SR-48 | P1 | §6.11 | G4's observe tier and chargeback fail on scale-to-zero, and repeat AgentDB's mistakes |
| SR-49 | P2 | §6.9 | Delayed drops aren't equivalent to drops |
| SR-50 | P2 | §6.9 | Change-path failure semantics and the B1 fix are undefined |
| SR-51 | P0 | §10.1, §12 | The G0 decommission inventory misses billed resources |
| SR-52 | P1 | §12 | The table-drop condition can never be true |
| SR-53 | P1 | §6.1, §9, G0-02 | The removal list is incomplete, G0-02 can't pass, and `agentdb:` rejection breaks startup |
| SR-54 | P0 | §10.2 | G1 can't be built as scoped |
| SR-55 | P1 | §0, §10.6, §11 | Distribution is the named constraint, but none is planned and the gates don't test pull |
| SR-56 | P1 | §1.4, §2.5, §3.8 | Unacknowledged departures from the research |
| SR-57 | P1 | §10 | Release order inverts the research ranking and conflicts with the lifeos rule |
| SR-58 | P1 | §3.9 | No LLM feature, despite the product principle |
| SR-59 | P1 | §11, §13 | The bench promises outcomes Guard can't produce |
| SR-60 | P1 | §8 | MCP and REST interfaces lack an error model and complete shapes |
| SR-61 | P1 | §9 | Configuration keys used but undefined, misnamed or unclassified |
| SR-62 | P2 | §6.12 | Posture detector thresholds, version gaps and cost |
| SR-63 | P2 | §0, §1 | Market and compliance claims overreach the research |
| SR-64 | P2 | passim | Citation and naming errors |
| SR-65 | P2 | §10, §14 | Dependencies treated as settled (PR #130, v2.3, leader election) |
| SR-66 | P2 | §5.1, §6.3 | Clocks, poolers, attribution limits, replicas and other cheap wins |
| SR-67 | P2 | §10.7, §4 | The enterprise track's order differs from the research's priorities |
| SR-68 | P1 | App. A, §10 | Traceability gaps: dropped SAFE ids, detection standing in for prevention, weak G1-01 |

---

## 4. Findings

### A. Gate and executor integration

#### SR-01 · P0 · §6.4 — Which gate stages bind agent requests is undefined; both readings are unsafe

**Problem.** Spec L565-566 says `Gate.Authorize` delegates to `agentguard.Decide` "before pg_sage's own trust logic", and that "the first match decides". The real evaluation order (`policy/gate.go:45-77`) is:
1. runtime;
2. `hardStop`: executor disabled, emergency stop, mutation on a replica;
3. `validateRequest`: typed contract, guardrails, `ValidateSQL`;
4. provider support;
5. binding facts;
6. the operator-approved path;
7. observe-only, when `trust.level=observation` or `execution_mode=manual`;
8. the trust level;
9. the policy document: allowed change classes, approval-required classes, budgets, windows;
10. tier and ramp;
11. the earned ledger (`restrictAutonomy`);
12. the verification wait.

The spec doesn't say which of these still bind a request that carries a principal. Two engineers will build:
- **Reading A, `Decide` replaces the evaluation.** Agent requests skip the emergency stop, the replica check, SQL validation, binding facts, the policy document's classes and budgets, and the operator's trust ceiling. That breaks the ledger's own rule that "the operator's own trust settings stay the outer bound" (`earned/level.go:1-7`).
- **Reading B, `Decide` is a prefix, then the existing evaluation runs.** The default `trust.level` is `observation` (`config/defaults.go:47`). Under it, every agent request ends `observe_only`, *approved or not*: operator-approved requests also stop at observation (`policy/gate_operator.go:12-14`). On a default install, no Guard action ever executes, including G1's grants. Above observation, the policy document blocks every agent action with `change_class_not_allowed`, because no agent change class exists (`policy/document.go:19-32`), and `validateClasses` rejects unknown ones.

**Evidence.** `policy/gate.go:45-77, 181-192, 242-245`; `policy/gate_operator.go:9-35`; `policy/document.go:19-32, 237-263`; `config/defaults.go:47`.

**Fix.** Add a normative "agent request evaluation" table. Proposed order:
1. `hardStop`, except narrowing actions (SR-02).
2. `validateRequest`: typed contract, then `ValidateSQL` on the canonical statement.
3. Provider support, then binding facts. Facts only narrow, so they always apply.
4. `Decide` steps 1–9.
5. Level = min(agent ledger level, operator ceiling). Decide explicitly between two options:
   - **Guard follows pg_sage's trust level.** Map `observation` → L1 (proposals only, as for pg_sage itself), `advisory` or `manual` → L2, `autonomous` → L3. Say in onboarding and in `agent_whoami` that Guard needs `advisory` or higher to act at all.
   - **Guard gets its own ceiling key** (`agents.max_level`). Then say why agents may act where pg_sage itself may not.
6. The policy document, with new change classes: `agent_access`, `agent_data_write`, `agent_schema_change`, `agent_sandbox`, `agent_estate` (policy schema version 4). A document without them means approval-only, not blocked.
7. The verification wait and change leases, unchanged.

Also state that an `OperatorApproved` agent request still passes `Decide` steps 1–6: kill, sponsor, ceiling, capability, classification and freeze.

#### SR-02 · P0 · §6.4, §6.7, G1-08 — Narrowing actions can't run through the gate as it is

**Problem.**
- The spec routes `agent_revoke` ("always allowed (narrows)"), lease-expiry revokes, `agent_freeze` and `estate_quarantine` through `Executor.Apply`.
- `Apply` authorizes twice through the gate (`executor/apply.go:93-127`).
- `hardStop` blocks every request while the executor is disabled or the emergency stop is on. That includes `Rollback` requests: "the rest of the gate still binds" (`policy/types.go:153-157`).
- Under the default `observation` level, the verdict is `observe_only`.

So an emergency stop, or a default install, leaves expired grants in place. That is the failure the spec criticizes in AgentDB (DP-05: "the kill switch also blocks cleanup", spec L304). Broker-role grants have no database-side expiry at all; only login roles get `VALID UNTIL`.

**Evidence.** `policy/gate.go:181-192`; `policy/types.go:153-157`; `executor/apply.go:93-127`; spec L590-596.

**Fix.**
- Add `Narrowing bool` to `policy.ActionContract`. Set it only for revoke, freeze, the kill steps, quarantine and `NOLOGIN`.
- The gate lets narrowing requests through `hardStop`, observe-only, budgets, windows and the DDL slot, and records them.
- Add a backstop that doesn't depend on the revoke running: every brokered call checks the lease registry and fails closed on an expired lease, even if the database grant still exists.
- Add **G1-08b:** "With `emergency_stop` on and `trust.level: observation`, a lease still expires within one reconciler tick."

#### SR-03 · P0 · §3.1, §3.5, §8.1 — Existing MCP tools let agents act under pg_sage's own trust

**Problem.**
- Agent tokens, and the stdio client, already reach 15 propose-scope tools (`mcp/scope.go:18-25`): `request_change`, `optimize_query`, `apply_migration`, `ensure_fk_indexes`, `set_maintenance_policy`, `sre_propose_action`, `sre_request_execution`, `sre_draft_runbook`, `sre_compile_runbook`, `sre_evaluate_autonomy`, `propose_fact`, `mark_object`, `report_source_fix`, `specialist_request_remediation` and `propose_policy_change`.
- `policy.ActionRequest` has no field for a caller (`policy/types.go:123-158`), and the intent tools' requests carry none (`mcp/intent_adapters.go:21-29`; `mcp/production_intent_executor.go:205-208, 263-266`). The gate therefore judges them as pg_sage's own self-initiated changes, under pg_sage's trust level and ledger. The SRE tools mostly queue for a human; whether every path does is UNVERIFIED.
- With a migration runtime configured, `apply_migration` executes expand steps after that decision (`:233-235`). It does so through the runtime that bypasses `Executor.Apply` (B7).

The spec never mentions these tools. After G2, freeze, taint, the environment ceiling and per-agent trust won't apply to the agent's most powerful existing tools. "Trust is earned per agent" (principle 5) is false from day one.

**Evidence.** `mcp/scope.go:18-25`; `mcp/intent_adapters.go:21-29`; `mcp/production_intent_executor.go:205-208, 215-235, 263-266`; `mcp/principal.go:115-116`.

**Fix.**
- In G1, set `Principal` on every `policy.ActionRequest` built from an MCP call by an agent principal, and map each tool to a capability class:
  - schema intents of `apply_migration` and `request_change` → `ddl.*`;
  - `optimize_query` and `ensure_fk_indexes` → `ddl.additive`;
  - SRE and runbook executions → `maint`, or a new `sre_action` class.
- Until G3's change path lands, cap agent-originated migrations and intents at L2.
- Add **G1-12:** "A frozen principal's `apply_migration` and `request_change` are denied. A tainted principal's are queued for a human."

#### SR-04 · P1 · §6.1, §6.4 — `Gate.Authorize` can't call `agentguard.Decide` directly

**Problem.**
- `internal/policy` imports no internal package, and `internal/earned` and `internal/facts` import `policy` (`go list` at `72646ab1`).
- `Decide` returns `policy.Decision` and reuses `earned`, so a `policy → agentguard` call is an import cycle.
- §6.1's note, "no import from internal/executor (the gate calls it)", points the dependency the wrong way.

**Evidence.** `go list -f '{{.Imports}}'` for `policy`, `earned`, `facts`; `policy/types.go:260-285`.

**Fix.**
- Define in `policy`:
  - `type PrincipalRef struct { ID, Kind string }`;
  - `type AgentDecider interface { Decide(context.Context, ActionRequest) (Decision, bool) }`.
- Add `GateConfig.Agents AgentDecider` and wire `agentguard` from `cmd/pg_sage_sidecar`. This is the existing pattern for `GateConfig.Autonomy` and `GateConfig.Facts`.

#### SR-05 · P1 · §6.4 — The decision vocabulary doesn't exist in the code

**Problem.**
- The spec's outcomes ("deny: frozen", "L1: proposal only", "L3: execute and notify") don't map to `policy.Verdict`. The verdicts are `execute`, `queue_approval`, `park`, `blocked` and `observe_only` (`policy/types.go:12-18`).
- Nor do they map to `sage.decision`'s verdict CHECK (`schema/ddl_agent_ledger.go:4-29`).
- `sage.decision` has no principal column, though spec L582 says every decision is logged "with the policy version and the principal".
- Every `Authorize` writes a `sage.decision` row. If `agent_query` is gated, that is an insert per read.

**Fix.**
- Add a mapping table:
  - deny → `blocked`, with `Reason` constants `agent_frozen`, `agent_unsponsored`, `agent_env_ceiling`, …;
  - L1 → `observe_only`, plus a recorded proposal;
  - L2 → `queue_approval`;
  - L3 → `execute`, with `ReasonAgentL3`.
- Add `principal_id`, `task_id` and `artifact_hash` to `sage.decision`.
- State that `agent_query` is decided by `Decide` alone, with its own audit row, not by `Gate.Authorize`. Reads shouldn't each write a decision row.

#### SR-06 · P1 · §6.4, §6.8, §6.11 — Typed actions have no executor contracts, and some contradict the core's caps

**Problem.**
- `Executor.Apply` needs a contract per action: risk tier, rollback class, and the post-checks that `Validate` requires (`executor/action_contract.go:5-31`). It also needs a `Feature` (change class), a lease target and slot behaviour (`executor/apply.go:27-68`). The spec gives only Effect, Reversal and Max level.
- The core caps level by rollback class (`policy/autonomy.go:158-179`): anything not `reversible` or `no_rollback_needed` stops at L1, and mitigation-only stops at L2. So these can't reach the spec's levels unless they get a reversible class:
  - `agent_role_retire` (L2, "none after the drop");
  - `sandbox_destroy` (L3);
  - `estate_reclaim` (L3 "by evidence");
  - `agent_undo` (reversal "None").
- Core L3 needs exactly one target object and an open maintenance window (`policy/autonomy.go:181-199`). The agent envelope uses object sets and hours.
- `read` is `RiskReadOnly`, which the ledger never governs (`policy/autonomy.go:77-80`), so "L3 for read" means nothing in the core.

**Fix.**
- Add a contract table with columns: action, `RiskTier`, `RollbackClass`, `Feature`/change class, lease target, DDL slot (y/n), post-checks, provider support, `Narrowing`.
- State that agent levels use the agent envelope, not `l3Blocker`.
- Justify each irreversible action's level. For example, `estate_reclaim` is `reversible` only while a provider soft-delete window is verified; otherwise it stays at L2.

#### SR-07 · P1 · §3.1, §6.3, §6.7, §6.9 — The exceptions to "one authority" aren't listed

**Problem.** Principle 1 says "there is no second gate" (spec L342-344), yet several paths sit outside it:
- The kill switch "never waits on the gate" (L742-743).
- The direct-lane watchdog cancels statements with `pg_cancel_backend` (L547-548). The core gate always sends backend signals to a human (`policy/gate.go:156-160`), so the watchdog cannot work through it.
- The trash purge is human-only.
- Decommission acknowledgements are configuration.

Two engineers will route these differently.

**Fix.**
- Add a closed list of out-of-gate paths: kill, watchdog cancel, trash purge, decommission. Give each its own audit row and the reason it sits outside the gate.
- Give the watchdog a contract the gate allows without a human, bounded to agent roles and to statements over the profile limits.

#### SR-08 · P1 · §3.4, §6.6, §6.7 — "Re-check in the same transaction" can't hold across databases

**Problem.**
- Principals, grants and leases are install-wide, so they live in the control DB, while the write commits in the target DB (spec L977-981).
- A kill that freezes the principal between re-authorization and COMMIT doesn't stop the commit, so principle 4's "re-checks them in the same transaction" (L352-353) can't hold whenever the control DB is not the target (meta mode, and every fleet database but one).
- The kill also can't find in-flight brokered statements (SR-17).

**Fix.**
- Hold `SELECT 1 FROM sage.agent_principals WHERE id=$1 AND status='active' FOR SHARE` on the control DB from re-authorization until the target COMMIT returns.
- The kill takes `FOR UPDATE` on the same rows, with a `lock_timeout`, and then cancels the target backends it tracks (SR-17).
- Document the residual window.

### B. Postgres identity, roles and privileges

#### SR-09 · P0 · §5.3, §6.3, §6.6, G1-04 — The environment binding can't tell production from its own clone

**Problem.**
- A label is bound to `system_identifier` from `pg_control_system()` plus `current_database()` (L549-553).
- Physical copies keep both values: DBLab thin clones, `pg_basebackup` copies, PITR restores and physical standbys share the source's system identifier and database name. For Aurora and Cloud SQL clones and Neon branches this is UNVERIFIED.
- So if a `dev` label's DSN is later pointed at the production primary, by mistake or by a DNS change, verification passes. Production then gets dev rights: direct-lane writes and L3 `ddl.additive`. That is the "production mistaken for test" failure (L37) the binding exists to stop.
- The statement hash (L665-666) uses the same inputs, so a hash computed on a clone equals the production one.
- Neither research file establishes `pg_control_system()`'s privileges or availability on managed services (incidents cross-check, T7(j)). The code already has a fallback for roles that can't read it (`value/fleet.go:166-176`), which the spec doesn't mention.

**Evidence.** Spec L549-553, L665-666. Existing identity code already goes further (`sre/probes/identity.go:24-33`: timeline and recovery state; `schema/sre_migration.go:17-27`: `sre_database_bindings` with `identity_strength`).

**Fix.**
- Bind to a tuple that differs between copies wherever possible:
  - the provider resource id (endpoint, branch or instance, from `cloudtel` or the adapter);
  - pg_sage's own `sre_database_bindings.database_id`;
  - the system identifier and the database OID.
- If one physical identity is seen under two labels, treat both as `prod` and raise a critical finding.
- Label sandboxes pg_sage created from their receipts.
- Use `database_id` (a UUID), not the name, in the artifact hash.
- Add **G1-04b:** "A DBLab or `pg_basebackup` clone of a `prod` database registered as `dev` is evaluated as `prod` until its provider identity differs. Re-pointing a `dev` DSN at the prod primary is evaluated as `prod`."

#### SR-10 · P1 · §6.3, §6.7, §7 — Roles are cluster-wide, but the spec models them per database

**Problem.**
- `sage.agent_db_roles` is keyed by (principal, database), and kill, freeze and retire run "per database".
- But the role itself, `VALID UNTIL`, `CONNECTION LIMIT` and `NOLOGIN` are all cluster-wide. Two monitored databases on one cluster share `sage_agent_<id>`.
- `DROP ROLE` fails while the role holds privileges in any other database of the cluster.
- `prior_attrs` stored per database can restore stale cluster-wide attributes.

**Fix.**
- Add `sage.agent_cluster_roles (principal_id, cluster_identity, login_role, broker_role, valid_until, prior_attrs, status)`, and keep grants per database.
- Retire runs `REVOKE`/`DROP OWNED BY` in every database of the cluster before `DROP ROLE`.
- The lease end is the maximum over the cluster's databases.
- A per-principal kill is cluster-wide by definition; say so.

#### SR-11 · P1 · §1.2, §6.3, G1-02, G2-07 — PUBLIC privileges defeat least privilege

**Problem.** Every agent role is a member of PUBLIC. By default PUBLIC has:
- `CONNECT` and `TEMP` on every database;
- `EXECUTE` on every function, including `SECURITY DEFINER` ones;
- `CREATE` on `public` on PG14, and in databases upgraded from PG14 by `pg_upgrade`.

Estates also commonly grant tables or default privileges to PUBLIC. The consequences:
- Agent logins reach every database on the cluster, including other tenants' and the control DB (SAFE-ID-05).
- A "read-only" direct-lane agent can call any PUBLIC-executable `SECURITY DEFINER` function that writes.
- A PG14 agent can create objects (which it then owns, AP-02) and plant search-path traps.
- "New tables stay invisible until classified" (L166) fails under default privileges to PUBLIC.
- G1-02 ("effective privileges … equal its grants") fails by construction.

**Fix.** Add a preflight per database before any grant, and per cluster before the direct lane. Refuse the direct lane while any of these holds:
- PUBLIC has `CREATE` on a schema in the profile's path;
- another database on the cluster grants `CONNECT` to PUBLIC (report the exact `REVOKE CONNECT ON DATABASE … FROM PUBLIC`);
- PUBLIC can execute a volatile `SECURITY DEFINER` function.

Also:
- Report PUBLIC table grants and default privileges as posture findings.
- Restate G1-02 as "effective privileges = Guard grants ∪ the PUBLIC baseline recorded at preflight, excluding `pg_catalog` and `information_schema`".

#### SR-12 · P1 · §6.3, G1-08 — Grantor semantics: revokes can silently no-op

**Problem.**
- §6.3 lets pg_sage grant "through membership in the owning role" (L536-542). Postgres then performs the GRANT as the owner and records the owner as grantor.
- A later REVOKE that isn't performed through the same role (for example, PG16+ membership with `INHERIT FALSE`, used without `SET ROLE`) revokes nothing. Postgres only emits a WARNING.
- Privileges from other grantors, or from PUBLIC, survive any revoke.
- Owner-role membership also gives pg_sage `DROP` over application tables.

**Fix.**
- Allow only privileges pg_sage holds `WITH GRANT OPTION`, and drop the owner-membership mode.
- Record the effective grantor at grant time from `aclexplode(relacl)`, and revoke with `REVOKE … GRANTED BY <grantor>` (PG14+).
- Verify against `aclexplode` for that grantee and grantor. On residue, report `revoke_incomplete: other grantor <x>` and fail closed in the broker for that object.
- Add a drift reconciler for grants removed by owners (`CASCADE`).

#### SR-13 · P2 · §6.3 — PG16+ membership mechanics; PG14/15 role management

**Problem.**
- The spec self-grants `GRANT sage_agentb_x TO CURRENT_USER WITH INHERIT FALSE, SET TRUE` after `CREATE ROLE` (L523). Whether every 16.x accepts a self-grant by the ADMIN holder is UNVERIFIED; the documented mechanism is `createrole_self_grant`.
- PG14 reaches end of life on 2026-11-12, before G1 (v2.5) can plausibly ship. Yet G1 adds `agents.allow_createrole_pre16`, on a version where `CREATEROLE` is near-superuser.

**Fix.**
- On PG16+, run `SET LOCAL createrole_self_grant = 'set'` before `CREATE ROLE`. pg_sage then gets ADMIN and SET without INHERIT.
- Support role management on PG16+ only, keep PG14/15 posture-only, and drop `allow_createrole_pre16`.

#### SR-14 · P1 · §6.2, §10.2 — The pre-E3 credential fallback recreates PocketOS

**Problem.**
- Until E3 (v2.6), the direct lane's password is "revealed **once** to the sponsor in the UI" (L507-508).
- In practice it goes into a `.env` or MCP client config in the workspace, where a coding agent can read it. That is INC-20, and enterprise ID-4: no static secrets in agent configs or MCP client files.
- G1 ships the direct lane before E3.

**Fix.**
- In G1, refuse direct-lane credentials to principals whose profile includes coding-agent classes; they use the brokered lane only.
- For app-runtime principals, offer the reveal with a warning, a `VALID UNTIL` of 24 hours or less, and a finding when the role is used from more than one `client_addr`.
- Or move the direct lane to G2, alongside E3.

#### SR-15 · P1 · §6.2, §10.6 — The stdio MCP client has no principal

**Problem.**
- The stdio transport binds a fixed `Principal{Actor: "stdio", Kind: agent, Scopes: read, propose}` with no token, on all databases (`mcp/principal.go:115-116`, `mcp/runtime.go:70`).
- The spec binds principals only to tokens. The local Claude Code client is likely the G2 dogfood path ("the user's Claude Code over MCP", L1416), and it would be either unattributed or locked out.

**Fix.**
- Add `mcp.stdio_principal: <principal name>`. It is required for `agent_*` tools over stdio; without it they return `unsponsored`.
- Say which existing tools stdio keeps.

#### SR-16 · P1 · §6.2, G1-11 — The token→principal migration violates the new constraints

**Problem.**
- Token names are free text of 1–100 characters and not unique (`schema/mcp_v2_migration.go:21`). Principal names must match `^[a-z][a-z0-9_-]{1,62}$` and be unique (L986-987). "A principal named after the token" (L490) fails at bootstrap on spaces, capitals, length or duplicates.
- Tokens also carry a `databases` list, which the spec never reconciles with per-database grants.
- G1-11 makes every decision for a migrated token `deny: unsponsored`. If that covers the existing read and propose tools, the upgrade breaks every current agent integration.
- `POST /api/v1/mcp/tokens` (`api/mcp_token_routes.go:31-33`) would keep minting unbound agent tokens.

**Fix.**
- Use a slug rule: lowercase, map other characters to `-`, truncate to 54, and add a `-<4 base32>` suffix on collision.
- Keep the token's `databases` list as an upper bound, intersected with grants.
- State the upgrade behaviour of existing tools for unsponsored principals. If it changes, put it in the CHANGELOG as breaking.
- Make the old endpoint reject `kind: agent` once G1 ships.

#### SR-17 · P1 · §6.7, G1-05 — The kill switch misses brokered sessions and standbys

**Problem.**
- Brokered statements run on pg_sage's own connections under `SET LOCAL ROLE`. `pg_stat_activity.usename` is the session user, pg_sage's login, so step 5 (`usename = ANY($agent_roles)`) never finds them. Step 7 then reports success while they run (G2 onward).
- `pg_terminate_backend` on the primary doesn't touch sessions on physical standbys or read replicas.
- `ALTER ROLE` runs with no `lock_timeout`, so an open transaction holding the role row blocks the kill.
- With the control DB down, step 1 fails. The spec doesn't say whether per-database steps proceed.
- There is no per-database scope (enterprise SF-3).
- `agent_unfreeze` restores the same password and grants after a possible compromise.
- There is no runbook for when pg_sage itself is down.

**Fix.**
- Adopting SR-18's broker logins makes brokered sessions visible by `usename`. Either way, tag brokered transactions with `SET LOCAL application_name = 'pg_sage agent:<principal>:<action>'`, and track their backend PIDs in memory and in `sage.agent_inflight`. The kill cancels these first.
- Enumerate replicas (`pg_stat_replication`, provider read replicas) and terminate there too.
- Use `lock_timeout` with retries.
- Proceed per database when the control DB is unreachable, and reconcile the audit afterwards.
- Add a `database` scope.
- An unfreeze after a kill forces credential rotation.
- Ship a documented SQL runbook (`ALTER ROLE … NOLOGIN` for `sage_agent_%`, then terminate) for when pg_sage is down.

### C. Brokered read and write paths

#### SR-18 · P0 · §5.1, §6.5, §6.6 — The brokered lane runs agent SQL on pg_sage's own login, so any escape lands on pg_sage's privileges

**Problem.**
- **The architecture contradicts its own research and principle 2.**
  - The brokered lane runs agent SQL on pg_sage's own connection, after `SET LOCAL ROLE sage_agentb_<id>` (L398, L607, L677). That connection's login is the most privileged role Guard has: it holds `CREATEROLE` and `GRANT OPTION`, or owner membership.
  - technical-substrate §2.3 (L184-192) says this pattern "isolates nothing": `RESET ROLE` or `SET ROLE NONE` returns the session user's privileges, so "an agent that submits SQL must log in as its own role".
  - The spec follows that advice for the direct lane only. Principle 2 promises that a parser bypass "must hit a privilege error, not data" (L345-346). Here, a bypass lands on pg_sage's privileges instead.
- **A concrete escape exists without any parser bypass.**
  - Only `<pred>` must pass the L3 catalog proof (L656). SET-list expressions, `VALUES` lists and the `SELECT` of `INSERT … SELECT` may call volatile functions.
  - One such call is `set_config('role', '<pg_sage login>', true)`. The session user is pg_sage, so Postgres permits it.
  - Every later statement in the transaction then runs with pg_sage's privileges: the post-image read, the agent's `verify_sql` (L686) and pg_sage's own bookkeeping.
  - Because `verify_sql` is agent-supplied, the agent can read anything pg_sage can, for example through a cast error such as `(SELECT secret FROM t LIMIT 1)::int` (SR-30).
  - `set_config('search_path', …)` similarly hijacks pg_sage's later unqualified names.
- **Owner-privileged code paths.** `STABLE` `SECURITY DEFINER` functions and views owned by privileged roles also read beyond the broker's grants (SR-24, SR-25).

**Fix.**
- Make the boundary hold if everything else fails. Run brokered SQL on a connection that **logs in as the agent's broker role**: give `sage_agentb_<id>` `LOGIN`, keep its credential inside pg_sage, and use a small per-agent pool with an idle timeout. `RESET ROLE` then returns to the agent's own role.
- A single shared "broker login" is not enough: it would hold `SET` on every agent's broker role, so one agent could switch to another's.
- Keep the defence in depth:
  - Apply the full L2/L3 proof to the whole statement, not just the predicate: immutable or stable functions only, the deny-list, no `SECURITY DEFINER`, no functions with `proconfig`.
  - Put `set_config` on the deny-list by name.
  - Never run pg_sage's own statements after agent SQL in the agent's transaction. The only pg_sage code inside it is the narrow capture function of SR-20.
  - Run verification in a new transaction.
- Add write-path cases (`set_config` in a SET list, in `VALUES`, in `INSERT … SELECT`) to the corpus behind G2-01, and assert that each fails with 42501 or is refused before execution.

#### SR-19 · P0 · §6.6, G2-03, G2-04 — Cascades, triggers and inheritance escape row bounds and undo

**Problem.**
- Bounds and pre-images cover only rows of `t`. All of these change other rows, uncounted and uncaptured:
  - foreign keys with `ON DELETE CASCADE`, `SET NULL` or `SET DEFAULT`;
  - user triggers, including `SECURITY DEFINER` triggers that build dynamic SQL from row values;
  - rules;
  - inheritance children, on UPDATE or DELETE of a parent without `ONLY`.
- A `DELETE … WHERE id = 5` with `max_rows: 1` can remove a million child rows, and undo restores one.
- Over-broad action is the research's main risk class (DBA-Bench, spec L136-142), so this is the case that matters.

**Fix.**
- Deny `write.delete`, and any `write.update` of key columns, on tables referenced by FKs with an action other than `NO ACTION`/`RESTRICT`.
- Deny writes on tables with non-internal triggers or rules, unless an admin allowlists the table (recorded as a fact).
- Require `ONLY`, or deny, on inheritance parents.
- Count total rows changed in the transaction (for example, `pg_stat_get_xact_tuples_*` per affected relation), and roll back above the bound.
- Add G2-03b (a cascading FK) and G2-04b (a trigger).

#### SR-20 · P1 · §6.6 — Pre-image capture needs privileges the broker must not have

**Problem.**
- Step 2 runs `SELECT <pk>, to_jsonb(t.*) … FOR UPDATE` as the broker role (L681).
- `to_jsonb(t.*)` is a whole-row reference, which needs `SELECT` on every column. A column grant excluding `secret` columns, the spec's own rule, makes every brokered UPDATE or DELETE fail with 42501.
- `FOR UPDATE` needs `UPDATE` privilege, which a delete-only profile lacks.
- The appended `RETURNING … to_jsonb(t.*)` has the same problem.
- The tempting fix, granting the broker full `SELECT`, breaks "secrets are never selectable" (L167).

**Fix.**
- Run only the agent's DML with the broker's privileges, with `RETURNING <pk>`.
- Capture full pre-images and post-images through a narrow `SECURITY DEFINER` function owned by pg_sage. It runs inside the broker's transaction, on the same snapshot as the DML, with a pinned `search_path`. It takes only a `regclass`, key values and an action token, and returns nothing to the caller.
- Have that function write to the target database's `sage` schema, so capture is atomic with the DML. This also settles SR-37's write order.
- State that pre-images then contain secret columns, and how they are stored (SR-42).

#### SR-21 · P1 · §6.6, G2-04 — Undo breaks on common table shapes

**Problem.**
- The inverse `UPDATE … SET (cols) = (pre)` fails on generated columns: stored ones, and on PG18 virtual ones too.
- Re-inserting deleted rows fails on `GENERATED ALWAYS` identity columns without `OVERRIDING SYSTEM VALUE`.
- Large TOASTed rows times `max_rows` have no byte cap.
- An UPDATE that changes key columns (including a partition key, which a partitioned table's primary key must contain) leaves the row under a new key. The spec doesn't say whether the inverse matches on the pre-image key or the post-image key; only the latter finds the row.
- Tables without a primary key are writable at L2 in `dev` and `branch`, but have no undo.
- The role undo runs as is unspecified; a delete-only broker can't re-insert.

**Fix.**
- Match the inverse UPDATE and DELETE on the post-image key.
- Exclude generated columns, and use `OVERRIDING SYSTEM VALUE` for identity columns.
- Add `agents.writes.max_preimage_bytes`, and deny above it.
- Report `undo_available: false` for tables without a key.
- Run undo as pg_sage, at L2.

#### SR-22 · P1 · §5.2 (`maint`), §6.5 — Maintenance and concurrent DDL can't run in the brokered lane

**Problem.**
- The lane is `BEGIN … SET LOCAL ROLE … COMMIT`. `VACUUM`, `REINDEX CONCURRENTLY` and `CREATE INDEX CONCURRENTLY` cannot run inside a transaction block. This is one of pg_sage's own known failure patterns.
- Before PG17, `VACUUM` and `ANALYZE` need table ownership (or database ownership or superuser). Agents own nothing, so `maint` is impossible for broker roles on PG14–16.

**Fix.**
- Run `maint` outside a transaction block: on the broker's own login connection (SR-18), or on a dedicated connection with session-level `SET ROLE`, then `RESET ROLE` and `DISCARD ALL`, or close the connection.
- On PG14–16, either run it as pg_sage on the agent's behalf (a gated, agent-requested pg_sage action) or mark the class PG17+.

#### SR-23 · P1 · §5.2, §6.9 — Who owns objects an agent's migration creates?

**Problem.** "Agent roles never own objects" (L534-535), but `ddl.additive` includes `CREATE TABLE`, and `CREATE INDEX CONCURRENTLY` needs table ownership. Every executing role has a cost:
- Run as the broker, the agent owns the table: AP-02, critical.
- Run as pg_sage, pg_sage owns application tables, which then blocks the application's own migrations.
- Run as the owner role, pg_sage needs membership in it (SR-12).

**Fix.**
- The change path runs as the table's or schema's owner role through `SET ROLE`, with pg_sage a member `WITH SET TRUE, INHERIT FALSE` (PG16+).
- Created objects are owned by that role, and the action records the owner.

#### SR-24 · P1 · §6.5 — `agent_query` can't reuse the EXPLAIN guard as is

**Problem.**
- The guard refuses every relation with row-level security policies and every foreign table (`explain/analyze_guard.go`, `relationKindRefusal`). Reused verbatim, `agent_query` would deny every Supabase table.
- It follows views to depth 4 (`maxViewDepth = 4`), not 3.
- Its refusals are volatility plus a list of SQL-running functions (`query_to_xml*`, `table_to_xml*`, `cursor_to_xml*`, `ts_stat`, `pg_input_is_valid`, …). That is not the spec's deny-list (L627-629).
- It allows `STABLE` `SECURITY DEFINER` functions. These read with their owner's privileges, beyond the agent's grants.

**Fix.**
- State the RLS rule for agent reads: allowed, since agent roles are `NOBYPASSRLS` and policies apply; refused only when a policy expression calls a volatile function.
- Deny `prosecdef` functions unless allowlisted.
- Make the guard's list the normative deny-list.

#### SR-25 · P1 · §5.1, §6.5, §6.7 — Output masking is bypassable; "masked direct lane" contradicts a non-goal

**Problem.**
- pgx `FieldDescriptions` carry a table OID and attribute number only for plain column references:
  - through a view, they name the view, not the base table;
  - `UNION`, expressions and scalar subqueries report OID 0;
  - whole-row references (`to_jsonb(t)`, `t::text`, `row_to_json(t)`) carry every column.
- pg_query returns a raw parse tree with no name resolution, so "a PII column inside an expression is denied" (L636-637) can't be checked reliably.
- Classification isn't propagated through views owned by privileged roles. A view granted to the agent can expose `secret` base columns with no 42501, so G2-07 passes on the table but not the view.
- §5.1 calls the direct lane "masked read-only in prod" (L396), while §5.4 makes "inline masking for direct connections" a non-goal (L433).

**Fix.**
- Say masking is a convenience, not a control.
- In `stage` and `prod`, enforce `pii` and `secret` through column privileges only, in both lanes.
- Propagate classification to views via `pg_depend`/`pg_rewrite`.
- Treat output columns with OID 0 as classified whenever the query references a classified relation.
- Reword §5.1 to "read-only, with `pii` and `secret` columns excluded by privilege".

#### SR-26 · P1 · §6.7, §7 — `column_class` needs facts schema changes and fails open

**Problem.**
- `sage.facts` CHECKs allow five fact types, and subject kinds `index`, `table`, `schema`, `slot` (`schema/facts_migration.go:21-24`). §7 has no ALTER for a `column_class` type or a `column` kind.
- Facts are keyed by name text (`facts_subject_key`), so renaming a classified column drops its classification.
- A table-level grant covers columns added later, so a new `api_key` column is readable the moment it's added.
- Keys inside `jsonb` aren't covered at all (community-pain rank 10, masking drift).

**Fix.**
- Add the type and kind under a versioned constraint.
- Key column facts by `(relid, attnum)`, with the name kept for display.
- Always grant column lists in `stage` and `prod`, never whole tables.
- On catalog change, re-classify before a new column becomes visible: revoke, then classify.

#### SR-27 · P0 · §6.6, G2-02 — Approvals don't bind what was approved

**Problem.**
- The hash covers the system identifier, database, broker role, canonical SQL, relations and parameters (L665-666).
- It omits `max_rows`, `verify_sql`, the environment label and binding, the principal, the policy version, and the appended `RETURNING` and undo plan.
- G2-02's flow ("approve statement S, then execute S") matches the approval by hash on a later call. So an agent can get S approved with `max_rows: 10`, then resubmit S with `max_rows: 10000` or a trivial `verify_sql`, and the hash still matches. That defeats SAFE-PERM-02 and SAFE-PERM-03 together.

**Fix.**
- Hash the whole request envelope: `{canon, rels, params, max_rows, verify_sql canon, principal_id, database_id, env label and binding id, policy version}`.
- Bind each approval to one action id as well as the hash.
- Vary each field in G2-02.

#### SR-28 · P1 · §6.7, G2-05 — Taint is laundered by choosing a new task id

**Problem.**
- Taint is keyed on `(principal, task_id)`, and the agent supplies `task_id` (L722-724, L1183-1185).
- After reading an untrusted table, the agent can write under a fresh id.
- Only the 60-second anomaly rule (L740) catches a quick switch.

**Fix.**
- Taint the principal, all tasks, by default.
- Allow per-task taint only when the task id is a broker-issued session id or comes from a trusted runtime claim.

#### SR-29 · P2 · §6.5, §6.6, G2-12 — Statement mechanics

**Problem.**
- **Row cap.** "Fetch at most `max_rows + 1` rows" doesn't bound server work: pgx's `Rows.Close` drains the rest of the result.
- **`search_path`.** The spec's path omits `pg_temp`, which Postgres then searches first for relations.
- **Catalogs.** All of `pg_catalog` is readable, including `pg_proc.prosrc`.
- **Deparse.** pg_sage executes the deparsed text, so a pg_query deparse bug changes what runs.
- **Shapes not classified:** `ON CONFLICT`, `DEFAULT VALUES`, `OVERRIDING`, `ONLY`, `WHERE CURRENT OF`, and sub-selects in SET.
- **Privileges `agent_grant` omits:**
  - `INSERT` into tables with `serial` columns needs `USAGE` on the sequence;
  - every table grant needs schema `USAGE`, which is shared across grants and so needs reference counting at revoke.
- **Latency.** The catalog proof costs several round trips per query, which makes G2-12's 25 ms p95 doubtful on remote databases.

**Fix.**
- Read through a cursor (`DECLARE … CURSOR FOR <q>`, then `FETCH n+1`), or cancel the query after n+1 rows. A `LIMIT` wrapper around a subquery doesn't guarantee the inner `ORDER BY`.
- Set `search_path = pg_catalog, <schemas>, pg_temp`.
- Define a readable-catalog allowlist (R0).
- Require `Fingerprint(Parse(canon)) == Fingerprint(Parse(sql))`.
- List accepted shapes positively; anything else is `unsupported_shape`.
- Grant sequence and schema `USAGE`, with reference counting.
- Cache catalog proofs by (canonical hash, catalog epoch), and state the latency target with the cache.

#### SR-30 · P1 · §6.5, §6.6, §8.1 — Error text leaks masked and escalated data

**Problem.**
- Server error messages carry values. `WHERE ssn::int = 0` returns the SSN in the cast error, which bypasses output masking (§6.5 L5) on a column the agent may filter on.
- Combined with SR-18, error text is the exfiltration channel.
- The MCP outputs (§8.1) don't say whether server errors reach the agent.

**Fix.** Return only SQLSTATE plus a fixed, pg_sage-authored message to agents, and log the full text for operators. Add an RO case that casts a masked column in a predicate.

### D. Approvals, separation of duties, the trust ledger

#### SR-31 · P1 · §3.3, §6.8, §8.2 — Separation of duties has holes; single-admin installs can't approve

**Problem.**
- A sponsor can sign their own agent's promotion; only production approvals exclude the sponsor (L350).
- One admin can widen everything alone, which D4a permits:
  - relabel `prod` as `dev` (labels widen rights);
  - add `agents.unmask`;
  - change a profile;
  - unfreeze;
  - create a second admin and approve with it.
- The "DBA role" that signs `ddl.*` and `maint` (L782) doesn't exist. pg_sage has `admin`, `operator` and `viewer` (`auth/types.go:21-35`).
- A one-person install has only one human, the sponsor, so "a sponsor cannot approve their own agent's production change" makes production approvals impossible there. lifeos is likely such an install (UNVERIFIED).

**Fix.**
- Require two people (a second admin, never the sponsor) for widening actions: label widening, unmask, profile and ceiling changes, and promotions.
- Map "DBA" to `operator` until E3 adds roles.
- Add an explicit `single_operator_mode`, off by default, that allows self-approval with a recorded reason and a post-hoc review queue.
- Detect departed sponsors (IdP or SCIM deprovisioning, or a disabled `sage.users` row), drop their agents to L0, and raise an "ownerless agent" finding (enterprise ID-2, EN:333).

#### SR-32 · P1 · §8.1, §8.2 — Where approvals happen is contradictory

**Problem.**
- §8.1 says approval happens only in pg_sage's UI, signed by a session user (L1197-1198).
- §8.2 says agent actions use the existing approval cards (L1219). Those are approved from chat buttons with single-use tokens (`schema/approval_cards_migration.go`, `sage.approval_card_deliveries`).
- A chat button can't do the typed object-name confirmation (SAFE-HUM-05), the sponsor exclusion or two-person approval.

**Fix.** Agent-action cards in chat are notification-only, with a deep link to the UI. Make any exceptions an explicit, configurable class list, empty by default.

#### SR-33 · P1 · §5.2, §7, G3-01 — Two-person approval has no data model

**Problem.**
- `sage.action_queue` has one `decided_by`, and `expires_at` defaults to 7 days (`schema/bootstrap.go:626-652`).
- `approval_card_deliveries` has one `used_by`.
- G3-01 needs two approvers in production, and the spec's TTL is 15 minutes.

**Fix.**
- Add `sage.action_approvals (queue_id, approver_user_id, decision, decided_at, card_hash, UNIQUE (queue_id, approver_user_id))`, with the quorum and sponsor-exclusion rules enforced in the executor.
- Set `expires_at = now() + approval_ttl` for agent items.

#### SR-34 · P1 · §6.8, §7 — The agent ledger is a refactor of `internal/earned`, not a mirror

**Problem.**
- The existing ledger (`schema/sre_m7_autonomy_migration.go`) has `deployment_id`, a `version` for compare-and-swap, `changed_by` and `change_reason`. It also has a proposals table (one pending proposal per pair), an append-only events table and an outcomes table.
- `agent_capability_autonomy` (L1068-1078) has none of these. It also records promotions in `sage.action_log` (L790) instead of `sre_autonomy_events`.
- `internal/earned` is typed around incident `Family` × executor `ActionClass` (`earned/class.go`), and the ledger tables CHECK both names against `^[a-z][a-z0-9_]{0,63}$` (`schema/sre_m7_autonomy_migration.go:13-14`). `write.insert` isn't even a valid name.
- "Reuses `internal/earned` … with a principal subject" is therefore a cross-cutting refactor, and it isn't in G2's estimate.

**Fix.**
- Either generalize the ledger (`subject_kind IN ('family','principal')`, `subject`, `capability`, with one proposals/events/outcomes schema) and estimate the work,
- or specify the agent tables fully: proposals, events, outcomes, version and deployment id.
- Either way, rename capabilities with underscores (`write_insert`).

#### SR-35 · P1 · §6.8, §5.2 — Promotion defaults contradict a user decision and the fatigue evidence

**Problem.**
- Promotion needs 30 approved writes (10 for DDL) over 7 days, per (principal, database, class) (L772-774).
- The user asked for trust to elevate "in hours not days/weeks" (session-history L190-191). Separately, in a LinkedIn-draft review, the user called the trust emphasis "important but overdone" (session-history L268).
- Approval fatigue is a research finding (community-pain §1 item 10 and §2.9), yet the "≤ 5 approvals per 100 actions" target counts only after promotion (L1471).
- Production `ddl.additive` at L3 (L411) is what practitioners reject most (community-pain L1392-1393).
- Starting levels for `ddl.locking`, `ddl.destructive`, `maint` and `sandbox` in `dev` and `branch` aren't given (L767).

**Fix.**
- Reuse the core's fast-trust ramp (`trust.ramp_*_hours`) for agents.
- Pool evidence across databases cloned from one template.
- Count ramp-period approvals in the bench target.
- Default production `ddl.additive` to L2, with L3 opt-in.
- Complete the starting-level table.

#### SR-36 · P2 · §6.7, §10.7 — No break-glass and no emergency change path

**Problem.**
- enterprise-needs lists break-glass (SF-4) and an emergency change path with retrospective review (AP-3) as MUSTs.
- If the IdP is down, admins can't log in to press the kill switch.

**Fix.**
- Add a local break-glass admin, with an alert on every use and a post-use review.
- Add an emergency path that records retrospective approval.
- Include both in the evidence packs.

### E. Data model and migrations

#### SR-37 · P0 · §7 — Install-wide tables keyed by per-database action ids

**Problem.**
- `sage.action_log` lives in each monitored database's `sage` schema, because the executor writes through its own pool (`executor/manual.go:482`, `executor/retention_run.go:89`). Action ids therefore repeat across databases.
- `agent_preimages` is install-wide with `PRIMARY KEY (action_id, seq)` and no database column (L1080-1091). `agent_grants.grant_action_id`, `revoke_action_id` and `restore_points.action_id` have the same flaw.
- In a fleet, the second database's pre-images collide; worse, undo for one database reads another's rows.
- The write order across databases is unspecified:
  - if the target commits and the control insert fails, the write has no undo;
  - in the reverse case, orphans remain.

**Fix.**
- Add `database_id uuid` (from `sre_database_bindings`) to every per-database row and to each key.
- Specify the ordering. Either write pre-images to the target's `sage` schema in the same transaction (SR-20's capture function does this) and copy them to the control DB asynchronously, or mark the action `undo_unavailable` when the control insert fails.

#### SR-38 · P1 · §7 — Without a meta DB, the control database can move between restarts

**Problem.**
- Without `mode: meta`, the control database is "the first one whose runtime starts" (`cmd/pg_sage_sidecar/fleet_bootstrap.go:15-24`), or the primary when it is up (cross-check: `fleet/manager.go:280-300`).
- If it changes, principals, grants and leases "disappear", and expiries are never revoked.

**Fix.**
- Agent Guard requires `mode: meta` or a pinned control database, and refuses to enable otherwise.
- Alternatively, keep each database's grant registry in that database, and only principals install-wide.

#### SR-39 · P1 · §7, G2-13, §6.9 — Columns the checks need don't exist

**Problem.**
- G2-13 requires the principal, task id, approval or envelope id, statement hash and policy version on every agent action record. `sage.action_log` has none of them (`schema/bootstrap.go`, `ddlActionLog`), and §7 adds none.
- `sage.decision` lacks a principal (SR-05).
- G1's grants need provenance, but the `proposed_via` change is scheduled for G2 (L1059).

**Fix.**
- List the ALTERs: `principal_id`, `task_id`, `approval_id`, `envelope_id`, `artifact_hash` and `policy_version` on `action_log` and `action_queue`.
- Add an `on_behalf_of` subject (from a token-exchange `act` claim in E2), which enterprise ID-3 requires and G2-13 omits.
- Move the provenance migration into G1.

#### SR-40 · P1 · §7 — The `proposed_via` migration re-runs on every boot

**Problem.**
- It drops and re-adds `action_queue_proposed_via_check` under the same name the Ask Sage migration guards with `IF NOT EXISTS` (`schema/ask_migration.go:52-62`). So it runs on every start and takes `ACCESS EXCLUSIVE` each time.
- Inside one transaction, `NOT VALID` followed by `VALIDATE` doesn't shorten the lock, contrary to the comment at L1065-1066.
- It must run after `ddlAsk` in the bootstrap list (`schema/bootstrap.go:421`).

**Fix.**
- Use a new constraint name (`action_queue_proposed_via_v2`) behind an `IF NOT EXISTS` block that drops the old one.
- Run `VALIDATE` in a separate transaction.
- Register the migration after `ddlAsk`.

#### SR-41 · P1 · §7, §12 — `sage.agent_db_roles` reuses the prefix being dropped

**Problem.**
- G1 creates `sage.agent_db_roles` and drops "the 27 tables" in the same release.
- Any prefix-based drop, grep or retention rule hits the new table.
- §7 also says "the 27 `sage.agent_db_*`, `agent_identities` and related tables" (L1168). The real set is 26 `agent_db_*` plus `agent_identities`.

**Fix.**
- Rename the new table (`sage.agent_roles` or `sage.guard_roles`).
- Drop by an explicit list of 27 names.
- Correct the wording.

#### SR-42 · P1 · §6.2, §6.6 — Encryption depends on an optional key

**Problem.**
- Pre-images and fallback passwords are "encrypted with `internal/crypto`" (L508, L692-693). That needs `encryption_key`, which is optional (`config/config.go:118`; `HasEncryptionKey`, `:1394-1396`). The spec doesn't say what happens without it.
- `crypto.Encrypt` uses AES-GCM with no associated data (`crypto/crypto.go:20-44`), so ciphertexts can be swapped between rows.

**Fix.**
- Features that store secrets or pre-images require `encryption_key` and fail closed without it: brokered UPDATE and DELETE and the fallback credential are refused, with a message naming the key.
- Bind each ciphertext to `(database_id, action_id, seq)` as associated data.
- Add key ids (E1).

#### SR-43 · P2 · §6.10, §7 — New identities duplicate existing ones

**Problem.**
- `pg_sage_install_id`, described as "new; one per install, in `sage.config`" (L861), duplicates `sage.sre_deployments.deployment_id`, a singleton UUID (`schema/sre_migration.go:11-15`).
- The environment registry duplicates `sage.sre_database_bindings` (`identity_strength`, `cluster_epoch`).

**Fix.** Reuse `deployment_id` as the install tag, and bind environments to `sre_database_bindings.database_id`.

#### SR-44 · P2 · §7, §10.7 — Retention and tamper evidence fall short of the enterprise research

**Problem.**
- Revoked grants are purged after 90 days and pre-images after 7, and no retention is set for agent decisions and actions (L1161-1166).
- enterprise-needs asks for at least six months of retention (AU-5) and for tamper evidence (AU-1). CG-05 names tamper evidence, but no release schedules it.
- Pre-images hold personal data, which erasure requests must also cover.

**Fix.**
- Make retention configurable, defaulting to six months or more for decisions, grants and actions.
- Hash-chain the audit (E2), with the SIEM as the off-box copy.
- Document how erasure requests cover pre-images.

### F. Sandboxes, rehearsal, drills and the estate

#### SR-45 · P1 · §6.10, §6.4 step 7, §3.7 — Restore points and drills don't work as designed

**Problem.**
- RDS, Aurora, Cloud SQL and Lakebase restore by timestamp, so restoring "to a recorded restore point" by LSN isn't possible there.
- `clone.Provider` has no point-in-time method at all (`clone/provider.go:19-23`).
- Checksums captured "when it created the restore point" aren't taken in the same snapshot as `pg_current_wal_lsn()`, so drills fail spuriously on a busy database.
- Capturing them at every risky action means a full scan of up to five tables of a million rows before each agent write.
- `t::text` depends on `TimeZone`, `DateStyle`, `IntervalStyle` and `extra_float_digits`.
- The rules contradict each other:
  - Principle 7 forbids autonomous production writes without proven recoverability (L360-361).
  - Gate step 7 caps at L2 only "once a substrate exists" (L576), so with no substrate, PITR posture alone allows L3.
  - §6.10.4 says no substrate caps at L2 (L886).
  - The enterprise research says to deny writes without verified PITR.

**Fix.**
- Separate a cheap per-action restore point (LSN, time and xid) from a periodic drill, for example daily.
- The drill restores to a timestamp and compares checksums captured in an exported snapshot at that time, with the settings above pinned.
- Define the drill for each substrate.
- Make step 7 read: "Writes in `prod`: deny unless PITR posture is OK; L2 unless a drill passed within `require_restore_drill_days`."

#### SR-46 · P1 · §6.10, G3-05 — Masking a sandbox after copying it leaks and costs

**Problem.**
- Masking with `UPDATE` after creation (L870-871) rewrites every PII row. That materializes a copy-on-write branch (cost and time) and takes hours on large databases.
- It also leaves the unmasked data in the branch's history (Neon point-in-time) or in the clone's snapshot (DBLab/ZFS).
- Nothing says credentials are withheld until masking finishes.

**Fix.**
- Mask once, at the golden template or a masked parent branch, and branch from that.
- Issue credentials only after masking and a passing PII scan.
- G3-05 checks that window (no credential before masking), and that pre-masking history is unreachable.

#### SR-47 · P1 · §6.10, §8.1, §9 — The sandbox interface is incomplete and breaks existing config

**Problem.**
- `request_sandbox` returns `endpoint_ref` and `credential_ref`, but nothing says how an agent resolves them without a secrets manager.
- `clone.Provider` returns a plaintext DSN (`clone/provider.go:13-17`).
- One global `clone.provider` can't serve a fleet whose databases sit on different substrates.
- The existing `clone.provider` accepts `none`, `dle` and `snapshot` (`config/agent_native.go:37-42`). §9 lists `none | dblab | neon | lakebase | template | schemaonly`, which renames `dle` and drops `snapshot`.

**Fix.**
- Register each sandbox as an ephemeral fleet database. Agents use it through `agent_query` and `agent_propose_write` with `database: <sandbox id>`, so no credential leaves pg_sage.
- Keep `dle` and `snapshot` as valid values.
- Make the substrate a per-database setting (`databases[].clone`).

#### SR-48 · P1 · §6.11 — G4's observe tier and chargeback fail on scale-to-zero, and repeat AgentDB's mistakes

**Problem.**
- `pg_stat_statements` counters live in shared memory and reset when a compute restarts (UNVERIFIED per provider), so hourly deltas miss most work on scale-to-zero computes.
- Each observation keeps a compute awake for its idle window.
- G4-04 compares chargeback with the same provider API it reads from.
- Three issues from the AgentDB audit carry into G4 (cross-check):
  - A-32: databases promoted to the full runtime inherit the global execution mode, which falls back to `auto`;
  - §3.12: their content reaches pg_sage's LLM features;
  - §3.6: the env-reference path allows `sslmode=prefer`.
- Neon, Supabase and Lakebase field names are UNVERIFIED (L899).

**Fix.**
- Take chargeback from provider usage APIs only; `pg_stat_statements` attribution is best effort.
- Estate databases start at observation (L0), whatever the global mode.
- Fence LLM context from estate databases as untrusted.
- Default to `sslmode=verify-full`.
- Rewrite G4-04 to compare against an independently metered workload.

#### SR-49 · P2 · §6.9, G3-07 — Delayed drops aren't equivalent to drops

**Problem.**
- Renaming (L839-840) keeps dependent views, foreign keys, publications, RLS policies and grants working, so an app expecting the table gone still reads it.
- `sage_trash_<ts>_<name>` exceeds 63 bytes for long names, and truncation can collide.
- A "renamed copy" for `TRUNCATE` is a full table copy.
- `DROP COLUMN` isn't delayed.

**Fix.**
- Name trash objects `sage_trash_<action_id>`, with a mapping table.
- On rename, revoke all privileges and detach the object from publications.
- Refuse when dependents exist, unless the statement had `CASCADE`; then trash the dependents too.
- Deny large `TRUNCATE` instead of copying.

#### SR-50 · P2 · §6.9, G3-08 — Change-path failure semantics and the B1 fix are undefined

**Problem.**
- Statements are applied one by one, but `down_sql` covers the whole migration (L830-834). After a partial failure, down either fails or undoes the wrong things.
- Running a destructive `down_sql` automatically in production is itself a risky change.
- A failed rehearsal caps at L1, but having no substrate caps at L2 (L828-829), which rewards not configuring one.
- B1's verdict constant is `promote_expand` (`migration/rehearsal/orchestrator.go:18`), not `promote` as G3-08 says.
- The spec doesn't say how the missing affected-query evidence will be captured.

**Fix.**
- Group non-`CONCURRENTLY` statements into transactions, with a down step per group.
- Auto-run down only for additive groups.
- Cap unrehearsed migrations at L1 as well. Allow an "unrehearsed" L2 only for `additive`.
- Design the B1 evidence: replayed queries, before and after latency, plan hashes.

### G. Decommissioning (G0)

#### SR-51 · P0 · §10.1 G0-01, §12 — The decommission inventory misses billed resources

**Problem.** G0 deletes the only code that knows about AgentDB resources, so the inventory must be complete. It isn't:
- **Wrong status list.**
  - G0-01 includes `creating`, which is never stored: it's an AWS status mapped to `provisioning` (`agentdb/aws_rds_runner.go:309`).
  - It omits `provisioning` and `status_unknown` (`agentdb/lifecycle.go:49-51`).
  - It omits `status_checked`, which teardown treats as live (`agentdb/teardown_reconcile.go:19`).
- **Live rows that look dead.**
  - Rows marked `failed` have `live_mode` and `create_operation_id` cleared (`agentdb/live_create.go:201-208`; DP-08).
  - Rows recorded as destroyed after a wrong-scope "not found" still bill (DP-02).
- **Resources with no row.** RDS final snapshots are untagged and never deleted; `local_postgres` databases and schemas stay in the control DB (DP-20); `sage` schemas sit inside fleet-attached agent databases (cross-check).
- **Fields not recorded.** §12 promises region and account in the log line (L1486-1488), but live receipts don't record them (cross-check: `live_create.go:181-185`).
- **Credentials stay attached.** Provider tokens, the Supabase master secret, `PG_SAGE_AGENTDB_*` DSNs and the runbook-granted IAM all remain.
- **In-flight rows freeze.** Rows mid-create or mid-destroy stop moving when the reconciler is deleted.

**Fix.**
- Select by evidence of any live call: `live_mode`, a non-empty `create_operation_id` or `provider_resource_id`, a live receipt, an `execute_live` attempt, or a consumed authorization. Cover every status.
- Add snapshots, local artifacts and agent-database `sage` schemas to the inventory.
- Print the deterministic resource names, and "account unknown" where it was never recorded.
- Write per-provider delete templates, pinned by golden tests:
  - Cloud SQL lifts deletion protection first;
  - RDS offers a final-snapshot choice;
  - Neon has separate project and branch commands.
- Add a credentials-and-IAM step, with a startup warning while provider tokens remain set.
- Document a drain on the last pre-G0 version.

#### SR-52 · P1 · §12.3 — The drop condition can never be true

**Problem.**
- The tables are dropped only when the acknowledgement exists "or when no table has any rows" (L1494-1495).
- AgentDB's `Ensure` creates all 27 tables, and seeds nine size profiles and a version row, on every install with an auth pool (cross-check: `agentdb/schema.go:72-80`, `agentdb/profiles.go:152-200`).
- So every v2.5 install refuses the drop, including installs that never used AgentDB.

**Fix.** Test only the operational tables: deployments, requests, receipts, authorizations and audit.

#### SR-53 · P1 · §6.1, §9, G0-02 — The removal list is incomplete, and G0-02 can't pass

**Problem.**
- §6.1 lists four paths. AgentDB also lives in all of these:
  - `web/src/pages/agentdb/` (17 files), `App.jsx` and `Layout.jsx`, and the Playwright specs;
  - `api/auth_middleware.go:113-117`, which exempts the AgentDB ping and agent-API prefixes from session auth;
  - `retention/agent_rules.go` and `retention/exemptions.go`;
  - `config/config.go:144-160`, `config/clone.go:23-35` and `config/key_classes.txt:112-116`;
  - `store/config_helpers.go` and `api/config_apply.go`;
  - the router golden file, the committed UI bundle and the generated config docs.
- G0-02's `git grep -il agentdb -- sidecar/` misses `agent_db` and `agent-db` (for example `retention/agent_rules.go` and `web/e2e/navigation.spec.ts`). `CHANGELOG.md` is at the repo root, outside `sidecar/`.
- Persisted `agentdb.*` overrides in `sage.config` survive a YAML-only rejection.
- Rejecting `agentdb:` keys makes a monitoring tool fail to start over a removed feature.

**Fix.**
- Write an explicit removal manifest.
- G0-02 uses `git grep -ilE 'agent[-_]?db|agt_'` with an allowlist.
- Remove the auth exemptions together with the routes.
- Add a G0 migration that deletes persisted `agentdb.*` rows, with an audit entry.
- The loader warns about and ignores `agentdb:` keys, failing only when `agentdb.live_provisioning_enabled: true`.

### H. Releases, gates, strategy and research fidelity

#### SR-54 · P0 · §10.2, §10.3 — G1 can't be built as scoped

**Problem.**
- G1 ships `agent_grant` and `agent_revoke` for `read` in the direct lane (L1338-1340), with "per capability trust" as the maximum level.
- But the agent branch of the gate (§6.4), the trust ledger (§6.8) and classification (`column_class`, §6.7) are all G2 scope (L1358-1360). So G1 has no decision procedure for a grant, and no classification.
- Under principle 6 ("unclassified table = invisible"), nothing is grantable.
- Without principle 6, production `read` grants, which start at L3 in prod (L769), expose `secret` columns.

**Fix.**
- Move into G1:
  - the gate composition (SR-01);
  - `Decide` steps 1–5;
  - `column_class` facts and column-level grants;
  - the provenance columns.
- In G1, every grant is L2 (operator-approved), and `prod` grants exclude unclassified columns. Given SR-01, that also requires `trust.level` of `advisory` or higher; say so.

#### SR-55 · P1 · §0, §10.6, §11, §13 R7 — Distribution is the named constraint, but none is planned

**Problem.**
- The spec says distribution, not features, binds (L100-102), with 13 stars and no users. It then plans about 37 agent-days of G0–G4, plus unestimated E1–E4 work and an unscheduled bench.
- The gates don't test pull:
  - G1's gate is internal: lifeos, where "zero false criticals" is near-trivial.
  - G2's "3 or more external installs using agent tokens" can't be measured without telemetry, and its fallback ships `agent_query` anyway.
  - G4's gate can be met by a self-scripted test org.
- AgentSafetyBench is the distribution asset, yet it has no release slot or effort.
- G0's 3 agent-days look optimistic for its scope: 14 detectors with fixtures on PG14–18, the decommission work, and the deletions and doc moves.

**Fix.**
- Make G0 the launch: posture, the RO corpus and the incident replays, published as AgentSafetyBench v0 with a write-up.
- Define a measurable pull metric: a named design partner, external issues or discussions, or an opt-in install ping.
- Gate G1 on that metric, and drop the ship-anyway fallback.
- Re-estimate G0.

#### SR-56 · P1 · §1.4, §2.5, §3.8, §6.10 — The spec departs from its research without saying so

**Problem.**
- **Provisioning.**
  - competitive-analysis ranks a governed provisioning broker as a win (W3) and recommends narrow, keep and add (§4.4).
  - The deploy-paths audit recommends freezing RDS, keeping the Neon and Lakebase branch runners, and keeping and hardening the reconciler (§12).
  - The spec kills all of it.
- **Migrations.** The research files schema change management under LOSE, and Atlas, Bytebase and Liquibase under INTEGRATE. The spec makes pg_sage "the reviewer of agent migrations" (L248) and never names those tools.
- **Instances.** "pg_sage creates no instances" (L362-363) forbids the Aurora and Cloud SQL clone adapters that §6.10 defers (L857). Estates on RDS, Aurora and Cloud SQL then have no rehearsal or drill substrate, so their production write autonomy stays at L2 permanently (§6.10.4).
- **The user's own framing.** session-history records the user's 2026-05-07 framing, in which pg_sage provisions (cross-check: SH:438-442), and the 2026-06-10 quotes the spec relies on are hedged ("may be ephemeral", SH:443-448) but quoted as facts (spec L119-121). §14 Q4 asks only about live resources. Deleting 20k lines of the user's feature should be an explicit decision, not a default.

**Fix.**
- Add a "departures from research" table with reasons.
- Make "delete provisioning" an explicit §14 decision for the user, with the research's narrow/keep/add alternative stated.
- Allow short-TTL provider clones under receipts as sandboxes, as an explicit exception to principle 8.
- Scope G3 to rehearsal evidence plus a PR or plan hand-off to Atlas and Bytebase, or justify building a second change-review product.

#### SR-57 · P1 · §10 — The release order inverts the research ranking and the lifeos rule

**Problem.**
- competitive-analysis ranks operating agent databases first (W1), and finds access-only MCP servers an order of magnitude more popular. enterprise-needs says pilots start on branches.
- The spec ships brokered production writes (G2) before sandboxes (G3).
- The gates clash with a recorded user rule, "lifeos: read-only for agents; reconfigured only as part of an upgrade" (session-history L419). G1's gate creates agent roles on lifeos, and G3's gate needs "G2 writes used on lifeos branches for 2 weeks" (L1417).
- Branches of lifeos need the G3 substrate, which that gate precedes.

**Fix.**
- Reorder:
  - G2a: role-enforced `agent_query`;
  - G2b: sandboxes (`template`, `schemaonly`) and rehearsal;
  - G2c: brokered writes in `branch` and `dev` only;
  - production writes last.
- Replace the lifeos-writes gates with sandbox gates, or get the user's explicit approval.

#### SR-58 · P1 · §3.9 — No LLM feature, despite the product principle

**Problem.**
- The decided principle is "LLM features on by default". The user said they are "the whole purpose of pg sage" (session-history L177-180).
- G0–G4 define none. §3.9 only restates the rule (L368-369).
- The spec also doesn't say which Guard data may reach the LLM.

**Fix.**
- Add advisory LLM features with deterministic fallbacks:
  - classification proposals (`pii`, `secret`) from names and comments;
  - migration-review narratives on the approval card;
  - posture explanations.
- Add a data-flow rule: no row data from agent-facing tables reaches the LLM; schemas only, with the existing fencing.

#### SR-59 · P1 · §11, §13 R2 — The bench promises outcomes Guard can't produce

**Problem.**
- The bench targets 95% or more of incident replays "prevented or contained", including PocketOS (INC-20).
- PocketOS was a provider token deleting a volume. INC-19, which §11 calls "a mislabeled environment and a destroy", is in the research a stale Terraform state file that pointed `terraform destroy` at the wrong target (incidents-security L549-570). Other dominant paths are ORM resets through the application's own connection string.
- Guard governs SQL on Guard-issued roles only. R2's mitigation is documentation, `client_patterns` won't match ORM CLIs, and AP-13 is info-only.

**Fix.**
- State the expected outcome for each incident: prevented, detected, or out of scope, with the reason. Score against that.
- Once any principal exists, raise unmanaged agent-like logins (AP-01, AP-13) to warning.

#### SR-60 · P1 · §8.1, §8.2 — Interfaces lack an error model and complete shapes

**Problem.**
- There is no error model: which outcomes are JSON-RPC errors (today `-32001`, `-32005` and `-32602` exist), and which are results with `decision: denied`?
- `agent_query`'s output has no `decision` or `reason_code`, so a denial has no shape.
- "Every tool takes `database`" (L1196) is contradicted by `agent_whoami`, `agent_action_status` and `release_sandbox`.
- `estate_list` falls under "`propose` for the rest" (L1177-1178), although it only reads.
- REST endpoints lack request bodies, status codes (403, 404, 409, 422), pagination and the kill report's shape. `PUT /agent-environments/{database}` and the promote body are undefined.

**Fix.**
- Add an error table mapping each reason code to an MCP error code or result, and to a REST status.
- Give every tool and endpoint a complete request and response schema, as the existing MCP tools have `InputSchema`.
- Paginate list endpoints.

#### SR-61 · P1 · §9 — Configuration gaps

**Problem.**
- **Used but undefined:**
  - `memory_growth_gb_day` (AP-12);
  - per-principal rate and budget limits (gate step 9);
  - a ceiling for write `max_rows`: `default_max_rows` exists, but the tool requires `max_rows`;
  - the reconciler tick (G1-08).
- **Inconsistent name:** `observe_interval` (§6.11) vs `observe_interval_minutes` (§9).
- **Undefined precedence:** the principal's `env_ceiling` (DDL default `dev`) vs the profile's `env_ceiling`.
- **Misplaced:** `approval_ttl_minutes` sits under `writes`, but approvals also cover grants and migrations.
- **Unclassified:** the widening keys (`profiles`, `unmask`, `direct_write_environments`, `exposed_roles`). The repo requires every key in `config/key_classes.txt`.

**Fix.**
- Define each key.
- Classify the widening keys as `safety_critical`.
- State "effective ceiling = min(principal, profile)".
- Move the TTL to `agents.approvals.ttl_minutes`.

#### SR-62 · P2 · §6.12 — Posture detector details

**Problem.**
- **AP-10.**
  - "Below 0.8.3 or 0.8.4" isn't testable. The research makes 0.8.4 the floor for the HNSW vacuum fixes.
  - competitive-analysis gives 0.8.7 for a critical IVFFlat build overflow, CVE-2026-103484 (L134, L312-314); technical-substrate lists 0.8.7 only as the latest release (L892). That check should apply whatever indexes exist today, since LangGraph builds them at setup, and its severity should be critical, not warning.
- **AP-06** skips PG14 (G0-03), where views over RLS tables are always owner-privileged. That is the worst case, not a non-case.
- **AP-01.** Its `application_name` patterns are client-controlled and over-match (`agent`).
- **AP-13** needs `pg_read_all_stats` to see `client_addr`.
- **Cost.** Running every detector in every analyzer cycle scans every relation's ACL, which is expensive on 100k-relation catalogs.
- **G0-03 can't be met as written.** It requires a positive fixture for every detector on PG14–18. AP-14's inheritance arm exists only on PG14/15; AP-11's provider arm (RDS deletion protection, provider PITR) can't be reproduced in CI; AP-12 needs growth over time.

**Fix.**
- G0-03 lists per-detector fixture coverage: version-specific arms skip with a recorded reason, and provider arms use recorded `cloudtel` fixtures.
- AP-10: `< 0.8.4` with HNSW present, and `< 0.8.7` regardless.
- AP-06 on PG14: report "no `security_invoker` available".
- Anchor the AP-01 patterns and treat them as hints.
- Run detectors on first look, on catalog change and daily.

#### SR-63 · P2 · §0, §1 — Market and compliance claims overreach the research

**Problem.**
- **Compliance.** "Satisfies SOC 2 CC8.1, ISO 27001 A.8.32 and DORA" (L239-241). The research says those controls *require* recorded, tested, approved changes, and that L3 "should map to" a standard change (enterprise-needs L52-57). Auditor acceptance is unproven.
- **Statistics and vendors.**
  - "Only 49% can revoke": the research says 49% *use* identity disabling or token revocation.
  - "None of the 41 vendors in Gartner's AI SRE guide" is enterprise-needs' own synthesis, not competitive §4.1.
- **Competitors.**
  - "Autonomy ships only inside walled gardens" is contradicted by the research body: DBtune applies changes on Aurora, Cloud SQL, Azure and self-managed Postgres.
  - pgai was archived on 2026-05-27, not "stalled".
  - "No open-source tool acts here" ignores Teleport (AGPL) and Bytebase (MIT core), which each cover parts of G1 and G2.
- **Incidents.**
  - "Every major one combines four failures": the research's own cases contradict universality. Replit restored from backups; the Supabase MCP leak deleted nothing.
  - "Near-weekly" is the author's estimate, and the quarterly series fell to 8 in Q3 2026.
- **Agent share.** Lakebase's 12M launches a day aren't described as agent-created.
- **Unnamed rivals.** The research names pgEdge (closest "on both halves"), postgres.ai, DBtune, Bytebase and Immuta; the spec names none of them.
- **Standard change.** If the L3 = standard-change mapping stays, the research's signer for an envelope is a change manager or CAB (enterprise-needs EN:833-835, 869-871). §6.8 has no such signer.

**Fix.**
- Restate each claim as the research does. Use competitive §3D's precise claim: no product combines any-Postgres, open-source self-hosting, autonomous apply, and verify plus rollback.
- Add a named-rival table, and a change-manager signer for L3 envelopes (or drop the standard-change claim).

#### SR-64 · P2 · passim — Citation and naming errors

**Problem.**
- AU-07 pairs texts with the wrong ids. A-01 is tokens, A-02 the restore gate, A-03 audit.
- "No recorded agent-database user" cites competitive §0.10; the source is prior-specs §0.
- "community §1.7" is credential leakage. Approval fatigue is community-pain §1 item 10 and §2.9.
- O-4 cites a "competitive UNVERIFIED list" that doesn't exist.
- §6.10 states Neon `expires_at` and Lakebase `spec.ttl` as facts. Neon's comes from technical-substrate; Lakebase's comes from pg_sage's own runner, last run live on 2026-05-10.
- "About 45 products" isn't a research figure.
- AU-09 cites DP §11 for the two sets of cloud clients; the source is prior-specs §0.9.
- The "Grant more" onboarding step is about trust level (`onboarding/checklist.go:143-158`).
- AU-10 misses `docs/agent-db-deployments.md:378` and the fixture scripts under `test-fixtures/` (cross-check). G0-06's check must compare hashes, not embed the password strings.
- §1.3 says an agent can be "fully autonomous on its own throwaway branch" while L4 is never granted. Say "L3 on its own branch". The research's M4 is "autonomous within budget", not "never granted".
- `config.go:1038` should name `internal/config/config.go`.
- §2.2 keeps "creation receipt written *before* the provider call" as a proven idea. Per the deploy-paths audit (DP-24), only the operation id and `live_mode` are written first, in two non-atomic statements (`live_create.go:100-109`, cross-check). §6.10 should require a single atomic receipt insert, not cite the old code as the model.
- §2.5 never mentions D4b (an approved request before a cloud register, shipped in v1.7.0), whose subject G0 removes. Record it as superseded.
- §6.10 gives the `template` adapter's speed as "seconds to minutes". technical-substrate marks `FILE_COPY` with `file_copy_method = clone` timings UNVERIFIED, "benchmark before advertising" (L1171-1172).
- §1.2 item 3 says CPU and I/O can't be capped per role. The research's row 9 also lists memory (only per-node `work_mem` exists).

**Fix.** Correct each.

#### SR-65 · P2 · §10, §14 — Dependencies treated as settled

**Problem.**
- G0-04 depends on PR #130. Its branch, `claude/firstlook-own-timeout`, is not merged into `72646ab1`, and at master a lower session `statement_timeout` wins (`firstlook/runner.go:118-127`).
- G4 depends on v2.3's `history.store: meta` and fleet fingerprints, both open PRs.
- E4's leader election duplicates the one planned in v2.3's fleet-learning track (`reviews/2026-10-02-ai-next/ROADMAP.md:156-157`). Guard's reconcilers race with two replicas until then (CG-07).
- Version numbers are tied to gates that may pause.
- R3 promises "the G1 live matrix on RDS, Cloud SQL and Azure" for managed-service privileges (O-1, O-2), but no G1 check requires it. It also omits Supabase and Neon, where agents create most databases (spec L25-27). The research covers these platforms' role management, pgAudit and event triggers only partly (incidents cross-check, T7(q)).

**Fix.**
- Add **G1-13:** live receipts per platform (RDS, Aurora, Cloud SQL, Azure Flexible, Supabase, Neon) for:
  - `CREATE ROLE` and `ALTER ROLE … SET` (each knob);
  - `GRANT … WITH GRANT OPTION`;
  - terminating agent backends;
  - reading `pg_control_system()`.
  Committed scrubbed, like G3-09.
- State the fallback if #130 doesn't land.
- Reuse v2.3's lease for Guard's reconcilers from G1 on.
- Name the releases G0–G4, and assign version numbers when each is cut.

#### SR-66 · P2 · §5.1, §6.3 — Clocks, poolers, attribution limits, replicas, and other cheap wins

**Problem.**
- **Clocks.** Expiries mix Go time with the database's `now()`: `CHECK (expires_at > granted_at)` with `granted_at DEFAULT now()`. `VALID UNTIL` uses the target's clock.
- **Poolers.** Through PgBouncer in transaction mode, pgx's cached named statements need `max_prepared_statements` (PgBouncer 1.21 or later), or unnamed statements.
- **Attribution.** G1-10 relies on `pg_stat_statements` rows keyed by `userid`. One role per agent multiplies entries per query shape, and entries are evicted at `pg_stat_statements.max` (default 5000), so attribution silently drops when `pg_stat_statements_info.dealloc` grows. The extension also needs `shared_preload_libraries`, which not every estate has.
- **Missed cheap wins** that the community research names:
  - a replica lane for read-heavy agents;
  - `default_transaction_read_only` on read-only login roles;
  - sqlcommenter tags for attribution.

**Fix.**
- Compute expiries in SQL, on the database that enforces them.
- Use `QueryExecModeExec` for broker connections.
- Add a replica option for `agent_query`.
- Set `default_transaction_read_only = on` on read-only login roles, as a hint rather than a boundary.
- Accept sqlcommenter tags into attribution.
- Make G1-10 report dropped attribution when `dealloc` advances, and fall back to brokered audit rows.

#### SR-67 · P2 · §10.7, §4 — The enterprise track's order differs from the research's priorities

**Problem.** The enterprise cross-check found the E1–E4 order at odds with enterprise-needs:
- **Late must-haves.** Group mapping and secrets-manager delivery (E3, v2.6) rank above agent OAuth (E2, v2.5) for today's buyers (EN:917, 921). pg_sage reading its *own* credentials from a secrets manager isn't planned at all.
- **No deliverable for database-side audit.** pgAudit appears only as posture (AP-11) and canary input, with no correlation of pgAudit records to action ids (AU-2, EN:463).
- **Signing late.** Image signing and an SBOM arrive in E4 (v2.8), after production writes (G2, v2.6). The research lists them as security-review evidence (EN:649-650).
- **No declarative configuration.** The spec names platform engineers the primary buyer (L377). The research says platform engineering vetoes tools it can't manage as code (EN:612-613, 929), yet principals, profiles and envelopes are API and UI objects.

**Fix.**
- Move group mapping and pg_sage's own secrets-manager reads to E1.
- Add an E2 item correlating pgAudit with action ids (for example through `application_name`).
- Move signing and the SBOM before G2 writes.
- Add a declarative, versioned file for principals, profiles and envelopes, with plan and apply semantics.

#### SR-68 · P1 · Appendix A, §10 — Traceability gaps: dropped requirements, detection standing in for prevention, weak checks

**Problem.** From the incidents cross-check of incidents-security §4.2 (48 SAFE-* requirements) against Appendix A:
- **Dropped without a scope note.** These are absent from Appendix A, unlike SAFE-VER-01/02, which are explicitly scoped out:
  - SAFE-NET-03: an egress allowlist for the agent runtime, with cloud metadata blocked;
  - SAFE-NET-04: production reachable only through the gateway;
  - SAFE-TOOL-08: agent rules and prompts are code the agent can't change.
- **Detection mapped to prevention requirements.**
  - SAFE-ID-05 (tenant binding in the data layer, tested by a cross-tenant zero-rows check) maps to G1-01 and an anomaly rule.
  - SAFE-PERM-06 (RLS enabled and forced, plus a two-user isolation test re-run on every change) maps to posture detectors capped at L1. The two-user test is absent.
  - SAFE-BAK-01 (backups immutable or delete-delayed by 48 h or more, in a separate account) maps to AP-11's deletion-protection flag and docs.
- **Missing halves.**
  - SAFE-TOOL-07 asks to pin MCP tool definitions by hash and re-approve on change; only the invisible-character half is specified.
  - SAFE-HUM-05 asks for rate-limited approval requests; no check covers it.
- **A weak check.** G1-01 asserts only `rolsuper = false` and `rolbypassrls = false`. SAFE-PERM-01 also excludes membership in `pg_read_server_files`, `pg_write_server_files`, `pg_execute_server_program` and `pg_write_all_data`, and nothing checks pg_sage's own role at startup.
- **Out of scope, though it motivates the spec.** SAFE-VER-01/02 (evidence preconditions, competing hypotheses) are "out of Guard scope" (L1582), yet they cover DBA-Bench's "missing safeguards" class, which §1.1.4 leans on.
- **Enterprise MUSTs untraced.** Appendix A traces only the incidents file. No enterprise-needs MUST (ID, AZ, AP, AU, DP, SF, FO, DR, CM) appears (enterprise cross-check).

**Fix.**
- Give every SAFE-* id a row with a check id, or "out of scope because …". NET-03/04 are plausibly out of scope, since pg_sage doesn't host the agent. TOOL-08 is in scope: agents must never edit profiles, policy or prompts.
- Add checks for the prevention requirements:
  - a two-user RLS isolation test in posture and the bench;
  - a cross-tenant zero-rows check for SAFE-ID-05;
  - an immutability or object-lock posture check for backups.
- Add tool-definition hashing for MCP, and an approval-request rate limit.
- Extend G1-01 to the predefined server-file roles, and add a startup self-check on pg_sage's own role.
- Add enterprise MUST rows, with check ids or "not covered".

---

## 5. Notes on what I could not verify

- The legality of a self-grant by the ADMIN holder on every PG16.x patch release (SR-13).
- Whether Neon branches, Aurora clones and Cloud SQL clones share the source's `system_identifier` (SR-09). Physical copies and standbys do.
- Whether `pg_stat_statements` survives compute restarts on each scale-to-zero provider (SR-48).
- The number of human users on lifeos (SR-31).
- `docs/agent-db-deployments.md:350` as a password line (§2).
- Provider API field names for Neon, Supabase and Lakebase (SR-48, SR-64). The spec already flags these.
- Whether every SRE MCP tool path queues for a human before executing (SR-03). I verified the intent tools.
- Lakebase's point-in-time target type (SR-45). The RDS, Aurora and Cloud SQL restore APIs take timestamps.
- Whether `pg_stat_statements` attributes brokered statements to the role set by `SET ROLE`. I believe it records the current user, so brokered statements count against the broker role; neither research file settles it (SR-66).
