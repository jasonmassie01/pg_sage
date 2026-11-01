# Native Databricks Apps operator guide

pg_sage runs its existing Go HTTP server and embedded React dashboard as a
Databricks App. Start with the [Databricks-native guide](databricks-native.md)
for the integration overview and the [Lakebase support matrix](lakebase.md)
for feature-specific permissions, limitations and test evidence. This page
covers packaging, installation, updates and recovery; it does not convert the
application to AppKit or require a custom runtime container.

The current recorded release is **2.5.0-dev.10**. Its
[acceptance report](../reviews/2026-10-30-v2.5-dev10-acceptance.md) records native
CGO/libpg_query packaging, an immutable App snapshot, unchanged resource/scope
bindings, Databricks-only sign-in, M2M AI, connected collection and desktop/mobile
checks. The App remains in Observation mode with metadata-only telemetry and no
approved/executed database change. Acceptance is scoped to that deployment;
it does not certify every Lakebase extension, cloud, privilege or long-duration
scenario. Older release evidence remains linked from the matrix and reports.

Follow the sections in order for a new installation. For an existing App, read
[resource preservation](#keep-all-resources-and-identity-settings) and
[update/rollback](#update-restart-and-roll-back) before changing its package.

## What runs where

The Apps process serves the dashboard and REST API on `0.0.0.0` and
`DATABRICKS_APP_PORT`. When that environment variable is present, pg_sage accepts
HTTP/1.1 and HTTP/2 cleartext (H2C), as required by the Apps proxy. Apps handles
external TLS and login. The package explicitly sets
`SAGE_AUTH_MODE=databricks_app`: protected requests use the Apps proxy's
`X-Forwarded-Email` identity, with no separate pg_sage password or session cookie.
pg_sage still enforces its local roles and action approvals. Configure
`SAGE_DATABRICKS_ADMIN_EMAIL` for the reviewed administrator; newly provisioned
platform users start as viewers, and existing platform-bound users retain their
pg_sage roles. Other local accounts require a reviewed identity migration;
matching an email does not automatically link them. Visiting first does not
grant administration.

This mode is restricted to the Apps runtime and requires its injected port and
`DATABRICKS_HOST` (a bare hostname is normalized to HTTPS). Missing platform
identity fails closed. Do not expose
the native listener through another proxy that lets clients supply identity
headers. The [public HTTP header reference](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/http-headers)
documents the forwarded IdP email. The public reference does not specify a header-signature verification procedure or promise that
every forwarded identity field is immutable. pg_sage does not infer roles from
client-supplied role headers or assume the forwarded user ID equals a workspace
SCIM ID. App service-principal authentication for background Lakebase/Gateway
operations remains separate from the current viewer's identity.

In Apps mode the outbound SDK is pinned to `oauth-m2m`, using the platform's
injected workspace, client ID and client secret. The launcher and native SDK
adapter reject PAT/basic/profile overrides instead of trying another identity.
No Unity Gateway API key is needed. AI calls use the shared App principal,
not the signed-in user's on-behalf-of permissions. General/advisor, optimizer
and SRE clients must use native Databricks auth without API keys and remain on
the exact configured HTTPS workspace origin, including its port. A dedicated
optimizer endpoint in that workspace retains OAuth. Invalid YAML, environment,
persisted/API or request-scoped discovery settings are refused. Standalone
provider choices remain available outside Apps.

`/api/v1/llm/status` reports each client's auth mode and endpoint origin, plus
safe SDK provider/workspace/principal metadata. `authentication_observed` means
the adapter previously authenticated a workspace HTTP request; it does not
promise that a cached token is currently valid. Credentials and token expiry
values are not exposed. The cached workspace/provider configuration requires
a process restart to change.

Administrators can retrieve `/api/v1/deployment/status` for an allowlisted
snapshot of the active global AI/SRE/source/action configuration after persisted
overrides. It includes configuration generation and safe flags, never the
configuration's secrets, DSNs or raw provider payloads. The response explicitly
excludes per-database overrides and runtime liveness; those require separate
runtime checks. `/api/v1/lakebase-observability/status` reports the telemetry
runtime's sanitized state to authenticated viewers without returning events.

Optional MCP, specialist, legacy signed SLI/change-event ingestion and local token
issuance return `501 databricks_app_protocol_unsupported` after platform
authentication. The separate administrator SLI sample route described below
uses ordinary platform authentication; it does not enable signed machine
ingestion. The normal user REST API remains available. The existing
agent transports depend on verified agent/task/database grants; signed ingest
depends on a source credential. An Apps email alone cannot preserve those
contracts. A supported service-principal identity contract and explicit stored
grant mapping are prerequisites to enabling these transports without another
login secret. Caller-supplied headers/body fields must not supply that identity.
Their existing standalone authentication and authorization remain unchanged.
The App dashboard hides MCP-token and agent-governance navigation; direct links
explain their unavailable status without calling those protocols. This does not
remove the backend's existing authorization or explicit unsupported responses.

PostgreSQL stores pg_sage's metadata, accounts, sessions, findings, and history.
The binary and generated listen-address config live in ephemeral temporary
storage; they can be recreated from the deployment package. The encryption key
must remain stable across restarts and deployments: supply it from an existing
Databricks secret resource. No database password or workspace token belongs in
the package.

Use a **dedicated Lakebase test branch/database** for the first deployment.
The app service principal needs an OAuth PostgreSQL role and database/schema
permissions. The bundle template attaches the current `postgres` resource with
`CAN_CONNECT_AND_CREATE`; check additional permissions needed for monitoring,
statistics and optional features. Let the app principal create and own the
`sage` schema on its first run. Existing user-owned `sage` objects require a
reviewed ownership/access migration; do not drop them to fix an access error.
Database-level configuration actions additionally require database ownership
and a supported, user-settable parameter; attaching the resource alone does
not establish those capabilities. Transfer ownership only for the dedicated
pg_sage database when that behavior is wanted. An App principal without
`CREATEROLE` runs agent governance in posture-only mode; monitoring and the
native Gateway integration do not require granting that privilege.

The launcher maps `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, and `PGSSLMODE` to
pg_sage's existing connection settings. `PGUSER` defaults to the injected
`DATABRICKS_CLIENT_ID`. `SAGE_LAKEBASE_ENDPOINT` is the full endpoint resource
path (`projects/.../branches/.../endpoints/...`); `LAKEBASE_ENDPOINT` and
`ENDPOINT_NAME` are accepted launcher aliases. Native Go unified authentication
uses the app's injected service principal credentials, and obtains Lakebase
credentials for new physical connections. The native pool caches a valid
database credential and remints when fewer than two minutes remain before its
reported expiry; no password is retained in the pool's bootstrap configuration.
The endpoint API's direct host and TLS binding are checked before connecting;
pooled endpoint hosts are refused. Existing connections can outlive a token,
so successful pooled queries alone do not establish refresh. Local runs can use an explicitly
selected Databricks CLI profile instead. A single `SAGE_LAKEBASE_ENDPOINT` applies
to every pool in the process, including a separate metadata database. For
different endpoint hosts, use the native `SAGE_LAKEBASE_ENDPOINTS` hostname-to-
resource JSON map in a custom deployment instead of the single-endpoint launcher.

## Establish the operator inputs

The command recipes in this guide use Bash from the repository root and
Databricks CLI **1.20.0**. Check the relevant `--help` with another CLI version.
Use Python 3, a running Docker/Buildx installation and an authenticated
Databricks CLI locally. The Dockerfile supplies Node 22 and Go 1.27 plus the C
compiler; local backend tests additionally need the repository's Go/C toolchain
and their documented fixture dependencies. The target App requires access to
its workspace API, direct Lakebase TLS endpoint and selected AI route; optional
history also needs the selected SQL warehouse and its Unity Catalog grants.

```bash
databricks --version
databricks auth profiles
read -r -p 'Explicitly selected profile: ' PG_SAGE_PROFILE
read -r -p 'Absolute reviewed settings JSON path: ' PG_SAGE_SETTINGS
read -r -p 'Absolute fresh local package directory: ' PG_SAGE_PACKAGE
PG_SAGE_APP="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["app_name"])' "$PG_SAGE_SETTINGS")"
```

Create the settings file from
[`settings.example.json`](../deploy/databricks-apps/settings.example.json), edit
it outside version control, and fill actual resource references before running
that block. No value in angle brackets is a deployable identifier. A fresh
package directory is required: `package.py` refuses an existing output directory.
Use the same explicit profile for every command in a release.

| Setting | Required meaning |
| --- | --- |
| `app_name` | Reviewed workspace-unique App name; renderer accepts a lowercase initial letter and at most 26 lowercase letters, digits or hyphens. |
| `branch` | Full Autoscaling branch resource path. Use an isolated development branch for first installation. |
| `endpoint` | Direct endpoint resource path belonging to that branch. |
| `host` | Exact direct connection hostname returned for that endpoint; no scheme, port or credentials. Pooled endpoint hostnames are refused. |
| `database_resource` | Full branch database resource path, including its API database ID. |
| `database_name` | Actual PostgreSQL database name; it need not equal the API resource ID. |
| `secret_scope`, `secret_key` | Existing reference holding the stable encryption passphrase; never the passphrase itself. |
| `databricks_admin_email` | Explicit administrator's Databricks identity; not a first-visitor admin rule. |
| `pg_user` | Optional reviewed connecting PostgreSQL role; defaults to the platform resource/App principal identity. |
| `gateway_endpoint`, `gateway_model` | Supply both for native AI; HTTPS workspace-origin base URL and fully qualified model service. |
| `gateway_json_mode` | Explicit boolean supported by the chosen model; omitted package default is false. |
| `lakebase_observability` | Optional bounded settings; enabled history needs an explicitly selected `warehouse_id`. Payload and compute consent are independent. |

Read resource metadata with the full names from that reviewed file using
`databricks postgres get-endpoint NAME --profile "$PG_SAGE_PROFILE" -o json`
and `databricks postgres get-database NAME --profile "$PG_SAGE_PROFILE" -o json`.
Here `NAME` means the exact corresponding resource path, not its short label.
Do not mint a database token merely to copy it into a file.

## Build and package

Apps runs Ubuntu 22.04; it does not provide the Go/C build toolchain. Build a
statically linked Linux executable ahead of deployment, retaining CGO and
`libpg_query` SQL AST validation. Do not use a macOS binary or disable CGO to
cross-compile. The dedicated Dockerfile is a build tool, not an Apps custom
container image.

Run the build from the repository root with Docker running and
`PG_SAGE_PACKAGE` set to a fresh absolute output directory from the operator
inputs above. Keep a known-good package before replacing any build output:

```bash
PG_SAGE_BUILD_COMMIT="$(git rev-parse --short HEAD)"
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  PG_SAGE_BUILD_COMMIT="${PG_SAGE_BUILD_COMMIT}+local"
fi
docker buildx build --platform linux/amd64 \
  -f deploy/databricks-apps/Dockerfile \
  --build-arg COMMIT="$PG_SAGE_BUILD_COMMIT" \
  --target artifact --output type=local,dest=dist/apps-linux .
python3 deploy/databricks-apps/package.py \
  --binary dist/apps-linux/pg_sage --output "$PG_SAGE_PACKAGE"
```

The builder reads the repository `VERSION`, embeds the supplied commit and UTC
build date, runs `--version`, and checks `sql-ast: libpg_query`. Optional `VERSION`
and `BUILD_DATE` build arguments override those defaults. Verify the target
workspace runtime architecture before deploying; the public Apps environment
page does not promise a CPU architecture. Build with `--platform linux/arm64`
if needed. The launcher refuses an architecture mismatch.

Apps limits each uploaded file to 10 MB. The packager compresses the native
binary into 8 MB chunks, records its size/SHA-256, and copies a small Python
standard-library launcher. At startup the launcher verifies and reassembles the
binary locally and makes it executable. A small supervisor relaunches the Go
process only for its intentional restart exit code (42), including dashboard
settings that require a restart. Ordinary failures remain visible as failures;
more than five restart requests in a minute stop a restart loop. SIGTERM/SIGINT
reach the entire child process group, with a 12-second shutdown budget inside
Apps' 15-second platform allowance. This avoids a startup download and does not require
network access to GitHub or a package registry. The checksum checks package
integrity; normal repository/artifact provenance is still required.

## Configure and validate

Copy `deploy/databricks-apps/settings.example.json` to a local settings file and
replace its placeholders with the exact isolated branch, endpoint hostname,
database identifiers, and existing secret references. The database resource ID
can differ from its PostgreSQL database name; obtain both from branch metadata.
The settings file contains resource references only, never secret values.

```bash
python3 deploy/databricks-apps/configure.py \
  --package "$PG_SAGE_PACKAGE" --settings "$PG_SAGE_SETTINGS"
```

The renderer writes equivalent runtime and bundle configuration, sets
observation trust, and attaches the PostgreSQL resource and encryption secret.
Runtime `app.yaml` uses `valueFrom`; bundle `config.env` uses `value_from`. The
current CLI ignores camelCase in the bundle block, so the renderer preserves
the schema-specific spelling. JSON output is valid YAML.

Set `databricks_admin_email` to the explicitly selected administrator's platform
email. The renderer emits `SAGE_AUTH_MODE=databricks_app` and
`SAGE_DATABRICKS_ADMIN_EMAIL`; no bootstrap password secret is required or
injected. The native server binds that administrator explicitly, preserving the
existing account where appropriate, and disables password/session authentication
in this mode. The launcher rejects stale `SAGE_BOOTSTRAP_ADMIN_*` settings so an
old deployment cannot silently retain the separate password flow. Standalone
pg_sage deployments retain their existing password/OAuth authentication.

For Unity AI Gateway, supply the actual OpenAI-compatible `gateway_endpoint`
(`https://<workspace>/ai-gateway/mlflow/v1`) and fully qualified `gateway_model`.
The renderer adds native Databricks authentication and a `MODEL_SERVICE` resource
with `EXECUTE` permission. Set optional `gateway_json_mode` to the boolean
supported by that model. The tested `system.ai.gpt-6-sol` route supports `true`;
some Claude routes reject `response_format` and require `false`. The package
default is `false` until explicitly configured. Test the selected model's
structured output rather than inferring support from another model. Other
supported pg_sage YAML keys can be represented in `config.json`; the launcher
sets the API bind address and validates its platform authentication/resource
bindings.

The optional `lakebase_observability` settings object is off by default and
defaults to metadata-only reads (`allow_payload: false`). To enable it, supply
`enabled: true` and an explicitly selected existing `warehouse_id`. The renderer
derives its sole host/database/endpoint binding from the reviewed deployment
settings, keeps bounded query/row/byte/time/retention settings, and declares a
`lakebase-history-warehouse` resource with `CAN_USE`, preserving the other
resources. The warehouse ID is injected through that resource; a conflicting
configured ID is refused. This consumes warehouse capacity and adds that
resource permission when applied. It does not grant Unity Catalog access or
PII-group membership. Review existing inherited UC exposure separately.

Default streams are plans, logs, ASH, DDL and database counters/gauges. Compute
streams additionally require explicit `allow_compute: true`; a database binding
does not authorize compute-wide history. Raw payload reads remain a separate
reviewed `allow_payload` choice. Source delivery is asynchronous, so all history
remains diagnostic evidence rather than a live maintenance admission source.
Raw-payload retention cannot exceed seven days. Generic `config.json` remains
available for reviewed native configuration options beyond this bounded renderer.

## Install or update an App

Use the same explicit workspace profile and rendered settings throughout an
installation or update. The commands below apply resources and deploy code;
local building, rendering and tests can be completed separately. No runtime
`pip`/`npm` installation is required.

### Keep all resources and identity settings

The generated bundle defines the bindings below. On an **existing** App these
are a required subset, not permission to replace its complete resource list.

| Resource key | Shape and permission | Additional boundary |
| --- | --- | --- |
| `postgres` | `postgres` with `branch`, full `database` resource path, `CAN_CONNECT_AND_CREATE` | Creates the App principal's database connection role/CONNECT/CREATE access. Does not grant workload table ownership, every monitoring capability or execution authority. |
| `sage-encryption-key` | `secret` with existing `scope`, `key`, `READ` | Keep the same secret value for existing encrypted metadata. Secret rotation is a separate key-rotation procedure. |
| `gateway-model` | `uc_securable` with `MODEL_SERVICE`, full model name, `EXECUTE` | Declared when Gateway is configured. This is the tested model-service binding, not a substitution with an unrelated serving endpoint. |
| `lakebase-history-warehouse` | `sql_warehouse` with selected `id`, `CAN_USE` | Declared only when retained history is enabled. UC access and payload/compute authorization remain separate. |

App access permissions, the App service principal, `user_api_scopes`, all other
resource bindings, and any reviewed per-environment configuration must survive
a code-only update. App resources are collections: a resources-field update
replaces the **whole list**. Removing a Lakebase resource can reassign owned
objects and remove the principal's PostgreSQL role; changing `database` to
`postgres` is not a harmless rename. Do neither as an upgrade workaround.

For a new App, create it without compute so its principal and resources can be
prepared before the first server start:

```bash
databricks apps create "$PG_SAGE_APP" --no-compute \
  --profile "$PG_SAGE_PROFILE" -o json
```

Attach the reviewed resources through the Apps UI, or use the current
`apps create-update` API/CLI surface after inspecting the generated resource
shapes. Let the principal own a fresh `sage` schema; an existing schema's
ownership migration requires a separate reviewed plan. Keep end-user App access
and PostgreSQL privileges scoped to the deployment's purpose.

For either installation or update, capture the App metadata privately:

```bash
read -r -p 'Absolute private release-evidence directory: ' PG_SAGE_EVIDENCE
mkdir -p "$PG_SAGE_EVIDENCE"
databricks apps get "$PG_SAGE_APP" --profile "$PG_SAGE_PROFILE" -o json \
  > "$PG_SAGE_EVIDENCE/app-before.json"
databricks apps list-deployments "$PG_SAGE_APP" --limit 20 \
  --profile "$PG_SAGE_PROFILE" -o json \
  > "$PG_SAGE_EVIDENCE/deployments-before.json"
```

If a resource change is actually required, prepare and review a JSON request
with the **complete merged** resource list. CLI 1.20.0 documents positional
`APP_NAME UPDATE_MASK`; its request body contains `app` and `update_mask`:

```bash
# Only after reviewing this separately prepared complete request:
databricks apps create-update "$PG_SAGE_APP" resources \
  --json "@$PG_SAGE_EVIDENCE/reviewed-resource-update.json" \
  --profile "$PG_SAGE_PROFILE"
```

The JSON shape is `{"update_mask":"resources","app":{"name":"actual-app-name","resources":[...]}}`;
the ellipsis is explanatory, never a literal request. Preserve every current
binding when constructing that array. Leave unrelated fields outside the mask;
never use `*`. Re-read the App afterward and compare complete resources and user
API scopes. A regular code-only update skips this resource-change step entirely.

### Validate the native package

Use the [build and package](#build-and-package) recipe with the chosen fresh
output, then render the reviewed settings:

```bash
python3 deploy/databricks-apps/configure.py \
  --package "$PG_SAGE_PACKAGE" --settings "$PG_SAGE_SETTINGS"
python3 -m unittest discover -s deploy/databricks-apps -p 'test_*.py' -v
```

Run the repository's backend/frontend release checks as well. Packaging tests
cover ELF/static-link/architecture validation, chunk integrity, runtime config,
OAuth conflict refusal, resource rendering and supervisor signals/restarts;
they do not connect to the chosen Lakebase branch or certify its grants.

```bash
(
  cd "$PG_SAGE_PACKAGE"
  databricks bundle validate --strict --profile "$PG_SAGE_PROFILE"
)
```

CLI 1.20.0's bundle schema omits `MODEL_SERVICE` from
`uc_securable.securable_type`, although the deployed Apps API accepted that
resource with `EXECUTE` in the recorded acceptance. A Gateway package therefore
fails strict validation with that specific enum warning. Retain the correct
resource, run non-strict `databricks bundle validate`, and establish that this
is the only warning before using the direct App deployment path below. Record
this as a schema discrepancy, **not** a strict-validation pass. Stop for other
validation errors; future CLI versions may remove this exception.

CLI 1.20.0 `databricks apps validate` recognizes Node projects and rejects this
native Go/Python-launcher package. Do not add a dummy Node project to bypass it.
`apps deploy` without an App name uses the enhanced bundle workflow; when the
CLI fully supports all chosen resource types, `--skip-validation` bypasses only
that unsuitable project check. Bundle deployment can be followed by
`databricks bundle run pg_sage` when a two-phase principal/grants setup is needed.
Keep that alternative separate from the already-configured direct snapshot path.

### Upload reviewed runtime files and deploy a snapshot

Make a runtime-only stage. The manifest decides the chunk count; dev10 had
seven runtime files, but seven is not a universal package requirement. Keep the
bundle, settings, source, evidence, environment dumps and `.databricks` state out
of this stage.

```bash
PG_SAGE_STAGE="$(mktemp -d "${TMPDIR:-/tmp}/pg-sage-upload.XXXXXX")"
python3 - "$PG_SAGE_PACKAGE" "$PG_SAGE_STAGE" <<'PY'
import hashlib, json, pathlib, shutil, sys
package, stage = map(pathlib.Path, sys.argv[1:])
manifest = json.loads((package / 'binary.json').read_text())
names = ['app.yaml', 'binary.json', 'config.json', 'launch.py', *manifest['chunks']]
assert len(names) == len(set(names)), 'duplicate runtime filename'
fingerprints = {}
for name in names:
    assert pathlib.Path(name).name == name, 'runtime path must be a filename'
    data = (package / name).read_bytes()
    assert len(data) <= 10_000_000, 'runtime file exceeds conservative 10 MB bound'
    shutil.copyfile(package / name, stage / name)
    fingerprints[name] = hashlib.sha256(data).hexdigest()
(stage.parent / (stage.name + '.sha256.json')).write_text(json.dumps(fingerprints, indent=2) + '\n')
print('Staged', len(names), 'runtime files; fingerprints are beside the stage.')
PY
read -r -p 'Fresh absolute /Workspace source directory: ' PG_SAGE_SOURCE
databricks workspace mkdirs "$PG_SAGE_SOURCE" --profile "$PG_SAGE_PROFILE"
for PG_SAGE_FILE in "$PG_SAGE_STAGE"/*; do
  databricks workspace import "$PG_SAGE_SOURCE/$(basename "$PG_SAGE_FILE")" \
    --file "$PG_SAGE_FILE" --format RAW --profile "$PG_SAGE_PROFILE"
done
databricks apps deploy "$PG_SAGE_APP" --source-code-path "$PG_SAGE_SOURCE" \
  --mode SNAPSHOT --profile "$PG_SAGE_PROFILE" -o json \
  > "$PG_SAGE_EVIDENCE/deployment.json"
```

Use a fresh workspace directory and omit `--overwrite` so a reused filename
fails. `RAW` preserves `launch.py` as a file; generic notebook import can change
Python filenames. An explicit App name selects direct deployment even from a
bundle directory. This deploys code/runtime configuration while preserving the
App's existing resource bindings. `SNAPSHOT` freezes deployed code; it does not
snapshot the database, grants, secret value or persisted pg_sage settings.

### Verify the deployed artifact and service

```bash
PG_SAGE_DEPLOYMENT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["deployment_id"])' "$PG_SAGE_EVIDENCE/deployment.json")"
databricks apps get-deployment "$PG_SAGE_APP" "$PG_SAGE_DEPLOYMENT" \
  --profile "$PG_SAGE_PROFILE" -o json \
  > "$PG_SAGE_EVIDENCE/deployment-verified.json"
databricks apps get "$PG_SAGE_APP" --profile "$PG_SAGE_PROFILE" -o json \
  > "$PG_SAGE_EVIDENCE/app-after.json"
databricks apps logs "$PG_SAGE_APP" --tail-lines 200 \
  --profile "$PG_SAGE_PROFILE"
```

Require deployment status `SUCCEEDED`, an actually running App, and successful
proxy readiness; these are separate signals. Read the App URL from `apps get`.
Compare all resources and `user_api_scopes` with `app-before.json`; compare the
App principal as well. The deployment response's
`deployment_artifacts.source_code_path` identifies the snapshotted code actually
loaded. Its outer `source_code_path` is the original upload source and is not
a proof of immutable contents.

For stronger provenance, export each runtime filename from that actual artifact
path with `databricks workspace export SOURCE_PATH --format RAW --file LOCAL_PATH
--profile "$PG_SAGE_PROFILE"` and compare SHA-256 with the private stage
fingerprints. Do not place the fingerprint file in the runtime stage. Record
`VERSION`, the embedded commit/dirty status, binary manifest SHA-256, runtime-file
hashes, configuration hash, deployment ID and test results. A base Git commit
alone does not identify a cumulative dirty working tree.

Use an ordinary authenticated Databricks browser for the
[live acceptance checks](#live-acceptance-checks). Protected API requests use
that platform session, not a copied token or a manufactured forwarded header.
No approval or database change is needed for release smoke checks. Inspect
`/health`, `/ready`, `/api/v1/auth/me`, `/api/v1/databases`,
`/api/v1/llm/status`, the administrator-only `/api/v1/deployment/status`, and the
telemetry status route. Check the intended database and actual collection health,
Observation mode, AI origin/auth mode, automatic-start/action flags and payload
consent. Check Cases, Actions, Ask Sage and Fleet on desktop/mobile. A manual AI
probe incurs model usage and requires its own operational authorization; a
readiness check alone does not establish model inference.

### Update, restart and roll back

For a code-only update, retain the existing App, principal, resource set,
secret, database and grants. Save the current known-good package and evidence,
build into fresh local/workspace directories, deploy another `SNAPSHOT`, then
repeat the artifact and service checks. Rendering a fresh template is not a
replacement for the deployed effective configuration: persisted pg_sage API
settings and per-database overrides must be reviewed separately.

For a restart of unchanged code, the dashboard's intentional restart exits 42
and the launcher restarts the child; an Apps stop/start recreates ephemeral
files. A reviewed full compute restart uses `databricks apps stop APP_NAME`
followed by `databricks apps start APP_NAME`, with the selected profile on both.
These interrupt service; they are not read-only verification steps. Changing
workspace credentials, endpoint mapping or SDK auth configuration requires a
process restart. Egress-policy changes may require an Apps restart as described
in the platform documentation.

A code rollback redeploys the retained, hash-verified previous runtime package
as a **new** `SNAPSHOT` to the same App, following the upload/deploy/check sequence.
Do not assume `--deployment-id` means "restore this old deployment"; it identifies
a deployment request, not a documented database rollback. Preserve all current
bindings/scopes and the stable encryption secret. Confirm that the older binary
supports the current metadata schema and configuration before running it:
startup migrations are not reversed by redeploying code.

If compatibility is uncertain, stop and plan recovery with the database owner.
An Apps code snapshot is not a database backup. Lakebase branch/PITR restore,
secret rotation and reverting an approved workload change are separate recovery
operations with different owners and effects. Do not drop the `sage` schema,
delete/recreate the App or detach its database to make an old binary start.

## Logs and troubleshooting

The launcher/Go server write to stdout/stderr, which Apps captures. Use
`databricks apps logs "$PG_SAGE_APP" --follow --tail-lines 200 --profile
"$PG_SAGE_PROFILE"` for an active investigation; stop following when finished.
Logs require OAuth-capable operator CLI authentication. Keep release evidence
private and sanitize it before sharing; never dump the App environment,
Authorization headers, OAuth responses, secret values or password-bearing DSNs.
An App RUNNING state alone does not establish useful collection or AI access.

| Symptom | Check and supported response |
| --- | --- |
| 302/login for an anonymous protected request | Expected platform ingress gate. Authenticate through Databricks; do not bypass it with identity headers. |
| Missing identity, local login form, or a separate session seems required | Confirm `SAGE_AUTH_MODE=databricks_app`, the runtime port/workspace inputs and proxy access path; remove stale bootstrap/password configuration. Check the intended explicit admin binding. |
| 403 after platform sign-in | Separate App access from pg_sage role and resource/database permissions. An email or App `CAN_USE` is not an administrator grant. |
| App stopped, startup failure or 502 | Inspect system/App logs; confirm `python launch.py`, architecture/chunk checks, `0.0.0.0:$DATABRICKS_APP_PORT`, H2C and proxy TLS termination. Check missing secret/resource injection before changing code. |
| Architecture or dynamic-interpreter rejection | Rebuild the static **Linux** ELF for the runtime architecture with CGO/libpg_query retained. A local macOS or dynamically linked binary is not compatible. |
| Package checksum/size mismatch | Re-stage and re-upload the complete reviewed manifest/chunks; do not disable the verification or mix release files. |
| `app OAuth requires removing ...` or launcher rejects credentials | Remove conflicting PAT/basic/profile/API-key/bootstrap variables from the App deployment. Keep platform `oauth-m2m`; do not add a fallback identity. |
| Lakebase host mismatch/pooled-endpoint/TLS refusal | Compare the exact endpoint resource with the API's direct hostname and `verify-full`. Do not substitute a pooled host, IP address or disabled TLS. |
| Refresh/reconnect failure after initial success | Check App principal permissions, current endpoint state, workspace reachability and expiry-aware fresh physical connections. Existing open sessions do not test renewal. Never paste a new token into the package. |
| `sage` migration/ownership error | Inspect the selected database and object owners. Arrange a reviewed ownership/access migration; resource attachment does not transfer arbitrary pre-existing schema ownership. |
| AI 401/403 or structured-output rejection | Check each general/optimizer/SRE client's origin, M2M metadata, model `EXECUTE` grant and model-specific JSON mode. Do not route to a different origin or supply an API key. |
| History empty/unavailable | Check warehouse binding/`CAN_USE`, UC privileges, source bindings, delivery delay, retention and payload/compute consent. Empty retained history is not proof of no incidents or a healthy live denominator. |
| Cases show blocked/unknown recovery or no Actions history | In Observation mode this is expected for changes. Do not raise trust, enable execution or approve work to make a deployment smoke test look active. |
| Baseline completed but index changes remain blocked | Disk-load baseline is one safety condition. Mode, provider applicability, permissions and approval gates still apply. |
| Encrypted metadata no longer decrypts | Confirm the original encryption secret reference/value. Follow [key rotation](security.md#secrets-at-rest); do not overwrite the database or generate a replacement key as a restart fix. |
| Repeated restart exit 42 | Launcher stops after more than five requests in 60 seconds. Inspect the restart-requiring setting and logs rather than masking a restart loop. |
| Request times out at the platform | Check current proxy limits and use the existing asynchronous investigation/status flow. Do not assume a client timeout proves that a submitted operation did not run. |

## Retained SRE history

With `lakebase_observability` configured, the investigator's `read_history`
tool can consume `lakebase_plan_history`, `lakebase_log_history`,
`lakebase_session_history`, `lakebase_ddl_history` and
`lakebase_database_history`. These fixed adapters read retained observations
under the current verified binding and owner policy, and the investigation's
pinned database policy. An explicit `sre.read_policy.allowed_sources` list must
include each requested source. The tool is exposed only when its investigation
plan and policy allow history; it does not expand the App principal's warehouse
or UC permissions.

Reads retain original event/receipt times and source identities, enforce
retention, and return at most 100 structured observations in a 60-second to
seven-day window (24 hours by default). Raw SQL, plan JSON, log messages, actor
identities and object names are excluded from durable SRE history evidence.
Historical RCA classifications and DDL changes add citable context without
asserting a live cause, current incident, exact event count, customer impact or
complete SLO denominator. Empty history means completeness is unknown.
See [Lakebase history](lakebase.md#advanced-telemetry-auto_explain-and-sre-history) for
source details and the separate controlled native delivery evidence.
On final dev.5 SNAPSHOT `01f1d48e0748107d95b5bdd1ffad0e22`, two real-Gateway
investigations executed and cited all five adapters with positive observations.
Plan/log case `e0f50f43-5e45-46d0-9701-ae1f8187a53d` retrieved the exact
controlled provider plan event, with four model calls/four tools/14,816 tokens
and a verified 14-event chain. Case `ea555ad2-3ca6-48a3-bbcc-69d48f18ed9c`
read ASH (100 observations), DDL (one) and database metrics (100), with three
calls/four tools/13,563 tokens and a verified 17-event chain. The cited DDL evidence
contains the exact controlled table-creation event also persisted by the actual
scheduler and returned through the authenticated history API. Its unique owned
schema/table were removed with identity checks; no trust, grant or payload
configuration changed. Both cases passed idempotent readback, accepted evidence
hashes/citations and eight unchanged action/approval ledgers.

The advisory outcomes were inconclusive; no database cause or remediation was
established. `allow_payload: false` remained active. This accepts structured
retained history and its citations; App raw-plan-cache ingestion and complete
telemetry remain unproven. The [dev.5 report](../reviews/2026-10-30-v2.5-dev5-acceptance.md#final-app-history-and-slo-evidence)
records artifact/event identities and preserved transport/cleanup failures.

## Application SLI ingestion

An administrator authenticated through Databricks Apps can submit cumulative
application counters to:

```text
POST /api/v1/sre/slos/{name}/samples?database=<registered-runtime-name>
```

Configure the objective at startup in `sre.slo.objectives`, with the exact
`name`, explicit `database`, `source: push`, `target`, `window_days`,
`min_eligible_events` and `stale_after_seconds`. Enable `sre.slo.enabled`;
`sre.slo.push.hmac_secret` can remain empty in Apps mode. The route does not
create or edit objectives. To start a read-only investigation on a page burn,
also enable `sre.slo.open_investigations` and allow `application_slo_status`
under any explicit read allowlist. This registered application signal can open
a `slo_burn` case while `sre.automatic_start` is false. Keep notification and
action configuration independently controlled.

The body accepts only `series`, `bad`, `eligible` and `observed_at`. Counters
must be finite and nonnegative with `bad <= eligible`; `observed_at` is a
current RFC3339 timestamp, at most ten minutes old or one minute ahead. The
server binds the series to the authenticated, persisted pg_sage administrator
and the registered database identity. Client-supplied actor, deployment or
database identity fields are refused. Same-origin mutation protection remains
in force. A new sample returns 202, an identical same-series/time replay
returns 200 without another row, and different counters at that identity
return 409. A missing database selector is invalid; ambiguous or rebound
database identity is refused.

These must be real cumulative bad/eligible counters for the same population
and time. Use stable series whose event populations do not overlap. Reposting
one population through multiple administrator identities creates distinct
series and can double count it. Counter decreases are resets. Both burn
windows need sufficient eligible traffic and at least 90% sample coverage;
no data, partial coverage, low traffic, stale samples or stale evaluations
yield unknown. Sampled provider logs and interval metrics do not meet this
application-denominator contract. Database proxy SLOs describe database
symptoms. This human-admin route does not establish machine-source identity
or support the legacy signed ingestion protocol, which remains 501 in Apps.

Authenticated viewers can read `/api/v1/sre/slos?database=<name>` and
`/api/v1/sre/slos/{name}?database=<name>`. Deployment status exposes SLO
configuration flags/counts; individual status exposes the target and burn
rules. The global config API does not expose every startup SRE field, so
validate the reviewed deployment configuration as well as runtime behavior.
The [dev.5 App SLO acceptance](../reviews/2026-10-30-app-slo-acceptance.md#dev5-post-upgrade-pass)
passed fifteen real-time samples across seven minutes, replay/refusal checks,
both burn windows, a cited real-Gateway case and actual stopped-source staleness
after 123.06 seconds. Eight action/approval ledgers were unchanged and exact
cleanup preserved the case. The final baseline has no application objectives or
synthetic samples/status/transitions. Adding a real source is an explicit
configuration choice. Synthetic counters establish only their scenario, and the
advisory inconclusive case establishes no database cause or remediation.

## Operator statistics maintenance

Dev.4 adds a typed `create_statistics` lifecycle under
`/api/v1/databases/{db}/investigations/{id}/maintenance`. Viewers/operators/admins
can list proposals. Operators/admins can explicitly propose columns, kinds,
rationale and the SHA-256/ID of fresh retained `stat_tables` v3 evidence, then
call `/{proposal}/request` to create a pending human approval. The investigation
must be concluded and the evidence must identify the same physical database
and table. Durable approval, live standing policy, trust and emergency-stop
checks gate execution. Only an admin can request `/{proposal}/rollback`, with
a reason. There is no automatic maintenance proposal/execution/rollback path;
observation trust prevents mutation.

The **App's connecting PostgreSQL role** needs CREATE on the statistics schema
and ownership of the target workload table. Database/schema ownership,
`CAN_CONNECT_AND_CREATE`, or `MAINTAIN` alone does not establish these rights;
the bridge excludes `sage` metadata tables. Evidence and approval expire within
15 minutes. Under a bounded table lock, the typed runner rechecks physical
table/owner/column identities, atomically creates statistics plus ANALYZE, and
verifies the built object's OID. Rollback reauthorizes and removes only that
exact object, preserving a same-name replacement. **It cannot restore prior
ANALYZE statistics.** Structural success establishes neither performance
improvement nor incident recovery and earns no value/autonomy credit.

The [native maintenance lifecycle](../reviews/2026-10-30-sre-maintenance.md)
passed on an owned PG18 fixture as the native user role. App-principal
maintenance execution remains unproven; no broad workload-table grants or
ownership transfer is implied by deployment.

## Live acceptance checks

These full pilot checks run against the actual App URL and Lakebase endpoint.
They include controlled writes, model usage and restart/endurance scenarios;
select and authorize those scenarios separately from code-only release smoke
checks. Record each result and any unproven item explicitly:

- The app reaches RUNNING, `/health` responds through the Apps proxy, and
  `/ready` verifies its control database and migrated schema. `/health` is
  liveness only; `/ready` does not verify an admin account. With an authenticated
  Databricks browser session, the dashboard must load directly without a
  pg_sage password form or `sage_session` cookie. `/api/v1/auth/me` must identify the
  intended platform user and role. Anonymous platform access must remain gated,
  and the old password endpoint must not establish authentication.
- Accounts, roles, findings and the encryption key survive both a dashboard
  requested restart and an Apps stop/start; only temporary executable/config
  files are recreated. Platform identity must continue to authenticate each
  request, and an existing local cookie must not bypass it. Explicit admin
  binding must not grant every platform user the administrator role.
- The service principal can create/use the intended `sage` schema, read the
  monitoring catalogs and perform permitted fixture operations. Unsupported
  extensions/admin settings are reported as unavailable.
- Open fresh database connections after the initial one-hour credential
  lifetime; verify OAuth refresh rather than relying on existing pooled
  sessions. Reconnect after a scale-to-zero cycle.
- One real structured request succeeds against the selected Unity Gateway
  route/model, and denied credentials/permissions fail without fallback to a
  different ungoverned endpoint.
- General/advisor, dedicated optimizer and SRE routing are checked separately.
  HTTP 401/403 remain terminal; retryable transport/429/503 paths reauthorize
  through the SDK, which may reuse a still-valid token. Measure actual renewal
  where observable and label elapsed-time endurance evidence accurately.
- For the current deployment, read retained history through the actual investigator tool and
  verify durable citations, source/event times and evidence hashes. Empty
  history proves neither positive delivery nor incident detection. Keep
  platform query support separate from the default database-only policy;
  explicitly authorized native cluster/filesystem reads do not expand that
  production policy. `pg_hint_plan` installation does not establish an active
  planner hook, and unavailable HypoPG does not establish a what-if benefit.
- For an App SLO pilot, push actual current-time counters across both burn
  windows, verify replay/refusal behavior and exactly one scoped durable case
  for a continuing synthetic acceptance burn, then stop pushes and observe
  actual stale-to-unknown behavior. Verify cited hashed SLO evidence, the real
  Gateway investigator's terminal transcript/event chain, and unchanged
  action/approval ledgers. Remove only owned fixture rows and the temporary
  objective while preserving the case; synthetic data makes no production
  customer-impact claim. These scoped dev.5 checks
  [passed on the dedicated development App](../reviews/2026-10-30-app-slo-acceptance.md);
  overall final deployment acceptance is tracked separately.
- SIGTERM shuts down cleanly within the platform allowance. The optional
  secondary Prometheus listener is not exposed by Apps' single public port.

The standard test suite includes backend, frontend and local PostgreSQL fixture
coverage beyond these deployment checks. Passing that suite cannot certify
host-level access, every supported PostgreSQL extension, or every LLM model.

## Sources and verification boundary

Internal documentation was consulted before checking public documentation,
installed CLI help/schema and the implementation. This portable guide cites
public sources and repository evidence only; internal design material is not a
shipped-feature contract. CLI caveats above are version-specific and should be
rechecked after upgrading the CLI.

- [Apps environment](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/system-env):
  Ubuntu, Python runtime, injected port and service principal credentials.
- [Apps best practices](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/best-practices):
  H2C, proxy TLS termination, required bind address and port.
- [Apps development](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/app-development):
  10 MB per-file deployment limit.
- [Runtime configuration](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/app-runtime):
  explicit command array, environment values and resource references.
- [Lakebase custom app tutorial](https://docs.databricks.com/aws/en/oltp/projects/tutorial-databricks-apps-autoscaling):
  OAuth connection credentials and service principal grants.
- [Azure Lakebase private connectivity](https://learn.microsoft.com/en-us/azure/databricks/oltp/projects/private-link):
  regional `*.database.<region>.azuredatabricks.net` ingress names. Provider
  classification supports AWS and Azure regional names; Azure remains untested
  against a live endpoint. OAuth still checks the actual endpoint API hostname.
- `databricks bundle schema` with CLI 1.20.0: current `postgres` resource shape
  (`branch`, `database`, `permission`), secret resource and app config fields.

- [App and user authorization](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/auth):
  dedicated App identity versus separately consented user authorization.
- [Lakebase App resources](https://docs.databricks.com/aws/en/dev-tools/databricks-apps/lakebase):
  principal roles, connection variables and resource-removal ownership effects.
- [Apps CLI reference](https://docs.databricks.com/aws/en/dev-tools/cli/reference/apps-commands):
  direct deployments, update masks, deployment status and logs. The installed
  CLI's `--help` was cross-checked for the command forms in this page.
