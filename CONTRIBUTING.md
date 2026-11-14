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

### Backend

Go tests that need PostgreSQL read one variable, `SAGE_TEST_DATABASE_URL`:
a disposable server on which every test package creates, and afterwards
drops, its own database. Without it those tests skip and each package prints
`testdb: SAGE_TEST_DATABASE_URL unset; database tests in <package> will skip`
(shown with `-v` or `-json`; `go test ./...` hides a passing package's
output), so a plain `go test ./...` reports `ok` while most database tests
never ran. Never point it at a shared or production server: the fixture
creates and force-drops databases there.

Start CI's own server image (pgvector with HypoPG, pg_hint_plan and pgaudit
installed, `pg_stat_statements` preloaded, logical WAL; recipe in
`scripts/ci/images/Dockerfile.pg`), from the repository root. Pull it, or
build it with the base digest from `.github/workflows/mirror-ci-images.yml`:

```bash
docker run -d --name pgsage-test -e POSTGRES_PASSWORD=postgres \
  -p 127.0.0.1:55432:5432 ghcr.io/jasonmassie01/pg_sage-ci-pgvector:0.8.2-pg17-r1
# or: docker build -f scripts/ci/images/Dockerfile.pg --build-arg PG_MAJOR=17 \
#   --build-arg BASE_DIGEST=sha256:<pg17 base> -t pgsage-ci:17 scripts/ci/images
bash scripts/ci/verify-test-postgres.sh pgsage-test 17
export SAGE_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
```

Then run the suites as CI does and check the skip budget and the coverage
floors:

```bash
cd sidecar
go test -race -cover -count=1 -tags=integration -timeout 1500s ./... -json > ../unit.json
go test -count=1 -tags=e2e -timeout 900s ./e2e -json > ../e2e.json
cat ../unit.json ../e2e.json | go run ./cmd/skipbudget -pg 17
go run ./cmd/covfloor < ../unit.json
```

`cmd/covfloor` fails when a package's statement coverage is under its floor in
`sidecar/.coverage-floors`: 70% unless a line lowers it (50% for test helpers
and generators). It also fails on a package that failed to build or printed no
coverage line. Lower a floor only with a `# reason` a reviewer can verify,
never to turn a red build green; the tool refuses a line without a reason.

`-race` needs cgo and a C compiler; leave it out without one. Build tags pick
the suites: `integration` adds the tests against the server, `e2e` the
end-to-end runs of the sidecar binary in `sidecar/e2e`; `perfgate`,
`providerlive` and `cloudlive` are CI or live-provider suites. A skipped test
fails `cmd/skipbudget` unless `sidecar/.skip-allowlist` names it with a reason
a reviewer can verify; add an entry only with such a reason. For a quick loop
on one package, `go test -count=1 -tags=integration ./internal/store/` is
enough. Remove the server with `docker rm -f -v pgsage-test`.

A package's fixture database starts empty (`testdb.Run`), so every
package bootstraps the sage schema, and a full run does that in about
200 databases at once. A package whose tests only need the schema opts
into one bootstrapped template per run instead:

```go
func TestMain(m *testing.M) {
	os.Exit(testdb.RunBootstrapped(m.Run, "internal/<pkg>", schema.Bootstrap))
}
```

The first such package builds `pgsage_tpl_<hash>` (the hash covers
`internal/schema/testdata/migration_checksums.txt` and the schema
package's sources, so a schema change builds a new one) under a cluster
lock; the others wait and copy it. Its fixture, and each database
`testdb.CreateBootstrappedDatabase(t, label)` makes, is a copy, so
`schema.Bootstrap` on it only checks. Tests still call `schema.Bootstrap`.
Without the checksum file the fixtures start from `template0` and the
package says so once. Templates of other hashes older than an hour are
dropped by the next build; removing the server removes them all. Keep
`testdb.Run` and `testdb.CreateDatabase` for tests of bootstrap or
migration behaviour: they need an empty database (`internal/schema`
asserts its fixture came from `template0`).

Every package shares that one server, so anything cluster-wide is shared too:
WAL volume and checkpoints, XID consumption, `pg_stat_reset_shared`,
`pg_stat_statements`, the I/O statistics and the whole-cluster backend count.
A test that measures or resets such state calls `testdb.Exclusive(t, reason)`
instead: it gets a fresh database on a second server, `SAGE_TEST_EXCLUSIVE_URL`,
which it holds alone until it ends (a cluster lock, so measuring tests run one
at a time across packages), then the database is dropped and the server
released. Start a second container from the same image on another port and
export it next to the first:

```bash
docker run -d --name pgsage-test-exclusive -e POSTGRES_PASSWORD=postgres \
  -p 127.0.0.1:55433:5432 ghcr.io/jasonmassie01/pg_sage-ci-pgvector:0.8.2-pg17-r1
bash scripts/ci/verify-test-postgres.sh pgsage-test-exclusive 17
export SAGE_TEST_EXCLUSIVE_URL="postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable"
```

`Exclusive` compares `system_identifier` (`pg_control_system()`) on both
servers and fails the test, "same cluster", when the two URLs name one
cluster, or when `SAGE_TEST_DATABASE_URL` is unset and it cannot tell. Without
`SAGE_TEST_EXCLUSIVE_URL` those tests skip with an `exclusive-cluster:` reason
that `.skip-allowlist` never lists, so the skip budget fails a CI job that
lost the server. Every CI job with a test server runs a `postgres-exclusive`
service on port 5433. A test that needs a second database there (the perf
gate's meta store) adds it with `testdb.ExclusiveDatabase(t, label)`.

To clear your own statement statistics on the shared server, call
`testdb.ResetStatements(ctx, pool)`: it resets only the current database's
`pg_stat_statements` entries (the three-argument reset) and returns the
extension's `dealloc` count, so you can tell an eviction from your reset.
Only a reset of every database moves `pg_stat_statements_info.stats_reset`;
a test that needs that, a `pg_stat_reset_shared` call or a `CHECKPOINT` runs
on `testdb.Exclusive`. `TestResetLint` (`go test -run TestResetLint
./internal/testdb/`) fails, naming file and line, on a zero-argument
`pg_stat_statements_reset()` (or its all-zero `(0, 0, 0)` form), a
`pg_stat_reset_shared(` call or an executed
`CHECKPOINT` in a test file or under `internal/testsupport/` that no
`Exclusive` test reaches.

Compare times on the clock that produced them. The test server's clock is
not the host's: the Docker Desktop VM clock slews by about -0.9 s to +2.5 s
within minutes. A stamp compared with a database timestamp (`now()`,
`clock_timestamp()`, a column default) comes from `testdb.DBNow(ctx, q)` or
`testdb.MustDBNow(t, q)`, never `time.Now()`; an interval the database
enforces (a lease TTL, a queue deadline) is measured as the difference of
two `DBNow` readings, not with `time.Since`. When a test bounds a
database-clock interval that host-timed steps drive (ticks, sleeps), start
`w, err := testdb.StartClockWatch(ctx, q)` before the interval and check
against `w.Widen(t, bound)`: it adds how far the database clock moved
against the host's monotonic clock (`w.Step(ctx)`), logs it, and fails the
test if the sample fails. Never widen such a bound by a constant. A time the
sidecar stamps on the host (a fallback log entry, for example) stays on the
host clock.

On a machine without cgo (Windows, for example), `scripts/ci/local-linux-suite.sh
<out-dir> [majors...]` runs the test job's suite command on Linux, with cgo and
`-race`, once per PostgreSQL major (default 14, 15 and 17; PostgreSQL 17 also
runs the e2e step). Then it runs the skip budget (and, on PostgreSQL 17, the
coverage floors) and writes `<out-dir>/summary.md`, which lists every failure,
data race, UNLISTED skip and package under its floor.
It uses local images only and never pulls one. `GO_IMAGE` needs gcc and a
race runtime that links. The default, `golang:1.27.3-alpine3.24`, has no gcc,
so the run stops with exit 7; set `GO_IMAGE` (and `GO_ROOT` for a Go tree
mounted at `/opt/go`) to an image that has them. It tests a `git archive` of
`LOCAL_SUITE_REV` (default `HEAD`), vendored offline from your module cache,
and never writes inside the worktree. It creates and removes only containers
named `improve-ci-*` (`LOCAL_SUITE_PREFIX`). A full run takes about 40 to 70
minutes per major, so run one per machine at a time.
`LOCAL_SUITE_DRY_RUN=1` checks the inputs and prints the plan, and
`scripts/ci/local-linux-suite-test.sh` tests the script without Docker.
The local servers are not CI's image. The `improve-pg` images lack pgaudit,
and `postgres:15` lacks pg_hint_plan, HypoPG and pgvector. On those servers
the run cannot exercise pg_hint_plan paths (tuner hint tables,
`test/hint_verify`, hint evidence), HypoPG what-if paths (the optimizer and the
explain "hypothetical" paths), pgvector (vectorlab) or pgaudit correlation.
`<out-dir>/pg<N>/server-check.txt` names what each server lacks. Each such
test shows up as an UNLISTED skip or as a failure labelled `image-limited`.
None of them is accepted silently, and none belongs in `.skip-allowlist`.

To tell port exhaustion from other dial failures in a full round, sample
the host's TCP ports while it runs: `scripts/ci/port-usage.sh start
<round-dir> <server-port...>` samples `netstat` every 10 s
(`PORT_USAGE_INTERVAL`), `scripts/ci/port-usage.sh group <round-dir>
<group>` labels the samples before each group starts, and
`scripts/ci/port-usage.sh stop <round-dir>` ends it.
`scripts/ci/port-usage.sh summary <round-dir>` writes
`<round-dir>/port-usage.md`, which gives, per group, the peak count of
dynamic ports in use and the TIME_WAIT and ESTABLISHED sockets to the test
servers. It also names the round's peak and says whether it came near the
range (50% by default, `PORT_USAGE_NEAR_PCT`). `scripts/ci/port-usage-test.sh`
tests the script with a stub socket table.

`docker-compose.test.yml` is not the Go test server: it runs two PostgreSQL 16
targets (no HypoPG) seeded from `test-fixtures/seed.sql`, plus mocks, for the
functional harness `test-fixtures/run-tests.sh`.

### Test environment

Variables the test suites and benches read. Runtime variables are in
[docs/configuration.md](docs/configuration.md#environment-variables).

<!-- test-env-vars:start (generated by cmd/gen_config_meta; do not edit) -->
| Variable | Default | Description |
|---|---|---|
| `SAGE_TEST_DATABASE_URL` | (none) | Disposable PostgreSQL server for database tests; each test package creates and drops its own database on it. Unset, database tests skip. Never point it at a shared server |
| `SAGE_TEST_SECOND_CLUSTER_URL` | (none) | A second disposable PostgreSQL server, for tests that need a database on another cluster; unset, they skip |
| `SAGE_TEST_EXCLUSIVE_URL` | (none) | A separate disposable PostgreSQL server for tests that measure or reset cluster-wide state; they run on it one at a time. It must not be the `SAGE_TEST_DATABASE_URL` server (checked by `system_identifier`). Unset, they skip, and the CI skip budget fails |
| `SAGE_TEST_REPLICA_URL` | (none) | Streaming replica of the `SAGE_TEST_DATABASE_URL` server, for replica tests; unset, they skip |
| `SAGE_TEST_DSN` | (set by the fixture) | Legacy test DSN name; the fixture overwrites it with the package database, so do not set it |
| `SAGE_BENCH_RUN` | (unset) | `1` runs `TestPGIncidentBench` in `sre-bench` |
| `SAGE_BENCH_REPEATS` | `1` | Runs of every PGIncidentBench scenario, 1 to 10 |
| `SAGE_BENCH_MIN_RUNS_PER_FAMILY` | (unset) | Scored top-1 runs every PGIncidentBench family needs: sizes the repeats, tops up short families and fails a family still short |
| `SAGE_BENCH_EXPECTED` | (unset) | PGIncidentBench expected-scenario manifest; a skip it does not allow fails the run |
| `SAGE_BENCH_REPLAY` | (unset) | `1` also replays the PGIncidentBench corpus (CI: the core shard only) |
| `SAGE_BENCH_REPORT_DIR` | (a temporary directory) | Directory that receives the PGIncidentBench JSON and Markdown reports |
| `SAGE_BENCH_FAMILIES` | (all) | Comma-separated trigger kinds a bench run is limited to, such as `checkpoint_storm,lwlock_contention` |
| `SAGE_BENCH_SPLIT` | `all` | Replayed cases: `all`, `held_out` or `tuning` |
| `SAGE_BENCH_LIVE_ARM` | (unset) | `1` runs `TestLiveModelArm`, which calls the configured LLM |
| `SAGE_BENCH_LIVE_MODEL_ARM` | `causal-graph+llm` | Model arm a live run measures: `causal-graph+llm` or `causal-graph+investigator` |
| `SAGE_BENCH_PG_SAGE_VERSION` | (none) | Release tag stamped into bench reports (a leading `v` is dropped); CI sets it on tags |
| `SAGE_BENCH_PG_SAGE_COMMIT` | (none) | Commit stamped into bench reports, so a sidecar refuses a report made for another build |
| `SAGE_SAFETY_BENCH_RUN` | (unset) | `1` runs the full `TestAgentSafetyBench` in `safety-bench` |
| `SAGE_SAFETY_BENCH_REPORT_DIR` | (a temporary directory) | Directory that receives the agent-safety bench JSON and Markdown reports |
<!-- test-env-vars:end -->

### Frontend

```bash
cd sidecar/web
npm run lint
npm test
npm run build
npx playwright install chromium   # once
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

`npm run test:e2e` runs the mocked dashboard suite (`sidecar/web/e2e`, every
`/api` route stubbed). Locally it starts the Vite dev server. With `CI` set it
runs `vite build` and serves the built bundle with `vite preview`, so the specs
exercise the bundle the sidecar embeds. Run `CI=1 npm run test:e2e` to
reproduce the CI run; it rebuilds `sidecar/internal/api/dist`, so restore that
directory before committing. CI runs it on every push and fails on any
skipped test.
The full-surface suite in `e2e/` drives the real binary against a disposable
fixture that `bash scripts/ci/full-surface-fixture.sh 17` builds; see
`e2e/README.md`. CI runs it nightly, on tags and on manual runs.

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
