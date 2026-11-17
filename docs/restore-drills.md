# Restore drills on self-managed servers

A restore drill proves that a database's backups restore. On a managed cloud, pg_sage asks the
provider for a clone ([managed clouds](managed-clouds.md#restore-drills)). A self-managed
server's backup tool leaves no evidence in the catalog. Without a drill its recoverability is
`unknown`. A Database Lab clone does not help here: it proves Database Lab's copy, not your
backup chain.

So on a self-managed server, pg_sage can run a restore command that you supply. The command
restores your own backups (pgBackRest, Barman or WAL-G) to a scratch server. The drill then
checks that copy like any other: catalog, row counts, a sample checksum, the marker, and the
measured RTO and RPO.

## How it works

```yaml
data_dir: /var/lib/pg_sage
clone:
  provider: none            # or dle; a managed provider is refused with a restore command
  allow_full_copy: true     # a restore is a full copy
drill:
  command:
    restore_argv: ["/opt/sage/drill-restore", "main"]
    destroy_argv: ["/opt/sage/drill-destroy"]
    env_allow: [PGBACKREST_REPO1_CIPHER_PASS]
```

The database's policy [drill block](policy-gate.md#restore-drills) must list `full_copy` in
`kinds`. Without it, or without `clone.allow_full_copy`, the database gets the info finding
`recoverability_unknown` naming the missing setting.

pg_sage runs the programs like this:

- **No shell.** Each list is the program and its arguments. The program must be an absolute
  path.
- **Working directory.** Both programs run in `data_dir/restore-drills`, which pg_sage creates
  with mode 0700.
- **Environment.** Each program gets `PATH` (and `SystemRoot` on Windows). It also gets the
  variables named in `env_allow` that are set. Nothing else from the sidecar's environment is
  passed. `SAGE_` names are refused in `env_allow`. pg_sage logs the program paths and the
  variable names at startup, never an argument or a value.
- **Restore.** The restore program gets `SAGE_DRILL_ID` (for example `sage-drill-0a1b2c3d`,
  unique per drill) and `SAGE_DRILL_DATABASE` (the pg_sage name of the database). It must
  print one line on stdout and exit 0 within the drill's `max_duration_minutes`:

  ```json
  {"dsn": "postgres://sage@localhost:55123/app", "id": "/var/lib/pg_sage/restore-drills/sage-drill-0a1b2c3d", "recovery_point": "2026-11-17T08:15:02Z"}
  ```

  `recovery_point` is the time the restore recovered to (RFC3339), and RPO is measured from
  it. Send logs to stderr. Any other stdout is a failed drill, including more than one line,
  more than 64 KiB, a missing field, a time in the future, or a `dsn` naming the monitored
  database.
- **Destroy.** The destroy program gets `SAGE_CLONE_ID` (the `id` printed). It runs when
  the drill ends, and again from the clone reaper if a crash left the restore behind. So it
  must succeed when there is nothing left to remove.

The drill connects to the `dsn` only after the identity guard shows it is not the source. pg_sage
keeps the DSN in memory only. The clone is a copy of production, so its roles and passwords are
production's: the monitoring role's `.pgpass` entry or password works.

### Failures

| What happens | Drill | Clone receipt |
|---|---|---|
| The restore exits non-zero | failed; the error quotes the last 16 KiB of stderr, redacted (connection URIs, credentials, tokens) | `failed`: your wrapper cleans up after itself |
| It runs past `max_duration_minutes` | failed; the program is killed | `create_uncertain`: a copy may be running; check `data_dir/restore-drills` |
| It exits 0 with unusable output | failed | `create_uncertain`, for the same reason |
| The destroy program fails | the result stands | `destroying`, retried by the clone reaper |

The keys are YAML only and apply at restart. The configuration API can never set them,
because they run programs on the sidecar host. pg_sage refuses them at startup when
`clone.provider` is a managed provider, or when `postgres.provider` or a database's
`provider` names a managed cloud. On a managed cloud, use the provider's clone kind.

## Example wrappers

These are examples to adapt. pg_sage does not ship them. Each restores into
`$PWD/$SAGE_DRILL_ID` and starts a scratch server on a local port derived from the drill id.
It starts that server with `archive_mode=off`, so the scratch copy never writes WAL into
your production repository. Run them as the OS user that owns your PostgreSQL binaries.

The destroy program is shared by all three. It refuses any id outside the drill directory:

```sh
#!/bin/sh
# /opt/sage/drill-destroy
set -eu
case "$SAGE_CLONE_ID" in
  "$PWD"/sage-drill-*) ;;
  *) echo "refusing to remove $SAGE_CLONE_ID" >&2; exit 1 ;;
esac
[ -d "$SAGE_CLONE_ID" ] || exit 0
pg_ctl -D "$SAGE_CLONE_ID" -m immediate stop >&2 || true
rm -rf -- "$SAGE_CLONE_ID"
```

The restore programs end with the same lines, which print the result:

```sh
# print_result PORT DATABASE
print_result() {
  rp=$(psql -At -h localhost -p "$1" -d postgres -c \
    "SELECT to_char(COALESCE(pg_last_xact_replay_timestamp(), now()) AT TIME ZONE 'UTC',
            'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")
  printf '{"dsn":"postgres://sage@localhost:%s/%s","id":"%s","recovery_point":"%s"}\n' \
    "$1" "$2" "$dir" "$rp"
}
dir="$PWD/$SAGE_DRILL_ID"
port=$((55000 + $(printf %s "$SAGE_DRILL_ID" | cksum | cut -d' ' -f1) % 1000))
start() {
  pg_ctl -D "$dir" -w -t 3600 -l "$dir.log" \
    -o "-p $port -c listen_addresses=localhost -c archive_mode=off" start >&2
}
```

`pg_last_xact_replay_timestamp()` is the last transaction the restore replayed. When nothing
was replayed it falls back to `now()`, which overstates the RPO. That errs on the safe side.

### pgBackRest

`--archive-mode=off` keeps the restored server from archiving into the stanza's repository.
The default recovery type replays every archived WAL segment and then promotes. To drill a
fixed target, add `--type=time --target=... --target-action=promote`.

```sh
#!/bin/sh
# /opt/sage/drill-restore STANZA
set -eu
# ... print_result, dir, port and start from above ...
pgbackrest --stanza="$1" --pg1-path="$dir" --archive-mode=off restore >&2
start
print_result "$port" app
```

Pass the repository's cipher passphrase or cloud credentials through `env_allow`, for example
`PGBACKREST_REPO1_CIPHER_PASS` or `PGBACKREST_REPO1_S3_KEY_SECRET`.

### Barman

`--get-wal` makes the restored server fetch WAL from Barman during recovery. Run the wrapper
on the Barman host, or use `--remote-ssh-command` to restore onto another host.

```sh
#!/bin/sh
# /opt/sage/drill-restore SERVER
set -eu
# ... print_result, dir, port and start from above ...
barman recover --get-wal "$1" latest "$dir" >&2
start
print_result "$port" app
```

### WAL-G

```sh
#!/bin/sh
# /opt/sage/drill-restore
set -eu
# ... print_result, dir, port and start from above ...
wal-g backup-fetch "$dir" LATEST >&2
cat >> "$dir/postgresql.auto.conf" <<EOF
restore_command = 'wal-g wal-fetch %f %p'
recovery_target_action = 'promote'
EOF
touch "$dir/recovery.signal"
start
print_result "$port" app
```

List the storage settings in `env_allow`, for example `WALG_S3_PREFIX`, `AWS_REGION`,
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.

In a fleet, one `drill.command` serves every database. Choose the stanza, server or prefix by
`$SAGE_DRILL_DATABASE` inside the wrapper.
