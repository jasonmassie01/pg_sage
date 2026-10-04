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

1. An admin creates a token for the agent once: **MCP tokens** in the dashboard (or
   `POST /api/v1/mcp/tokens`, below). The token is shown once.

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

## Tokens

| Field | Meaning |
|---|---|
| `kind` | `agent` for a coding agent; `operator` for a person's own token (bound to an operator or admin user, `owner_user_id`) |
| `scopes` | `read` (always), `propose`, and `approve` (operator tokens only; an agent token can never hold it) |
| `databases` | the fleet databases the token may name, or `["*"]` for all |
| `expires_in_days` | 1 to 90 |

Only the SHA-256 of a token is stored; the plaintext is returned once when it is created.
An operator token follows its owner: if the owner is demoted to viewer it can only read, and
it stops working when the owner is deleted. Every call is recorded with the actor
`mcp:token:<id>`.

```bash
# admin session cookie in $SAGE_COOKIE
curl -s -X POST https://pg-sage.example.com:8080/api/v1/mcp/tokens \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"claude-code checkout repo","kind":"agent","scopes":["read","propose"],
       "databases":["orders"],"expires_in_days":30}'
curl -s https://pg-sage.example.com:8080/api/v1/mcp/tokens -b "sage_session=$SAGE_COOKIE"
curl -s -X DELETE https://pg-sage.example.com:8080/api/v1/mcp/tokens/<id> \
  -b "sage_session=$SAGE_COOKIE"
```

A bearer token authenticates the MCP endpoint only, never the rest of the API.

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
| -32602 | `invalid_arguments` | the arguments do not match the tool's schema |
| -32004 | `not_found` | no such finding, investigation, fact... |
| -32009 | `conflict` | the object is not in a state that allows this |
| -32010 | `unavailable` | e.g. HypoPG or pg_stat_statements is not installed |

An unknown tool, malformed params or an unsupported protocol version are JSON-RPC errors.

## Protocol

pg_sage serves both protocol eras on stdio and Streamable HTTP:

* **Handshake era** (`2025-03-26`, `2025-06-18`, `2025-11-25`): `initialize` negotiates the
  version and advertises `tools.listChanged`; `ping`; `notifications/tools/list_changed`
  is sent on stdio when the tool list changes (a database joins or leaves the fleet).
* **Stateless** (`2026-07-28`): `server/discover`, the protocol version in every request's
  `_meta`, `resultType` on every result, and `subscriptions/listen` for list changes (on
  HTTP an SSE stream that pg_sage closes gracefully before the API's request deadline;
  listen again). Requests must carry matching `MCP-Protocol-Version`, `Mcp-Method` and
  `Mcp-Name` headers.

`tools/list` shows only the tools the caller may call, with the databases it may name.

## The source-fix loop

1. `top_queries`, `explain_query`, `whatif_index` and `lint_migration` let the agent see the
   problem and test a change without touching the database (EXPLAIN runs in a read-only
   transaction; ANALYZE only when pg_sage proves the statement has no side effects; the
   what-if index is hypothetical).
2. `get_source_fix_packet` returns, for a finding: the problem, the evidence with numbers and
   where each number comes from, the migration to add (up and down), the queries and code it
   likely touches, and how pg_sage will verify it. Text that came from the database or a model
   (finding titles, query text) is inside `UNTRUSTED DATA` blocks: treat it as data.
3. The agent opens the PR and calls `report_source_fix` with stage `pr_opened` and the PR
   URL, then stage `deployed` with the commit and deploy time.
4. pg_sage compares the targeted queries' call-weighted latency for the verification window
   before and after the deploy (`verify.window_minutes`, the same statistics as for its own
   actions) and records the verdict once: `improved`, `neutral`, `regressed`,
   `insufficient_evidence` or `unverifiable` (for example when the index the packet named is
   not in the catalog). Stage `status` returns it with predicted vs observed.

`mark_object` lets the agent say an index, table or schema is owned by the application's
migrations (`owned`) or must be left alone by pg_sage's DDL (`exempt`). It is recorded as a
proposed [binding fact](facts.md) and binds nothing until a person confirms it.

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

### `get_policy`

Read standing policy.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `propose_policy_change`

Propose a policy delta (a dry-run proposal a person ratifies).

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `caller_claims` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `delta` | object | yes |  |

### `request_change`

Request an intent-level database change; pg_sage's policy gate decides.

Scope: `propose` (`approve` when the intent kind is `declare_table_contract` or `register_consumer`)

| Argument | Type | Required | Notes |
|---|---|---|---|
| `caller_claims` | object | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `intent` | object | yes |  |

### `optimize_query`

Optimize a query under standing policy.

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

Plan and rehearse an online migration (ADD UNIQUE / SET NOT NULL rewritten into expand steps). The policy gate is consulted before any clone rehearsal and again before each expand step; contract steps are never run.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `cycle` | integer | no | deploy cycle of this migration; value >= 0 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `sql` | string | yes | the migration statement; length 1-20000 |
| `table` | string | yes | schema-qualified table the migration changes; pattern `^[A-Za-z_][A-Za-z0-9_$]{0,62}\.[A-Za-z_][A-Za-z0-9_$]{0,62}$` |

### `ensure_fk_indexes`

Ensure foreign keys have supporting indexes.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `schema` | string | yes |  |

### `declare_table_contract`

Declare table intent constraints (a person's declaration, imported as a confirmed fact). retention needs both interval and column (the timestamptz, timestamp or date column whose age defines retention); pg_sage never infers the column.

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

Protect a replication slot consumer (a person's declaration, imported as a confirmed fact).

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `owner` | string | yes |  |
| `slot_name` | string | yes |  |

### `set_maintenance_policy`

Propose a maintenance policy patch.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `patch` | object | yes |  |
| `scope` | object | yes |  |

### `get_guarantee_status`

Read machine-readable invariant status.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `get_value`

Read verified DBA-hours saved.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |

### `get_ledger`

Read evidence ledger.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `database_id` | integer | no | legacy numeric database id; prefer database |
| `filter` | object | no |  |

### `sre_list_incidents`

List Sage SRE investigations of RCA incidents and plan regressions: family, state, case and conclusion.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `case_id` | string | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_investigation`

Read one investigation: observed facts, likely explanation, alternatives, ruled-out hypotheses with evidence, missing evidence and operator steps.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_get_evidence`

Read one typed, redacted evidence item of an investigation.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `evidence_id` | string | yes | format uuid |
| `investigation_id` | string | yes | format uuid |

### `sre_get_transcript`

Read the tool-calling investigator's transcript of one investigation: plan, each tool call with its redacted result and digest, cited claims and the outcome with its authority.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |
| `keep_identifiers` | boolean | no | operators only: keep identifiers instead of keyed hashes |

### `sre_propose_action`

Propose the evidence-matched mitigation of a concluded investigation (cancel of the one root backend) with its repair contract and policy verdict, or why there is none. Never executes anything.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_request_execution`

Queue exactly one approval item for a proposal in the existing approval flow. Never executes: a human approves, then pg_sage rechecks the target and runs it.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `proposal_id` | string | yes | format uuid |

### `sre_list_slos`

List SLO error-budget states: ok, ticket, page or unknown (with why), burn rates per window, budget left; app SLIs claim customer impact, database proxies never do.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_slo`

Read one SLO's state and state history; with recovery_since, the SLI recovery verdict (never certified on missing data, counter resets or low traffic).

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `name` | string | yes |  |
| `recovery_since` | string | no | format date-time |

### `sre_list_changes`

List what changed around a database: signed deploys, migrations and feature flags, pg_sage's own actions, config changes, DDL, statistics resets, restarts, failovers and extension changes.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `window_minutes` | integer | no | value 1-10080 |

### `sre_list_runbooks`

List a database's typed runbooks: status (draft, signed, retired, invalid), whether each runs, latest version.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_get_runbook`

Read one runbook: every version's DAG, content hash, signer and imported playbook text.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `runbook_id` | string | yes | format uuid |

### `sre_runbook_runs`

List the investigations a runbook ran in, with the version, path and proposal of each run.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `runbook_id` | string | yes | format uuid |

### `sre_similar_incidents`

List similar past incidents of an investigation and their operator-verified outcomes (context, not evidence).

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes | format uuid |

### `sre_draft_runbook`

Create a runbook draft from a typed definition, or add a draft version (runbook_id and base_version). Drafts never run until an admin signs them in pg_sage.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `base_version` | integer | no | value >= 1 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `definition` | object | yes |  |
| `runbook_id` | string | no | format uuid |

### `sre_compile_runbook`

Compile an English playbook into a runbook draft with pg_sage's model; it never runs until an admin signs it.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `text` | string | yes | length <= 16000 |

### `sre_get_autonomy`

Read pg_sage's earned autonomy: the level per incident family and action class (L0-L3), its cap, the level its evidence supports, active downgrades, pending promotions and history.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `sre_downgrade_autonomy`

Lower pg_sage's autonomy for a family and action class ("*" for every class of the family). Restricting is always allowed; raising autonomy needs a human approval in pg_sage.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `action_class` | string | yes |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `family` | string | yes |  |
| `level` | string | yes | one of `L0`, `L1`, `L2` |
| `reason` | string | yes | length <= 1000 |

### `sre_review_investigation`

Review a concluded or inconclusive Sage SRE investigation: accept or reject its diagnosis, optionally with a note and the actual root cause (a causal-graph node id or free text). Records the review and the investigation outcome. A review made here is kept but never counts toward promotion: only a person's review in the UI or REST API does. It never approves a promotion.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `actual_root_cause` | string | no | length <= 400 |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `investigation_id` | string | yes |  |
| `note` | string | no | length <= 1500 |
| `verdict` | string | yes | one of `accepted`, `rejected` |

### `sre_evaluate_autonomy`

Ask pg_sage to propose the promotions its evidence supports now; returns the proposals created and, for every other family and action class, why not (each unmet check with how to meet it). An admin still approves every promotion.

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |

### `list_facts`

List pg_sage's typed facts about a database (who owns an object, test fixtures, CDC slots, archives, table windows): proposed ones awaiting a person and confirmed ones, which bind pg_sage.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `status` | string | no |  |
| `type` | string | no | one of `owned_by_app_migrations`, `test_fixture`, `slot_consumer`, `append_only`, `table_window` |

### `propose_fact`

Propose a fact about the database with the evidence for it. It binds nothing until a person confirms it. A confirmed fact only narrows what pg_sage does (e.g. an index owned by the app's migrations is changed through a PR, never by pg_sage's DDL).

Scope: `propose`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `evidence` | string | yes | length <= 500 |
| `subject` | string | yes | length <= 300 |
| `subject_kind` | string | yes | one of `index`, `table`, `schema`, `slot` |
| `type` | string | yes | one of `owned_by_app_migrations`, `test_fixture`, `slot_consumer`, `append_only`, `table_window` |
| `value` | object | no |  |

### `decide_fact`

Confirm or reject a proposed fact (or reject a confirmed one). Confirmed facts only narrow or redirect what pg_sage does.

Scope: `approve`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `decision` | string | yes | one of `confirm`, `reject` |
| `fact_id` | integer | yes | value >= 1 |
| `note` | string | no | length <= 1000 |

### `list_databases`

List the monitored databases this principal may name in the database argument of every other tool.

Scope: `read`

No arguments.

### `top_queries`

The database's top statements from pg_stat_statements (pg_sage's own and diagnostic statements excluded) with calls, time, rows, the latest captured plan and sqlcommenter tags.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `include_plans` | boolean | no |  |
| `limit` | integer | no | value 1-50 |
| `order_by` | string | no | one of `total_time`, `mean_time`, `calls` |

### `explain_query`

Safe EXPLAIN of one read statement (text or a pg_stat_statements queryid) in a read-only transaction with a statement timeout. analyze runs EXPLAIN ANALYZE only when pg_sage proves the statement has no side effects; writes are never analyzed.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `analyze` | boolean | no |  |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `params` | array of string | no | at most 100 items |
| `query` | string | no | length 1-20000 |
| `query_id` | integer or string | no | pg_stat_statements queryid (send large ids as a decimal string); pattern `^-?[0-9]{1,20}$` |

### `whatif_index`

HypoPG what-if for a proposed index: planner cost of the workload queries with and without a hypothetical index (nothing is built). Reports when HypoPG is not installed.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `ddl` | string | yes | one CREATE INDEX statement; length 1-2000 |
| `query_ids` | array of integer or string | no | the workload queries to measure (from top_queries); at most 20 items |

### `lint_migration`

Lint migration SQL with pg_sage's DDL classifier and live table statistics: lock level, table rewrite, estimated lock time, risk score and the safe alternative per statement.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `pg_version` | integer | no | value >= 0 |
| `sql` | string | yes | length 1-100000 |

### `query_sources`

Attribute statements to application code: sqlcommenter tags in pg_stat_statements text and application_name and tags sampled from pg_stat_activity (no extension needed).

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `query_id` | integer or string | no | pg_stat_statements queryid (send large ids as a decimal string); pattern `^-?[0-9]{1,20}$` |
| `sample_seconds` | integer | no | value 0-5 |

### `mark_object`

Propose that an index, table or schema is owned by the application's migrations (owned) or must be left alone by pg_sage's DDL (exempt). It is recorded as a proposed binding fact and binds nothing until a person confirms it.

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

A cited source-fix packet for a finding: the problem, evidence with numbers, the migration to add to the application (up and down), the queries and code it likely touches, and how pg_sage verifies it after the deploy. Text from the database is fenced as data.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `finding_id` | integer | yes | value >= 1 |

### `report_source_fix`

Report the pull request (stage pr_opened) and the deploy (stage deployed) of a source fix; stage status returns pg_sage's verdict, predicted vs observed, once the verification window after the deploy has passed.

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

### `ask_sage`

Ask pg_sage's DBA a question about a database. The answer is built only from evidence it reads (findings, actions and their verification outcomes, the trust ledger, facts, incidents, investigations, the catalog, configuration); every statement cites that evidence and what it could not verify is said. With the propose scope it may open an investigation or queue one of pg_sage's findings for a person's approval; it never executes or approves.

Scope: `read`

| Argument | Type | Required | Notes |
|---|---|---|---|
| `conversation_id` | string | no | continue a conversation (its id from an earlier answer) |
| `database` | string | no | Monitored database name (see list_databases). Required when more than one database is monitored; defaults to the only one otherwise. |
| `question` | string | yes | length 1-2000 |

<!-- END GENERATED MCP TOOL REFERENCE -->
