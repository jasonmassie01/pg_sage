# D1 — Enforce `RefusalSet` / `LockDurationCeilingMS` / `SerializeMode` (G4-B17 remainder)

**Current state:** all three fields are parsed, round-tripped and (partly) validated, but no code enforces them. Every stored policy carries the full refusal set.
**Recommendation:** enforce all three with *precise* definitions. Refusal tokens become typed predicates over the contract (RollbackClass, drop kind, owner declaration). The self-initiated path gets `queue_approval`. The net effect on today's action catalog is zero. The lock ceiling caps `lock_timeout` on in-transaction DDL. `park` becomes a real, non-failing park.
**Door:** RefusalSet is **one-way** (it changes the meaning of every stored policy), but a behaviour-preserving definition limits the impact and needs no row migration. The lock ceiling and serialize mode are **two-way**.

Evidence was read at HEAD of this worktree on 2026-09-27. Line numbers are `sidecar/…` unless noted.

## 1. What the code does today

| Field | Parsed / stored | Validated | Enforced |
|---|---|---|---|
| `RefusalSet []string` | `internal/policy/document.go:83,97,141,218` | **No.** Any string is accepted, and `ValidateDocument` (`document.go:224-247`) never looks at it | **No consumer** (grep: only `document.go` and tests) |
| `LockDurationCeilingMS int64` | `document.go:78,92,132,213` | `>= 0` only (`document.go:231-232`) | **No consumer** |
| `SerializeMode string` | `document.go:85,99,143,220` | must be `park` or `queue` (`document.go:228-229`) | **No consumer** |

- Both shipped profiles inherit `baseProfile()` (`document.go:329-345`): `LockDurationCeilingMS: 3000`, `SerializeMode: "park"`, and the RefusalSet `rls_change, grant_expansion, major_upgrade, non_dup_object_drop, unrollbackable` (`:338-341`).
- `Store.Bootstrap` writes that document once and then always returns the stored row (`internal/policy/store.go:20-49`). Every existing install therefore stores the same five tokens, and later profile edits do not reach it.
- The gate never reads the fields. `documentDecision` checks the change class, then approval, then usage limits (`internal/policy/gate.go:94-121`), and `operatorDecision` checks trust, the class and windows (`internal/policy/gate_operator.go:9-33`).
- `policy.ActionContract` has no `RollbackClass` (`internal/policy/types.go:76-83`). `policyContract()` drops it when it converts the executor contract (`internal/executor/executor.go:346-357`).
- Editors: `POST /api/v1/policy/proposals` (operator/admin) and `/ratify` (admin) (`internal/api/policy_handlers.go:46-71`). Proposals pass `ParseDocument` (`internal/policy/store_queries.go:147-153`). MCP `propose_policy_change` goes through `internal/mcp/postgres_access.go:201` and `server.go:82`.
- The web UI has **no** policy editor: nothing under `web/src` references `refusal_set`, `serialize_mode` or `lock_duration_ceiling_ms`. `docs/` doesn't document these fields. Their only source is `specs/agent-native-autonomy-build-spec.md:115,243-255`.
- Design intent: the refusal set covers "classes the agent will *never* auto-do … parked, never applied" (`research/2026-07-22-agent-native-feature-spec.md:502`). It exists for the no-reviewer posture (`:151`), and slot drop is the canonical example (`:409-411`).

## 2. RefusalSet: literal vs precise mapping onto the action catalog

Executor action types come from `actionTypeForProposalSQL` (`internal/executor/executor.go:643-678`). Contracts are defined in `internal/executor/action_contract.go:77-260` and the helpers below it.

| Token | Literal reading hits | Today's outcome for those | Precise predicate (proposed) | Net change |
|---|---|---|---|---|
| `rls_change` | Nothing reachable. `ALTER TABLE` is limited to `SET (`/`RESET (` plus 5 migration regexes (`internal/executor/validate.go:73-92,266-291`). `CREATE POLICY` is not allow-listed (`validate.go:16-29`) | park `no_typed_contract` (`gate.go:177-184`) | Statement class ∈ {ENABLE/DISABLE/FORCE RLS, CREATE/ALTER/DROP POLICY} | none (defense in depth) |
| `grant_expansion` | Nothing reachable. GRANT, REVOKE, ALTER ROLE and OWNER TO are not allow-listed (`validate.go:16-29`) | same | Statement class ∈ {GRANT, GRANT ROLE, ALTER ROLE, OWNER TO, ALTER DEFAULT PRIVILEGES} | none |
| `major_upgrade` | No contract exists. `ALTER EXTENSION` is not allow-listed | same | Action type ∈ future {`major_upgrade`, `extension_update`} | none |
| `non_dup_object_drop` | `drop_unused_index` (the same action type covers unused `rules_index.go:141-152`, invalid `:212-221` and subset-duplicate `:395-412` drops), `revert_created_index` (`executor/contract_revert.go:9`), and `alter_table … DROP CONSTRAINT` (`validate.go:83`) | unused/invalid drops auto-run after autonomous + tier3_moderate + 31-day ramp (`action_contract.go:163-189`, guardrail `:178`). Revert is pg_sage's own rollback | Drop of a **non-derivable** object (table, column, constraint, sequence, schema, replication slot). Indexes are derivable: each drop finding carries `RollbackSQL = IndexDef` (`internal/analyzer/rules_index.go:151,220,291,412`) | DROP CONSTRAINT is already high + approval (`action_contract.go:191-215`) → none |
| `unrollbackable` | By RollbackClass: `not_reversible` covers cancel/terminate (`action_contract.go:473,509`) and `retention_delete` (`executor/retention_authorization.go:64`). `forward_fix_only` covers `alter_table`, `ddl_preflight`, sequence migration, bloat plan (`:213,248,443,652`) and **`set_table_autovacuum`** (`:610`). The broadest reading adds `analyze`/`vacuum` (`no_rollback_needed`, `:65,569`) | `set_table_autovacuum` and `retention_delete` auto-run after the ramp. The rest are already approval-only (backend signal `gate.go:108-111,125-127`, high tier `gate.go:358-360`) | RollbackClass ∈ {`not_reversible`, `forward_fix_only`} **and** no prior owner/human authority | none, after 2 corrections below |

The literal reading bans the unused-index drop, the invalid-index drop, pg_sage's own index revert, per-table autovacuum tuning and bounded retention. Read as "has no rollback", it also bans VACUUM and ANALYZE. The spec's warning (MASTER-SPEC §10.5) is correct.

Two corrections make the precise reading behaviour-neutral:
1. **`set_table_autovacuum` is misclassified.** The analyzer ships a rollback, `ALTER TABLE … RESET (autovacuum_vacuum_scale_factor)` (`internal/analyzer/rules_autovacuum_tuning.go:82-83`), and the contract's precheck captures the prior reloptions (`action_contract.go:583-613`). Reclassify it to `reversible`, with rollback restoring the captured prior value rather than a bare RESET.
2. **`retention_delete` runs under owner authority.** It requires an explicit owner retention contract and a dry run at least 24 h old (`internal/autonomy/retention_postgres.go:20-23,74,133-137`). The owner's declaration is the authority, which matches the ledger lens. Exempt requests that carry an owner declaration. This depends on D5, the retention column.

The verdict is `queue_approval` (reason `refusal_set`) for self-initiated requests. An operator-approved request is not refused, because a human decided and the gate already treats that path separately (`gate.go:45-47`). Explain/readiness shows the same reason (`gate.go:27-29`).

## 3. LockDurationCeilingMS: available plumbing

- Every executor path already sets `lock_timeout` from `safety.lock_timeout_ms`, default **30000** (`internal/config/defaults.go:55`, `config.go:503-507`). The sites are:
  - `ExecInTransaction` `SET LOCAL lock_timeout` (`internal/executor/ddl.go:68-121`, `:101-108`)
  - top-level/CONCURRENTLY `SET lock_timeout` (`internal/executor/session_scope.go:40-61`)
  - callers: `executor.go:770`, `custodian.go:294`, `manual.go:124`, `index_verification_runtime.go:142-145`, rollback `rollback.go:141`, `analyze.go:96`
  - retention hard-codes `2s` (`internal/autonomy/retention_postgres.go:27,187`)
- The ceiling (3000) is 10× tighter than the safety default, and nothing connects the two.
- Semantics: `lock_timeout` bounds the **wait to acquire** a lock, not how long it is held. The case DDL-preflight "lock_timeout" check is only a report (`internal/cases/ddl_safety.go:44-45,88-89`).
- Precise reading: the effective `lock_timeout` = `min(policy ceiling, safety.lock_timeout_ms)` on the **in-transaction** path. That path runs `ALTER TABLE` forms that can queue an ACCESS EXCLUSIVE request and stall all traffic behind them.
- CONCURRENTLY paths keep the safety value. CIC/RIC wait on older transactions, and a 3 s cap there would mostly produce failed builds (INVALID leftovers). Confirm this with an integration test (§6 T7) before extending the ceiling to them.

## 4. SerializeMode: what "park" means today vs the spec

- Spec: an overlapping lease → the later request is parked with `ddl_conflict`, or queued when `serialize_mode=queue` (`specs/agent-native-autonomy-build-spec.md:115`).
- Code: `acquireDDLLease` is called only from `executeFinding` (`executor.go:746-752,886-911`). It uses non-blocking `pg_try_advisory_lock` (`internal/policy/postgres_lease.go:106-113`). On conflict, the executor logs an **action_log row with outcome `failed`** (`executor.go:748`, `actionOutcome` `:1232-1236`).
- Consequence: lease conflicts count toward `exceedsMaxRetries`, which counts `failed` rows and then abandons the finding (`executor.go:1040-1065`). They also count toward the self-initiated rate/blast usage (`internal/executor/policy_runtime.go:96-107`). The current behaviour is therefore "fail, and eventually give up", which is neither park nor queue.
- Scope gap: ExecuteManual, custodians, rollbacks and MCP intents take no lease (single call site). This is tracked separately as G4-B29.

## 5. Options

| Option | What | Door | Verdict |
|---|---|---|---|
| A. Delete the three fields | Remove them from the struct and the wire format | **One-way**: `decodeStrict` uses `DisallowUnknownFields` (`document.go:155-165`), so every stored row would fail to parse → gate `policy_unavailable` fail-closed (`gate.go:97-99`) unless rewritten. It also discards the spec intent | Reject |
| B. Enforce literally | Token = action type/rollback class as written | **One-way**: it immediately bans five autonomous actions on every install (§2) | Reject |
| **C. Enforce precisely** | §2 predicates, §3 min() on in-tx DDL, §4 true park + bounded queue | RefusalSet one-way but behaviour-neutral. Lock/serialize two-way | **Recommend** |

C matches the ledger lens: it enforces the intent precisely instead of banning broadly, and the refusal set becomes binding for future unrollbackable features such as slot drop.

## 6. Implementation sketch
1. `internal/policy/types.go`: add `RollbackClass`, `DropKind` (`derivable|non_derivable|""`) and `OwnerDeclared bool` to `ActionContract`. Add `ReasonRefusalSet` and `ReasonDDLConflict`.
2. `internal/policy/refusal.go` (new, <100 lines): a known-token table plus `refused(doc, req) (token string, bool)`.
3. `internal/policy/document.go`: `ValidateDocument` rejects unknown tokens. Stored rows are only the five known tokens unless someone used the API, so add a startup scan that logs any row that would now fail.
4. `internal/policy/gate.go` `documentDecision`: after the class check, a refusal returns `queue_approval`. Leave `operatorDecision` unchanged.
5. `internal/executor/executor.go:346-357` copies RollbackClass. Construction sites without a contract must set the class explicitly:
   - `internal/mcp/intent_adapters.go:22`
   - `internal/mcp/production_intent_executor.go:206,241`
   - `internal/migration/runtime/orchestrator.go:150`

   `online_migration` = `forward_fix_only`, which is already approval-required in both profiles (`document.go:333`).
6. `action_contract.go:610`: `set_table_autovacuum` → `reversible` with captured-prior rollback. `retention_authorization.go:31-39`: set `OwnerDeclared`.
7. Lock: expose `Document.EffectiveLockTimeout(safetyMs)`. Pass it through `ActionPolicyDecision` into the in-tx `WithLockTimeout` at `executor.go:770` and `manual.go:124`.
8. Serialize: on `ErrLeaseConflict`, record a gate decision with verdict park and reason `ddl_conflict`. Do not write an action_log row. For `queue`, make a blocking `pg_advisory_lock` attempt under `SET lock_timeout = ceiling`, then park.

## 7. Test plan (write first; each must fail on HEAD)
- T1 `policy/refusal_test.go` table test: for every `ContractForActionType` key, the precise predicate refuses **none** of today's autonomous actions. It asserts the exact list `{analyze_table, vacuum_table, create_index_concurrently, drop_unused_index, reindex_concurrently, set_table_autovacuum, alter_*_guc, retention_delete(owner)}` is not refused.
- T2 Literal-ban regression: a synthetic `DropKind=non_derivable` moderate request with `non_dup_object_drop` → `queue_approval/refusal_set`. The same request with `OperatorApproved` → execute.
- T3 A `not_reversible` request without `OwnerDeclared` → `queue_approval`. `retention_delete` with owner declaration at autonomous + ramp → execute.
- T4 `ValidateDocument` rejects `refusal_set:["drop_everything"]`. Both shipped profiles still validate.
- T5 `policyContract` preserves RollbackClass. It fails today, because `executor.go:346-357` drops the field.
- T6 Lock: an in-tx DDL under a policy ceiling of 3000 and safety 30000 issues `SET LOCAL lock_timeout = '3000ms'` (fake pool / pgx recorder).
- T7 Integration (Postgres): hold ACCESS SHARE in session A. `ALTER TABLE … SET (fillfactor=…)` through the executor fails with 55P03 in ≤ 3.5 s. Separately, record CIC behaviour under a 3 s cap before any extension.
- T8 Serialize: two `executeFinding` calls on the same target. The second writes **no** `failed` action_log row and a `park/ddl_conflict` decision, and `exceedsMaxRetries` stays 0. It fails today (`executor.go:748`).
- T9 `queue` mode: the second caller waits until the first releases (within the ceiling) and then executes.

**Open question for jmass:** should an unused-index drop also count as a `non_dup_object_drop`, the strict variant? The change is one predicate line. It would send every unused/invalid index drop to approval, overriding the 31-day earned-autonomy guardrail (`action_contract.go:178`).
