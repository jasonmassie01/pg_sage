# Agent Database Deployment Module Plan

## Goal

Build the pg_sage Agent Database Deployment module as an advisory-first DBA
control plane for databases and schemas created by agents. Keep vector, PostGIS,
JSONB, and extension tuning hints in pg_sage. Keep HNSW experiment planning and
full hosted AgentDB memory/runtime surfaces out of scope.

## Slices

- [x] Repair and expand the `agentdb` domain model: requests, deployments,
      leases, backup readiness, cost samples, tuning hints, audit events, and
      recommendations.
- [x] Add policy decisions and idempotency semantics for agent API requests:
      schema allowed, external reviewed, database/branch deferred, invalid
      inputs rejected, conflicting idempotency keys rejected.
- [x] Implement schema deployment support with sanitized schema identity and
      recorded credential scope metadata, without assuming superuser-only role
      creation.
- [x] Add ping, lease, stale detection, cleanup candidate, archive, restore, and
      guarded delete flows. Destructive cleanup must require archive first and a
      verified restore-capable backup when backup policy requires it.
- [x] Publish query recommendations and recommendation feedback through the
      agent API.
- [x] Add budget and cost tracking so deployments can be marked when they cross
      soft or hard budget limits.
- [x] Add first-class tuning packs for vector, PostGIS, JSONB, and extension
      configuration hints.
- [x] Add REST handlers for the spec endpoints plus read APIs needed by the UI.
- [x] Add an Agent DB UI page with request queue, provisioning form,
      deployments, lifecycle actions, cost, backup status, and tuning hints.
- [x] Verify with Go unit and integration tests, web unit tests, lint/build,
      Playwright e2e, and browser checks where available.

## Verification Targets

- `go test ./internal/agentdb ./internal/api -count=1 -timeout 300s`
- `go test -cover -count=1 -p 1 ./...`
- `npm test`, `npm run lint`, and `npm run build` in `sidecar/web`
- `npm run test:e2e` in `sidecar/web`
- Report skipped tests, coverage gaps, and blocked manual/browser checks.
