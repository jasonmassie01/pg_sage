# LLM Costing Guide

pg_sage calls a model from the sixteen features listed under
[Model consumers](#model-consumers). Each has its own switch and its own per-call cap, and
every call is charged to a daily token budget. This guide covers the defaults, what each
feature spends, pricing, and recommended budgets.

## Defaults

| Key | Default | What it bounds |
|-----|---------|----------------|
| `llm.token_budget_daily` | 500,000 | Daily tokens (input + output) of each LLM client |
| `llm.fleet_token_budget_daily` | 0 | Fleet-wide daily tokens split per database (0: off) |
| `llm.global_token_budget_daily` | 0 | Sum of every client's spend (0: derived, see below) |
| `llm.cooldown_seconds` | 300 | Identical-request dedup window and circuit-breaker pause |
| `llm.optimizer_llm.enabled` | false | Adds a dedicated optimizer client per database |
| `llm.optimizer_llm.token_budget_daily` | 500,000 | Daily tokens of each optimizer client |
| `llm.optimizer_llm.cooldown_seconds` | 300 | As `llm.cooldown_seconds`, for the optimizer client |
| `llm.optimizer_llm.fallback_to_general` | true | Tuning retries on the general client when the optimizer fails |
| `briefing.schedule` | `0 6 * * *` | One briefing a day |
| `advisor.interval_seconds` | 86,400 (one day) | Seconds between configuration-advisor cycles |
| `tuning.max_cases_per_cycle` | 3 | Tuning cases worked per analyzer cycle and database |
| `tuning.max_requests_per_cycle` | 12 | Tuning model requests per analyzer cycle and database |
| `tuning.max_tokens_per_cycle` | 60,000 | Tuning tokens per analyzer cycle and database |
| `explain.max_tokens` | 4,096 | Output tokens of one explain answer |
| `ask.daily_tokens_per_database` | 300,000 | Ask Sage's own daily tokens for one database |
| `ask.daily_tokens_per_user` | 100,000 | Ask Sage's daily tokens for one user on one database |
| `ask.max_tokens_per_question` | 40,000 | Tokens one Ask Sage question may use across its calls |
| `rca.llm_correlation_threshold` | 3 | Concurrent signals, some not covered by a rule, before RCA asks the model |

LLM features are on by default (`llm.enabled`, `tuning.enabled`, `llm.optimizer.enabled`,
`advisor.enabled`, `tuner.llm_enabled`, `rca.narration_enabled`, `explain.enabled`)
but spend nothing until `llm.endpoint` and `llm.api_key` are set. Once they are,
`llm.token_budget_daily` (default 500,000 tokens) caps each LLM client's daily spend;
when it is exhausted that client's features fall back to their deterministic path until
the next UTC day. The cap is per client, not per sidecar: the shared client and, in
fleet mode, each database's general client get their own copy, and
`llm.optimizer_llm.enabled` (default `false`) adds one more optimizer client per
database. A default two-database fleet with the optimizer LLM on therefore has five
copies, 2,500,000 tokens a day in all, unless `llm.fleet_token_budget_daily` is set.
To turn a feature off, set its switch to `false`; `llm.enabled: false` turns all of
them off.

### Global ceiling and policy cap

`llm.global_token_budget_daily` caps the sum of every client's spend (Ask Sage keeps
its own budget). The default `0` derives it as the sum of every client's
`token_budget_daily`; in fleet mode with `llm.fleet_token_budget_daily` set, that fleet
total stands in for the per-database clients it scopes, and the shared client keeps its
own budget on top. Any unlimited counted client makes it unlimited. The derived value
is today's effective total, so the default
spends exactly what it did before; set the key to make one true process-wide cap. It
is hot-reloadable, the startup log names the ceiling and how it was derived, and
`GET /api/v1/llm/status` reports it as `global{ceiling, used, reserved, source,
exhausted}` (`source` is `configured`, `fleet` or `derived`).

The standing policy's `budgets.llm_tokens_daily` also binds: each client's daily cap
is the stricter of `token_budget_daily` and the policy value (`null`, no cap, means
the config alone; `0` refuses every call). In meta-DB mode the database's own policy
applies, otherwise the global policy. pg_sage rereads the policy at most once a minute
and keeps the last value, with a warning, when the control database cannot be read.

A call holds its `max_tokens` against each budget while it is in flight. A UTC day turn
or `POST /api/v1/llm/budget/reset` restarts spend at zero but keeps those holds, so a
call that straddles a reset is charged in full to the new day.

Retries do not multiply the charge: a call retried after a 429 or a 5xx (up to 4
attempts, see [provider errors and the circuit
breaker](configuration.md#llm-provider-errors-and-the-circuit-breaker)) is reserved and
charged once, and a timed-out attempt is never re-sent. `POST /api/v1/llm/test` is a real
request of a few tokens, charged like any other.

## Model consumers

Every model call in the code, by feature. "Cap" is the most output tokens one call may
ask for; the prompt comes on top. All of them use a general client except the tuning
agent, which uses the optimizer client when `llm.optimizer_llm.enabled` is `true` (and
the general client otherwise). In fleet mode explain and the agent column classifier
are charged to the shared client; the others to the database's own general client.
Ask Sage is also held to its own `ask.*` budgets.

| Feature | Code | Calls the model | Cap per call |
|---------|------|-----------------|--------------|
| Briefing | `internal/briefing` | once per `briefing.schedule` run (default daily) | 4,096 |
| Configuration advisor | `internal/advisor` | once per `advisor.interval_seconds` (default daily): up to 4 calls, one each for WAL, connections, rewrites and bloat (`advisor.*_enabled`) | 4,096 |
| Tuning agent | `internal/tuning` | each analyzer cycle that has a case (`tuning.enabled`) | bounded per cycle by `tuning.max_requests_per_cycle` and `tuning.max_tokens_per_cycle` |
| Plan-change narrator | `internal/analyzer` | each `plan_regression` finding of an analyzer cycle | 512 |
| Action justification | `internal/executor` | each action the executor applies | 256 |
| RCA narration | `internal/rca` | each incident notification (`rca.narration_enabled`) | 1,024 |
| RCA correlation | `internal/rca` | an RCA cycle with at least `rca.llm_correlation_threshold` signals, some not covered by a rule | 2,048 |
| Explain | `internal/explain` | each explain request (`explain.enabled`) | `explain.max_tokens` (4,096) |
| Ask Sage | `internal/ask`, `internal/agentloop` | each question (`ask.enabled`), a tool loop of several calls | `ask.max_tokens_per_question` (40,000) per question |
| SRE investigations | `internal/sre`, `internal/agentloop` | each investigation in the default `sre.llm.mode` (`investigator`) | each call holds a durable reservation against the day's budget |
| Runbook compiler | `internal/sre/runbook` | each English playbook compiled to a draft | 4,000 |
| Migration review | `internal/migration` | each risky DDL seen (`migration.enabled`): a safer alternative and a script | 2,048 and 4,096 |
| Schema lint (JSONB) | `internal/schema/lint` | each scan with JSONB findings (`schema_lint.enabled`, hourly by default) | 4,096 |
| Facts | `internal/facts` | at most once a day per database, to propose facts from catalog evidence | 2,048 |
| First look | `internal/firstlook` | a database's first-look summary, when its items changed since the last first look (start or re-check) | 400 |
| Agent column classifier | `internal/agentguard/classify` | the daily column classification pass of agent governance | 2,048 |
| Sage Review model pass | `internal/review/modelpass`, `internal/agentloop` | each review with candidates (`review.model_pass`, default `auto`): `review.model_runs` (default 3) tool loops, or 2 with `review.consensus: refute`; the `review` client (`llm.review_llm`, else the optimizer, else the general client) | 6,000 per call, at most 150,000 tokens per run |

The scheduled consumers make a handful of calls a day per database: one briefing, up to
four advisor calls, one facts pass, the classifier's daily pass, and the first look once.
The rest follow events: analyzer cases, incidents, operator questions and DDL. On a busy
database the tuning agent dominates, and the binding cap is the client's
`llm.token_budget_daily`.

## Estimates by Database Size

Rough planning figures per database, not measurements; read your real spend from
`GET /api/v1/llm/status` (see [Monitoring](#monitoring-token-usage)).

| DB Size | Tables | Queries Tracked | Est. Daily Tokens |
|---------|--------|-----------------|-------------------|
| Small (<10 GB, <50 tables) | <50 | <200 | 80,000-120,000 |
| Medium (10-100 GB) | 50-500 | 200-1,000 | 120,000-250,000 |
| Large (100+ GB) | 500+ | 1,000+ | 250,000-500,000 |

## Dual-Tier Model Configuration

pg_sage supports two LLM tiers to balance cost and quality:

### General LLM (`llm.*`)
Used by: every consumer in the table above except the tuning agent, and by the tuning
agent too while `llm.optimizer_llm.enabled` is `false` (the default).
These features need good summarization but not deep SQL reasoning.
**Recommended:** Gemini 2.5 Flash, Claude Haiku, GPT-4o-mini

### Optimizer LLM (`llm.optimizer_llm.*`)
Used by: the tuning agent (`internal/tuning`) only, for all of its proposal types (index,
configuration, statistics and hint). The client serves the `index_optimization` and
`query_tuning` purposes; everything else stays on the general client. With
`fallback_to_general: true` (the default) a failed tuning call is retried on the general
client, and that retry is charged to the general client's budget.
These proposals analyze query plans and generate SQL — accuracy matters.
**Recommended:** Gemini 2.5 Pro, Claude Sonnet, GPT-4o

```yaml
llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  api_key: ${SAGE_LLM_API_KEY}
  model: "gemini-2.5-flash-preview"      # cheap, fast
  token_budget_daily: 500000
  optimizer_llm:
    enabled: true
    model: "gemini-2.5-pro-preview"       # accurate, more expensive
    fallback_to_general: true
```

## Pricing Reference (as of March 2026)

| Provider | Model | Input $/1M | Output $/1M | ~Daily Cost (150K tokens) |
|----------|-------|-----------|-------------|---------------------------|
| Google | Gemini 2.5 Flash | $0.15 | $0.60 | ~$0.06 |
| Google | Gemini 2.5 Pro | $1.25 | $10.00 | ~$0.84 |
| Anthropic | Haiku 3.5 | $0.80 | $4.00 | ~$0.36 |
| Anthropic | Sonnet 4 | $3.00 | $15.00 | ~$1.35 |
| OpenAI | GPT-4o-mini | $0.15 | $0.60 | ~$0.06 |
| OpenAI | GPT-4o | $2.50 | $10.00 | ~$0.94 |

*Prices are approximate. Check provider pricing pages for current rates.*

## Recommended Budgets

| Use Case | `token_budget_daily` | Monthly Est. Cost |
|----------|---------------------|-------------------|
| Cost-conscious (Flash only) | 200,000 | $1-3 |
| Balanced (Flash + Pro optimizer) | 500,000 | $5-15 |
| Full autonomy (large DB) | 1,000,000 | $15-40 |

## Cost Reduction Tips

1. **Keep the advisor daily** (`advisor.interval_seconds: 86400`, the default) — each cycle
   is up to 4 calls; an hourly advisor makes 24 times as many
2. **Keep briefing daily** (default) vs hourly — saves ~23x briefing tokens
3. **Lower `tuning.max_requests_per_cycle` / `tuning.max_tokens_per_cycle`** — the tuning agent's per-database, per-cycle budget
4. **Use Flash/Haiku for general**, Pro/Sonnet only for `optimizer_llm`
5. **Disable unused proposal types** — e.g. `advisor.vacuum_enabled: false` stops storage-parameter proposals
6. **Lower `context_budget_tokens`** (for example 2048) if your schema is simple. It is the
   trim target for attached context: lowering it shrinks advisor, tuning and explain prompts.
   It does not cap answers (the briefing has its own 4096-token cap) or agent conversations
7. **Increase analyzer interval** to 1,200s+ for stable workloads — fewer tuning cycles

## Circuit Breaker and Dedup

`llm.cooldown_seconds` (default 300) does two things on each client. An identical
request (same endpoint, model, key and prompt) is not re-sent while one is in flight or
within that many seconds of its last success. After 3 failures in a row, or one 401/403,
the client's breaker opens and its calls pause that long; then one probe decides.
`llm.optimizer_llm.cooldown_seconds` does the same for the optimizer client (0 inherits
`llm.cooldown_seconds`). The daily token budget (`token_budget_daily`) is a hard cap per
client: once exhausted, that client's features degrade to deterministic-only mode until
midnight UTC.

## Monitoring Token Usage

- Usage ledger: `GET /api/v1/llm/usage?days=30` (viewer; `days` 1-400, default 30) answers
  "what spent the tokens, and what did it cost". Each row is one UTC `day`, database
  (`database_key`, and `database` when the key still names one), `llm_role` (`general` or
  `optimizer`), `purpose` and `model`, with `prompt_tokens`, `completion_tokens`,
  `reasoning_tokens` (the reasoning share of the completion), `calls`, `failures` and
  `est_cost_usd`. Every client adds to it, per database in fleet mode; it is flushed to
  `sage.llm_usage` in the control database every 60 s and on shutdown, and kept 400 days.
  A failed flush keeps the counts for the next one and logs once; a call never fails
  because of it. `source` is `estimate` when the provider reported no usage and pg_sage
  split its own estimate. Tokens a provider charged beyond its prompt/completion split (some
  report thinking tokens only in the total) count as completion, so the ledger never shows
  less than the budget charged.
- Purposes: `advisor`, `agent_guard`, `ask`, `briefing`, `explain`, `facts`, `first_look`,
  `justification`, `migration_advice`, `migration_script`, `plan_narrative`, `probe`
  (the connection test), `query_tuning`, `rca_narration`, `rca_tier2`, `runbook_compile`,
  `schema_lint`, `sre` / `sre_investigation`, and `agent_loop` for an agent conversation
  no feature labelled. `unlabeled` should never appear; `overflow` appears only past
  10,000 distinct keys between two flushes.
- Cost: set `llm.pricing` to your provider's rates, in USD per million tokens:

  ```yaml
  llm:
    pricing:
      gpt-4o-mini: {input_per_m: 0.15, output_per_m: 0.60}
      o3-mini: {input_per_m: 1.10, output_per_m: 4.40, reasoning_per_m: 4.40}
  ```

  The key is the model id exactly as configured. `est_cost_usd` is prompt x input plus
  completion x output (the reasoning share at `reasoning_per_m` when set). pg_sage ships
  no prices and never guesses one: a model without an entry, or a day with calls made
  before its price was set, has a `null` cost. A negative or missing price, or an
  unknown price kind, fails at load; a price change applies at restart.
- REST API: `GET /api/v1/llm/status` lists every client (`clients`, each with
  `tokens_used`, `token_budget`, `budget_exhausted`, `circuit_state` and `resets_at`), the
  process-wide `global` ceiling and `any_exhausted`. `GET /api/v1/metrics` reports fleet
  status, not tokens.
- Fleet budget: with `llm.fleet_token_budget_daily` set, every row of
  `GET /api/v1/databases` carries `llm_budget` (`allocation`, `used`, `weight`), the
  database's share and today's use; the field is omitted without a fleet budget. The
  **Fleet** page shows it as used / allocation.
- Prometheus: every client has its own series, labelled `client` (`general`, `optimizer`)
  and `database` (empty for the shared client and in standalone mode), in every mode.
  `pg_sage_llm_tokens_total{purpose,kind}` counts the tokens charged since start by
  purpose, split into `prompt`, `completion` and `unattributed` (estimated, or not split by
  the provider); costs are in the usage ledger, not on `/metrics`.
  `pg_sage_llm_requests_total{result}` counts calls by result (`ok`, `error`, `timeout`,
  `rate_limited`, `budget_refused`, `circuit_open`, `empty`, `cancelled`), and
  `pg_sage_llm_request_duration_seconds` is their latency. `pg_sage_llm_tokens_used_today`,
  `pg_sage_llm_tokens_budget_daily` (the live budget) and `pg_sage_llm_circuit_open` are
  per client too. See [deployment](deployment.md#llm-health).
- Logs: there is no per-call token log line; the usage ledger is the per-call record. The
  client logs when its circuit breaker opens, with the last error class and the pause.
- Restarts: today's usage is kept in `sage.llm_usage` in the control database and restored
  at startup; `GET /api/v1/llm/status` reports `usage_restored_at` and `ledger_last_flush_at`
