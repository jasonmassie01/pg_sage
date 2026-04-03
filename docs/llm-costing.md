# LLM Costing Guide

pg_sage uses LLM calls for five features. Each has independent controls to
manage daily token spend. This guide covers token estimates, pricing, and
recommended budgets.

## Token-Consuming Features

| Feature | Default Interval | Avg Tokens/Call | Calls/Day (default) | Daily Estimate |
|---------|-----------------|-----------------|---------------------|----------------|
| Briefing | `0 6 * * *` (daily) | 2,000-4,000 | 1 | ~3,000 |
| Advisor | 3,600s (1h) | 1,500-3,000 | 24 | ~48,000 |
| Optimizer | 600s (per analyzer cycle) | 3,000-6,000 | 10-20 | ~60,000 |
| Tuner (LLM mode) | per analyzer cycle | 2,000-4,000 | 10-20 | ~40,000 |
| Diagnose (MCP) | on-demand | 4,000-8,000 | varies | varies |

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
Used by: optimizer (index recommendations), tuner (hint generation).
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
3. **Raise `min_query_calls`** to 100+ for optimizer and tuner — avoids analyzing one-off queries
4. **Use Flash/Haiku for general**, Pro/Sonnet only for `optimizer_llm`
5. **Disable unused advisors** — set `vacuum_enabled: false`, etc. for categories you don't need
6. **Set `context_budget_tokens`** lower (2048) if your schema is simple
7. **Increase analyzer interval** to 1,200s+ for stable workloads — fewer optimizer/tuner cycles

## Circuit Breaker

pg_sage has a built-in circuit breaker (`cooldown_seconds`, default 300s) that
pauses LLM calls after 3 consecutive failures. The daily token budget
(`token_budget_daily`) is a hard cap — once exhausted, all LLM features
gracefully degrade to deterministic-only mode until midnight UTC.

## Monitoring Token Usage

- Prometheus metric: `pg_sage_llm_tokens_used_today`
- REST API: `GET /api/v1/metrics` includes `llm_tokens_used_today`
- Logs: each LLM call logs token count at INFO level
