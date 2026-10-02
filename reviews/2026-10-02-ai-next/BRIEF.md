# AI-next feature review — shared brief (2026-10-02)

The user (pg_sage's creator, a 25-year database engineer) asked: "Review all existing features to see
if we can make them better, iterate on them, expand on them. How can we take it to the next level?
Think automated, AI first. AI driven."

Product principle (fixed): pg_sage is an **AI DBA**. LLM features are the product and on by default.
It **earns trust, then becomes autonomous**: every action goes through `policy.Gate` /
`Executor.Apply`, is evidence-backed, verified afterwards and reversible where possible. Irreversible
actions stay ≤ L1; humans approve promotions. "AI first" means the model drives the loop
(observe → hypothesize → act → verify → learn) inside those guardrails, not a chat box bolted on.

Code: `C:/Users/jmass/pg_sage-review` (master @ v1.8.1). Go sidecar in `sidecar/`, React UI in
`sidecar/web`, docs in `docs/`, specs/reviews in `reviews/` and `specs/`. Read-only: do NOT edit code,
run git commands that change state, push, or touch Docker containers/databases.

Real-world evidence (dogfood on the user's 18 GB "lifeos" database, v1.8.0 → v1.8.1, today):
- It autonomously dropped 19 unused/duplicate/invalid indexes (all with months of zero scans),
  set maintenance_work_mem 512MB, tuned autovacuum — all verified, none regressed.
- It also: overloaded the DB with its own forecaster query (12k sequences × 2k snapshots); couldn't
  collect a 50k-index catalog in 500 ms; kept 143 stale incidents open; fought the app's migrations by
  re-dropping an index the app recreates (8× in June); stored 9.3 GB of its own snapshots; flagged
  160 leaked `test_*` schemas only as thousands of duplicate-index recommendations.
- LLM: OpenAI gpt-6-luna works (cheap); promotions above L1 need 3 accepted reviews in a 4 h window
  — the user tried to "promote" but nothing was proposable yet (review/approval UX is not obvious).

## Your output
Write ONE markdown file to `C:/Users/jmass/pg_sage-review/reviews/2026-10-02-ai-next/<your-area>.md`
(≤ 3,000 words), with these sections:
1. **Inventory** — table: feature | what it does today | where (package/file) | LLM used? how |
   maturity (prototype / solid / production) | evidence it works (tests, bench, dogfood).
2. **What is weak** — concrete problems (bugs, gaps, dead or unwired code, UX friction, features that
   collect data nobody uses, deterministic logic that is brittle where a model would generalize).
   Cite `file:line`. Verify every claim in the code; mark anything inferred as "inferred".
3. **Iterate** — improvements to existing features (small/medium), each with value, effort (S/M/L),
   risk, and the guardrail it needs.
4. **Expand** — new capabilities in your area.
5. **AI-first redesign** — how this area would work if the model drove it end to end inside the trust
   guardrails: what the model decides, what tools/probes it needs (MCP/tool-calling), how its output
   is validated, how it is evaluated (PGIncidentBench-style, replay, shadow mode, outcome ledger), how
   it learns from outcomes, and cost control. Be specific to pg_sage's architecture.
6. **Top 5** — your five highest-leverage recommendations, ranked, one paragraph each.

Be concrete and honest; no marketing language. Return a 10-line summary when done.
