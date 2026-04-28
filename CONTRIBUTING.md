# Contributing to pg_sage

Thanks for your interest in pg_sage. Contributions to code, docs, tests,
research, and real-world workload reports are welcome.

## Current Project Shape

The active v0.9 product is the Go sidecar and embedded React UI:

- Backend: `sidecar/`
- Web UI: `sidecar/web/`
- Legacy PostgreSQL extension code: `src/` (kept for compatibility/history, not
  the primary product surface)

## Development Setup

```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage/sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
```

Run against a local database:

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@localhost:5432/postgres?sslmode=disable"
```

On first startup, pg_sage prints a one-time password for
`admin@pg-sage.local`; use it to log in at `http://localhost:8080`.

## Test Commands

Backend:

```bash
cd sidecar
go test -p 1 -count=1 ./...
go test -p 1 -cover -count=1 ./...
```

Frontend:

```bash
cd sidecar/web
npm run lint
npm test
npm run build
npm run test:e2e
```

Use `-count=1` for Go tests so cached results do not hide failures.

## Pull Request Process

1. Create a focused branch.
2. Add or update tests for behavior changes.
3. Update docs when setup, UI routes, API contracts, or config keys change.
4. Run the relevant verification commands and include results in the PR.
5. Submit a PR with a clear description of what changed and why.

## Code of Conduct

Be respectful, be constructive, and optimize for making Postgres operations
safer.

## License

By contributing, you agree that your contributions will be licensed under
AGPL-3.0.
