# Agent Guard: agent posture

<!-- The product name appears only in the title above. To rename, change that line. -->

pg_sage's agent posture check reports how exposed a PostgreSQL database is to the AI agents
and untrusted clients that connect to it. This covers MCP servers, coding agents, LangGraph
or CrewAI workers, and PostgREST/Supabase anonymous roles. It reads only the system catalog
and flags the grants, roles, policies and functions that let an agent do more than its job:
- an agent role with `BYPASSRLS`, or one that owns tables;
- a table the anonymous role can read with row-level security off;
- a definer function anyone can call and hijack through `search_path`.

Posture is on by default and needs no configuration. It works on any PostgreSQL 14–18,
self-managed or managed, and needs only pg_sage's usual `pg_monitor` role. Every fix it
proposes is a SQL script for you to review and run. pg_sage never runs a posture fix
itself: posture findings stop at autonomy level L1.

## Where you see it

- **First look.** The catalog-only first look of every new database has an **Agent posture**
  section: one check per detector, its findings with catalog evidence, and the fix script.
  It runs in the first look's own read-only transaction, under the same 5 s per-statement
  budget, and gets one retry for a check that a transient error (a timeout, a lock) degraded.
- **Findings.** The analyzer runs posture again:
  - on its first cycle;
  - whenever the catalog rows posture reads change;
  - once a day at `agents.posture.daily_at`.

  The catalog check uses pg_sage's own hash of roles, memberships, per-role settings, ACLs,
  ownership, row-level security, policies, view rules and extension versions. Data changes,
  `ANALYZE` and `VACUUM` don't trigger a run.
  - Each problem is a finding in `sage.findings` with the category `agent_posture:AP-NN`.
    It resolves on its own once a completed check no longer sees it.
  - The fix script is stored as `detail.manual_script`, with `detail.proposal_level = L1`.
  - The finding has no `recommended_sql`, so the executor never acts on it.

Every posture statement carries the `/* pg_sage first_look */ /* agent_posture AP-NN */` tag,
so its cost shows in `pg_stat_statements` and in pg_sage's performance gate.

## Which roles count

**Exposed roles** are the roles untrusted clients reach:
- `PUBLIC`, always;
- every role listed in `agents.exposed_roles`;
- `anon` and `authenticated`, added automatically when both exist (Supabase).

A role is listed under `missing` when it's configured but doesn't exist. Privileges are
read with `aclexplode` on the object's ACL, or its default ACL. A grant counts only when
the exposed role (or `PUBLIC`) also has `USAGE` on the object's schema. Privileges an
exposed role inherits through membership in another role are not followed.

**Agent roles** come in two kinds:
- **Registered** agent roles follow pg_sage's agent role naming: `sage_agent_` or
  `sage_agentb_` plus 10 lower-case base32 characters. Until pg_sage manages agent identities
  itself, this naming is what makes a role registered.
- **Client hints**: a role with a session whose `application_name` matches one of
  `agents.client_patterns`. Matching is anchored and case-insensitive, and the default
  patterns are `^mcp`, `^claude`, `^cursor`, `^codex`, `^langgraph` and `^crewai`. A hint
  is a guess, so the same problem on a hinted role is reported one severity lower.

pg_sage's own role is never counted as an agent.

## Detectors

| Id | Reports | Severity | Fix script |
|---|---|---|---|
| AP-01 | An agent role that is superuser or holds `BYPASSRLS`, `CREATEROLE`, `CREATEDB` or `REPLICATION`, or is a member (directly or not) of `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files` or `pg_write_all_data` | critical (registered), warning (hint) | `ALTER ROLE … NO…`; `REVOKE <group> FROM …` for direct memberships |
| AP-02 | Objects owned by an agent role, in this database or the cluster's shared catalogs (from `pg_shdepend`), with up to five named | critical (registered), warning (hint) | `REASSIGN OWNED BY … TO app_owner` |
| AP-03 | A table with row-level security off that grants `SELECT`, `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE` to an exposed role or `PUBLIC`. Also a view granted to one that runs as its owner over such a table | critical | `ENABLE ROW LEVEL SECURITY` (write the policies first), or `REVOKE ALL … FROM …` |
| AP-04 | A permissive policy for an exposed role or `PUBLIC` whose `USING` or `WITH CHECK` is plain `true` | warning | `ALTER POLICY … USING (<predicate>)`, or `DROP POLICY` |
| AP-05 | A `SECURITY DEFINER` function or procedure that an exposed role (or `PUBLIC`, the default) can execute, with no `search_path` in its settings | warning | `ALTER FUNCTION … SET search_path = pg_catalog, pg_temp`; optionally `REVOKE EXECUTE` |
| AP-06 | A view in an exposed schema (one where an exposed role has `USAGE`) over a table with row-level security, without `security_invoker`. On PostgreSQL 14 it's reported as "no security_invoker available" | warning | PostgreSQL 15+: `ALTER VIEW … SET (security_invoker = true)`. PostgreSQL 14: revoke the view from exposed roles |
| AP-07 | A schema where `PUBLIC` holds `CREATE` (PostgreSQL 14's `public` schema by default) | warning | `REVOKE CREATE ON SCHEMA … FROM PUBLIC` |
| AP-08 | A non-superuser login role without a non-zero `statement_timeout` or `idle_in_transaction_session_timeout`. Settings count when made for the role (in this database or all), for the database, or in the server configuration where pg_sage's session can see it | info | `ALTER ROLE … SET statement_timeout = '30s'` and `idle_in_transaction_session_timeout = '60s'` |
| AP-09 | Remote access or host code an exposed role (or `PUBLIC`) can reach: `dblink` functions it can execute, the `postgres_fdw`/`dblink_fdw` wrappers and their servers it can use, a user mapping (stored credentials) for it on such a server, and an untrusted language (`plperlu`, `plpython3u`, `pltclu`, …) marked trusted | critical | `REVOKE EXECUTE ON FUNCTION … FROM …`, `REVOKE USAGE ON FOREIGN DATA WRAPPER/SERVER/LANGUAGE …`, `DROP USER MAPPING FOR … SERVER …` |
| AP-10 | pgvector below 0.8.4 while HNSW indexes exist (vacuum fixes), or below 0.8.7 while IVFFlat indexes exist (index build overflow); the server's major at, past or within 90 days of its end of life | critical (pgvector), warning (end of life) | `ALTER EXTENSION vector UPDATE` after installing the fixed release; the upgrade plan |
| AP-11 | No point-in-time recovery: on a self-managed server `archive_mode = off`, or on with nothing archiving; on RDS, Aurora and Cloud SQL, automated backups or PITR off or retention 0, as cloud telemetry last read the instance. Deletion protection off (RDS, Cloud SQL). No pgaudit (neither preloaded nor installed) while agent roles use the database | warning | `ALTER SYSTEM SET archive_mode …`; the provider's CLI command; `CREATE EXTENSION pgaudit` after preloading it |
| AP-12 | A schema whose LangGraph checkpoint tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) grew more than `agents.posture.memory_growth_gb_day` GB (GiB) a day with no rows deleted | info | A query for the largest threads, to delete past your retention |
| AP-13 | A login role used at once by 3 or more distinct clients (`application_name` and client address), at least one of them agent-like | info; warning once any registered agent role exists | `CREATE ROLE <login>_agent LOGIN NOINHERIT`, then move the agent to it |
| AP-14 | pg_sage's own role is a superuser, holds `BYPASSRLS`, or has the privileges of an agent role | warning | `ALTER ROLE … NOBYPASSRLS`; `REVOKE <agent> FROM …` (PostgreSQL 14/15) or `REVOKE INHERIT OPTION FOR <agent> FROM …` (16+); a dedicated non-superuser role |
| AP-15 | The `PUBLIC` baseline: tables, views and foreign tables granted to `PUBLIC` (one finding per schema), and default privileges that grant `PUBLIC` on new tables or sequences. Extension-owned objects are skipped | warning | `REVOKE ALL ON TABLE … FROM PUBLIC`; `ALTER DEFAULT PRIVILEGES FOR ROLE … REVOKE ALL ON TABLES FROM PUBLIC` |
| AP-16 | Once any registered agent role exists, a login role seen with an agent-like `application_name` that isn't one (an agent outside Agent Guard) | warning | `ALTER ROLE … NOLOGIN` after the agent moves to its own credentials |

AP-03 and AP-06 skip tables and views an extension owns (pg_hint_plan's `hint_plan.hints`,
for example): those grants come from the extension, not from you.
AP-08 skips pg_sage's own role, reserved `pg_` roles and managed-service admin logins
(`rdsadmin`, `cloudsqladmin`, `azure_superuser`, `alloydbadmin` and similar).

Notes on AP-09 to AP-16:
- **AP-10.** The pgvector fix releases come from pg_sage's research of pgvector's changelog;
  no CVE id is cited because none is verified. End-of-life dates are from the PostgreSQL
  versioning policy (PostgreSQL 14: 2026-11-12). A major older than 13 counts as past end of
  life; a major newer than 18 isn't reported until its date is known.
- **AP-11.** The provider check uses what cloud telemetry already reads with the instance
  (no extra API call); without telemetry nothing is reported, never "off". pg_sage can't see
  whether backups are immutable or kept after the instance is deleted, so check your
  provider's backup vault or retained-backup settings yourself. A tool that streams WAL
  (`pg_receivewal`, Barman streaming) can give PITR with `archive_mode` off; the caveat says so.
  On PostgreSQL 14 the `archive_library` arm is skipped (the setting is 15+).
- **AP-12** needs two observations at least an hour apart, kept in memory by the analyzer's
  posture run: the first run records, a later run (usually the next daily one) compares.
  The first look has none and reports nothing. After a restart, the next finding waits for
  two new observations, so it can come a day later.
- **AP-13** needs `pg_read_all_stats` (part of `pg_monitor`) to see other roles' client
  addresses. Without it, clients are counted by `application_name` only, and the finding's
  caveat says so.
- **AP-14.** pg_sage also warns once at startup when it connects as a superuser: Agent Guard
  features need a non-superuser role. Posture still runs. Inheritance is read with
  `pg_has_role` on every version; the arms decide the fix (PostgreSQL 14/15 inherit per role,
  16+ per grant).

**Version arms.** A detector part that applies only to some PostgreSQL versions is an *arm*.
On other versions the arm is skipped, and the check's note records why. For example, on
PostgreSQL 14, AP-06's note reads "arm security_invoker skipped: security_invoker views need
PostgreSQL 15+".

## Configuration

```yaml
agents:
  exposed_roles: []        # roles untrusted clients reach; PUBLIC always counts,
                           # anon + authenticated are added when both exist
  client_patterns: ["^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"]
                           # anchored, case-insensitive; [] disables hints
  posture:
    memory_growth_gb_day: 5  # agent memory-store growth reported (AP-12)
    daily_at: "03:00"        # local HH:MM of the daily posture run
```

| Key | Class | Notes |
|---|---|---|
| `agents.exposed_roles` | safety_critical | `PUBLIC` is implicit and can't be listed |
| `agents.client_patterns` | operator_preference | Each pattern must start with `^` |
| `agents.posture.memory_growth_gb_day` | operator_preference | Greater than 0 |
| `agents.posture.daily_at` | operator_preference | `H:MM` or `HH:MM`, 24-hour |

The keys take effect on restart. The analyzer's posture run reads them each time it runs.

## Manual runbook

These are the SQL statements you run yourself when pg_sage is down or you want to check by
hand. Run them as a role that can read the catalog (`pg_monitor` is enough to read). The
containment steps need `CREATEROLE` or superuser.

**Contain registered agent roles** (stop new logins, end live sessions):

```sql
DO $$ DECLARE r record; BEGIN
  FOR r IN SELECT rolname FROM pg_roles WHERE rolname ~ '^sage_agentb?_[a-z2-7]{10}$' LOOP
    EXECUTE format('ALTER ROLE %I NOLOGIN CONNECTION LIMIT 0', r.rolname);
  END LOOP; END $$;
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE usename ~ '^sage_agentb?_[a-z2-7]{10}$';
```

For an agent that logs in with a role of your own, run the same two statements for that role
name. Do this on the primary and every replica the agent can reach.

**Find agent-like sessions** (the AP-01 hint):

```sql
SELECT DISTINCT usename, application_name, client_addr
FROM pg_stat_activity
WHERE backend_type = 'client backend'
  AND application_name ~* '^(mcp|claude|cursor|codex|langgraph|crewai)';
```

**Dangerous agent role attributes** (AP-01):

```sql
SELECT rolname, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb, rolreplication
FROM pg_roles WHERE rolname = 'my_agent_role';
SELECT g.rolname FROM pg_roles g
WHERE g.rolname IN ('pg_execute_server_program', 'pg_write_server_files',
                    'pg_read_server_files', 'pg_write_all_data')
  AND pg_has_role('my_agent_role', g.oid, 'MEMBER');
```

**Tables exposed without row-level security** (AP-03; replace `anon` with your exposed role):

```sql
SELECT n.nspname, c.relname, a.privilege_type
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
WHERE c.relkind IN ('r', 'p') AND NOT c.relrowsecurity
  AND a.grantee IN (0, 'anon'::regrole)
  AND n.nspname NOT IN ('pg_catalog', 'information_schema');
```

**Definer functions without a pinned search_path** (AP-05):

```sql
SELECT p.oid::regprocedure FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prosecdef AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) s WHERE s LIKE 'search_path=%');
```

**Schemas where anyone can create** (AP-07):

```sql
SELECT n.nspname FROM pg_namespace n
CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a
WHERE a.grantee = 0 AND a.privilege_type = 'CREATE';
```

**dblink callable by anyone** (AP-09):

```sql
SELECT p.oid::regprocedure FROM pg_proc p
JOIN pg_depend d ON d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'dblink'
WHERE has_function_privilege('public', p.oid, 'EXECUTE');
```

**WAL archiving and pgaudit** (AP-11):

```sql
SELECT name, setting FROM pg_settings
WHERE name IN ('archive_mode', 'archive_command', 'archive_library', 'shared_preload_libraries');
```

**Relations granted to PUBLIC** (AP-15):

```sql
SELECT n.nspname, c.relname, a.privilege_type
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) a
WHERE a.grantee = 0 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema');
```

Review each fix script before you run it. Enabling row-level security without a policy hides
every row from roles other than the owner. Pinning a function's `search_path` requires its
body to schema-qualify what it uses. A role timeout must fit the role's longest legitimate
statement.
