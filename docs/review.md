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
