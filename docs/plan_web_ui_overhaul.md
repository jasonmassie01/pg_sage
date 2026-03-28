# pg_sage Web UI Overhaul — Implementation Plan

> Web UI becomes the primary management interface. MCP server removed.
> All database onboarding, config, actions, and monitoring done through the dashboard.

## Architecture Changes

### Bootstrap Flow
```
./pg_sage_sidecar --meta-db postgres://user:pass@host:5432/sage_meta

# First run:
# 1. Connects to meta DB, bootstraps sage.* schema
# 2. Creates default admin user (prints generated password to stdout)
# 3. Starts web UI on :8080
# 4. User logs in, adds first monitored database via setup wizard
```

### Database Topology
```
┌─────────────────────────────────────────────┐
│  pg_sage sidecar (single Go binary)         │
│                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
│  │ Collector │  │ Analyzer │  │ Executor │  │
│  │ (per DB)  │  │ (per DB) │  │ (per DB) │  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  │
│       │              │              │        │
│  ┌────▼──────────────▼──────────────▼────┐  │
│  │         Fleet Manager (≤50 DBs)       │  │
│  └───────────────────┬───────────────────┘  │
│                      │                      │
│  ┌───────────────────▼───────────────────┐  │
│  │  Meta DB (sage.* schema)              │  │
│  │  - users, sessions, roles             │  │
│  │  - databases (credentials encrypted)  │  │
│  │  - findings, actions, snapshots       │  │
│  │  - config overrides                   │  │
│  └───────────────────────────────────────┘  │
│                                             │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐     │
│  │ Prod DB1│  │ Prod DB2│  │ Prod DBn│     │
│  │ (read + │  │ (read + │  │ (read + │     │
│  │  DDL)   │  │  DDL)   │  │  DDL)   │     │
│  └─────────┘  └─────────┘  └─────────┘     │
│                                             │
│  ┌───────────────────────────────────────┐  │
│  │  Web UI (:8080) — React dashboard     │  │
│  │  REST API (/api/v1/*)                 │  │
│  │  Prometheus (:9187)                   │  │
│  └───────────────────────────────────────┘  │
└─────────────────────────────────────────────┘
```

---

## Phases

### Phase 1: Auth & User Management
**Goal**: Secure the web UI before adding management capabilities.

#### Schema: `sage.users`
```sql
CREATE TABLE sage.users (
    id          SERIAL PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    password    TEXT NOT NULL,         -- bcrypt hash
    role        TEXT NOT NULL DEFAULT 'viewer',  -- admin | operator | viewer
    created_at  TIMESTAMPTZ DEFAULT now(),
    last_login  TIMESTAMPTZ
);

CREATE TABLE sage.sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     INT REFERENCES sage.users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);
```

#### Roles
| Role     | Can view | Can approve actions | Can config | Can manage users |
|----------|----------|-------------------|------------|-----------------|
| viewer   | Yes      | No                | No         | No              |
| operator | Yes      | Yes               | No         | No              |
| admin    | Yes      | Yes               | Yes        | Yes             |

#### Tasks
- [ ] 1.1 Add `sage.users` and `sage.sessions` tables to schema bootstrap
- [ ] 1.2 Auth middleware — cookie-based sessions, redirect to `/login` if unauthenticated
- [ ] 1.3 Login page (React) — email + password form
- [ ] 1.4 Bootstrap admin CLI: `--meta-db` flag creates first admin, prints password to stdout
- [ ] 1.5 API: `POST /api/v1/auth/login`, `POST /api/v1/auth/logout`, `GET /api/v1/auth/me`
- [ ] 1.6 User management page (admin only) — list, create, delete users, change roles
- [ ] 1.7 Session cleanup goroutine — expire old sessions
- [ ] 1.8 RBAC middleware — check role on protected endpoints

**Estimated tests**: ~40

---

### Phase 2: Database Onboarding via Web UI
**Goal**: Add/remove/edit monitored databases through the dashboard.

#### Schema: `sage.databases`
```sql
CREATE TABLE sage.databases (
    id              SERIAL PRIMARY KEY,
    name            TEXT UNIQUE NOT NULL,      -- user-friendly alias
    host            TEXT NOT NULL,
    port            INT NOT NULL DEFAULT 5432,
    database        TEXT NOT NULL,
    username        TEXT NOT NULL,
    password_enc    BYTEA NOT NULL,            -- AES-256-GCM encrypted
    sslmode         TEXT NOT NULL DEFAULT 'require',
    max_connections INT NOT NULL DEFAULT 2,
    enabled         BOOLEAN DEFAULT true,
    created_at      TIMESTAMPTZ DEFAULT now(),
    created_by      INT REFERENCES sage.users(id)
);
```

#### Credential Encryption
- AES-256-GCM with key derived from `--encryption-key` flag or `SAGE_ENCRYPTION_KEY` env var
- Key is never stored in the database
- Passwords encrypted at rest, decrypted in memory only when creating connection pools

#### Tasks
- [ ] 2.1 Encryption helpers: `internal/crypto/` — encrypt/decrypt with AES-256-GCM
- [ ] 2.2 `sage.databases` table in schema bootstrap
- [ ] 2.3 API: `POST /api/v1/databases` — add database (validates connection before saving)
- [ ] 2.4 API: `GET /api/v1/databases` — list (never returns password)
- [ ] 2.5 API: `PUT /api/v1/databases/:id` — edit
- [ ] 2.6 API: `DELETE /api/v1/databases/:id` — remove (stops monitoring, drops pool)
- [ ] 2.7 API: `POST /api/v1/databases/:id/test` — test connection without saving
- [ ] 2.8 "Add Database" page — form: name, host, port, database, user, password, SSL mode
- [ ] 2.9 Connection test feedback in UI — shows PG version, extensions, permissions check
- [ ] 2.10 Fleet manager integration — hot-add/remove databases without restart
- [ ] 2.11 Migrate `--meta-db` flag to replace `postgres:` config block in YAML
- [ ] 2.12 CSV bulk import: `POST /api/v1/databases/import` — up to 50 databases

**Estimated tests**: ~50

---

### Phase 3: Config Management via Web UI
**Goal**: All configuration editable through the dashboard. YAML remains for automation.

#### Config Precedence (highest wins)
1. Per-database overrides in `sage.config` (set via UI)
2. Global overrides in `sage.config` (set via UI)
3. YAML config file (automation / bootstrap)
4. Built-in defaults

#### Tasks
- [ ] 3.1 Settings page — tabbed layout: General, Collector, Analyzer, Trust, LLM, Alerting, Safety
- [ ] 3.2 Per-database settings — override global defaults per DB
- [ ] 3.3 API: `GET /api/v1/config/:database_id` — merged config with source indicators
- [ ] 3.4 API: `PUT /api/v1/config/:database_id` — save per-DB override
- [ ] 3.5 API: `PUT /api/v1/config/global` — save global override
- [ ] 3.6 Config validation — reject invalid values with clear error messages
- [ ] 3.7 Trust level controls — observation/advisory/autonomous selector with explanation
- [ ] 3.8 Execution mode selector: `auto` | `approval` | `manual` (per-database)
- [ ] 3.9 Hot reload — config changes take effect without restart
- [ ] 3.10 Config audit log — who changed what, when

**Estimated tests**: ~35

---

### Phase 4: Action Management via Web UI
**Goal**: Approve/reject, manually trigger, and review all executor actions.

#### Execution Modes
| Mode       | Behavior                                                    |
|------------|-------------------------------------------------------------|
| `auto`     | Current behavior — executor acts per trust level            |
| `approval` | Executor proposes actions → queued until approved in UI     |
| `manual`   | No proposals — user clicks "Take Action" on findings        |

#### Schema Addition
```sql
ALTER TABLE sage.action_log ADD COLUMN approved_by INT REFERENCES sage.users(id);
ALTER TABLE sage.action_log ADD COLUMN approved_at TIMESTAMPTZ;

CREATE TABLE sage.action_queue (
    id              SERIAL PRIMARY KEY,
    database_id     INT REFERENCES sage.databases(id),
    finding_id      INT REFERENCES sage.findings(id),
    proposed_sql    TEXT NOT NULL,
    rollback_sql    TEXT,
    action_risk     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | approved | rejected | expired
    proposed_at     TIMESTAMPTZ DEFAULT now(),
    decided_by      INT REFERENCES sage.users(id),
    decided_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ DEFAULT now() + INTERVAL '7 days'
);
```

#### Tasks
- [ ] 4.1 `sage.action_queue` table in schema bootstrap
- [ ] 4.2 Executor: in `approval` mode, insert into queue instead of executing
- [ ] 4.3 API: `GET /api/v1/actions/pending` — list pending approvals
- [ ] 4.4 API: `POST /api/v1/actions/:id/approve` — approve and execute
- [ ] 4.5 API: `POST /api/v1/actions/:id/reject` — reject with reason
- [ ] 4.6 API: `POST /api/v1/actions/execute` — manual trigger from finding
- [ ] 4.7 Actions page — pending queue with approve/reject buttons
- [ ] 4.8 Finding detail — "Take Action" button (manual mode or ad-hoc)
- [ ] 4.9 Action detail — before/after state, rollback button, execution log
- [ ] 4.10 Expiry goroutine — auto-expire stale pending actions after 7 days

**Estimated tests**: ~45

---

### Phase 5: Notifications
**Goal**: Email and Slack notifications for actions and approvals.

#### Schema
```sql
CREATE TABLE sage.notification_channels (
    id          SERIAL PRIMARY KEY,
    type        TEXT NOT NULL,           -- email | slack
    config      JSONB NOT NULL,          -- {"webhook_url": "..."} or {"smtp_host": "...", ...}
    enabled     BOOLEAN DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE sage.notification_rules (
    id          SERIAL PRIMARY KEY,
    channel_id  INT REFERENCES sage.notification_channels(id),
    event       TEXT NOT NULL,           -- action_executed | action_failed | approval_needed | finding_critical
    min_severity TEXT DEFAULT 'warning',
    enabled     BOOLEAN DEFAULT true
);
```

#### Tasks
- [ ] 5.1 `internal/notify/` package — dispatcher with channel interface
- [ ] 5.2 Slack integration — webhook-based, rich message formatting
- [ ] 5.3 Email integration — SMTP with TLS
- [ ] 5.4 Notification settings page — add/test channels, configure rules
- [ ] 5.5 API: CRUD for channels and rules
- [ ] 5.6 Hook into executor: fire notifications on action events
- [ ] 5.7 Hook into approval queue: notify on new pending actions
- [ ] 5.8 Notification log page — delivery history with status

**Estimated tests**: ~30

---

### Phase 6: Remove MCP Server
**Goal**: Clean removal of all MCP code.

#### Tasks
- [x] 6.1 Remove `main.go` MCP server setup, SSE handler, MCP tools
- [x] 6.2 Remove MCP port from config, docs, Dockerfile, docker-compose
- [x] 6.3 Remove `.mcp.json` files from repo (none existed)
- [x] 6.4 Update README, architecture docs
- [ ] 6.5 Remove MCP-related dependencies from `go.mod`
- [ ] 6.6 Update goreleaser config

**Estimated tests**: negative (removing tests for removed code)

---

### Phase 7: Bug Fixes (can be done in parallel)
- [ ] 7.1 `acted_on_at` set on failed actions prevents retry (executor.go:320)
- [ ] 7.2 Gemini model name — query ListModels API to find valid name
- [ ] 7.3 Graceful shutdown — Ctrl+C hangs, needs signal handling for goroutines

---

## Execution Order

```
Phase 7 (bugs) ──────────────────────────────────► (parallel, anytime)

Phase 1 (auth) → Phase 2 (onboarding) → Phase 3 (config UI)
                                              ↓
                              Phase 4 (actions UI) → Phase 5 (notifications)
                                                          ↓
                                                   Phase 6 (remove MCP)
```

Phase 1 must come first — everything else needs auth.
Phase 6 is last — MCP serves as fallback until the web UI covers all functionality.

## Non-Goals (for now)
- OAuth2/OIDC (follow-up after session auth proves out)
- Multi-sidecar coordination (single sidecar handles ≤50 DBs)
- Audit trail export / compliance reporting
- Mobile-responsive UI (desktop-first)
