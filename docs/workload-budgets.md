# Workload budgets

One noisy workload can starve the rest: a report that runs for an hour,
an ORM that leaves transactions open, a batch job that opens 300
connections. Workload budgets, in the style of PlanetScale Traffic
Control, give each workload a declared limit. pg_sage measures every
budget in **warn mode** first and changes nothing; only a role budget an
operator puts in **enforce** mode becomes role settings, and only after a
week of evidence that it would not have cancelled anything protected.

## Declaring budgets

```yaml
budgets:
  - name: web
    match: {role: web_app}
    statement_timeout: 6s
    idle_in_transaction_session_timeout: 30s
    connection_limit: 80
    pool_size: 40
    mode: warn            # warn (default) or enforce
  - name: checkout
    match: {tag: route=/checkout}   # sqlcommenter tag key=value
    statement_timeout: 2s
  - name: reports
    match: {application_name: metabase}
    statement_timeout: 5min

advisor:
  budget_proposals: true  # default
```

| Key | Meaning |
|---|---|
| `name` | Unique; the key of the budget's ledger rows and findings. |
| `match` | Exactly one of `role`, `application_name` (exact) or `tag` (`key=value`, read from the sqlcommenter comment of long-running active queries). |
| `statement_timeout` | Longest statement the budget allows: `250ms`, `6s`, `2min`, `1h`; a bare number is milliseconds. Empty or `0`: none. |
| `idle_in_transaction_session_timeout` | Longest idle in transaction wait, same format. |
| `connection_limit` | Most client sessions of the role, cluster-wide. Role budgets only. |
| `pool_size` | Suggested PgBouncer pool size. Advisory text only: the pooler's configuration is outside the database. |
| `mode` | `warn` (default) or `enforce` (role budgets only). |

Configuration validation refuses a budget without any limit, a negative
or malformed duration, a match of more or fewer than one field, a
`connection_limit` or `enforce` mode on an `application_name` or `tag`
budget, and the roles `postgres` and the provider administration logins
(`rdsadmin`, `rds_*`, `cloudsqladmin`, `azure_superuser`, `alloydbadmin`
and their replicas). At run time pg_sage also refuses `pg_` roles, its own
role and any superuser. `budgets` takes effect at restart.

## What pg_sage observes

Each collector cycle reads the `workload_sessions` snapshot category
(see [architecture](architecture.md)), whether or not any budget is set:
this database's client sessions grouped by role, `application_name` and
state (at most 200 groups), its 50 longest-running sessions past one
second with the sqlcommenter tags of the active ones, per-role statement
time from `pg_stat_statements` (`userid`) and each role's client sessions
cluster-wide. pg_sage's own sessions are left out. Every read is bounded
and read-only.

## Warn mode

Each analyzer cycle counts, per budget, into `sage.budget_shadow` (per
database, budget and UTC day):

- `would_cancel`: statements (or idle in transaction waits) strictly past
  the budget's timeout. A statement of exactly 6 s under a 6 s budget
  runs; one of 6.001 s would be cancelled. Each statement is counted once
  a day however many cycles see it.
- `would_reject`: the day's largest number of the role's sessions past its
  connection limit.
- `protected_cancels`: would-cancels of an application matching
  `runaway.safe_patterns`.
- `worst_ms`: the longest matching statement.

A budget with a would-cancel or would-reject today raises an `info`
[`workload_budget_violation`](findings.md#workload_budget_violation)
finding. `application_name` and `tag` budgets stay warn-only by design:
PostgreSQL applies settings per role. Their enforcement path is the
runaway policy (`runaway.policies`), which cancels by duration.

## Proposals

With `advisor.budget_proposals` on, a role that ran at least 100
statements and has no role budget gets an `info`
[`workload_budget_proposal`](findings.md#workload_budget_proposal):
`statement_timeout` = 3x its 14-day p99, rounded up to a whole second and
never below its longest statement. A role with p99 2 s gets 6 s. The p99
is estimated per statement from `pg_stat_statements` (mean + 2.326
standard deviations, capped at the statement's maximum) and the largest
is kept, the conservative choice; the daily estimates live in
`sage.workload_role_daily`. The finding carries a `budgets:` snippet in
warn mode.

## Enforce mode

A role budget with `mode: enforce` acts only when its ledger reaches back
7 days (6 days 23 hours is not enough) and records no would-cancel of a
protected application. Then, for each setting the role does not already
have, pg_sage raises a
[`workload_budget_enforce`](findings.md#workload_budget_enforce) action:

| Action | Statement | Risk |
|---|---|---|
| `set_role_timeout` | `ALTER ROLE r IN DATABASE d SET statement_timeout` (or `idle_in_transaction_session_timeout`) | moderate: the policy gate and trust level decide, as for other configuration changes |
| `set_role_connection_limit` | `ALTER ROLE r CONNECTION LIMIT n` | HIGH: always needs an operator's approval, at every trust level |

Before anything changes the executor refuses superusers, pg_sage's own
role, `pg_` roles and provider administration roles, an `IN DATABASE`
other than the connected database, and a connection limit below the
role's current sessions plus a margin (2 sessions or 10%, the larger).
It captures the prior value: the rollback sets back a prior
`pg_db_role_setting` value byte for byte, and RESETs only when there was
none (a connection limit rollback restores the prior `rolconnlimit`).
Execution takes a cluster-wide role lock (an advisory lock in the
server's `postgres` database), so two databases or sidecars changing one
role take turns; it re-reads the prior value in its transaction and
refuses when it changed. The read-back checks the catalog. A new setting
applies to sessions opened afterwards.

**Fleet mode.** Timeouts are per database (`IN DATABASE`); a connection
limit is cluster-wide and only the fleet leader applies it.

**Managed providers.** RDS and Cloud SQL allow `ALTER ROLE` on roles the
admin role created. When the server refuses for lack of privilege
(SQLSTATE 42501), the action fails as refused with the server's message
and nothing changes. PostgreSQL 14 to 18 behave the same.

**Not yet automatic.** Reverting an enforced timeout when its observed
cancels exceed the shadow prediction by 2x needs a source of actual
statement-timeout cancels (the server log). Until then an operator
reverts through the action's rollback.
