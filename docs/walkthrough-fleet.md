# pg_sage Fleet Walkthrough: Two Databases, Full Feature Tour

Monitor two PostgreSQL databases from a single pg_sage sidecar. This guide walks
through every feature: fleet dashboard, findings, actions, LLM advisor, notifications,
user roles, hot-reload config, emergency stop, and Prometheus metrics.

**Time**: ~20 minutes
**Platform**: Windows (Git Bash) or Linux/macOS

## Prerequisites

- Docker Desktop running
- Go 1.24+
- Node.js 18+ and npm
- Ports 5433, 5434, 8080, 9187 available

---

## Step 1: Start Two PostgreSQL Instances

From the `pg_sage/` root:

```bash
docker compose up -d pg1 pg2
docker compose ps   # wait for both to show "healthy"
```

This starts:
- **production** -- PostgreSQL 16 on `localhost:5433`
- **staging** -- PostgreSQL 16 on `localhost:5434`

---

## Step 2: Create Databases and Seed Data

### Production (pg1)

```bash
docker exec pg_sage-pg1-1 psql -U postgres -c "CREATE DATABASE app_production;" 2>/dev/null
docker exec -i pg_sage-pg1-1 psql -U postgres -d app_production << 'SQL'
CREATE TABLE IF NOT EXISTS customers (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE IF NOT EXISTS products (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    price NUMERIC(10,2) NOT NULL,
    stock INT DEFAULT 0
);
CREATE TABLE IF NOT EXISTS orders (
    id SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(id),
    status TEXT DEFAULT 'pending',
    total NUMERIC(10,2),
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE IF NOT EXISTS order_items (
    id SERIAL PRIMARY KEY,
    order_id INT REFERENCES orders(id),
    product_id INT REFERENCES products(id),
    quantity INT NOT NULL,
    price NUMERIC(10,2) NOT NULL
);

-- Seed realistic data
INSERT INTO customers (name, email)
SELECT 'Customer ' || i, 'cust' || i || '@example.com'
FROM generate_series(1, 1000) i ON CONFLICT DO NOTHING;

INSERT INTO products (name, price, stock)
SELECT 'Product ' || i, (random()*100)::numeric(10,2), (random()*500)::int
FROM generate_series(1, 200) i ON CONFLICT DO NOTHING;

INSERT INTO orders (customer_id, status, total)
SELECT (random()*999+1)::int,
  CASE (random()*3)::int WHEN 0 THEN 'pending' WHEN 1 THEN 'shipped'
  WHEN 2 THEN 'delivered' ELSE 'cancelled' END,
  (random()*500)::numeric(10,2)
FROM generate_series(1, 20000);

INSERT INTO order_items (order_id, product_id, quantity, price)
SELECT (random()*19999+1)::int, (random()*199+1)::int,
  (random()*5+1)::int, (random()*100)::numeric(10,2)
FROM generate_series(1, 50000);

-- Plant problems for pg_sage to detect:
CREATE INDEX IF NOT EXISTS idx_oi_product ON order_items(product_id);
CREATE INDEX IF NOT EXISTS idx_oi_product_dup ON order_items(product_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_status_dup ON orders(status);
CREATE SEQUENCE IF NOT EXISTS ticket_seq AS integer MAXVALUE 100;
SELECT setval('ticket_seq', 95);
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
```

### Staging (pg2)

```bash
docker exec pg_sage-pg2-1 psql -U postgres -c "CREATE DATABASE app_staging;" 2>/dev/null
docker exec -i pg_sage-pg2-1 psql -U postgres -d app_staging << 'SQL'
CREATE TABLE IF NOT EXISTS customers (
    id SERIAL PRIMARY KEY, name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL, created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE IF NOT EXISTS products (
    id SERIAL PRIMARY KEY, name TEXT NOT NULL,
    price NUMERIC(10,2) NOT NULL, stock INT DEFAULT 0
);
CREATE TABLE IF NOT EXISTS orders (
    id SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(id),
    status TEXT DEFAULT 'pending', total NUMERIC(10,2),
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE IF NOT EXISTS order_items (
    id SERIAL PRIMARY KEY,
    order_id INT REFERENCES orders(id),
    product_id INT REFERENCES products(id),
    quantity INT NOT NULL, price NUMERIC(10,2) NOT NULL
);

INSERT INTO customers (name, email)
SELECT 'Customer '||i, 'cust'||i||'@staging.com'
FROM generate_series(1, 500) i ON CONFLICT DO NOTHING;

INSERT INTO products (name, price, stock)
SELECT 'Product '||i, (random()*80)::numeric(10,2), (random()*300)::int
FROM generate_series(1, 100) i ON CONFLICT DO NOTHING;

INSERT INTO orders (customer_id, status, total)
SELECT (random()*499+1)::int,
  CASE (random()*3)::int WHEN 0 THEN 'pending' WHEN 1 THEN 'shipped'
  WHEN 2 THEN 'delivered' ELSE 'cancelled' END,
  (random()*300)::numeric(10,2)
FROM generate_series(1, 10000);

INSERT INTO order_items (order_id, product_id, quantity, price)
SELECT (random()*9999+1)::int, (random()*99+1)::int,
  (random()*5+1)::int, (random()*80)::numeric(10,2)
FROM generate_series(1, 20000);

-- Same planted problems
CREATE INDEX IF NOT EXISTS idx_oi_product ON order_items(product_id);
CREATE INDEX IF NOT EXISTS idx_oi_product_dup ON order_items(product_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_status_dup ON orders(status);
CREATE SEQUENCE IF NOT EXISTS ticket_seq AS integer MAXVALUE 100;
SELECT setval('ticket_seq', 95);
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
```

---

## Step 3: Configure the Sidecar

Create `e2e_config.yaml` in the `pg_sage/` root. This is the **bootstrap
config** — just the database connections and listen addresses. Everything
else (thresholds, LLM, trust level, etc.) is configured through the web UI
once the sidecar is running.

```yaml
mode: fleet

databases:
  - name: production
    host: localhost
    port: 5433
    user: postgres
    password: postgres
    database: app_production
    sslmode: disable
  - name: staging
    host: localhost
    port: 5434
    user: postgres
    password: postgres
    database: app_staging
    sslmode: disable

api:
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

---

## Step 4: Build and Start

```bash
cd sidecar

# Build React dashboard
cd web && npm ci && npm run build && cd ..

# Build Go binary
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar/

# Start (keep this terminal open)
./pg_sage_sidecar --config ../e2e_config.yaml
```

Watch the logs. Within 15 seconds you should see:
```
INF fleet: registered instance=production
INF fleet: registered instance=staging
INF collector cycle database=production
INF collector cycle database=staging
INF analyzer: 5 findings database=production
INF analyzer: 7 findings database=staging
```

---

## Step 5: Log In

Open **http://localhost:8080** in your browser.

The sidecar bootstraps an admin account automatically if no users exist.
Default credentials:

- **Email:** `admin`
- **Password:** `admin`

If the admin user doesn't exist (check logs for "bootstrapped admin user"), create
one manually:

```bash
# Generate bcrypt hash
cd sidecar
cat > /tmp/hash.go << 'EOF'
package main
import ("fmt"; "golang.org/x/crypto/bcrypt")
func main() {
    h, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
    fmt.Println(string(h))
}
EOF
HASH=$(go run /tmp/hash.go)

# Insert into the production database (sidecar reads auth from first DB)
echo "INSERT INTO sage.users (email, password, role)
VALUES ('admin', E'$HASH', 'admin')
ON CONFLICT (email) DO UPDATE SET password = EXCLUDED.password, role = EXCLUDED.role;" \
| docker exec -i pg_sage-pg1-1 psql -U postgres -d app_production
```

---

## Step 6: Dashboard -- Fleet Overview

After login you land on the **Dashboard**. Verify:

- [ ] **Hero section** shows "2 Databases Monitored"
- [ ] **Summary cards**: Databases=2, Healthy=2, Critical > 0
- [ ] **Database list**: Both "production" and "staging" appear
- [ ] Each database shows a green "connected" dot
- [ ] Trust level shows "advisory" for both
- [ ] Severity badges visible (red=critical, yellow=warning, blue=info)
- [ ] **Recent Recommendations** section shows top findings

---

## Step 7: Findings -- View and Filter

Click **Findings** in the sidebar.

### All databases (default)

- [ ] Findings from both databases are listed
- [ ] Each row shows: Severity badge, Category, Title, Database name, Count
- [ ] Tab bar shows: Open (active), Suppressed, Resolved

### Expected findings

You should see at minimum:

| Severity | Category | Title | DB |
|----------|----------|-------|----|
| Critical | duplicate_index | Duplicate index idx_oi_product | both |
| Critical | duplicate_index | Duplicate index idx_orders_status | both |
| Critical | sequence_exhaustion | ticket_seq 95% consumed | both |
| Warning | slow_query | Slow query (various ms) | production |

### Filter by database

- [ ] Click the **Database Picker** in the sidebar
- [ ] Select "production" -- only production findings appear
- [ ] Select "staging" -- only staging findings appear
- [ ] Select "All Databases" -- both appear again

### Expand a finding

- [ ] Click a duplicate_index finding to expand
- [ ] See: Recommendation text, Recommended SQL, Detail grid, Risk badge
- [ ] The `DROP INDEX CONCURRENTLY ...` SQL is shown
- [ ] Risk badge shows "safe" (green)

---

## Step 8: Take Action -- Fix a Duplicate Index

While viewing an expanded duplicate_index finding:

1. [ ] Click **Take Action**
2. [ ] Confirmation modal shows the SQL and target database
3. [ ] Click **Confirm**
4. [ ] Finding status changes to "resolved"
5. [ ] Success message appears

### Verify the action

- [ ] Navigate to **Actions** in the sidebar
- [ ] **Executed** tab shows the action with "success" outcome
- [ ] Expand the row to see SQL Executed and Rollback SQL
- [ ] Go back to **Findings** -- the finding is now in the "Resolved" tab

### Verify in the database

```bash
docker exec pg_sage-pg1-1 psql -U postgres -d app_production \
  -c "\di public.idx_oi_product*"
# Should show only idx_oi_product_dup (the duplicate was dropped)
```

---

## Step 9: Suppress a Finding

Some findings are intentional (e.g., the ticket_seq is a test sequence).

1. [ ] Find the "ticket_seq 95% consumed" finding
2. [ ] Click the row to expand, then click **Suppress**
3. [ ] Finding disappears from "Open" tab
4. [ ] Click "Suppressed" tab -- finding appears there
5. [ ] Click **Unsuppress** to restore it

---

## Step 10: Notifications -- Channels and Rules

Navigate to **Notifications** in the sidebar (admin only).

### Create a Slack channel

1. [ ] In the **Channels** tab, fill in:
   - Name: `team-alerts`
   - Type: Slack
   - Webhook URL: (paste a real Slack webhook, or use a placeholder for testing)
2. [ ] Click **Create**
3. [ ] Channel appears in the list with "slack" badge

### Create a PagerDuty channel

1. [ ] Name: `oncall-pd`
2. [ ] Type: PagerDuty
3. [ ] Routing Key: (your PD routing key)
4. [ ] Click **Create**

### Test a channel

- [ ] Click **Test** next to the Slack channel
- [ ] If the webhook is valid, status shows "sent"
- [ ] If invalid, error message appears inline

### Create notification rules

In the **Rules** tab:

1. [ ] Create rule: Channel=team-alerts, Event=finding_critical, Severity=critical
2. [ ] Create rule: Channel=oncall-pd, Event=finding_critical, Severity=critical
3. [ ] Rules appear in the list with enabled toggles

### View notification log

- [ ] Click the **Log** tab
- [ ] Test notifications and rule-triggered alerts appear here
- [ ] Each entry shows: timestamp, severity, channel, status

---

## Step 11: User Management

Navigate to **Users** (admin only).

### Create an operator user

1. [ ] Email: `ops@example.com`, Password: `ops123`, Role: operator
2. [ ] Click **Create**
3. [ ] User appears in the list

### Create a viewer user

1. [ ] Email: `viewer@example.com`, Password: `view123`, Role: viewer
2. [ ] Click **Create**

### Test role permissions

1. [ ] Log out (user menu, top-right)
2. [ ] Log in as `viewer@example.com` / `view123`
3. [ ] Verify: Findings visible, but no "Take Action" button
4. [ ] Verify: Settings, Users, Notifications hidden from sidebar
5. [ ] Log out, log in as `ops@example.com` / `ops123`
6. [ ] Verify: Can see and approve pending actions
7. [ ] Verify: Cannot access Settings or Users pages
8. [ ] Log out, log back in as admin

---

## Step 12: LLM Features -- Models and Advisor

### Check available models

Navigate to **Settings > AI & Alerts**. The LLM section shows:
- [ ] Current model: gemini-2.0-flash
- [ ] Endpoint configured and reachable

Or via API:

```bash
curl -s -b cookies http://localhost:8080/api/v1/llm/models
```

Returns models like: gemini-2.5-flash, gemini-2.5-pro, gemini-2.0-flash.

### Enable the advisor

In Settings > Advanced > LLM tab:

1. [ ] Set `advisor.enabled` to true
2. [ ] Click **Save**
3. [ ] Sidecar logs show "advisor: starting analysis"

The advisor will analyze vacuum tuning, WAL settings, memory, connections, and
query rewrites. New findings with LLM-powered recommendations will appear on the
Findings page within 1-2 minutes.

---

## Step 13: Settings -- Hot Reload

### Change a threshold

In Settings (Advanced mode > Analyzer tab):

1. [ ] Change `slow_query_threshold_ms` from 500 to 2000
2. [ ] Click **Save**
3. [ ] Green success banner appears
4. [ ] Sidecar log shows: `config updated key=analyzer.slow_query_threshold_ms`
5. [ ] Next analyzer cycle uses the new threshold

### View audit trail

```bash
curl -s -b cookies http://localhost:8080/api/v1/config/audit | python -m json.tool
```

Shows: who changed it, when, old value (500), new value (2000).

### Reset to default

1. [ ] Click the **Reset** button next to the modified setting
2. [ ] Value reverts to the compiled default
3. [ ] Click **Save**

---

## Step 14: Emergency Stop

1. [ ] Navigate to Settings > General
2. [ ] Click the red **Emergency Stop** button
3. [ ] Confirm the action
4. [ ] Dashboard shows red "STOPPED" banner
5. [ ] No new actions will be executed while stopped
6. [ ] Click **Resume** to restore normal operation
7. [ ] Banner disappears, actions resume

---

## Step 15: Prometheus Metrics

Open **http://localhost:9187/metrics** in a new browser tab.

Verify these metrics exist:

```
pg_sage_info{version="dev",mode="fleet"} 1
pg_sage_fleet_databases 2
pg_sage_fleet_healthy 2
pg_sage_fleet_findings_total <N>
pg_sage_fleet_findings_critical <N>
pg_sage_fleet_instance_findings{database="production"} <N>
pg_sage_fleet_instance_findings{database="staging"} <N>
pg_sage_connection_up 1
```

These can be scraped by Prometheus and visualized in Grafana.

---

## Step 16: API Quick Reference

```bash
# Login
curl -c cookies -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@pgsage.local","password":"password123"}'

# Fleet overview
curl -b cookies http://localhost:8080/api/v1/databases

# Findings (all / filtered)
curl -b cookies "http://localhost:8080/api/v1/findings?limit=20"
curl -b cookies "http://localhost:8080/api/v1/findings?database=staging"

# Suppress / unsuppress
curl -b cookies -X POST http://localhost:8080/api/v1/findings/1/suppress
curl -b cookies -X POST http://localhost:8080/api/v1/findings/1/unsuppress

# Actions
curl -b cookies http://localhost:8080/api/v1/actions

# Config (get / hot-reload update)
curl -b cookies http://localhost:8080/api/v1/config
curl -b cookies -X PUT http://localhost:8080/api/v1/config \
  -H 'Content-Type: application/json' -d '{"analyzer.slow_query_threshold_ms":1000}'

# LLM models
curl -b cookies http://localhost:8080/api/v1/llm/models

# Notifications
curl -b cookies http://localhost:8080/api/v1/notifications/channels
curl -b cookies -X POST http://localhost:8080/api/v1/notifications/channels \
  -H 'Content-Type: application/json' \
  -d '{"name":"slack","type":"slack","config":{"webhook_url":"https://hooks.slack.com/..."}}'

# Emergency stop / resume
curl -b cookies -X POST http://localhost:8080/api/v1/emergency-stop
curl -b cookies -X POST http://localhost:8080/api/v1/resume

# Metrics (no auth)
curl http://localhost:9187/metrics
```

---

## Cleanup

```bash
# Stop sidecar: Ctrl+C in the terminal running it

# Stop containers
docker compose down

# Remove volumes too (deletes all data)
docker compose down -v
```

---

## Architecture

```
 Browser (:8080)
     |
     v
+--------------------------------------------+
|           pg_sage sidecar (Go)             |
|                                            |
|  Collector ---> Snapshots ---> Analyzer    |
|     (15s)                        (30s)     |
|                                    |       |
|  Advisor (LLM) <-- Gemini Flash    |       |
|  Forecaster                        v       |
|  Tuner                    sage.findings    |
|                                    |       |
|  REST API + React Dashboard        |       |
|  Session Auth (bcrypt + cookie)    |       |
|  Notification Dispatcher           |       |
|    -> Slack, PagerDuty, Email      |       |
|                                    v       |
|  Prometheus Metrics (:9187)  Action Log    |
+-----+------------------+------------------+
      |                  |
      v                  v
+----------+      +----------+
| pg1:5433 |      | pg2:5434 |
| prod     |      | staging  |
+----------+      +----------+
```
