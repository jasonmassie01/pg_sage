# Natural-language EXPLAIN

`POST /api/v1/explain?database=<name>` explains one read statement of a
monitored database (operator or admin role; `explain.enabled` turns it
off). The dashboard's **Explain this query** button, on a case whose
evidence names a pg_stat_statements query, opens the same explanation.
The MCP tool `explain_query` resolves a `query_id` the same way.

## Request

| Field | Type | Notes |
|---|---|---|
| `query` | string | One read statement. Give `query` or `query_id`, not both. |
| `query_id` | integer or decimal string | A pg_stat_statements `queryid` of the named database. Send large ids as a string: a browser rounds integers past 2^53. |
| `plan_only` | boolean | Plan without running the statement. Without it, `EXPLAIN ANALYZE` runs only when pg_sage proves the statement has no side effects. |
| `params` | array of string | Values for `$n` placeholders. With `params`, a `query_id` is always planned afresh. |

A `query_id` is looked up in `pg_stat_statements` for the named database
only. An id with no statement there, including one that exists only in
another database, is a 400, and so is a server without a usable
`pg_stat_statements`. pg_sage then prefers a captured executed plan
(`sage.explain_cache`, from auto_explain) that is still fresh under the
[plan freshness rule](hint-verification.md#which-plan-a-hint-is-built-from) (`tuner.plan_max_age_minutes`, the
last ANALYZE of a relation it reads, pg_sage's last action on one). A
capture without actuals, or a stale one, is replaced by a fresh EXPLAIN of
the statement text. A captured plan is not stored in the explain result
cache.

## Response

| Field | Meaning |
|---|---|
| `summary` | One line. Without an LLM it comes from the plan rules, for example `Hash Join; sort spilled 8.2 MB to disk`. With one, the model writes it. |
| `slow_because`, `recommendations` | The plan rules' entries first, then the model's. The model can add entries, but it never removes a rule entry. An entry the rules already made is not repeated. |
| `insights` | The same entries as `{kind, text, source, symptom}`. `kind` is `slow_because` or `recommendation`, `source` is `rules` or `llm`, and `symptom` names the plan-scan symptom behind a rule entry (`disk_sort`, `hash_spill`, `bad_nested_loop` and so on). |
| `node_breakdown[].time_ms` | The node's inclusive time over all its loops: `Actual Total Time x Actual Loops`. An inner nested-loop node that took 0.2 ms per loop over 5,000 loops shows 1,000 ms. Before tuning-10 this was the per-loop time. |
| `node_breakdown[].loops` | `Actual Loops`; absent when the plan was not executed. |
| `node_breakdown[].warning` | Set when actual and estimated rows per loop differ by 10x or more, in either direction. A value below 1 counts as 1, and a node that never ran does not warn. |
| `actual_time_ms` | The statement's `Execution Time`, else the root node's time. |
| `plan_source` | `explain` for a plan made now, `explain_cache` for a captured plan. |
| `plan_capture` | For a captured plan, its capture source (for example `auto_explain`). |
| `plan_age_s` | The plan's age in seconds: 0 for a plan made now. A result served from the explain cache counts the time since it was stored. |

The plan rules are the query tuner's plan scan (disk sorts, hash spills,
misestimated nested loops, seq scans with a usable index, serial scans of
large tables, sorts feeding a small LIMIT, and index or join filters that
discard most rows). They need no LLM. A plan without actuals skips the
rules that need an executed plan. Results cached before this change have no
`insights` field until they expire (`explain.cache_ttl_minutes`).
