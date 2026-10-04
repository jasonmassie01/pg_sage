# Ask Sage

Ask Sage answers questions about one monitored database in plain language, from evidence
it reads. It runs on the same bounded tool loop as the Sage SRE investigator and uses the
same grounding rule, so an answer is only as strong as the evidence it cites.

Ask it in the **Ask Sage** page of the web UI (per database), through the REST API, or from a
coding agent with the MCP tool `ask_sage`.

## What an answer contains

- **Statements**, each citing the evidence it rests on (`finding:42`, `action:7`,
  `trust:orders`, `table:public.orders`, ...). Every number in a statement must appear in
  the evidence it cites. The UI links each citation to the finding, action, Trust page,
  facts or investigation it came from, and shows the digest of exactly what the model read.
- **Could not verify**: what Ask Sage looked for and did not find. "Not observed" is a
  valid answer. These notes may not contain numbers (a number is a claim and needs a
  citation).
- **Dropped**: statements the citation filter removed, and why (`uncited`,
  `unknown_evidence`, `ungrounded`, ...). They are never part of the answer.
- **Actions**: an investigation Ask Sage opened or a proposal it queued, if any.
- A **status**: `answered`, `not_observed`, `budget_exhausted`, `llm_unavailable` or
  `incomplete` (the run was cut short by a provider error, a timeout or its own bounds).

## What it reads

Read-only tools only: findings and their recommended SQL and rollback, fixes waiting for
approval, executed actions with their verification outcome (predicted versus observed),
facts, incidents, investigations, the trust ledger, one table's shape (`describe_table`,
including its comment), the heaviest statements (query text with literals and comments
removed), configuration keys (never secrets or values that may carry a credential) and
short explanations of pg_sage concepts.

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
  table, a window), citing evidence it read in the same turn. The fact stays proposed until
  a person confirms it on the Facts page; Ask Sage never changes a confirmed or rejected
  fact.
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

All keys are read when a database's runtime starts (restart to change). Without an LLM,
Ask Sage answers `llm_unavailable`.

## API

All routes use the session; every signed-in role may ask.

| Method | Path | |
|---|---|---|
| `POST` | `/api/v1/databases/{db}/ask` | `{"question": "...", "conversation_id": "..."}` → the answer |
| `GET` | `/api/v1/databases/{db}/ask/conversations` | the caller's conversations, newest first |
| `GET` | `/api/v1/databases/{db}/ask/conversations/{id}` | one conversation with its answers |
| `GET` | `/api/v1/databases/{db}/ask/budget` | today's usage of the database and the caller |

Errors are `{"error", "code"}`: `invalid_request` (400), `not_found` (404; another user's
conversation is also "not found"), `ask_disabled` and `ask_unavailable` (503).

## MCP

`ask_sage` takes `database`, `question` and an optional `conversation_id`, and needs the
`read` scope. See [MCP for Coding Agents](mcp.md).
