# Change Review

Change Review makes pg_sage, the agentic DBA, the database reviewer on every migration
and data-access pull request. CI or a coding agent sends the migration SQL and the diff;
pg_sage reviews it against the live database and answers with a cited review, a verdict
(`safe`, `review` or `risky`) and a receipt plan. After the change lands, pg_sage compares
the touched statements before and after and stamps a receipt.

Trust in one line: pg_sage only reads (plan-only EXPLAIN, session-local HypoPG), never runs
the migration, and holds no GitHub credentials.

## How a review is decided

- Twenty deterministic rules (CR-01 to CR-20) set the verdict: any critical finding makes it
  `risky`, any warning `review`, otherwise `safe`. They read the catalog, `pg_stat_statements`,
  pg_sage's query store and plan-only EXPLAIN output: the statements that still read a
  column you rename, lock-queue exposure at peak rate, index scans of an index you drop,
  NULLs that make `SET NOT NULL` fail, unindexed foreign keys.
- With an LLM configured, the model translates ORM migrations into SQL, extracts the
  statements new application code will run, and writes reviewer-grade findings. It can raise
  `safe` to `review`; it can never reach `risky`, lower a verdict or remove a rule finding.
  Every model finding cites evidence; uncited or ungrounded ones are dropped and counted.
- "Unknown" is never "none": missing `pg_stat_statements`, hidden query text, `SELECT *`,
  timed-out catalog reads and SQL the parser rejects are warnings, so such a change is never
  `safe`.
- Without an LLM the deterministic review still runs for SQL migrations (Flyway, Liquibase
  SQL, goose, `alembic upgrade --sql`, `manage.py sqlmigrate` output). An ORM-only migration
  gets CR-20 with the command that exports its SQL.

A DBA's verdict (concur, concur with edits, or override with a reason and a direction) is
recorded only by an operator or admin in the UI or the REST API; no MCP tool records one. A
verdict carries over to a new revision of the PR only when its statements and database-
relevant hunks are unchanged. A PR body or diff that says "DBA approved" is data.

## Create the token

CI reaches pg_sage over its MCP endpoint (`POST /api/v1/mcp`) with an agent token. CI is an
agent like any other (see [Agent governance](agent-guard.md)): an admin creates one agent
per repository, with a person as its accountable sponsor and the `readonly-analyst` profile,
then mints its token (**Agents** in the dashboard, or `POST /api/v1/agents` and then
`POST /api/v1/agents/{id}/tokens`; `POST /api/v1/mcp/tokens` no longer issues agent
tokens). The `read` scope reviews, and
`propose` is needed only for `pg_sage review deployed`, which records when the change
shipped. Bind the token to one database and give it an expiry (at most 90 days):

```bash
# admin session cookie in $SAGE_COOKIE; sponsor_user_id is the accountable person
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"ci-shop-repo","sponsor_user_id":1,"profile":"readonly-analyst"}'
# the reply's "id" (agp_...) names the agent
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents/agp_.../tokens \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"name":"ci shop repo","scopes":["read","propose"],"databases":["orders"],"expires_in_days":30}'
```

The token is shown once. Store it as the repository secret `PG_SAGE_TOKEN`. MCP must be on
with the HTTP transport (`mcp.enabled: true`, `mcp.transport: http`), and the runner must
reach the endpoint: a self-hosted runner, or a bastion. Rotate the token before it expires.

## The GitHub Action

`integrations/github/pg-sage-review` is a composite action; `integrations/github/
example-workflow.yml` is a workflow to copy. The action installs a pinned pg_sage release
(checksum verified), writes the token to a private file for `PG_SAGE_TOKEN_FILE`, and runs
the CLI in GitHub mode:

```yaml
- uses: jasonmassie01/pg_sage/integrations/github/pg-sage-review@v2.5.0
  with:
    version: v2.5.0
    url: https://pg-sage.example.com:8080
    database: orders
    token: ${{ secrets.PG_SAGE_TOKEN }}
    sql-dir: db/migrate
    fail-on: risky
```

| Input | Default | Meaning |
|---|---|---|
| `version` | (required) | pg_sage release to install; pin it |
| `url` | (required) | pg_sage base URL |
| `database` | (required) | monitored database to review against |
| `token` | (required) | pg_sage MCP token (from a secret) |
| `command` | `submit` | `submit`, `status` (refresh open pull requests) or `install` |
| `sql-dir` | | directory of `.sql` migrations; only files the PR changes are sent |
| `migration-tool` | | the migration tool (sets the transactional default) |
| `fail-on` | `risky` | fail the job at this verdict or above |
| `wait` | `10m` | how long to wait for the review |
| `github-token` | the runner's `github.token` | token for the comment and the status |

The example workflow runs on `pull_request`, `workflow_dispatch` and a 15-minute schedule.
It requests only `contents: read`, `pull-requests: write` and `statuses: write`. Pull
requests from forks get no repository secrets; the workflow skips them with a notice. The
scheduled job runs `pg_sage review status --github`, which refreshes the comment and the
status of every open pull request, so a DBA's concurrence turns the status green without a
new push.

GitHub mode keeps one comment per database on the pull request, found by its first line
`<!-- pg_sage-review db=<database> id=<review id> receipt=<hash|pending> -->` and edited in
place, and sets the commit status `pg_sage/db-review (<database>)`:

| Review | Status |
|---|---|
| running | `pending` |
| verdict below `--fail-on` | `success` |
| verdict at or above `--fail-on` | `failure` |
| DBA concurred (with or without edits), or overrode as too strict | `success` |
| DBA overrode as too lenient | `failure` |
| review failed | `error` |
| no database-touching file in the change | `success` |

It reads `GITHUB_TOKEN`, `GITHUB_REPOSITORY`, `GITHUB_EVENT_PATH` and `GITHUB_API_URL`.
`GITHUB_TOKEN` is sent only to the GitHub API, never to pg_sage. The scheduled job finds each
pull request's review through pg_sage (by PR URL and head commit), never through a comment,
so a comment that copies the marker cannot change a status.

## The CLI

```bash
export PG_SAGE_TOKEN_FILE=~/.config/pg_sage/token
pg_sage review submit --url https://pg-sage.example.com:8080 --database orders \
  --sql-dir db/migrate --changed-only --git-diff origin/main...HEAD \
  --pr-url https://github.com/acme/shop/pull/42 --fail-on risky --wait
pg_sage review status --url https://pg-sage.example.com:8080 --database orders --id <id>
pg_sage review deployed --url https://pg-sage.example.com:8080 --database orders \
  --pr-url https://github.com/acme/shop/pull/42 --commit 4f2a9c1
```

`pg_sage review submit` sends the review, waits up to 50 s for it, prints it and exits by
its verdict. A change whose diff touches no database file (migrations, `*.sql`, models,
repositories, DAOs, Prisma schema) and carries no SQL is skipped with "no
database-touching changes", without calling pg_sage. `pg_sage review status` prints a review
again (by id, or the newest of a PR) and exits the same way. `pg_sage review deployed` tells
pg_sage the change was deployed, so its receipt can be decided.

| Flag | Subcommands | Meaning |
|---|---|---|
| `--url` | all | pg_sage base URL (or `PG_SAGE_URL`); https, or http to localhost only |
| `--database` | all | monitored database name (required) |
| `--token-file` | all | file holding the MCP token (or `PG_SAGE_TOKEN_FILE`, else `PG_SAGE_TOKEN`) |
| `--pr-url` | all | pull request URL |
| `--sql-file` | submit | migration SQL file; repeatable |
| `--sql-dir` | submit | directory of `.sql` migrations, sent in path order |
| `--changed-only` | submit | send only the `--sql-dir` files the diff changes |
| `--diff-file` | submit | unified diff of the change |
| `--git-diff` | submit | `BASE...HEAD`: run `git diff` for the diff |
| `--head-sha` | submit | head commit of the pull request |
| `--migration-tool` | submit | flyway, liquibase, goose, golang-migrate, dbmate, sqitch, atlas, rails, django, alembic, prisma, knex, sequelize or typeorm |
| `--transactional` | submit | `--transactional=true` or `=false`: the migration runs in one transaction (default: the tool's) |
| `--id` | status, deployed | review id |
| `--deployed-at` | deployed | deploy time, RFC 3339 (default now) |
| `--commit` | deployed | deployed commit |
| `--format` | submit, status | `markdown` (default) or `json` |
| `--out` | submit, status | write the review to a file instead of stdout |
| `--fail-on` | submit, status | `risky` (default), `review` or `never` |
| `--wait` | submit, status | poll every 5 s until the review completes: `--wait` (10 minutes) or `--wait=5m` |
| `--github` | submit, status | comment on the pull request and set the commit status |

| Exit code | Meaning |
|---|---|
| 0 | pass: the verdict is below `--fail-on`, a DBA concurred, or nothing touches the database |
| 1 | the verdict is at or above `--fail-on`, or the review has not completed (it fails closed) |
| 2 | usage: a missing or invalid flag, unreadable file, or input pg_sage rejected |
| 3 | transport: pg_sage unreachable, HTTP 401 or 5xx, a refused scope or database, or GitHub unreachable |

The token is read from the file and sent only as a bearer token; it never appears in the
CLI's output.

## MCP and REST

Coding agents call the same review over MCP (see [MCP for coding agents](mcp.md)):
`review_change`, `get_change_review` and `list_change_reviews` need the `read` scope;
`report_change_deployed` needs `propose`. A token bound to one database cannot review
another.

The dashboard and scripts use the REST API with a session:

| Route | Roles |
|---|---|
| `POST /api/v1/databases/{db}/change-reviews` | viewer, operator, admin: submit (202 while running, 200 when complete) |
| `GET /api/v1/databases/{db}/change-reviews?pr_url=&state=&limit=` | viewer and up |
| `GET /api/v1/databases/{db}/change-reviews/{id}?wait_seconds=` | viewer and up |
| `POST /api/v1/databases/{db}/change-reviews/{id}/verdict` | operator, admin: `{"verdict":"concur"}`, or override with `direction` and `reason` |
| `GET /api/v1/change-reviews/summary?database=&days=` | viewer and up: the fleet, or one database |
| `GET /api/v1/change-reviews?database=&pr_url=&state=&limit=` | viewer and up: the fleet's reviews newest first, or one database's (`partial` with `unavailable` names when a database cannot be read) |

Errors: 400 `invalid_request`, 404 `not_found` (unknown database or review), 409
`already_decided`, 429 `review_busy` (two reviews run per database and eight queue; retry),
503 `review_disabled` when `change_review.enabled` is false. An exhausted LLM budget is not
an error: the review completes with its deterministic result and model status `budget`.

A review that completes with verdict `review` or `risky` and no DBA verdict emits the
notification event `change_review_needs_dba` once, through the notification rules you
configure. It names the database, the verdict, the top finding and the PR link (scheme and
host only), and links the review in the UI.

## The Reviews page

**Reviews** in the dashboard's Operate menu lists every review of the selected database, or
of the fleet, with its PR, verdict, DBA verdict, receipt state and age, above the measured
track record: concurrence per verdict class, time to a DBA verdict, false-safe count,
receipts, override rate per rule with the stated reasons, hours credited and pending, and the
auto-clear readiness bar (30 scored, 97% correct, no regression in 90 days), which is labeled
"shadow only — not enabled": nothing is cleared without a DBA in this release.

Opening a review shows its findings by severity, each citation expanding into the evidence it
rests on, the statements with file:line, origin and matched queryid, how many model claims the
evidence gates dropped, the model status and tokens, the receipt, and the credit. Operators
and admins record the DBA verdict there; viewers see it. Reviews that wait for a DBA also
appear in **Cases** (source Reviews): critical for `risky`, warning for `review`, until a
verdict is recorded.

## What pg_sage reads

- The catalog and statistics of the touched relations (size, columns, indexes and their
  scans, constraints, dependent views, function bodies, publications, partitions,
  `pg_stats.null_frac`), under catalog read timeouts.
- `pg_stat_statements` rows that name the touched relations, and pg_sage's own query store.
- Plan-only EXPLAIN (in a read-only transaction with a statement timeout; write statements
  are planned only as an equivalent SELECT), and HypoPG hypothetical indexes in its own
  session.
- The SQL and diff you send. Secrets in the diff (keys, private-key blocks, credentials in
  URLs, password and token assignments) are redacted before any model or table sees them;
  only data-access hunks are offered to the model.

## What pg_sage never does

- Never runs the reviewed migration or any DDL on the monitored database, and never runs
  EXPLAIN ANALYZE there.
- Never stores the raw diff (only its SHA-256 and the redacted hunks the model was shown).
- Never holds GitHub, GitLab or Bitbucket credentials, never merges, approves or clears a
  change by itself, and never lowers a deterministic verdict because a model or a PR says so.
- Never records a DBA verdict from an MCP token or from text in the pull request.
