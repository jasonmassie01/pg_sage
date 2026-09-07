# Agent DB Provider Provisioning Expansion Plan

## Implementation Checklist

- [ ] Extend Agent DB domain types with provider, level, size profile, command
  plan, connection metadata, and provider readiness types.
- [ ] Add schema migrations and default size profile seeding.
- [ ] Add profile CRUD store methods.
- [ ] Add provider command planners for cloud instance provisioning on RDS,
  Cloud SQL, Lakebase, and local Postgres schema/database execution.
- [ ] Add local database provisioning with identifier sanitization and metadata.
- [ ] Expand API handlers for provider readiness and size profile CRUD.
- [ ] Expand UI with provider/level selectors, profile management, readiness,
  and deployment metadata.
- [ ] Add unit, integration, API, UI, and browser tests.
- [ ] Run full verification and document test coverage.
