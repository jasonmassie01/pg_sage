# Contributing to pg_sage

Thanks for your interest in pg_sage. Contributions to code, docs, tests,
research, and real-world workload reports are welcome.

## Current Project Shape

The product is the Go sidecar and embedded React UI:

- Backend: `sidecar/`
- Web UI: `sidecar/web/`
- The former C extension was removed; its last source is at the
  `c-extension-final` tag

## Development Setup

A C compiler (gcc or clang) is also needed: with cgo the binary links
libpg_query, which checks every executor statement against its PostgreSQL
parse tree. Without a C compiler the build still succeeds, but that layer is
left out; `pg_sage --version` then reports `sql-ast: unavailable`, startup
logs a warning, and changes pg_sage would make on its own wait for operator
approval instead. Release binaries and Docker images always include it.

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

For dashboard work, run `npm run dev` in `sidecar/web` and open
`http://localhost:5173`. The Vite dev server proxies `/api` to the sidecar on
`:8080`, so the browser sees one origin; the API sends no CORS headers and
refuses cross-origin writes.

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

CI runs the unit tests as `npm run test -- --coverage --maxWorkers=2`. Coverage
of `src/hooks` and of `src/lib` must each stay at or above 70% of lines and
functions, and `src/components/ConfigTooltip.jsx` at 100%; below that the
command exits non-zero. Test the shared data layer through the real hooks:
`vitest.setup.js` installs a `FakeEventSource` (`src/test/fakeEventSource.js`)
on `globalThis`, and `stubFetch` (`src/test/fetchStub.js`) answers `fetch` with
real `Response` objects, so a test does not need to mock `useAPI` or
`useLiveEvents` away.

`npm run test:e2e` runs the mocked Playwright specs in `e2e/`. Locally it starts
the Vite dev server. With `CI` set it runs `vite build` and serves the built
bundle with `vite preview`, so the specs exercise the bundle the sidecar
embeds. Run `CI=1 npm run test:e2e` to reproduce the CI run; it rebuilds
`sidecar/internal/api/dist`, so restore that directory before committing.

With Node 26, run unit tests with
`NODE_OPTIONS=--no-experimental-webstorage npm test`. Its experimental global
storage otherwise overrides jsdom's browser storage and fails storage-dependent
tests. The production browser uses its own storage.

Use `-count=1` for Go tests so cached results do not hide failures.

The Go release that builds every shipped artifact is the `toolchain` line in
`sidecar/go.mod`. To move to a new Go release, change it together with the
`golang:` stages in `sidecar/Dockerfile` and `deploy/databricks-apps/Dockerfile`
and the `goreleaser-cross` tag in `.github/workflows/ci.yml`;
`TestToolchainPinsAgree` (`internal/startup`) fails when they disagree. The
`security` CI job (govulncheck, npm audit, Trivy) must pass before an image or
release is published.

When a release changes the minor version in `VERSION`, bump the supported row of
the version table in `SECURITY.md` in the same change;
`TestSecurityDocsSupportedVersionMatchesVERSION` (`internal/config`) fails until
it matches.

Databricks Apps packaging (from the repository root, Python standard library only):

```bash
python3 -m unittest discover -s deploy/databricks-apps -p 'test_*.py' -v
```

## Pull Request Process

1. Create a focused branch.
2. Add or update tests for behavior changes.
3. Update docs when setup, UI routes, API contracts, or config keys change.
   New API routes are registered with `route(mux, pattern, gate, handler)` in
   `sidecar/internal/api` (see `route_registry.go`), never with `mux.Handle`;
   `TestNoBareAPIHandle` fails on a bare registration and the router table
   tests probe every registered route as nobody, viewer, operator and admin.
   Add a golden probe for the route to `router_table_probes_test.go` and
   regenerate with `go test ./internal/api -run RouterTable_MatchesGolden
   -update-router-golden`.
4. Run the relevant verification commands and include results in the PR.
5. Submit a PR with a clear description of what changed and why.

## Code of Conduct

Be respectful, be constructive, and optimize for making Postgres operations
safer.

## License

By contributing, you agree that your contributions will be licensed under
AGPL-3.0.
