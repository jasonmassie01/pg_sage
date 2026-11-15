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
| `slot_consumer` | replication slot | The slot feeds a named consumer (Debezium, Fivetran, Airbyte...) | Never drops or advances the slot and never bounds WAL in a way that could invalidate it; it alerts instead. `max_slot_wal_keep_size` is cluster-wide, so any finite bound (and `RESET`) is blocked whichever slot it names; only `-1` (unbounded) is allowed. A confirmed slot is also registered in `sage.slot_consumer_registry`, so the WAL custodian escalates instead of acting; rejecting or expiring the fact yourself unregisters the slot again (owner and last confirmation are kept); an automatic expiry does not (see [Expiry and re-verification](#expiry-and-re-verification)). |
| `append_only` | table | Rows are only inserted (an archive or a log) | Never deletes its rows, rewrites it (`VACUUM FULL`, bloat plans) or drops its indexes (they look unused because an archive is rarely read). |
| `table_window` | table | `maintenance` window (act only inside) or `batch` window (never act during) | Holds pg_sage's own work on the table until the window allows it. Your own approvals are not held. |
| `owner_brief` | the database (`*`) | What you know and telemetry does not: who owns the database, its peak windows, expected growth, retention rules | Nothing on its own: it never binds, narrows or widens what the gate allows. Prompts carry it first, as context. See [The owner brief](#the-owner-brief). |

## Subjects

* Index and table subjects are schema-qualified: `public.idx_orders_status`, `app.orders`.
* An `owner_brief` has subject kind `database` and subject `*`: the database whose facts
  hold it. Any other subject is refused.
* Identifiers fold like PostgreSQL's: unquoted names are lower-cased, quoted names are exact
  (`"Billing"."Invoices"` is not `billing.invoices`).
* `*` matches any run of characters (`test_memory_*`, `public.idx_thesis_*`); nothing else
  is a wildcard.
* A subject that could match pg_sage's own `sage` schema or a system schema (`*`, `s*`,
  `pg_*`, `*.orders`) is refused.
* To see what a subject matches today, use **Check what it matches** on a proposed fact or
  in the declare form, or `GET /api/v1/facts/coverage?database=...&kind=...&subject=...`
  (any signed-in role). It lists at most 50 matching objects in name order and says
  `truncated` when there are more; the form shows the first 10 and "50+" for a cut list.
  It uses the same patterns re-verification checks, as a read-only catalog query under a
  2 s statement timeout. An invalid subject gives `400 invalid_fact`, one that could match a
  protected schema `400 protected_subject`, and no match an empty list.
* Values: `slot_consumer` names its `consumer`, `table_window` its `kind` and `window`;
  `owner_brief` takes the keys listed under [The owner brief](#the-owner-brief); the other
  types take an optional `note`. `owned_by_app_migrations` no longer takes `repo`
  or `path` (nothing read them): a new proposal with either is refused, while facts stored
  with them keep loading and binding. MCP `mark_object` still accepts `repo` and `path` and
  records them in the mark's evidence, where the person deciding sees them.

## The owner brief

The owner brief is the note a senior DBA keeps in their head: who owns the database, when
it is busy, how fast it grows and what must be kept. There is one per database; declaring
it again replaces it in place (the same fact, `proposals` counts the declarations, and
`decided_by` and `decided_at` move to the latest declaration).

| Key | What it says |
|---|---|
| `owner` | The team or person who owns the database |
| `contact` | How to reach them (a channel, a rota, an address) |
| `peak_windows` | When it is busiest: one or more windows in the [maintenance window grammar](configuration.md#maintenance-windows), separated by `;` (`weekdays 09:00-17:00 America/Chicago; sat 10:00-14:00`) |
| `growth` | Expected growth in words (`about 20 GB a month`) |
| `growth_pct_month` | Expected growth in percent per month, a number from 0 to 1000 |
| `retention` | Retention rules (`orders kept 7 years`) |
| `note` | Anything else |

At least one key is required; each value is at most 300 characters, and blank keys are
dropped. Declare it with `POST /api/v1/facts?database=...` and
`{"type": "owner_brief", "subject_kind": "database", "subject": "*", "value": {...}}`, or
propose it over MCP (`propose_fact`); a form in the dashboard follows.

The brief never binds: the gate allows exactly what it allowed without it. Re-verification
never expires it for absence (its subject is the database itself, so there is no catalog
lookup); it expires only on its expiry date or when you expire it. Prompts carry it as one
line of at most 300 characters, first; a longer brief is cut and the line ends with
`(rest of the brief omitted)`.

To see whether it helps, `/metrics` exports `pg_sage_owner_brief_present{database}` and
`pg_sage_owner_brief_steps{database,period,outcome}`: approved or executed (`accepted`)
and `rejected` queued actions since the brief was confirmed (`after`) and in as long before
it (`before`). Without a brief the two periods split 30 days ago.

## Where facts come from

* **Detectors** propose facts from deterministic evidence:
  * an index pg_sage dropped that came back with the same definition (the application's
    migrations recreated it). The subject spells the index as the catalog does, so an
    EF Core-style `"IX_Orders_CustomerId"` is proposed as `public."IX_Orders_CustomerId"`;
    a drop that named no schema, and an index whose name holds `*`, are not proposed;
  * test-named schemas (`test_*`, `tmp_*`, `*_test`, ...) with no scans or writes for six
    hours: a family of copies with generated suffixes (`test_memory_a1b2`,
    `test_memory_c3d4`) as one pattern (`test_memory_*`), any other one by name. A family
    is proposed only once every test-named schema its pattern would match is idle, members
    or not, so a live `test_memory_live` holds `test_memory_*` back (confirming it would
    hide live traffic). The first look on a new database proposes the same subjects by the
    same rule from the statistics window, when that window covers at least six hours (it reads
    at most 500 test-named schemas in name order; when it hits that cap, the last one read
    counts as live, so its family waits for the detector, which reads them all);
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
* **Ask Sage** (`propose_fact`) can propose one fact per question, citing evidence it read
  in that run. It has no confirm path; it can list facts (`list_facts`) but not decide them.
* **MCP clients** can propose with `propose_fact`; the fact stays proposed.
* **You** can declare a fact directly; your declaration confirms it.
* What you already declared with the MCP tools `register_consumer` and
  `declare_table_contract` (append-only) is imported as confirmed facts, once: a fact about
  the same subject, whatever its status, is kept, so a rejected fact is never re-confirmed
  by an import. `register_consumer` can still register a slot after you reject its fact
  (the WAL custodian then escalates again); the fact stays rejected.

Nothing proposed binds anything until a person confirms it, whichever of these proposed it.
Every new proposal, from any of these paths (the detectors and the model, the first look,
Ask Sage, MCP `propose_fact` and `mark_object`), sends one fact card to the channels that
receive approval requests (see [Confirming and rejecting](#confirming-and-rejecting)).
Proposing the same fact again sends no second card; an expired fact that a proposal reopens
sends a new one. A card that cannot be sent is logged (`db "...": fact card N not sent`) and
the proposal stands; it can still be decided on the Facts page. In a fleet each database's
runtime sends the cards of its own database.
A rejected fact stays rejected when it is proposed again (the proposal count grows); new
evidence reopens only an expired fact. Rejected facts are kept as tombstones: retention never
purges them, so the rejection is remembered for as long as the database is monitored.

## Confirming and rejecting

* **Facts page** in the UI: proposed facts with their evidence, what their subject matches
  today, Confirm / Reject, and Reject or Expire for confirmed facts. A form declares a new
  fact and previews what its subject matches before you declare it.
* **Inline** on findings and approval cards: the facts that bind the object, and proposed
  facts about it with Confirm / Reject. A finding redirected by a fact shows its source-fix
  packet; a cleanup finding shows its batch.
* **Slack and Telegram**: a proposed fact is sent to the channels that receive approval
  requests, with Confirm / Reject buttons. The buttons use the approval-card rules: a
  single-use token bound to the channel, signed callbacks, a mapped operator or admin, and
  the fact unchanged since the card was sent.
* **API**: `GET /api/v1/facts`, `POST /api/v1/facts` (declare),
  `POST /api/v1/facts/{id}/confirm|reject|expire`, `GET /api/v1/facts/match?object=...`,
  `GET /api/v1/facts/coverage?kind=...&subject=...`.
  Viewers read; operators and admins decide.
* **MCP**: `list_facts`, `propose_fact` (stays proposed), `decide_fact` (operator or admin).

Revoking (Reject) or expiring a confirmed fact lifts a guard, so the Facts page asks first:
it names the fact and says that pg_sage and operator approvals may then act on its subject
again. Cancel sends nothing. Confirming or rejecting a proposal is one click.

## History

Every change of a fact's status or decision appends one row to `sage.fact_events`: when, who
(`actor`), what (`verb`: `proposed`, `declared`, `confirmed`, `rejected`, `expired`,
`reopened`, `restored` or `baseline`), the status before and after, the fact's hash after the change (the
one a chat card binds to) and the note or expiry reason. A trigger on `sage.facts` writes it
in the same transaction as the change, so every path is recorded (the UI and API, chat cards,
MCP `decide_fact`, re-verification, and agent column classes), and a change whose event cannot
be written fails. Re-proposing a fact (its proposal count and evidence) and re-verification
marks (`last_verified_at`, `missing_since`) are not decisions and add no event.

* The row in `sage.facts` keeps only the latest decision; the history keeps every one, so the
  person who first confirmed a fact can still be read after someone revokes it.
* Facts that existed before the upgrade get one `baseline` event when the history is first
  installed: their current status, by their latest decider (or proposer), at the decision
  time. Who expired an older fact was never recorded, so its actor is `unknown` and the note
  keeps the decider and the expiry reason.
* `expired_by` names who expired a fact: the signed-in user for an expiry through the UI or
  API, `pg_sage:verifier` for re-verification. A restored confirmation is recorded as
  `pg_sage`'s.
* The history is append-only (an update is refused) and is an audit chain (`fact_events`):
  `GET /api/v1/audit/verify` checks it and SIEM export ships it, like `config_audit`.
* It outlives its fact: retention purges expired facts after `retention.actions_days`, and
  keeps their events. Events have no age retention.

```sql
SELECT at, actor, verb, from_status, to_status, note
FROM sage.fact_events WHERE fact_id = 12 ORDER BY id;
```

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

Confirmed facts also reach the model, as bounded context fenced as data (the gate, not the
model, is what enforces them):

* The **advisor**'s review prompts and the **investigator**'s review turns carry the
  database's confirmed facts: at most 12 facts and 1,500 characters. When there are more,
  the owner brief goes first (it is about the whole database and reaches every prompt,
  whatever objects it is about), then the facts behind WAL and vacuum advice:
  `slot_consumer`, then `table_window`,
  `append_only`, `owned_by_app_migrations` and `test_fixture`, by fact id within a type.
  The last line then says how many were left out (`- 19 more confirmed facts omitted.`);
  the budget still holds with that line.
* The **tuning agent**'s case packet carries the confirmed facts about the case's tables
  (an index fact counts for every table in its schema), one line each of at most 300
  characters, citable as `F<id>`, within the packet's 12,000-byte budget.
* **Ask Sage**'s prompt does not carry them; it reads them with its `list_facts` tool.

Proposed, rejected and expired facts are never presented to the model as true. A fact past
its expiry date stops binding and leaves these prompts at that moment, before
re-verification marks it expired.

The gate reads the confirmed facts through a cache of at most 10 seconds per database. A
confirm, reject or expire in the same pg_sage process (UI, API, MCP or a chat card) clears it
at once, including a read already in flight, so the next action sees the decision. A change
written by another process is seen within 10 seconds. The advisor's and the investigator's
prompt context reads through the same cache; the tuning agent reads the facts afresh for each
run. If the facts cannot be read, a prompt goes without them (logged), while the gate still
fails closed.

### Partitioned tables

A table fact on a partitioned table covers all its partitions (declarative partitioning;
legacy inheritance children are not covered). A fact on `public.events` binds work on
`public.events_2026_11`, on a sub-partition of it, and on a partition's index, as it binds
work on `public.events` itself: `ALTER TABLE public.events_2026_11 SET (...)`, a
`VACUUM (FULL)` of a partition, a `VACUUM` of a partition inside the table's batch window, or
`DROP INDEX` of a partition's index. A schema fact also covers a partition that lives in
another schema than its partitioned table. The binding names the partition as its object and
the partitioned table's fact as its subject.

The partition tree is read from the catalog of the database the action runs on
(`pg_partition_root`, PostgreSQL 12 and later), in one bounded, read-only query per gate
check, so a partition attached after the fact was confirmed is covered at once. If the
catalog cannot be read, the gate fails closed (`facts_unavailable`). The analyzer reads it
once per cycle to turn findings on partitions into source-fix packets; if that read fails,
the findings keep their SQL for that cycle (logged) and the gate still binds them. A
partition the action names without a schema is matched by its own name only.

A fact on a partitioned table does not expire while the table exists, even with no
partitions. A fact on a partition covers that partition alone, not its siblings or its
partitioned table.

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

Every 15 minutes pg_sage re-checks proposed and confirmed facts. A fact that a detector or the
model proposes again in the same pass counts as present. When a fact's subject no longer names
anything (the schema was dropped, the slot removed), pg_sage records when it was first seen
absent (`missing_since`, kept in `sage.facts`, so a restart does not reset it) and clears it
when the subject is back. The fact expires once the absence has lasted its grace:

| Fact | Absence grace |
|---|---|
| Confirmed, proposed by an operator or the model | `facts.confirmed_absence_days` (default 7, 1-90) |
| Proposed (not yet decided), or proposed by a detector | 1 hour |

A confirmed fact keeps binding while its subject is absent (it matches nothing until the
subject is back), so a table rebuilt and swapped in, a blue/green schema swap or a Debezium
slot recreated after a connector reset keeps its confirmation. The Facts page and the API show
`missing_since` ("absent since"). When a confirmed fact expires this way, pg_sage logs a
warning and sends a `fact_expired` notification (warning severity) naming the fact and who
confirmed it. If the same subject then comes back and a detector or the model proposes the
same value within `facts.confirmed_absence_days` of the expiry, the confirmation is restored:
the fact is confirmed again with its original `decided_by` and `decided_at`, the decision note
gains "subject reappeared; confirmation by ... kept", and no card is sent. A different value,
a proposal after that window, a fact you expired yourself and a fact whose expiry date passed
reopen as a proposal for you to decide; a rejected fact never reopens. A confirmed fact whose
subject no longer parses (after an upgrade changed the subject rules) is reported and kept
confirmed; one fact that cannot be checked never stops the others being checked. Declaring,
confirming or rejecting a fact starts any absence of its subject afresh, so a fact you declare
again while its subject is still absent gets its full grace. A fact with an expiry date
expires on that date, and stops binding from that moment. Expired facts are purged after
`retention.actions_days`; proposed, confirmed and rejected facts are kept (a rejected fact is
the record that you said no). Expiring a slot consumer fact yourself unregisters its slot, and
so does the expiry date of a fact you confirmed. A slot that is gone keeps its registration
when its fact expires, so a consumer that recreates it under the same name is still escalated
until you decide; the same holds for a proposed fact's expiry date.

Slots whose fact was rejected before this release are still registered: pg_sage does not
unregister them on its own, because that would widen what the WAL custodian may do without a
fresh decision. Find them, then confirm and reject a rejected fact again (the reject
unregisters the slot), or unregister the slot yourself with
`UPDATE sage.slot_consumer_registry SET registered = false WHERE slot_name = '...'`:

```sql
SELECT f.id, r.slot_name, r.owner_tag
FROM sage.facts f
JOIN sage.slot_consumer_registry r ON r.slot_name = f.subject
WHERE f.fact_type = 'slot_consumer' AND f.status IN ('rejected', 'expired') AND r.registered;
```
