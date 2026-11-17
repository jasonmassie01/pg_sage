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
