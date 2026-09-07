#!/usr/bin/env python3
"""pg_sage capability demo — setup + load generator.

Creates the `pgsage_demo` database on the health_pg container, seeds the
capability scenarios (seed.sql), and then hammers the read queries so they
accumulate in pg_stat_statements where pg_sage's optimizer/advisor can see
them. Each query is sent as a TOP-LEVEL statement (pg_stat_statements default
track='top' ignores nested EXECUTE), so the load is generated client-side and
piped to psql.

Usage:
    python run_demo.py --setup          # create db + run seed.sql (one time)
    python run_demo.py                  # one round of load
    python run_demo.py --rounds 5       # keep totals climbing for a few min
    python run_demo.py --setup --rounds 3

Everything runs through `docker exec health_pg psql` so no local driver or
psql client is required.
"""
import argparse
import os
import random
import subprocess
import sys
import time

CONTAINER = "health_pg"
DB = "pgsage_demo"
SEED_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "seed.sql")


def psql(dbname, sql, ignore_errors=True):
    """Pipe a SQL buffer to psql inside the container; return (rc, out)."""
    flags = "-q" if ignore_errors else "-q -v ON_ERROR_STOP=1"
    cmd = [
        "docker", "exec", "-i", CONTAINER,
        "psql", "-U", "postgres", "-d", dbname, "-X",
    ] + flags.split()
    proc = subprocess.run(cmd, input=sql, capture_output=True,
                          text=True, encoding="utf-8", errors="replace")
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def setup():
    print(f"[setup] creating database {DB} ...")
    # CREATE DATABASE must not run inside a txn block; psql sends it standalone.
    rc, out = psql("postgres",
                   f"DROP DATABASE IF EXISTS {DB} WITH (FORCE);\n"
                   f"CREATE DATABASE {DB};\n")
    if "ERROR" in out:
        print(out)
    with open(SEED_FILE, "r", encoding="utf-8") as f:
        seed = f.read()
    print(f"[setup] seeding scenarios from {os.path.basename(SEED_FILE)} ...")
    rc, out = psql(DB, seed, ignore_errors=False)
    print(out.strip()[-2000:])
    if rc != 0:
        print("[setup] seed FAILED", file=sys.stderr)
        sys.exit(1)
    print("[setup] done.")


def rand_vec(dim=256):
    return "[" + ",".join(f"{random.random():.4f}" for _ in range(dim)) + "]"


def build_load_sql():
    """One round of mixed top-level queries hitting every scenario."""
    parts = []
    types = ["click", "view", "purchase", "signup", "logout"]
    regions = ["NA", "EU", "APAC", "LATAM"]
    statuses = ["pending", "paid", "shipped", "cancelled"]

    # A — missing index on orders.customer_id (point lookups, seq scan).
    for _ in range(120):
        cid = random.randint(1, 5000)
        parts.append(
            f"SELECT count(*), sum(total_cents) FROM orders "
            f"WHERE customer_id = {cid};")

    # B — unindexed FK join orders <-> order_items.
    for _ in range(60):
        cid = random.randint(1, 5000)
        parts.append(
            f"SELECT count(*) FROM orders o JOIN order_items i "
            f"ON i.order_id = o.id WHERE o.customer_id = {cid};")

    # C — jsonb containment (wants a GIN index).
    for _ in range(80):
        t = random.choice(types)
        parts.append(
            f"SELECT count(*) FROM events "
            f"WHERE payload @> '{{\"type\":\"{t}\"}}'::jsonb;")

    # D — vector KNN (wants an HNSW index).
    for _ in range(40):
        parts.append(
            f"SELECT id FROM documents "
            f"ORDER BY embedding <-> '{rand_vec()}'::vector LIMIT 10;")

    # E — big GROUP BY / sort that spills at work_mem=64kB (wants more work_mem).
    for _ in range(25):
        parts.append(
            "SELECT customer_id, count(*) c, sum(total_cents) s FROM orders "
            "GROUP BY customer_id ORDER BY s DESC LIMIT 50;")

    # F — 3-way join with selective filters (query-rewrite / join-hint candidate).
    for _ in range(40):
        r = random.choice(regions)
        s = random.choice(statuses)
        parts.append(
            "SELECT o.id, c.name, i.sku "
            "FROM orders o JOIN customers c ON c.id = o.customer_id "
            "JOIN order_items i ON i.order_id = o.id "
            f"WHERE c.region = '{r}' AND o.status = '{s}' LIMIT 100;")

    random.shuffle(parts)
    return "\n".join(parts) + "\n"


def load_round(n):
    sql = build_load_sql()
    t0 = time.time()
    rc, out = psql(DB, sql)
    dt = time.time() - t0
    errs = out.count("ERROR")
    print(f"[load] round {n}: {sql.count(';')} queries in {dt:.1f}s"
          + (f"  ({errs} errors)" if errs else ""))
    if errs:
        # surface the first error so problems aren't silent
        for line in out.splitlines():
            if "ERROR" in line:
                print("       " + line)
                break


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--setup", action="store_true",
                    help="(re)create + seed the pgsage_demo database first")
    ap.add_argument("--rounds", type=int, default=1,
                    help="how many load rounds to run (default 1)")
    ap.add_argument("--sleep", type=float, default=20.0,
                    help="seconds between rounds (default 20)")
    args = ap.parse_args()

    if args.setup:
        setup()

    for i in range(1, args.rounds + 1):
        load_round(i)
        if i < args.rounds:
            time.sleep(args.sleep)
    print("[done] pg_stat_statements populated — pg_sage will analyze on its "
          "next cycle (~60s).")


if __name__ == "__main__":
    main()
