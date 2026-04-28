# pg_sage Demo

Local demo assets for showing the v0.9 autonomous DBA workflow.

## Quick Demo

```bash
cd demo
./run-live.sh
```

The script starts PostgreSQL on `localhost:5433`, builds the embedded web UI
and sidecar binary, and starts pg_sage on:

| Service | URL |
|---|---|
| Dashboard/API | `http://localhost:8080` |
| Prometheus metrics | `http://localhost:9187/metrics` |
| PostgreSQL demo target | `localhost:5433/sage_demo` |

On first start, copy the one-time `INITIAL ADMIN PASSWORD` from the sidecar
stderr and log in as `admin@pg-sage.local`.

`SAGE_GEMINI_API_KEY` is optional. If set, the LLM-backed advisor and optimizer
paths can run; deterministic rules, Cases, Actions, Fleet, Settings, and Shadow
Mode work without it.

## What To Show

1. **Overview** -- fleet health, database tile, Provider Readiness.
2. **Cases** -- ranked DBA cases from findings and action state.
3. **Actions** -- pending approval and executed action history.
4. **Settings** -- emergency controls and Shadow Mode avoided-toil report.
5. **API** -- authenticated `/api/v1/cases`, `/api/v1/actions/pending`, and
   `/api/v1/shadow-report`.
6. **Metrics** -- Prometheus `/metrics`.

## API Example

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
```

## Recording

The older `demo.sh` asciinema script is kept as a recording helper, but the
product walkthrough source of truth is now [WALKTHROUGH.md](WALKTHROUGH.md).
Prefer `run-live.sh` for live verification.
