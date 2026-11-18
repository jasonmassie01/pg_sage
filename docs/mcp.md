# MCP for coding agents

pg_sage speaks the [Model Context Protocol](https://modelcontextprotocol.io) so a coding
agent (Claude Code, Cursor, any MCP client) can ask your database's DBA what is wrong, why,
and what to change in the application, then open the pull request itself. pg_sage hands the
agent a **source-fix packet**, the agent opens the PR in the application repository, and after
the deploy pg_sage measures whether the change did what it predicted.

An agent never gets more authority than pg_sage already has. It can **read** and
**propose**. Proposals go through the same policy gate as pg_sage's own initiative, and every
**approval** (confirming a fact, a table contract or a slot consumer, reviewing an
investigation, changing autonomy) stays with a person.

## Set up Claude Code in one line

MCP is on by default and served over HTTP at `/api/v1/mcp` on the API port, behind
authentication: every MCP request needs an MCP token. A dashboard session cookie never
authenticates MCP (a browser would send it cross-site); without a token the endpoint answers
401 with `"code": "mcp_token_required"`. There is no anonymous MCP.

1. An admin registers the agent once and mints its token: **Agents** in the dashboard (create
   the agent, then **Tokens** on it), or `POST /api/v1/agents` and
   `POST /api/v1/agents/{id}/tokens` (below). The token is shown once. Every agent token acts
   for its agent, so freezing, killing or retiring the agent reaches its tokens too. The
   **MCP tokens** page issues operator tokens only: a person's own token, acting for its owner.

2. Add pg_sage to Claude Code:

   ```bash
   claude mcp add --transport http pg_sage https://pg-sage.example.com:8080/api/v1/mcp --header "Authorization: Bearer $PG_SAGE_MCP_TOKEN"
   ```

   Use `--scope project` to share the server entry with your team through `.mcp.json`
   (keep the token in an environment variable, not in the file).

Cursor (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "pg_sage": {
      "url": "https://pg-sage.example.com:8080/api/v1/mcp",
      "headers": { "Authorization": "Bearer ${env:PG_SAGE_MCP_TOKEN}" }
    }
  }
}
```

To use stdio instead, set `mcp.transport: stdio`: the sidecar process itself then speaks
MCP on stdin/stdout. Whatever launched it is treated as an agent: it can read and propose,
never approve. `mcp.enabled: false` turns MCP off.

How the stdio transport behaves:

- **Size limit.** One message is one line of at most 4 MiB (the line ending not
  counted). A longer line is skipped up to its newline and answered
  `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"message exceeds 4 MiB"}}`;
  the session keeps serving the next line. A blank line is ignored. Send a larger
  `lint_migration` body over HTTP, or split it.
- **Concurrency.** Each `tools/call` runs on its own, at most 4 at a time; a fifth waits
  for a free slot, and the reader waits with it (so a cancel sent then is read only once a
  slot frees or a call times out). `initialize`, `ping`, `tools/list` and
  `subscriptions/listen` are answered in order, so a slow call never delays them and
  `initialize` finishes before the next request is read. Responses can arrive out of
  order: match them by `id`. An id that is still in flight is refused with -32600.
- **Timeout.** A call that runs longer than `mcp.call_timeout_seconds` (5 to 600,
  default 90) is cancelled and answered -32800 "tools/call timed out after ...". A call
  that completed is never reported as timed out.
- **Cancellation.** `notifications/cancelled` with the call's `requestId` cancels it, and
  no response is sent for it (as the MCP specification says), unless it had already
  succeeded: that result is still sent, so ignore it if you no longer want it. Cancelling
  an id that is not
  in flight does nothing; cancelling a `subscriptions/listen` id ends that subscription.
- **End of input.** At EOF the sidecar waits for in-flight calls, writes their responses
  and stops serving MCP. A failed write to stdout also ends the stdio session.

## Tokens

| Field | Meaning |
|---|---|
| `kind` | `agent` for a coding agent, minted for its agent at `POST /api/v1/agents/{id}/tokens` (the **Agents** page); `operator` for a person's own token, bound to an operator or admin user (`owner_user_id`), created at `POST /api/v1/mcp/tokens` (the **MCP tokens** page), which refuses `agent` |
| `scopes` | `read` (always), `propose`, and `approve` (operator tokens only; an agent token can never hold it) |
| `databases` | the fleet databases the token may name, or `["*"]` for all; with `api.database_grants` on, a person's token reads only those its owner is granted ([Per-database Authorization](security.md#mcp-tokens-and-ask-sage)) |
| `expires_in_days` | 1 to 90 |

Only the SHA-256 of a token is stored; the plaintext is returned once when it is created.
An operator token follows its owner: if the owner is demoted to viewer it can only read, and
it stops working when the owner is deleted. An agent token follows its agent: the list's
`principal_status` is the agent's status, and a retired agent's token stops working (the
**MCP tokens** page shows it as `agent retired`, and a frozen agent's as `agent frozen`).
Every call is recorded with the actor `mcp:token:<id>`.

```bash
# admin session cookie in $SAGE_COOKIE; sponsor_user_id is the accountable person
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"claude-code-checkout","sponsor_user_id":5,"profile":"readonly-analyst"}'
# the response's "id" (agp_...) names the agent: mint its token
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents/<agent id>/tokens \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"claude-code checkout repo","scopes":["read","propose"],
       "databases":["orders"],"expires_in_days":30}'
# a person's own operator token, acting for user 5
curl -s -X POST https://pg-sage.example.com:8080/api/v1/mcp/tokens \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"oncall laptop","kind":"operator","scopes":["read","propose"],
       "databases":["orders"],"expires_in_days":30,"owner_user_id":5}'
curl -s https://pg-sage.example.com:8080/api/v1/mcp/tokens -b "sage_session=$SAGE_COOKIE"
curl -s -X DELETE https://pg-sage.example.com:8080/api/v1/mcp/tokens/<id> \
  -b "sage_session=$SAGE_COOKIE"
```

A bearer token authenticates the MCP endpoint only, never the rest of the API.
[Agent governance](agent-governance.md#tokens) covers what each kind of token follows, what
a kill does to agent tokens, legacy tokens and OAuth access tokens.

## OAuth

An agent runtime that holds OAuth access tokens (JWTs) from your identity provider can use
them on the MCP endpoint instead of a pg_sage token. Turn it on with `mcp.oauth` (it needs
`mcp.transport: http`; every key is safety-critical and takes effect on restart):

| Key | Meaning |
|---|---|
| `mcp.oauth.enabled` | accept access tokens beside pg_sage's own MCP tokens (default off) |
| `mcp.oauth.resource` | the MCP endpoint's canonical URL, e.g. `https://sage.example.com/api/v1/mcp` |
| `mcp.oauth.issuers` | the allowed issuers: `[{issuer, jwks_uri, signing_algs}]`; https only (http on loopback) |
| `mcp.oauth.task_claim` | the claim carrying the runtime's task id (default `task_id`; empty ignores tasks) |

- **Per-database resources.** Each monitored database is its own protected resource,
  `<resource>/databases/<name>`, served at that path. A token whose audience names one
  database is refused at another's; at `<resource>` itself the token reaches only the
  databases its audience also names. Each resource's metadata (RFC 9728) is under
  `/.well-known/oauth-protected-resource`.
- **Scopes.** `read` is required, `propose` adds the propose tools, and an OAuth token never
  gets `approve`.
- **Task claim.** The `task_id` claim (or the one `task_claim` names) keys per-task taint. A
  missing claim is fine; a malformed one refuses the token.
- **Delegation.** Under RFC 8693 `act.sub` is the agent and `sub` whom it acts for. pg_sage
  binds the actor and records `sub` as on-behalf-of.
- **Binding.** Each identity, the pair (issuer, actor subject), must be bound to an agent
  principal by an admin: on the agent's **OAuth identities** section of the **Agents** page,
  or with `POST /api/v1/agents/{id}/identities` `{"issuer", "subject"}`. The issuer must be
  exactly one of `mcp.oauth.issuers`. An unbound identity is refused with 403
  `identity_unbound`; unbinding refuses its tokens at once.

[Agent governance](agent-governance.md#oauth) has the full rules, the binding routes and
their errors.

## Databases

Every tool takes a `database` argument (the fleet name; `list_databases` lists the ones the
caller may use). With a single monitored database it is the default; in fleet mode it is
required. A token restricted to some databases gets the same refusal for any other name,
whether that database exists or not.

## Errors

A tool call the agent can correct comes back as a tool result with `isError: true`; its
`structuredContent.error` has a `code` and a `reason`:

| Code | Reason | When |
|---|---|---|
| -32001 | `scope_required` | the caller lacks the scope (a viewer proposing, a read-only token) |
| -32005 | `approval_reserved_for_humans` | an agent asked for a person's decision |
| -32003 | `database_not_permitted` | the token may not use that database |
| -32006 | `database_required` | several databases are monitored and none was named |
| -32007 | `unknown_database` | no monitored database has that name |
| -32602 | `invalid_arguments` | the arguments do not match the tool's schema (see below) |
| -32004 | `not_found` | no such finding, investigation, fact... |
| -32009 | `conflict` | the object is not in a state that allows this |
| -32010 | `unavailable` | retry later: e.g. target database unavailable, no HypoPG |

The error also says what to do next. These keys are added where they apply; `code`, `reason`
and `message` keep their values:

| Key | Value |
|---|---|
| `next` | `{"tool": ..., "arguments": {...}}` to call next; for `invalid_arguments` and `runbook_rejected` the same tool with `required`, its schema's required arguments (and for `request_change` an `example` call with a valid intent); or `"wait"` |
| `retry_after` | whole seconds to wait before retrying (at least 1), where pg_sage knows it |
| `approval_url` | for `approval_reserved_for_humans`, the dashboard page where a person decides |

| Reason | `next` | `retry_after` |
|---|---|---|
| `approval_reserved_for_humans` | `get_inbox` (with the call's `database`) | none: a person decides |
| `database_not_permitted`, `database_required`, `unknown_database` | `list_databases` | none |
| `invalid_arguments`, `runbook_rejected` | the same tool, with `required` | none |
| `not_found` | the matching list tool (`list_facts`, `sre_list_incidents`, `sre_list_runbooks`, `sre_list_slos`) where there is one; otherwise the same tool with `required`, to correct the identifier (none for a tool without required arguments) | none |
| `rate_limited` | `"wait"` | the limiter's reset |
| `unavailable` | `"wait"` | 30, unless the feature is switched off (then neither key) |
| `conflict`, `too_many_investigations`, `scope_required` | `"wait"` | none: the state, a live investigation or the token has to change first |
| `internal_error` | none | none |

`"wait"` without `retry_after` means the condition clears only when something else changes;
do not retry in a loop. Every reason has a `next` rule except `internal_error`; a test fails
when a new reason is added without one.

### Held results

A call that succeeds but leaves work waiting on a person says so in the result itself:
a `request_change`, `optimize_query` or `ensure_fk_indexes` the policy check queued
(`decision: queued`) or held back (`parked`), a `propose_policy_change` or
`set_maintenance_policy` proposal, a `propose_plan` plan, a `specialist_request_remediation`
queued for approval and an `agent_request_capability` waiting for an operator. Each adds:

| Key | Value |
|---|---|
| `approval_url` | the dashboard page where a person decides (kept as the tool returned it when it already has one) |
| `handle` | the item, `<kind>:<id>`: the inbox kind and id (`policy_exception:42`, `change_approval:7`, `agent_request:4`, `plan:9`) or `change:<evidence_id>` for a change the policy check queued or held back |
| `status` | `{"tool": "get_request_status", "arguments": {"handle": ..., "database": ...}}`, a read-only call that reports the item now, when the caller is one the item's store records as its requester (an agent principal; a person for a plan or a policy proposal). Otherwise it stays the list the item shows up in: `get_ledger` for a `change:` handle, `get_inbox` for the item's kind (also when no id came back). A tool that has its own `status` field keeps it (`propose_plan`'s plan status) |
| `wait` | `{"tool": "wait_for_decision", "arguments": {...}}` with the same arguments, present whenever `status` is `get_request_status` |

None of these approve anything: a person decides on the page `approval_url` names.

### Waiting for a decision

`get_request_status {handle}` answers at once; `wait_for_decision {handle, timeout_seconds}`
(1 to 60, default 30) answers when the item reaches its next state. Both return the same
shape:

| Key | Value |
|---|---|
| `state` | `pending` (no person has decided), `approved`, `running`, `halted`, `executed`, `verified`, `completed`, `rejected`, `failed`, `expired`, `cancelled`, `reverted`, `rolled_back`, `recorded`, `held`, `blocked`, `resolved` or `unknown` |
| `decided_by_kind` | `person`, `policy` (the standing policy decided, no person), `system` (expiry, voiding) or empty while nobody has decided. An agent never decides |
| `outcome` | one line on what happened |
| `next` | `wait_for_decision` while more can happen, `get_request_status` after `unknown`, `null` when the item is final |

How a wait ends:

- the item changes state (an item already final answers at once). Call again to follow the
  next transition: pending, approved, executed, verified;
- `timeout_seconds` passes: `state` as last read (`pending`), `next: wait_for_decision`;
- the live-event stream shuts down (the sidecar is stopping): `state: unknown`,
  `next: get_request_status`. A wait never hangs.

The wait listens to the sidecar's own live-event stream (every successful API write, such
as a person's approval, signals it at once; the dashboard's change poller and heartbeat
also wake it) and reads the item once per signal: no polling loop, and no database
connection is held while it waits. Each principal may have 5 waits open and the sidecar
200; one more is `rate_limited` with `retry_after` (when the soonest open wait ends).

The status is the requester's own: another principal's handle, or a caller with no principal,
gets `not_found`, the same answer as a handle that does not exist. A person over MCP can read
the plans and policy proposals they proposed (the store records the proposing actor); a
change or an approval item records only an agent principal, so a person's held change points
at `get_ledger` or `get_inbox` instead.
A `change:<evidence_id>` handle resolves for the agent principal recorded on the ledger
decision; a decision that queued an approval reports that approval's state. A change the
policy check held back without an approval is `held` and final: nothing waits on a person
(see `get_ledger`).

In the dashboard's Inbox, an item an agent asked for (a change approval it queued, a
capability request, a policy proposal) says "Asked by agent", links the agent's page where
its requests are listed, and shows the handle the agent waits on; the item's Open link is
the approval itself.

### Tool descriptions

Every tool's description says when to use it ("Use when"), what it returns ("Returns") and
what it never does ("Never"), in at most 400 characters and in the words of the
[glossary](glossary.md). A test fails on a tool that breaks the rule.

Every tool rejects an argument its schema does not list, or one of the wrong type, with
-32602 (a misspelled `databse_id` is an error, not ignored); so is an intent pg_sage cannot
plan, such as an unsupported `request_change` kind. Any other failure is -32603
`internal_error`; its detail is logged by pg_sage, never returned. An unknown tool,
malformed params or an unsupported protocol version are JSON-RPC errors.

## Protocol

pg_sage serves both protocol eras on stdio and Streamable HTTP:

* **Handshake era** (`2025-03-26`, `2025-06-18`, `2025-11-25`): `initialize` negotiates the
  version and advertises `tools.listChanged` and `resources`; `ping`;
  `notifications/tools/list_changed` is sent on stdio when the tool list changes (a
  database joins or leaves the fleet, or a tool becomes available or unavailable).
* **Stateless** (`2026-07-28`): `server/discover`, the protocol version in every request's
  `_meta`, `resultType` on every result, and `subscriptions/listen` for list changes (on
  HTTP an SSE stream that pg_sage closes gracefully before the API's request deadline;
  listen again). Requests must carry matching `MCP-Protocol-Version`, `Mcp-Method` and
  `Mcp-Name` headers.

`tools/list` shows only the tools the caller may call, with the databases it may name.

### Tool availability

`tools/list` also leaves out a tool that cannot be served, so an agent does not pick a tool
that can only answer `unavailable`. A tool that works on some databases only is listed with
a `database` enum of just those. The rules (the "Available when" column of the
[tool reference](#tool-reference)):

* `ask_sage`: Ask Sage is enabled for the database (`ask.enabled`) and an enabled LLM is
  configured.
* `specialist_*` (all five): the Postgres-specialist contract is enabled.
* `top_queries` and `whatif_index`: the database has `pg_stat_statements` (`whatif_index`
  measures the queries its `query_ids` name, which come from there). `whatif_index` is not
  hidden without HypoPG: it says so in its result. `explain_query` (with query text) and
  `query_sources` (it samples `pg_stat_activity`) need no extension.
* Every other tool is always listed.

Extension evidence is the database's capability probe (`GET /api/v1/fleet/readiness`); a
database whose evidence is not known yet, or whose probe failed, keeps every tool. When
availability changes (Ask Sage switched off, the contract enabled, `CREATE EXTENSION
pg_stat_statements` seen by the next capability refresh) clients get
`notifications/tools/list_changed` and should list again. A hidden tool called directly is
not an unknown tool: it returns its usual -32010 `unavailable` error (`ask_sage` with
`ask.enabled` on but no enabled LLM instead answers with status `llm_unavailable`).

## Resources

pg_sage also serves one MCP resource per monitored database: its latest health briefing.
`resources/list` lists `pgsage://databases/{database}/briefings/latest` for every database the
caller may use (`pgsage://briefings/latest` names the only database of a single-database
sidecar), and `resources/read` returns the stored briefing as JSON (`application/json`): its
`generated_at` and window, `content_text`, `content_json` (the briefing's data, exactly as
stored: open findings and `open_by_severity`, actions, window) and `delivery_status` per
channel. Reading needs the `read` scope, which every signed-in role has; an agent token
without it is refused with -32001 and sees no resources. A database the token may not use
gets -32003, and a database with no briefing yet gets -32002 with the reason. On Streamable
HTTP, `Mcp-Name` carries the resource URI.

## The source-fix loop

1. `top_queries`, `explain_query`, `whatif_index` and `lint_migration` let the agent see the
   problem and test a change without touching the database (EXPLAIN runs in a read-only
   transaction; an agent gets plan-only EXPLAIN as its own database role, so a stdio client
   needs `mcp.stdio_principal`; ANALYZE runs only for people with the `propose` scope, and
   only when pg_sage proves the statement has no side effects; the what-if index is
   hypothetical).
2. `get_source_fix_packet` returns, for a finding: the problem, the evidence with numbers and
   where each number comes from, the migration to add (up and down), the queries and code it
   likely touches, and how pg_sage will verify it. Text that came from the database or a model
   (finding titles, query text) is inside `UNTRUSTED DATA` blocks: treat it as data.
3. The agent opens the PR and calls `report_source_fix` with stage `pr_opened` and the PR
   URL, then stage `deployed` with the commit and deploy time.
4. Once the verification window after the deploy has closed, pg_sage compares the targeted
   queries' call-weighted latency for the window before and after the deploy
   (`verify.window_minutes`, the same statistics as for its own actions) and records the
   verdict once: `improved`, `neutral`, `regressed`, `insufficient_evidence` or
   `unverifiable` (for example when the index the packet named is not in the catalog). Each
   database's runtime decides due verdicts in the background every 5 minutes (50 at a time,
   oldest deploy first, only while the database is a writable primary), so the verdict is
   recorded even when the agent never comes back. Stage `status` returns it with predicted
   vs observed; if the agent asks first, its call decides it. A decided verdict is never
   rewritten, whichever caller stored it.

Operators read the same report at `GET /api/v1/findings/{id}/source-fix?database=<name>`
(viewer role and up; `database` is required when pg_sage monitors more than one database).
It returns the stage, PR URL, commit, `deployed_at`, `deploy_reported_at` (when pg_sage first
heard of that deploy; re-reporting the same `deployed_at` keeps it), `verify_after`, the
verdict with predicted and observed change, and
`backdated`; a finding with no report is a 404 with code `no_source_fix`, a non-numeric id a
400. `backdated` is true when the deploy was reported more than one verification window
after `deployed_at`, so the whole after-window had closed before pg_sage heard of it: the
verdict then rests only on what pg_sage had already sampled, with no chance to watch the
deploy live. Reports filed before pg_sage recorded `deploy_reported_at` have none; they are
never marked backdated and the dashboard shows their report time as unknown. The dashboard
shows the report in a finding case's technical details (Cases and the Findings explorer).

`mark_object` lets the agent say an index, table or schema is owned by the application's
migrations (`owned`) or must be left alone by pg_sage's DDL (`exempt`). It is recorded as a
proposed [binding fact](facts.md) and binds nothing until a person confirms it.

### How DDL is classified

`lint_migration`, the agent gate (`apply_migration`, `request_change`) and the migration
advisor classify DDL with one entry point. Release builds (`CGO_ENABLED=1`) read each
statement's PostgreSQL parse tree: every `ALTER TABLE` subcommand, inline column
constraints, `CONCURRENTLY` and `NOT VALID`, with the version the server runs (or the
`pg_version` given). A statement holds the strongest lock of its subcommands. Each
statement is either *known* (a rule below matched, or it is a form proven safe, such as
`CREATE INDEX CONCURRENTLY`, `ADD CONSTRAINT ... NOT VALID`, `VALIDATE CONSTRAINT`,
`DETACH PARTITION ... CONCURRENTLY`, a plain `VACUUM`, a new `CREATE FUNCTION` or
`ALTER SEQUENCE ... AS / OWNED BY`) or *unrecognized* (no rule
matched and the form is not proven safe, or it did not parse).

| Rule | Form | Lock | Rewrite |
|---|---|---|---|
| `ddl_index_not_concurrent` | `CREATE INDEX` without `CONCURRENTLY` | SHARE | no |
| `ddl_constraint_not_valid` | `ADD CHECK` without `NOT VALID`, `ADD COLUMN ... CHECK` | ACCESS EXCLUSIVE | no (scan) |
| `ddl_fk_not_valid` | `ADD FOREIGN KEY` without `NOT VALID`, `ADD COLUMN ... REFERENCES` | SHARE ROW EXCLUSIVE (ACCESS EXCLUSIVE with `ADD COLUMN`) | no (scan) |
| `ddl_add_key_builds_index` | `ADD PRIMARY KEY` / `UNIQUE` (also inline on `ADD COLUMN`) without `USING INDEX` | ACCESS EXCLUSIVE | no (index build) |
| `ddl_add_exclusion_builds_index` | `ADD ... EXCLUDE` | ACCESS EXCLUSIVE | no (index build) |
| `ddl_set_not_null` | `ALTER COLUMN ... SET NOT NULL` | ACCESS EXCLUSIVE | no (scan) |
| `ddl_not_null_constraint_scan` | PG 18 `ADD CONSTRAINT n NOT NULL col` without `NOT VALID` | ACCESS EXCLUSIVE | no (scan) |
| `ddl_alter_type_rewrite` | `ALTER COLUMN ... TYPE` (no rewrite only when binary coercible, see below) | ACCESS EXCLUSIVE | yes |
| `ddl_add_column_volatile_default` | `ADD COLUMN` with a volatile default, serial, identity or stored generated column | ACCESS EXCLUSIVE | yes |
| `ddl_set_logged` | `SET LOGGED` / `SET UNLOGGED` | ACCESS EXCLUSIVE | yes |
| `ddl_set_access_method` | `SET ACCESS METHOD` (PG 15+) | ACCESS EXCLUSIVE | yes |
| `ddl_set_tablespace` | `SET TABLESPACE` | ACCESS EXCLUSIVE | yes |
| `ddl_drop_column` / `ddl_drop_table` | `DROP COLUMN` / `DROP TABLE` | ACCESS EXCLUSIVE | no |
| `ddl_drop_index_not_concurrent` | `DROP INDEX` without `CONCURRENTLY` | ACCESS EXCLUSIVE | no |
| `ddl_attach_partition_no_check` | `ATTACH PARTITION` | ACCESS EXCLUSIVE | no (scan) |
| `ddl_detach_partition_not_concurrent` | `DETACH PARTITION` without `CONCURRENTLY` | ACCESS EXCLUSIVE | no |
| `ddl_rename` | `RENAME` of a table, column, constraint or index | ACCESS EXCLUSIVE (SHARE UPDATE EXCLUSIVE for an index) | no |
| `ddl_create_trigger` | `CREATE TRIGGER` | SHARE ROW EXCLUSIVE | no |
| `ddl_drop_trigger` | `DROP TRIGGER` | ACCESS EXCLUSIVE | no |
| `ddl_drop_function` | `DROP FUNCTION` (the lock is on the function, not a table) | ACCESS EXCLUSIVE | no |
| `ddl_lock_table` | `LOCK TABLE` | the mode it names | no |
| `ddl_reindex_not_concurrent` | `REINDEX` without `CONCURRENTLY` | ACCESS EXCLUSIVE | no |
| `ddl_vacuum_full` | `VACUUM FULL` | ACCESS EXCLUSIVE | yes |
| `ddl_refresh_not_concurrent` | `REFRESH MATERIALIZED VIEW` without `CONCURRENTLY` | ACCESS EXCLUSIVE | yes |
| `ddl_cluster` | `CLUSTER` | ACCESS EXCLUSIVE | yes |
| `ddl_missing_lock_timeout` | an ACCESS EXCLUSIVE statement with no earlier `SET lock_timeout` | (advice) | no |

`ALTER COLUMN ... TYPE` is reported as no rewrite only when the column's current type, read
from the catalog, makes the change binary coercible: `varchar(n)` to `varchar(m)` with m >= n,
to unbounded `varchar`, or to `text`. It stays a rewrite with a `USING` expression, with a
`COLLATE` clause, when one statement changes the type of more than one column, and when the
column has a collation other than the default. PostgreSQL then rewrites the table or rebuilds
the column's indexes.

A build without cgo has no parser. It logs once that classification is degraded and
matches the older rules with regular expressions. `SET LOGGED`, `SET ACCESS METHOD`,
`EXCLUDE`, `DETACH PARTITION`, the NOT NULL constraint, `RENAME`, `CREATE TRIGGER`,
`DROP INDEX` and inline `ADD COLUMN` constraints are then unrecognized (`review` in lint;
at the gate `ddl_locking`, or `ddl_destructive` for a drop), never safe.

### What counts as additive DDL

The agent gate classes DDL additive only when every statement is known and on a short
allowlist; anything else is not proven safe. The allowlist is `CREATE INDEX CONCURRENTLY`,
`CREATE TABLE` (not `AS`,
`PARTITION OF` or `INHERITS`), `CREATE [OR REPLACE] VIEW`, `CREATE SCHEMA`, `COMMENT ON`,
`CREATE SEQUENCE`, and `ALTER TABLE ... ADD [COLUMN] name type` with a built-in type and at
most a constant or stable default (`now()`, `current_timestamp`). `SET lock_timeout`,
`SET statement_timeout`, `BEGIN` and `COMMIT` are allowed alongside. A statement containing
`UNIQUE`, `REFERENCES`, `CHECK`, `EXCLUDE`, `PRIMARY KEY`, `LOGGED`, `ACCESS METHOD`,
`NOT NULL`, `RENAME` or `TRIGGER` is never additive, and neither is SQL that ends inside an
unterminated literal, quoted identifier or comment.

* The agent gate classes such DDL `ddl_locking` (any `DROP` or `TRUNCATE` is still
  `ddl_destructive` first), so it earns trust in the locking class, not the additive one.
  A statement that matches a rule, is unrecognized or does not parse is always
  `ddl_locking`.
* `lint_migration` returns `safe` only when every statement is known. It returns
  `review`, never `safe`, for an unrecognized statement (on a build without the parser:
  one outside the allowlist that no rule matched), and its `note` says how many there are.
* `lint_migration` is a thin wrapper over the shared pre-merge review that the
  `lint-migration` CI gate and `POST /api/v1/migrations/lint` also call
  ([Migration review in CI](migration-ci.md)), and that scores exactly as the migration
  advisor does: it applies `migration.ddl_row_threshold`, marks a binary-coercible
  `ALTER COLUMN ... TYPE` (`varchar(20)` to `text`) as no rewrite, and lists
  `ddl_missing_lock_timeout` as an unscored annotation (`"annotation": true`,
  `risk_score` 0) instead of a hazard. Items also carry `statement_index`, `verdict` and
  `guarded_rewrite`; the result carries `pg_version`, `row_threshold` and the full
  per-statement `review`. A catalog read that fails or times out is an error, never a
  verdict.

### What `apply_migration` can plan

`apply_migration` rewrites exactly one `ALTER TABLE` clause on the named table. It plans two
forms:

* `ALTER TABLE t ADD CONSTRAINT name UNIQUE (col [, ...])`, optionally followed by
  `[NOT] DEFERRABLE` and `INITIALLY DEFERRED | INITIALLY IMMEDIATE`. The expand step is
  `CREATE UNIQUE INDEX CONCURRENTLY "name_idx"`; the contract step is
  `ADD CONSTRAINT "name" UNIQUE USING INDEX "name_idx"` with the same timing clauses, so a
  deferrable request stays deferrable. PostgreSQL renames the index to the constraint name
  when it attaches.
* `ALTER TABLE t ALTER [COLUMN] col SET NOT NULL`, planned as a `NOT VALID` check named
  `<table>_<col>_nn`, validated, then `SET NOT NULL` and the check dropped.

The planner reads the statement with PostgreSQL's own parser (libpg_query), so the SQL means
what PostgreSQL says it means: a comment is never a clause, and `ONLY`, `IF EXISTS` and
spaces around the schema dot read as they do in `psql`. Names resolve the way PostgreSQL
resolves them: a bare `Email` is the column `email`, a quoted `"Email"` keeps its case, and
every name in the plan is quoted exactly once. Bare names must be ASCII; quote any other name.

The planner refuses, and the call returns `invalid_arguments` (-32602) with the reason:

* `unsupported option "..."; plan it by hand` for anything it cannot carry into the plan:
  `INCLUDE (...)`, `WITH (...)`, `USING INDEX TABLESPACE`, `USING INDEX`,
  `NULLS [NOT] DISTINCT`, and a repeated timing clause such as `DEFERRABLE DEFERRABLE`;
* `PostgreSQL cannot parse the migration SQL: ... at "..."` for SQL PostgreSQL rejects,
  including an empty column list and contradictory timing such as
  `NOT DEFERRABLE INITIALLY DEFERRED`;
* `unsupported migration statement` for anything but one `ALTER TABLE` with one of the two
  clauses above: `ADD PRIMARY KEY`, an unnamed `ADD UNIQUE`, a repeated column, several
  clauses or statements, or a database-qualified table;
* `identifier too long` for any name in the SQL longer than 63 bytes, quoted or not, and
  `name "..." is N bytes` for a derived index (`<constraint>_idx`) or proof name past 63
  bytes. PostgreSQL would silently truncate them.

Plan those migrations by hand. A build without cgo (a local `CGO_ENABLED=0` build; release
images and binaries are built with cgo) has no parser, so it refuses every online-migration
plan with `online migration planning needs the parse-tree build`.

### The migration ledger

Every `apply_migration` call leaves one row in `sage.migration_run`, keyed by `evidence_id`.
The `verdict` column uses this vocabulary:

| Verdict | Meaning |
|---|---|
| `blocked` | The policy gate refused an expand step before any rehearsal. `reason` holds the gate's reason code, for example `change_class_not_allowed`. |
| `recommend_only` | The plan was returned for a person to run; nothing was rehearsed or applied. |
| `parked` | Rehearsal or the gate held the migration for review. |
| `promote` | The expand steps ran. The contract steps wait for `contract_not_before` (expand time + `migration.contract_delay_minutes`); pg_sage never runs them. |
| `failed` | The rehearsal or an expand step failed. |
| `pending` | Reserved for a run that has not reached a verdict. |

A run uses the gate's evidence id. When the gate returned none, the run gets a fresh id, so
two runs never share a row. If a later write reuses an evidence id, the row takes that
write's verdict, `source_sql_hash` and `reason`.

### What `apply_migration` returns

The result always carries `verdict` and `reason`, so an agent can tell why a migration stopped:

| Field | Meaning |
|---|---|
| `verdict` | `expanded`, `recommend_only`, `parked`, `blocked` or `failed`. |
| `reason` | Why: the gate's reason code (`blocked`, `parked`); the rehearsal's (`stale_clone`, `inconclusive_no_workload`, `plan_regression`, `step_budget_exceeded: <step, measured time, budget>`, `clone_unavailable`, `rehearsal_failed:<SQLSTATE>`, ...), with `; clone_cleanup_failed: <error>` appended when the rehearsal clone could not be destroyed; `contract_pending` after an expand; `no_rehearsal_runtime` when no clone provider is configured; `plan_only` for a plan that was never rehearsed. |
| `pending_contract_sql` | After an expand: the contract statements pg_sage did not run, in order. |
| `contract_script` | After an expand: a forward-fix script for those statements (below). |
| `contract_not_before_cycle` | The deploy cycle the contract waits for. |
| `plan` | The full plan: expand and contract steps with their locks, `ContractNotBefore` and the proof (`ContractGuard`). |

A rehearsal promotes only with workload evidence. Today's rehearsal runner measures the
expand steps (time and lock per step, disk delta) but does not yet replay the workload, so a
rehearsed migration returns `recommend_only` with `inconclusive_no_workload` unless a step
already parked it. A step parks with `step_budget_exceeded` when 1.5 times its time on the
clone exceeds the applier's 10-minute `statement_timeout`, or when a step without
`CONCURRENTLY` held `ACCESS EXCLUSIVE` longer than `migration.max_lock_ms`. A query whose
plan hash changed parks above `rehearsal.plan_flip_pct` (default 5%) slower; any query parks
above 15% slower.

pg_sage never runs the contract phase. `contract_script` is one transaction for a person to
review and run after `contract_not_before` (`sage.migration_run`, set to the expand time plus
`migration.contract_delay_minutes`):

```sql
BEGIN;
DO $pg_sage_guard$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = ...
      AND conname = 'users_email_nn' AND contype = 'c' AND convalidated) THEN
    RAISE EXCEPTION 'pg_sage: proof % (validated_check) does not hold; run nothing', ...;
  END IF;
END
$pg_sage_guard$;
SET LOCAL lock_timeout = '5000ms';
ALTER TABLE "public"."users" ALTER COLUMN "email" SET NOT NULL;
ALTER TABLE "public"."users" DROP CONSTRAINT "users_email_nn";
COMMIT;
```

The guard checks the proof the expand phase built: the `CHECK (col IS NOT NULL)` constraint
is validated (`convalidated`), or the unique index is valid and unique (`indisvalid`,
`indisunique`). When it does not hold, the guard raises, the transaction aborts and nothing
changes, even in a client that keeps going after an error. A lock wait over 5 seconds aborts
it the same way; run it again later.

## Upgrade readiness

`upgrade_readiness` checks the resolved database for a major-version upgrade to
`target_major` (above the server's major, at most the newest major pg_sage knows)
by `method` (`pg_upgrade`, the default; `blue_green`; `logical`). It returns each
check U1-U7 as `pass`, `warn`, `block` or `unknown`, with its reason, up to 50
evidence rows and the total; see [Upgrade Readiness](findings.md#upgrade-readiness)
for what each check reads. Name `target_database`, a monitored database already at
the target major, to compare installed extensions against what it offers; without it
U2 is `unknown`. The tool only reads, in one read-only transaction with a statement
timeout; it writes nothing and opens no finding (the daily check does that). The REST
form is `GET /api/v1/upgrade-readiness?database=&target_major=&method=&target_database=`
(any signed-in role; 400 for an invalid target, 404 for an unknown database).

## Query to source attribution

Without any extension pg_sage attributes statements from two places:

* **sqlcommenter** comments (`/*controller='orders',route='%2Fcheckout'*/`) in the text
  `pg_stat_statements` keeps for each statement (the text of its first execution);
* **`pg_stat_activity` samples**: `application_name` and the comment tags of the statements
  running while `query_sources` samples (up to 5 seconds), matched by `query_id`
  (PostgreSQL 14+, `compute_query_id` on).

`pg_stat_statements` does not record `application_name`, so a statement that never runs
while sampled and carries no comment cannot be attributed. Add sqlcommenter to the
application's database driver (most ORMs have a plugin) for reliable attribution.

## Tool reference

<!-- BEGIN GENERATED MCP TOOL REFERENCE: regenerate with `go test ./internal/mcp -run TestToolReferenceDocsMatchSchemas -update-docs` -->

| Tool | Scope | Available when |
|---|---|---|
| [`get_policy`](#get_policy) | `read` | always |
| [`propose_policy_change`](#propose_policy_change) | `propose` | always |
| [`request_change`](#request_change) | `propose` | always |
| [`optimize_query`](#optimize_query) | `propose` | always |
| [`apply_migration`](#apply_migration) | `propose` | always |
| [`ensure_fk_indexes`](#ensure_fk_indexes) | `propose` | always |
| [`declare_table_contract`](#declare_table_contract) | `approve` | always |
| [`register_consumer`](#register_consumer) | `approve` | always |
| [`set_maintenance_policy`](#set_maintenance_policy) | `propose` | always |
| [`get_guarantee_status`](#get_guarantee_status) | `read` | always |
| [`get_value`](#get_value) | `read` | always |
| [`get_ledger`](#get_ledger) | `read` | always |
| [`sre_list_incidents`](#sre_list_incidents) | `read` | always |
| [`sre_get_investigation`](#sre_get_investigation) | `read` | always |
| [`sre_get_evidence`](#sre_get_evidence) | `read` | always |
| [`sre_get_transcript`](#sre_get_transcript) | `read` | always |
| [`sre_propose_action`](#sre_propose_action) | `propose` | always |
| [`sre_request_execution`](#sre_request_execution) | `propose` | always |
| [`sre_refresh_table_evidence`](#sre_refresh_table_evidence) | `propose` | always |
| [`sre_propose_statistics`](#sre_propose_statistics) | `propose` | always |
| [`sre_request_statistics`](#sre_request_statistics) | `propose` | always |
| [`sre_rollback_statistics`](#sre_rollback_statistics) | `approve` | always |
| [`sre_list_slos`](#sre_list_slos) | `read` | always |
| [`sre_get_slo`](#sre_get_slo) | `read` | always |
| [`sre_list_changes`](#sre_list_changes) | `read` | always |
| [`sre_list_runbooks`](#sre_list_runbooks) | `read` | always |
| [`sre_get_runbook`](#sre_get_runbook) | `read` | always |
| [`sre_runbook_runs`](#sre_runbook_runs) | `read` | always |
| [`sre_similar_incidents`](#sre_similar_incidents) | `read` | always |
| [`sre_draft_runbook`](#sre_draft_runbook) | `propose` | always |
| [`sre_compile_runbook`](#sre_compile_runbook) | `propose` | always |
| [`sre_get_autonomy`](#sre_get_autonomy) | `read` | always |
| [`sre_downgrade_autonomy`](#sre_downgrade_autonomy) | `approve` | always |
| [`sre_review_investigation`](#sre_review_investigation) | `approve` | always |
| [`sre_evaluate_autonomy`](#sre_evaluate_autonomy) | `propose` | always |
| [`list_facts`](#list_facts) | `read` | always |
| [`propose_fact`](#propose_fact) | `propose` | always |
| [`decide_fact`](#decide_fact) | `approve` | always |
| [`list_databases`](#list_databases) | `read` | always |
| [`top_queries`](#top_queries) | `read` | the database has the `pg_stat_statements` extension |
| [`explain_query`](#explain_query) | `propose` | always |
| [`whatif_index`](#whatif_index) | `read` | the database has the `pg_stat_statements` extension |
| [`lint_migration`](#lint_migration) | `read` | always |
| [`query_sources`](#query_sources) | `read` | always |
| [`mark_object`](#mark_object) | `propose` | always |
| [`get_source_fix_packet`](#get_source_fix_packet) | `read` | always |
| [`report_source_fix`](#report_source_fix) | `propose` | always |
| [`specialist_open_investigation`](#specialist_open_investigation) | `read` | the Postgres-specialist contract is enabled |
| [`specialist_investigation_status`](#specialist_investigation_status) | `read` | the Postgres-specialist contract is enabled |
| [`specialist_investigation_result`](#specialist_investigation_result) | `read` | the Postgres-specialist contract is enabled |
| [`specialist_investigation_transcript`](#specialist_investigation_transcript) | `read` | the Postgres-specialist contract is enabled |
| [`specialist_request_remediation`](#specialist_request_remediation) | `propose` | the Postgres-specialist contract is enabled |
| [`ask_sage`](#ask_sage) | `read` | Ask Sage is enabled for the database (`ask.enabled`) with an enabled LLM |
| [`fleet_findings`](#fleet_findings) | `read` | always |
| [`agent_query`](#agent_query) | `read` | always |
| [`agent_whoami`](#agent_whoami) | `read` | always |
| [`agent_request_capability`](#agent_request_capability) | `propose` | always |
| [`upgrade_readiness`](#upgrade_readiness) | `read` | always |
| [`get_inbox`](#get_inbox) | `read` | always |
| [`get_workload_pack`](#get_workload_pack) | `read` | always |
| [`get_outcomes`](#get_outcomes) | `read` | always |
| [`propose_plan`](#propose_plan) | `propose` | always |
| [`list_plans`](#list_plans) | `read` | always |
| [`get_plan`](#get_plan) | `read` | always |
| [`get_request_status`](#get_request_status) | `read` | always |
| [`wait_for_decision`](#wait_for_decision) | `read` | always |

### `get_policy`

Use when you plan a request_change or optimize_query and need the rules pg_sage's policy check applies to this database. Returns the standing policy's version and profile. Never changes policy and never approves anything; propose_policy_change asks a person for a change.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `propose_policy_change`

Use when the standing policy blocks what you think it should allow, or allows too much. Returns proposal_id and the dry-run impact (newly allowed, newly blocked) and, while it waits, approval_url and status. Never changes policy itself: an admin ratifies or rejects the proposal.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `caller_claims` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `delta` | object | yes |  |

### `request_change`

Use when you want a database change by intent, not SQL: intent names a kind plus that kind's arguments (one branch of its oneOf). Returns the policy check's decision, evidence_id, reason and, when it ran, the outcome; a held change adds approval_url and status. Never runs your SQL and never approves: a held change waits for a person.

Scope: `propose` (`approve` when the intent kind is `declare_table_contract` or `register_consumer`)

| Argument | Type | Required | Notes |
|---|---|---|---|
| `caller_claims` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `intent` | object | yes | the change: kind is optimize_query, ensure_fk_indexes, apply_migration, declare_table_contract or register_consumer, plus that kind's arguments (such as query_id, schema, table or slot_name); see oneOf for each kind |

`intent` is one of these kinds; a shape matching none is refused with the list of kinds.

| Kind | Arguments | Example |
|---|---|---|
| `optimize_query` | `constraints`, `goal`, `query_id`, `query_text` | `{"goal":"latency","kind":"optimize_query","query_id":4711}` |
| `ensure_fk_indexes` | `schema` (required) | `{"kind":"ensure_fk_indexes","schema":"public"}` |
| `apply_migration` | `cycle`, `sql` (required), `table` (required) | `{"kind":"apply_migration","sql":"ALTER TABLE public.orders ADD CONSTRAINT orders_ref_key UNIQUE (ref)","table":"public.orders"}` |
| `declare_table_contract` | `append_only`, `exemptions`, `expected_pk`, `retention`, `table` (required) | `{"append_only":true,"kind":"declare_table_contract","retention":{"column":"created_at","interval":"90 days"},"table":"public.events"}` |
| `register_consumer` | `consumer_identity`, `owner` (required), `slot_name` (required) | `{"kind":"register_consumer","owner":"data-platform","slot_name":"debezium_orders"}` |

### `optimize_query`

Use when a query is slow: name it by query_id or query_text with goal latency. pg_sage plans an index under standing policy and its policy check decides whether it runs. Returns the decision, evidence_id, reason and outcome; a held change adds approval_url and status. Never runs SQL you send and never approves: a held change waits for a person.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `constraints` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `goal` | constant | yes | must be `latency` |
| `query_id` | integer | no |  |
| `query_text` | string | no |  |

### `apply_migration`

Use when a migration (ADD UNIQUE, SET NOT NULL) must run online. pg_sage rewrites it into expand steps, rehearses them on a clone and asks the policy check before the rehearsal and each step. Returns the verdict, reason, plan and the contract SQL it left for you. Never runs contract steps.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `cycle` | integer | no | deploy cycle of this migration; value >= 0 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `sql` | string | yes | the migration statement; length 1-20000 |
| `table` | string | yes | schema-qualified table the migration changes; pattern `^[A-Za-z_][A-Za-z0-9_$]{0,62}\.[A-Za-z_][A-Za-z0-9_$]{0,62}$` |

### `ensure_fk_indexes`

Use when foreign keys in a schema may lack supporting indexes (slow deletes, lock waits on the parent). Returns the policy check's decision, evidence_id, reason and outcome for the indexes pg_sage plans. Never builds an index the policy check holds; a held change waits for a person.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `schema` | string | yes |  |

### `declare_table_contract`

Use when a person states a table's intent: append_only, retention (interval and the timestamptz, timestamp or date column whose age defines it), expected_pk or exemptions. Returns the decision on the declaration, stored as a confirmed fact. Needs the approve scope. Never infers the retention column.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `append_only` | boolean | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `exemptions` | array of string | no |  |
| `expected_pk` | string | no |  |
| `retention` | object | no |  |
| `table` | string | yes |  |

### `register_consumer`

Use when a replication slot belongs to a consumer pg_sage must protect (CDC, a replica): name slot_name and owner. Returns the decision on the registration, stored as a confirmed fact. Needs the approve scope. Never registers on an agent's word: it is a person's declaration.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `consumer_identity` | string | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `owner` | string | yes |  |
| `slot_name` | string | yes |  |

### `set_maintenance_policy`

Use when pg_sage should maintain a scope differently (a patch to its maintenance policy). Returns proposal_id and the dry-run impact and, while it waits, approval_url and status; a person ratifies the proposal. Never applies the patch itself and never approves it.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `patch` | object | yes |  |
| `scope` | object | yes |  |

### `get_guarantee_status`

Use when you need to know whether pg_sage's safety invariants hold on this database before you rely on it. Returns the machine-readable status of each invariant. Never changes anything; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `get_value`

Use when you report what pg_sage has been worth. Returns verified DBA-hours saved by period, feature and database, incidents avoided, hours still pending and a daily trend; partial and unavailable name any source that could not be read. Never changes anything: it is read-only and never writes.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `get_ledger`

Use when you audit what pg_sage's policy check decided. Returns the newest policy decision records (evidence_id, decision, feature), narrowed by filter {decision, feature, limit 1-1000, default 100}. Never approves and never changes a record. It holds decisions, not results: get_outcomes has what an executed change did.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `filter` | object | no |  |

### `sre_list_incidents`

Use when you look for what is wrong now or was recently. Returns pg_sage's investigations of incidents and plan regressions: family, state, issue and conclusion. Never opens or changes an investigation; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `case_id` | string | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_investigation`

Use when you have an investigation id from sre_list_incidents. Returns its observed facts, likely explanation, alternatives, ruled-out hypotheses with evidence, missing evidence and operator steps. Never acts on those steps; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_get_evidence`

Use when you need one evidence item an investigation cites. Returns that item, typed and redacted. Never returns the unredacted value; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `evidence_id` | string | yes | format uuid |
| `investigation_id` | string | yes | format uuid |

### `sre_get_transcript`

Use when you need how the tool-calling investigator reached its conclusion. Returns its plan, each tool call with its redacted result and digest, cited claims and the outcome with its authority. Never replays a tool call; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |
| `keep_identifiers` | boolean | no | operators only: keep identifiers instead of keyed hashes |

### `sre_propose_action`

Use when a concluded investigation should be mitigated. Returns the evidence-matched mitigation (cancel of the one root backend) with its repair contract and policy decision, or why there is none. Never executes anything; sre_request_execution queues it for a person.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_request_execution`

Use when sre_propose_action returned a mitigation that should run. Returns the one approval item it queued in the existing approval flow. Never executes: a person approves, then pg_sage rechecks the target and runs it.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `proposal_id` | string | yes | format uuid |

### `sre_refresh_table_evidence`

Use when a statistics proposal needs table evidence under 15 minutes old: re-reads stat_tables once for a table a concluded or inconclusive investigation observed, at most once a minute per investigation. Returns the fresh evidence, or a refusal when the table's database or OID changed. Never executes a change.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |
| `schema` | string | yes |  |
| `table` | string | yes |  |

### `sre_propose_statistics`

Use when an investigation points at misestimated rows over correlated columns: propose CREATE STATISTICS as your explicit hypothesis. Returns the proposal with its repair contract and policy decision; pg_sage cites the newest matching stat_tables evidence itself. Never executes.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `columns` | array of string | yes | at most 8 items |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |
| `kinds` | array of string | no |  |
| `rationale` | string | yes | the operator's correlation hypothesis, 10 to 2048 characters |
| `schema` | string | yes |  |
| `table` | string | yes |  |

### `sre_request_statistics`

Use when sre_propose_statistics returned a proposal that should be built. Returns the one approval item it queued. Never executes: a person approves, then pg_sage rechecks the table and column identities under the table lock.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `proposal_id` | string | yes | format uuid |

### `sre_rollback_statistics`

Use when a statistics object pg_sage created should go: it is dropped after pg_sage rechecks it is that object. Returns the rollback's result. Needs the approve scope (admin people only). Never restores the prior ANALYZE statistics.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `proposal_id` | string | yes | format uuid |
| `reason` | string | yes |  |

### `sre_list_slos`

Use when you need to know whether users are affected. Returns SLO error-budget states (ok, ticket, page or unknown, with why), burn rates per window and budget left. Never claims customer impact from a database proxy; only app SLIs do. Read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_slo`

Use when you need one SLO's history or whether it recovered. Returns its state and state history and, with recovery_since, the SLI recovery decision. Never certifies a recovery on missing data, counter resets or low traffic. Read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `name` | string | yes |  |
| `recovery_since` | string | no | format date-time |

### `sre_list_changes`

Use when you ask what changed before a problem started. Returns signed deploys, migrations, feature flags, pg_sage's own changes, config changes, DDL, statistics resets, restarts, failovers and extension changes around a database. Never changes anything; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `window_minutes` | integer | no | value 1-10080 |

### `sre_list_runbooks`

Use when you look for a runbook that fits an issue. Returns a database's typed runbooks: status (draft, signed, retired, invalid), whether each runs and its latest version. Never runs one; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_runbook`

Use when you need one runbook's content. Returns every version's DAG, content hash, signer and imported playbook text. Never runs or signs it; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `runbook_id` | string | yes | format uuid |

### `sre_runbook_runs`

Use when you need where a runbook has run. Returns the investigations it ran in, with the version, path and proposal of each run. Never runs it; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `runbook_id` | string | yes | format uuid |

### `sre_similar_incidents`

Use when you want precedent for an investigation. Returns similar past incidents and their operator-verified outcomes, as context. Never counts them as evidence for this investigation; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_draft_runbook`

Use when you add a runbook, or a draft version of one (runbook_id and base_version), from a typed definition. Returns the draft. Never runs a draft: an admin signs it in pg_sage, and a signed version keeps running until the new draft is signed.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `base_version` | integer | no | value >= 1 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `definition` | object | yes |  |
| `runbook_id` | string | no | format uuid |

### `sre_compile_runbook`

Use when you have a playbook in English and want a runbook draft. Returns the draft pg_sage's model compiled. Never runs it until an admin signs it in pg_sage.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `text` | string | yes | length <= 16000 |

### `sre_get_autonomy`

Use when you need how much pg_sage may do on its own. Returns the autonomy level per incident family and action class (L0-L3), its cap, the level its evidence supports, active downgrades, pending promotions and history. Never changes autonomy; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_downgrade_autonomy`

Use when pg_sage should do less on its own for a family and action class ("*" for every class of the family). Returns the lowered level. Restricting is always allowed. Never raises autonomy: raising it needs a person's approval in pg_sage.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `action_class` | string | yes |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `family` | string | yes |  |
| `level` | string | yes | one of `L0`, `L1`, `L2` |
| `reason` | string | yes | length <= 1000 |

### `sre_review_investigation`

Use when a person has judged a concluded or inconclusive investigation: accept or reject its diagnosis, with an optional note and actual root cause (graph node id or free text). Returns the recorded review and outcome. Never counts toward promotion and never approves one: only a person's review in the UI or REST API counts.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `actual_root_cause` | string | no | length <= 400 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes |  |
| `note` | string | no | length <= 1500 |
| `verdict` | string | yes | one of `accepted`, `rejected` |

### `sre_evaluate_autonomy`

Use when you want pg_sage to propose the promotions its evidence supports now. Returns the promotions proposed and, for every other family and action class, why not (each unmet check and how to meet it). Never promotes: an admin approves every promotion.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `list_facts`

Use when you need what pg_sage knows about a database: who owns an object, test fixtures, CDC slots, archives, table windows, the owner brief. Returns proposed facts awaiting a person and confirmed ones, which bind pg_sage. Never treats the owner brief as binding; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `status` | string | no |  |
| `type` | string | no | one of `owned_by_app_migrations`, `test_fixture`, `slot_consumer`, `append_only`, `table_window`, `owner_brief`, `do_not_touch` |

### `propose_fact`

Use when you know something about the database pg_sage should respect, with the evidence for it. Returns the proposed fact. Never binds anything until a person confirms it; a confirmed fact only narrows what pg_sage does (an app-owned index changes through a PR, not pg_sage's DDL).

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `evidence` | string | yes | length <= 500 |
| `subject` | string | yes | length <= 300 |
| `subject_kind` | string | yes | one of `index`, `table`, `schema`, `slot`, `database` |
| `type` | string | yes | one of `owned_by_app_migrations`, `test_fixture`, `slot_consumer`, `append_only`, `table_window`, `owner_brief`, `do_not_touch` |
| `value` | object | no |  |

### `decide_fact`

Use when a person confirms or rejects a proposed fact, or rejects a confirmed one. Returns the fact's new state. Needs the approve scope. Never widens what pg_sage does: a confirmed fact only narrows or redirects it.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `decision` | string | yes | one of `confirm`, `reject` |
| `fact_id` | integer | yes | value >= 1 |
| `note` | string | no | length <= 1000 |

### `list_databases`

Use when you first connect, or when an error says database_required or unknown_database. Returns the monitored databases you may name in every other tool's database argument. Never lists a database you may not use.

Scope: `read`

No arguments.

### `top_queries`

Use when you look for the statements that cost the most. Returns the top statements from pg_stat_statements with calls, time, rows, the latest captured plan and sqlcommenter tags. Never includes pg_sage's own or diagnostic statements; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `include_plans` | boolean | no |  |
| `limit` | integer | no | value 1-50 |
| `order_by` | string | no | one of `total_time`, `mean_time`, `calls` |

### `explain_query`

Use when you need a query plan. Returns a safe EXPLAIN of one read statement (text or queryid) in a read-only transaction with a statement timeout. People need the propose scope; analyze runs only on a statement proven free of side effects. Agents get plan-only EXPLAIN as their own database role. Never analyzes a write.

Scope: `propose` (operator) for people; `read` for agents, plan-only, under the agent's role

| Argument | Type | Required | Notes |
|---|---|---|---|
| `analyze` | boolean | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `params` | array of string | no | at most 100 items |
| `query` | string | no | length 1-20000 |
| `query_id` | integer or string | no | pg_stat_statements queryid (send large ids as a decimal string); pattern `^-?[0-9]{1,20}$` |

### `whatif_index`

Use when you want to know whether an index would help before you propose it. Returns the planner cost of the workload queries with and without a hypothetical HypoPG index, or that HypoPG is not installed. Never builds an index.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `ddl` | string | yes | one CREATE INDEX statement; length 1-2000 |
| `query_ids` | array of integer or string | no | the workload queries to measure (from top_queries); at most 20 items |

### `lint_migration`

Use when you are about to ship migration SQL. Returns, per statement, the lock level, table rewrite, estimated lock time, risk score and the safe alternative, from pg_sage's DDL classifier and live table statistics. Never runs the SQL.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `pg_version` | integer | no | a major (14-18) or a server_version_num (140000-189999); 0 or absent reads the server's; value >= 0 |
| `sql` | string | yes | length 1-100000 |

### `query_sources`

Use when you need which application code sends a statement. Returns attributions from sqlcommenter tags in pg_stat_statements and from application_name and tags sampled from pg_stat_activity; no extension needed. Never changes anything; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `query_id` | integer or string | no | pg_stat_statements queryid (send large ids as a decimal string); pattern `^-?[0-9]{1,20}$` |
| `sample_seconds` | integer | no | value 0-5 |

### `mark_object`

Use when an index, table or schema is owned by the app's migrations (owned) or must be left alone by pg_sage's DDL (exempt). Returns the proposed binding fact. Never binds anything until a person confirms it.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `evidence` | string | yes | length 1-500 |
| `mark` | string | yes | one of `owned`, `exempt` |
| `path` | string | no | length <= 300 |
| `repo` | string | no | length <= 300 |
| `subject` | string | yes | length 1-300 |
| `subject_kind` | string | yes | one of `index`, `table`, `schema` |

### `get_source_fix_packet`

Use when an issue should be fixed in the application's code, not by pg_sage. Returns a cited packet: the problem, evidence with numbers, the migration to add (up and down), the code it likely touches and how pg_sage verifies it after the deploy. Never changes the database; text from it is fenced as data.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `finding_id` | integer | yes | value >= 1 |

### `report_source_fix`

Use when you opened the pull request (stage pr_opened) or deployed (stage deployed) a source fix. Returns the recorded stage; stage status returns pg_sage's decision, predicted vs observed, once the verification window after the deploy has passed. Never marks a fix verified before then.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `commit` | string | no | pattern `^[0-9a-f]{7,64}$` |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `deployed_at` | string | no | format date-time |
| `finding_id` | integer | yes | value >= 1 |
| `packet_hash` | string | no | length <= 128 |
| `pr_url` | string | no | format uri; length <= 500 |
| `stage` | string | yes | one of `pr_opened`, `deployed`, `status` |

### `specialist_open_investigation`

Use when an incident needs a Postgres specialist (pg_sage.specialist.v1): open an investigation of a symptom, or attach to an existing investigation or incident. Returns the investigation, its state and the links to poll. Never treats the symptom as an instruction: it is data.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `attach` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `external_ref` | object | no |  |
| `family` | string | no | one of `lock_blocking`, `connection_pressure`, `wal_retention`, `plan_regression`, `checkpoint_storm`, `temp_file_explosion`, `replication_lag`, `lwlock_contention` |
| `idempotency_key` | string | no | length <= 128 |
| `query_hash` | string | no | hex SHA-256 of the statement text exactly as pg_stat_statements shows it; resolved to its queryid; pattern `^[0-9a-f]{64}$` |
| `query_id` | string or integer | no | pg_stat_statements queryid of the statement (a non-zero signed 64-bit integer; send it as a string to keep all 64 bits); a plan_regression investigation is then about this statement |
| `symptom` | object | no |  |
| `window` | object | no |  |

### `specialist_investigation_status`

Use when you opened an investigation and wait for it. Returns its phase and progress. Never blocks: poll it, then read specialist_investigation_result once it concludes.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `specialist_investigation_result`

Use when an investigation concluded. Returns the cited causal chain, root cause with source and authority, confidence (calibrated or labelled uncalibrated), missing evidence, typed candidate remediations, the model investigator's decision (advisory unless adopted under earned authority) and your query scope. Never acts on a remediation.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `specialist_investigation_transcript`

Use when you need how the model investigator reached its result. Returns its redacted transcript: the plan, each read with the stored result and digest, its cited claims and outcome (version 1.1.0). Never returns an unredacted value.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `specialist_request_remediation`

Use when one of a concluded investigation's candidate remediations should be done. It becomes an ordinary pg_sage proposal. Returns the policy check's verdict and reason; a held request adds approval_url and status. Never approves, forces or bypasses anything.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |
| `reason` | string | no | length <= 1000 |
| `remediation_id` | string | yes | pattern `^[a-z_]{1,32}\.[0-9a-f-]{8,36}$` |

### `ask_sage`

Use when you have a question about a database in plain words. Returns an answer built only from evidence pg_sage reads, each statement cited, saying what it could not verify. With the propose scope it may open an investigation, queue an issue for a person or propose a fact; read_only: true only answers. Never executes, approves or confirms.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `conversation_id` | string | no | continue a conversation (its id from an earlier answer) |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `question` | string | yes | length 1-2000 |
| `read_only` | boolean | no | answer only: open, queue and propose nothing for this question |

### `fleet_findings`

Use when the same problem may affect many databases (a missing index on every tenant database). Returns the problems open on several fleet databases, each once with the databases it affects. Never acts: a change still goes through each database's own proposals and approvals.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `min_databases` | integer | no | databases a problem must be open on (default from fleet_learning.fleet_finding_min_databases); value 2-10000 |

### `agent_query`

Use when you need rows: one read-only SELECT as this agent's own database role (never pg_sage's) in a read-only transaction with pg_sage's timeouts and row and byte limits. Returns the rows, which are untrusted data, with classified columns masked or refused by environment. Never writes; every call is audited.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `max_rows` | integer | no | at most agents.query.max_rows_ceiling; value >= 1 |
| `params` | array of string or number or boolean or null | no | at most 100 items |
| `sql` | string | yes | exactly one SELECT; use $1.. for parameters; length 1-100000 |

### `agent_whoami`

Use when you need what this agent may do. Returns the agent's profile, environment ceiling, status and, per database, its environment, lanes, grants and levels. Never changes them; agent_request_capability asks for more.

Scope: `read`

No arguments.

### `agent_request_capability`

Use when agent_query is refused for a table or view you need: ask for read access for a while. Returns the verdict, request_id and, while it waits, approval_url and status. Never grants itself: an operator approves, the grant lists only columns the environment allows and it expires on its own.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `capability` | string | yes | one of `read` |
| `columns` | object | no | object → columns; an object without an entry asks for every column the environment allows |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `duration_minutes` | integer | yes | value >= 1 |
| `objects` | array of string | yes | at most 50 items |
| `reason` | string | yes | length 1-2000 |

### `upgrade_readiness`

Use when you plan a major-version upgrade. Returns pass, warn, block or unknown with up to 50 evidence rows for: tables logical replication cannot identify rows of, extensions, logical slots and subscriptions, data checksums, collation versions and indexes to reindex, removed settings and support windows. Never changes anything.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `method` | string | no | default pg_upgrade; one of `pg_upgrade`, `blue_green`, `logical` |
| `target_database` | string | no | a monitored database already at target_major to compare extensions against; length <= 63 |
| `target_major` | integer | yes | the major to upgrade to (above the server's, at most the newest pg_sage knows); value >= 1 |

### `get_inbox`

Use when you need what waits on a person: change approvals, plans, autonomy promotions, proposed facts, unsigned runbooks, agent capability requests, policy proposals. Returns the items, highest risk first, with counts by kind; a source it could not read is named in errors. Never decides: it never approves, a person decides each item on its page (href).

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases); without it, every database you may read. |
| `kinds` | array of string | no | kinds to list (default all) |
| `limit` | integer | no | items returned (default 500); counts cover every item; value 1-10000 |

### `get_workload_pack`

Use when a model needs a database's workload evidence. Returns a pack's model tier (redacted column statistics, sanitized statement texts, plan shapes, schema, settings, sparkline PNGs) and its size. Never writes; it never returns the local tier (statement text as written, common values, plan conditions). Packs are built in the dashboard.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `content_sha256` | string | yes | the pack's content hash (64 lower-case hex); pattern `^[0-9a-f]{64}$` |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `get_outcomes`

Use when you need what pg_sage changed in a window and whether it worked. Returns one row per executed change: what, why, origin, expected and measured effect, its verdict (pending until judged) and whether it can still be rolled back, plus the share with a measured effect. Never changes anything; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases); without it, every database you may read. |
| `evidence` | string | no | only the changes a decision or credited incident evidence id cites; length <= 200 |
| `limit` | integer | no | rows returned (default 500); the summary covers the window; value 1-10000 |
| `since` | string | no | window as a duration from 1s to 9600h (default 24h), such as 24h or 168h |
| `verdict` | string | no | one of `pending`, `improved`, `no_change`, `regressed`, `inconclusive`, `unverifiable`, `expired` |

### `propose_plan`

Use when a fix takes several ordered steps. Proposes a plan (goal, scope, steps and edges, each step citing a finding_id or evidence_ids that resolve) for a person to decide. Returns the stored plan, status proposed. Never runs a step and never approves: a person approves in pg_sage. At most 5 open plans per principal.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `plan` | object or string | yes | the plan as a JSON object (goal, scope, pack_hash, steps, edges), or a string holding exactly that JSON, without markdown fences |

### `list_plans`

Use when you need a database's plans. Returns them newest first with their status (proposed ones await a person). Never changes a plan; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `limit` | integer | no | value 1-500 |
| `status` | string | no |  |

### `get_plan`

Use when you need one plan in full. Returns its steps and edges, every approval and what each approval covers now. Never changes the plan; read-only.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `plan_id` | integer | yes | value >= 1 |

### `get_request_status`

Use when a held result gave you a handle and you want its state now. Returns state, decided_by_kind, outcome and next for your own request only; another principal's handle is not_found. Never decides or waits.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `handle` | string | yes | the handle a held result returned, such as change_approval:12; pattern `^(change_approval\|plan\|agent_request\|policy_exception\|change):[^\s]+$`; length <= 160 |

### `wait_for_decision`

Use when a held result gave you a handle and you want to wait for its next state instead of polling. Returns when it changes, at timeout_seconds (at most 60; state pending, next wait_for_decision) or state unknown. Never decides; at most 5 waits per principal.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `handle` | string | yes | the handle a held result returned, such as change_approval:12; pattern `^(change_approval\|plan\|agent_request\|policy_exception\|change):[^\s]+$`; length <= 160 |
| `timeout_seconds` | integer | no | how long to wait (default 30); value 1-60 |

<!-- END GENERATED MCP TOOL REFERENCE -->
