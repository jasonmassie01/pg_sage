# Fix-phase brief (all fix agents)

You own ONE fix area in ONE git worktree (path given in your prompt), on branch
`fix/2026-09-26-<area>`. Findings live in
`C:/Users/jmass/pg_sage-claude-review/reviews/2026-09-26/` (group-NN files + `codex/`).

## Process (mandatory, from the user's CLAUDE.md)
1. **Read before changing.** Re-verify each assigned bug in current code. If a finding is
   wrong, do not "fix" it — record `NOT A BUG` with the reason.
2. **Phase 1 — tests first.** For each bug, write a regression test that FAILS on current
   code and asserts the specific value/state/side-effect (never only `err == nil`).
   Commit tests first: `test(<scope>): add failing regression tests for <IDs>`.
   Run them and confirm they fail for the right reason.
3. **Phase 2 — fix.** Minimal, root-cause fix. One logical change per commit:
   `fix(<scope>): <imperative description> (<IDs>)`. Never weaken a test to pass; if a test
   was wrong, explain in the commit message.
4. **Verify.** `go build ./...`, `go vet ./...`, `go test -count=1 -cover` for every touched
   package with `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`
   (a disposable PG17 with hypopg + pg_stat_statements; the fixture creates a private
   database per test package, so parallel agents are safe). Also run with `-tags=integration`
   for touched packages. grep output for SKIP. Web changes: `npm ci` (from lockfile) then
   `npm run lint && npm run test -- --run && npm run build` in `sidecar/web`.
   If you change web source, rebuild `sidecar/internal/api/dist` via `npm run build`.
5. **Report.** Write `reviews/2026-09-26/fixes-<area>.md` in YOUR worktree:
   table `ID | status (FIXED / NOT A BUG / DEFERRED) | commit | test name | notes`,
   plus the CLAUDE.md "Test Results" block (command, pass/fail/skip counts, per-package
   coverage, skipped tests, bugs found). Commit it as `docs(review): record <area> fixes`.

## Rules
- Hard limits: functions ≤ 50 lines, files ≤ 500 lines (split files if you grow one past it),
  lines ≤ 100 chars, parameterized SQL, no swallowed errors, no TODO without issue.
- Stay inside your owned packages. If a fix truly needs a change elsewhere (e.g. one line of
  wiring in `cmd/pg_sage_sidecar/main.go`), keep it minimal and list it under
  "Cross-area edits" in your report — other agents work in parallel and the lead merges.
- Do not push, do not open PRs, do not touch Docker/containers, do not run e2e or live cloud
  tests, do not modify `sage` data in any database other than the fixture.
- Prefer failing CLOSED on safety paths. When a "fix" would be a product decision (e.g.
  delete a feature vs. finish it), choose the safer minimal option and note it.
- Dead code: delete only items marked DELETE in the group files for your packages, in a
  separate commit `refactor(<scope>): remove unreachable <what>`.
- Commit trailer on every commit: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`
- Priority: every P0, then every P1, then P2s that are cheap and clearly correct. Record
  anything not done as DEFERRED with a reason. Security issues: describe impact + fix only.
