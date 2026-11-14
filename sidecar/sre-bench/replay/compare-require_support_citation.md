# Replay compare: require_support_citation (fake model)

sre-03b (sre-03 Phase 2). The 73-case corpus in `cases/` replayed on PostgreSQL 17 (local
ephemeral server) with `sre.investigator.require_support_citation` off (Phase 1) and on, on
the same commit, through the causal graph, the review turn and the tool-calling investigator
arms (fake models). Generated 2026-11-14T01:11:17Z by

    go test ./sre-bench/ -run TestReplayCompare -args --compare require_support_citation

Replay gates with the rule on: 86 pass, 0 fail (the test also holds the run with the rule
off to every gate). The fake investigator's contrarian and injection-following
transcripts contest the weakest open hypothesis citing whatever evidence they saw, so the rule
is expected to drop most of their contests; these numbers measure the plumbing only. The
shipped default is decided by the live-model run (CHECK-19, OWNER ACTION).

## Compare: require_support_citation

Arm causal-graph+investigator, both settings on one corpus and commit. Kept = contests and conclusions with the rule on / with it off; a family fails when it keeps less than 90% (a fall of more than 10 points). A fake model measures plumbing only: a live-model run decides the shipped default.

| family | runs | contests+conclusions off | on | downgraded | kept | gate |
| --- | --- | --- | --- | --- | --- | --- |
| connection_pressure | 24 | 1 | 0 | 1 | 0.0% | fail |
| lock_blocking | 21 | 4 | 2 | 2 | 50.0% | fail |
| plan_regression | 6 | 0 | 0 | 0 | n/a | pass |
| wal_retention | 22 | 2 | 0 | 2 | 0.0% | fail |
| all | 73 | 7 | 2 | 5 | 28.6% | fail |

Deterministic-arm parity: identical (state, root, ranking, safe_pass, top-1).
