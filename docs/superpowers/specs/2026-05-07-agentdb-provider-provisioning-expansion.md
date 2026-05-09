# Agent DB Provider Provisioning Expansion Spec

## Goal

Expand Agent DB deployment from local schema provisioning into a provider-aware
provisioning control plane for agent-created databases. pg_sage should track,
tune, cost, back up, and clean up databases created for agents across local
Postgres, AWS RDS, GCP Cloud SQL, and Databricks Lakebase.

## Scope

- Provisioning levels: local Postgres supports `schema` and `database`; cloud
  providers support `instance` only.
- Providers: `local_postgres`, `aws_rds`, `gcp_cloudsql`,
  `databricks_lakebase`.
- Custom t-shirt size profiles for every provider and level.
- UI support for creating profiles and viewing agent-provisioned databases.
- API support so agents can request deployments, publish pings, and consume
  query tuning recommendations.
- Lakebase is first-class in product language, profile metadata, and provider
  command planning.

## Provisioning Model

`schema` provisions an isolated namespace inside an existing local Postgres
database.

`database` provisions a database inside an existing local Postgres instance with
a real `CREATE DATABASE`.

`instance` provisions a new service boundary. For RDS and Cloud SQL this maps to
managed instances. For Lakebase this maps to a Lakebase project/branch or
provisioned database instance depending on selected profile parameters.

Cloud `schema` and `database` levels are intentionally out of scope. Agents
requesting cloud providers must request `instance`.

## Execution Policy

Local Postgres `schema` and `database` provisioning may execute immediately.
Cloud providers generate deterministic instance-level command plans by default.
The plan is stored with the deployment so an operator or future executor can run
it with budget, approval, and account guardrails. This avoids silently creating
paid cloud resources from a UI click.

## Size Profiles

Each size profile stores:

- `provider`
- `provisioning_level`
- `name`
- CPU, memory GB, storage GB, max connections, monthly budget
- provider params JSON for native knobs such as RDS instance class, Cloud SQL
  tier, Lakebase mode, project name, branch name, and region

Defaults are seeded for local schema/database and cloud instance profiles. Users
can create custom profiles in the UI and agents can reference them by
`size_profile_id`.

## API

- `GET /api/v1/agent-dbs/providers`
- `GET /api/v1/agent-dbs/size-profiles`
- `POST /api/v1/agent-dbs/size-profiles`
- `DELETE /api/v1/agent-dbs/size-profiles/{profile_id}`
- `POST /api/v1/agent-dbs` accepts `provider`, `provisioning_level`,
  `size_profile_id`, and `execute`.

## UI

The Agent DBs page shows:

- active and archived agent-provisioned databases
- provider, level, size, and provisioning status
- local/cloud provider readiness
- custom size profile creation
- Lakebase as a first-class provider option

## Verification

- Unit tests for provider command plans and size profile validation.
- Store tests for profile persistence and local database provisioning.
- API tests for provider readiness and profile CRUD.
- UI unit tests for provider/level/profile controls.
- Playwright tests for the provisioning page happy path.
- Integration test that actually creates and drops a local PostgreSQL database.
