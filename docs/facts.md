# Binding facts

Some things about a database are true for a long time and cost a lot when pg_sage gets
them wrong: an index belongs to the application's migrations, a family of schemas is left
over from test runs, a replication slot feeds a change-data-capture pipeline. pg_sage keeps
these as **typed facts**. You confirm a fact once; from then on it binds what pg_sage does.

A confirmed fact can only **narrow or redirect** what pg_sage does on its own. It never
grants autonomy, never lets an action skip a check, and never makes pg_sage do something it
would not have done anyway.

## Fact types

| Type | Subject | What it means | What pg_sage does |
|---|---|---|---|
| `owned_by_app_migrations` | index, table or schema | The application's schema migrations create and change it | Never creates, drops or alters it. The change becomes a **source-fix packet**: the migration pg_sage recommends, attached to the finding, for you or a coding agent to put in a PR. |
| `test_fixture` | schema pattern | Schemas left by test runs, not workload | Removes them from findings and from the budgets, takes no action on them by itself, and offers one **cleanup batch** (a reviewed script you run once). pg_sage never drops schemas itself. |
| `slot_consumer` | replication slot | The slot feeds a named consumer (Debezium, Fivetran, Airbyte...) | Never drops or advances the slot and never bounds WAL in a way that could invalidate it; it alerts instead. `max_slot_wal_keep_size` is cluster-wide, so any finite bound (and `RESET`) is blocked whichever slot it names; only `-1` (unbounded) is allowed. A confirmed slot is also registered in `sage.slot_consumer_registry`, so the WAL custodian escalates instead of acting. |
| `append_only` | table | Rows are only inserted (an archive or a log) | Never deletes its rows, rewrites it (`VACUUM FULL`, bloat plans) or drops its indexes (they look unused because an archive is rarely read). |
| `table_window` | table | `maintenance` window (act only inside) or `batch` window (never act during) | Holds pg_sage's own work on the table until the window allows it. Your own approvals are not held. |

## Subjects

* Index and table subjects are schema-qualified: `public.idx_orders_status`, `app.orders`.
* Identifiers fold like PostgreSQL's: unquoted names are lower-cased, quoted names are exact
  (`"Billing"."Invoices"` is not `billing.invoices`).
* `*` matches any run of characters (`test_memory_*`, `public.idx_thesis_*`); nothing else
  is a wildcard.
* A subject that could match pg_sage's own `sage` schema or a system schema (`*`, `s*`,
  `pg_*`, `*.orders`) is refused.

## Where facts come from

* **Detectors** propose facts from deterministic evidence:
  * an index pg_sage dropped that came back with the same definition (the application's
    migrations recreated it);
  * test-named schemas (`test_*`, `tmp_*`, `*_test`, ...) with no scans or writes for six
    hours, as one pattern per family of copies;
  * logical replication slots and their consumer;
  * tables with a million inserts and no update or delete.
* **The model** (when an LLM is configured) proposes facts from a bounded summary of the
  catalog, at most once a day. Every model proposal must cite the evidence it was shown and
  passes the same validation as any other; it is only ever a proposal.
* **You** can declare a fact directly; your declaration confirms it.
* What you already declared with the MCP tools `register_consumer` and
  `declare_table_contract` (append-only) is imported as confirmed facts.

Nothing proposed binds anything until a person confirms it. A rejected fact stays rejected
when it is proposed again (the proposal count grows); new evidence reopens only an expired
fact.

## Confirming and rejecting

* **Facts page** in the UI: proposed facts with their evidence, Confirm / Reject, and
  Reject or Expire for confirmed facts. A form declares a new fact.
* **Inline** on findings and approval cards: the facts that bind the object, and proposed
  facts about it with Confirm / Reject. A finding redirected by a fact shows its source-fix
  packet; a cleanup finding shows its batch.
* **Slack and Telegram**: a proposed fact is sent to the channels that receive approval
  requests, with Confirm / Reject buttons. The buttons use the approval-card rules: a
  single-use token bound to the channel, signed callbacks, a mapped operator or admin, and
  the fact unchanged since the card was sent.
* **API**: `GET /api/v1/facts`, `POST /api/v1/facts` (declare),
  `POST /api/v1/facts/{id}/confirm|reject|expire`, `GET /api/v1/facts/match?object=...`.
  Viewers read; operators and admins decide.
* **MCP**: `list_facts`, `propose_fact` (stays proposed), `decide_fact` (operator or admin).

## How a fact binds

The policy gate checks every action against the confirmed facts by typed matching (the
action's objects and SQL against the fact subjects, never model text). A bound action is
blocked with the reason `bound_by_fact` and a detail that names the fact, for example:

```
fact #12: index public.idx_thesis_allocation_run is owned by the application's
migrations (confirmed by alice@example.com on 2026-10-04); route: source_fix
```

Facts bind your own approvals too, except test-fixture and window facts: to let pg_sage act
on an object again, reject or expire the fact first (one click; it is recorded). Read-only
diagnostics and pg_sage undoing its own change are never bound. If the facts cannot be read,
the gate fails closed (`facts_unavailable`).

The optimizer, advisor and investigator prompts carry the confirmed facts as bounded context.
Proposed facts are never presented to the model as true.

### Slot facts are cluster facts

Replication slots and `max_slot_wal_keep_size` belong to the whole cluster, not to one
database, and PostgreSQL invalidates any slot that falls further behind than the bound.
So a confirmed `slot_consumer` fact binds:

* every `ALTER SYSTEM SET max_slot_wal_keep_size` with a finite value, `... TO DEFAULT`,
  `ALTER SYSTEM RESET max_slot_wal_keep_size` and `ALTER SYSTEM RESET ALL`, whichever slot
  (or `instance`) the proposal names. Only `-1` (unbounded) is exempt from this rule. This is
  deliberately strict: a bound that is safe today invalidates the slot the next time its
  consumer falls behind, and a managed provider's default may be finite. The WAL
  custodian's bound is then blocked (`bound_by_fact`, route `alert`) instead of applied;
* dropping or advancing that slot (by name, as before).

In fleet and meta-db mode, the databases one pg_sage process monitors are grouped by
cluster (`pg_control_system().system_identifier`, or the host and port when that cannot be
read). A slot fact confirmed on one database binds the slot work and WAL bounds proposed
for every database on the same cluster, and the detail names the database that holds
the fact (`fact #7 of database "orders": ...`). If a sibling's facts cannot be read, a
slot-touching action fails closed (`facts_unavailable`). Other fact types stay with their
own database. Databases monitored by separate sidecar processes do not see each other's
facts: confirm the slot fact on each of them, or monitor the cluster's databases from one
process.

## Expiry and re-verification

Every 15 minutes pg_sage re-checks proposed and confirmed facts. A fact whose subject no
longer names anything (the schema was dropped, the slot removed) expires after an hour of
absence; a fact with an expiry date expires on that date. Expired and rejected facts are
purged after `retention.actions_days`; proposed and confirmed facts are kept.
