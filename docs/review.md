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
`triggers` (`first_look`, `daily`, or the trigger kinds of a review, see [Triggers](#triggers)), `rubric_version`, `status`, `overall_grade`,
`overall_score`, `coverage_pct`, `started_at`, `finished_at` and `error`. A run is `running`
until it ends `done`, `partial` (a producer could not be read: its checks are unknown and the
error names it) or `failed`; a finished run never moves again. One run per database can be
running (a partial unique index); a second gets "already running". A run a killed sidecar
left running for 30 minutes is failed with "sidecar stopped during run". `sage.review_grade`
holds each dimension's letter, score, coverage and check results. Runs are kept 400 days.

## The Review page

`#/review` (Operate, every role) shows one database's review: the database named in `?db=`,
else the one picked in the header. The grade chip on the first look ("Day-one grade") and in
the database page header ("Grade") links to `#/review?db=<name>`. Neither chip shows before
the first grade.

What the page shows, top to bottom:

- **A running banner** while a grade is in progress ("A grade is running since ..."). The
  page then polls every 5 seconds, and the poll after the run ends swaps the banner for the
  new grade. Before the first grade it says "First grade runs after the first look."
- **The grade card**: the overall letter in large type, the coverage and the rubric version,
  and "Partial" when a producer could not be read.
- **The dimensions**: eight stacked rows (they fit a 375 px phone with no sideways scroll),
  each with its letter, its coverage and a disclosure listing the checks behind it, each
  check with its status and reason. A reason longer than 100 characters is cut, with the
  full text on hover.
- **What changed since <date>**: the checks that moved since the previous grade, as
  `check: Fail → Pass`, or "No change since <date>". A first grade says there is nothing to
  compare with, and a rubric change is named.
- **The report**: the model pass status line, then the ranked findings. Until a model pass
  is recorded (review-03a), the report is templated: the failed checks, then the warned
  ones, critical first, then by weight, then in rubric order. A grade-only run reads
  "Model pass: skipped: grade-only run". When a model pass is done, the page adds its
  cross-dimension links, its hypotheses labelled as advice (pg_sage does not act on them),
  its text per dimension, and a "Review the plan" link to the plan in Actions.
- **The review scorecard** (review-05b): the model gate card and the scorecard by origin. It
  is read on its own, so a scorecard that cannot be read is named in its section with Retry
  and the rest of the page stays; see
  [On the Review and Value pages](#on-the-review-and-value-pages).

Letters are written out on a colour with at least 4.5:1 contrast, so colour is never the
only signal. A dimension or overall without a letter reads "Unknown: <reason>" (for example
"Unknown: coverage 42%, below 60%" or "Unknown: 3 of 8 dimensions known, 4 needed"), never a
grey letter or an empty cell. A letter outside A to F or a score outside 0 to 100 reads
"Unknown: invalid grade". Model text is shown as plain text, never as HTML or markdown.

## Read receipts

Opening a finished review (`done` or `partial`) sends
`POST /api/v1/databases/{db}/review/runs/{id}/viewed` once, unless the grade route's
`viewed_at` says this person already opened it. A failed receipt is retried once after a
second and never blocks the page. The server keeps the first `viewed_at` per run and user in
`sage.review_view` (`run_id`, `user_id`, `viewed_at`; primary key `(run_id, user_id)`), so two
tabs give one row. Receipts go with their run (`ON DELETE CASCADE`).

This is the evidence that a person read a review. The share of finished reviews opened by a
person within 7 days, per database:

```sql
SELECT r.database_name,
       count(*) FILTER (WHERE EXISTS (
           SELECT 1 FROM sage.review_view v
           WHERE v.run_id = r.id AND v.viewed_at < r.finished_at + interval '7 days'))
         ::float8 / count(*) AS opened_within_7_days
FROM sage.review_run r
WHERE r.status IN ('done', 'partial') AND r.finished_at < now() - interval '7 days'
GROUP BY r.database_name;
```
## Triggers

A review is worth most when someone has a decision to make, so events start one and the
calendar is the fallback (`internal/review/trigger`). Each trigger is a row of
`sage.review_trigger` in the database's own sage schema: `kind`, `detail` (a JSON object: the
"why now"), `fired_at`, `state`, the `run_id` that consumed it and `coalesced_into`.

| Kind | Fires when | Source |
|---|---|---|
| `enrollment` | The rollups hold two complete weekly cycles: the oldest coverage hour is 14 days old and at least 80% of the last 336 hours have a `sage.query_rollup_coverage` row. Once, and only before the first full review. The grade itself runs on day one. | polled hourly |
| `pre_upgrade` | An upgrade readiness check (`GET /api/v1/upgrade-readiness`) succeeds; once per target major and UTC day. Bypasses the interval: the review starts at the next tick. | the route |
| `unresolved_regression` | A `query_regression` or `plan_regression` finding open for 72 hours or more whose action was reverted, or that has no verified action (one that ran and that verification judged `improved` in `sage.action_outcome`). Once per finding. | polled hourly |
| `cost_growth_anomaly` | The newest whole day's database size growth (system snapshots, one value a day, 29 days) is a robust z of 4 or more over the earlier days (`baseline.Judge`, at least 7 days of history) and at least 256 MiB over their median. A missing day breaks the series. Attributed cost is not judged yet. | polled hourly |
| `large_migration` | A live migration lint (`POST /api/v1/migrations/lint`, not `static`) measured a table larger than `review.large_table_bytes`; once per table set and UTC day. | the route |
| `instance_change` | The instance class, vCPU count or memory that cloud telemetry reports differs from the last value seen (`sage.review_trigger_state`). A fact unknown on either side is not a change; the first observation is recorded, not fired. Without a cloud adapter (self-managed, Neon, Supabase, serverless) this kind is unavailable: pg_sage does not guess. | polled hourly |
| `calendar` | `review.calendar_days` (default 30) after the last full review finished. | polled hourly |
| `fleet_outlier` | The fleet triage picked the database for a deep review ([fleet tiering](#fleet-tiering)); once per calendar month (key `fleet:YYYY-MM`). | the fleet pass |

**Coalescing.** The oldest waiting trigger opens a window of `review.min_interval_hours`
(default 24). Every trigger fired inside it joins one review run, and the run starts when the
window closes: two triggers 23 h 59 min apart give one run, 24 h apart give two. A
`pre_upgrade` starts the run at once with everything waiting. A trigger fired while a review
runs waits for the next run. The run's `triggers` lists the distinct kinds; the first trigger
is `consumed` and the rest `coalesced` into it, each with the run's id.

**Leader, restart and claims.** The loop ticks every 5 minutes, on the leader only. It claims
the due triggers with `FOR UPDATE SKIP LOCKED` (two sidecars never claim one trigger), records
the run id on the claim as soon as the run is recorded, and holds the claim for 35 minutes (the
stale-run reaper's 30 minutes, plus margin). A claim whose sidecar died goes back to `fired`
when its lease ends, keeping its run id: the next leader consumes it into that run if the run
finished, waits while it runs, and starts a new run if it failed or has been running for
longer than the lease (its sidecar died mid-run; the new run's reaper fails it). So a trigger
gets one finished run. A grade that is already running makes the loop release the claim and
retry at the next tick.

**What a run does.** Today a trigger-started run is a `review` run graded against the rubric,
read-only, carrying its triggers. Nothing runs on the monitored database: a review reads and
proposes. The grade route's `why_now` lists the triggers the latest run consumed or coalesced,
with their details.

**Errors.** A failed trigger insert names the kind and the database and is returned to the
source. The routes log it and answer as before; it is counted in
`pg_sage_review_trigger_fire_errors_total{kind}`. `pg_sage_review_runs_total{cause}` counts
the runs started by an `event` or by the `calendar`. The median time from trigger to finished
run is `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY r.finished_at - t.fired_at) FROM
sage.review_trigger t JOIN sage.review_run r ON r.id = t.run_id`.

## Fleet tiering

A full agentic review of every member of a large fleet costs more than the databases do: an
estimated $3,000 to $18,000 a month for 1,000 small agent-created databases, against $100 to
$300 for a tiered pass. In fleet and meta mode, with `review.enabled`, the leader runs one
tiered pass of the fleet a calendar month (UTC) on the control database
(`internal/review/fleet`). Single-database installs never run it.

1. **Rollup**, deterministic, for every member, read-only from its own sage schema: the newest
   grade and the one before it, coverage, the newest run's candidates by generator and their
   best deterministic score, the idle rule (no workload calls for 14 days, pg_sage's own
   excluded), the database size and, only with operator rates in `cost.rates`, its monthly
   cost. Eight members are read at a time.
2. **Triage**, a low-cost model over the rollup (`review.triage_model`, default the general
   client's model), 200 members a call, each call's prompt under about 50K tokens, purpose
   `review_triage`. The model names outliers by rollup id (`R1`..) and cites the rollup ids its
   reason rests on. An item naming an unknown member, citing an unknown id, citing nothing or
   naming a member twice is dropped with its reason. A batch whose call fails (the error class
   is stored: `timeout`, `rate`, ...), whose reply is empty, or whose reply is not JSON
   (markdown fences are fine) is triaged deterministically instead: a grade drop of 5 points or
   more, an F, a candidate scoring 10 or more, or a cost of 3 times the fleet median, ranked in
   that order. With llm disabled the whole triage is deterministic.
3. **Fleet review**, once over the rollup, its answers computed in code: the abandoned members
   (the idle rule), the anti-patterns shared by two or more members with the same candidate
   kind (`generator/kind`) and the same schema fingerprint (fleet learning's table shapes),
   and what retiring the idle members would save a month (only priced members carry dollars;
   the rest are counted as unpriced). The general client writes a short summary over those
   facts (purpose `review_fleet`); without it the summary is a template. A fleet of one skips
   this tier and the model triage.
4. **Deep reviews** only for the top outliers, at most `review.fleet_deep_max` (default 30) a
   calendar month; members waiting from an earlier month go first, then by need. The month's
   tokens (200,000 a review on average) are split by need with `fleet.AllocateByNeed` (each
   between half and three times the even share). A deep review is a `fleet_outlier` trigger in
   the member's own queue, with its tokens in `detail`; the member's trigger loop runs it. The
   31st outlier waits for the next month. A member gets one fleet deep review a month.

Each member moves `rollup` -> `triaged` -> `deep_scheduled` -> `reviewed` in
`sage.review_fleet_member`; `reviewed` comes when a run consumed its trigger. A fleet-wide
emergency stop halts scheduling: the outliers stay `triaged` and are scheduled at the next
hourly tick after the release. The pass row (`sage.review_fleet_pass`) keeps the triage mode,
its reasons and error class, the fleet review and the per-tier tokens and cost (`tier_tokens`,
priced from `llm.pricing`); `sage.llm_usage` attributes the tiers' spend by purpose
(`review_triage`, `review_fleet`, `review`). Nothing here changes a monitored database.

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

## Schema debt generator

The `schemadebt` generator joins the shapes the schema lint finds to a rate. The lint says
"int4 key"; the runway monitor says "sequence at 80%"; this generator says "exhausts in 247
days" and attaches the migration plan. It reads the pack's schema, workload and index sections,
the catalog facts of the debt columns (bounded, read-only), and the sequences' samples in
`sage.runway_samples` since the pack window's start (the newest epoch of each sequence only).

| Kind | When | Output |
|---|---|---|
| `int4_key` | A single-column `smallint` or `integer` primary key fed by a sequence (serial or identity). | Under 365 days of measured runway: `prepare_sequence_capacity_migration` with the sequence playbook's forward-fix script. Otherwise `advice`. |
| `fk_type_mismatch` | An integer foreign key column narrower than the key it references. | `advice`: the referenced key's sequence rate against the column's own limit. |
| `timestamp_without_time_zone` | A `timestamp without time zone` column of a table the window's top query families read. | `advice` carrying the online swap to `timestamptz`, or `advice` saying why it cannot be planned. |
| `large_unpartitioned_table` | An unpartitioned table of at least `review.large_table_bytes` (default 10 GiB) whose primary key or an index leads with a date or timestamp column. | `advice`: range partitioning on that column. |
| `queue_table`, `soft_delete_table` | At least 100000 rows updated or deleted over the window, at least half of all its writes, and a `status`/`state` or `deleted_at`/`is_deleted`/`deleted` column. | `advice`. |
| `column_swap` | Beside an int4 key or a foreign key mismatch, when the migration planner drafts the online widening. | `advice` carrying the planner's expand, backfill and contract script for a person to run. An `operator` step needs a setting landing pg_sage can watch, and a column type is not one. |

**The runway arithmetic.** The rate is a least-squares fit of the sequence's samples against
time, in values a day. The remaining values run from the newest sample to the binding limit:
the smaller of the sequence's own limit (as the runway store records it) and the column's type
(2147483647 for `integer`; the minimum, -2147483648, for a descending sequence, which the store
samples negated). The runway is the whole days of remaining values at that rate, and the
statement gives the range at the rate plus and minus 2 standard errors. With fewer than 3
samples, or samples all at one instant, the statement is `rate not measured yet` and no date
is given. A rate at or below zero is "not consuming values", also without a date.

**Never automatic.** Every schema-debt candidate needs a person's approval at every trust
level: the capacity migration's contract is forward-fix only and direct execution is disabled;
a type change, a table rewrite and a partition conversion are never automatic. When the planner
refuses a column (an identity column, a default that is not the column's own sequence, a
partitioned table, foreign keys it cannot carry over), the refusal is attached to the debt
item's reason and the runway is kept.

The review's numbers are the debt items with a measured runway, and the share of int4 keys
whose runway is measured rather than `rate not measured yet`.

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
target, because the growth outruns it, the table's own threshold (left alone, see step 5) or the
0.001 floor keeps the predicted peak over it, or the table is already tuned tighter, the candidate
is advice that autovacuum is falling behind.

## Connection generator

The `connections` generator judges the connection architecture from the window's
`workload_sessions` snapshots: the newest 288 for recycle herds, and the whole window for each
role's cluster-wide peak, each role's longest idle-in-transaction session and each
application's state mix. It reads `pg_sage`'s history and, on the monitored database, the
roles it names (bounded, read-only). Its candidates cite the pack's connection hours and the
`max_connections` setting.

| Kind | When | Output |
|---|---|---|
| `recycle_herd` | In at least 3 samples, more than 30% of an application's sessions (10 or more) connected in the same minute. | `advice`: jitter the pool's connection lifetime, so its sessions stop reconnecting together. |
| `pooler_sizing` | An application with 10 or more sessions on average, at least half of them idle. | `advice`: a pooler in transaction mode with a pool of its active peak plus 20%, and what transaction mode breaks. |
| `idle_in_transaction_timeout` | A role's session sat idle in a transaction for more than 10 minutes, and the role's own timeout is unset or looser. | `set_role_timeout`: `idle_in_transaction_session_timeout = '600000ms'` for the role in this database. Its rollback restores the role's prior value, or resets it. |
| `role_connection_limit` | Under 10% of usable connections (`max_connections` less the reserved slots) were free at the window's peak, and the role's peak is over its fair share (the usable connections over the roles seen). | `set_role_connection_limit`: the role's peak plus 20%, capped at the usable connections, and never below the peak plus the contract's margin (2 sessions or 10%). Its rollback restores the prior limit. |

A session whose `backend_start` pg_sage's role cannot see has no age and is skipped. A
negative age (clock skew, or a session that connected during the read) is skipped too. The
coverage counts both. A truncated age read is judged against the application's session
total, so a cut list never inflates a share.

**Always approved.** Both steps need a person's approval at every trust level, because a
review has none of a workload budget's warn-mode shadow data. A role the role-budget contract
refuses (a superuser, pg_sage's own role, a `pg_` role or a provider administration role) gets
no step, and the coverage names it. When the roles cannot be read, each step becomes advice.
Pooler mode, pool size and lifetimes are advice only: pg_sage never changes a pooler.

Application names never enter a model tier: the pack's connections section holds none, and
candidates are not part of a pack. The review's numbers are the herds found and the headroom
at peak. The headroom after accepted steps is the next review's headroom at peak.

## Plan-stability generator

The `planstability` generator looks for the statistical cliffs a plan can fall off before it
does: a column the planner's sample describes badly that a top query filters on. It reads the
pack's top 20 queries by total time, their statement texts (filters are read from the
normalized text: `col = $1`, `$1 = col`, `col IS NOT NULL`, `col < $1`; a text that does not lex
is skipped with the reason) and, from the monitored database through the bounded read-only
reader, `pg_stats` (null fraction, distinct values, correlation and the leading
`most_common_freqs` value: a frequency, never a value), `pg_attribute.attstattarget`,
`pg_class.reltuples` and `n_mod_since_analyze`. No user data is read.

A [`plan_flip_after_analyze`](findings.md#plan_flip_after_analyze) finding whose one
re-ANALYZE did not restore the earlier plan is reopened with `reanalyze_verdict: no_change` and
`handed_to: plan_stability` and no SQL: pg_sage never runs a second automatic ANALYZE of that
relation within 24 hours. The per-column statistics target this generator proposes for the
query's filtered columns is the next step for it.

| Cliff | Flagged when | Proposal |
|---|---|---|
| Tiny non-null share | `null_frac` >= 0.999 on a column filtered with `=` or `IS NOT NULL` | `set_column_statistics`, then `analyze_table` |
| Skew beyond the sample | leading `most_common_freqs` > 0.5 with more than 1000 distinct values | `set_column_statistics`, then `analyze_table` |
| Time-ordered inserts past the histogram | `correlation` > 0.95, more than 10% of `reltuples` changed since the last ANALYZE, and more distinct values than the column's target (fewer means every value is in the most-common list and there is no histogram) | `set_column_statistics`, then `analyze_table` |
| Correlated predicates | two or more equality columns of one table whose stored plan's scan estimate is off from its actual rows by more than 10x | `create_statistics` (`dependencies`, `mcv`), pg_sage's form |

The new target is 10 times the column's current one (the `default_statistics_target` when it has
none), at most 10000; the rollback sets the prior target back (`-1` for the default, which reads
`NULL` on PostgreSQL 17+). A column already at 10000, or with statistics switched off (target 0)
by its owner, gets advice instead. `set_column_statistics` is a moderate, reversible action
that follows the planner-statistics class's trust level, like `create_statistics`; it takes
SHARE UPDATE EXCLUSIVE under `lock_timeout`, and its read-back is `pg_attribute.attstattarget`.
Without table ownership (which `SET STATISTICS` needs), or for a name the executor's validator
refuses (a `;` in an identifier), the step is an operator step with the exact SQL.

Without an executed plan (no `auto_explain`, as on Neon and Lakebase) the correlated-predicate
rule compares the stored estimate with the rows each call returned (estimate drift only) and
says so; without a captured plan it is not checked, and the coverage says which query.

A table whose statistics pg_sage cannot read is `unknown`, never "no risk": the coverage is
partial with `pg_stats:<table>` missing and a note with the reason, which is the exact
`GRANT SELECT ON TABLE ... TO <role>` when the role cannot read it, "never analyzed" when the
table has no statistics yet, or "not found". A filtered column with no `pg_stats` row of its
own (a partitioned table keeps only inherited statistics, or the column was added since the last
ANALYZE) is noted as unknown the same way.

## Capacity generator

The `capacity` generator (rubric dimension `capacity_cost`) gives a review one capacity
section. It reads the pack's growth section (the daily database size and the largest tables'
sizes), the window's query rollups by day, and, with a cloud provider resource, the hourly host
usage `cost` right-sizes from. Every output is `advice` for a person.

| Kind | When | Output |
|---|---|---|
| `growth_plan`, `table_growth` | A size series with 7 or more daily points. | A least-squares line through the days (a missing day keeps its place), projected 30, 90 and 365 days past the last measured day, each with a band of twice a prediction's standard error, and the fit error (the residual standard error). A falling fit projects shrinkage and never a size below zero. |
| `storage_headroom` | Cloud telemetry reports the allocated storage, and the database's 90-day projection plus its band reaches 85% of it. | Grow the instance's storage. The database is a lower bound: WAL and other databases come on top. |
| `right_size` | `cost.EvaluateRightsize` says downsize or upsize. | The current and target class, the CPU projection, and the monthly compute change at the `cost.rates` entry's prices, with their source and date; without rates, "unknown: no operator rates". |
| `idle_database` | 14 or more observed days in a row, ending at the window's end, without a workload call (pg_sage's own statements are not workload). | The database's size and, with a `storage_gb_month` rate, its monthly storage cost; archiving and dropping it is the operator's decision. |

**Never automatic.** An instance class or storage change is an operator step at the provider,
at every trust level; no candidate carries SQL. pg_sage ships no price tables: no dollar figure
appears anywhere in the section without `cost.rates`.

**Unknown, not silent.** Fewer than 7 daily points is a note, not a projection. A right-size
verdict of unknown (no hourly usage, a size with no vCPU count, stale telemetry) and a
right-size error are carried in the coverage as `unknown:` with their text, and the coverage is
partial with `rightsize` missing. Without a cloud resource, the coverage says SQL alone gives
growth and idle databases. A day the rollup did not observe (no rollup row, pg_sage's own
included, and no coverage hour) ends the quiet run: no history is no proof of idleness. A
statement whose text pg_sage never captured counts as workload.

The review's number is the forecast error: each review records its 30-day database size
projection in `sage.review_capacity_forecast`, and the first later review whose pack holds the
target day's size measures it once, as (projected - measured) / measured. A second pass at the
same mark changes nothing. `capacity.Idle` is the idle rule fleetpol-03 reuses.

## Model pass

After the generators, a model **ranks** the candidates, **attacks** each one (what could make
it wrong), **links** related ones (`conflicts`, `depends_on`, `supersedes`, `related`),
proposes **hypotheses** the generators missed, and writes one short **report** text per
dimension (`internal/review/modelpass`). It is on by default whenever a model is usable
(`review.model_pass: auto`). The model reviews and ranks. It never writes SQL that runs and
never applies anything: its answer schema has no SQL field, any SQL-looking text it writes is
shown to a person as prose, and a candidate's own SQL is never shown to it. Every step still
comes from a generator template and runs through the executor's contract for its action type.

**What leaves the network.** Only the pack's model tier, each candidate's numbers and the
grade. The model reads:

| Evidence | Example of what the model sees |
|---|---|
| A pack fact, cited as `E5` | `index/idx/public.orders_created_at {"size_bytes":8192,"scans":0,...}` |
| A candidate, cited as `E1` | `{"candidate_id":"3f9a...","kind":"unused","target":"public.orders_created_at","prediction":{"metric":"index_bytes","tier":"none"},...}` |
| The grade | `dimension indexes: grade C, score 60, coverage 100` |
| A pack section, on request | the model tier's `workload.json` (normalized statement texts, literals redacted, wrapped as untrusted data) |

The model tier holds no most-common values, histogram bounds, plan filter values or literals
([security.md](security.md#workload-evidence-pack-tiers)). With `review.hash_identifiers: true`
every schema, table, index and column name is a keyed pseudonym (`t_<10 hex>`) in what the
model reads, and the report maps them back to names. Which providers may receive the model
tier is decision D3 (OWNER ACTION); see
[security.md](security.md#sage-review-model-pass-and-provider-retention).

**Which model.** `llm.review_llm` when enabled, else the optimizer model
(`llm.optimizer_llm`), else the general client: `ForPurpose("review")`. Usage and cost are
labelled with the purpose `review`. Any OpenAI-compatible provider works, a local one too.
When the review model is disabled, its daily budget is spent or its breaker is open, the pass
runs on the general client if `llm.review_llm.fallback_to_general` is true (the default), and
otherwise is skipped (a spent budget or an open breaker skips it even with `model_pass: on`).
`llm.review_llm.max_output_tokens` caps each call's completion below the pass's own cap of
6,000 tokens.

**Variance control.** One shot is not a review: `review.model_runs` (default 3, odd, 1-5)
independent runs are combined in code.

- `majority` (default): a candidate is kept when most runs keep it, at its median rank (ties:
  more votes, then the deterministic score). A hypothesis survives when most runs propose the
  same `(kind, target)`, case and spaces aside; a link when most runs give the same pair and
  relation. Report text comes from the first run that wrote a cited one.
- `refute`: run 1 ranks; run 2 sees run 1's kept items and must refute each with cited
  evidence. A refutation whose claims survive the filter drops the item. Only these two runs
  are made.
- `model_runs: 1` applies no consensus; the report says `single run`.

When fewer than a majority of the runs finish, the model's output is not used.

**Claims.** Every ranking, link, hypothesis and report text carries claims that cite the
aliases of the run's evidence. A claim citing an alias the run does not hold (`E999`) is
dropped, and so is a claim with a number its cited evidence does not hold; an attack,
rationale or text with an ungrounded number is dropped too. A ranking of an unknown candidate,
a duplicate, an invalid decision or rank, a link or hypothesis without a surviving claim, and
an uncited report text are rejected and listed in the report with the reason. The tool-calling
transport does not retry a 429: a rate-limited run stops with error class `rate`.

**Without a model** (no model configured, `llm.enabled: false`, `review.model_pass: off`, the
token budget exhausted, the breaker open, every run failed, or a run that could not be
recorded) the review is still graded and still has candidates. They are ranked by a
deterministic score, the predicted effect (the size in percent, 1 when unknown) times the
ladder tier's confidence (fleet 1, canary 0.9, clone 0.75, plan_only 0.5, none 0.25) divided
by the risk weight (1, plus 1 when the step needs approval, plus 1 when an executed step
cannot be rolled back), and each dimension's text comes from a template
(`internal/review/report/templates`). The report's `model_pass` is `skipped` or `failed` with
the reason (`no model is configured`, `review.model_pass is off`, `token budget`, `only 0 of 3
model runs finished: ...`). `model_pass` moves from `pending` to `running` to `done`, `failed`
or `skipped`. Turning `review.model_pass` off mid-review lets the run in flight finish and
skips the rest. A review without candidates is not sent to a model.

**Stored runs.** Every model run is one row of `sage.review_model_run`, written once with its
final status (`done`, `failed`, `skipped`): `attempt`, `role` (`rank` or `refute`), `model`,
`prompt_version`, `pack_hash`, the raw output (capped at 256 KiB, `raw_truncated` set when
cut), `output_sha256` of the whole output, `kept_claims`, `dropped_claims`, `tokens_in`,
`tokens_out`, `cost_usd` (NULL when `llm.pricing` does not price the model), `error_class`
(`rate`, `timeout`, ...) and `reason`. The table is append-only: a trigger refuses every
UPDATE. A plan's audit record cites `output_sha256`, so a plan can be replayed against a later
model. Rows go with their review run.

## From review to plan

A review ends as **one plan** (`internal/review/plan`), stored through the ordinary plan store
([policy-gate.md](policy-gate.md#plan-approval-coverage)): validation, caps, plan hash and the
created event. Nothing runs until a person approves it, and every step runs, if ever, through
the executor's contract for its action type. An agent over MCP can read a review plan
(`list_plans`, `get_plan`) and never approve one.

**Generator steps** are the candidates the review kept: by the model pass's consensus, or by
the deterministic ranking when no model ran. A candidate the model dropped is not planned. An
`advice` or `operator` candidate stays advice for a person.

**Model steps.** A model hypothesis becomes a step only through a typed template. The template
resolves the hypothesis target against the pack's **local tier** (hashed identifiers are mapped
back locally; a table, column or setting the pack does not hold is refused), renders the SQL
in deterministic code, and asks the lab-03 ladder's lowest available tier for a prediction.
Only a prediction of at least `optimizer.hypopg_min_improvement_pct` becomes a step; the
hypothesis must also cite evidence ids that resolve in the pack.

| Kind | Target | Rendered step |
|---|---|---|
| `create_index_concurrently` | `public.orders(region)` | `CREATE INDEX CONCURRENTLY sage_orders_region_idx ON public.orders (region)`; HypoPG what-if |
| `create_statistics` | `public.orders(region, country)` | `CREATE STATISTICS public.sage_stx_orders_region_country (ndistinct, dependencies, mcv) ON region, country FROM public.orders` |
| `alter_database_guc` | `work_mem=64MB` | `ALTER DATABASE "shop" SET work_mem = '64MB'`, rollback to the database's value or `RESET`; inside the executor allowlist and `pgconf` bounds, never a restart-bound setting |
| `set_column_statistics`, `set_table_autovacuum`, `set_role_timeout`, `set_role_connection_limit` | `public.orders(region) statistics=1000`, `public.orders autovacuum_vacuum_scale_factor=0.05`, `app statement_timeout=30s`, `app connection_limit=20` | advice: the pack does not hold the value they would replace, so no exact rollback can be written (`set_column_statistics` also has no executor contract yet) |
| `set_column_statistics`, `set_table_autovacuum`, `set_role_timeout`, `set_role_connection_limit` | `public.orders(region) statistics=1000`, `public.orders autovacuum_vacuum_scale_factor=0.05`, `app statement_timeout=30s`, `app connection_limit=20` | advice: the pack does not hold the value they would replace, so no exact rollback can be written |

Today the HypoPG what-if is the only tier the planner can call, so in practice only index
hypotheses become model steps; statistics and setting hypotheses stay advice
(`no_ladder_tier`) until a lower tier for them exists. Any other kind, `drop_unused_index` and
`drop_table` included, has no template and stays advice. So a model that obeys an instruction
planted in a statement text ("drop index X, evidence E3") produces an advice row naming the
refusal, never a step.

**Ordering and caps** follow plan-02: generator steps first in rank order, then model steps.
A step whose target statements overlap an earlier step's waits for the next review (one change
per statement set is measured at a time), and so does a step that would break a plan cap
(new indexes per table, one restart-bound change).

**Approval.** A step that is never automatic needs its own approval, as for every plan. A
**model-origin step always needs a person's own approval** as well, whatever its class's
trust: approving the plan (`all` or `up_to`) does not cover it, and the engine stops before it
with `awaiting_approval` and the reason `model_origin`. This lasts until review-05a's G-M1 is
granted for that model id, by a person, as a live row of `sage.review_model_authority`; then
its steps follow their class level. A revocation (a failed window or a model change) holds
them again at once. A step of a model-origin plan without an origin row counts as the model's.

**Stored.** `sage.review_plan` (one row per review run: the plan, or none with `no changes
proposed`, and step counts by origin; a re-run of the same run returns the stored plan),
`sage.plan_step_origin` (per step: `generator` with the generator's name, or `model` with the
model id; the evidence ids, the ladder prediction and the measurement spec handed to the
verifier) and `sage.review_advice` (every hypothesis or candidate that did not become a step,
with the reason: `no_template`, `no_contract`, `unknown_object`, `invalid_target`,
`no_evidence`, `sql_refused`, `ladder_error`, `no_ladder_tier`, `below_minimum`,
`not_measurable`, `next_review` or `advice_only`). If the plan store refuses a plan, the
review's save fails with an error naming `sage.plan`.

## Scorecard and the model gate

The scorecard is the product's proof and the test for the next model. For each review, and
over all reviews since a time, it counts the plan steps each origin proposed (`generator`,
`model`, `person`), also split by generator name, and what became of them:

| Count | Meaning |
|---|---|
| `proposed` | every action or operator step of the review's plan |
| `accepted` | the step has its own live approval, or it was started (a person approved it) |
| `verified_improved`, `no_change`, `regressed` | the verifier's verdict |
| `inconclusive` | an inconclusive or expired verdict |
| `unverifiable` | the verifier could not measure it |
| `reverted` | any rollback of the step: its state, or its action's `rolling_back`, `rolled_back`, `rollback_failed` or `reverted` outcome |
| `pending` | no verdict yet |
| `decided` | improved, no change or regressed, plus every reverted step |

`model_only_accepted` counts accepted model steps that no generator of the same review also
proposed (same action and SQL). The reads are SQL over `sage.review_run`,
`sage.review_model_run`, `sage.review_plan`, `sage.plan_step`, `sage.plan_step_origin`,
`sage.plan_step_approval` and `sage.action_log`; the arithmetic is in Go. Read it with
`GET /api/v1/databases/{db}/review/scorecard` ([API](api.md#sage-review-scorecard)) or the MCP
tool `review_scorecard`.

**Which reviews count.** The gate reads the newest `review.gate.min_reviews` reviews of the
database that are `done`, on a real database, and whose model pass finished on the running
review model and no other. A review of a database named in `review.gate.fixture_databases`
(bench and game-day fixtures) or by a clone receipt (`sage.clone_instances`) never counts. A
new model id starts with no reviews: that is the test for the next model.

**G-M1 (give the model more).** It passes when all three hold:

1. at least `review.gate.min_reviews` reviews count (default 50);
2. the model's verified-improved rate is at least the generators' rate minus
   `review.gate.max_gap_points` (default 10 points);
3. the model's revert rate is at most `review.gate.max_revert_pct` (default 2%).

Rates are decided steps over decided steps; the Wilson 95% intervals are reported beside them,
never instead of them. Invalid counts, no decided model or generator steps, or an error never
pass. A pass does not promote by itself: it files one high-risk **autonomy promotion** in the
inbox with the numbers. Only an admin's approval
(`POST /api/v1/databases/{db}/review/gate/{id}/approve`) grants it: the newest G-M1
evaluation, still pending, becomes `granted` and sets review-03b's
`sage.review_model_authority` row for that model id, so its steps run at their class's trust
level without their own approval. A grant for one model id never applies to another.

**Revocation is automatic.** At each evaluation (after every grade), a window that fails
G-M1, a change of the running review model, or a grant with no approved evaluation behind it
revokes the flag at once, with a `revoked` row and a digest line. Evaluations are hourly ticks, so a window that fails within the hour its grant was approved in is revoked at the next tick's evaluation. Plans of another model id
never use the old grant, since the grant is keyed by model id.

**G-M2 (drop the model pass).** Over the same reviews, if no accepted step was the model's
alone, `review.model_pass: auto` stops calling that model id for the review, and the digest
says so. It only narrows: `review.model_pass: on` always runs the pass, a gate that cannot be
read changes nothing, and a new model id starts on. The model pass reads this verdict once the review pipeline runs it (review-04a); until then G-M2 is evaluated, recorded and stated in the digest, but no review calls a model to skip.

**Stored.** `sage.review_gate`: one row per model, rule and hourly evaluation tick (`model`,
`rule`, `tick`, `evaluated_at`, `verdict` `not_eligible`, `pending_approval`, `granted`,
`revoked`, `model_on` or `model_off`, `reason`, `evidence` with the counts and intervals,
`approved_by`, `approved_at`, `revoked_at`). A `granted` row without an approval is refused by
the table's CHECK.

**The thresholds are proposals for the owner** (decision D11: 50 reviews, 10 points, 2%).
Measuring them takes 50 or more reviews on real databases, which is calendar time on real
deployments; they ship as proposals until the owner confirms or changes them.

### On the Review and Value pages

The Review page shows the scorecard under the report; the Value page (`#/value`) shows a
compact section for the database picked in the header (the whole fleet has none, and says so),
with a link to the review. Both read `GET /api/v1/databases/{db}/review/scorecard` each minute.

- **The gate card** is the newest G-M1 evaluation: "Model gate: 32 of 50 reviews; model 78%
  improved vs generators 84%; reverts 0%", then its verdict (`Not eligible`, `Eligible, not
  yet filed for approval`, `Waiting for an admin's approval`, `Granted by an admin` with the
  approver, `Revoked`) and its reason, and the newest G-M2 reason. An evaluation without its counts shows its reason instead. Before the
  first evaluation it reads "Model gate: 0 of 50 reviews; not evaluated yet".
- **Approval** happens in the Inbox, like every autonomy promotion: a pending G-M1 shows an
  admin "Approve in the Inbox", and the Inbox's Approve (typed-phrase confirm, high risk)
  posts `POST /api/v1/databases/{db}/review/gate/{id}/approve`. Other roles see the verdict
  only.
- **The table by origin** (generators, model, people): proposed; accepted of proposed;
  improved and reverted of decided, each with its rate; no change, regressed and pending.
  The Value page shows proposed, improved and reverted.

Every count is shown with its denominator. A rate whose denominator is under 10 reads "too
few to judge", never a percentage. A count that cannot be one (negative, fractional, or more
than its denominator, a rate above 100%) reads "invalid", never the number. No reviews in the
window reads "No reviews yet".
