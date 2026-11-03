# Native Databricks Apps package

This directory packages pg_sage's existing Go server and embedded React dashboard
for Databricks Apps. Start with the [Databricks-native guide](../../docs/databricks-native.md)
and follow the [operator runbook](../../docs/databricks-apps.md) for prerequisites,
resource setup, builds, validation, snapshot deployment, updates and rollback.
The [Lakebase matrix](../../docs/lakebase.md) records feature permissions and limits;
[dev.10 acceptance](../../reviews/2026-10-30-v2.5-dev10-acceptance.md) records the
current deployment's evidence and remaining gaps.

The Docker image builds a static Linux CGO/libpg_query executable; it is a build
tool. Apps runs the packaged executable through the Python standard-library
launcher. This keeps the existing architecture and requires neither AppKit nor
a custom runtime container.

| File | Purpose |
| --- | --- |
| `Dockerfile` | Builds React assets and the static Linux Go executable, retaining SQL AST validation. |
| `package.py` | Checks the Linux ELF/architecture/static-link shape, compresses the binary into 8 MB chunks and writes its SHA-256 manifest. Requires a fresh output directory. |
| `configure.py` | Renders runtime and bundle configuration from explicit deployment settings and resource references. |
| `settings.example.json` | Placeholder inputs for the App, direct Lakebase endpoint/database, stable encryption-secret reference, administrator and optional AI/history. |
| `launch.py` | Verifies and extracts the binary, validates native OAuth/resource inputs, sets the platform listener and supervises intentional restarts. |
| `app.yaml` | Runtime command and environment template. |
| `config.json` | Native pg_sage configuration template. |
| `databricks.yml` | Declarative bundle template; installed-CLI schema caveats are documented in the runbook. |
| `test_package.py` | Local packaging, rendering and launcher regression tests. |

After building the Linux executable using the runbook, run these commands from
the repository root. Set `PG_SAGE_PACKAGE` to a fresh absolute output directory
and `PG_SAGE_SETTINGS` to the completed local settings JSON file first:

```bash
python3 deploy/databricks-apps/package.py \
  --binary dist/apps-linux/pg_sage --output "$PG_SAGE_PACKAGE"
python3 deploy/databricks-apps/configure.py \
  --package "$PG_SAGE_PACKAGE" --settings "$PG_SAGE_SETTINGS"
python3 -m unittest discover -s deploy/databricks-apps -p 'test_*.py' -v
```

These local commands do not create App resources or deploy code. Keep settings,
evidence and credentials out of version control and the runtime upload. Upload
only the manifest-selected runtime files described in the runbook. Never put a
PAT, OAuth token, database password or secret value in the package.

For an existing App, preserve its principal, complete resource list,
`user_api_scopes`, access permissions and stable encryption secret. A resource
update replaces the whole list; a fresh template is not a merge of existing
bindings. Direct code deployment uses an explicit App name and `SNAPSHOT`, with
a fresh runtime-only workspace source directory. A snapshot freezes code, not
the database or persisted configuration; rollback requires a compatible retained
package and the same artifact/service checks as an update.
