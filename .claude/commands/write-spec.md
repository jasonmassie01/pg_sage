Write a spec for pg_sage version $ARGUMENTS. Follow this exact process:

## Phase 1: Deep Research (4 parallel agents)

Spawn 4 research agents in parallel. Each writes findings to `research/v{VERSION}_*.md`:

1. **Reddit/Community agent**: Search Reddit (r/postgresql, r/devops, r/database), StackOverflow, HN for real user pain points related to the feature area. Find problems to solve. Write to `research/v{VERSION}_reddit_community.md`.

2. **Competitive analysis agent**: Research every competing product in this space. Analyze features, pricing, architecture, gaps. Write to `research/v{VERSION}_competitive_analysis.md`.

3. **Technical deep dive agent**: Research PostgreSQL internals relevant to the feature. Lock levels, catalog views, extension APIs, version-specific behavior (PG12-PG18). Write to `research/v{VERSION}_technical_deep_dive.md`.

4. **Incidents/postmortems agent**: Find documented real-world incidents, outages, and postmortems related to the feature area. Extract prevention patterns. Write to `research/v{VERSION}_incidents_postmortems.md`.

Each agent should go DEEP -- 500+ lines of findings with specific examples, SQL queries, and citations.

## Phase 2: Write Spec

Synthesize all 4 research documents into a single spec at `specs/v{VERSION}-{feature-slug}.md`. The spec must include:

- Thesis (why this matters, what differentiator it creates)
- Feature summary table (priority, effort, new packages)
- For each feature: problem statement, architecture, detection method, Go struct definitions, SQL queries, config schema, REST API endpoints, dashboard UI description
- Operational constraints (standby behavior, partitioned tables, managed services, pg_stat_statements dependency)
- Schema CHECK constraint updates
- REST API additions with response schemas
- Dashboard UI changes
- Package structure
- Config additions (full YAML)
- Implementation order (phased, with week estimates)
- Testing strategy
- Success criteria
- Appendices (competitive moat, research sources)

Target: 1000+ lines. Be specific enough that two engineers would implement the same thing.

## Phase 3: Self-Review

Review the spec for:
- Ambiguity (under-specified behavior where two engineers would disagree)
- Missing edge cases (production scenarios that would break)
- Contradictions (same thing defined differently in two places)
- Under-specified interfaces (API endpoints without response shapes)
- Dependency risks (features that depend on unverified infrastructure)
- Version-specific behavior (PG12 vs PG15 differences)
- Implementation gaps (missing tables, missing dedup logic, etc.)

Write review to `research/v{VERSION}-spec-review.md` with priority ratings (P0/P1/P2).
Integrate ALL P0 and P1 amendments inline into the spec.

## Phase 4: External LLM Review

Send the amended spec to Gemini CLI for external feedback:
```
cat specs/v{VERSION}-*.md | gemini -m gemini-3.1-pro-preview -p "Review this PostgreSQL tool spec critically..."
```

Save Gemini feedback to `research/v{VERSION}_gemini_review.md`.
Integrate actionable feedback into the spec.

## Phase 5: Finalize

Update the spec status line to reflect all reviews completed.
Report summary to user.
