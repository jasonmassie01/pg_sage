# pg_sage Fresh Local QA Report

Date: 2026-05-09
Repo: `C:\Users\jmass\pg_sage`
HEAD: `001a91b`

## Local Sidecar

- URL: `http://127.0.0.1:18085`
- Metrics: `http://127.0.0.1:19187/metrics`
- Login: `admin@pg-sage.local` / `CodexVerify123!`
- Fresh Postgres: `pg_sage_fresh_qa` on `127.0.0.1:55432`
- DSN: `postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable`
- Gemini live testing used environment variables only; no key is recorded here.

## Summary

Fresh local validation confirms the Agent DB deployment module works locally for
schema and database provisioning, lifecycle pings, lease extension, backup
checks, restore-drill gating, destroy dry-run, cost summary, query tuning
recommendation publishing, custom T-shirt sizes, and the Agent DB UI.

The current product surface is usable at the URL above. The backend and current
sidecar web suites are passing. The remaining red area is the legacy root
Playwright suite, which needs fixture separation and route expectation updates
before it can be used as a single release gate.

## Bugs Found And Fixed

1. Live Gemini returned a broad planner toggle for a bitmap-scan tuning case.
   The tuner prompt now steers toward supported hints and forbids broad
   `enable_*` toggles. Targeted live retry and the full Go e2e suite passed.

2. Root Playwright login/navigation smoke used stale Dashboard-era assumptions.
   Helpers and assertions now match the current Overview/Cases UI. Targeted root
   smoke passed.

3. Root Playwright mixed standalone and full-surface assumptions. Current-route
   smoke was updated, full-surface-only specs were explicitly gated, and the
   standalone root gate now passes serially.

4. The fresh Postgres container exited during one rerun, causing sidecar login
   failures while Postgres refused connections. The container was restarted,
   admin credentials were reset, and login was reverified.

5. Hermes identified a real `pg_hint_plan` action SQL bug. Generated tuner
   action SQL now targets `hint_plan.hints.query_id` instead of the obsolete
   `norm_query_string` column, with a database-backed regression test.

## Verification

- Backend coverage: `go test -cover -count=1 -p 1 ./...` passed.
- Go e2e with live Gemini: `go test -tags=e2e -count=1 -v ./e2e` passed.
- Live API checks: 17/17 passed.
- Web Vitest: 30 passed.
- Web lint: passed.
- Web production build: passed with a Vite chunk-size warning.
- Sidecar web Playwright: 48 passed.
- Live UI checks: 4/4 passed.
- Targeted root Playwright smoke: 12 passed, 1 fixture-gated skip.
- Root Playwright standalone gate: 44 passed, 138 explicitly skipped.

## Full Root Playwright Status

The full root suite failures are classified in
`tasks\subagent_root_e2e_failure_classification.md`.

No definitive product bug was proven by that run. The failures were mainly:

- stale route/page expectations after Findings, Incidents, Schema Health, and
  Query Hints were consolidated into Cases;
- fleet/full-surface specs run against a standalone fresh sidecar;
- hard-coded Docker database reads that do not prove they are inspecting the same
  DB the sidecar used.

The notification masked-secret test now verifies through the same API target as
the running sidecar instead of reading a hard-coded Docker database.

## Receipt Files

- `QA_CHECKPOINT.md`
- `QA_REPORT.md`
- `tasks\qa_go_cover_final_20260509.log`
- `tasks\qa_go_e2e_tags_retry_20260509.log`
- `tasks\qa_live_api_checks_20260509.txt`
- `sidecar\tasks\qa_live_ui_checks_20260509.txt`
- `tasks\qa_web_vitest_20260509.log`
- `tasks\qa_web_lint_20260509.log`
- `tasks\qa_web_build_20260509.log`
- `tasks\qa_web_playwright_retry_20260509.log`
- `tasks\qa_root_playwright_targeted_retry_20260509.log`
- `tasks\qa_root_playwright_fixtureaware_workers1_retry_20260509.log`
- `tasks\qa_hermes_remediate_tuner_executor_final_20260509.log`
- `tasks\qa_hermes_remediate_go_cover_retry_20260509.log`
- `tasks\qa_hermes_remediate_root_playwright_retry_20260509.log`

## Next Recommendation

Run the skipped full-surface root coverage once that fixture is intentionally
started, with `PG_SAGE_E2E_FIXTURE=full-surface`. Also decide whether the old
dedicated Findings, Incidents, Schema Health, and Query Hints pages should be
restored or permanently represented by Cases.

