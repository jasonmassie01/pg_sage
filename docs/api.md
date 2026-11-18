# HTTP API

The dashboard and the CLI use the JSON API under `/api/v1`. Every route needs a
signed-in user (or, in a Databricks App, the platform identity); the role gates
are viewer, operator and admin. This page documents the routes that span
pg_sage's features. Each feature's own page documents its routes.

## Inbox

A person who must decide something finds every waiting decision in one list:
the inbox. It reads eight sources across every database the caller may read:

| Kind | What waits | Decided on (Href) | Who decides |
|---|---|---|---|
| `change_approval` | A pending, unexpired action in the approval queue | `#/actions?tab=pending&queue=<id>&database=<db>` | operator |
| `plan_approval` | A plan proposed or halted, or a step waiting on its operator or its own approval | `#/actions?tab=plans&plan=<id>&database=<db>` | operator |
| `autonomy_promotion` | An earned-autonomy promotion the trust ledger proposed | `#/trust?database=<db>&proposal=<id>` | admin |
| `fact` | A proposed fact | `#/facts?database=<db>&fact=<id>` | operator |
| `runbook_signature` | A runbook whose latest version is an unsigned draft | `#/advanced/runbooks?database=<db>&runbook=<id>` | admin |
| `agent_request` | An agent's unexpired pending capability request | `#/agents?agent=<id>&request=<id>&database=<db>` | operator |
| `policy_exception` | A policy proposal waiting for ratification, an agent's `propose_policy_change` among them | `#/agents?tab=policy&proposal=<id>&database=<db>` | admin |
| `policy_exception` | A policy-set exception in force on the database (id `exception-<database_id>`): moderate without an expiry, safe and due at its expiry otherwise | `#/settings?tab=policy-sets&database=<db>` | admin |
| `advice` | A sprawl census TTL proposal for an unadopted discovered member with no real activity in 30 days (fleetpol-03); read once for the whole fleet, observe-only members included, and kept to the databases the caller may read | `#/manage-databases?database=<db>` | operator |

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

### `GET` and `POST /api/v1/inbox/seen`

The time the signed-in person last saw the Inbox, kept on their user row
(`sage.users.inbox_seen_at`), so it follows them across devices. `GET` answers
`{"seen_at": "<RFC 3339>"}`, or `{"seen_at": null}` on a first visit. `POST`
takes an optional `{"seen_at": "<RFC 3339>"}` (the time the page showed; absent
means now) and answers the stored time. The time only moves forward: an
earlier time, or two tabs marking at once, leaves the later one, and a time
after the server's clock is stored as now. Every role may read and mark its
own time and no one else's; it changes no monitored database. Without a
control database the routes are absent (404). Errors: 400 for a body that is
not that object, 401 without a signed-in user, 404 when the user row is gone.

### The Inbox page

The dashboard's home page (`#/`) is the Inbox. **Needs you** lists the inbox
items; a change approval opens its approval card in place, a proposed fact is
confirmed or rejected in place and an autonomy promotion is approved in place
after a confirm (a HIGH one needs the typed phrase). Every other kind links to
its own page. Each decision goes to the item's own route with
`X-Sage-Surface: inbox`. **Since you were last here** lists the outcome ledger
rows executed since `inbox_seen_at` (the last 24 hours on a first visit, said
so on the page), each once; a row whose rollback is `available` offers
**Undo**, which loads the action, shows its stored rollback SQL and asks for a
reason. When nothing waits and every source answered, the page says so from
counts, for example "Nothing needs you. 6 changes since Tuesday, all
verified." When a source could not be read the page names it ("Could not
read: autonomy promotions") and never claims that nothing waits. The page
marks itself seen after 5 seconds visible. The Value page moved to `#/value`;
the Inbox shows one Value line linking to it. The one nav badge is on Inbox:
`/api/v1/inbox/count` `total`, `N+` for a lower bound and `99+` above 99.

Rollbacks from the Inbox and from Actions require a reason (3 to 500
characters), sent as `reason` in `POST /api/v1/actions/{id}/rollback` with
`record_kind`; the ledger and the action history keep it.

Actions takes exact links: `#/actions?tab=<key>` opens a tab
(`recommendations`, `pending`, `progress`, `executed` or `decisions`) and
`queue=<id>` outlines a queued proposal's approval card on the review tab;
`action=<id>` still focuses an executed change. An unknown tab or a malformed
id is ignored.

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

## Outcome ledger

The outcome ledger answers "what did pg_sage change, and what did it do": one
row per change pg_sage executed, with what it was, why, how it came to run,
the effect it expected, the effect the verifier measured, the verdict and
whether the change can still be rolled back. It reads `sage.action_log`,
lab-01's verdict record (`sage.action_outcome`), the decision, the approval
card and the plan step, and writes nothing. A failed action changed nothing
and is not a row. The dashboard shows it at `#/ledger`.

### `GET /api/v1/outcomes`

Viewer and up. Query parameters, all optional:

- `since`: the window, a duration from `1s` to `9600h` (the 400-day history
  retention), such as `24h` (the default) or `168h`. Anything else is `400`
  with the range in the message. A change executed exactly `since` ago is in
  the window.
- `database`: one monitored database. An unknown database, or one your grants
  do not cover, is `404 database not found`; a malformed name is `400`.
- `verdict`: one of `pending`, `improved`, `no_change`, `regressed`,
  `inconclusive`, `unverifiable`, `expired`; anything else is `400`.
- `evidence`: only the changes a decision's or a credited incident's evidence
  id cites (at most 200 characters).
- `limit`: the rows returned, from 1 to 10000 (default 500). The summary
  covers every change in the window.

```json
{
  "since": "24h",
  "rows": [
    {"action_id": 12, "database": "orders", "action_type": "create_index",
     "what": "Add an index", "why": "orders lookups read 1.2 GB per call",
     "origin": "approved",
     "expected": {"metric": "blocks_per_call", "change_pct": -40, "source": "hypopg"},
     "measured": {"metric": "blocks_per_call", "change_pct": -42},
     "verdict": "improved", "rollback_state": "available",
     "evidence_id": "ev-orders-1", "finding_id": 9, "saved_hours": 0.75,
     "executed_at": "2026-11-16T03:00:00Z"}
  ],
  "summary": {"changes": 1, "verified": 1, "reverted": 0, "inconclusive": 0,
              "pending": 0, "measured": 1, "measured_share": 1},
  "truncated": false,
  "errors": []
}
```

- `origin` is `plan` (a step of an approved change plan), `approved` (a person
  approved it), `agent` (an agent ran it under its grant) or `autonomous`
  (pg_sage ran it under policy and trust).
- `expected` is the prediction stored before the change ran, else the plan
  step's expectation; `null` when there was none.
- `measured` is the verifier's measured change in percent, to one decimal
  place. It is `null` with `measured_reason` while the change is not judged
  yet, or where the verifier declares the target unverifiable (for example a
  scale-to-zero suspend broke the counters' epoch).
- `verdict` is `pending` until the verifier decides, then one of the verdicts
  above (lab-01's verdicts in the plan engine's words: `neutral` is
  `no_change`, `insufficient_evidence` is `inconclusive`).
- `rollback_state` is `available`, `rolled_back`, `rollback_failed`,
  `rolling_back` (a rollback still running) or `not_reversible` (no rollback
  statement).
- `summary.verified` counts changes judged on a measurement (improved, no
  change, regressed); `inconclusive` counts inconclusive, unverifiable and
  expired ones; `measured_share` is `measured / changes` to three places and
  `null` (not 0) for a window with no changes.
- `truncated` is `true` when the window held more than 10000 changes on a
  database: the summary then covers the newest 10000.
- A database that could not be read is named in `errors`
  (`{database, error}`, for example `read outcomes: orders: not connected`);
  its changes are missing, never shown as none. The details are in the
  sidecar's log.

With `api.database_grants` on, a user scoped by grants sees only the databases
they are granted.

### Over MCP and the CLI

`get_outcomes` answers the same payload to an agent ([MCP](mcp.md#get_outcomes));
`get_ledger` still reads policy decisions. `pg_sage ledger` prints one
database's ledger, one line per change, or this payload with `--json`:

```sh
pg_sage ledger --since 24h [--database NAME] [--verdict V] [--limit N] [--json] [--dsn DSN]
```

It reads the pg_sage tables of the database named by `--dsn`, else
`SAGE_HISTORY_MONITORED_DSN`, else `SAGE_META_DB` (like `pg_sage history`);
`--database` names it in the output (default: the DSN's database name). It
only reads, exits 0 for a read and 2 for a usage error or a database it could
not read (the error goes to stderr and nothing to stdout).

`pg_sage_outcomes_measured_ratio{database}` is the share of the last 24 hours'
changes with a measured effect at the last complete ledger read of that window
([Metrics](metrics.md)).

## Sage Review grade

### `GET /api/v1/databases/{db}/review/grade`

Viewer; read scope on `{db}`. The latest graded run of the database (`done` or `partial`),
the one before it and the delta between them (see [Sage Review](review.md)). Before the
first grade, `latest`, `previous` and `delta` are null; the first grade's delta is the
whole report (`first_run: true`). It never starts a grade. The MCP tool `review_grade` is
the same read. `why_now` is the triggers the latest run consumed or coalesced, oldest first,
each with its `kind`, `detail`, `fired_at`, `state`, `run_id` and `coalesced_into` (empty
for a grade and before the first run). A database that is not connected is 503
`database_unavailable`; one that is not monitored is 404.

```json
{"latest": {"id": 12, "database": "orders", "kind": "grade", "triggers": ["daily"],
  "rubric_version": "pg_sage-rubric/1", "status": "done",
  "grade": {"overall": "B", "overall_score": 84.5, "coverage_pct": 77.1,
    "known_dimensions": 6, "dimensions": [{"dimension": "schema", "letter": "A",
      "score": 92.31, "coverage": 100, "checks": [{"check": "schema.duplicate_index",
        "status": "pass", "weight": 2, "evidence": ["first_look:40#duplicate_index"]}]}]}},
 "previous": {"id": 11},
 "delta": {"first_run": false, "rubric_changed": false, "from_overall": "B",
  "to_overall": "B", "dimensions": []},
 "why_now": []}
```

The Review page reads three more fields. `running` is the run in progress (`id`, `kind`,
`started_at`) or null. `report` is the latest grade's report: `run_id`, `model_pass`
(`status` with `model` or `reason`; a grade-only run is `skipped`) and `findings`, the failed
then warned checks ranked (critical first, then by weight, then rubric order), each with
`rank`, `dimension`, `dimension_title`, `check`, `status`, `weight`, `reason` and `evidence`.
`viewed_at` is when the signed-in caller first opened the latest grade, or null.

### `POST /api/v1/databases/{db}/review/runs/{id}/viewed`

Any signed-in role, scoped like a read of `{db}`; no body. Records the caller's first opening
of a finished review (`done` or `partial`) in `sage.review_view`, once per run and user, and
answers `{"run_id": 12, "viewed_at": "2026-11-16T10:00:00Z", "first": true}`; a repeat
answers the same time with `first: false`. It decides nothing and changes no monitored data.
An id that is not a positive integer is 400, a request without a user id 401, an unknown
database or run 404, a running or failed run 409, a disconnected database 503.

## Sage Review scorecard

### `GET /api/v1/databases/{db}/review/scorecard`

Viewer; read scope on `{db}`. The database's review scorecard since `since` (RFC 3339; default
the last 90 days), the newest G-M1 and G-M2 evaluations of the model gate (`gate`, empty
before the first) and the thresholds (see
[the model gate](review.md#scorecard-and-the-model-gate)). `by_origin` and `by_generator` hold
the counts; `per_review` is each review, newest first. A bad `since` is 400. The MCP tool
`review_scorecard` is the same read.

```json
{"scorecard": {"database": "orders", "reviews": 50,
  "by_origin": {"model": {"proposed": 50, "accepted": 50, "verified_improved": 45,
    "no_change": 5, "inconclusive": 0, "regressed": 0, "reverted": 0, "unverifiable": 0,
    "pending": 0, "decided": 50}},
  "by_generator": {"indexes": {"proposed": 50, "verified_improved": 50, "decided": 50}},
  "model_only_accepted": 50, "per_review": []},
 "gate": [{"id": 7, "model": "m1", "rule": "G-M1", "verdict": "pending_approval",
   "reason": "G-M1: 50 of 50 reviews; model 45/50 improved (90.0%) vs generators ...",
   "evidence": {"reviews": {"k": 50, "n": 50}, "model_improved": {"k": 45, "n": 50}}}],
 "thresholds": {"min_reviews": 50, "max_gap_points": 10, "max_revert_pct": 2}}
```

### `POST /api/v1/databases/{db}/review/gate/{id}/approve`

Admin. Approves the live G-M1 promotion `{id}` (the inbox's `autonomy_promotion` item from
`sage.review_gate`): the row becomes `granted` with the approver, and the model's review steps
run at their class's trust level. Only the newest G-M1 evaluation, still pending, can be
approved: an older or already decided one is 409, an unknown id 404, a malformed id 400.
Nothing else grants model authority; a failed window or a model change revokes it at the next
evaluation.
