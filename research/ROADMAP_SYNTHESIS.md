# pg_sage Feature Roadmap — Research Synthesis

> Compiled: 2026-04-12
> Sources: 4 parallel research agents covering competitive landscape, user pain points, LLM automation opportunities, and ROI/market data

---

## The Thesis

pg_sage is the only open-source tool that monitors, analyzes, AND executes database optimizations with trust-ramped safety. The closest competitor (OtterTune, CMU-backed, $12M raised) shut down in 2024 because config tuning alone wasn't enough value. PostgresAI is the nearest threat at $128-512/month SaaS. pg_sage's advantage is breadth, autonomy, transparency, and price ($5-15/month in LLM tokens vs $149-512/month for competitors).

The ROI math is overwhelming: 2,400% ROI on a single database, break-even at 2% CPU savings on a db.r6g.2xlarge. "Costs less per month than one hour of the compute it saves."

PostgreSQL adoption hit 55.6% in 2025 (#1 database, 3 consecutive years). The TAM is growing fast.

---

## Prioritized Feature Roadmap

### v0.9 — "Root Cause & Prevention" (Next Release)

The theme: move from "pg_sage fixes things" to "pg_sage prevents outages and explains why."

| # | Feature | Pain Point Rank | Impact | Effort | LLM Role |
|---|---------|----------------|--------|--------|----------|
| 1 | **Root Cause Analysis** | #2 (Query Diagnosis) + #5 (Monitoring Gaps) | Very High | 3-4 weeks | Enhances |
| 2 | **Lock Chain Detection & Resolution** | #6 (Long-Running Txns) + #7 (DDL Locking) | High | 1-2 weeks | Enhances |
| 3 | **Storage Growth Forecasting** | #8 (WAL/Disk Space) | High | 1 week | Optional |
| 4 | **Runaway Query Termination** | #4 (Connection Exhaustion) | High | 1 week | Optional |
| 5 | **Natural Language EXPLAIN** | #2 (Query Diagnosis) | High | 2 weeks | Required |

**Why these first**: Root cause analysis is the #1 gap in every existing tool. 47 alerts firing and none saying why — that's the quote that sells this feature. Lock chain resolution prevents cascading failures. Storage forecasting is a quick win with high visibility. Natural language EXPLAIN democratizes performance knowledge to developers who don't read query plans.

**Competitive moat**: No PostgreSQL-specific tool does automated root cause analysis. Generic AIOps tools lack database context. This is a genuine differentiator.

**The pitch**: "During an incident, pg_sage doesn't just tell you the database is slow. It tells you WHY: 'Performance degraded at 14:32. An idle-in-transaction session (PID 12345) has been holding an ACCESS SHARE lock on orders for 47 minutes, blocking autovacuum. 612M dead tuples have accumulated. Recommended action: terminate PID 12345 and run VACUUM on orders.'"

---

### v0.10 — "Schema Intelligence" (Following Release)

The theme: move from runtime optimization to structural optimization.

| # | Feature | Pain Point Rank | Impact | Effort | LLM Role |
|---|---------|----------------|--------|--------|----------|
| 6 | **Safe Migration Planning** | #7 (DDL Locking) | Very High | 2-3 weeks | Enhances |
| 7 | **Schema Anti-Pattern Detection** | Related to #3 (Index) + #11 (Config) | High | 2 weeks | Enhances |
| 8 | **N+1 Query Detection** | #2 (Query Diagnosis) | High | 3-4 weeks | Required |
| 9 | **Materialized View Recommendations** | #2 (Query Diagnosis) | High | 2-3 weeks | Enhances |

**Why these**: Safe migration planning prevents the single most common preventable outage class. A $4.5M migration mistake was documented in 2025. Schema anti-patterns cause compounding performance problems. N+1 detection is the developer-facing killer feature — nobody does this well for PostgreSQL.

**The pitch**: "Before you run `ALTER TABLE orders ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`, pg_sage tells you: 'This will acquire ACCESS EXCLUSIVE lock on a 500M-row table. Estimated lock duration: 45 minutes. Safe alternative: ADD COLUMN without DEFAULT, then backfill in batches of 10,000.' Zero surprises. Zero 3am pages."

---

### v0.11 — "Workload Intelligence" 

The theme: understand the workload holistically, not just individual queries.

| # | Feature | Pain Point Rank | Impact | Effort | LLM Role |
|---|---------|----------------|--------|--------|----------|
| 10 | **"What Changed?" Analysis** | #5 (Monitoring Gaps) | High | 2-3 weeks | Enhances |
| 11 | **Batch vs OLTP Separation Advice** | Related to #2 (Query Perf) | High | 2 weeks | Enhances |
| 12 | **Connection Pool Health Analysis** | #4 (Connection Exhaustion) | High | 2 weeks | Enhances |
| 13 | **Overprivileged Role Detection** | Security concern | High | 1 week | Enhances |
| 14 | **Time-of-Day Maintenance Windows** | #1 (Vacuum/Bloat) | Medium | 1 week | Optional |

**Why these**: "What changed?" is the first question every DBA asks during an incident. Workload separation is a genuine differentiator — no PostgreSQL tool classifies OLTP vs analytical queries automatically. Connection pool analysis addresses the #4 pain point. Role detection addresses compliance requirements (SOC2, PCI-DSS).

---

### v1.0 — "Cost Intelligence" (The Revenue Story)

The theme: quantify the value pg_sage delivers. Make the ROI undeniable.

| # | Feature | Impact | Effort | LLM Role |
|---|---------|--------|--------|----------|
| 15 | **Cost Savings Dashboard** | Very High | 3-4 weeks | Enhances |
| 16 | **Instance Right-Sizing Recommendations** | Very High | 4-6 weeks | Enhances |
| 17 | **Cloud Cost Projection** | Very High | 4-6 weeks | Enhances |
| 18 | **PII Detection** | High | 3-4 weeks | Required |
| 19 | **Pre-Deployment Index Suggestion (CI/CD)** | High | 2-3 weeks | Optional |

**Why here**: This is where pg_sage becomes a product someone pays for. The cost dashboard shows: "pg_sage saved you $4,368 this quarter by optimizing 47 queries and enabling a downsize from db.r6g.2xlarge to db.r6g.xlarge." That's the screenshot that goes in the CFO email.

**The pitch**: "pg_sage costs $15/month in LLM tokens. Last month it saved $554 in compute. That's 37:1 ROI. Here's the receipt."

---

## Features NOT on the Roadmap (and Why)

| Feature | Why Not |
|---------|---------|
| MCP Server | Just removed it. Revisit when MCP adoption stabilizes. |
| Replication failover | Too risky for an autonomous agent. Recommend Patroni instead. |
| Major version upgrade automation | High risk, low frequency. Better as advisory-only. |
| Full query rewriting (subquery flattening, CTE rewriting) | High complexity, needs more LLM reliability research. Tier 3. |
| Audit log analysis (pgaudit) | Niche — requires pgaudit to be enabled. Opportunistic add. |
| Column type optimization | Low urgency, low impact relative to effort. |

---

## Competitive Gaps to Close (Urgent)

These are features competitors have that pg_sage should match:

| Gap | Who Has It | Priority |
|-----|-----------|----------|
| Natural language EXPLAIN | pgMustard (basic), no one does it well | v0.9 |
| Migration safety analysis | Squawk, dryrun (static only) | v0.10 |
| Cost quantification | Revefi (Snowflake only) | v1.0 |
| Query plan pinning | AlloyDB, Azure SQL | v1.x |

---

## Competitive Moats to Build (Defensible)

These are features nobody does well that pg_sage can own:

| Moat | Why It's Defensible |
|------|-------------------|
| **N+1 detection at pg_stat_statements level** | Requires temporal query correlation + LLM understanding of query relationships |
| **Root cause analysis with PostgreSQL-specific signals** | Needs deep catalog knowledge + LLM reasoning across 10+ signal types |
| **Autonomous ANALYZE with fleet-wide concurrency control** | Already built in v0.8.5 — no competitor has this |
| **Trust-ramped execution** | Unique safety model. Oracle is all-auto. Everyone else is recommendation-only. |
| **Denormalization recommendations from query patterns** | Requires correlating pg_stat_statements with schema structure + LLM |
| **Hint lifecycle management** | Already built in v0.8.5 — no competitor manages pg_hint_plan autonomously |

---

## Key Metrics for the Landing Page

From ROI research:

| Metric | Value | Source |
|--------|-------|-------|
| Cloud budget waste | 32% | Flexera 2025 |
| Typical CPU reduction from query optimization | 25-67% | Multiple case studies |
| Break-even threshold | 2% CPU savings | ROI model |
| ROI per database | 2,400% | Single db.r6g.2xlarge |
| pg_sage monthly cost | $5-15 | LLM token spend |
| Instance downsize savings | $379-554/month | RDS pricing |
| DBA-to-database ratio | 1:40 | Industry average |
| DBA fully loaded cost | $120-150K/year | PayScale + overhead |
| pg_sage fleet cost (40 DBs) | $600/year | LLM tokens |
| PostgreSQL market share | 55.6% | StackOverflow 2025 |
| DB performance monitoring market | $3.5B by 2033 | Industry reports |

**Killer one-liners**:
- "pg_sage costs less per month than one hour of the compute it saves."
- "Your database is 30% overprovisioned. pg_sage fixes that."
- "pg_sage pays for itself with the first index it creates."
- "A senior DBA costs $150K/year. pg_sage watches 40 databases for $600/year."

---

## Sources

Full research documents:
- [Competitive Landscape](competitive_landscape.md) — 640 lines, 28 sources
- [User Pain Points](user_pain_points.md) — 336 lines, 20+ sources
- [LLM Automation Opportunities](llm_automation_opportunities.md) — 620 lines, 30 features rated
- [ROI & Market Data](roi_and_market.md) — 313 lines, 30+ sources
