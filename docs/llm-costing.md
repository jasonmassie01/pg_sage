# LLM Costing Guide

pg_sage uses LLM calls for five features. Each has independent controls to
manage daily token spend. This guide covers token estimates, pricing, and
recommended budgets.

## Defaults

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
its own budget). The default `0` derives it: `llm.fleet_token_budget_daily` when that
is set, else the sum of every client's `token_budget_daily`, and unlimited when any
client is unlimited. The derived value is today's effective total, so the default
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

## Token-Consuming Features

| Feature | Default Interval | Avg Tokens/Call | Calls/Day (default) | Daily Estimate |
|---------|-----------------|-----------------|---------------------|----------------|
| Briefing | `0 6 * * *` (daily) | 2,000-4,000 | 1 | ~3,000 |
| Advisor | 3,600s (1h) | 1,500-3,000 | 24 | ~48,000 |
| Tuning agent | per analyzer cycle, only when a case exists | 3,000-8,000 per request | at most `tuning.max_requests_per_cycle` (12) per cycle | bounded by `tuning.max_tokens_per_cycle` (60,000) per cycle |

**Typical daily total (all features, small-medium DB): ~150,000 tokens**

## Estimates by Database Size

| DB Size | Tables | Queries Tracked | Est. Daily Tokens |
|---------|--------|-----------------|-------------------|
| Small (<10 GB, <50 tables) | <50 | <200 | 80,000-120,000 |
| Medium (10-100 GB) | 50-500 | 200-1,000 | 120,000-250,000 |
| Large (100+ GB) | 500+ | 1,000+ | 250,000-500,000 |

## Dual-Tier Model Configuration

pg_sage supports two LLM tiers to balance cost and quality:

### General LLM (`llm.*`)
Used by: briefing, advisor, diagnose.
These features need good summarization but not deep SQL reasoning.
**Recommended:** Gemini 2.5 Flash, Claude Haiku, GPT-4o-mini

### Optimizer LLM (`llm.optimizer_llm.*`)
Used by: the tuning agent (index, configuration, statistics and hint proposals).
These features analyze query plans and generate SQL — accuracy matters.
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

1. **Increase advisor interval** to 7,200s (2h) instead of 3,600s — saves ~50% advisor tokens
2. **Keep briefing daily** (default) vs hourly — saves ~23x briefing tokens
3. **Lower `tuning.max_requests_per_cycle` / `tuning.max_tokens_per_cycle`** — the tuning agent's per-database, per-cycle budget
4. **Use Flash/Haiku for general**, Pro/Sonnet only for `optimizer_llm`
5. **Disable unused proposal types** — e.g. `advisor.vacuum_enabled: false` stops storage-parameter proposals
6. **Set `context_budget_tokens`** lower (2048) if your schema is simple
7. **Increase analyzer interval** to 1,200s+ for stable workloads — fewer tuning cycles

## Circuit Breaker

pg_sage has a built-in circuit breaker (`cooldown_seconds`, default 300s) that
pauses LLM calls after 3 consecutive failures. The daily token budget
(`token_budget_daily`) is a hard cap — once exhausted, all LLM features
gracefully degrade to deterministic-only mode until midnight UTC.

## Monitoring Token Usage

- Prometheus metric: `pg_sage_llm_tokens_used_today`
- REST API: `GET /api/v1/metrics` includes `llm_tokens_used_today`
- Logs: each LLM call logs token count at INFO level
