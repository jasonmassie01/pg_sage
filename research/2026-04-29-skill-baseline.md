# Skill Baseline Research Process: pg_sage

## Purpose

This artifact defines the process I would follow when asked to deeply review pg_sage and recommend what to add next. It is meant to be reusable as a future skill baseline, not a completed research report.

## Operating Rules

- Do not modify source code during research.
- Separate evidence from interpretation.
- Prefer primary sources: pg_sage docs/code, PostgreSQL docs, pgvector docs, competitor docs, benchmark posts with reproducible details, issue trackers, and user/forum threads.
- Record every source with URL, date accessed, claim extracted, and confidence.
- Treat product ideas as hypotheses until tied to user pain, technical feasibility, and measurable value.
- Include uncomfortable questions the user did not ask.

## Research Workflow

1. Establish current product shape.
   - Read README, docs, examples, config, tests, architecture notes, CLI/API surfaces, and recent commits.
   - Summarize pg_sage in one paragraph: target user, current promise, strongest capability, weakest boundary.
   - Map core workflows: install, connect database, observe workload, generate advice, execute actions, verify outcomes.

2. Build a capability inventory.
   - Extract features into categories: analysis, recommendations, execution, safety, fleet mode, LLM use, UI/API, observability, and tests.
   - Identify missing states: dry-run, rollback, confidence calibration, Postgres version handling, extension absence, permission limits, hosted Postgres constraints.

3. Research user demand.
   - Search forums and communities: Reddit PostgreSQL, Hacker News, pgsql mailing lists, Stack Overflow, GitHub issues, Supabase/Neon/RDS discussions, Discord/Slack archives if available.
   - Capture recurring pain around slow queries, unused indexes, vector search, vacuum/autovacuum, bloat, lock safety, multi-tenant DBs, and "what should I do next?" tooling.
   - Distinguish expert DBA needs from developer/operator needs.

4. Research competitors and adjacent tools.
   - Compare against pganalyze, pgMustard, OtterTune, Tembo, Supabase advisors, Neon observability, RDS Performance Insights, pg_stat_monitor, HypoPG workflows, pgBadger, explain visualizers, and AI database copilots.
   - For each: audience, core loop, automation level, safety model, pricing signal, blind spots, and where pg_sage can be meaningfully different.

5. Deep dive vector search.
   - Study pgvector IVFFlat and HNSW behavior, build-time tradeoffs, memory parameters, recall/latency tradeoffs, filter selectivity, hybrid search, reindexing, and embedding-dimension effects.
   - Research real production problems: index choice, `ef_search`, `m`, `ef_construction`, maintenance memory, concurrent builds, replica lag, approximate recall measurement, tenant-specific tuning.
   - Identify advisor opportunities: benchmark harness, HNSW parameter autoresearch, workload-aware recall tests, index health checks, and hybrid search plan warnings.

6. Explore agent-created databases.
   - Consider agents that generate schemas, load seed data, create indexes, run experiments, mutate query workloads, and compare outcomes.
   - Define guardrails: disposable databases only, bounded cost, no production credentials, deterministic fixtures, explicit teardown, audit logs.
   - Evaluate whether pg_sage should create databases for learning, benchmarking, regression tests, or user-specific advisor simulations.

7. Ask missing strategic questions.
   - Is pg_sage an advisor, autonomous operator, benchmark lab, observability product, or developer copilot?
   - Who trusts it enough to execute DDL?
   - What is the smallest recommendation that creates repeated value?
   - What recommendations are dangerous without workload context?
   - What hosted Postgres constraints make automation impossible?
   - What data must never leave the user environment?
   - How will pg_sage prove that advice improved real performance?

## Output Artifact Structure

1. Executive Recommendation
   - Top 5 additions, ranked by user value, feasibility, risk, and differentiation.

2. Evidence Map
   - Source table: source, claim, quote or paraphrase, confidence, relevance.

3. Current-State Assessment
   - Product summary, workflow map, strengths, gaps, architectural constraints.

4. Market and Community Signals
   - Recurring forum pain, frequency estimate, representative examples, underserved users.

5. Competitor Matrix
   - Tool, audience, capabilities, automation level, safety model, gaps, pg_sage opportunity.

6. Vector Search and HNSW Research
   - Tuning variables, known failure modes, benchmark design, proposed advisor features.

7. Agent-Created Database Opportunities
   - Use cases, safety boundaries, lifecycle, cost controls, proposed experiments.

8. Recommendation Backlog
   - Each item includes problem, proposed feature, evidence, implementation sketch, tests, risks, and success metric.

9. Questions Not Asked
   - Strategic, security, UX, trust, pricing, deployment, and data-boundary questions.

10. Research Gaps
   - Sources not yet checked, claims needing validation, benchmark work still required.
