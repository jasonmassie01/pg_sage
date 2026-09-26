# Group 09 — Web dashboard (excluding agentdb pages)

## Scope (packages/files reviewed, LOC)

- `sidecar/web/src` excluding `pages/agentdb/*` and `pages/AgentDBsPage.jsx` (owned by the
  agentdb reviewer): `App.jsx`, `main.jsx`, `hooks/useAPI.js`, `hooks/useLiveEvents.jsx`,
  `context/TimeRangeContext.jsx`, all `components/*`, `pages/*.jsx`, `pages/databases/*`,
  `pages/notifications/*`, `generated/config_meta.json` (usage only).
  About 10.4k production JS/JSX LOC and 1.7k test LOC. About 2.0k of those production LOC are dead
  (see section B).
- `vite.config.js`, `package.json`, `e2e/fixtures.ts` (the mock shapes).
- Contract counterparts I read in Go: `internal/api/router.go`, `handlers.go`,
  `handlers_new.go`, `action_handlers.go`, `cases_handlers.go`, `config_handlers.go`,
  `config_apply.go`, `llm_handlers.go`, `value_handlers.go`, `events.go`, `middleware.go`,
  `database_handlers.go`, `notification_handlers.go`, `internal/cases/case.go`,
  `internal/fleet/{manager,types,overview_types,capabilities}.go`, `internal/llm/manager.go`,
  `internal/executor/{action_policy,manual}.go`, `internal/store/config_helpers.go`,
  `internal/store/database_store.go`, `cmd/pg_sage_sidecar/wire.go`.

### API contract audit summary

I checked every URL the UI calls against `router.go`. **Every live call maps to a registered
route with the same method.** The drift is in the payload, not in the routes:

| UI call | Route | Payload/field drift |
|---|---|---|
| `GET /api/v1/llm/status` (TokenBudgetBanner) | OK | **UI reads `data.general` / `data.optimizer`; API returns `{clients:{general,optimizer}, any_exhausted}`** (B02) |
| `POST /api/v1/restart` (Settings) | OK | **no `Content-Type` → 415 from `requireJSONMiddleware`** (B03) |
| `GET /api/v1/actions` (Actions "Executed") | OK | merged queue and log rows have no discriminator; UI treats queue rows as executed (B01, B04, B14) |
| `GET /api/v1/cases` | OK | UI reads `impact_score`/`urgency_score` (absent); `actions[].expires_at` is zero-time for log rows (B06, B07) |
| `GET /api/v1/actions/pending` (single mode) | OK | handler parses `?database=` as an **int id**; UI sends the **name** (B27) |
| `GET /api/v1/findings?limit=5` (Dashboard "Recent") | OK | default sort is `severity`, not recency (B08) |
| `GET /api/v1/config/global`, `/config/databases/{id}` | OK | UI ignores `pending_restart`, `active_generation`, `desired_generation` (only used for CAS) |
| `GET /api/v1/value` | OK | fields match; `since`/`until` supported but never sent |
| `GET /api/v1/databases`, `/fleet/health`, `/fleet/readiness`, `/shadow-report`, `/alert-log`, `/snapshots/latest`, notifications, users, auth, managed DBs | OK | fields match |
| `GET /api/v1/events` (SSE) | OK | named events `findings/actions/health/heartbeat` match `events.go:23-26` |

Settings keys vs `store/config_helpers.go` `allowedConfigKeys`: all 48 UI keys are allowed, and
`execution_mode` is special-cased in the DB handler. Every key also has a `hotReload*` case in
`config_apply.go`. **But `trust.tier3_high_risk` has no runtime consumer at all** (B11).

---

## A. Bugs

| ID | Sev | Conf | file:line | summary |
|---|---|---|---|---|
| G9-B01 | **P0** | CONFIRMED | `web/src/pages/Actions.jsx:379-394`, `internal/api/handlers.go:1847-1848,2043-2067`, `internal/executor/manual.go:131-135` | "Roll Back Action" appears on **queued (unapproved) proposals**. Clicking it sends the queue id to an endpoint that looks up `action_log` by id, so it runs the rollback SQL of an **unrelated executed action** |
| G9-B02 | P1 | CONFIRMED | `web/src/components/TokenBudgetBanner.jsx:76-77` vs `internal/api/llm_handlers.go:191-194` | LLM budget-exhausted banner can never render (payload shape drift). The e2e mock uses the wrong shape, which masks it |
| G9-B03 | P1 | CONFIRMED | `web/src/pages/SettingsPage.jsx:257-260`, `internal/api/middleware.go:151` | "Restart now" always fails with 415 because `Content-Type` is missing |
| G9-B04 | P1 | CONFIRMED | `web/src/pages/Actions.jsx:14-24,179-181,367-368` | Action history mislabels queued proposals as "Monitoring" and "SQL Executed". Verified log rows show "not_started" |
| G9-B05 | P2 | CONFIRMED | `web/src/context/TimeRangeContext.jsx:25`, `web/src/hooks/useAPI.js:23-27` | 30 s time-range tick changes the URL, so useAPI wipes data. The skeleton flashes and the expanded Executed row (with its rollback button) collapses every 30 s |
| G9-B06 | P2 | CONFIRMED | `internal/cases/case.go:150`, `internal/api/cases_handlers.go:473-485`, `web/src/pages/CasesPage.jsx:49-54,350` | Case timeline shows "Expires: 1/1/0001…" for every executed action |
| G9-B07 | P2 | CONFIRMED | `web/src/pages/CasesPage.jsx:26-28,145-146` | Every case shows "Impact: n/a · Urgency: n/a" because the fields don't exist in the Case JSON |
| G9-B08 | P2 | CONFIRMED | `web/src/pages/Dashboard.jsx:358,391-395,340-342,41` | Findings fetch error is rendered as "No recent recommendations". "Newest recommendations" are actually sorted by severity |
| G9-B09 | P2 | CONFIRMED | `web/src/App.jsx:68-70`, `web/src/components/Layout.jsx:339` | Stale `pg_sage_db` (deleted or renamed DB) with ≤1 DB left: picker hidden, every page 404s "database not found", and no visible way out |
| G9-B10 | P2 | CONFIRMED | `web/src/pages/databases/DatabaseForm.jsx:13-24`, `cmd/pg_sage_sidecar/wire.go:340`, `internal/store/database_store.go:203` | Editing a managed DB in the Fleet UI wipes its tags |
| G9-B11 | P2 | CONFIRMED | `web/src/pages/SettingsPage.jsx:1129-1130` | "Tier 3: High Risk" toggle has no consumer (policy always queues high risk) |
| G9-B12 | P2 | CONFIRMED | `web/src/pages/SettingsPage.jsx:15-28`, `web/src/components/TrustBadge.jsx:10,18,26` | Trust copy misstates the execution policy (ignores execution_mode, tier3 flags, 8/31-day ramps, maintenance window) |
| G9-B13 | P2 | CONFIRMED | `web/src/App.jsx:191-196`, `internal/api/router.go:321-327`, `SettingsPage.jsx:884-1057` | Emergency stop is reachable only by admins (API allows operators). No scope label, no current-state display, and Resume is always enabled |
| G9-B14 | P2 | CONFIRMED | `web/src/pages/Actions.jsx:429-431`, `web/src/components/DataTable.jsx:74-77` | Queue id N and log id N collide as row keys: duplicate React keys, and expanding one row expands both |
| G9-B15 | P2 | CONFIRMED | `internal/api/action_handlers.go:86` | Fleet pending list silently skips a DB whose query errors. The UI then says "No actions waiting for approval" |
| G9-B16 | P2 | CONFIRMED | `web/src/pages/CasesPage.jsx:116-188` | In "All Databases", case cards never show which database they belong to (`database_name` exists) |
| G9-B17 | P3 | CONFIRMED | `Dashboard.jsx:354`, `Actions.jsx:55`, `AlertLogPage.jsx:155`, `FleetHealthChart.jsx:59`, `SettingsPage.jsx:897,959`, `DatabasePage.jsx:69` | `?database=` not URL-encoded (other pages do encode) |
| G9-B18 | P3 | CONFIRMED | `web/src/hooks/useAPI.js:23-27` | Data is cleared in the effect rather than at render, so for one commit the old DB's data renders under the new selection |
| G9-B19 | P3 | CONFIRMED | `web/src/components/ProviderReadinessMatrix.jsx:65` | Fetch error returns `null`, so the tab silently shows nothing |
| G9-B20 | P3 | CONFIRMED | `web/src/pages/ValuePage.jsx:206-211` | "View evidence" links to `#/ledger?...`, which renders Not Found |
| G9-B21 | P3 | CONFIRMED | all direct `fetch()` mutations | 401 on a mutation does not trigger `sage:auth-expired`; the user sees an "authentication required" toast |
| G9-B22 | P3 | CONFIRMED | `web/src/components/TokenBudgetBanner.jsx:116-131` (button `:118`) | Reset Budget is shown to viewers/operators but the endpoint is admin-only (403) |
| G9-B23 | P3 | PLAUSIBLE | `web/src/components/SQLBlock.jsx:79-82` | `navigator.clipboard` is undefined on plain-HTTP non-localhost origins; the Copy button throws in its handler |
| G9-B24 | P3 | CONFIRMED | `web/src/pages/AlertLogPage.jsx:101-111`, `handlers_new.go:338` | Date filter runs client-side over only the last 100 rows per DB, so older ranges say "No alerts match" |
| G9-B25 | P3 | CONFIRMED | `web/src/components/ConfigDiff.jsx:7-31` | Review modal renders new `llm.api_key` / PagerDuty key in plaintext |
| G9-B26 | P3 | PLAUSIBLE | `internal/api/config_handlers.go:47`, `SettingsPage.jsx:1007-1009` | Settings "Databases: N" is `len(cfg.Databases)` (YAML), so it shows 0 for API-managed fleets |
| G9-B27 | P3 | CONFIRMED | `internal/api/action_handlers.go:25-29` | Single-mode pending handler parses `?database=` as an int, so the UI's name filter is silently ignored |
| G9-B28 | P3 | CONFIRMED | `web/src/pages/notifications/shared.jsx:13-18` vs `internal/notify/types.go:31-37` | UI event list is missing `query_rewrite_suggested`, so no rule can be created for it |
| G9-B29 | P3 | CONFIRMED | `web/src/components/FleetHealthChart.jsx:50-55` | X-axis shows HH:MM only, even for 7d/30d windows (ambiguous days) |
| G9-B30 | P3 | CONFIRMED | `web/src/components/DatabaseTile.jsx:18,59-66` | Disconnected DB with 0 findings shows the green "Clean" badge next to "Disconnected / health 0" |

### G9-B01 — Rollback button on queued proposals rolls back an unrelated action (P0)

- **Failure scenario:** The "Executed" tab (`/actions`, also `/advanced/actions`) lists
  `queryActionsWithQueueLedger`, which is `action_log` rows plus `action_queue` rows with
  `status <> 'executed'` (`handlers.go:1847-1848`). A queued proposal awaiting approval has
  `outcome = status = 'pending'` and usually a `rollback_sql` (for example
  `DROP INDEX CONCURRENTLY idx_new`). The UI shows "Roll Back Action" whenever
  `rollback_sql && outcome ∈ {success, monitoring, pending}` (`Actions.jsx:379-382`). The
  operator clicks it (plausibly to "cancel" the proposal) and confirms "Run the stored rollback
  SQL for this action?". The UI then POSTs `/api/v1/actions/{queueId}/rollback`.
  `Executor.RollbackAction` runs `SELECT rollback_sql, outcome FROM sage.action_log WHERE id = $1`
  (`manual.go:131-135`). If `action_log` row `queueId` exists (both are serial ids of similar
  magnitude, so this is very likely), pg_sage executes **that other action's** rollback SQL
  against the monitored DB and marks it `rolled_back`. For example, it drops a production index
  created weeks ago by a different action.
- **Root cause:** The API merges two id spaces into one list with no `kind`/`source` field
  (`buildQueuedActionLedgerMap`, `handlers.go:2043-2067`). The UI keys its "can roll back"
  decision on `outcome == 'pending'`, which in `action_log` means "executed, monitoring" and in
  `action_queue` means "awaiting approval".
- **Fix (minimal):** API: add `"ledger": "queue"` in `buildQueuedActionLedgerMap` and
  `"ledger": "log"` in `buildActionMap`, and prefix queue ids (`"queue:17"`), as `cases_handlers`
  already does with `queue:%d`/`log:%d`. UI: render the rollback button only when
  `row.ledger === 'log'`. For queue rows, show "Pending approval → open in Pending tab".
  Defence in depth: `RollbackAction` should require `outcome IN ('success','monitoring','pending')`
  AND a matching `finding_id`/`sql_executed` passed by the caller, or take an opaque ledger key.
- **Test to add:** Vitest in `Actions.test.jsx`: a ledger row `{id:'7', status:'pending',
  outcome:'pending', rollback_sql:'DROP INDEX …', ledger:'queue'}` must NOT render
  `rollback-action-button`. Go handler test: `GET /actions` returns `ledger` for both kinds, and
  ids are unique across kinds.

### G9-B02 — LLM budget banner never renders (P1)

- **Scenario:** The daily token budget is exhausted, so all LLM features stop. `/api/v1/llm/status`
  returns `{"clients":{"general":{"budget_exhausted":true,…}},"any_exhausted":true}`
  (`llm_handlers.go:191-194`, `llm/manager.go:62`). The banner reads `data.general?.budget_exhausted`
  (`TokenBudgetBanner.jsx:76`), which is undefined, so it returns `null`. The operator never learns
  why LLM analysis stopped, and the "Reset Budget" button is unreachable.
- **Root cause:** Contract drift. `e2e/fixtures.ts:313` (`mockLLMStatusExhausted`) mocks the old
  top-level shape, so `token-budget.spec.ts` passes against a shape the server never emits.
- **Fix:** Read `data.clients?.general` / `data.clients?.optimizer` (and gate on
  `data.any_exhausted`). Update the fixture to the real shape. Also surface `circuit_open`.
- **Test:** Go: a JSON golden test for `llmStatusHandler`. Vitest: render with the real shape and
  expect `token-budget-banner`. Add a contract check (I14) that e2e fixtures unmarshal into the Go
  types.

### G9-B03 — "Restart now" always 415 (P1)

- **Scenario:** An admin saves a restart-bound field, gets "Pending restart: …", and clicks
  **Restart now**. `fetch('/api/v1/restart', {method:'POST', credentials})` has no
  `Content-Type`. `requireJSONMiddleware` (`middleware.go:143-156`) rejects POST without
  `application/json`, so the toast shows "Content-Type must be application/json" and nothing
  restarts.
- **Fix:** Add `headers: {'Content-Type': 'application/json'}` (same as logout, `App.jsx:113-121`).
  Better: a single `apiFetch()` helper that always sets it (see I06).
- **Test:** Vitest asserting that the restart fetch init includes the header. An e2e (real server)
  that POSTs `/restart` without a supervisor and expects 501, not 415.

### G9-B04 — Action history misreports what the agent did (P1)

- **Scenario:** In the same merged ledger as B01, a queued proposal (never executed) renders with
  an outcome badge of "Monitoring" (`outcomeStyle('pending')`, `Actions.jsx:179-181`). Its expanded
  panel is titled **"SQL Executed"** (`:367-368`, only expired/rejected get "Proposed SQL"). The
  Risk column title claims "safe/moderate auto-run". Separately, `verificationStatus(row)` falls
  back to `row.status` then `'not_started'` (`:22-24`), and `action_log` rows carry neither field
  (`buildActionMap`, `handlers.go:2101-2114`). So a successfully **verified** action (has
  `measured_at`) shows "Verification: not_started" whenever any queue row made the column appear.
  Executed-row risk is `deriveDisplayActionRisk` (SQL-prefix heuristic, `handlers.go:1993`), so an
  approved high-risk action shows "safe". The audit trail is the core trust surface, and it
  contradicts reality.
- **Fix:** With the `ledger` discriminator from B01: queue rows get a "Proposed/Queued/Approved"
  badge and a "Proposed SQL" heading. Log rows get `verification_status` computed server-side
  (reuse `actionLogVerificationStatus`, `cases_handlers.go`) and the real `action_risk` from
  the queue/policy record instead of the SQL-prefix guess.
- **Test:** Vitest: a queue-pending row shows "Pending approval" and "Proposed SQL". A log row
  with `measured_at` and outcome `success` shows "verified".

### G9-B05 — Time-range tick resets every range-scoped page every 30 s (P2)

- **Scenario:** `TimeRangeProvider` updates `nowTick` every 30 s (`TimeRangeContext.jsx:25`), so
  `fromISO/toISO` change, so `withTimeRange()` URLs change (Actions executed list,
  FleetHealthChart). `useAPI` treats a new URL as a new resource: `dataRef=null; setData(null)`
  and then `setLoading(true)` (`useAPI.js:23-34`). `ExecutedTab` swaps to skeleton rows, which
  unmounts the `DataTable`, so the operator's expanded row (SQL, rollback SQL, **Roll Back** button)
  collapses mid-read every 30 s. The chart flashes its skeleton. Each hook's own interval (60 s for
  the chart) is torn down and replaced by the tick, so configured intervals are meaningless.
- **Fix:** Stale-while-revalidate: only clear data when the *resource identity* changes (strip
  `from`/`to`, or pass a `resetKey` param such as the database). Or send a relative window
  (`?window=24h`) and let the server compute `now()`, which also fixes clock skew.
- **Test:** Vitest with fake timers: expand a row, advance 30 s, and the row stays expanded and no
  `executed-actions-loading` appears.

### G9-B06 — "Expires: 1/1/0001" on executed actions in Cases (P2)

- **Scenario:** `CaseAction.ExpiresAt` is a non-pointer `time.Time` with `omitempty`
  (`case.go:150`). `omitempty` never omits structs, so `caseActionFromActionLog`
  (`cases_handlers.go:473-485`, which never sets it) serialises `"0001-01-01T00:00:00Z"`.
  `formatDate` accepts it (valid Date), so every executed action in a case timeline says
  "Expires: 12/31/0, 7:03 PM" (local tz).
- **Fix:** Make it `*time.Time` (as `CooldownUntil` is) or use `omitzero` (Go ≥1.24). UI:
  `formatDate` returns null for `getUTCFullYear() < 1971`.
- **Test:** Go: marshal a log-derived CaseAction and assert no `expires_at` key. Vitest:
  `formatDate('0001-01-01T00:00:00Z') === null`.

### G9-B07 — Impact/Urgency always "n/a" (P2)

- `CasesPage.jsx:145-146` renders `impact_score` and `urgency_score`. `cases.Case` has neither
  (`case.go:53-67`), although findings carry `impact_score` (`handlers.go` finding map).
  Every card says "Impact: n/a Urgency: n/a", which reads as "pg_sage doesn't know".
- **Fix:** Project `impact_score` from the source finding into `Case` and compute urgency, or
  delete the two spans. **Test:** Go projector test asserting `impact_score` survives projection.
  A Vitest snapshot with a real Case fixture.

### G9-B08 — Recent recommendations: error shown as "none", and mislabeled as newest (P2)

- `Dashboard.jsx:358` fetches findings, but only the databases fetch error is checked (`:391`).
  When findings returns 500 (or 404 via B09), the panel says "No recent recommendations for the
  selected scope." (`:340-342`), which is a false "all clear". The tab description says "Newest
  recommendations" (`:41`), but the API default sort is `severity desc` (`handlers.go:1040`).
- **Fix:** Render `<ErrorBanner>` for `findings.error`. Request `sort=last_seen&order=desc`, or
  relabel as "Top recommendations". **Test:** Vitest: findings useAPI returns an error, so an
  error banner is visible and the empty text is not.

### G9-B09 — Stale selected database traps the UI (P2)

- `selectedDB` is restored from localStorage (`App.jsx:68-70`) and never validated against the
  fleet. Scenario: fleet had `db1, db2`; the user selected `db2`; an admin deletes `db2`. Now
  `databases.length === 1`, so the picker is hidden (`Layout.jsx:339`). Every per-DB fetch gets
  404 "database not found" (`helpers.go:103-115`), so Cases, Actions, Value and Dashboard all show
  errors. The only escape is Ctrl+K "Select all databases" or clearing storage.
- **Fix:** In App, when `fleetData` loads and `selectedDB !== 'all'` is not in `databases`, reset
  to `'all'` and show a toast. Always render the picker when `selectedDB !== 'all'`.
  **Test:** Vitest: localStorage `pg_sage_db='gone'` with a fleet of one DB, so the effective
  selection becomes `all`.

### G9-B10 — Fleet "Edit" wipes tags (P2)

- `DatabaseForm` state has no `tags` (`DatabaseForm.jsx:13-24`). PUT decodes `tags` as nil, and
  `updatedDatabaseRecord` sets `old.Tags = input.Tags` (`wire.go:340`). The store writes
  `json.Marshal(nil)` = `null` (`database_store.go:203`). Tags set via API/MCP (used for
  fleet filtering and policy) vanish when anyone edits a port or password in the UI.
- **Fix:** Backend: treat an absent `tags` as "keep" (use `*map` in `dbCreateRequest`). UI:
  round-trip `db.tags`. **Test:** Go: PUT without `tags` keeps existing tags.

### G9-B11 — "Tier 3: High Risk" toggle does nothing (P2)

- `trust.tier3_high_risk` is only written (`config_apply.go:261-262`). No reader exists in
  `internal/` or `cmd/` (grep). `EvaluateActionPolicy` always queues `high` for approval
  (`action_policy.go:92-93`). An operator enabling it believes high-risk actions can now
  auto-run (or fears they can).
- **Fix:** Remove it from the UI (and mark it `mode:"reserved"` in config meta), or implement it.
  **Test:** A config contract test: every UI-exposed key must have a non-config reader (grep-based
  Go test over the `config_meta.json` UI subset).

### G9-B12 — Trust copy misstates what will auto-execute (P2)

- Confirm dialog and TrustBadge: "Advisory: pg_sage may automatically execute SAFE actions",
  "Autonomous: … SAFE and MODERATE" (`SettingsPage.jsx:15-28`, `TrustBadge.jsx:10-26`). Actual
  policy (`action_policy.go:58-172`):
  - `execution_mode` must be `auto`, otherwise everything queues or is observe-only.
  - SAFE requires `tier3_safe` and an 8-day ramp.
  - MODERATE under advisory **queues**. Under autonomous it requires `tier3_moderate`, a 31-day
    ramp, and an open maintenance window.

  So an operator who confirms "autonomous" expecting action today sees nothing happen. Worse, one
  who sees the green "Autonomous" badge assumes it is live when `execution_mode=approval`.
- **Fix:** Derive the text from `status.capabilities.action_families[*].decision/blocked_reason`
  (already in `/api/v1/databases`) and show "what would happen right now" (see I04).
  **Test:** Vitest: a badge with `execution_mode=approval` renders "queues for approval".

### G9-B13 — Emergency stop is admin-only in the UI; state and scope are hidden (P2)

- The only stop button is in Settings → General. `/settings` is admin-gated (`App.jsx:191-196`),
  while the API allows operators (`router.go:321-327`), and operators are exactly the people
  approving actions. The button doesn't say whether it stops *all* databases or the selected one
  (it depends on the header picker). It doesn't show the current state (only the header badge
  does, and that badge is fleet-wide "any stopped", `manager.go:144`). Resume is always enabled.
  After a stop, it refetches config, not fleet data, so the header badge lags up to 30 s.
- **Fix:** A header-level stop control for operator+, labelled with its scope. Show per-DB stop
  state. Disable Resume unless stopped. Call the App fleet refetch on success.
  **Test:** Vitest: an operator sees an `emergency-stop-button` outside Settings.

### G9-B14 — Row-key collision in the merged ledger (P2)

- `actionRowKey = JSON([database_name, id])` (`Actions.jsx:429-431`). Queue id 12 and log id 12
  of the same DB produce identical keys. React warns about duplicate keys and may reuse the wrong
  row. `DataTable` opens *both* rows because `expanded === k` (`DataTable.jsx:74-77`). The fix is
  the B01 discriminator. **Test:** a Vitest ledger with colliding ids; expanding one opens exactly
  one panel.

### G9-B15 — Fleet pending list hides failures as "nothing pending" (P2)

- `fleetPendingActionsHandler`: `if err != nil { continue }` (`action_handlers.go:86`). One DB's
  queue query fails (permissions, schema drift), its pending approvals disappear, and the UI shows
  "No actions waiting for approval." The pending-count badge likely has the same pattern.
- **Fix:** Collect per-DB errors and return `{"pending":[…],"errors":[{database,error}]}`.
  UI: render a warning strip. **Test:** a Go handler test with one failing pool asserts that
  `errors` is present.

### G9-B16 — Cases in "All Databases" don't say which database (P2)

- `CaseCard` never renders `database_name` (present in `case.go:56`), and ordering is
  per-DB append, not a global rank (`cases_handlers.go:90-133`). Two identical "Missing index on
  orders" cases from prod and staging are indistinguishable, which is a fleet-context leak at the
  presentation layer. **Fix:** Render the DB chip and sort globally by severity/observed_at.
  **Test:** a Vitest card test.

### G9-B17..B30 (P3, brief)

- **B17** — Use `encodeURIComponent` everywhere, or a shared `dbParam()` (CasesPage has one).
  Impact is low because the server allowlists `[A-Za-z0-9_.-]`.
- **B18** — Key `useAPI` state by URL (`{url,data}`) and return `null` when `state.url !== url`.
- **B19** — Render `<ErrorBanner>` on error.
- **B20** — Either add a ledger/evidence route or link to `#/actions` filtered by the id.
- **B21** — Put 401 handling into a shared `apiFetch`.
- **B22** — Gate the reset button on `user.role==='admin'` (pass role via context).
- **B23** — Guard with `navigator.clipboard?.writeText(...).catch(...)` and fall back to a
  `document.execCommand` select.
- **B24** — Pass `from/to` to `/alert-log`, filter server-side, and drop `LIMIT 100` in favour of
  pagination.
- **B25** — Mask `type=password` keys in `buildDiffRows` ("•••• (changed)").
- **B26** — Report fleet size from `mgr.InstanceCount()`.
- **B27** — Accept a name in the single-mode handler, or have the UI omit the param in single
  mode.
- **B28** — Import event types from an API endpoint, or add the missing one.
- **B29** — Format ticks as date+time when range > 24h.
- **B30** — `healthy` must also require `s.connected && !s.error`.

---

## B. Dead / unwired / half-built

| ID | file:line | what | verdict | why |
|---|---|---|---|---|
| G9-D01 | `web/src/pages/Findings.jsx` (799 LOC) | Legacy findings table with **suppress/unsuppress, approve/reject, manual execute**, visit-tracking. Not imported anywhere (`App.jsx` routes `/advanced/findings` to `CasesPage`) | **WIRE** the actions into Cases, then **DELETE** | The UI has no path to suppress a false positive or manually execute a recommendation. `POST /findings/{id}/suppress`, `/unsuppress`, `/actions/execute` and `/findings/{id}/pending-actions` have no live caller |
| G9-D02 | `pages/IncidentsPage.jsx` (345), `ForecastsPage.jsx` (151), `SchemaHealthPage.jsx` (289), `QueryHintsPage.jsx` (210, test-only import) | Superseded by CasesPage source filters | **DELETE**, after porting **incident resolve** (WIRE) | `POST /incidents/{id}/resolve` is only called from dead IncidentsPage, so incidents cannot be resolved from the UI |
| G9-D03 | `pages/DatabaseSettingsPage.jsx` (225) | Per-DB config editor | DELETE | Superseded by SettingsPage DB scope |
| G9-D04 | `components/SparklineChart.jsx` | Never imported | DELETE | — |
| G9-D05 | `App.jsx` routes `/alerts`, `/notifications`, `/users`, `/database`; `Layout.jsx:19-45`; `CommandPalette.jsx:4-16` | Routed pages with **no nav or palette entry**. Notification channels/rules and user admin are reachable only by typing a URL | **WIRE** | `docs/ui-redesign-v2.md` §4.1 planned them as Settings tabs; never done. The reverse spec's claim "reachable by Settings links" is false (no such links) |
| G9-D06 | `App.jsx` `/advanced/findings`, `/advanced/actions` | "Findings explorer" and "Action history" render the exact same components as Cases/Actions | DELETE (or WIRE a real findings explorer) | Duplicate nav that promises a different view |
| G9-D07 | `Dashboard.jsx:362-389` | "+N new since visit" badge. The key `pg_sage_findings_visit` is only written by dead `Findings.jsx:137` | DELETE (or write the key on Cases visit) | Dead feature; its count also uses a severity-sorted top-5, not "new" |
| G9-D08 | `hooks/useLiveEvents.jsx:20,34,66` | `connected` / `lastEventAt` state is never read by any component, yet `setLastEventAt` fires on every heartbeat. With a new `value` object each time, every consumer re-renders every 15 s | WIRE (a "Live" indicator) or delete the state | Half-built live-status feature |
| G9-D09 | `vite.config.js:14` | Dev proxy for `/sse`; no such server path (SSE is `/api/v1/events`) | DELETE | Stale |
| G9-D10 | `SettingsPage.jsx:1129-1130` | `trust.tier3_high_risk` toggle | DELETE from UI | No consumer (B11) |
| G9-D11 | API endpoints with no UI caller: `GET /config/audit`, `GET /policy`, `/policy/history`, `POST /policy/proposals`, `/policy/proposals/{id}/ratify`, `POST /explain`, `GET /actions/{id}`, `GET /findings/{id}`, `GET /snapshots/history`, `GET /findings/stats`, `GET /forecasts/growth`, `GET/PUT /api/v1/config` (legacy), `GET /llm/models` | — | **WIRE** `config/audit` (who changed trust, when), policy ratification (a human-ratify step with no human UI), and `explain`. The rest are MCP/API-only and fine (TEST-ONLY OK / keep) | These are trust-relevant surfaces that exist server-side only |
| G9-D12 | `DatabasePage.jsx` (`/database`) | Raw `JSON.stringify` snapshot dump, orphaned route; `setError` during render (`:21`) | DELETE, or WIRE into a DB detail drawer | Orphaned, and low value as-is |
| G9-D13 | `App.jsx:130-141` | `TODO(fleet-ctx)` without an issue link | Hygiene: link an issue or delete | CLAUDE.md rule |
| G9-D14 | `CasesPage.jsx:56` | `user` prop passed by App (`:155,167,189`) but ignored | WIRE (role-gated case actions, I03) | Signals intended-but-unbuilt actions |
| G9-D15 | `components/DatabaseTile.jsx:4` imports `formatTrustLevel` from `pages/Dashboard` (circular) | — | Move to `lib/format.js` | Hygiene |
| G9-D16 | `SettingsPage.jsx` ignores `pending_restart` / `active_generation` from GET (`config_handlers.go:36-39`) | Server-side lifecycle state only surfaces transiently after a save | WIRE | A persistent "restart required" banner belongs on every Settings visit |

---

## C. Feature improvements (ranked impact × effort; H/M/L)

The central question is whether the UI makes autonomous actions trustworthy. **Not yet.**
It hides *why* an action ran (policy decision and reason exist in the API), shows the wrong
*what* (B04), never shows *before/after evidence* (`before_state`/`after_state` are returned and
never rendered), shows verification only as a bare word, and can *roll back the wrong thing*
(B01).

| ID | Area | Current → proposed | Impact | Effort |
|---|---|---|---|---|
| G9-I01 | Actions | Add an explicit `ledger` kind, split "Queue" and "Executed" views, fix labels (B01/B04/B14) | H | L |
| G9-I02 | Actions detail | Render `before_state` / `after_state` (already returned by `buildActionMap`), `measured_at`, the verification verdict and the metric that proved it, the policy decision and its reason, approver and approval time, and a finding/case link. This is the "why + evidence + result" panel | H | L-M |
| G9-I03 | Cases | Make cases actionable, role-gated via the unused `user` prop: Approve/Reject for queued candidates, Suppress (with reason and expiry), Resolve incident. Show `proposed_sql`, `confidence`, `risk_tier`, `rollback_class`, `verification_plan`, `requires_maintenance_window`, `database_name`, `observed_at` (all already in `ActionCandidate`, `case.go:71-85`) | H | M |
| G9-I04 | Autonomy status | Replace static trust hints with a computed per-DB "What will pg_sage do right now?" panel: trust, execution_mode, tier3 flags, ramp days remaining (8/31), next maintenance window, emergency-stop and executor state, from `capabilities.action_families` (already served) | H | M |
| G9-I05 | Safety controls | Operator-visible, scoped emergency stop in the header with state. Approve confirmation for moderate/high (show SQL, lock level from `ddl_preflight`, target DB). Disable-while-in-flight on Approve/Reject/Rollback (double-click guard) | H | L |
| G9-I06 | Data layer | One `apiFetch` (Content-Type, credentials, 401 handling, JSON error parse), used by every call site. Stale-while-revalidate in `useAPI` keyed by resource. Relative time windows. Fixes B03/B05/B18/B21 structurally | M-H | M |
| G9-I07 | Fleet honesty | Endpoints return `errors: [{database, error}]` for partial fleet failures. UI shows "3 of 4 databases loaded". The dashboard scopes stat cards to the selected DB. The HealthHero counts warnings, pending approvals and disconnected DBs | M-H | M |
| G9-I08 | Settings | Show per-field lifecycle (live / reconfigure / restart) before saving (`config.LookupFieldLifecycle`). Persistent pending-restart banner. A "Config history" tab from `/config/audit`. Hide non-overridable fields in DB scope instead of toasting on edit | M | M |
| G9-I09 | Navigation | Settings tabs for Users, Notifications and Alert log (ui-redesign-v2 plan). Command-palette entries for all routes. Deep links for a case or action (`#/actions/:db/:ledger/:id`) so evidence can be shared | M | L |
| G9-I10 | Value | Honor the header time range (`since`/`until` supported server-side). Make "View evidence" open the action detail (I02) | M | L |
| G9-I11 | LLM visibility | Fix B02. Show `circuit_open` and the model per client. Show which features are degraded when the LLM is off | M | L |
| G9-I12 | Contract safety | Generate TS/JSON-schema from Go response structs, or a Go test that unmarshals every `e2e/fixtures.ts` mock into the handler's response type. Would have caught B02 and B07 | M-H | M |
| G9-I13 | Hygiene | Delete about 2.0k LOC of dead pages/components (D01-D04, D12) after porting their actions | L-M | L |
| G9-I14 | Load | Cases and shadow-report are polled every 30 s per open tab. Each poll re-projects up to 500 findings and queries `pg_stat_statements` on every monitored DB (`cases_handlers.go:192-200`). Add a short server-side cache and lengthen the interval on Settings | M | L |

---

## D. Questions the user isn't asking

1. **Should proposals and executions ever share one list and one id space?** B01 exists because
   they do. An agent that can roll back the wrong row is the opposite of the trust thesis.
2. **Who is the UI for?** Operators can approve DDL but cannot reach emergency stop, notifications
   or the alert log, and cannot suppress a false positive. Is the operator role actually usable?
3. **Does any UI test run against the real Go API?** The token-budget e2e passes against a payload
   the server never sends. How many other e2e mocks have drifted?
4. **Does the dashboard itself load the monitored DBs?** Every open Cases or Settings tab runs
   finding projection and a `pg_stat_statements` query per DB every 30 s. Is that overhead
   measured, and is it counted against the "low-overhead sidecar" promise?
5. **Can an operator answer "what did pg_sage change in prod last night, why, and did it
   help?"** in under a minute from the UI? Today the answer to "did it help" is a bare
   `verified` string with no metric.
6. **Is plain-HTTP deployment supported?** Clipboard (B23), and cookie `Secure` flags (auth group),
   behave differently. Decide and document it.
7. **What should the UI do while the sidecar restarts?** `/auth/me` failing drops the user to the
   login screen even with a valid session.
8. **Is `trust.tier3_high_risk` a promised feature or a vestige?** The same question applies to
   other `hotReload`-only keys; a reader-exists contract test would answer it fleet-wide.

---

## E. Verification notes

- **Not run (constraint):** `sidecar/web/node_modules` does not exist, so I did **not** run
  `npm run lint`, `npm run build`, or `vitest`/Playwright, and did not `npm install`, as
  instructed. No servers were started, so all runtime claims come from reading code.
  "CONFIRMED" means both sides of the contract were read end to end.
- **dist freshness:** `internal/api/dist` is tracked in git. The last commit touching it is
  `91704dd` (2026-09-07), the same commit as the last `web/src` change; there are 0 `web/src`
  commits after it. Spot-check: the bundle contains strings introduced in that commit ("Auto-safe ",
  Supabase/neon) and current-source strings ("Per-database overrides currently support only
  trust", "View evidence", "Restart now"). **dist is not stale** relative to source. It embeds
  bugs B01-B30 as-is.
- **Route inventory:** 86 `"METHOD /api/v1/..."` patterns extracted from non-test `internal/api/*.go`.
  All UI URLs matched a pattern and method.
- **Config keys:** 48 UI `configKey`s diffed against 113 `allowedConfigKeys`. Only
  `execution_mode` is outside the map, and it is special-cased in the DB handler. Consumer grep for
  a sample of 17 exposed fields found only `trust.tier3_high_risk` with zero readers.
  `advisor.interval_seconds` is read via `Advisor.Interval()`.
- **XSS:** no `dangerouslySetInnerHTML`, `innerHTML` or `eval` in scope. LLM/DB text (case titles,
  evidence, `pr_body`, rollback reasons) is rendered as React text nodes. `href`s are
  static or `encodeURIComponent`-built. No XSS sink found.
- **Polling/leak audit:** `useAPI`, `LiveTimeAgo`, `TimeRangeProvider`, `GeneralTab` arm timer,
  `DataTable` ResizeObserver, `Layout` listeners, and EventSource all clean up on unmount. No
  interval leak found. Toast/SQLBlock `setTimeout`s are uncleared but harmless in React 19.
- **Not verified:** actual React duplicate-key behavior under B14 in the browser. The pending-count
  handler's error path (assumed to be the same pattern as B15). Exact collision frequency between
  `action_queue.id` and `action_log.id` in real deployments (B01 severity assumes both are serial
  and overlapping, which the schema suggests).
