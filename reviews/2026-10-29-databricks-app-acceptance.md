# Databricks Apps development deployment — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

> Historical acceptance/audit snapshot. For current dev.4 behavior and validation, see the [current acceptance matrix](2026-10-30-v2.5-dev4-acceptance.md) and [per-source native SRE matrix](2026-10-30-native-sre-source-matrix.md). Original results and failures below are retained.

> Historical validation record. The [dev.3 acceptance report](2026-10-29-v2.5-dev3-acceptance.md) records the final October 30 artifacts, deployment, replacement-composite counts and remaining gaps; the results below retain their original scope and dates.

## Current deployment: Databricks-only authentication

development App (deployment URL omitted) runs `2.5.0-dev.2` with Databricks authentication only. There is no separate pg_sage password form, password-login route or local session-cookie authentication in this mode. pg_sage still enforces durable roles and action approvals. The App service principal continues to authenticate background Lakebase and Unity Gateway operations.

- Workspace/profile: [deployment URL omitted] explicitly selected `<test-workspace-profile>`.
- Successful SNAPSHOT deployment: `01f1d3d41751196ea9b23119bf897d8c`; final state RUNNING, compute ACTIVE.
- Version/provenance: `2.5.0-dev.2`, `cc0e4481+local`; CGO/libpg_query retained.
- Linux amd64 built 20:04:45Z, SHA256 `7f6656d2244e644b2f3519c17613649ad78dfc7f7212bbf7ba01ebe28521ae85`.
- Native macOS built 20:04:31Z, SHA256 `32f4fbaac3f0ab0e7aafa18271726055305195d723ab80bb48630fbb32c16ccf`. `pg_sage` resolves to `~/.local/bin/pg_sage`; version and libpg_query were verified.
- Same dedicated Lakebase branch/database and App principal as the historical deployment below. Seven runtime files, maximum 8,000,000 bytes; no secret values or bundle state uploaded.

The renderer explicitly selects `SAGE_AUTH_MODE=databricks_app` and the reviewed administrator email. The native server accepts only the Apps proxy's authenticated email identity in this mode, scopes it to the HTTPS workspace issuer, and reads the user's durable role on each request. It does not derive roles from forwarded role/user headers or link arbitrary existing local accounts by email. Explicitly configured admin migration preserved ID 1, removed its database password and revoked its old sessions. Newly provisioned platform users default to viewer; these role boundaries are also covered by focused auth/API tests. This is an Apps ingress trust mode, not an option for exposing an unauthenticated native listener elsewhere.

## Current live acceptance and cleanup

Both initial passwordless acceptance and the final post-cleanup API/Chrome runs passed:

- Anonymous App access redirected to workspace OIDC (HTTP 302). The selected operator’s workspace OAuth authenticated App requests without a pg_sage password or `sage_session` cookie. `/auth/me` identified `<operator-email>`, admin, ID 1.
- Chrome rendered the dashboard directly, without the separate login form or sign-out flow. It used the selected operator’s workspace OAuth through the actual Apps ingress; an interactive IdP login was not automated. Its real same-origin browser POST reached the disabled password route (404). No unexpected console, page or HTTP errors occurred; the final screenshot was visually inspected.
- Two bounded client forwarded-email/user/role probes still resolved to the selected operator/admin. The email probe was deliberately invalid, so failure to replace it would reject before provisioning an alternate account. The durable user inventory remained exactly one unchanged account. This verifies the tested ingress behavior; it does not claim every forwarded header is immutable.
- Password login returned 404 and did not issue a cookie. An untrusted browser origin with cross-site fetch metadata returned 403. Valid same-origin requests reached the disabled route.
- Direct database readback confirmed admin password NULL, `oauth_provider=databricks_apps`, workspace-scoped issuer, the selected operator’s canonical email subject, and zero local sessions.
- Readiness reported config/control database/schema all ok. PG18.6 remained connected with a known Lakebase adapter. Final recent persisted and overview collector timestamps matched at 20:07:38.987194Z, reporting 18,866,176 bytes.
- Uncached native App-principal Unity Gateway calls succeeded before and after resource cleanup. Final unique SELECT1 returned a parsed explanation and increased token spend 1864→2230 on `system.ai.gpt-6-sol`.
- After first passwordless acceptance, a resources-only update detached only `sage-bootstrap-admin-password`. It reached SUCCEEDED, with the original PostgreSQL, encryption-secret and model-service entries preserved exactly. Only the task-created `pg-sage-dev/bootstrap-admin-password` secret key was then deleted; the encryption key and all other scope keys were preserved.
- Final App resources are `postgres`, `sage-encryption-key` and `gateway-model`. Runtime configuration contains no bootstrap-password environment variable. The private login file was replaced with Databricks-only instructions and remains 0600. Final API/browser checks passed after deletion, proving no dependency on that secret.

The first dev.2 deployment (`01f1d3d36c1a1afaab4cf787c5250702`) failed at startup because its auth helper required a fully qualified HTTPS URL while the platform injects `DATABRICKS_HOST` as a bare hostname. Its failed deployment, artifact/source fingerprints and private runtime log were retained. The helper now safely normalizes the bare platform hostname to HTTPS and still rejects insecure schemes, userinfo, paths, queries and fragments. Focused race-enabled regressions passed before rebuilding. A fixed-enum-only launcher diagnostic confirmed `bare-hostname` in the successful live runtime without printing its value or any credentials.

Eighteen Python packaging/launcher/renderer tests passed, including rejection of stale local bootstrap settings. Full affected Go/auth/API/command and frontend quality evidence is recorded in the [full suite report](2026-10-29-full-test-suite.md). Final production/embedded build fingerprints cover 1,768 files. The known MODEL_SERVICE CLI schema warning remains the sole non-strict bundle validation warning; strict validation is not claimed to pass.

The authentication change does not add the missing Lakebase Advanced Telemetry connector or establish complete SRE feature coverage. The final capability message now distinguishes unavailable pg_sage ingestion from provider telemetry availability. Observation mode, auto-safe blocking, unknown ANALYZE permission for this monitored database, extension and managed-service limits remain visible. See the [Advanced Telemetry and SRE assessment](2026-10-29-lakebase-advanced-telemetry.md).

Current ignored evidence: `.local-test-results/apps-dbx-auth-artifacts.json`, `apps-dbx-auth-source-manifest.json`, `apps-dbx-auth-deployment.json`, `apps-dbx-auth-final-acceptance.json`, `apps-dbx-auth-final-browser.json`, `apps-dbx-auth-final-dashboard.png`, `apps-dbx-auth-cleanup.json`, resource-update records, host-format log and corrected build logs. No password, bearer token or cookie value is included in this report.

## Historical dev.1 deployment and persistence tests

The sections below describe the earlier password-based dev.1 artifact. Its local-cookie restart checks are historical and do not describe the current Databricks-only UI. The elapsed credential test concerns native Lakebase/Gateway App-principal authentication; it was not rerun as a 65-minute soak on dev.2.

### Final deployment

- Workspace: [deployment URL omitted] (explicitly selected `<test-workspace-profile>`).
- App: development App (deployment URL omitted), RUNNING; compute ACTIVE.
- Final SNAPSHOT deployment: `01f1d3cc0cc910e18c0e11ab6eff532d`, created 2026-10-29T19:07:56Z.
- Version: `2.5.0-dev.1`, provenance `cc0e4481+local`, CGO/libpg_query retained. Linux amd64 built 19:02:03Z; native macOS built 19:01:51Z.
- Final Linux SHA256: `3cbfaef7f2d55313f91428810862643b66d70062c0cbf0574a27db9cefcd4460`.
- Installed macOS SHA256: `69011c5b91708d378c9d55758be564fcde895a6660b5cf3ea50e458efe47dd6d`; executable `~/.local/bin/pg_sage`.
- Source: `<workspace-source-directory>>/pg-sage-dev`; seven runtime files, largest 8,000,000 bytes. No bundle state or inline secret values uploaded.
- Isolated Lakebase PG18 branch: `projects/<project>/branches/<branch>`, endpoint `primary`, PostgreSQL database `pg_sage` (resource ID `pg-sage`). No pre-existing customer database altered.
- App service principal: `<app-principal-id>`. PostgreSQL grants include monitoring/signal access and public schema CREATE; App created its own sage schema on first run. Only the fresh pg_sage database was subsequently transferred to this principal for scoped supported ALTER DATABASE actions.
- All four resources were preserved on final deployment: PostgreSQL CAN_CONNECT_AND_CREATE; secret references `pg-sage-dev/encryption-key` and `pg-sage-dev/bootstrap-admin-password` (READ); `system.ai.gpt-6-sol` MODEL_SERVICE EXECUTE.

### Acceptance on the final artifact

- `/health` and embedded assets respond through the Apps proxy. `/ready` returned ready with configuration, control database and schema checks all ok.
- Installed Chrome/Playwright completed the real pg_sage login form, rendered the dashboard cards and database row, and reported zero unexpected console, page or HTTP errors. Its screenshot was visually inspected.
- Original first-admin account remains ID 1, email `<operator-email>`, role admin. Session cookie is Secure and HttpOnly. Earlier restart checks confirmed exactly one account and absence of the supplied bootstrap password from runtime logs.
- Lakebase PG18.6 is connected, provider adapter known, observation trust. The current pg_sage auto_explain ingestion and HypoPG report unavailable; pg_stat_statements is available. This auto_explain badge describes the missing pg_sage connector, not Lakebase's full capability: Beta `system.lakebase.plan_history` and `postgres_logs` expose provider telemetry. See the [Advanced Telemetry and SRE assessment](2026-10-29-lakebase-advanced-telemetry.md).
- A real persisted collector snapshot at 19:08:05.199448Z reported 18,350,080 bytes. The overview exposed the same fresh in-memory collector timestamp/size; analyzer last run was 19:08:06.056377846Z. This verifies the collector display fix.
- A real config API write of `llm.auth=databricks` read back as an override. Invalid `oauth` returned HTTP 400 without changing generation or the native auth value. A finally block restored the exact original `databricks` value and YAML source.
- A unique plan-only SELECT1 used the native App service principal and Unity Gateway route `system.ai.gpt-6-sol`, returned a parsed natural-language explanation, and increased token spend 1454→1810. This was an uncached gateway call.
- The synthetic encrypted-secret fixture was absent, and original token budget 500000/YAML survived final deployment.

### Earlier persistence and encryption checks

The first deployment (`01f1d3beea5614af843fde30090b46a0`, 17:34:04Z) passed native App-principal Gateway model discovery (53 models), real Gateway EXPLAIN, dashboard-requested exit42/supervisor restart, and full Apps stop/start. The same pg_sage session/account and persisted explanation cache survived both restart paths. Logout returned 200 and subsequent account access 401.

The soak deployment (`01f1d3c207f913b2a9f67ac784fb6d6e`, 17:56:13Z) used Linux SHA256 `f5f642782ad1e69cc899710950d170c274d4103bbad24ae78a1bc96669abc49f`, built 17:54:59Z. Its final clean process startup was 17:59:27Z. It passed an exact encrypted config roundtrip:

- A safe synthetic `llm.api_key` override was written through the real API while native Databricks auth remained configured, so the synthetic value was unused as a credential.
- PostgreSQL confirmed `sage-enc:v1` ciphertext and absence of plaintext.
- After process restart, the API read/decrypted and masked the override. A native helper derived the deployment key using its persistent salt and verified that opening the stored ciphertext exactly matched the synthetic value.
- The override was deleted through the API, its database row confirmed absent, and the process restarted into the original clean configuration. No notification was sent or external credential replaced.

### Elapsed App-principal credential and reconnect checks

The unchanged soak process ran from 17:59:27Z through the post-soak checks after 19:07Z. The watcher began 17:59:37Z, retained the same authenticated session, and passed each checkpoint through 65 minutes. Initial physical backends 6330/6331 were automatically replaced after the 30-minute pool lifetime, and again around 60 minutes.

At 3906.5 seconds elapsed, the watcher confirmed unchanged deployment/startup and recycled only two idle client backends scoped to the App principal, database `pg_sage`, and application_name `pg_sage`. Readiness recovered and the **original session assertion passed**. Its subsequent assertion that the original explanation must still be cached failed. This failed fixture is retained: the configured cache TTL is 60 minutes, so the original result had legitimately expired during the 65-minute soak; the post-soak request upserted a fresh result. The original watcher is not reported as an entirely passing test.

A bounded followup on the same unchanged process verified the 60-minute TTL and fresh persisted row created 19:04:43.748915Z. It recycled only scoped idle backends again, observed a new physical backend 16716 started 19:06:22.508004Z, and confirmed readiness, the followup authenticated session, identical cache timestamp/summary, and exact unchanged database row before/after reconnect. The original watcher's cookie was deliberately never persisted and its process had exited, so this followup used a new authenticated session; it does not claim the same cookie across both tests.

Before final deployment, another unique uncached Gateway EXPLAIN increased token spend 2416→2764. The App then wrote token budget 500000→500001 through the real API, read it back as an override, and restored the exact original 500000/YAML in a finally block. The process startup remained 17:59:27Z throughout. The budget is intentionally restart-bound: an earlier fixture incorrectly required no pending-restart marker, failed that assertion, and still restored the original value. That failed fixture is also retained. The corrected check proves persisted App-principal database write/readback after the elapsed interval, without claiming hot application.

These are **elapsed-time credential tests** beyond the nominal one-hour lifetime. Exact injected App token mint/expiry is not exposed, so they do not prove an exact expiry instant. No scale-to-zero cycle was forced; full Apps stop/start was tested separately.

The final artifact adds the in-memory collector display fix and the `llm.auth` store enum/serialization fix. All 15 saved runtime authentication, connection and launcher source fingerprints remained identical to the soak artifact. The 65-minute test therefore ran on the earlier artifact; the final artifact passed the fresh live acceptance above, including the new config-store behavior.

### Build and bootstrap checks

- Final static Linux artifact ran successfully through the package launcher and Ubuntu 22.04; SQL AST validation remained libpg_query. The same build runs in the real Apps runtime.
- Race-enabled focused bootstrap tests passed against a dedicated disposable PostgreSQL fixture for standalone and meta-db paths. They cover supplied env/file credentials, validation, no supplied-password stderr logging, and preserving existing accounts without re-reading invalid bootstrap settings.
- Seventeen stdlib Python package/launcher/renderer tests passed, including real subprocess restart-loop and shutdown-signal checks.
- Pinned golangci-lint 2.11.4 with its supported Go 1.26.1 toolchain reported zero issues for the command package. Broader Go/frontend/database results are recorded in the [full suite report](2026-10-29-full-test-suite.md).

### Discrepancies and compatibility boundaries

CLI 1.20.0's bundle schema omits MODEL_SERVICE, although the deployed Apps API accepts and returns it with EXECUTE. Non-strict bundle validation passed with exactly this one enum warning; strict validation did not pass. The Node-only `apps validate` does not support this native package. Deployment used direct Apps API SNAPSHOT mode with app.yaml and preserved all four resources.

The initial live capability report showed provider-adapter unknown despite runtime executor provider=lakebase. Startup firstlook/fleet wiring was fixed, and final acceptance verifies a known adapter with truthful unavailable-extension badges. An initial browser freshness assertion also exposed cached overview status refreshing only on the 605-second orchestrator cadence despite healthy 60-second persisted collector cycles; the final in-memory overlay fixes the display, and final browser assertions verify fresh status.

The App principal lacks CREATEROLE, so agent governance is correctly posture-only. No broader account/project permission was added. The dashboard remains in observation mode with auto-safe actions blocked and ANALYZE permission unknown for this monitored database; App health does not establish unrestricted execution. Optional extensions, host logs, superuser operations and every Gateway model are not certified by these checks. Lakebase OAuth uses the direct endpoint; its built-in transaction pooler requires native passwords. Supported mutations still require object permissions/database ownership. This is a tested development pilot with explicit limits, not universal PostgreSQL compatibility.

Detailed evidence is local and ignored under `.local-test-results/`: `apps-final-artifacts.json`, `apps-final-deployment.json`, `apps-final-display-acceptance.json`, `apps-final-display-browser.json`, `apps-final-dashboard.png`, `apps-final-dev-acceptance.json` (retained failed TTL assertion), `apps-elapsed-oauth-followup.json`, `apps-post-soak-budget-fixture-failed.json`, `apps-post-soak-write-gateway.json`, initial acceptance/runtime logs, bootstrap QA and build logs. No password, bearer token or cookie value is included in this report.
