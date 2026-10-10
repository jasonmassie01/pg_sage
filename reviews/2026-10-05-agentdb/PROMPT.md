# AgentDB review and spec: the enriched prompt (2026-10-05)

Original request: review previous pg_sage specs; look at AgentDB functionality and deployment code
paths; start from first principles (should we do this, did we do it correctly, where should it go);
what databases for agents need; what enterprises deploying databases for agents need; research the
market; find problems that need solutions; account for recent work in the session "pg_sage review
and AI SRE feature spec"; write the spec.

## Goal

A first-principles verdict and a buildable spec for pg_sage's agent-database direction. It covers
both halves of "databases for agents":

1. **Databases that agents use.** Coding agents and app agents create, change and query Postgres.
   What do those databases need: provisioning, branching, isolation, credentials, guardrails,
   memory and vector state, audit, cost, teardown?
2. **Agents that run databases.** pg_sage is an AI DBA and SRE. How does it operate the databases
   that agents create and touch, safely and at scale?

## Questions the spec must answer

1. **Should we do this?** Who is the user? What job do they hire it for? Why Postgres, and why
   pg_sage rather than Neon, Supabase, Tiger Data, Xata, PlanetScale, Prisma, Databricks Lakebase or
   the hyperscalers? What is the opportunity cost against the AI DBA and SRE core (product
   principle: pg_sage earns trust, then becomes autonomous)?
2. **Did we do it correctly?** Audit what exists against what the earlier specs promised, with
   file:line evidence:
   - the `internal/agentdb` domain model, policy, identity and agent tokens, blueprints, cost guard,
     backup assurance, restore drill and monitoring;
   - the deployment paths: provider runners (RDS, Cloud SQL, Lakebase, hosted, local), Terraform,
     deploy requests and promotion, lifecycle and teardown, credentials;
   - the API, MCP and UI surface, and the tests.
   Name bugs, security gaps, dead weight and wrong abstractions.
3. **Where should it go?** Keep, narrow, pivot or kill each part. What do we build next, in what
   order, behind what gates?
4. **What do databases for agents need?** Ground this in Postgres primitives, MCP and the
   copy-on-write branching landscape.
5. **What do enterprises deploying databases for agents need?** Identity for non-human actors,
   least privilege, approvals, audit and compliance, cost attribution, blast-radius limits, kill
   switches, data protection.
6. **Which unsolved problems are worth solving?** Use incidents, community pain and competitor
   gaps as evidence.

## Inputs

- Prior specs, plans, reports and decisions: `docs/superpowers/specs|plans/*agentdb*`,
  `docs/agent-db-deployments.md`, `docs/reverse_spec/05-agentdb.md`, `docs/reports/*agentdb*`,
  `docs/runbooks/agentdb-*`, `research/v2_agentdb_at_scale.md`,
  `research/2026-07-22-agent-native-feature-spec.md`, `specs/agent-native-autonomy-build-spec.md`,
  `reviews/2026-09-26/{group-08-agentdb,fixes-agentdb,AI-SRE-SPEC,MASTER-SPEC}.md`,
  `reviews/decisions/D4-agentdb-tenancy-and-registration.md`,
  `reviews/2026-10-02-ai-next/ROADMAP.md`.
- Code: `sidecar/internal/{agentdb,mcp,mcptoken,ask,agentloop,agenttools,clone,rollout,migration}`,
  their wiring in `sidecar/cmd/pg_sage_sidecar`, API routes and web pages.
- The transcript of the session "pg_sage review and AI SRE feature spec" (session file
  `72f07164-…jsonl`): decisions, corrections and the latest work.
- External research, as of 2026-10-05, with dated sources.

## Process (the repo's `/write-spec`, adapted to the 2026-09-26 AI-SRE spec layout)

1. Research in parallel. Each agent writes its findings to `research/`:
   - internal: prior specs; AgentDB code audit; deployment code-path audit; session history;
   - external: competitive analysis; community pain; incidents and security; technical substrate;
     enterprise needs.
2. Synthesize `AGENTDB-SPEC.md`. It must contain:
   - a verdict, principles, personas and jobs;
   - an audit summary and keep/narrow/pivot/kill decisions;
   - the architecture, data model, API/MCP/UI and config;
   - phased releases with numbered, verifiable CHECKs, and risks.
   Specific enough that two engineers would build the same thing. Every claim about the code is
   verified against the code; every claim about the market is cited.
3. Self-review: a fresh reviewer writes `spec-review.md` (P0/P1/P2); P0 and P1 are folded in.
4. External review with Gemini: `gemini-review.md`; actionable points are folded in.
5. Finalize: the status line records the reviews; commit on branch `claude/agentdb-spec`.

Product principle (decided, not reopened): pg_sage is an AI DBA. LLM features are on by default,
and it earns trust, then becomes autonomous. Every action goes through `policy.Gate` and
`Executor.Apply`, is evidence-backed and verified, and is reversible where possible.
