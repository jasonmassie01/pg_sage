# pg_sage Full Review — Enriched Brief (2026-09-26)

Branch: `claude/full-review-ai-sre-2026-09-26` (worktree `C:/Users/jmass/pg_sage-claude-review`),
based on `origin/master` @ `b396595` (v1.5.0).

## Original ask (user)

1. Review every feature group. First: no bugs, no dead code paths, nothing started-but-unwired.
2. Then: how can each existing feature be made better?
3. Then: deep research + spec for a new **AI-driven SRE** feature.
4. Write findings to a file. Integrate Codex's parallel findings where good. Then fix the bugs.

## Enriched ask (meta-prompt — what the user didn't say but a staff engineer would)

- **Ground truth over docs.** `docs/REVERSE_SPEC.md` (2026-06-10) lists "coded-but-unwired" items.
  Many may since be fixed. Every claim must be re-verified against current code with file:line.
- **Objective dead-code signal.** `golang.org/x/tools/cmd/deadcode` output (functions unreachable
  from `main`, i.e. reachable only from tests) is in `raw-deadcode-prod.txt`. Each entry must be
  classified: **WIRE** (feature is valuable, finish it), **DELETE** (superseded/duplicate), or
  **TEST-ONLY OK** (test helper that legitimately lives in a non-_test file).
- **Unwired ≠ unreachable.** Also hunt: config keys with no consumer, config keys the UI exposes
  that do nothing, DB tables written but never read (or read but never written), API endpoints
  with no UI/MCP caller, UI calls to endpoints that don't exist, metrics never emitted,
  goroutines never started, feature flags that are always off, error paths that log and continue
  where they should fail closed, `TODO`/`not implemented`/stub returns.
- **Bug classes to hunt proactively** (known pg_sage failure patterns from CLAUDE.md):
  default-value masking (zero instead of intended default), markdown-wrapped LLM JSON,
  transaction-scope errors (VACUUM/CONCURRENTLY inside tx), fleet-mode leaks (wrong database
  name/context bleeding across instances), confidence-threshold boundaries, emergency-stop and
  trust-gate bypasses, unbounded table growth (no retention), goroutine/ticker/conn leaks,
  races on shared maps, context not propagated/cancelled, SQL string interpolation,
  secrets in logs, hot-reload not applied to running components.
- **Severity model:** P0 = data loss / unsafe mutation of a monitored DB / security hole;
  P1 = feature silently broken or wrong answer shown to user; P2 = degraded behavior, leak,
  or misleading UI; P3 = hygiene.
- **Every bug needs:** file:line, concrete failure scenario (inputs → wrong output), confidence
  (CONFIRMED by reading/executing, or PLAUSIBLE), minimal fix, and the test that would catch it.
- **Improvements must be concrete and ranked** (impact × effort), grounded in the code as-built,
  and should prefer finishing/strengthening over new surface area.
- **Questions the user isn't asking** — surface them per group.
- **AI SRE feature** must build on existing substrate (rca, incidents, cases, logwatch, forecaster,
  querystore, verify, policy, notify, mcp, autonomy) rather than invent parallel systems, must
  respect the trust/reversibility thesis, and must define evaluation (how do we know the AI SRE
  is right?) — not just capabilities.

## Constraints for review agents

- READ-ONLY on source. Only write your findings file under `docs/reviews/2026-09-26/`.
- You may run `go build`, `go vet`, and **unit** `go test -count=1` for your packages.
  Do not run `-tags=integration`/e2e, do not touch local Postgres/Docker, do not start servers.
- Hard limits from CLAUDE.md apply to suggested fixes: funcs ≤50 lines, files ≤500 lines,
  lines ≤100 chars, no bare catch, parameterized SQL.

## Output format for review group files

```
# Group NN — <name>
## Scope (packages/files reviewed, LOC)
## A. Bugs  (table: ID | Sev | Conf | file:line | summary) then one subsection per bug:
   failure scenario, root cause, fix, test-to-add
## B. Dead / unwired / half-built  (table: ID | file:line | what | verdict WIRE/DELETE/TEST-ONLY | why)
## C. Feature improvements (per feature: current state → proposed improvements, ranked I×E)
## D. Questions the user isn't asking
## E. Verification notes (what you ran, what you could not verify)
```
IDs: `<GROUP>-B01` bugs, `<GROUP>-D01` dead code, `<GROUP>-I01` improvements.
