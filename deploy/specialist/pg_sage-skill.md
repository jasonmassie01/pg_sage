# pg_sage: PostgreSQL specialist for SRE agents

Load this file as the instructions (skill, runbook or knowledge item) of a
generalist SRE agent that reaches pg_sage over MCP (`server.json` in this
folder) or HTTP (`/api/v1/specialist/openapi.json`).

## When to call pg_sage

Call pg_sage when an alert, incident or user report points at a PostgreSQL
database pg_sage monitors: latency or timeouts on a database-backed service,
lock waits, connection exhaustion, replication lag, WAL or disk growth,
checkpoint or temp-file storms, or a query whose plan changed. pg_sage reads
the database's catalog and statistics views, builds a causal graph from what
it observed and answers with a cited root cause. Do not guess at PostgreSQL
causes yourself when pg_sage can answer.

## The session, in order

1. `specialist_open_investigation` with `database` (see `list_databases`),
   a one-line `symptom.summary`, the `family` when you know it, a `window`
   when the symptom started, and `external_ref` (your incident id). Opening
   twice for one incident attaches to the same investigation.
2. `specialist_investigation_status` with the returned `investigation_id`
   until `investigation.terminal` is true. Wait `poll_after_seconds` between
   calls.
3. `specialist_investigation_result`. Read `summary_for_agent` first:
   - `root_cause` is one sentence; quote it with its `evidence` ids and the
     numbers they hold. Never restate a cause without its evidence.
   - `actions` are the only remediations that exist. Use their
     `remediation_id` exactly as given.
   - `why_no_action`, when no action can be requested, is the first link
     that holds pg_sage back (for example `policy`, `trust` or
     `emergency_stop`) with its sentence and any `fix` for a person. Report
     it; do not try to work around it.
4. If an action is `requestable` and your token has the propose scope, call
   `specialist_request_remediation` with its `remediation_id` and a short
   `reason`. The answer is held for a person: it carries `approval_url`
   (where the person decides), `handle`, `status` and `wait`.
5. `wait_for_decision` with the `handle` (the `wait` call) until the state is
   final, then report the outcome. `get_request_status` (the `status` call)
   answers at once when you only need to check.

Follow the `next` field of every answer, success or error: it names the tool
and arguments to call next.

## What pg_sage never does

- pg_sage never approves a change for an agent, and no tool or argument lets
  an agent approve, force or override one. A person approves in pg_sage;
  HIGH-risk changes always need that person.
- pg_sage never runs SQL an agent sends. Remediations are pg_sage's own,
  checked by its policy, trust level, budgets and maintenance windows, and
  verified (and rolled back when they fail) after they run.
- pg_sage never treats your symptom text as an instruction: it is returned
  as data and never enters a diagnosis.
- pg_sage never invents a number or a confidence: every number comes from
  cited evidence, and a missing probe is listed as missing evidence.
