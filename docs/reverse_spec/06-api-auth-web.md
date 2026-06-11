# 06 — API, Auth & Web Dashboard

Reverse-engineered from source. Citations are `file:line` relative to
`sidecar/`. Module: `github.com/pg-sage/sidecar`.

---

## 1. Server Topology

Two HTTP servers run in the process (`cmd/pg_sage_sidecar/main.go`):

| Server | Default addr | Handler | Auth |
|---|---|---|---|
| API + Dashboard | `cfg.API.ListenAddr` (`:8080`) | `api.NewRouterFull` (`wire.go:124`) | session cookie |
| Prometheus | `cfg.Prometheus.ListenAddr` (`:9187`) | `mux` → `/metrics` (`main.go:1752-1761`) | **none** |

The root mux (`router.go:162-178`) routes `/api/v1/` to the middleware-wrapped
API handler and serves everything else from the embedded React build
(`go:embed dist`, SPA fallback: any path with no `.` rewrites to `/`).

> **Dead allowlist entry:** `shouldSkipAuth` (`auth_middleware.go:107`) special-cases
> `/health`, but **no `/health` route is ever registered** on either mux. The
> dashboard root mux only handles `/api/v1/` and `/`. `/health` therefore falls
> through to the SPA file server and returns `index.html`, not a health JSON.

---

## 2. Middleware Stack

Applied to `/api/v1/*` only (`router.go:145-159`), outer→inner order as a
request travels in:

1. `corsMiddleware` (`middleware.go:14`) — CORS for dev only; allowlist of
   `localhost:5173/8080`, `127.0.0.1:5173/8080` (`middleware.go:47-59`).
   Answers `OPTIONS` with 200.
2. `securityHeadersMiddleware` (`middleware.go:78`) — sets
   `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
   `Referrer-Policy: strict-origin-when-cross-origin`,
   `Permissions-Policy: camera=(),microphone=(),geolocation=()`.
   **No HSTS, no CSP** (deliberately, comment `middleware.go:75-77`).
3. `timeoutMiddleware` (`middleware.go:115`) — 30 s per-request context deadline
   (`defaultRequestTimeout`, `middleware.go:105`). **Exempts `/api/v1/events`**
   (SSE) from the deadline (`middleware.go:124`).
4. `maxBodyMiddleware` (`middleware.go:63`) — 1 MB body cap on
   POST/PUT/PATCH.
5. `requireJSONMiddleware` (`middleware.go:137`) — rejects POST/PUT/PATCH
   without `Content-Type: application/json` → **415**. (This is why the web
   client sends a JSON content-type even on empty-body logout.)
6. Caller-supplied `middlewares...`, applied innermost (`router.go:147-149`),
   wired in `wire.go:112-122` as:
   - `SessionAuthMiddleware(authPool)` — cookie auth (always).
   - `rateLimitMiddleware(rl)` — per-IP token-bucket (only if a limiter is
     configured). 429 with `{"error":"rate limit exceeded"}`
     (`main.go:2096-2107`). Trusted-proxy aware via `X-Forwarded-For`
     (`main.go:2109-2168`, default trusted = loopback only).

### Auth middleware (`auth_middleware.go`)

- `SessionAuthMiddleware` reads cookie **`sage_session`**, calls
  `auth.ValidateSession`, injects `*auth.User` into request context under
  `userContextKey` (`auth_middleware.go:19-58`). Missing cookie → 401; invalid/
  expired session → 401.
- `shouldSkipAuth` (`auth_middleware.go:97`) unauthenticated allowlist:
  `POST /api/v1/auth/login`, `GET /api/v1/auth/oauth/{callback,config,authorize}`,
  `/health` (dead, see above), **any agent-ping path** (`isAgentPingPath`,
  `auth_middleware.go:117-125`: matches `/api/v1/agent-dbs/{id}/agent-ping`), and
  any path not starting with `/api/` (static assets).
- `RequireRole(roles...)` (`auth_middleware.go:63`) — per-route RBAC; 401 if no
  user in context, 403 if role not allowed.

---

## 3. Auth & Session Model (`internal/auth`)

- **Roles** (`types.go:21-35`): `admin`, `operator`, `viewer`. `ValidRoles` map +
  `IsValidRole`.
- **Session** stored in `sage.sessions` (UUID PK, `user_id`, `expires_at`).
  `SessionDuration = 24h`, `BcryptCost = 12` (`types.go:26-27`).
  - `CreateSession` (`auth.go:108`), `ValidateSession` joins sessions→users,
    checks `expires_at > now()` (`auth.go:124`), `DeleteSession` (logout),
    `CleanExpiredSessions` (background cleaner, `cleaner.go`).
- **Passwords**: bcrypt via `HashPassword`/`CheckPassword` (`auth.go:29-45`);
  `CreateUser` enforces min 8 chars (`auth.go:52`).
- **Login cookie** (`auth_handlers.go:225-233`): `sage_session`, `HttpOnly`,
  `Secure = isSecureRequest(r)` (TLS or `X-Forwarded-Proto: https`),
  `SameSite=Lax`, `MaxAge = 24h`.
- **Login brute-force limiter** (`auth_handlers.go:25-175`): per-email, 5 failed
  attempts / 15 min window, memory-capped at 10 000 entries with eviction,
  background cleanup goroutine. Env `PG_SAGE_DISABLE_LOGIN_RATE_LIMIT=1`
  disables it (test only, `auth_handlers.go:21`). Returns 429 on trip.
- **Admin bootstrap** (`auth.go:439-452`): `BootstrapAdmin` creates the first
  admin **only if `UserCount == 0`**, else rejects.
- **Last-admin protection**: `DeleteUserPreservingAdmin` /
  `UpdateUserRolePreservingAdmin` (`auth.go:221-333`) take a
  `LOCK TABLE sage.users IN EXCLUSIVE MODE` and refuse to remove/demote the last
  admin (`ErrLastAdmin`). Handlers also block self-delete and self-demote
  (`auth_handlers.go:387-392`, `:567-574`).

### OAuth (`internal/auth/oauth.go`, handlers `auth_handlers.go:416-545`)

- Providers: `github`, `google` (OIDC discovery on accounts.google.com),
  `oidc` (custom issuer via `.well-known/openid-configuration`)
  (`oauth.go:50-66`). Disabled by default; enabled via `cfg.OAuth.Enabled`
  (`router.go:113-126`). Discovery failure → provider set to nil (degrades to
  "disabled").
- CSRF: random `state` bound to browser via `oauth_state` cookie
  (HttpOnly, Secure, SameSite=Lax, 10 min). Callback requires
  `cookieState == state` AND a live server-side state entry
  (`oauth.go:149-182`). State map capped at 10 000
  (`maxOAuthStates`, `oauth.go:202`) with periodic cleaner goroutine
  (`StartStateCleaner`, lifecycle bound to shutdown ctx via
  `router.go:123` / `SetShutdownContext`).
- Callback exchanges code → fetches email (GitHub `/user` + `/user/emails`
  fallback, or OIDC userinfo), then `FindOrCreateOAuthUser` with
  `cfg.OAuth.DefaultRole` (defaults to `viewer`), issues a `sage_session`
  cookie, and 302-redirects to `/`.

---

## 4. REST API Route Table

All under `/api/v1/`. **Auth = session cookie unless noted.** "Role" = extra
`RequireRole` gate. "DB?" = honors `?database=` fleet scoping (validated by
`readDatabaseParam`, `helpers.go:85`; `""`/`all` = fleet-wide).

### 4.1 Core / monitoring (`registerAPIRoutes`, `router.go:181`)

| Method | Path | Role | DB? | Purpose |
|---|---|---|---|---|
| GET | `/databases` | — | (fleet list) | Fleet overview + health scores |
| GET | `/findings` | — | yes | List findings (filters severity/status/db) |
| GET | `/findings/{id}` | — | — | Finding detail |
| GET | `/findings/stats` | — | yes | Findings aggregate (`source=schema_lint` filter) |
| POST | `/findings/{id}/suppress` | operator+ | — | Suppress finding |
| POST | `/findings/{id}/unsuppress` | operator+ | — | Unsuppress finding |
| GET | `/cases` | — | yes | Unified cases feed (findings/hints/forecasts/incidents) |
| GET | `/shadow-report` | operator+ | yes | Shadow-mode would-have-done report |
| GET | `/actions` | — | yes | List executed actions |
| GET | `/actions/{id}` | — | — | Action detail |
| GET | `/forecasts` | — | yes | Forecaster predictions |
| GET | `/forecasts/growth` | — | yes | Growth forecast (v0.9) |
| GET | `/query-hints` | — | yes | Query rewrite/hint suggestions |
| GET | `/alert-log` | — | yes | Alert delivery history |
| GET | `/snapshots/latest` | — | yes | Latest collector snapshot |
| GET | `/snapshots/history` | — | yes | Snapshot time-series |
| GET | `/fleet/health` | — | (fleet) | Fleet-wide health time-series (v0.12) |
| GET | `/fleet/readiness` | — | (fleet) | Provider readiness matrix |
| GET | `/config` | — | yes | Effective config (per-db if scoped) |
| PUT | `/config` | **admin** | — | Update trust level (in-memory) |
| GET | `/metrics` | — | yes | JSON fleet/instance status (not Prometheus) |
| POST | `/emergency-stop` | operator+ | yes | Halt autonomous actions |
| POST | `/resume` | operator+ | yes | Resume after emergency stop |
| GET | `/events` | — | — | **SSE** live-update stream (timeout-exempt) |

### 4.2 LLM (`router.go:255-268`)

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/llm/models` | — | List configured models |
| POST | `/llm/models` | — | Discover models from provider |
| GET | `/llm/status` | — | LLM manager status / circuit / budget |
| POST | `/llm/budget/reset` | **admin** | Reset daily token budget |
| POST | `/explain` | operator+ | LLM EXPLAIN narrative (v0.9) |

### 4.3 Incidents (`router.go:270-292`)

| Method | Path | Role | DB? | Purpose |
|---|---|---|---|---|
| GET | `/incidents` | — | yes | List incidents |
| GET | `/incidents/active` | — | yes | Active incidents |
| GET | `/incidents/{id}` | — | — | Incident detail |
| POST | `/incidents/{id}/resolve` | operator+ | — | Resolve incident |

### 4.4 Auth (`registerAuthRoutes`, `router.go:320`)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/auth/login` | public | Email/password login → `sage_session` |
| POST | `/auth/logout` | session | Delete session, clear cookie |
| GET | `/auth/me` | session | Current user `{id,email,role}` |
| GET | `/auth/oauth/config` | public | `{enabled, provider}` |
| GET | `/auth/oauth/authorize` | public | Returns `{url}` + sets `oauth_state` cookie |
| GET | `/auth/oauth/callback` | public | Code exchange → session → 302 `/` |

### 4.5 Users (`registerUserRoutes`, `router.go:347`) — all **admin**

| Method | Path | Purpose |
|---|---|---|
| GET | `/users` | List users (no hashes) |
| POST | `/users` | Create user (201) |
| DELETE | `/users/{id}` | Delete user (blocks self/last-admin) |
| PUT | `/users/{id}/role` | Change role (blocks self/last-admin demote) |

### 4.6 Config store (`registerConfigRoutes`, `router.go:369`) — all **admin**

| Method | Path | Purpose |
|---|---|---|
| GET | `/config/global` | Effective global overrides |
| PUT | `/config/global` | Set global overrides (hot-reload) |
| DELETE | `/config/global/{key}` | Remove a global override |
| GET | `/config/databases/{id}` | Per-db effective config |
| PUT | `/config/databases/{id}` | Set per-db overrides |
| DELETE | `/config/databases/{id}/{key}` | Remove a per-db override |
| GET | `/config/audit` | Config change audit log |

### 4.7 Notifications (`registerNotificationRoutes`, `router.go:513`) — all **admin**

| Method | Path | Purpose |
|---|---|---|
| GET | `/notifications/channels` | List channels |
| POST | `/notifications/channels` | Create channel |
| PUT | `/notifications/channels/{id}` | Update channel |
| DELETE | `/notifications/channels/{id}` | Delete channel |
| POST | `/notifications/channels/{id}/test` | Send test notification |
| GET | `/notifications/rules` | List routing rules |
| POST | `/notifications/rules` | Create rule |
| PUT | `/notifications/rules/{id}` | Update rule |
| DELETE | `/notifications/rules/{id}` | Delete rule |
| GET | `/notifications/log` | Delivery log |

Senders registered: Slack, Email, PagerDuty (`router.go:576-585`).

### 4.8 Actions queue (`registerActionRoutes`, `router.go:417`) — all **operator+**

Registered only when `ActionDeps.Store` or `.Fleet` is non-nil. Fleet vs
standalone pick different handler impls; same routes.

| Method | Path | Purpose |
|---|---|---|
| GET | `/actions/pending` | Pending action queue |
| GET | `/actions/pending/count` | Pending count (drives nav badge) |
| GET | `/findings/{id}/pending-actions` | Pending actions for a finding |
| POST | `/actions/{id}/approve` | Approve queued action |
| POST | `/actions/{id}/reject` | Reject queued action |
| POST | `/actions/{id}/rollback` | Roll back executed action |
| POST | `/actions/execute` | Manually execute an action |

> When neither Store+Executor nor Fleet is wired, the four mutating routes
> register a stub returning **501 Not Implemented** (`router.go:495-510`).

### 4.9 Managed databases (`registerDatabaseRoutes`, `database_handlers.go:28`) — all **admin**

Registered only when `DatabaseDeps.Store` is non-nil (meta-db/standalone/fleet,
`wire.go:67-110`).

| Method | Path | Purpose |
|---|---|---|
| GET | `/databases/managed` | List managed DB records |
| POST | `/databases/managed` | Add managed DB |
| POST | `/databases/managed/import` | CSV bulk import |
| GET | `/databases/managed/{id}` | Get one |
| PUT | `/databases/managed/{id}` | Update |
| DELETE | `/databases/managed/{id}` | Delete |
| POST | `/databases/managed/{id}/test` | Test stored connection |
| POST | `/databases/managed/test-connection` | Test ad-hoc connection (preview) |

### 4.10 Agent DBs (`registerAgentDBRoutes`, `agent_db_handlers.go:15`)

The agent-DB area (ephemeral provisioned databases for AI agents) uses a hand-
rolled subrouter (`agentDBSubrouterWithRegistry`, `agent_db_handlers.go:45`)
mounted on the prefix `/api/v1/agent-dbs/`. Top-level routes are **operator+**;
the subrouter is wrapped operator+ too, **except** the token agent-ping path
which is unauthenticated (`shouldSkipAuth` → `isAgentPingPath`).

Enumerated routes (method + path, all under `/api/v1/agent-dbs`):

```
POST   /{deployment_id}/agent-ping               (PUBLIC — token auth)
GET    /                                          list deployments
POST   /                                          register/provision
POST   /cleanup                                   archive all expired
GET    /{id}                                      get deployment
DELETE /{id}                                      delete (archive first)
POST   /{id}/ping                                 heartbeat
POST   /{id}/extend-lease
GET    /{id}/recommendations
POST   /{id}/recommendations
POST   /{id}/recommendations/{recId}/feedback
GET    /{id}/audit
GET    /{id}/audit/export
GET    /{id}/deploy-requests
POST   /{id}/deploy-requests
GET    /{id}/deploy-requests/{drId}
POST   /{id}/deploy-requests/{drId}/request-review
POST   /{id}/deploy-requests/{drId}/approve
POST   /{id}/deploy-requests/{drId}/deny
GET    /{id}/ping-tokens
POST   /{id}/ping-tokens
POST   /{id}/ping-tokens/{tokenId}/rotate
POST   /{id}/ping-tokens/{tokenId}/revoke
POST   /{id}/cost-samples
GET    /{id}/cost
GET    /{id}/backups
POST   /{id}/backups
POST   /{id}/backups/check
POST   /{id}/backups/restore-drill-dry-run
GET    /{id}/tuning-hints
POST   /{id}/provision/preflight
POST   /{id}/provision/execute
POST   /{id}/provision/status
POST   /{id}/provision/destroy-dry-run
POST   /{id}/provision/destroy-live
GET    /{id}/provision/attempts
GET    /{id}/cleanup
POST   /{id}/archive
POST   /{id}/restore
# collection sub-resources (not deployment-scoped):
POST   /requests
GET    /requests
GET    /requests/{reqId}
POST   /requests/{reqId}/approve
POST   /requests/{reqId}/deny
POST   /requests/{reqId}/provision
GET    /providers
GET    /provider-configs
POST   /provider-configs/{provider}
GET    /terraform-templates
POST   /terraform-templates
POST   /terraform-templates/{tplId}/approve
POST   /terraform-templates/{tplId}/provision
GET    /blueprints
POST   /blueprints
POST   /blueprints/{bpId}/approve
POST   /blueprints/{bpId}/provision
GET    /identities
POST   /identities
POST   /reconcile
GET    /size-profiles
POST   /size-profiles
DELETE /size-profiles/{id}
```

Agent-DB error mapping (`agent_db_handlers.go:378-412`): `ErrNotFound`→404,
`ErrRestoreRequired`→409, `ErrRateLimited`→429, `ErrRunnerUnavailable`→409,
`ErrBlueprintLLMRequired`→503, `ErrDeleteBlocked`→409, `ErrInvalid`→400,
`ErrConflict`→409.

### Endpoint count

Counting distinct method+path pairs registered:

- Core/monitoring: 26
- LLM: 5
- Incidents: 4
- Auth: 6
- Users: 4
- Config store: 7
- Notifications: 10
- Actions queue: 7
- Managed DBs: 8
- Agent DBs: 62

**Total ≈ 139 REST endpoints** (vs. the stale "17" claimed in `pg_sage/CLAUDE.md`;
the agent-DB subsystem alone is 62). Without the agent-DB subsystem the
"classic" DBA surface is ~77.

---

## 5. Prometheus Metrics (`:9187/metrics`)

Hand-rendered text format (no client_golang) by `handleMetrics`
(`main.go:1772+`). **Unauthenticated**, no labels-per-request scoping. Metric
families emitted:

| Metric | Type | Labels | Source |
|---|---|---|---|
| `pg_sage_info` | gauge | `version`,`mode` | build (`main.go:1785-1787`) |
| `pg_sage_mode` | gauge | — | 0=extension/1=standalone |
| `pg_sage_connection_up` | gauge | — | pool ping |
| `pg_sage_findings_total` | gauge | `severity` | `sage.findings` open by severity |
| `pg_sage_circuit_breaker_state` | gauge | `breaker` (db/llm) | breaker state |
| `pg_sage_collector_last_run_timestamp` | gauge | — | latest snapshot |
| `pg_sage_llm_enabled` | gauge | — | config |
| `pg_sage_llm_circuit_open` | gauge | — | LLM breaker |
| `pg_sage_llm_tokens_used_today` | gauge | — | client |
| `pg_sage_llm_tokens_budget_daily` | gauge | — | config |
| `pg_sage_optimizer_recommendations_total` | gauge | `category` | index findings |
| `pg_sage_optimizer_enabled` | gauge | — | config |
| `pg_sage_fleet_databases` / `_healthy` / `_findings_total` / `_findings_critical` | gauge | — | fleet summary (`main.go:1959-1991`) |
| `pg_sage_fleet_instance_findings` / `_instance_health` | gauge | `database` | per-instance |
| `pg_sage_connections_total` | gauge | `state` | `pg_stat_activity` |
| `pg_sage_database_size_bytes` | gauge | — | `pg_database_size` |
| (cache-hit ratio etc.) | gauge | — | `writeDatabaseMetrics` (`main.go:1994+`) |

Note: `/api/v1/metrics` (§4.1) is a **different** surface — JSON fleet/instance
status, not Prometheus text (`handlers.go:844`).

---

## 6. Web Dashboard (`web/src`)

React 19 + Vite, **hash-based routing** (no react-router). Route switch in
`App.jsx:136-188`; `hashchange` listener re-renders (`App.jsx:88-92`).

### 6.1 Routed pages

| Hash | Component | Gate | Shows |
|---|---|---|---|
| `/` | `Dashboard` | — | Fleet hero, stat cards, `FleetHealthChart`, tabbed tiles/readiness/recos |
| `/manage-databases` | `DatabasesPage` | admin | Managed-DB CRUD + CSV import |
| `/agent-dbs` | `AgentDBsPage` | — | Ephemeral agent-DB provisioning workspace |
| `/findings`, `/cases` | `CasesPage` | — | Unified cases table (`/api/v1/cases`) |
| `/forecasts` | `CasesPage initialSource=forecast` | — | filtered cases |
| `/query-hints` | `CasesPage initialSource=query_hint` | — | filtered cases |
| `/schema-health` | `CasesPage initialSource=schema_health` | — | filtered cases |
| `/incidents` | `CasesPage initialSource=incident` | — | filtered cases |
| `/actions` | `Actions` | — | Executed + pending action queue (approve/reject) |
| `/database` | `DatabasePage` | — | Raw latest snapshot JSON viewer |
| `/alerts` | `AlertLogPage` | — | Alert delivery history |
| `/settings` | `SettingsPage` | admin | Global config editor, emergency stop, embeds ShadowMode |
| `/notifications` | `NotificationsPage` | admin | Channels/Rules/Log tabs (no nav entry) |
| `/users` | `UsersPage` | admin | User CRUD (no nav entry) |
| default | `NotFound` | — | — |

**Distinct routed components: 10** (`Dashboard`, `DatabasesPage`, `AgentDBsPage`,
`CasesPage`, `Actions`, `DatabasePage`, `AlertLogPage`, `SettingsPage`,
`NotificationsPage`, `UsersPage`) + `LoginPage` (pre-auth) and
`NotFound`/`AccessDenied` helpers. **15 routed URL paths** (6 collapse onto
`CasesPage` via `initialSource`).

Nav (`Layout.jsx:19-39`): one "Operate" group — Overview, Cases, Actions,
Agent DBs, Fleet (admin), Settings (admin). Actions item shows a pending-count
red badge fed by `/api/v1/actions/pending/count`. `/notifications` and `/users`
are reachable by URL/Settings links but **have no nav entry**.

### 6.2 Dead / unrouted pages

These page components exist but are imported only by their own tests — routing
was consolidated into `CasesPage`:

- `Findings.jsx` (legacy findings table)
- `DatabaseSettingsPage.jsx` (per-db overrides — never imported anywhere)
- `ForecastsPage.jsx`
- `IncidentsPage.jsx`
- `QueryHintsPage.jsx`
- `SchemaHealthPage.jsx`

`ShadowModePage.jsx` is reachable **only** embedded in SettingsPage, never as a
standalone route.

### 6.3 `useAPI` polling model (`hooks/useAPI.js`)

- Signature `useAPI(url, interval = 30000)` — default **30 s** poll
  (`useAPI.js:6`). `interval <= 0` or `url === null` disables polling.
- Does **not** build URLs; callers append `?database=` and time-range params.
  Helper `withTimeRange(base, range)` appends `from`/`to` ISO params
  (`useAPI.js:74-81`).
- `AbortController` cancels in-flight requests on dep change/unmount; non-OK
  responses throw `Error("<status> <statusText>")` into `error` state.
  **No automatic 401→login redirect** — auth-loss recovery is handled by the
  App-level `/auth/me` gate, not useAPI.
- Returns `{ data, loading, error, refetch }`.
- **SSE** via `hooks/useLiveEvents.jsx`: one `EventSource('/api/v1/events',
  {withCredentials:true})` at mount, named events `findings`/`actions`/`health`/
  `heartbeat`; `useLiveRefetch(types, refetch)` invalidates polled data live.

### 6.4 Client-side auth

- `App.jsx` fetches `/api/v1/auth/me` (`credentials:'include'`) on mount; sets
  user or shows `LoginPage`.
- `LoginPage` POSTs `/api/v1/auth/login` JSON, calls `onLogin(user)` → re-render
  (no redirect). OAuth button appears only if `/auth/oauth/config` →
  `{enabled:true}`; click hits `/auth/oauth/authorize` then hard-redirects
  `window.location.href = url`.
- Logout POSTs `/api/v1/auth/logout` with a JSON content-type header (required
  by `requireJSONMiddleware`) then `setUser(null)`.
- `isAdmin = user.role === 'admin'`; action review needs `admin || operator`.

### 6.5 Toasts (`components/Toast.jsx`)

`ToastProvider` (wraps app); `useToast()` → `{push,success,error,info,warning,
dismiss}` (no-op shim outside a provider). Default duration 4 s, auto-dismiss,
fixed bottom-right `aria-live=polite` viewport, per-variant color + lucide icon.

### 6.6 Time-range context (`context/TimeRangeContext.jsx`)

Five fixed windows: `1h`, `6h`, `24h`, `7d`, `30d`. Default `24h`, persisted in
`localStorage['pg_sage_range']`. Recomputes `from`/`to` every 30 s. Exposes
`{rangeKey,setRangeKey,range,ranges,from,to,fromISO,toISO}`.
Propagation is **opt-in** per page via `withTimeRange` (used by Actions,
Findings); most pages ignore it. `TimeRangePicker` is a `<select>` in the header.

### 6.7 Fleet `?database=` selection (`components/DatabasePicker.jsx`)

`<select>` with `all` + one option per DB; rendered in the header **only when
`databases.length > 1`**. Selection lives in `App.jsx selectedDB`
(localStorage `pg_sage_db`, default `all`) and is passed to each page as the
`database` prop. Pages append `?database=<name>` (skipped when `all`). A
`TODO(fleet-ctx)` documents that id-scoped POSTs (approve/reject/suppress) and
global-resource pages intentionally omit the param.

---

## 7. Discrepancies vs. project docs

- `pg_sage/CLAUDE.md` claims **17 REST endpoints** and **5 dashboard pages** —
  both are badly stale. Actual: ~139 endpoints, 10 distinct routed pages
  (15 routes).
- `/health` is in the auth-skip allowlist but is **not a real endpoint** — it
  serves the SPA `index.html`.
- `PUT /api/v1/config` only mutates the in-memory trust level
  (`handlers.go:815-842`); durable config changes go through the
  `/api/v1/config/global` and `/config/databases/{id}` store routes.
- Six page components (`Findings`, `DatabaseSettingsPage`, `ForecastsPage`,
  `IncidentsPage`, `QueryHintsPage`, `SchemaHealthPage`) are dead code,
  superseded by `CasesPage`.
