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
