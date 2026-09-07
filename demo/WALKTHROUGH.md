# pg_sage v1 Demo Walkthrough

This is the current live demo path. It intentionally avoids old extension/MCP
language: v1 is a Go sidecar with an authenticated React UI and REST API.

## 1. Start the Demo

```bash
cd demo
./run-live.sh
```

The script starts PostgreSQL on `localhost:5433`, builds the web assets and Go
binary, then starts pg_sage:

| Surface | Address |
|---|---|
| UI/API | `http://localhost:8080` |
| Metrics | `http://localhost:9187/metrics` |
| Demo database | `localhost:5433/sage_demo` |

Copy the one-time `INITIAL ADMIN PASSWORD` from stderr.

## 2. Log In

Open `http://localhost:8080`.

- Email: `admin@pg-sage.local`
- Password: the startup password

## 3. Show the UI

| Screen | Talking points |
|---|---|
| Overview | pg_sage is an autonomous DBA control surface, not just graphs. Show fleet health, database tile, Provider Readiness, and recent recommendations. |
| Cases | The primary work queue. Cases combine deterministic evidence, why-now text, policy state, lifecycle state, and next actions. |
| Actions | Pending approval and executed history. Expand a row to show SQL, rollback/mitigation, expiration, and verification status. |
| Fleet | Managed database connection state. |
| Settings | Emergency stop/resume and Shadow Mode proof. |

The legacy `#/findings` route is kept as a compatibility alias and opens Cases.

## 4. API Checks

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/actions/pending
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
```

## 5. Metrics

```bash
curl -s http://localhost:9187/metrics | head
```

## 6. Clean Up

Stop pg_sage with `Ctrl+C`.

```bash
docker compose -f demo/docker-compose-live.yml down -v
```

The `-v` flag deletes the local demo database volume.
