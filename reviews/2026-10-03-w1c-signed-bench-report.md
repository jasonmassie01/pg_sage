# W1c: signed release bench reports, provenance and local bench runs (roadmap 1.1)

Branch `claude/w1c-signed-bench`, cut from `origin/master` at e9feff49. Roadmap item: Phase 1.1,
"Ship a CI-signed bench report with every release and ingest it at startup; allow a local
bench run on a clone to count for the families it covered."

## What was built

| Area | Files | What |
|---|---|---|
| Signing (CI) | `.github/workflows/ci.yml`, `.goreleaser.yml`, `sidecar/Dockerfile`, `sidecar/release-bench/README.md`, `.gitignore` | Bench reports name the build they scored. A new `bench-sign` job signs the three shard reports keyless and verifies them twice. The image and the archives carry them, and the release lists them as assets |
| Verification | `sidecar/internal/benchsig/` (with the embedded `trusted_root.json`), `sidecar/cmd/sigstore_trusted_root/` | Offline Sigstore verification (sigstore-go v1.2.0), pinned to the pg_sage CI workflow identity. A tool refreshes the trusted root through TUF |
| Report stamping | `sidecar/sre-bench/provenance.go`, `report.go`, `gameday.go`, `bench_test.go` | `pg_sage_version` and `pg_sage_commit` in the report. `RepeatsFor` sizes game days and local runs |
| Ledger provenance | `sidecar/internal/earned/provenance.go`, `bench.go`, `evidence.go`, `service*.go`, `store*.go`, `view.go`, `guidance.go`; `internal/schema/sre_bench_provenance_migration.go` (one line added to `bootstrap.go`) | Every stored report keeps its origin, build and signature. Builds are matched, a signature is bound to the report's commit, and reports of an older build stop counting. The view carries a bench summary per family |
| Ingest | `sidecar/internal/benchingest/` | Reads the shipped directories and `bench_results_path`, pairs each report with its bundle, and refuses bad signatures. Moved here from `main` |
| Local runs | `sidecar/internal/gameday/localbench.go`, `internal/api/autonomy_bench_handlers.go` (3 lines added to `autonomy_handlers.go`) | `LocalBench` runs on a clone. New routes `GET/POST /api/v1/sre/autonomy/bench-runs` |
| Wiring and CLI | `cmd/pg_sage_sidecar/bench_release.go`, `bench_cmd.go`; small edits to `autonomy_runtime_loops.go`, `autonomy_wiring.go`, `autonomy_fleet.go`, `main.go` | Ingest at startup and hourly, local bench registration, `pg_sage bench verify` |
| UI | `web/src/pages/autonomy/BenchEvidence.jsx`, `PathToNextLevel.jsx`, `AutonomyPage.jsx`; rebuilt `internal/api/dist` | Provenance per family, local-run status, "Run bench locally" |
| Docs | `docs/configuration.md`, config doc tags (with a regenerated `config_meta.json`), `CHANGELOG.md` | |

### Signing approach (keyless Sigstore)

- The repo did not use cosign before this change. The new `bench-sign` job runs on master pushes and `v*` tags, after `test`, with `id-token: write`. It:
  1. downloads the `pgincidentbench-pg17` artifact of the same run and keeps only the three shard reports;
  2. refreshes the Sigstore trusted root through TUF (`go run ./cmd/sigstore_trusted_root`);
  3. signs each report with `cosign sign-blob --yes --bundle <report>.sigstore.json` (cosign-installer v4, keyless, GitHub OIDC);
  4. verifies each report with `cosign verify-blob` (identity `https://github.com/<repo>/.github/workflows/ci.yml@<ref>`, issuer GitHub) **and** with `pg_sage bench verify --commit $GITHUB_SHA`.

  A format or trust-root mismatch therefore fails the build before anything ships. No secret is needed.
- `docker` and `release` now need `bench-sign`. Where the reports go:
  - **Image:** at `/usr/share/pg_sage/bench`, together with the trusted root they were verified with, so an air-gapped install has them.
  - **Archives:** `bench/<shard>/...` next to the binary. A local goreleaser snapshot confirmed this layout.
  - **GitHub release:** `pgincidentbench-<shard>.json`, `.json.sigstore.json` and `.md` as assets.
- The sidecar verifies offline. It requires:
  - a Fulcio chain to the embedded trusted root;
  - an SCT from a trusted CT log;
  - a transparency-log entry and an observer timestamp;
  - a certificate SAN matching `^https://github\.com/jasonmassie01/pg_sage/\.github/workflows/ci\.yml@refs/(heads/master|tags/v...)$`, issued by GitHub Actions;
  - a certificate source-repository digest equal to the report's `pg_sage_commit`.

### Ingest and provenance rules (product decisions)

- **Provenance values.** A report is `signed_release`, `local_run`, `operator` ("unsigned (operator-provided)") or `game_day`. The view and the UI show it per family.
- **Version gate.** A report names the build it scored.
  - When both the report and the running binary name a commit, the commit decides. The same commit under another version label is the same code.
  - Otherwise the version decides, compared without its leading `v`.
  - A stamped report for another build is **refused** at ingest (`ErrBuildMismatch`, HTTP 400).

  The alternative was to count only families whose detector code is unchanged. That needs a per-family detector fingerprint, embedded at build time and kept in sync by every detector change. Refusal is simpler and safe, and every release now ships its own report.
- **Old-build reports stop counting after an upgrade.** `LatestBench` filters stored reports by the running build. An unstamped report keeps counting exactly as before. That covers everything ingested before this change, and manual uploads.
- **Stamping.** Signed, local-run and shipped reports must name their build. An operator upload need not.
- **Bad signatures.** A report whose bundle does not verify is **refused**, not downgraded to unsigned, because a bad signature means tampering. Without verification material (no verifier) a bundle is ignored and the report goes in unsigned, never as signed.
- **Duplicates.** A duplicate (same hash) that arrives signed marks the stored copy signed. An unsigned copy never downgrades a signed one.
- **Release sample size.** CI ran the bench with 1 repeat, which gives n = 3 to 6 per family. That is below the spec's `MinTop1N = 10`, so even a perfect release report could never meet `bench_top1` at default thresholds.
  - Tag builds now run `SAGE_BENCH_REPEATS=4`. The smallest family has 3 scored scenarios, so it reaches n = 12.
  - At 1 repeat the three shards took 2, 6.5 and 3 min on the v1.8.4 tag run, so 4 repeats stay inside the 40-minute step timeouts.
  - Local runs repeat until every covered family reaches `max(MinTop1N, MinSafePassN)` (`RepeatsFor`, capped at 10).
- **Local runs ("Run bench locally").**
  - They use `clone.provider` (dle or snapshot), else `sre.autonomy.game_days.local_dsn`. Game days need not be enabled; the doc is updated.
  - Every monitored database **and the control/metadata database** are refused.
  - Only an admin can start one, and a database runs one at a time. The clone is always destroyed.
  - Only the deterministic causal-graph arm runs, so no model tokens are spent.
  - The report is stamped with the running build, ingested as `local_run`, and counts only for the families it covered.

  Clone infrastructure already exists, so I built the API/UI action rather than a `pg_sage bench --clone` CLI.
- **Manual verification.** `pg_sage bench verify [--commit SHA] <report>` lets anyone verify a downloaded release asset offline. CI uses the same command.

## Spec and brief checks covered

1. The release workflow signs the tag's reports keyless and attaches them (`bench-sign`, release `extra_files`). Only a real tag build proves this (see below).
2. The reports are embedded in the image at a fixed path: Dockerfile `COPY release-bench/ /usr/share/pg_sage/bench/`.
3. Shipped reports are ingested at startup and hourly, and only for the matching version: `TestShippedSignedReportIsIngestedAtStartup`, `TestShippedReportForAnotherVersionIsRefused`.
4. Signature verification (valid, tampered, wrong signer, untrusted Sigstore, no SCT, malformed bundle): `internal/benchsig`.
5. Provenance is recorded with the evidence: `TestIngestBenchStoresASignedReleaseReport` and the view summary.
6. An operator upload works as before, marked unsigned: `TestUnsignedOperatorReportWorksAsBefore`, `TestAutonomyAPI_ViewShowsBenchProvenance`.
7. A local run counts only for the families it covered and is marked local run: `TestLocalRunCountsOnlyForTheFamiliesItCovered`, plus `TestLocalBenchRunCountsForTheFamilyItCovered` end to end on real PostgreSQL.
8. A local run refuses the monitored DSN: `TestLocalBenchProviderNeverTargetsAMonitoredDatabase`, `TestLocalBenchTargetRefusesTheMonitoredDatabase`.
9. UI provenance and "Run bench locally": `AutonomyPage.bench.test.jsx` (8 tests).

## What only a real tag build can prove

- `id-token: write` works for this repo, and cosign keyless signing succeeds (Fulcio and Rekor are reachable from the runner).
- pg_sage's verifier, with the freshly fetched trusted root, accepts the bundle cosign v3 writes. That bundle is a message signature with a Rekor v1 or v2 entry, possibly with a TSA timestamp.
  - The unit tests use sigstore-go's virtual Sigstore, which cannot issue SCTs or reproduce cosign's exact output.
  - The `bench verify` step in `bench-sign` fails the release if verification does not work.
- The layouts across jobs: the `download-artifact` results, the image containing the three reports and their bundles, the archive `bench/` directory, and the release assets.
- The tag bench at 4 repeats passes its gates within the step timeouts.
- The goreleaser-cross build compiles with the refreshed `trusted_root.json`.

The first master push after merge exercises `bench-sign` and `docker` (`:edge`) the same way. Because `docker` now needs `bench-sign`, a Sigstore outage also blocks the edge image.

## Test Results

**Commands (Docker `golang:1.25`, `--cpus=2`, repo root mounted):**

- **Full suite, PG17** (`pgsage-ag8`, :55478): `go test -p 2 -count=1 -cover -timeout 1800s ./...`
  - 89 packages ok, 3 failed:
    - `internal/optimizer` and `internal/partition`: the fixture database timed out on the busy shared Docker VM; not a code problem. Both pass on rerun.
    - `internal/startup` (`TestShippedBuildsEnableCgoForSQLAST`): a real catch, bug 2 below. Fixed; passes on rerun.
  - Final: **92/92 packages pass.**
- **Touched packages with the race detector, PG17:** `go test -race -p 2 -count=1 -json` on benchsig, benchingest, earned, gameday, api, schema, cmd/pg_sage_sidecar, cmd/sigstore_trusted_root and sre-bench. Result: **2035 passed, 0 failed, 1 skipped**.
- **Touched packages, PG14** (:55414) **and PG18** (:55418): all pass.
  - The first PG14 run failed `TestLocalBenchRunCountsForTheFamilyItCovered` because of a test-fixture ordering bug (bug 5 below).
  - After the fix, `cmd/pg_sage_sidecar` passes on PG14 (374 s).
- **E2E:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/` gives **78 passed, 0 failed, 13 skipped**.
- **Lint and config checks:**
  - golangci-lint v2.11.4 (Windows binary), `run ./...`: **0 issues**.
  - gofmt (Linux): clean on the changed files.
  - actionlint v1.7.12 (Go-installed) on `ci.yml`: clean.
  - `goreleaser check`: valid. A scratch snapshot confirmed the `bench/*/*` archive layout.
- **Web:**
  - `npm run lint`: clean.
  - `npm test`: **53 files, 283 tests passed**, 8 of them new in `AutonomyPage.bench.test.jsx`.
  - `npm run build`: `internal/api/dist` rebuilt and committed. `node_modules` removed afterwards.
- **Mutation testing:** 17 mutants (15 Go, 2 UI), **all killed**.

**Total:** all 92 packages pass on PG17. The touched packages under race: 2035 passed, 0 failed, 1 skipped. E2E: 78 passed, 0 failed, 13 skipped.

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/benchsig | 87.9% |
| internal/benchingest | 95.1% |
| internal/earned | 88.9% |
| internal/gameday | 91.2% |
| internal/api | 78.8% |
| internal/schema | 83.4% |
| cmd/pg_sage_sidecar | 81.7% |
| cmd/sigstore_trusted_root (utility) | 56.1% |
| sre-bench | 62.4% |

### Skipped tests (must be zero or justified)

- sre-bench `TestPGIncidentBench`: skipped unless `SAGE_BENCH_RUN=1`. CI runs it in its own three shard steps, and it is allow-listed.
- e2e: 13 live-LLM tests (`TestLLM*`, `TestTunerLLM_*`, `TestOptimizerMultiQueryConsolidation`). They need a live model endpoint (`PG_SAGE_LIVE_LLM=1`) and are unrelated to this change.

### Failures

None remaining. The transient failures and the fixed ones are listed above.

### Coverage gaps

- `sre-bench` is at 62.4%, below 70%.
  - The uncovered code is the fault-program harness, which only `TestPGIncidentBench` runs (in the CI bench steps).
  - All new code there is covered: `BuildFromEnv`, `RepeatsFor`, `gameDayRun` and the stamping.
  - A real local run in `cmd/pg_sage_sidecar` drives `RunGameDay` end to end.
- `cmd/sigstore_trusted_root` is at 56.1%. It is a utility, so this is above its 50% floor. The uncovered part is the TUF network fetch, which the release workflow exercises.
- All other touched packages meet their thresholds.

### Manual checks remaining

- CHECK-REL-01: MANUAL. Needs a real master push and a tag build (see "What only a real tag build can prove").
- CHECK-UI-01: MANUAL. A visual check of the panel in a browser. Its behaviour is covered by vitest.

## Bugs found

1. [BUG, pre-existing, fixed] The release bench ran with 1 repeat (n = 3 to 6 per family), so no CI report could ever meet the default `bench_top1` sample size (n >= 10).
2. [BUG, mine, caught by an existing contract test] The first `bench-sign` step used `CGO_ENABLED=0 go run`. `internal/startup` `TestShippedBuildsEnableCgoForSQLAST` forbids that in ci.yml. Fixed in 57205c8c.
3. [CI artifact layout] The `pgincidentbench-pg17` artifact also holds the replay report at its root. Shipped as-is, the sidecar would refuse it at every hourly ingest. `bench-sign` now keeps only the shard reports.
4. Two test corrections, each explained in its commit:
   - 2515aeca: the virtual Sigstore cannot issue SCTs, so the valid-signature tests could not pass against a verifier that requires one. A new test asserts that the production verifier does require an SCT.
   - fe3c91f5: a depth expectation contradicted the documented 3-level `bench_results_path` walk.
5. [BUG, mine, found on PG14] The startup-ingest test fixture dated a shipped report one second in the future. The package's tests share one deployment, so the next test's local run came out older and lost the newest-report choice. Fixed by dating the fixture a minute in the past; no assertion changed.

## Post-test audit

- **Inputs not tested:**
  - a real cosign v3 bundle (needs a tag build);
  - a bundle with only a Rekor v2 entry and a TSA timestamp;
  - a dle or snapshot clone provider for local runs. A fake provider, and the local provider on real PostgreSQL, stand in.
- **Assertions that could pass when broken:** checked by mutation testing. All 17 mutants were killed:
  - the build match ignoring the commit;
  - the signature commit left unchecked;
  - `RequireBuild` ignored;
  - `LatestBench` without the build filter;
  - a duplicate never upgraded to signed;
  - no identity policy;
  - no SCT required;
  - a bad signature falling back to unsigned;
  - shipped reports not required to name the build;
  - a local run marked operator;
  - the clone not destroyed on failure;
  - the local provider ignoring refused DSNs;
  - POST open to operators;
  - `bench verify` ignoring `--commit`;
  - wrong repeat rounding;
  - the UI button shown to non-admins;
  - the UI button shown on every pair.
- **Fakes that hide failures:**
  - The virtual Sigstore has no SCTs and does not produce cosign's encoding. The double verification in CI covers this.
  - `benchingest` and the cmd tests use a fake verifier. The real one is tested in `benchsig`.
  - The API tests use a fake ledger for local runs. The real ledger is covered end to end in cmd.

## Coordinator decisions

1. **Binary size.** sigstore-go brings in rekor, grpc, protobuf and otel. The stripped no-cgo binary grows from 28.6 MB to 48.1 MB (+19.5 MB).
   - The alternative is an ed25519/minisign key in a repo secret: stdlib verification and about 0 MB, at the cost of managing the key.
   - I kept keyless signing, as the brief prefers.
2. **sigstore-go v1.2.0, not v1.3.0.** v1.3.0 requires `go 1.25.8`. That would bump `go.mod`, and the goreleaser-cross v1.25.0 image does not carry it.
3. **Publishing depends on signing.** `docker` (including `:edge`) and `release` now depend on `bench-sign`, so a Sigstore outage blocks publishing. That is acceptable for releases; for `:edge` the dependency could be made soft.
4. **Longer release CI.** Tag builds run the bench 4 times, so the bench steps take about 4x as long.

## What is left

- Prove the items under "What only a real tag build can prove" on the first master push and the next tag.
- Optional: per-family detector fingerprints, so a report stays valid across releases whose family code is unchanged.
- Optional: a `pg_sage bench run --dsn` CLI for installs without a running sidecar.
