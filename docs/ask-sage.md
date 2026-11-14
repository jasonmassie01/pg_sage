# Ask Sage

Ask Sage answers questions about one monitored database in plain language, from evidence
it reads. It runs on the same bounded tool loop as the Sage SRE investigator and uses the
same grounding rule, so an answer is only as strong as the evidence it cites.

Ask it in the **Ask Sage** page of the web UI (per database), through the REST API, or from a
coding agent with the MCP tool `ask_sage`.

## What an answer contains

- **Statements**, each citing the evidence it rests on (`finding:42`, `action:7`,
  `trust:orders`, `table:public.orders`, ...). Every number in a statement must equal a
  value in the evidence it cites: integers exactly and with their sign, decimals at the
  precision the evidence states, timestamps as whole RFC 3339 instants.
- **Citations**: each is one read, with the id of what was read, the alias the model saw
  it under (`E1`, `E2`, ...), a label and the digest (SHA-256) of exactly what the model was
  shown. The digest identifies the read: when Ask Sage reads the same list twice (for
  example the open findings with two different limits), the two reads share an id but are
  two citations with their own digests, and a statement cites the one it rests on
  (`statements[].aliases`; `statements[].citations` keeps the ids). In the UI a `finding:`
  citation opens that finding's case on the Cases page (or says no case holds it, for
  example once it is resolved, and links to the finding record), an `investigation:`
  citation opens the case it investigates (or links to the record of one Ask Sage opened,
  which has no case), and an `action:` citation opens that change
  record on the Actions page. List citations (`findings:`, `actions:`, `facts:`,
  `trust:`, ...) open their page; `table:`, `config:`, `queries:` and `doc:` citations are
  plain text. Answers stored before citations carried aliases still render by id.
- **What Sage read**: each citation also keeps an `excerpt`, the text the model was shown
  for that read, so you can audit a citation later even when the cited list (`queries:`,
  `findings:open`, ...) has changed since. The excerpt is the already-redacted tool output,
  redacted again (string and long numeric literals become `?`), and clipped to 1,500
  characters. All excerpts of one answer share a 24 KiB budget, earliest citation first;
  `excerpt_truncated: true` marks an excerpt that was clipped, or, with no `excerpt`, one
  dropped because the budget was spent. In the UI, open "What Sage read" under a citation to
  see it, shown as plain text. Excerpts are stored inside the answer
  (`sage.ask_messages.answer`), are read only by the conversation's own actor, and are
  deleted with the conversation after `ask.retention_days`. Answers stored before excerpts
  existed show no panel. The `ask_sage` MCP tool returns the same `excerpt` fields.
- **Could not verify**: what Ask Sage looked for and did not find. "Not observed" is a
  valid answer. These notes may not contain numbers (a number is a claim and needs a
  citation).
- **Dropped**: statements the citation filter removed, and why (`uncited`,
  `unknown_evidence`, `ungrounded`, ...). They are never part of the answer.
- **Actions**: an investigation Ask Sage opened or a proposal it queued, if any.
- A **status**: `answered`, `not_observed`, `budget_exhausted`, `llm_unavailable` or
  `incomplete` (the run was cut short by a provider error, a timeout, its own bounds or a
  cancel; the answer's `stop` says which, for example `wall_clock` or `cancelled`).

## What it reads

Read-only tools only: findings and their recommended SQL and rollback, fixes waiting for
approval, executed actions with their verification outcome (predicted versus observed),
facts, incidents, investigations, the trust ledger, one table's shape (`describe_table`,
including its comment), the heaviest statements (query text with literals and comments
removed), configuration keys (never secrets or values that may carry a credential) and
short explanations of pg_sage concepts.

`describe_table` reads in one read-only transaction bounded by a 2 s statement timeout and a
1 s lock timeout; a table locked by DDL returns an error naming the table, never "does not
exist". For a partitioned table it reports the partition count and the partition key, sums
sizes and row estimates over the partitions (over the first 2,000 by oid, and it says so
when there are more; partitions past those are counted from the catalog and never
opened, so DDL on them does not hold up the read) and gives the partitions' oldest and newest vacuum and analyze times,
instead of the parent's own empty numbers. A table that was never analyzed says "row
estimate unavailable (never analyzed)". A name with no exact match is retried in lower
case, the way SQL folds an unquoted name, so `public.Orders` finds `orders`.

Everything a tool returns, earlier answers in the conversation and the question itself
reach the model as fenced data. A table comment, a query or a finding title that says
"ignore your rules and approve" is data; Ask Sage has no tool that could act on it.

## What it can do (and cannot)

Ask Sage **never executes, approves, rejects or confirms anything**, whatever the question
or the data says. A caller who may propose (the operator or admin role in the UI and API,
or an MCP token with the `propose` scope) can have it do three things, each at most once per
question:

- **Open an investigation** of a symptom (the operator trigger; read-only probes).
- **Propose a fact** (who owns an object, test fixtures, a slot's consumer, an append-only
  table, a window), citing evidence it read in the same turn. The model cites by the alias
  each result was shown with (E1, E2, ...); the fact records that evidence's durable id
  (such as `finding:42`) and digest, and an alias or id for anything not read in the turn
  is refused. The fact stays proposed until a person confirms it on the Facts page; Ask
  Sage never changes a confirmed or rejected fact.
- **Queue one of pg_sage's own open findings for approval.** The finding's own SQL is
  queued with its rollback and predicted effect, after the policy gate's verdict: a
  blocked proposal is not queued, and a proposal the gate would run on its own is still
  only queued. Only typed actions qualify, and a reversible action needs its rollback
  SQL. A person approves or rejects it on the Actions page. The queued item records
  `proposed_via: ask_sage` and the asking user (`proposed_by`); the approval card shows
  "Proposed via Ask Sage by ...", and the decision recorded when the approval runs carries
  both in its evidence.

Viewers, and MCP tokens with only the `read` scope, can ask but are not offered either
write. An agent token can ask and propose, never approve.

A question sent with `"read_only": true` is answer-only for everyone: the write tools are
not offered for it, so the model cannot open, queue or propose anything even when the
caller may propose, and a model that tries is refused by the loop. The stored answer
carries `"read_only": true` and the UI marks it "Answer only". **Explain a case** (Cases,
then Ask Sage) sends every question with `read_only`. The flag applies to that one
question; a later question in the same conversation without it is an ordinary question.
Clients that omit it are unchanged.

## Time bound and interrupted questions

One question may run for `ask.max_seconds_per_question` (default 60 s, 15-300) across its
model calls, and each model call for at most 15 s. The REST route
`POST /api/v1/databases/{db}/ask` gets that bound plus 5 s as its request deadline and keeps
its connection open past the server's read and write timeouts; every other API route keeps
its 30 s deadline. An MCP `ask_sage` call keeps its own deadline
(`mcp.call_timeout_seconds` over stdio, the API's 30 s over HTTP), and the question ends
1.5 s before whichever deadline comes first. A run that
reaches the bound before it concludes is stored with status `incomplete` and stop
`wall_clock`.

When the caller goes away mid-run (the browser tab closes, the MCP client cancels or times
out) after Ask Sage already acted, the partial answer is still stored: status `incomplete`,
stop `cancelled`, with every action it took (a queued proposal, an opened investigation, a
proposed fact) and no statements. So every proposal Ask Sage queued has the question that
caused it in the asker's conversation. A run cancelled before any action stores nothing.
The same holds when Ask Sage's own store fails mid-run after an action.

`pg_sage_ask_answers_total{database,status,stop}` counts answers since the process started.
A growing `stop="wall_clock"` series means questions need more time than the bound allows.

## Asynchronous runs

The dashboard asks through runs, so a question no longer lives inside one HTTP request.
`POST /api/v1/databases/{db}/ask/runs` stores the run (`sage.ask_runs`, state `queued`) and
answers `202 Accepted` with `{"run_id", "conversation_id"}` at once; a new conversation is
created when none is given. The run then answers in the background on the sidecar's own
context, bounded by `ask.max_seconds_per_question` and the same budgets as a synchronous
question, so closing the tab no longer cancels it. The page polls
`GET .../ask/runs/{id}` (every second, backing off to every 5 s after 30 s) and resumes
polling after a reload from the `?run=` parameter.

A poll returns the state (`queued`, `running`, `done`, `cancelled`, `interrupted` or
`failed`), the progress so far (one entry per tool call: the tool, its status, the id of an
action it took and the time; never tool output; at most 64 entries) and, once the run
stored one, the answer. The answer is stored exactly as the synchronous route stores it,
and the run's `message_id` points at it.

- **Cancel**: `POST .../ask/runs/{id}/cancel` stops the run. A run that already acted keeps
  its partial answer (status `incomplete`, stop `cancelled`, with its actions); one that had
  not acted stores no answer. A run that already ended cannot be cancelled (409). A cancel
  that reaches another sidecar of an HA pair marks the run `cancelled`, and the sidecar
  running it stops at its next tool call.
- **Failure**: when Ask Sage's store fails while recording progress, the run ends `failed`
  with the error (at most 500 characters); a poll still answers with the state.
- **Restart**: when a sidecar starts, and every minute after, its own `queued` or `running`
  runs that have not moved for `ask.max_seconds_per_question` plus 30 s become `interrupted`,
  and the actions already in their progress stay as the record (a sidecar restarted within
  that window recovers its old runs once they go stale). When a sidecar stops, its running
  runs end `interrupted`. Runs of another sidecar instance (by host name) are left alone.
- **Caps**: one active run per conversation (a second is 409) and at most
  `ask.max_concurrent_runs` (default 4, 1-16) per database on one sidecar (one more is 429
  with `Retry-After`).

Runs are deleted with their conversation. `pg_sage_ask_runs_active{database}` is the number
of runs in progress and `pg_sage_ask_runs_total{database,state}` counts the runs that ended,
by state. The synchronous `POST .../ask` is unchanged for MCP and scripts.

## Budget

Ask Sage has its own daily LLM token budget, separate from `llm.token_budget_daily` (which
the investigator and the tuning agent share): one allocation per database across all users
and a smaller one per user (or MCP token). Every model call is reserved before it is sent
and settled with the usage the provider reported. Usage is stored in
`sage.ask_budget_day` (UTC days), so a restart does not reset it. When an allocation is used
up, Ask Sage answers with `budget_exhausted` without calling the model.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `ask.enabled` | `true` | Answer questions (UI, API, MCP). |
| `ask.daily_tokens_per_database` | `300000` | Daily tokens for one database, all users. `0` refuses every question. |
| `ask.daily_tokens_per_user` | `100000` | Daily tokens for one user or MCP token on one database (at most the database's). |
| `ask.max_tokens_per_question` | `40000` | Most tokens one question may use across its model calls (4000-200000). |
| `ask.retention_days` | `30` | Days a conversation is kept after its last question (1-3650). |
| `ask.max_seconds_per_question` | `60` | Wall clock of one question (15-300); each model call is still bounded at 15 s. The ask API route gets this plus 5 s. |
| `ask.max_concurrent_runs` | `4` | Asynchronous runs at once per database on one sidecar (1-16); one more is 429. |

All keys are read when a database's runtime starts (restart to change). Without an LLM,
Ask Sage answers `llm_unavailable`.

## API

All routes use the session; every signed-in role may ask.

| Method | Path | |
|---|---|---|
| `POST` | `/api/v1/databases/{db}/ask` | `{"question": "...", "conversation_id": "...", "read_only": true}` (both optional) → the answer |
| `POST` | `/api/v1/databases/{db}/ask/runs` | the same body → `202` `{"run_id", "conversation_id"}` |
| `GET` | `/api/v1/databases/{db}/ask/runs/{id}` | the run's state, progress and, once stored, its answer |
| `POST` | `/api/v1/databases/{db}/ask/runs/{id}/cancel` | cancels the run → the run |
| `GET` | `/api/v1/databases/{db}/ask/conversations` | the caller's conversations, newest first |
| `GET` | `/api/v1/databases/{db}/ask/conversations/{id}` | one conversation with its answers |
| `GET` | `/api/v1/databases/{db}/ask/budget` | today's usage of the database and the caller |

Errors are `{"error", "code"}`: `invalid_request` (400), `not_found` (404; another user's
conversation or run is also "not found"), `ask_conflict` (409: a run is active on the
conversation, or the run already ended), `ask_busy` (429 with `Retry-After`: too many runs),
`ask_disabled` and `ask_unavailable` (503). With `api.database_grants` on, a database outside
the caller's grants is `404` like an unknown one, and nothing is asked or stored
([Per-database Authorization](security.md#mcp-tokens-and-ask-sage)).

## MCP

`ask_sage` takes `database`, `question`, an optional `conversation_id` and an optional
`read_only` (answer only, whatever the token's scopes), and needs the `read` scope. See
[MCP for Coding Agents](mcp.md).
