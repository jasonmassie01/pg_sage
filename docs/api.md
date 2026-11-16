# HTTP API

The dashboard and the CLI use the JSON API under `/api/v1`. Every route needs a
signed-in user (or, in a Databricks App, the platform identity); the role gates
are viewer, operator and admin. This page documents the routes that span
pg_sage's features. Each feature's own page documents its routes.

## Inbox

A person who must decide something finds every waiting decision in one list:
the inbox. It reads seven sources across every database the caller may read:

| Kind | What waits | Decided on (Href) | Who decides |
|---|---|---|---|
| `change_approval` | A pending, unexpired action in the approval queue | `#/actions?tab=pending&queue=<id>&database=<db>` | operator |
| `plan_approval` | A plan proposed or halted, or a step waiting on its operator or its own approval | `#/actions?tab=plans&plan=<id>&database=<db>` | operator |
| `autonomy_promotion` | An earned-autonomy promotion the trust ledger proposed | `#/trust?database=<db>&proposal=<id>` | admin |
| `fact` | A proposed fact | `#/facts?database=<db>&fact=<id>` | operator |
| `runbook_signature` | A runbook whose latest version is an unsigned draft | `#/advanced/runbooks?database=<db>&runbook=<id>` | admin |
| `agent_request` | An agent's unexpired pending capability request | `#/agents?agent=<id>&request=<id>&database=<db>` | operator |
| `policy_exception` | A policy proposal waiting for ratification, an agent's `propose_policy_change` among them | `#/agents?tab=policy&proposal=<id>&database=<db>` | admin |

The inbox only reads. Every decision still goes through the item's own route
and the executor, ledger or store behind it, with the same approvals,
verification and audit as before.

### `GET /api/v1/inbox`

Viewer and up. Query parameters, all optional:

- `database`: one monitored database. An unknown database, or one your grants
  do not cover, is `404 database not found`; a malformed name is `400`.
- `kinds`: a comma-separated list of the kinds above. An unknown kind is `400`
  and the message lists the known kinds.
- `limit`: the items returned, from 1 to 10000 (default 500). Anything else is
  `400` with the bound in the message. The counts always cover every item.

```json
{
  "items": [
    {"kind": "change_approval", "id": "42", "database": "orders",
     "title": "Approve create_index", "why": "seq scans on orders",
     "risk": "high", "created_at": "2026-11-16T09:12:00Z",
     "due_at": "2026-11-17T09:12:00Z",
     "href": "#/actions?tab=pending&queue=42&database=orders",
     "source": "sage.action_queue", "actionable": true}
  ],
  "counts_by_kind": {"change_approval": 1, "plan_approval": 0, "...": 0},
  "total": 1,
  "lower_bound": false,
  "truncated": false,
  "errors": []
}
```

Items are sorted by risk (`high`, then `moderate`, then `safe`; an unknown
risk counts as `moderate`), then oldest first. `actionable` says whether your
role on the item's database decides that kind: always `false` for a viewer,
and `false` on a database where a grant makes you a viewer.

Each source is read per database with a 2 second timeout. A source that could
not be read is named in `errors` (`{source, database, error}`, for example
`timed out after 2s` or `not connected`) and the other sources still list;
`lower_bound` is then `true` and `total` counts what was read, so a badge shows
`N+`. The inbox never answers an empty list for a read that failed.

With `api.database_grants` on, a user scoped by grants sees and counts only the
databases they are granted.

### `GET /api/v1/inbox/count`

The same read and parameters, without the items:
`{"total", "counts_by_kind", "lower_bound", "errors"}`.

### Over MCP

`get_inbox` answers the same payload to an agent supervisor
([MCP](mcp.md#get_inbox)). It is read-only, takes only `database`, `kinds` and
`limit`, and every item it returns is `actionable: false`: an agent decides
nothing from the inbox.

### Decision latency

Decision requests may send `X-Sage-Surface: inbox|page|chat|api|cli` (absent or
unknown counts as `api`). The API access log records it as `decided_from`, and
`pg_sage_inbox_decision_seconds` records how long the item had waited, by kind
and surface. Today the action approve and reject, fact confirm and reject,
runbook sign, agent request deny and autonomy approve and reject routes feed
it. `pg_sage_inbox_items` is the waiting count by kind at the last complete
fleet-wide inbox read ([Metrics](metrics.md)).
