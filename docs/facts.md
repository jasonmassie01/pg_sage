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
| `test_fixture` | schema pattern | Schemas left by test runs, not workload | Removes them from findings and from the budgets, takes no action on them by itself, and offers one **cleanup batch** (a reviewed script you run; see [The cleanup batch](#the-cleanup-batch)). pg_sage never drops schemas itself. A family pattern such as `test_*` also covers schemas created after you confirm it; the batch re-checks each schema's activity and locks before dropping it. |
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
    migrations recreated it). The subject spells the index as the catalog does, so an
    EF Core-style `"IX_Orders_CustomerId"` is proposed as `public."IX_Orders_CustomerId"`;
    a drop that named no schema, and an index whose name holds `*`, are not proposed;
  * test-named schemas (`test_*`, `tmp_*`, `*_test`, ...) with no scans or writes for six
    hours, as one pattern per family of copies;
  * logical replication slots and their consumer;
  * tables with a million inserts and no update or delete.
* **The model** (when an LLM is configured) proposes facts from a bounded summary of the
  catalog, at most once a day: schema summaries, logical slots, and indexes pg_sage dropped
  that came back. Every model proposal must cite the evidence it was shown and passes the
  same validation as any other; it is only ever a proposal. The cited evidence must also be
  about the proposal's own subject: a schema fact cites that schema's summary, a slot fact
  its slot, an index fact its drop history, and a subject with `*` cites at least two
  distinct objects it matches. The model is shown no table evidence, so it proposes no
  table facts (`append_only`, `table_window`, or ownership of a table); the detectors and
  you still can. A proposal that fails this is dropped and logged
  (`facts: model proposal dropped: ... the cited evidence is not about the subject`).
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

Targets that name no relation never bind. The target of a configuration, setting, database,
instance or query finding (`instance`, a database name, a query id) is not matched against
facts (only its SQL and any `slot:` target are), so a confirmed `test_*` fixture fact does
not hold back an `ALTER SYSTEM` recommendation. The target still counts for change leases,
budgets and the one-object rule of earned autonomy. An unqualified target relation (`orders`) is looked up in
the catalog of the database the action runs on, whatever the `search_path`: it binds through
every user schema that has a relation of that name (`test_a.orders` is a test fixture,
`public.orders` is not), and a name no user schema has binds nothing. Relations the action's
SQL names stay conservative: an unqualified one that cannot be resolved still matches every
schema subject. If the catalog cannot be read, the gate fails closed (`facts_unavailable`).

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
cluster (`pg_control_system().system_identifier`). A database whose identifier cannot be read
is grouped with every database at the same host and port, so a failed read never splits one
cluster in two. A slot fact confirmed on one database binds the slot work and WAL bounds proposed
for every database on the same cluster, and the detail names the database that holds
the fact (`fact #7 of database "orders": ...`). If a sibling's facts cannot be read, a
slot-touching action fails closed (`facts_unavailable`). Other fact types stay with their
own database. Databases monitored by separate sidecar processes do not see each other's
facts: confirm the slot fact on each of them, or monitor the cluster's databases from one
process.

## The cleanup batch

Each confirmed `test_fixture` fact with matching schemas gets one `test_fixture_cleanup`
finding whose `cleanup_sql` drops at most 500 schemas (a comment says how many more follow
in the next batch). Run it with `psql` in autocommit mode, as the owner of the schemas (on
managed providers too), against the database the finding names:

```
psql "$DATABASE_URL" -f cleanup.sql
```

Never wrap it in `BEGIN` or run it with `psql -1`, and do not paste it into a tool that
sends the whole script as one query (that runs as one implicit transaction): each schema is
dropped by its own `DO` block in its own transaction, so a few hundred schemas never exhaust
the shared lock table (`max_locks_per_transaction`). The script sets `lock_timeout = '5s'`,
and each block first re-reads the schema's activity (scans and row writes over its tables,
the same counters the fixture detector reads) and checks `pg_locks`. A schema is skipped
with a `NOTICE` when its activity differs from the baseline taken when the batch was built
(it was used, or the statistics were reset), when another session holds a lock on one of
its relations, or when the drop waits past the lock timeout. A failing block does not stop
the next one (psql's default `ON_ERROR_STOP` is off).

The baselines are re-read every analyzer cycle, so the guard covers activity since the
batch's `generated_at`, not before it: a schema that is used now and then but was idle in
that window is dropped. Review the `schemas` list before you run the batch, run it soon
after it was generated, and narrow the fact's pattern if it names a schema you keep.
Statistics reach `pg_stat_all_tables` a few seconds after a transaction ends, so a write
that commits just before the run can be missed; an open transaction is caught by the lock
check. Skipped schemas come back in the next batch with fresh baselines.

Building a batch costs one bounded, read-only catalog aggregate per analyzer cycle while a
fixture fact has matching schemas; nothing is stored. If that read fails, the batch is
withheld for the cycle (logged) and the earlier batch stays open.

## Expiry and re-verification

Every 15 minutes pg_sage re-checks proposed and confirmed facts. A fact whose subject no
longer names anything (the schema was dropped, the slot removed) expires after an hour of
absence; a fact with an expiry date expires on that date. Expired and rejected facts are
purged after `retention.actions_days`; proposed and confirmed facts are kept.
