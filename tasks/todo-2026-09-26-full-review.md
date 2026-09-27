# 2026-09-26 Full review + AI SRE spec + bug fixes

Branch `claude/full-review-ai-sre-2026-09-26`. Output dir `reviews/2026-09-26/`.

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

## Roadmap §5.1 #3 — the standing gate is the only policy authority (G4-I01, G4-I03)

Problem: execution is gate-only (every production executor installs the gate before
bootstrap, fail-closed), but the legacy `EvaluateActionPolicy` engine still produces the
policy verdicts shown to operators: proposal metadata, approval readiness, the cases API,
fleet action-family readiness, and two nil-gate fallbacks. The two engines disagree: the
legacy one has no change classes, policy windows, or usage limits, and the gate has no
provider-support check. So the UI can show "execute" for an action the gate will queue.

- [x] A. Gate completeness: `ActionContract.ProviderSupport` + `RuntimeState.Provider`;
      the gate blocks unsupported providers (`provider_unsupported`). Tests first.
- [x] B. `Gate.Explain(ctx, req)`: the same evaluation as Authorize, with no decision
      recording (no ledger rows). `req.ExplainFamily` skips SQL validation for
      action-family readiness, where there is no concrete SQL.
- [x] C. `Executor.ExplainAction`: gate-backed and fail-closed. Proposal metadata and
      both nil-gate fallbacks are converted. e2e pipeline executors now install the
      standing policy, as production does; they previously ran the legacy engine.
- [ ] C2. Approval readiness still uses the legacy engine for its display verdict, and
      `operatorApprovalBlock` is the operator-approval authority. Next: route operator
      approvals through `Authorize` with an `OperatorApproved` request (hard stops,
      provider, change class, windows; no tier or ramp).
- [ ] D. The cases API and fleet capability readiness use the per-database executor's
      gate. Needs a snapshot evaluator (load runtime, document and usage once, evaluate
      ~25 families in memory); per-family Explain costs 2-3 DB reads each, per request.
      The current legacy readiness fakes a satisfied 365-day ramp.
- [ ] E. Delete `EvaluateActionPolicy`, `ActionPolicyContext` and legacy helpers; migrate
      or delete their tests; add a test that no production code path references them.
- [ ] Verify: unit + integration + e2e; lint; record results in MASTER-SPEC §10.7.
