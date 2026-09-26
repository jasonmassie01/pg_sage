# 2026-09-26 Full review + AI SRE spec + bug fixes

Branch `claude/full-review-ai-sre-2026-09-26`. Output dir `docs/reviews/2026-09-26/`.

- [x] Worktree + baseline build/vet/staticcheck/deadcode
- [x] Disposable CI-equivalent PG17 (+hypopg, pg_stat_statements) on 127.0.0.1:55499
- [ ] Baseline unit + integration tests against test DB
- [ ] G1–G10 feature-group reviews (bugs, dead/unwired, improvements, questions)
- [ ] AI SRE research: market, prior art, internal substrate
- [ ] Verify every P0/P1 claim from agents myself before it enters the master spec
- [ ] Write master findings doc (`MASTER-SPEC.md`) incl. AI SRE spec
- [ ] Locate Codex findings file; integrate (accept / reject with reason)
- [ ] Fix bugs: phase 1 write failing tests, phase 2 fix; one commit per logical fix
- [ ] Full test run (unit + integration + race on touched pkgs + web lint/build) + report
- [ ] Update lessons / memory
