# Enriched assignment: pg_sage completion audit and AI SRE design

Act as a skeptical staff engineer, database reliability engineer, security reviewer,
product designer and researcher. Use the current repository, not historical claims.

1. Establish revision, remote freshness, working-tree state, runtime modes and actual
   feature groups. Read prior decisions without treating old test results as current.
2. Trace every group through trigger, config, startup, dependencies, database identity,
   persistence, API, UI, permissions, failure, restart and visible outcome.
   Tests or exported symbols without production callers do not establish delivery.
3. Find concrete bugs, unreachable paths, inert controls, incomplete integrations,
   misleading success/health/value claims and lifecycle transitions that lose work.
4. Try to disprove serious findings. Cite source and enabling conditions; distinguish
   reproduction, source confirmation, plausible risk and missing evidence.
5. Run uncached tests, coverage, static checks, builds, integration and browser checks
   using disposable targets. Preserve exact failures, skips and fixture limitations.
   Add targeted regression probes that can fail when the existing suite passes.
6. Assess every feature for a useful improvement, first slice, priority, dependencies,
   acceptance criteria, safety boundary and measurable value.
7. Research demand and alternatives using current primary sources. Include Postgres,
   managed providers, AI SRE, other database ecosystems, vector quality and agent DBs.
   Separate vendor claims, independent evidence and product judgment.
8. Specify an AI SRE capability beyond existing RCA: personas, incident examples,
   architecture, data model, API, UX, state machine, budgets, authority, recovery,
   tests, rollout and value. Model reasoning must not grant permissions.
9. Challenge missing questions: what refutes the diagnosis; who owns the action;
   what if pg_sage causes harm; what survives restart; what is actually reversible;
   what proves customer recovery; what would make the product unnecessary?
10. Deliver one complete report, supporting evidence and a buildable specification.
    Clearly distinguish proposed checks from executed verification.

Scope is review, reproduction, research and specification. Preserve original source
and live services. Do not publish, contact others, provision paid cloud resources,
or silently start optimization. Repair proposals must be concrete and reviewable.

