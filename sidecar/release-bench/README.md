# Release bench reports

CI (the `bench-sign` job in `.github/workflows/ci.yml`) places this
commit's signed PGIncidentBench reports here before building the image:
one `pgincidentbench.json` per shard (`core/`, `reactive/`, `runway/`),
each with its Sigstore bundle `pgincidentbench.json.sigstore.json`.

The Dockerfile copies this directory to `/usr/share/pg_sage/bench`, where
the sidecar ingests the reports at startup and hourly as earned-autonomy
evidence after verifying their signatures offline. A local `docker build`
ships no report (only this file); point `sre.autonomy.bench_results_path`
at a report instead, or use "Run bench locally".

Verify a downloaded release report by hand:

```bash
pg_sage bench verify --commit <release commit> pgincidentbench-core.json
# or, with cosign:
cosign verify-blob --bundle pgincidentbench-core.json.sigstore.json \
  --certificate-identity https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/tags/v<version> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  pgincidentbench-core.json
```

(`pg_sage bench verify` expects the bundle next to the report as
`<report>.sigstore.json`; rename the release asset accordingly.)
