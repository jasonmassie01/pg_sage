# Browser scenario verification

Run these tests only against a disposable pg_sage server and disposable PostgreSQL databases.
The September 4 audit removed full-surface writes to real health and monitoring databases.
`fixture-targets.ts` contains the exact three disposable targets used by the SQL scenarios:

| Logical fleet name | Docker container | Physical database |
|---|---|---|
| testdb | pgsage_audit_20260904 | audit_browser_target1 |
| testdb2 | pgsage_audit_20260904 | audit_browser_target2 |
| health_test | pgsage_audit_20260904 | audit_browser_target3 |

The API test server uses metadata DB audit_browser on local port 5455, API 8086, and metrics9189.
Each target needs schema bootstrap plus pg_stat_statements. Use metadata fleet mode with an
explicit test-only encryption key, and create the three managed targets. The action helpers
seed their own disposable findings/indexes; never point them at user data. Temporary audit
infrastructure is removed at handoff; recreate these fixtures before a future full-surface run.

Provide PG_SAGE_ADMIN_EMAIL, PG_SAGE_ADMIN_PASS, PG_SAGE_E2E_BASE_URL,
PG_SAGE_E2E_METRICS_URL, and PG_SAGE_E2E_FIXTURE=full-surface in the test process environment.
Create the ephemeral admin through the sidecar cmd/create_admin utility against the isolated
metadata DB; keep its password out of committed files. Run from this directory:

```powershell
npx playwright test --grep-invert 'encryption-key requirement' --workers=1 --reporter=list
```

The encryption-key requirement test uses a separate standalone server with no key; that negative
configuration conflicts with the encrypted metadata fleet used for the other tests. Verify it
separately rather than calling it skipped or accepting an unexpected successful save.

Walkthrough mutations restore config and remove owned records in finally blocks. Run mutations
serially. Notification and model-discovery scenarios use local HTTP receivers; they do not send
to Slack, email recipients, or external LLM providers. The Agent DB navigation test does not
provision cloud infrastructure; actual cloud creation/teardown is outside this browser gate.

Reports and the original 124-check mapping are in ../tasks/browser-audit-2026-09-04.md and
../tasks/walkthrough-repair-2026-09-04.md. Local task reports are preserved independently of git.
