# Group 06 — REST API, Auth, MCP (excluding agent_db_* handlers)

Reviewer: Claude (lead, written directly after the delegated reviewer's file write was
blocked twice). Worktree `C:/Users/jmass/pg_sage-claude-review` @ `b396595`.
Delegated reviewer ran `go build`, `go vet` (clean) and unit tests without a DB
(934 pass / 0 fail / 245 DB-skipped); coverage api 40.1%, auth 47.9%, mcp 58.4%,
sanitize 71.4%. Items below were re-verified by the lead by reading code unless noted.
Codex cross-references (`codex/surface-audit.md`) are given as `SURF-nn`.

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G6-B01 | P0 | CONFIRMED | `internal/explain/explain.go:222-226` | `/explain` parameterized path sends `PREPARE … AS <user query>` over the simple protocol, so a multi-statement body leaves the READ ONLY transaction |
| G6-B02 | P1 | CONFIRMED | `internal/api/router.go:156-159` | HTTP MCP mounted without `RequireRole`; viewer sessions reach mutating intents (= SURF-01) |
| G6-B03 | P1 | CONFIRMED | `middleware.go:105-131`, `executor/manual.go:79-90` | Approve/execute DDL uses the 30 s request context; long `CREATE INDEX CONCURRENTLY` is cancelled → INVALID index, `action_log` insert on the dead ctx fails |
| G6-B04 | P1 | CONFIRMED | `auth/oauth.go:329-341`, `auth/auth.go:361-375` | OIDC userinfo email accepted without `email_verified`; matched to existing (possibly admin/password) user by email only (= SURF-02) |
| G6-B05 | P1 | CONFIRMED | `api/handlers.go:1799-1861,2050-2056` + `executor/manual.go:121-150` | Actions ledger merges `action_queue` rows (queue id, `outcome=pending`) with `action_log` rows; UI shows Rollback on queued rows and the API resolves the id against `action_log` → rollback SQL of an unrelated action (see G9-B01) |
| G6-B06 | P2 | CONFIRMED (Codex) | `api/auth_handlers.go:117-129` | Login limiter map bypasses its 10k-entry bound via the allow path; allow→auth→record not atomic (= SURF-16) |
| G6-B07 | P2 | CONFIRMED (Codex) | `api/agent_db_blueprint_handlers.go:39,63`, `agent_db_terraform_template_handlers.go:42,65` | Approval actor strings taken from request JSON (= SURF-17) |
| G6-B08 | P2 | CONFIRMED | `api/handlers_v09.go:419-436` | Incident resolve discards `reason`; next RCA persist cycle re-opens the incident (= SURF-19 + R04) |
| G6-B09 | P2 | CONFIRMED (Codex) | `api/cases_handlers.go:643` | queryid converted through float64 — precision loss above 2^53 (= SURF-09) |
| G6-B10 | P2 | CONFIRMED (Codex) | `api/cases_handlers.go:486-502` | Cases label any `outcome=success` as `verified` (= SURF-12) |
| G6-B11 | P3 | CONFIRMED | `auth/oauth.go:340` | Logs the authenticated user's email at INFO (PII in logs; CLAUDE.md §8) |
| G6-B12 | P3 | CONFIRMED | `api/router.go:156-163` + `api/handlers*.go` | No per-database authorization: every authenticated user can read every fleet database via `?database=`; there is no DB-scoped role model |

### G6-B01 — `/explain` escapes its read-only transaction (P0)
- **Boundary:** `POST /api/v1/explain` (operator role) → `Explainer.Explain` →
  `runExplainParameterized` when the query contains `$N`.
- **Root cause:** `conn.Exec(ctx, "PREPARE _sage_explain AS " + query)` has no bind
  arguments, so pgx uses the simple query protocol, which executes every `;`-separated
  statement. The only input filter is a leading-keyword DDL denylist (`isDDL`). A
  transaction-control statement inside the body ends the READ ONLY transaction, so
  later statements run with the sidecar's privileges (often superuser) with no policy
  gate, emergency-stop check or action_log entry.
- **Impact:** an operator (or a stolen operator session) can mutate or read anything on a
  monitored database, outside every safety mechanism the product advertises.
- **Fix:** (1) run PREPARE through the extended protocol
  (`conn.Exec(ctx, prepSQL, pgx.QueryExecModeExec)`), which PostgreSQL restricts to one
  statement; (2) replace the DDL denylist with an allowlist: exactly one statement whose
  first keyword is `SELECT`, `WITH`, `VALUES` or `TABLE`, rejecting `;` outside
  literals/comments via the existing executor multi-statement scanner; (3) keep EXPLAIN
  ANALYZE for read-only statements only.
- **Test:** multi-statement bodies (with and without `$1`) are rejected with
  `ErrExplainInvalidRequest` and a probe table is unchanged afterwards; single `SELECT … $1`
  still returns a plan.

### G6-B03 — manual DDL bound to the HTTP deadline (P1)
- `timeoutMiddleware` wraps every request in a 30 s deadline. `approve`/`execute`
  handlers call `Executor.ExecuteManual(r.Context(), …)`, which runs
  `execManualSQLWithRetry(ctx, …)` and `logManualAction(ctx, …)` on that context.
- **Scenario:** approve `CREATE INDEX CONCURRENTLY` on a table that needs 90 s → context
  cancelled at 30 s → PostgreSQL leaves an INVALID index; `logManualAction` fails on the
  cancelled context, so there is no `action_log` row; the queue row stays `approved`.
- **Fix:** detach execution and logging from the request
  (`context.WithoutCancel` + an explicit DDL deadline) inside `ExecuteManual`; the
  rollback goroutine already does this. **Test:** cancel the caller context mid-DDL and
  assert the statement completes and an `action_log` row exists.

### G6-B05 — Rollback button resolves queue ids against action_log (P1, UI P0)
- `queryActionsWithQueueLedger` returns queued rows with `"id": <queue id>` and
  `"outcome": <queue status>`; nothing marks the row kind. `Actions.jsx` shows
  "Roll Back Action" for `outcome ∈ {success, monitoring, pending}` and posts
  `/actions/{row.id}/rollback`. `RollbackAction` loads `sage.action_log WHERE id=$1`.
- **Impact:** clicking Rollback on a queued proposal executes the stored rollback SQL of
  whichever executed action shares that numeric id.
- **Fix:** add `"record_kind": "executed" | "queued"` to every ledger row; the UI shows
  Rollback only for `executed`; server keeps the action_log lookup. **Test:** API contract
  test asserts `record_kind` on both kinds; UI test asserts no rollback button on queued rows.

## B. Dead / unwired

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G6-D01 | `api/router.go:89,100,112` | `NewRouter`, `NewRouterWithActions`, `NewRouterFull` | TEST-ONLY → move to test helper | Production uses `NewRouterWithRuntime`; tests exercise a router without runtime wiring |
| G6-D02 | `api/router.go:450`, `api/handlers.go:829` | `registerConfigRoutes`, `configUpdateHandler` | DELETE | Superseded by `registerConfigRoutesRuntime` |
| G6-D03 | `api/database_handlers.go:537,575` | `checkHost`, `isBlockedHost` | DELETE | Superseded by `resolveSafeHost` (still enforced) |
| G6-D04 | `api/handlers.go:1535`, `action_handlers.go:887`, `cases_handlers.go:287,409` | `buildFindingMap`, `actionTimelineMap`, `enrichCaseActionTimeline`, `queryActionLogsByFinding` | DELETE (or wire timeline into Cases per SURF-10) | No production caller |
| G6-D05 | `api/events.go:92,122`, `router.go:46` | `EventBroker.SubscriberCount/Stop`, `DefaultEventBroker` | WIRE `Stop` into shutdown; SubscriberCount → metric | SSE clients are not closed on graceful shutdown |
| G6-D06 | `auth/auth.go:202,265,392,409` | `DeleteUser`, `UpdateUserRole`, `GetUserByID`, `CountAdmins` | DELETE (handlers implement their own SQL) or make handlers call them | Duplicate logic invites last-admin-invariant drift |
| G6-D07 | `mcp/intent_adapters.go:32` | `FailClosedIntentExecutor.Execute` | TEST-ONLY OK | Used as a test double |

## C. Improvements (ranked impact × effort)

1. **Route capability manifest + generated authz tests** (H×M): one table of
   method/path → required role → owner → request schema, generated from `router.go`; a test
   asserts every mutating route rejects viewers on the real mounted router.
2. **Per-database RBAC** (H×M): role grants scoped to database IDs; `?database=all`
   filtered to granted databases.
3. **Principal-bound MCP** (H×S): pass the authenticated user into the MCP backend;
   per-tool read/write role; actor ID persisted in ledger evidence.
4. **Issuer+subject identity for OIDC** (H×S): new `sage.users.oauth_issuer/subject`,
   explicit account linking.
5. **Async manual execution** (M×M): approve returns 202 with an action id; UI polls.
6. **SSE broker shutdown + subscriber metric** (M×S).
7. **Explain via a least-privilege role** (M×M): optional `explain.role` that the sidecar
   `SET ROLE`s to, so ANALYZE of side-effecting functions cannot use superuser.

## D. Questions the user isn't asking
- Which API actions should be possible for a *viewer* at all (MCP read tools? explain?)
- Should the sidecar ever connect as superuser? Most blast-radius findings (G6-B01,
  G8 local provisioning, G4 retention) are amplified by superuser connections.
- Is there an audit trail of *reads* (who ran explain on what)?

## E. Verification notes
Lead verified G6-B01, B02, B03, B04, B05, B08 by reading the code paths listed. Delegated
reviewer's inventory: 82 distinct non-AgentDB method+path routes; config-audit, policy
ratification and explain have no UI caller; several routes are called only from UI pages
that are no longer routed (see G9).
