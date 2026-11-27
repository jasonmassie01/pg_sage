# Full-surface browser suite

This suite drives the real pg_sage binary through the real dashboard: approvals, emergency
stop, role boundaries, the walkthrough and the fleet views. It runs only against the disposable
fixture that `scripts/ci/full-surface-fixture.sh` builds. Never point it at a monitored database:
the action helpers seed findings, indexes and hints into the fixture's own target databases.

The mocked dashboard suite in `sidecar/web/e2e` (every `/api` route stubbed) is separate; it runs
on every push. This one runs in CI as the `web-e2e-full` job on nightly, tag and manual runs, and
a release waits for it.

## The fixture

`bash scripts/ci/full-surface-fixture.sh 17` (any major from 14 to 18) does all of this, from a
clean machine with Docker, Go, Node, curl and openssl:

| Piece | Value |
|---|---|
| PostgreSQL container | `pgsage_e2e` (`PG_SAGE_E2E_PG_CONTAINER`), the mirrored CI image, 127.0.0.1:5455 |
| Metadata database | `e2e_meta` |
| Targets (fleet name / database) | `testdb` / `e2e_target1`, `testdb2` / `e2e_target2`, `health_test` / `e2e_target3`, each with `pg_stat_statements` |
| pg_sage | built from this checkout (dashboard included), metadata fleet mode, API 127.0.0.1:8086, metrics 127.0.0.1:9189 |
| Encryption key | random per run, test-only |
| Admin | `e2e-admin@pg-sage.local`, created by `cmd/create_admin` with a random password |

The script stops with a message if the container name is taken, the PostgreSQL major is missing
or out of range, or a port is invalid. It checks that `/ready` returns 200 and that
`GET /api/v1/databases` lists exactly the three targets, or it exits non-zero. It never prints
the admin password; in Actions it masks it.

The settings the suite reads (`PG_SAGE_E2E_FIXTURE=full-surface`, `PG_SAGE_E2E_BASE_URL`,
`PG_SAGE_E2E_METRICS_URL`, `PG_SAGE_E2E_PG_CONTAINER`, `PG_SAGE_E2E_PG_PORT`,
`PG_SAGE_ADMIN_EMAIL`, `PG_SAGE_ADMIN_PASS`) go to `$GITHUB_ENV` in Actions, or to
`$PG_SAGE_E2E_WORKDIR/e2e.env` (default `/tmp/pg_sage_e2e/e2e.env`) locally. Without them the
run fails in global setup; no test skips for a missing fixture.

## Running it locally

```bash
bash scripts/ci/full-surface-fixture.sh 17
set -a; . /tmp/pg_sage_e2e/e2e.env; set +a
cd e2e && npm ci && npx playwright install chromium
npx playwright test --grep-invert 'encryption-key requirement' --workers=1 --reporter=list
cd .. && bash scripts/ci/full-surface-fixture.sh down
```

Run it serially (`--workers=1`): the walkthrough and emergency-stop specs change runtime config
and restore it in `finally` blocks, and parallel mutations would race.

The encryption-key requirement test needs a server without a key, which the encrypted metadata
fleet cannot be. `full-surface-fixture.sh keyless` starts a standalone server with no key on
127.0.0.1:8087 against the same container; run that one test with

```bash
PG_SAGE_E2E_BASE_URL=http://127.0.0.1:8087 npx playwright test \
  --grep 'encryption-key requirement' --workers=1
```

Notification and model-discovery scenarios use local HTTP receivers; they never send to Slack,
email recipients or LLM providers. The Agent DB navigation test does not provision cloud
infrastructure.

In CI, `scripts/ci/playwright-skip-gate.mjs` reads the JSON report and fails the job when any
test was skipped or failed.
