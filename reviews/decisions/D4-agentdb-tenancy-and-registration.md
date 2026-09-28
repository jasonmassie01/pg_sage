# D4 — AgentDB human-operator tenancy (D4a) and approved-request-before-cloud-register (D4b)

**Current state:** agents are tenant-bound, but every human admin/operator acts on every tenant.
`POST /api/v1/agent-dbs` registers cloud deployments without citing any request, and the UI never consumes
the approval it just obtained.
**Recommendation:** D4a: keep humans global and say so as a product statement. Close the audit gap now
(approver identity) and design additive per-user tenant grants for later. D4b: require an approved,
unconsumed request for cloud instance registers, routed through the existing
`/requests/{id}/provision`.
**Door type:** both are two-way doors. D4a Option B (grants) is additive, with "no grants = global". D4b is
an API contract tightening that can be relaxed, and it leaves existing rows alone.

Evidence was verified on master `f99a302` (worktree `pg_sage-decisions`). All paths are under `sidecar/`
unless noted.

## 1. What the code does today

**Human principals.**
- `sage.users` has `id, email, password, role, created_at, last_login` and no tenant column
  (`internal/schema/bootstrap.go:560-567`).
- Roles are `admin|operator|viewer` (`internal/auth/types.go:21-35`).
- Sessions carry only `user_id` (`internal/auth/types.go:15-19`).

**AgentDB routing.**
- Every management route is wrapped in `RequireRole("admin","operator")` with no tenant predicate
  (`internal/api/agent_db_handlers.go:24-38`).
- Admin-only sub-routes are provider-config upsert (`:95-98`), agent-token mint (`:142-146`), restore
  drill (`:241-245`) and `authorize-live` (`:254-257`).
- The operator list accepts `tenant_id` only as an optional *filter* (`internal/api/agent_db_list.go:13`).
- Human-created requests take `tenant_id`/`owner_id` from the body
  (`internal/api/agent_db_request_handlers.go:20,22`). Identities do the same
  (`internal/api/agent_db_identity_handlers.go:26`).
- The model is documented as intentional in the code comment at `internal/api/agent_db_agent_api.go:11-20`
  and in the docs at `docs/agent-db-deployments.md:71-77` (repo root): "global operators ... may act on
  any tenant".

**Agent principals (the P0-19 fix).**
- `/api/v1/agent-api/` routes use `requireAgentPrincipal` (`internal/api/agent_db_agent_api.go:36-70`).
- The tenant is forced from the token on list (`:80`), get (`GetForTenant`, `:93`) and create-request
  (`:113`).
- Tests: `internal/api/agent_db_tenant_fix_test.go:77,98,116,148,172`.

**Tenant data model.**
- `tenant_id text NOT NULL` is on `agent_identities`, `agent_db_requests` and `agent_db_deployments`
  (`internal/agentdb/schema_statements.go:5-16,17-36,46-63`).
- `idx_agent_db_deployments_tenant_status` exists (`:106-107`).

**Approval audit.**
- The approve and deny handlers pass only `Decision` and `Reason`
  (`internal/api/agent_db_request_handlers.go:96-119`).
- `DecisionRequest` has no actor field (`internal/agentdb/types.go:91-94`).
- `SetRequestDecision` writes status, policy and reasons only (`internal/agentdb/store.go:81-114`).
- **Doc/code mismatch:** the docs claim `approved_by` is taken from the signed-in user
  (`docs/agent-db-deployments.md:74-77`), but request approvals record no approver at all.

**Register path (D4b).**
- `agentDBRegisterHandler` builds a `RegisterRequest` purely from the body, including tenant, agent,
  provider, size profile and budget. It has no `request_id` (`internal/api/agent_db_handlers.go:419-450`).
- `Store.Provision` builds a plan for cloud `instance` level and calls `Register` with status `planned`
  (`internal/agentdb/schema.go:94-125`).
- The approved-request path instead checks `status='approved' AND policy_decision='allow'`, consumes the
  request atomically, then calls the same `Provision` (`internal/agentdb/provision_links.go:148-184,
  269-281`).
- That path cannot carry `size_profile_id`, `schema_name` or `secret_ref`:
  - `RequestProvisionRequest` has only `DeploymentID, LeaseSeconds, Metadata, ProviderParams`
    (`internal/agentdb/types.go:392-397`).
  - The handler reads only those fields (`internal/api/agent_db_request_handlers.go:121-139`).
- **The UI flow:**
  - `AgentDBsPage` creates a request (`web/src/pages/AgentDBsPage.jsx:232-233`) and stops unless the
    request is `approved` (`:234-238`).
  - It then calls `POST /api/v1/agent-dbs` directly with the form's fields (`:240-256`). The approval is
    never linked or consumed.
- **Request policy only runs at request creation.** `DecideRequest` covers these checks
  (`internal/agentdb/policy.go:12-43`):
  - tenant and agent required;
  - region vs `AllowedRegions`;
  - masking for sensitive data classifications;
  - review for cloud instances with `budget_usd <= 0`.
  - A cloud instance with a positive budget is auto-`approved` (`:32-35`).
- **What still guards live spend regardless of path:**
  - `authorize-live` is admin-only (`agent_db_handlers.go:254-257`).
  - `EvaluateLiveProvisionPolicy` enforces the global/provider enablement, TTL, public IP, region and
    account allow-lists (`internal/agentdb/provider_policy.go:45-70`).
  - The budget is clamped to the global/provider ceiling (`internal/api/agent_db_live_authority.go:115-156`).

## 2. Concrete risk today

**D4a.**
- Any `operator` can do all of the following for any tenant's deployments with no tenant check
  (`agent_db_handlers.go:24-38,176-260`):
  - list and read them;
  - approve or deny requests;
  - mint ping tokens;
  - run dry-run provision execute, lease extension, backups and cleanup.
- Impact depends on deployment shape:
  - **Single platform team** (humans = the operators of one install): the behaviour is as designed, and
    the risk is audit only.
  - **Several customer teams sharing one sidecar:** one team's operator can approve, extend or clean up
    another team's databases.
- The audit gap is real in both shapes. A request approval leaves no record of *which* human approved it
  (`store.go:93-102`), so four-eyes review is not demonstrable.

**D4b.**
- An operator can create a planned cloud deployment for any tenant/agent while skipping request policy
  (`agent_db_handlers.go:419-450`). Skipped checks:
  - the data-classification masking rule;
  - the review requirement;
  - the tenant-scoped idempotency (`schema_statements.go:43-45`).
- Approvals are not bound to what gets registered:
  - The UI's approved request says nothing about the size profile or provider params that are then
    registered (`AgentDBsPage.jsx:240-256`).
  - The request stays unconsumed and reusable.
- **Bounded:** actually creating cloud resources still needs an admin `authorize-live` plus the live
  policy's region, TTL and cost ceilings (above). This is a *policy-bypass and traceability* risk. It is
  not a direct spend-bypass risk.

## 3. Options

**D4a — human tenant scoping**

| Option | What | Pros | Cons | Door |
|---|---|---|---|---|
| A. Global operators, explicit (recommended now) | Keep the model. Record the approver/denier from the session. Add a "single operator team per install" product statement. | Small. Matches the self-hosted sidecar deployment. Fixes the audit gap. | Not suitable for sharing one install across customer teams. | Two-way |
| B. Additive tenant grants (recommended as the designed next step) | New `sage.user_tenant_grants(user_id, tenant_id)`. A user with **no** rows stays global (backward compatible). A user with rows is restricted. Admin is always global. | Backward compatible. Opt-in per user. | Every store query needs a `TenantScope` (group-08 §3 "L"). The UI needs a tenant picker. | Two-way (drop grants = today) |
| C. Mandatory tenant per user | `users.tenant_id NOT NULL`. Every human is single-tenant. | Strongest isolation. | Breaks existing installs and needs a backfill. Admins need a special case. | **One-way** (stored semantics and backfill) |

**D4b — approved request before cloud register**

| Option | What | Door |
|---|---|---|
| A. Status quo, documented | Keep direct register. Rely on admin `authorize-live` as the only gate. | Two-way |
| B. Require an approved request for cloud instances (recommended) | `POST /agent-dbs` with a cloud provider and `instance` level requires `request_id`. The request must be `approved`, `allow`, unconsumed, and match tenant/agent/provider. Consume it atomically. `local_postgres` schema/database registers are unchanged. | Two-way (a flag or relaxation restores today) |
| C. Admin bypass with reason | Option B, plus admins may register without a request if they give a `reason` recorded in the audit. | Two-way. Add later only if a real operator flow needs it. |

## 4. Implementation sketch

**D4a-A**
- Add `ActorID` to `DecisionRequest` (`internal/agentdb/types.go:91`).
- Pass `authenticatedActor(r)` from the approve and deny handlers (`agent_db_request_handlers.go:96-119`).
- Persist `decided_by` and `decided_at`: add `ALTER TABLE sage.agent_db_requests ADD COLUMN IF NOT EXISTS
  decided_by text NOT NULL DEFAULT ''` in `schema_statements.go`.
- Write an audit row.
- Update `docs/agent-db-deployments.md:71-77` to state the single-team model and link this memo.

**D4a-B (later)**
- Add the grants table.
- Add `TenantScope{Global bool; Tenants []string}`, resolved once per request in middleware from the
  session user.
- Thread it through `ListPage`, `Get`, `RequestsForTenant` and the subrouter's per-deployment branch
  (`agent_db_handlers.go:176`). Reuse the `GetForTenant` pattern (`agent_db_agent_api.go:93`).

**D4b-B**
- Extend `RequestProvisionRequest` with `SizeProfileID`, `SchemaName`, `SecretRef`, `SecretRefProvider`
  (`types.go:392`). Map them in `ProvisionApprovedRequest` (`provision_links.go:164-182`), and in the
  handler (`agent_db_request_handlers.go:127-132`).
- In `agentDBRegisterHandler`, reject cloud `instance` registers (HTTP 409 `approved request required`).
- Point clients at `POST /agent-dbs/requests/{id}/provision`.
- Change `AgentDBsPage.jsx:240-256` to call `/api/v1/agent-dbs/requests/${request.request_id}/provision`
  with the same fields.
- Leave existing rows alone: no backfill, and no invalidation of deployments registered before the change.
- Update `docs/agent-db-deployments.md` register examples.

## 5. Test plan (write these first; each must fail today)

**D4a-A**
- `TestRequestApprovalRecordsSessionActor`: approve as `op@x`; the `decided_by` column equals `op@x`
  (today there is no column or value).
- `TestRequestDenialRecordsSessionActor`: the same for deny.
- `TestRequestApprovalIgnoresBodyApprovedBy`: a body `approved_by: "someone"` is ignored.

**D4a-B (when built)**
- `TestOperatorWithTenantGrantCannotReadOtherTenant`: operator granted `t1`; GET of a `t2` deployment
  returns 404.
- `TestOperatorWithoutGrantsRemainsGlobal`: guards backward compatibility.
- `TestAdminIgnoresGrants`.
- `TestTenantScopedOperatorCannotApproveOtherTenantRequest`.

**D4b-B (API, `internal/api`)**
- `TestRegisterCloudInstanceWithoutRequestRejected`: `provider=aws_rds, provisioning_level=instance`, no
  `request_id`, returns 409. Today it returns 200 with status `planned`.
- `TestRegisterLocalSchemaWithoutRequestStillAllowed`: regression guard.
- `TestProvisionApprovedRequestCarriesSizeProfileAndSecretRef`: the deployment's `size_profile_id` and
  `secret_ref` equal the body values. Today they are dropped.
- `TestProvisionApprovedRequestTwiceWithDifferentIDConflicts`: exists in spirit as
  `TestApprovedRequestIsSingleUse`. Extend it to the cloud path.
- `TestProvisionReviewRequestRejected`: a request in `requested`/`review` status returns ErrInvalid.

**Web (Vitest)**
- `AgentDBsPage provisions through the approved request`: after an approved request, the next POST URL is
  `/api/v1/agent-dbs/requests/{id}/provision` and not `/api/v1/agent-dbs`.

## 6. Open question for the owner

Is one pg_sage install ever shared by several customer teams (SaaS)? `reviews/2026-09-26/group-08-agentdb.md:458-459`
asks the same thing. If yes, schedule D4a-B before any multi-team pilot. If no, Option A closes P0-19 for
humans.
