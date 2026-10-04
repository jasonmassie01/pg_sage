# P3: MCP v2 for coding agents (roadmap phase 3)

Branch `claude/p3-mcp-v2`, stacked on `claude/w3c-binding-facts` (#109; base unchanged at
push time). Agent report, 2026-10-04.

## What was built

Claude Code or Cursor can now connect to pg_sage with one `claude mcp add` line and a scoped
token, read what is wrong with a database, test a fix without touching it, receive a cited
**source-fix packet** for a finding, open the PR in the application repository, report the
deploy, and get pg_sage's verdict (predicted vs observed) back through the same tool.

1. **Protocol conformance** (`internal/mcp`). Dual-era server on stdio and Streamable HTTP:
   handshake era `2025-03-26`/`2025-06-18`/`2025-11-25` (initialize negotiation,
   `tools.listChanged`, `ping`, `notifications/tools/list_changed` on stdio when the fleet
   changes) and stateless `2026-07-28` (`server/discover`, `_meta` protocol version on every
   request, `UnsupportedProtocolVersion` -32022 with the supported list, `resultType`,
   `ttlMs`/`cacheScope`, `subscriptions/listen` with acknowledgment, tagged notifications,
   cancellation and graceful completion; HTTP `MCP-Protocol-Version`/`Mcp-Method`/`Mcp-Name`
   validation with the base64 sentinel, -32020 on mismatch, 404 for unknown modern methods,
   405 for GET/DELETE, 403 for foreign `Origin`, 400 for batches). Tool results are
   `content[]` + `structuredContent`; tool failures are `isError` results with a structured
   `{code, reason, message}`; unknown tools are `-32602 Unknown tool`. Tools carry
   annotations (`readOnlyHint`), deterministic order.
2. **Scoped API tokens** (`internal/mcptoken`, `internal/api/mcp_bearer.go`,
   `mcp_token_routes.go`, `sage.mcp_tokens`, web `MCPTokensPage`). read/propose/approve;
   approve only on operator tokens bound to an operator/admin person (DB CHECK repeats it);
   per-database restriction; 1-90 day expiry; SHA-256 at rest; admin create/list/revoke in UI
   and API; bearer authenticates only `POST /api/v1/mcp`. Agents read and propose; they
   never approve, never confirm facts, and every MCP gate path clears approval flags
   (`mcp.NeverApproved`).
3. **`database` on every tool** (`internal/mcp/database.go`), validated against the fleet
   (`fleetMCPDirectory`), defaulting only in single-database mode; `list_databases`; the
   tools/list schema enumerates the caller's permitted databases; backends route by the
   resolved name (`mcpInstanceFor`).
4. **`apply_migration`** advertised `{ddl, intent, constraints}` while its executor needs a
   schema-qualified `table` and `sql`: every schema-following call failed. Schema is now
   `{table, sql, cycle}`, validated strictly; the gate is asked for every expand step before
   any clone rehearsal (a blocked step ends the request, recorded `blocked`).
5. **New tools** (`internal/agenttools`, built by a sub-agent tests-first): `top_queries`
   (pgss + latest captured plan + sqlcommenter tags, one exclusion hook), `explain_query`
   (the existing safe-EXPLAIN path, no LLM; ANALYZE only when the guard proves it
   side-effect free), `whatif_index` (HypoPG, reports when absent), `lint_migration` (the
   DDL classifier + live risk), `mark_object` (proposed `owned_by_app_migrations` fact),
   `query_sources` (sqlcommenter in pgss text + pg_stat_activity sampling).
6. **Source-fix packets**: `get_source_fix_packet` (problem, cited evidence, migration
   up/down, targets and attribution, verification plan, hash; database/model text fenced)
   and `report_source_fix` (pr_opened, deployed, status → verdict via `verify` statistics on
   `sage.query_store`, decided once, `sage.source_fix`).
7. **Docs**: `docs/mcp.md` (setup, tokens, errors, protocol, loop, attribution) with the tool
   reference generated from the schemas and a test that they match; mkdocs nav, README link,
   CHANGELOG bullet.

Bug fix found on the way: `internal/explain` planned pg_stat_statements text with unbound
`$n` as NULL, folding `col = NULL` to a cost-0 Result; it now uses the generic plan.

## Product calls

1. Dual-era server: clients today speak the handshake era; the current spec is stateless.
2. Failures a model can act on are `isError` results with the existing distinguishable
   codes; JSON-RPC errors only for protocol problems and cancellation.
3. Approve scope = `decide_fact`, `declare_table_contract`, `register_consumer` (both are
   imported as confirmed facts since #109, so an agent using them would confirm a fact),
   `sre_review_investigation`, `sre_downgrade_autonomy` (a standing change; a person's call),
   and `request_change` carrying one of those kinds.
4. stdio is an agent principal (read + propose): whatever launches the sidecar over stdio is
   a program. Behavior change: stdio could decide facts and declare contracts before.
5. Unbound (library) callers may only read; tools/list shows only callable tools.
6. Fleet mode requires `database` everywhere, including former "list across all" tools
   (`sre_list_incidents`) and `get_value` (now per database). A restricted token gets the
   same -32003 for an existing and a missing database.
7. Tokens: `agent` or `operator` kind; operator tokens follow the owner's current role and
   die with the owner; invalid bearer never falls back to the session; storage errors are
   503 (a client keeps a valid token through an outage); revoked/expired tokens are kept as
   audit (retention exemption); `last_used_at` written at most once a minute.
8. An agent's mutating intent is judged like pg_sage's own initiative (never as an
   approval): it executes only where earned autonomy already allows it, otherwise it queues.
9. `mark_object exempt` = the existing owned fact with an "exempt" note (no new fact type:
   facts only narrow, a new type would widen the binding rules); existence checked.
10. Top queries and attribution exclude statements with #110's `workload.Excluded` (the
    one rule advice uses).
11. queryids are strings in output and accepted as integer or string (JS loses precision).
12. No fake what-if without HypoPG (`available: false`); what-if takes 1-20 query ids.
13. Source fixes: one report per finding; verdict decided once, lazily on `status` after the
    window (no background worker); kept in `sage.source_fix`, not in `sage.action_outcome`
    (that ledger is keyed by pg_sage's own actions; a deploy is not one).
14. Lint verdict: risky >= 0.7, review >= 0.3 or any rewrite / ACCESS EXCLUSIVE, else safe;
    unrecognized DDL is said to be unproven, not safe.
15. Default `mcp.transport` is now `http` (coordinator decision; the API server always
    runs) and MCP over HTTP is token-only (a later coordinator decision: a session cookie
    never authenticates it, CSRF risk): tested for every
    method, with no or bad credentials, with and without the session middleware. stdio
    stays available. Setup = a token in the UI + one `claude mcp add` line.
16. HTTP subscriptions close gracefully before the API's 30 s request deadline (clients
    listen again) instead of exempting the MCP path from the deadline.
17. UI defaults a new token to read only, all databases, 30 days.

## Test Results

**Command:** `go test -count=1 -p 2 -cover -v ./...` (Docker `golang:1.25`, own PG17
`pgsage-ag7` :55477, repo root mounted, label `owner=p3-mcp-v2`)
**Total:** 12,760 passed (9,929 top-level), 0 failed, 21 skipped (98 packages ok)

**Coverage (touched packages):**

| Package | PG17 | PG14 (-race) | PG18 (-race) |
|---|---|---|---|
| internal/mcp | 84.6% | ok 84.6% | ok 84.6% |
| internal/mcptoken (new) | 90.2% | ok 90.2% | ok 90.2% |
| internal/agenttools (new) | 87.5% | ok 87.5% | ok 87.5% |
| internal/explain | 93.7% | ok | ok |
| internal/schema | 84.4% | ok | ok |
| internal/retention | 87.3% | ok | ok |
| internal/api | 79.3% | ok (MCP tests) | ok (MCP tests) |
| cmd/pg_sage_sidecar | 79.8% | ok (MCP tests) | ok (MCP tests) |

Also: `go test -tags=e2e -count=1 -timeout 900s ./e2e/` ok (197 s);
`PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$' ./cmd/pg_sage_sidecar`
ok (168 s); `golangci-lint run ./...` 0 issues; web `npm test` 74 files, 433 passed, 0
failed, 0 skipped; `npm run lint` clean; dist rebuilt.

### Skipped Tests (must be zero or justified)
None in touched packages. The 21 skips are the pre-existing environment-gated ones listed in
the #109 report: live cloud provisioning and AgentDB gauntlets (6), Azure live parameters
(2), live LLM providers (3), container failover/restart fixtures (3), PgBouncer fixtures (3),
a Windows-only path test, the RCA child-process fixture, the plan fixture generator and
`TestPGIncidentBench`.

### Failures
None in the final runs.

### Coverage Gaps
All packages meet coverage thresholds (lowest touched: api 79.3%, cmd 79.8%, mcp 84.6%).

### Bugs Found This Session
1. [BUG] `explain.go`: unbound `$n` planned as NULL → constant-false cost-0 plan (fixed,
   `generic_plan_db_test.go`).
2. [BUG, cross-cutting, fixed product-wide by another agent] PostgreSQL 18's pg_stat_statements drops leading
   comments from stored text, so pg_sage's `/* pg_sage */` prefix no longer marks its own
   statements on PG18 (verified on the matrix 18.4; 17 keeps it). Its statements that do
   not touch `sage.*`/pg_stat catalogs look like workload on PG18. Filed as a follow-up task.
3. [BUG] `apply_migration` schema vs executor mismatch (fixed, item 4 above).
4. [BUG, mine] `mark_object` schema had an extra `}`; the registry's JSON then failed to
   marshal (caught by the tools/list tests; fixed before commit).
5. [BUG, tests] `server/discover` test used a string id with a `json.Number` decoder;
   agenttools sources test created its extra database after the sessions it sampled ended.

## Post-test audit

- **Mutation testing** (each applied on a container copy, suite run, restored): mcp scope
  and boundary logic 16/16 killed (agent-never-approve in `Has` and in the error path,
  authorize no-op, request_change intent kind, unbound propose, restricted database before
  and after lookup, `MayUseDatabase`, unfiltered tools/list, NeverApproved flags, migration
  preflight, stdio principal as operator, migration table pattern, fence marker
  neutralization, `Mcp-Name` check, Origin check); mcptoken 4/4 (revoked filter, expiry
  filter, viewer narrowing, `*` alone); agenttools 18/20 (one equivalent survivor: the early
  decided-verdict guard duplicates the conditional UPDATE; one not applicable); web 6/6.
- **Untested inputs**: stdio requests are processed sequentially, so `notifications/cancelled`
  cannot cancel an in-flight call (only subscriptions); `x-mcp-header` is not used; HTTP
  subscriptions behind a proxy with its own idle timeout rely on the 15 s keep-alive comment.
- **Assertions that could pass when broken**: the HTTP subscription test only waits 3 s for
  list_changed with a 10 ms watch interval; the production 15 s interval is untested.
- **Fakes hiding real failures**: mcp tests use recording backends; the real chain (router,
  session/bearer middleware, token store, server, fact backend) is covered by
  `internal/api/mcp_bearer_test.go` on Postgres, and the agent tools by agenttools DB tests
  on PG14/17/18 including HypoPG. Not covered end to end: a real Claude Code client.

### Manual Checks Remaining
- CHECK-M1: MANUAL: `claude mcp add --transport http ...` against a running sidecar and a
  real Claude Code session (protocol covered by the stdio/HTTP conformance suites).
- CHECK-M2: MANUAL: MCP tokens page in a browser (covered by vitest).

## Open questions

1. (Decided: HTTP default; source-fix verdicts stay out of the trust ledger; the PG18 tag
   placement is fixed product-wide by another agent.) A background verifier and a UI
   surface for source-fix reports remain open.
4. MCP resources/prompts (findings, schema) are still not served.
5. Legacy intent tools other than `apply_migration` are not strictly validated against their
   schemas by the server.
6. Pre-existing: `internal/retention/exemptions.go` is not gofmt-clean on master (one
   misaligned line); left untouched.

## Follow-up (coordinator answers)

- Merged `origin/release/v1.10.0` (#109, #110, #111 and test-timing fixes); the PR is now
  stacked on #112.
- `workload.Excluded` replaces the interim hook (test: the default filter equals the
  workload rule on pg_sage, EXPLAIN, maintenance, reset, COPY TO and application
  statements).
- `mcp.transport` defaults to `http`; `internal/api/mcp_anonymous_test.go` proves no
  anonymous MCP over HTTP. Config metadata and lifecycle docs regenerated; dist rebuilt.
- Results: config, api (79.4%), cmd (79.9%), mcp, mcptoken, agenttools, schema, retention,
  explain, workload ok on PG17; e2e ok; small perf gate ok; golangci-lint 0 issues; vitest
  75 files, 443 passed, 0 failed, 0 skipped; lint clean.

## Follow-up 2: token-only MCP over HTTP

- `POST /api/v1/mcp` accepts only `Authorization: Bearer <mcp token>`; the session
  middleware leaves the path to the token check, and a request with only a session cookie
  gets 401 `{"code":"mcp_token_required"}` explaining how to create a token.
- Tests: session cookie without token → 401; a read-only token sent with an admin's cookie
  is refused propose (the cookie lends nothing) and a full token is recorded as
  `mcp:token:<id>`, not the cookie's user; invalid token + valid cookie → 401; the
  anonymous test stays. Tests that called MCP through a session now use tokens (rule
  change); the router golden records the route as `open:401`.
