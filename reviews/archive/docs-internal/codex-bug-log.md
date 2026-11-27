# Codex Bug Log

Last updated: 2026-04-27

Total tracked fixes: 66

## Fixed

- Fleet incident, finding, and action detail endpoints could scan all pools by ID and return the first matching row, so duplicate IDs across databases could surface the wrong record. Detail endpoints now require an explicit `?database=` in multi-pool fleet mode, while still allowing implicit lookup when exactly one database is registered.
- Successful recommendation actions left source findings open, so the UI could continue to offer the same action. Successful action execution now marks linked findings resolved and stores the action log id.
- Rejected recommendations could immediately reappear as pending approvals. Approval mode now suppresses recently rejected finding/SQL pairs during the cooldown window.
- Pending approval counts did not update live. SSE now emits action events from both `sage.action_queue` and `sage.action_log`, and the layout subscribes to refresh pending counts.
- Recommendations could still show manual action controls while a matching approval was pending. The UI now hides duplicate manual action controls when pending approvals exist.
- Emergency Stop cancelled monitoring goroutines, and Resume could not restart them. Emergency Stop now gates actions without cancelling monitoring, so Resume restores operation.
- Several bodyless UI `POST` calls were rejected with HTTP 415 because they omitted `Content-Type: application/json`. Fixed emergency stop/resume, notification test-send, DB test, incident resolve, and token budget reset call sites.
- Managed database edits updated `sage.databases` but did not refresh the active fleet runtime instance. Updates now remove the old runtime name and register the updated record.
- Managed database updates against missing ids could report an internal readback failure instead of not found. Store updates now check affected rows and the API maps missing records to 404.
- Notification channel toggles could persist masked secrets returned by the list API, corrupting Slack webhooks, PagerDuty routing keys, or email passwords. Store updates now preserve existing real secrets when masked placeholders are replayed, and API masking covers `smtp_pass`.
- Notification channel/rule update and delete calls reported success for missing ids, and rule creation relied on database FK errors for missing channels. Notification stores now return not-found/validation errors deterministically and handlers map them to 404/400.
- Notification channel updates treated omitted `enabled`, `name`, or `config` fields as false/empty values, so partial clients could accidentally disable or invalidate channels. The update handler now preserves existing omitted fields.
- Admin users could demote themselves or demote the last admin, potentially leaving the system with no administrator. The API now forbids self admin-role changes and last-admin demotion; the Users UI disables those controls.
- Edit Database `Test Connection` tested the saved connection instead of unsaved form edits. The edit form now submits current fields, and the backend merges the stored password when the password field is left blank.
- Edit Database `Test Connection` with unsaved preview fields bypassed the SSRF host guard used by Add Database testing. Preview edit tests now resolve and block unsafe hosts through the same safety path.
- Editing a managed database could send `max_connections` as a string from the UI, failing JSON decode into the Go integer field. The form now coerces `max_connections` numerically like `port`.
- The edit-form SSRF guard initially blocked testing an already-managed local/private database host, breaking legitimate local fixture edits. Edit tests now allow the stored managed host while still blocking unsafe host changes.
- Settings always saved global config, even when a specific database was selected. Fleet status now exposes database ids, Settings maps the selected database name to id, and selected-database edits use `/api/v1/config/databases/{id}`.
- Global Settings exposed `execution_mode` as an editable field even though global saves discarded it. The global UI now marks execution mode as database-only, while selected-database Settings can edit and persist it.
- `GET /api/v1/config/databases/{id}` returned config for nonexistent ids and defaulted null `execution_mode` to `manual`. It now returns 404 for missing databases and uses the managed DB default `approval`.
- Saved per-database config overrides could not be reset from Settings. A per-database override delete endpoint now removes stored DB overrides, and the Settings reset button calls it for DB overrides.
- Per-database `trust.level` reset removed the stored override but did not update the running fleet executor/status. Reset now synchronizes the effective inherited trust level back into runtime state.
- Multi-key config saves could partially persist earlier keys before later validation failures returned 400. Config batch saves now prevalidate all keys before writing any overrides.
- LLM model discovery could write a masked `llm.api_key` back into global config and ignored selected database scope. Discovery now uses a non-persistent model-discovery request and skips masked keys.
- `/api/v1/explain` accepted viewer-role requests even though it can run database queries. The route now requires operator-or-admin access and has API coverage for valid and invalid explain requests.
- Parameterized explain requests interpolated caller-supplied parameter strings directly into `EXPLAIN EXECUTE`. Parameters are now emitted as escaped SQL literals, with NUL bytes rejected as invalid input.
- Incident resolution from the UI sent an empty JSON request despite the API requiring a JSON body, so resolving incidents could fail with HTTP 400. The UI now sends `{ "reason": "" }`.
- Incident confidence never rendered because the API returns `confidence` but the UI read `confidence_score`. The incident detail drawer now renders the API field.
- Fleet-wide Query Hints, Incidents, and Action History read only the primary database for all-database views, hiding secondary database rows. These handlers now merge all registered pools, include `database_name`, sort deterministically, and apply global limits where needed.
- Action details searched only the primary database. Detail lookup now searches all pools so secondary-database action history rows are reachable.
- Manual action execution accepted any valid SQL for any numeric finding id, including missing, resolved, or mismatched findings. Execution now verifies the finding is open/unresolved and the SQL matches the recommendation before running DDL.
- Pending action lists and counts could expose queue rows for findings already resolved by another action, leading users to approve stale actions that could not safely run. Pending action queries and approval now require the linked finding to still be open and actionable.
- Fleet suppress/unsuppress without `?database=` could mutate the first matching finding id by map-order scan across databases. Mutations now require a database when more than one instance exists and only allow implicit targeting for a single connected instance.
- Emergency stop/resume returned 200 with zero changes for unknown database names and ignored persistence errors from writing the stop flag. Handlers now return 404 for unknown databases and surface persistence failures instead of reporting success.
- Fleet Health chart used raw database aliases as Recharts `dataKey` values. Aliases containing dots could be interpreted as object paths and fail to render; the chart now uses safe internal series keys and display names separately.
- Schema Health applied status/severity/category filters to the table but not to the summary stats, so the summary could contradict the visible rows. Stats now use the same filters, and the summary label now describes matching findings rather than always saying open findings.
- Query Hints treated missing `after_cost` as zero due JavaScript null coercion, creating fake 100% cost improvements. Cost improvement and average improvement now require both before and after costs.
- Query Hints lifecycle states existed (`retired`, `broken`) but `/api/v1/query-hints` only returned active rows, making the UI Status column misleading and hiding revalidation outcomes. The endpoint now returns recent hints across statuses by default and supports `status=active|retired|broken` filtering.
- Schema Health could not filter the `safety` thematic category even though the backend schema linter emits it. The category dropdown now includes safety, and live coverage verifies expanded rows show impact, recommendation, and suggested SQL.
- Fleet Health chart series keys were safe for dotted aliases but still depended on unsorted response object order, so series key-to-database mapping could shift between refreshes. Series are now sorted deterministically, and live coverage verifies chart rendering from seeded `health_history` rows across all three local databases.
- Incidents accepted `low`, `medium`, and `high` risk values plus newer source values, but the UI only labeled the older `safe`, `moderate`, and `high_risk` variants. The detail drawer now labels all valid risk/source values.
- Viewer users were shown the Pending Approval tab and the Actions page fetched operator-only pending approvals, causing forbidden requests for read-only users. Pending-review UI and polling now render only for admin/operator roles, with live viewer/operator boundary coverage.
- Notification channel UI did not expose PagerDuty even though the backend supports it, and selecting unsupported types would fall through to the email form shape. The Channels form now exposes PagerDuty with a routing-key field and stable browser-test selectors for all channel types.
- Notification masked-secret preservation now has browser-level coverage: creating a Slack channel through the UI, receiving a masked webhook from the API, toggling the channel from the UI, and verifying the database still stores the original webhook rather than the masked placeholder.
- Existing managed database connection tests in YAML fleet mode tried to decrypt the `sage.databases.password_enc` placeholder even though runtime credentials live in the fleet instance config. The endpoint now uses fleet runtime credentials for fleet-registered rows and only reports 404 for actual missing database records.
- Managed Database edit-form connection testing now has browser-level coverage for unsaved field changes and empty password fallback, including a failing unsaved database name followed by a successful retry without saving.
- Managed Database API/UI edits did not round-trip `max_connections`; saving from the UI could overwrite a configured fleet limit with `0`. The API now returns and accepts `max_connections`, defaults absent values safely, and the UI includes the field.
- In local YAML fleet mode, saving a managed database edit could close the primary fleet pool, which is also captured for auth and catalog storage, causing subsequent logins/API calls to hang or fail. No-meta fleet edits now refresh runtime metadata in place without closing the primary pool, with browser coverage for saved edits updating runtime trust state.
- Background/live refetches set `loading=true` even when data was already rendered, unmounting tables and collapsing/detaching expanded finding action panels while a user was approving or rejecting queued work. `useAPI` now keeps existing data mounted during background refetches and only shows loading on initial load or URL changes.
- Local no-meta fleet mode exposed Add Database even though new database credentials cannot be encrypted without an `encryption_key`, causing a 500. The store now returns a validation error, and browser coverage verifies the UI surfaces the `encryption_key` requirement.
- Global Settings reset could not remove global overrides correctly because hot reload mutated the live config and the API no longer had an immutable YAML/default baseline to restore from. Config routes now keep a detached baseline snapshot, expose `DELETE /api/v1/config/global/{key}`, reload live config from baseline plus remaining overrides, and the Settings UI can reset global overrides.
- The full-surface fixture did not include a deterministic invalid-index case and only printed finding summaries. The fixture now creates a failed concurrent unique index, asserts expected analyzer categories, schema-lint rule IDs, and active query hints across all three local databases, and polls until the expected surface appears.
- The legacy serial walkthrough could not run against local Docker fixtures without hardcoded database credentials and failed strict-mode locators when multiple finding detail/suppress controls were visible. The walkthrough now supports fixture DB credential env vars, has an encrypted meta-db config for add/edit/delete flows, and scopes duplicate UI controls to the first visible target.
- The encrypted walkthrough fixture was still a manual setup sequence. `run_walkthrough_fixture.ps1` now provisions isolated meta/target databases, seeds the fixture schema, starts the encrypted sidecar, resets the admin password, runs the 124-check walkthrough, and can restore the standard full-surface sidecar afterward.
- The long walkthrough used Playwright serial mode, so one failed checkpoint skipped every later check and hid independent endpoint/UI regressions. The global serial mode is now removed; the wrapper still runs the file with one worker to preserve fixture ordering, but later checks continue after failures.
- Successful create-index approvals could be followed by a stale analyzer cycle reopening the same missing-index finding as a new open row. Finding upserts now suppress immediate reopen of recently action-resolved findings while still allowing genuinely unresolved issues to reappear after the grace window.
- Stale or parallel create-index approvals could execute after an equivalent index already existed, creating duplicate indexes with auto-generated names. Manual/approved create-index execution now checks for an existing valid covering index before running DDL and records the action as successful without creating another index.
- Action lifecycle verification only checked row presence in browser tests. The full-surface harness now verifies pending rows across all three local databases, approve execution, rejected queue removal, executed action logs, resolved finding links, and no post-refresh missing/duplicate-index noise.
- Failed `CREATE INDEX CONCURRENTLY` attempts could leave invalid autogenerated index stubs that blocked the next approval of the same recommendation. Create-index execution now drops invalid covering index blockers before retrying the DDL.
- Transient lock timeouts during approved `CREATE INDEX CONCURRENTLY` actions could fail the UI workflow and leave an invalid index stub. Create-index execution now retries lock-timeout failures and cleans invalid blockers between attempts.
- Mutating action browser specs selected the first available pending create/drop action, so parallel runs could contend for the same database/object depending on queue order. The specs now use disjoint deterministic database/action targets.
- The full-surface high-total-time fixture used a PL/pgSQL loop that pg_stat_statements counted as one call, making the `high_total_time` category intermittent. The workload now uses psql `\gexec` so the high-frequency query is recorded as thousands of real calls.
- Incident resolve mutations in a multi-database fleet could resolve the first matching incident UUID by pool order when the UI did not send a database target. Resolve now requires an explicit database when multiple pools are connected, the UI sends the row database, and duplicate incident UUID coverage verifies only the selected database is mutated.
- Incident listing for a selected fleet alias could drop rows whose stored `database_name` was the physical PostgreSQL database name, and the UI could then resolve against that physical name instead of the fleet alias. Incident list responses now include `fleet_database_name` for mutations, and selected-pool listing no longer filters rows by alias.
- Last-admin demote/delete checks were split from the mutation, so concurrent requests could both pass a stale admin count and leave the system without an admin. User role changes and deletes now run in a locked transaction with DB-backed concurrency coverage.

## Still Open / Next Targets

- Split the walkthrough into smaller domain specs over time so setup-heavy checks and independent endpoint checks can run separately in CI.
- Continue adding targeted browser regressions whenever a manual workflow exposes a bug class not covered by the current app-specific specs.

## Verification

- `go test ./internal/store ./internal/api -run "TestCoverage_NotificationStore|TestPhase2_(Create|Update|Delete).*Handler|TestNotificationHandlers" -count=1`
- `npm test -- --run src/components/FleetHealthChart.test.jsx src/pages/QueryHintsPage.test.jsx` in `sidecar/web`
- `go test ./internal/executor ./internal/api -run "TestCoverage_ExecuteManual|TestManualExecuteHandler|TestCoverage_LogManualAction|TestCoverage_LogAction" -count=1`
- `go test ./internal/api -run "TestAPI_Suppress|TestAPI_Unsuppress|TestCoverage_Suppress|TestCoverage_Unsuppress|TestPhase2_SuppressHandler|TestPhase2_UnsuppressHandler|TestCoverage_RegisterActionRoutes|TestCoverage_NewRouterFull_FleetActions" -count=1`
- `go test ./internal/fleet -run "TestManager_EmergencyStop|TestManager_Resume" -count=1`
- `go test ./internal/api -run "TestAPI_EmergencyStop|TestAPI_Resume|TestCoverage_EmergencyStop|TestCoverage_Resume" -count=1`
- `go test ./internal/api -run "TestGlobalGet_ContainsAllExpectedKeys|TestPhase2_ConfigDBDeleteHandler_RemovesDBOverride" -count=1`
- `go test ./internal/api -run "TestPhase2_TestManagedDBHandler_(PreviewBlocksUnsafeHosts|AllowsStoredPrivateHost)|TestCoverage_TestManagedDB_InvalidID" -count=1`
- `go test ./internal/store ./internal/api -run "TestCoverage_ActionStore|TestPhase2_FleetPending|TestCoverage_NewRouterFull_FleetActions|TestCoverage_RegisterActionRoutes" -count=1`
- `go test ./internal/api ./internal/store ./internal/executor ./internal/fleet`
- `npm test -- --run src/pages/SettingsPage.test.jsx src/pages/databases/DatabaseForm.test.jsx src/components/FleetHealthChart.test.jsx src/pages/QueryHintsPage.test.jsx` in `sidecar/web`
- `npm run build` in `sidecar/web`
- `go test ./internal/api/... ./internal/config/... ./internal/executor ./internal/store ./internal/analyzer ./internal/fleet ./cmd/pg_sage_sidecar`
- `npm.cmd test -- --run` in `sidecar/web`
- `npm.cmd run build` in `sidecar/web`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_full_surface.ps1 -ConfigPath .\test-fixtures\full_surface\verify_config_approval.yaml -PollSeconds 70`
- `.\node_modules\.bin\playwright.cmd test users.spec.ts notifications.spec.ts databases.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test settings.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test incidents.spec.ts explain-api.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test fleet-aggregation.spec.ts incidents.spec.ts explain-api.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test schema-health.spec.ts fleet-aggregation.spec.ts incidents.spec.ts explain-api.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test query-hints-lifecycle.spec.ts schema-health.spec.ts fleet-aggregation.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test schema-health.spec.ts query-hints-lifecycle.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test fleet-health-chart.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test incidents.spec.ts fleet-health-chart.spec.ts schema-health.spec.ts query-hints-lifecycle.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test role-boundaries.spec.ts actions.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test notifications.spec.ts databases.spec.ts` in `e2e`
- `.\node_modules\.bin\playwright.cmd test notifications.spec.ts databases.spec.ts --workers=1` in `e2e`
- `.\node_modules\.bin\playwright.cmd test findings-actions.spec.ts --workers=1` in `e2e`
- `.\node_modules\.bin\playwright.cmd test role-boundaries.spec.ts actions.spec.ts schema-health.spec.ts query-hints-lifecycle.spec.ts fleet-aggregation.spec.ts fleet-health-chart.spec.ts incidents.spec.ts explain-api.spec.ts findings-actions.spec.ts --workers=2` in `e2e`
- `.\node_modules\.bin\playwright.cmd test databases.spec.ts notifications.spec.ts findings-actions.spec.ts --workers=1` in `e2e`
- `.\node_modules\.bin\playwright.cmd test settings.spec.ts --workers=1` in `e2e`
- `.\node_modules\.bin\playwright.cmd test login.spec.ts navigation.spec.ts dashboard.spec.ts findings.spec.ts databases.spec.ts users.spec.ts notifications.spec.ts actions-workflow.spec.ts emergency-stop.spec.ts live-events.spec.ts walkthrough.spec.ts --workers=2` in `e2e`
- `.\node_modules\.bin\playwright.cmd test walkthrough.spec.ts --workers=1` in `e2e` with `PG_SAGE_E2E_FIXTURES=1` and local encrypted meta-db walkthrough config
- PowerShell parser validation for `test-fixtures/full_surface/run_walkthrough_fixture.ps1`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_walkthrough_fixture.ps1 -SkipTests -RestoreFullSurface`
- `go test ./internal/analyzer -run TestPhase2_UpsertFindings_DoesNotReopenRecentlyResolvedAction -count=1`
- `go test ./internal/executor -run "TestCoverage_ExecuteManual_CreateIndexIsIdempotentWhenCovered|TestCoverage_ExecuteManual_SuccessfulExecution" -count=1`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_full_surface.ps1 -ConfigPath .\test-fixtures\full_surface\verify_config_approval.yaml -PollSeconds 90`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_full_surface.ps1 -ConfigPath .\test-fixtures\full_surface\verify_config_approval.yaml -PollSeconds 150`
- `.\node_modules\.bin\playwright.cmd test role-boundaries.spec.ts settings.spec.ts databases.spec.ts notifications.spec.ts actions-workflow.spec.ts findings-actions.spec.ts emergency-stop.spec.ts --workers=2` in `e2e` with `PG_SAGE_E2E_BASE_URL=http://127.0.0.1:18085` (26 passed, 1 skipped before action-spec target correction)
- `.\node_modules\.bin\playwright.cmd test actions-workflow.spec.ts findings-actions.spec.ts --workers=2` in `e2e` with `PG_SAGE_E2E_BASE_URL=http://127.0.0.1:18085`
- `.\node_modules\.bin\playwright.cmd test actions-workflow.spec.ts --workers=1` in `e2e`
- `.\node_modules\.bin\playwright.cmd test --workers=2` in `e2e`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_walkthrough_fixture.ps1 -RestoreFullSurface`
- `go test ./internal/executor -run "TestCoverage_ExecuteManual_DropsInvalidCreateIndexBlocker|TestCoverage_ExecuteManual_CreateIndexIsIdempotentWhenCovered|TestCoverage_ExecuteManual_SuccessfulExecution" -count=1`
- `.\node_modules\.bin\playwright.cmd test actions-workflow.spec.ts findings-actions.spec.ts --workers=2` in `e2e`
- `go test ./internal/analyzer ./internal/executor`
- `powershell -ExecutionPolicy Bypass -File .\test-fixtures\full_surface\run_walkthrough_fixture.ps1 -RestoreFullSurface`
- `go test ./internal/api -run "TestIncidentResolveHandler|TestPhase2_UpdateUserRolePreservingAdmin|TestPhase2_DeleteUserPreservingAdmin" -count=1`
- `go test ./internal/tuner -run "TestRevalidate_LifecycleMarkers|Test(Revalidate|DecideHintFate|FetchQueryStats|LoadHintsForRevalidation|UpdateHintStatus|UpdateRevalidationTimestamp|StartRevalidationLoop|IsNoRowsErr)" -count=1`
- `go test ./internal/auth ./internal/api`
- `go test ./internal/api -run "TestIncident|TestIncidentsListUsesFleetAliasAsResolveTarget|TestPhase2_UpdateUserRolePreservingAdmin|TestPhase2_DeleteUserPreservingAdmin" -count=1`
- `go test ./internal/api -run "TestAPI_(FindingDetail|ActionDetail)|TestIncidentDetailHandler|TestIncidentResolveHandler|TestIncidentsListUsesFleetAliasAsResolveTarget" -count=1`
- `go test ./internal/api -count=1`
- `go test ./internal/auth ./internal/api ./internal/tuner -count=1`
- `cmd /c npm run build` in `sidecar/web`
