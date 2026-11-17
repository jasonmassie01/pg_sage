# Sage Review: the rubric and the grade

Sage Review grades each monitored database against a named, versioned "well-run Postgres"
rubric. The grade is deterministic: code adds up checks pg_sage already runs, with no model.
It is read-only on the monitored database and writes only `sage.review_run` and
`sage.review_grade` in the database's own sage schema.

- **When.** Once right after the database's first look (within minutes of registration),
  then every `review.grade_interval_hours` (default 24). Metric:
  `pg_sage_review_first_grade_seconds{database}`, the time from registration to the first
  grade (target under 10 minutes).
- **Where.** `GET /api/v1/databases/{db}/review/grade` (viewer) and the MCP tool
  `review_grade` return the latest grade, the previous one and the delta between them.
- **Switch.** `review.enabled` (default `true`).

## The rule that matters

A check that could not run is `unknown`. It stays in the denominator and adds nothing to the
numerator, so **a check that could not run can only lower a grade, never raise it**. A hidden
fact (a managed platform that does not expose backups, a producer that has not run yet, a
missing grant, a timeout, a degraded first-look check) is `unknown`, never `pass`.

## Check results

| Result | Counts | When |
|---|---|---|
| `pass` | full weight | The producer ran and found nothing open. |
| `warn` | half weight | Only info-severity findings, a pending restart, a drill pass older than 35 days, a major within 90 days of end of life. |
| `fail` | nothing | A warning or critical finding, a failed drill, a major past end of life. |
| `unknown` | nothing, stays in the denominator | The check could not run, with a reason. |
| `not_applicable` | leaves the denominator | The platform does not have the fact (for example the WAL archiver on RDS), or upgrade readiness on a major that is not near end of life. |

Every result carries the evidence it rests on: a finding (`finding:<id>`), a first-look
check (`first_look:<report id>#<rule>`), a drill (`drill:<id>`), a lint rule, or a setting.

## Grade arithmetic

- **Dimension score** = passed weight / applicable weight, in percent (warn counts half).
- **Coverage** = applicable weight that ran (pass, warn or fail) / applicable weight.
- **Letter**: A at 90 or more, B at 80, C at 70, D at 60, F below. Below 60% coverage the
  letter is `unknown` (null) and the score is still stored.
- **Critical checks**: a critical check that failed, or could not show that it passed, caps
  its dimension at D.
- **Overall score**: the mean of the scores of every dimension that has applicable weight,
  known or not. An unknown dimension's score already counts its unknown checks as nothing.
- **Overall letter**: `unknown` when fewer than four dimensions are known; otherwise the
  letter of the overall score, never more than one letter above the worst dimension's
  letter of score.

The overall counts unknown dimensions on purpose. If it averaged only known dimensions, a
weak dimension that dropped below 60% coverage would leave the mean and raise the grade.
A property test checks 10,000 random cases where the only change is checks turning unknown:
no score and no letter ever improves.

## Rubric `pg_sage-rubric/1`

Weights are in parentheses; **critical** checks cap their dimension at D. "Self-managed" is
`postgres` and `cnpg`; "cloud" is RDS, Aurora, Cloud SQL, AlloyDB and Azure.

| Dimension | Checks |
|---|---|
| Recoverability | WAL archive failing (3, critical, self-managed), WAL archive stale (2, self-managed), inactive replication slot (1), slow replication slot (1), backup stale (2, cloud), backup retention below policy (1, cloud), PITR window below policy (1, cloud), last passed restore drill (5) |
| Change safety | migration safety findings (2), login roles without session timeouts AP-08 (3), serial columns (1, lint) |
| Workload and plan stability | query regression (3), plan regression (3), stale statistics (2), temp spill (1) |
| Schema | integer primary key (2), FK type mismatch (2), no primary key (2), timestamp without time zone (1), all from schema lint; duplicate index (2), redundant index (1), unindexed foreign key (2), from the first look |
| Capacity and cost | disk growth forecast (2), XID runway (3, critical), multixact runway (2), sequence runway (2), right-sizing (1, cloud, needs `cost.rates`) |
| Security and access | agent posture AP-01 role attributes (2), AP-03 tables without RLS reachable by exposed roles (3, critical), AP-05 definer functions (1), AP-07 PUBLIC CREATE (2), AP-13 shared logins (1), AP-15 PUBLIC grants (2), AP-16 unmanaged logins (1) |
| Configuration | memory, WAL and connection advisor findings (1 each), checkpoint pressure (1), settings waiting for a restart (1) |
| Lifecycle | server major end of life (3, critical), extension drift (2), upgrade readiness (1, only for a major at or near end of life) |

Where each result comes from:

- **First look and agent posture**: the newest stored first look. `ok` passes, `finding`
  fails (warns when every item is info), `degraded` or a check that did not run is unknown.
  The first look runs at startup and on a re-check only, so a report that finished more
  than 7 days ago grades unknown: re-check it (`POST /api/v1/first-look/recheck`).
- **Findings**: open rows of `sage.findings`. With none open, a check passes only when its
  producer has run for the database: the analyzer and the migration safety detector (a
  collected snapshot), the forecaster (a snapshot over a day old), the advisor (a
  successful run). Cloud backup checks have no run
  record, so they are unknown unless a finding is open.
- **Schema lint**: a live, read-only scan of the lint rules (when `schema_lint.enabled`); a
  rule in `schema_lint.disabled_rules` or whose catalog read failed is unknown.
- **Restore drill**: a pass in the last 35 days passes; an older pass warns, or fails when
  the newest drill failed; drills that never passed fail; no drill yet is unknown.
- **Settings**: one read-only probe of `pg_settings` under a 5 s `statement_timeout`.

What a SQL-only connection can grade: Schema, Workload, Configuration, Security and Lifecycle
in full. Recoverability is `unknown` until a restore drill has run, and on a managed platform
without cloud telemetry. Capacity and cost needs the forecaster's history, and right-sizing
needs `cost.rates`.

## The delta

Each grade is compared with the previous graded run. The delta lists the dimensions whose
letter, coverage band (`low` below 60%, `partial` below 90%, `high`) or check results
changed, with the checks that moved. An unchanged run has an empty delta: silence means
healthy. The first run's delta is the whole report (`first_run`). A rubric change bumps the
version; the delta then says `rubric_changed` and lists the new report instead of comparing
letters across versions.

## Runs

`sage.review_run` holds one row per run: `kind` (`grade`, or `review` for a Sage Review),
`triggers` (`first_look`, `daily`), `rubric_version`, `status`, `overall_grade`,
`overall_score`, `coverage_pct`, `started_at`, `finished_at` and `error`. A run is `running`
until it ends `done`, `partial` (a producer could not be read: its checks are unknown and the
error names it) or `failed`; a finished run never moves again. One run per database can be
running (a partial unique index); a second gets "already running". A run a killed sidecar
left running for 30 minutes is failed with "sidecar stopped during run". `sage.review_grade`
holds each dimension's letter, score, coverage and check results. Runs are kept 400 days.

## Candidates

A review's generators turn the workload pack into **candidates**: proposals only. A candidate
becomes a change when a plan (review-03b) includes it, and then it runs through the executor's
contract for its action type like every other change (policy gate, trust, approval,
verification, rollback, audit). The generators add no action type and no write path.

Each candidate (`internal/review/candidate`) has:

- an `id`: the SHA-256 of its generator, kind, target and forward SQL, so the same proposal
  keeps its id across reviews;
- an `action_type`: a registered executor contract, or `advice` (text for a person) or
  `operator` (a step an operator runs);
- the forward and rollback SQL, rendered by the existing builders (never free text);
- `evidence_ids`: the pack facts it rests on, each of which must resolve in the pack;
- a `prediction`: metric, direction, size, the lab ladder tier that measured it (`none` when
  nothing did), its source, and the computed quantities;
- `approval`: `automatic` or `approval_required`.

Validation refuses an unknown action type, a candidate without evidence, an evidence id the
pack does not hold, a registered action without SQL, and SQL that classifies as another action
type. A step that is never automatic (table rewrites, type changes, dropped data, restarts,
topology, privilege revokes, pooler mode) is forced to `approval_required`.

Candidates are stored in `sage.review_candidate`, one row per (run, candidate), and go with
their run. A candidate starts `proposed` and moves once: to `planned`, `rejected` or
`superseded`. A superseded candidate never becomes planned.

The window is `review.window_days` (default 90, 14-100). When the retained history is shorter,
the pack is shorter and the generator reports its coverage as `partial`.

## Index portfolio generator

The `indexes` generator judges a database's indexes as a set, over the whole pack window:

| Kind | Proposal | Validation |
|---|---|---|
| `duplicate` | Drop every exact copy but one (constraint-backing, then unique, then the most scanned, then by name is kept). | None needed: a drop is reversible by rebuild. |
| `consolidation` | Drop the indexes whose keys lead a wider btree index of the same table, which serves the same lookups. At most one set per table per review. | HypoPG hides the whole set and plans the table's top statements. The set is kept only when no statement that used it rises more than `optimizer.hypopg_min_improvement_pct`. |
| `unused` | Drop an index with no scan over the whole window. | The statistics must be unbroken across the window: a reset makes every index `unknown`, never unused. A window under 14 days calls no index unused. |

Constraint-backing, primary, unique, invalid and partition indexes are never proposed, nor is
an index another drop of the same review relies on: a set's covering index or a duplicate's
kept copy is never dropped as unused too. At most
`optimizer.max_indexes_per_table` candidates are proposed per table per review.

Without HypoPG (not installed, too old to hide an index, or refused), a consolidation set is
**advice** with tier `none`, and the reason says what HypoPG could not do. Plain duplicates and
unused indexes stay steps.

Each drop states the index bytes it reclaims and, when the table's writes are known, the
write blocks it saves a day: the table's inserted and updated rows a day, one leaf block per
row per index. That is an upper bound, since a HOT update writes no index. The review's numbers
are the sum over its steps.

## Configuration generator

The `config` generator judges the server's settings as a set. Every setting of the pack whose
source is not the built-in default is listed as a fact. Then a fixed rule table checks each
judged setting, default or not, against the workload and hardware facts that contradict it:

| Setting | Judged against | Rule |
|---|---|---|
| shared_buffers | the server's memory | Under 15% or over 40% of memory (from the provider's memory fact) proposes 25%. |
| work_mem | the temp files written per hour | 256MiB or more of temp files an hour doubles work_mem, capped at 25% of memory shared by `max_connections`. |
| max_wal_size | requested against timed checkpoints and the WAL rate | More requested than timed checkpoints sizes max_wal_size to one `checkpoint_timeout` of WAL times (1 + `checkpoint_completion_target`), rounded up to a whole GB. |
| random_page_cost | the storage class | 2 or more on a managed service (SSD storage) proposes 1.1. A self-managed server's storage is not known, so it is not judged. |
| effective_io_concurrency, io_method | PostgreSQL 18's asynchronous I/O | On 18, an `effective_io_concurrency` of 1 or less set outside the defaults becomes 18's default 16; `io_method = sync` is advice. |
| idle_in_transaction_session_timeout | sessions idle in a transaction | Off while sessions sit idle in a transaction is advice for 30min, set per database with approval. |

Values are computed in code and clamped to the `pgconf` safe range; a value outside it is never
emitted. How a contradiction becomes a candidate:

- A reloadable setting on a self-managed server is an `alter_system_guc` step that needs
  approval. Its rollback restores the value the pack saw; the executor captures the live prior
  again when the step runs.
- A restart-bound setting (`pgconf.RequiresRestart`) on a self-managed server is an
  **operator** step: the operator runs `ALTER SYSTEM` and restarts, and pg_sage watches the
  setting land. At most one restart-bound step is proposed per review; another goes to the next
  review.
- On a managed service every setting is an operator step carrying managedparam's proposal (the
  provider command and its revert). pg_sage never applies or reverts it.
- Where managedparam cannot describe the change (Neon, Supabase, Lakebase, an unknown platform),
  the candidate is advice, with the reason.

Every configuration prediction says it has no control group: a global setting is verified as an
interrupted time series against the seasonal baseline. The review's number is the count of
settings the workload contradicts. The LLM configuration advisor keeps running as before.

## Autovacuum generator

The `autovacuum` generator sizes each table's own autovacuum settings from its vacuum history in
the pack: its live rows, its dead tuples on each day of the window, and the server's autovacuum
settings (PostgreSQL's defaults when the pack has none).

1. The target is a dead-tuple peak of at most 5% of the live rows. A table whose peak is at or
   under the target gets nothing.
2. Growth is the median day-over-day rise of the dead tuples, per hour. A drop is a vacuum and is
   left out.
3. The trigger budget is the target less the growth over `autovacuum_naptime` plus one hour.
4. The threshold keeps its value unless it is over a tenth of the budget. The scale factor takes
   the rest of the budget (at least 0.001), and the analyze scale factor half of it.
5. The step is `ALTER TABLE ... SET (...)` (`set_table_autovacuum`). Its rollback is `RESET` of
   the keys the table had no value for, or `SET` of their prior values. One statement must roll
   back exactly, so a key whose prior state differs from the scale factor's is left out.

A table with fewer than two days of dead tuples, unknown live rows, autovacuum switched off by
its owner, or storage parameters that do not parse is left alone, with the reason in the
coverage. A table under 100,000 rows is left alone without a note. When no trigger can hold the
target, because the growth outruns it or the table is already tuned tighter, the candidate is
advice that autovacuum is falling behind.
