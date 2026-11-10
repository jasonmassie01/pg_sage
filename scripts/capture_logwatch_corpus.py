"""Recapture the logwatch classifier corpus from real PostgreSQL servers.

For each major version this starts a throwaway container that writes csvlog
(and jsonlog on PG15+), provokes the events pg_sage's log classifier must
recognise (backend crash, WAL gap, slot invalidation, XX000/XX001 errors,
XID wraparound outage, ...), and writes the server's own log records to
sidecar/internal/logwatch/testdata/pg<major>/<format>/corpus.log.

Each record is preceded by a header line naming the signal the classifier
must produce for it:

    -- expect=log_panic_server_crash event=crash_sigkill source=captured

expect=none marks a lookalike that must not produce any signal. Multixact
limit messages cannot be provoked cheaply, so they are rendered from the
server binary's own format strings into a captured record's skeleton and
tagged source=format-string.

Usage: python scripts/capture_logwatch_corpus.py [--majors 14,15,16,17,18]
           [--image-template pgvector/pgvector:0.8.2-pg{major}]
           [--name-prefix pgsage-corpus]

Only containers named <name-prefix>-<major> are created and removed.

--serve <major> --port <port> --log-dir <dir> instead starts one csvlog
server whose log directory is <dir> on this machine and prints the
SAGE_TEST_CSVLOG_* variables the logwatch crash integration test reads.
Remove it afterwards with: docker rm -f -v <name-prefix>-serve-<major>
"""

from __future__ import annotations

import argparse
import csv
import io
import json
import re
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "sidecar" / "internal" / "logwatch" / "testdata"
PGDATA = "/var/lib/postgresql/data/pgdata"
WRAP_DATA = "/tmp/wrapdata"
WRAP_PORT = "5499"
CSV_COL = {"severity": 11, "state": 12, "message": 13, "backend_type": 23}
JSON_KEY = {"severity": "error_severity", "state": "state_code",
            "message": "message", "backend_type": "backend_type"}


def run(args: list[str], check: bool = True, timeout: int = 180) -> str:
    proc = subprocess.run(args, capture_output=True, text=True,
                          timeout=timeout, encoding="utf-8", errors="replace")
    if check and proc.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout


class Server:
    """One throwaway capture container."""

    def __init__(self, name: str, major: int, image: str):
        self.name, self.major, self.image = name, major, image
        self.formats = ["csvlog"] + (["jsonlog"] if major >= 15 else [])

    def start(self) -> None:
        run(["docker", "rm", "-f", "-v", self.name], check=False)
        run(["docker", "run", "-d", "--name", self.name,
             "-e", "POSTGRES_PASSWORD=postgres", "-e", f"PGDATA={PGDATA}",
             self.image, "postgres",
             "-c", "logging_collector=on",
             "-c", "log_destination=" + ",".join(self.formats),
             "-c", "log_directory=log", "-c", "log_filename=postgresql.log",
             "-c", "lc_messages=C", "-c", "max_wal_size=32MB",
             "-c", "max_slot_wal_keep_size=1MB", "-c", "checkpoint_warning=60s"])
        self.wait_ready()

    def wait_ready(self, port: str = "5432") -> None:
        for _ in range(120):
            probe = subprocess.run(
                ["docker", "exec", "-u", "postgres", self.name, "psql", "-X",
                 "-p", port, "-Atc", "SELECT 1"], capture_output=True, text=True)
            if probe.returncode == 0 and probe.stdout.strip() == "1":
                return
            time.sleep(1)
        raise RuntimeError(f"{self.name}: server on port {port} never became ready")

    def psql(self, sql: str, port: str = "5432", conn: str = "dbname=postgres",
             check: bool = False) -> str:
        return run(["docker", "exec", "-u", "postgres", self.name, "psql", "-X",
                    "-p", port, "-Atq", "-d", conn, "-c", sql], check=check)

    def sh(self, script: str, check: bool = True) -> str:
        return run(["docker", "exec", "-u", "postgres", self.name, "sh", "-c",
                    script], check=check)

    def read_log(self, fmt: str, data_dir: str = PGDATA) -> list[str]:
        ext = "csv" if fmt == "csvlog" else "json"
        text = self.sh(f"cat {data_dir}/log/postgresql.{ext} 2>/dev/null",
                       check=False)
        return split_records(text, fmt)

    def remove(self) -> None:
        run(["docker", "rm", "-f", "-v", self.name], check=False)


def split_records(text: str, fmt: str) -> list[str]:
    """Split a log file into raw records; csvlog quotes may hold newlines."""
    if fmt == "jsonlog":
        return [line for line in text.splitlines() if line.strip()]
    records, start, quoted = [], 0, False
    for i, ch in enumerate(text):
        if ch == '"':
            quoted = not quoted
        elif ch == "\n" and not quoted:
            if text[start:i].strip():
                records.append(text[start:i])
            start = i + 1
    return records


def fields(record: str, fmt: str) -> dict[str, str]:
    if fmt == "jsonlog":
        obj = json.loads(record)
        return {k: str(obj.get(v, "")) for k, v in JSON_KEY.items()}
    row = next(csv.reader(io.StringIO(record)))
    return {k: (row[i] if i < len(row) else "") for k, i in CSV_COL.items()}


# ---------------------------------------------------------------------------
# Events: each provokes server behaviour, then selects the records to keep.
# ---------------------------------------------------------------------------

def kill_backend(srv: Server, signal: str) -> None:
    run(["docker", "exec", "-d", "-u", "postgres", srv.name, "psql", "-X",
         "-c", "SELECT pg_sleep(120) /* corpus-victim */"])
    pid = ""
    for _ in range(30):
        pid = srv.psql("SELECT pid FROM pg_stat_activity WHERE query LIKE "
                       "'%corpus-victim%' AND pid <> pg_backend_pid()").strip()
        if pid:
            break
        time.sleep(0.5)
    if not pid:
        raise RuntimeError(f"{srv.name}: victim backend never appeared")
    run(["docker", "exec", srv.name, "sh", "-c",
         f"kill -{signal} {pid.splitlines()[0]}"])
    time.sleep(2)
    srv.wait_ready()


def fill_wal(srv: Server, rounds: int, checkpoint: bool = True) -> None:
    for i in range(rounds):
        srv.psql(f"DROP TABLE IF EXISTS walfill{i}; CREATE TABLE walfill{i} AS "
                 "SELECT g, repeat('x', 200) AS pad "
                 "FROM generate_series(1, 150000) g", check=True)
        srv.psql("SELECT pg_switch_wal()")
        if checkpoint:
            srv.psql("CHECKPOINT")


def ev_lookalike(srv: Server) -> None:
    for level in ("LOG", "WARNING", "EXCEPTION"):
        srv.psql(f"DO $$BEGIN RAISE {level} 'server process (PID 4242) was "
                 f"terminated by signal 9: Killed'; END$$")
    srv.psql("DO $$BEGIN RAISE LOG 'server process (PID abc) was terminated "
             "by signal 9: Killed'; END$$")


def ev_wal_gap(srv: Server) -> None:
    fill_wal(srv, 3)
    srv.psql("START_REPLICATION 0/1000000",
             conn="dbname=postgres replication=true")


def ev_slot(srv: Server) -> None:
    srv.psql("SELECT pg_create_physical_replication_slot('corpus_slot', true)",
             check=True)
    fill_wal(srv, 3)
    time.sleep(1)
    srv.psql("CHECKPOINT")


def ev_corruption(srv: Server) -> None:
    srv.psql("CREATE TABLE corrupt_me AS SELECT g FROM generate_series(1,100) g;"
             " CHECKPOINT", check=True)
    path = srv.psql("SELECT pg_relation_filepath('corrupt_me')").strip()
    srv.sh(f"printf 'corpus-garbage-not-a-page-header' | dd of={PGDATA}/{path} "
           "bs=1 seek=0 conv=notrunc 2>/dev/null")
    run(["docker", "restart", srv.name])
    srv.wait_ready()
    srv.psql("SELECT count(*) FROM corrupt_me")


def ev_wraparound(srv: Server) -> None:
    """Run a second cluster whose next XID sits past the stop limit.

    pg_resetwal moves the next XID near the wrap point; a single-user VACUUM
    then recomputes the limit from the untouched datfrozenxid values, so the
    restarted server refuses XIDs until autovacuum (forced even when off)
    freezes the ballast table. The client loop wins that race.
    """
    b = f"/usr/lib/postgresql/{srv.major}/bin"
    xid = 2147384347
    seg = f"{xid // 1048576:04X}"
    srv.sh(f"rm -rf {WRAP_DATA} && {b}/initdb -D {WRAP_DATA} -U postgres "
           f"--no-sync >/dev/null 2>&1 && {b}/pg_ctl -D {WRAP_DATA} "
           f"-o '-p {WRAP_PORT}' -w start >/dev/null")
    srv.psql("CREATE TABLE ballast AS SELECT g FROM generate_series(1, 3000000) g",
             port=WRAP_PORT, check=True)
    srv.sh(f"{b}/pg_ctl -D {WRAP_DATA} -w stop >/dev/null && {b}/pg_resetwal "
           f"-x {xid} -D {WRAP_DATA} >/dev/null && dd if=/dev/zero "
           f"of={WRAP_DATA}/pg_xact/{seg} bs=262144 count=1 2>/dev/null && "
           f"echo 'VACUUM pg_class' | {b}/postgres --single -D {WRAP_DATA} "
           "postgres >/dev/null 2>&1")
    opts = (f"-p {WRAP_PORT} -c autovacuum=off -c logging_collector=on "
            f"-c log_destination={','.join(srv.formats)} -c log_directory=log "
            "-c log_filename=postgresql.log -c lc_messages=C")
    srv.sh(f"{b}/pg_ctl -D {WRAP_DATA} -o '{opts}' start >/dev/null; "
           "for i in $(seq 1 300); do psql -X -p " + WRAP_PORT +
           " -qc 'CREATE TABLE wrap_probe (x int)' 2>/dev/null && break; "
           "sleep 0.02; done; sleep 1", check=False)


def sql_event(sql: str):
    return lambda srv: srv.psql(sql)


def msg_has(text: str, server_only: bool = False):
    def select(f: dict[str, str]) -> bool:
        if server_only and f["backend_type"] == "client backend":
            return False
        return text in f["message"]
    return select


def state_is(code: str):
    return lambda f: f["state"] == code


EVENTS = [
    ("crash_sigkill", "log_panic_server_crash", lambda s: kill_backend(s, "9"),
     msg_has("was terminated by signal", server_only=True)),
    ("crash_sigsegv", "log_panic_server_crash", lambda s: kill_backend(s, "11"),
     msg_has("was terminated by signal", server_only=True)),
    ("lookalike_raise", "none", ev_lookalike, msg_has("terminated by signal")),
    ("internal_error", "log_internal_error",
     sql_event("SELECT amvalidate(0)"), state_is("XX000")),
    ("statement_timeout", "log_statement_timeout",
     sql_event("SET statement_timeout = 50; SELECT pg_sleep(2)"),
     state_is("57014")),
    ("temp_file", "log_temp_file_created",
     sql_event("SET log_temp_files = 0; SET work_mem = '64kB'; SELECT count(*) "
               "FROM (SELECT g FROM generate_series(1, 200000) g "
               "ORDER BY g DESC) s"), msg_has("temporary file")),
    ("checkpoint_pressure", "log_checkpoint_too_frequent",
     lambda s: fill_wal(s, 4, checkpoint=False),
     msg_has("checkpoints are occurring too frequently")),
    ("wal_gap", "log_wal_segment_removed", ev_wal_gap,
     msg_has("has already been removed")),
    ("slot_invalidated", "log_replication_slot_inactive", ev_slot,
     msg_has("invalidating")),
    ("program_limit", "none",
     sql_event("SELECT array_fill(1, ARRAY[1000000000])"), state_is("54000")),
    # PG18 I/O workers also log the failed read at LOG level; only the
    # WARNING+ report carries the signal, so those companions are not kept.
    ("data_corruption", "log_data_corruption", ev_corruption,
     lambda f: f["state"] == "XX001" and f["severity"] != "LOG"),
]

# Multixact limit messages, read from the server binary's format strings.
MULTIXACT_FORMATS = [
    (r'database "%s" must be vacuumed before %u more MultiXactIds are used',
     "WARNING", "01000", "log_multixact_wraparound_warning"),
    (r"database with OID %u must be vacuumed before %d more multixact "
     r"members are used", "WARNING", "01000",
     "log_multixact_wraparound_warning"),
    (r'database is not accepting commands that \w+ new MultiXactIds to avoid '
     r'wraparound data loss in database "%s"', "ERROR", "54000",
     "log_multixact_wraparound_outage"),
    (r'multixact "members" limit exceeded', "ERROR", "54000",
     "log_multixact_wraparound_outage"),
]


def render_format(fmt: str) -> str:
    return (fmt.replace('"%s"', '"postgres"').replace("%u", "1234567")
            .replace("%d", "1234567"))


def binary_formats(srv: Server) -> list[tuple[str, str, str, str]]:
    binary = f"/usr/lib/postgresql/{srv.major}/bin/postgres"
    text = srv.sh(f"grep -aoE '[[:print:]]*MultiXactId[[:print:]]*|"
                  f"[[:print:]]*multixact (\"members\"|members)[[:print:]]*' "
                  f"{binary} | sort -u", check=False)
    found = []
    for pattern, level, state, expect in MULTIXACT_FORMATS:
        rx = re.compile("^" + pattern.replace("%", "%") + "$")
        hits = [line for line in text.splitlines() if rx.match(line)]
        if not hits:
            raise RuntimeError(f"{srv.name}: format string {pattern!r} not found")
        found.append((render_format(hits[0]), level, state, expect))
    return found


def rewrite(record: str, fmt: str, level: str, state: str, msg: str) -> str:
    if fmt == "jsonlog":
        obj = json.loads(record)
        obj.update(error_severity=level, state_code=state, message=msg)
        for key in ("detail", "hint", "statement", "context"):
            obj.pop(key, None)
        return json.dumps(obj, separators=(",", ":"))
    row = next(csv.reader(io.StringIO(record)))
    row[11], row[12], row[13] = level, state, msg
    for i in (14, 15, 18, 19):
        row[i] = ""
    buf = io.StringIO()
    csv.writer(buf, lineterminator="").writerow(row)
    return buf.getvalue()


def header(expect: str, event: str, source: str) -> str:
    return f"-- expect={expect} event={event} source={source}"


def capture_events(srv: Server) -> dict[str, list[str]]:
    out: dict[str, list[str]] = {fmt: [] for fmt in srv.formats}
    for event, expect, provoke, select in EVENTS:
        before = {fmt: len(srv.read_log(fmt)) for fmt in srv.formats}
        provoke(srv)
        time.sleep(1.5)
        for fmt in srv.formats:
            kept = [r for r in srv.read_log(fmt)[before[fmt]:]
                    if select(fields(r, fmt))]
            if not kept:
                print(f"warning: {srv.name} {fmt}: {event} produced no record",
                      file=sys.stderr)
            for rec in kept[:4]:
                out[fmt] += [header(expect, event, "captured"), rec]
    return out


def capture_wraparound(srv: Server, out: dict[str, list[str]]) -> None:
    ev_wraparound(srv)
    for fmt in srv.formats:
        records = srv.read_log(fmt, WRAP_DATA)
        for expect, select in (
                ("log_txid_wraparound_warning", msg_has("must be vacuumed within")),
                ("log_wraparound_outage", state_is("54000"))):
            kept = [r for r in records if select(fields(r, fmt))]
            if not kept:
                print(f"warning: {srv.name} {fmt}: no {expect} record",
                      file=sys.stderr)
            for rec in kept[:2]:
                out[fmt] += [header(expect, "xid_stop_limit", "captured"), rec]


def add_multixact(srv: Server, out: dict[str, list[str]]) -> None:
    for fmt in srv.formats:
        skeleton = next((r for r in srv.read_log(fmt)
                         if fields(r, fmt)["state"] == "XX000"), None)
        if skeleton is None:
            raise RuntimeError(f"{srv.name} {fmt}: no XX000 record to reuse")
        for msg, level, state, expect in binary_formats(srv):
            out[fmt] += [header(expect, "multixact_limit", "format-string"),
                         rewrite(skeleton, fmt, level, state, msg)]


def capture(major: int, image: str, prefix: str) -> None:
    srv = Server(f"{prefix}-{major}", major, image.format(major=major))
    try:
        srv.start()
        out = capture_events(srv)
        capture_wraparound(srv, out)
        add_multixact(srv, out)
        for fmt, lines in out.items():
            path = OUT / f"pg{major}" / fmt / "corpus.log"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("\n".join(lines) + "\n", encoding="utf-8",
                            newline="\n")
            print(f"wrote {path.relative_to(ROOT)} ({len(lines) // 2} records)")
    finally:
        srv.remove()


def serve(major: int, image: str, prefix: str, port: int, log_dir: str) -> None:
    host_dir = Path(log_dir).resolve()
    host_dir.mkdir(parents=True, exist_ok=True)
    name = f"{prefix}-serve-{major}"
    srv = Server(name, major, image.format(major=major))
    run(["docker", "rm", "-f", "-v", name], check=False)
    run(["docker", "run", "-d", "--name", name, "-p", f"127.0.0.1:{port}:5432",
         "-v", f"{host_dir}:/pglog", "-e", "POSTGRES_PASSWORD=postgres",
         "-e", f"PGDATA={PGDATA}", srv.image, "postgres",
         "-c", "logging_collector=on", "-c", "log_destination=csvlog",
         "-c", "log_directory=/pglog", "-c", "log_filename=postgresql.log",
         "-c", "log_file_mode=0644", "-c", "lc_messages=C"])
    srv.wait_ready()
    print(f"SAGE_TEST_CSVLOG_URL=postgres://postgres:postgres@127.0.0.1:{port}/"
          "postgres?sslmode=disable")
    print(f"SAGE_TEST_CSVLOG_CONTAINER={name}")
    print(f"SAGE_TEST_CSVLOG_DIR={host_dir}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--majors", default="14,15,16,17,18")
    parser.add_argument("--image-template",
                        default="pgvector/pgvector:0.8.2-pg{major}")
    parser.add_argument("--name-prefix", default="pgsage-corpus")
    parser.add_argument("--serve", type=int, metavar="MAJOR")
    parser.add_argument("--port", type=int, default=56499)
    parser.add_argument("--log-dir", default="pglog-crash")
    args = parser.parse_args()
    if args.serve:
        serve(args.serve, args.image_template, args.name_prefix, args.port,
              args.log_dir)
        return 0
    for major in (int(m) for m in args.majors.split(",")):
        capture(major, args.image_template, args.name_prefix)
    return 0


if __name__ == "__main__":
    sys.exit(main())
