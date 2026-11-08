#!/usr/bin/env python3
"""Exact-owned, bounded Lakebase demo fixtures; credentials remain in memory.

Select an isolated development endpoint and database explicitly. Only the fixed
nerdlab_20261030 schema is managed; this is not a production load generator or
an automatic action runner. See docs/databricks-native.md before use.
"""
import argparse
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import time

SCHEMA = "nerdlab_20261030"
SPILL_QUERY = f"SELECT sum(length(payload)) FROM (SELECT payload FROM {SCHEMA}.scan_20261030 ORDER BY payload OFFSET 0) sorted"


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["preflight", "seed", "exercise", "slow-query", "status", "cleanup"])
    parser.add_argument("--seconds", type=int, default=120)
    for name in ("profile", "endpoint", "host", "database", "user"):
        parser.add_argument("--" + name, default=os.getenv("SAGE_NERDLAB_" + name.upper()),
                            help="Explicit target; alternatively set SAGE_NERDLAB_" + name.upper())
    parser.add_argument("--manifest", type=Path,
                        default=Path(".local-test-results/nerdlab-app-fixtures.json"),
                        help="Private per-target ownership/budget ledger; retain between operations")
    args = parser.parse_args(argv)
    for name in ("profile", "endpoint", "host", "database", "user"):
        if not getattr(args, name) or getattr(args, name).startswith("-"):
            parser.error("--" + name + " is required and must be an explicit target value")
    if not re.fullmatch(r"projects/[A-Za-z0-9_-]+/branches/[A-Za-z0-9_-]+/endpoints/[A-Za-z0-9_-]+", args.endpoint):
        parser.error("--endpoint must be a full Lakebase endpoint resource path")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*", args.host):
        parser.error("--host must be a direct endpoint hostname, without a URL or port")
    if not 1 <= args.seconds <= 300:
        parser.error("exercise duration must be between 1 and 300 seconds")
    return args


def main():
    args = parse_args()
    endpoint, database = args.endpoint, args.database
    target = {name: getattr(args, name) for name in ("endpoint", "host", "database", "user")}
    MANIFEST = args.manifest
    MANIFEST.parent.mkdir(parents=True, exist_ok=True)
    lock_handle=MANIFEST.with_suffix(".lock").open("a")
    try:
        fcntl.flock(lock_handle,fcntl.LOCK_EX|fcntl.LOCK_NB)
    except BlockingIOError:
        raise RuntimeError("Another fixture harness operation is already running") from None
    endpoint_result = subprocess.run(
        ["databricks", "postgres", "get-endpoint", endpoint, "--profile", args.profile, "-o", "json"],
        capture_output=True, text=True, check=True, timeout=60,
    )
    hosts = json.loads(endpoint_result.stdout).get("status", {}).get("hosts", {})
    if args.host.lower().rstrip(".") != hosts.get("host", "").lower().rstrip("."):
        raise RuntimeError("Host does not match the endpoint API's direct read-write host; refusing")
    credential = subprocess.run(
        ["databricks", "postgres", "generate-database-credential", endpoint, "--profile", args.profile, "-o", "json"],
        capture_output=True, text=True, check=True, timeout=60,
    )
    env = dict(os.environ, PGHOST=args.host, PGPORT="5432", PGDATABASE=database,
               PGUSER=args.user, PGSSLMODE="verify-full",
               PGSSLROOTCERT="system", PGPASSWORD=json.loads(credential.stdout)["token"],
               PGAPPNAME="nerdlab_20261030")

    def sql(statement, expected_failure=False):
        result = subprocess.run(["psql", "-XqAt", "-v", "ON_ERROR_STOP=1"],
                                input="SET statement_timeout='20s'; SET lock_timeout='5s';\n" + statement,
                                env=env, capture_output=True, text=True, timeout=35)
        if expected_failure:
            if result.returncode == 0 or "could not create unique index" not in result.stderr:
                raise RuntimeError("Expected duplicate-key index build failure did not occur")
        elif result.returncode:
            raise RuntimeError("Scoped fixture SQL failed: " + result.stderr.strip())
        return result.stdout.strip()

    def identity():
        return json.loads(sql(f"""SELECT json_build_object('database',current_database(),
          'database_oid',(SELECT oid FROM pg_database WHERE datname=current_database()),'user',current_user,
          'schema', (SELECT json_build_object('oid',oid,'owner',pg_get_userbyid(nspowner))
                     FROM pg_namespace WHERE nspname='{SCHEMA}'));"""))

    def inventory():
        return json.loads(sql(f"""SELECT COALESCE(json_agg(x ORDER BY x.oid),'[]'::json) FROM (
          SELECT c.oid,c.relname,c.relkind,pg_get_userbyid(c.relowner) AS owner
          FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
          WHERE n.nspname='{SCHEMA}')x;"""))

    def check_owned_manifest():
        saved = json.loads(MANIFEST.read_text())
        if saved.get("endpoint") != endpoint or saved.get("target", target) != target:
            raise RuntimeError("Fixture manifest belongs to a different target; refusing")
        current = identity()
        if current != saved["identity"] or current["schema"]["owner"] != current["user"]:
            raise RuntimeError("Fixture database, schema OID, or owner changed; refusing")
        if inventory() != saved["objects"]:
            raise RuntimeError("Fixture object inventory changed; review exact OIDs before cleanup or exercise")
        return saved

    current = identity()
    if current["database"] != database or current["user"] != env["PGUSER"]:
        raise RuntimeError("Unexpected database or principal")
    if args.action == "preflight":
        print(json.dumps(current, indent=2))
        return
    if args.action == "seed":
        if current["schema"] is not None or MANIFEST.exists():
            raise RuntimeError("Fixture schema or manifest already exists; seed refuses reuse")
        created = datetime.now(timezone.utc).isoformat()
        sql(f"CREATE SCHEMA {SCHEMA}; COMMENT ON SCHEMA {SCHEMA} IS 'Exact-owned pg_sage Lakebase nerdlab fixture; remove using manifest guard';")
        try:
            sql(f"""
CREATE TABLE {SCHEMA}.dups_20261030(id int,a int);
INSERT INTO {SCHEMA}.dups_20261030 SELECT g,g%100 FROM generate_series(1,12000)g;
CREATE INDEX dup_a_one ON {SCHEMA}.dups_20261030(a);
CREATE INDEX dup_a_two ON {SCHEMA}.dups_20261030(a);
CREATE TABLE {SCHEMA}.parent_20261030(id int PRIMARY KEY);
INSERT INTO {SCHEMA}.parent_20261030 SELECT generate_series(1,100);
CREATE TABLE {SCHEMA}.child_20261030(id int,parent_id int REFERENCES {SCHEMA}.parent_20261030(id));
INSERT INTO {SCHEMA}.child_20261030 SELECT g,1+g%100 FROM generate_series(1,12000)g;
CREATE TABLE {SCHEMA}.stale_20261030(id int) WITH (autovacuum_enabled=false);
INSERT INTO {SCHEMA}.stale_20261030 SELECT generate_series(1,12000);
VACUUM {SCHEMA}.stale_20261030;
CREATE TABLE {SCHEMA}.bloat_20261030(id int,payload text) WITH (autovacuum_enabled=false);
INSERT INTO {SCHEMA}.bloat_20261030 SELECT g,repeat('x',40) FROM generate_series(1,12000)g;
ANALYZE {SCHEMA}.bloat_20261030;
DELETE FROM {SCHEMA}.bloat_20261030 WHERE id<=9000;
CREATE TABLE {SCHEMA}.scan_20261030(id int,payload text);
INSERT INTO {SCHEMA}.scan_20261030 SELECT g,md5(g::text)||md5((g+1)::text) FROM generate_series(1,110000)g;
ANALYZE {SCHEMA}.scan_20261030;
CREATE SEQUENCE {SCHEMA}.pressure_20261030 AS integer MAXVALUE 1000 START 950;
SELECT nextval('{SCHEMA}.pressure_20261030');
SELECT pg_stat_force_next_flush();
""")
            sql(f"CREATE UNIQUE INDEX CONCURRENTLY invalid_a ON {SCHEMA}.dups_20261030(a);", expected_failure=True)
            # Exactly 105 bounded scans; no connection/CPU saturation fixture.
            sql("\n".join([f"SELECT count(*) FROM {SCHEMA}.scan_20261030 WHERE id=-1;" for _ in range(105)]) + "\nSELECT pg_stat_force_next_flush();")
            # Establish the exact normalized query without a spill so the new
            # analyzer can compare a real baseline after deployment.
            sql("BEGIN; SET LOCAL work_mem='64MB'; " + SPILL_QUERY + "; COMMIT;")
        finally:
            MANIFEST.parent.mkdir(parents=True, exist_ok=True)
            record = {"created_at": created, "endpoint": endpoint, "target": target, "identity": identity(),
                      "objects": inventory(), "exercise_executions": 0,
                      "read_scan_budget": {"seed": 106, "exercise_max": 4, "total_max": 110},
                      "cleanup": "Repeat the same explicit target and manifest with action cleanup"}
            MANIFEST.write_text(json.dumps(record, indent=2) + "\n")
            MANIFEST.chmod(0o600)
        print(json.dumps(record, indent=2))
    elif args.action == "exercise":
        saved=check_owned_manifest()
        remaining=4-saved.get("exercise_executions",0)
        if remaining<=0:
            raise RuntimeError("The cumulative four-query exercise budget is exhausted")
        end = time.monotonic() + args.seconds
        runs = 0
        while time.monotonic() < end and runs<remaining:
            # Reserve before executing so an interrupted query still counts.
            saved["exercise_executions"]+=1
            MANIFEST.write_text(json.dumps(saved,indent=2)+"\n")
            sql("BEGIN; SET LOCAL work_mem='64kB'; " + SPILL_QUERY + "; COMMIT;")
            runs += 1
            if runs<remaining:
                time.sleep(min(args.seconds/remaining, max(0, end-time.monotonic())))
        print(json.dumps({"low_work_mem_executions": runs, "seconds": args.seconds}))
    elif args.action == "slow-query":
        saved=check_owned_manifest()
        if saved.get("slow_query") is not None:
            raise RuntimeError("The single constant-result slow-query budget is already reserved")
        nonce=time.time_ns()
        label=f"nerdslow_{nonce:x}"
        shape="0"+"".join(" + 0" if nonce&(1<<bit) else " - 0" for bit in range(64))
        query=f"SELECT pg_sleep(1.2) AS {label}, ({shape}) AS constant_result"
        # VERBOSE without ANALYZE obtains the parsed query identifier without
        # calling pg_sleep. Refuse if queryID cannot be proved fresh beforehand.
        plan=json.loads(sql("EXPLAIN (VERBOSE,FORMAT JSON) "+query))
        raw_id=plan[0].get("Query Identifier")
        if raw_id is None:
            raise RuntimeError("EXPLAIN did not expose a query identifier; no slow statement executed")
        query_id=int(raw_id)
        if query_id>=(1<<63): query_id-=1<<64
        collisions=sql(f"""SELECT (SELECT count(*) FROM pg_stat_statements
          WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND queryid={query_id}) +
          (SELECT count(*) FROM sage.query_store WHERE queryid={query_id});""")
        if query_id==0 or collisions!="0":
            raise RuntimeError("Slow-query identifier is not fresh; no slow statement executed")
        record={"label":label,"queryid":query_id,"preflight_collisions":0,
                "reserved_at":datetime.now(timezone.utc).isoformat(),"execution_budget":1,
                "sleep_seconds":1.2,"table_scans":0,"resets":0}
        saved["slow_query"]=record
        MANIFEST.write_text(json.dumps(saved,indent=2)+"\n")
        sql("SET statement_timeout='5s'; "+query+";")
        measured=json.loads(sql(f"""SELECT json_build_object('calls',calls,'mean_exec_ms',mean_exec_time)
          FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
          AND userid=(SELECT oid FROM pg_roles WHERE rolname=current_user) AND queryid={query_id}
          AND strpos(query,'{label}')>0;"""))
        if measured["calls"]!=1 or measured["mean_exec_ms"]<=1000:
            raise RuntimeError("Actual one-call default-threshold timing was not proved")
        record.update(measured)
        record["completed_at"]=datetime.now(timezone.utc).isoformat()
        MANIFEST.write_text(json.dumps(saved,indent=2)+"\n")
        MANIFEST.with_name("nerdlab-app-slow-query.json").write_text(json.dumps(record,indent=2)+"\n")
        print(json.dumps(record,indent=2))
    elif args.action == "status":
        check_owned_manifest()
        print(sql(f"""SELECT json_build_object('findings',COALESCE(json_agg(x),'[]'::json)) FROM (
          SELECT id,category,severity,status,object_identifier,title FROM sage.findings
          WHERE object_identifier LIKE '{SCHEMA}.%' OR
            (category='temp_spill' AND strpos(detail->>'query','{SCHEMA}.')>0)
          ORDER BY category,id)x;"""))
    elif args.action == "cleanup":
        saved=check_owned_manifest()
        tables=", ".join(f"{SCHEMA}.{name}_20261030" for name in ["child","parent","dups","stale","bloat","scan"])
        objects=json.dumps(saved["objects"],separators=(",",":"))
        schema_oid=saved["identity"]["schema"]["oid"]
        database_oid=saved["identity"]["database_oid"]
        # All DROP statements use RESTRICT. An outside dependent object makes
        # the transaction fail instead of extending the cleanup scope. Table
        # locks and the existing sequence's unchanged OWNED BY NONE option
        # retain object locks through the OID/owner recheck and deletion.
        sql(f"""BEGIN;
LOCK TABLE {tables} IN ACCESS EXCLUSIVE MODE;
ALTER SEQUENCE {SCHEMA}.pressure_20261030 OWNED BY NONE;
DO $guard$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_database WHERE datname=current_database() AND oid={database_oid}) OR
    NOT EXISTS(SELECT FROM pg_namespace WHERE nspname='{SCHEMA}' AND oid={schema_oid}
      AND nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user)) OR
    (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.oid),'[]'::jsonb) FROM (
      SELECT c.oid,c.relname,c.relkind,pg_get_userbyid(c.relowner) AS owner
      FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
      WHERE n.nspname='{SCHEMA}')x) <> $inventory${objects}$inventory$::jsonb
 THEN RAISE EXCEPTION 'Fixture database/schema/object OID or ownership changed'; END IF;
END $guard$;
DROP TABLE {tables};
DROP SEQUENCE {SCHEMA}.pressure_20261030;
DROP SCHEMA {SCHEMA};
COMMIT;""")
        saved=json.loads(MANIFEST.read_text());saved["cleaned_at"]=datetime.now(timezone.utc).isoformat()
        MANIFEST.write_text(json.dumps(saved,indent=2)+"\n")
        print("Exact-owned fixture schema removed; evidence manifest retained")


if __name__ == "__main__":
    main()
