# Backfill pressure: pace a bulk write between batches

A backfill, a bulk `UPDATE` or a data migration can hurt a database without
any DDL: replicas fall behind, WAL piles up, autovacuum cannot keep up with
transaction ID age, and sessions queue behind the rows it locks. pg_sage
already reads all of that. `check_pressure` turns it into one answer a job
can ask for between batches: **proceed**, **slow** (with a smaller batch
and a pause) or **stop** (see [Backfill pressure in the policy
gate](policy-gate.md#backfill-pressure) for the inputs and their limits).
It only reads; the job decides.

This page shows three ways to call it, how to register a job, and the
optional, approval-gated enforcement for a job that keeps writing through a
stop.

## The CLI

`pg_sage_sidecar pressure` asks a running sidecar and exits with the
verdict, so any script can branch on it:

```sh
export SAGE_API_URL=https://sage.example.com:8080
export SAGE_API_TOKEN=...   # an API token with the read scope
pg_sage_sidecar pressure --database orders --table public.orders --job backfill_orders --batch 1000
```

| Exit | Meaning | stdout |
|------|---------|--------|
| 0 | proceed | `proceed batch=1000 factor=1 sleep=0` |
| 10 | slow: use the suggested batch, then pause | `slow batch=500 factor=0.5 sleep=4.5` |
| 20 | stop: pause, then ask again | `stop batch=0 factor=0 sleep=30` |
| 2 | usage error (a missing `--database`, no `SAGE_API_URL` or token) | nothing |
| 3 | no answer: the sidecar is unreachable, the token is refused, the table is unknown or the inputs could not be read | nothing |

Treat exit 3 like stop. The reasons for a slow or stop answer go to stderr;
`--json` prints the sidecar's whole answer (every input's value, limits and
freshness) instead of the one line. The token travels only in the
`Authorization` header, over https or to a loopback address, and is never
printed. `SAGE_API_TOKEN_FILE` may name a file holding it instead.

## A shell loop

```sh
batch=1000
while :; do
  code=0
  answer=$(pg_sage_sidecar pressure --database orders --table public.orders --job backfill_orders --batch "$batch") || code=$?
  pause=$(printf '%s\n' "$answer" | sed -n 's/.*sleep=\([0-9.]*\).*/\1/p')
  case $code in
    0|10)
      [ "$code" = 10 ] && batch=$(printf '%s\n' "$answer" | sed -n 's/.*batch=\([0-9]*\).*/\1/p')
      moved=$(PGAPPNAME=backfill_orders psql -X -At -v batch="$batch" -f batch.sql) || exit 1
      [ "$moved" = 0 ] && break
      [ "$code" = 10 ] && sleep "${pause:-5}"
      ;;
    *) sleep "${pause:-30}" ;;   # 20 (stop) and 3 (no answer)
  esac
done
```

where `batch.sql` moves one batch and prints how many rows it moved:

```sql
WITH batch AS (
  SELECT id FROM public.orders
   WHERE total_cents IS NULL
   ORDER BY id
   LIMIT :batch
   FOR UPDATE SKIP LOCKED
), moved AS (
  UPDATE public.orders o SET total_cents = o.total * 100
    FROM batch WHERE o.id = batch.id
  RETURNING 1
)
SELECT count(*) FROM moved;
```

## A psql loop

psql can run the same loop by itself: a backquoted command runs the CLI,
`\gset` reads its answer into variables, `\if` branches on them, and the
script includes itself again for the next batch. `\watch` cannot run
meta-commands between its repetitions, so the loop uses `\ir`; it stops
after `max` batches (each `\ir` nests one level), and you run it again to
continue.

```psql
-- backfill.psql: psql -X -v batch=1000 -v n=0 -v max=500 -f backfill.psql
SET application_name = 'backfill_orders';
\set answer `pg_sage_sidecar pressure --database orders --table public.orders --job backfill_orders --batch :batch`
SELECT split_part(:'answer', ' ', 1) IN ('proceed', 'slow') AS go,
       split_part(:'answer', ' ', 1) = 'slow' AS slow,
       coalesce(substring(:'answer' from 'batch=([0-9]+)')::int, :batch) AS suggested,
       coalesce(substring(:'answer' from 'sleep=([0-9.]+)')::numeric, 30) AS pause,
       :n + 1 AS n, :n + 1 < :max AS under \gset
\if :go
  \if :slow
    \set batch :suggested
  \endif
  WITH batch AS (
    SELECT id FROM public.orders WHERE total_cents IS NULL
     ORDER BY id LIMIT :batch FOR UPDATE SKIP LOCKED
  ), moved AS (
    UPDATE public.orders o SET total_cents = o.total * 100
      FROM batch WHERE o.id = batch.id RETURNING 1
  )
  SELECT count(*) > 0 AS more FROM moved \gset
  \if :slow
    SELECT pg_sleep(:pause);
  \endif
\else
  -- stop, or no answer: wait, then ask again
  SELECT pg_sleep(:pause);
  SELECT true AS more \gset
\endif
\if :more
  \if :under
    \ir backfill.psql
  \endif
\endif
```

While it runs, `\watch` in a second psql session shows the job's sessions
and whether they hold a transaction ID (that is, they are writing):

```sql
SELECT pid, state, backend_xid IS NOT NULL AS writing, now() - xact_start AS xact_age
  FROM pg_stat_activity
 WHERE application_name = 'backfill_orders' \watch 5
```

## In CI: a GitHub Actions step

The job registers itself, then calls the CLI between batches. The runner
needs the `pg_sage_sidecar` binary on its `PATH` (from the release
archive) and network access to the sidecar's API. The token needs the read
scope to ask and the propose scope to register; store it as a secret.

```yaml
name: backfill-orders
on: workflow_dispatch
jobs:
  backfill:
    runs-on: ubuntu-latest
    env:
      SAGE_API_URL: https://sage.example.com:8080
      SAGE_API_TOKEN: ${{ secrets.SAGE_API_TOKEN }}
      PGAPPNAME: backfill_orders
      PGDATABASE: orders
      DATABASE_URL: ${{ secrets.ORDERS_DATABASE_URL }}
    steps:
      - uses: actions/checkout@v4
      - name: Register the backfill with pg_sage
        run: |
          curl -fsS -X POST "$SAGE_API_URL/api/v1/databases/orders/pressure-jobs" \
            -H "Authorization: Bearer $SAGE_API_TOKEN" -H 'Content-Type: application/json' \
            -d '{"application_name":"backfill_orders","table":"public.orders","ttl_seconds":21600}'
      - name: Backfill between pressure checks
        run: |
          batch=1000
          while :; do
            code=0
            pg_sage_sidecar pressure --database orders --table public.orders --job backfill_orders --batch "$batch" > answer || code=$?
            pause=$(sed -n 's/.*sleep=\([0-9.]*\).*/\1/p' answer)
            if [ "$code" = 0 ] || [ "$code" = 10 ]; then
              [ "$code" = 10 ] && batch=$(sed -n 's/.*batch=\([0-9]*\).*/\1/p' answer)
              moved=$(psql "$DATABASE_URL" -X -At -v batch="$batch" -f batch.sql)
              [ "$moved" = 0 ] && break
              [ "$code" = 10 ] && sleep "${pause:-5}"
            else
              echo "pg_sage says stop (exit $code); waiting ${pause:-30}s"
              sleep "${pause:-30}"
            fi
          done
```

## Registering a job

Registration ties the job's sessions to the job through their
`application_name`. It is optional for asking `check_pressure`, and
required for enforcement.

```sh
curl -fsS -X POST "$SAGE_API_URL/api/v1/databases/orders/pressure-jobs" \
  -H "Authorization: Bearer $SAGE_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"application_name":"backfill_orders","table":"public.orders","enforce":true,"ttl_seconds":21600}'
```

- `application_name` (required): the name the job's sessions set (`PGAPPNAME`,
  `SET application_name`), 1 to 63 printable ASCII characters. A name
  containing `pg_sage` is refused: pg_sage's own sessions are never signalled.
- `table`: the table the job writes, schema-qualified; the enforcer's checks
  name it, so its vacuum and lock-queue inputs count.
- `enforce` (default false): allow the approval-gated cancel below.
- `ttl_seconds` (default 86400, at most 604800): the registration ends then.

Registering the same name again while it is active refreshes that job
instead of adding another. `GET .../pressure-jobs` lists the active jobs and
`POST .../pressure-jobs/{id}/end` ends one early, for example when the
backfill finishes. Registering and ending need an operator (a signed-in
operator, or an operator's API token with the propose scope); listing needs
a viewer or a read-scope token. See [the API reference](api.md#backfill-jobs).

## Enforcement: approval-gated, never automatic

For a job registered with `enforce`, pg_sage checks pressure on the job's
table every 10 seconds. When the answer is stop for **two consecutive
checks** and one of the job's sessions is still writing (an active session
under the job's `application_name` that holds a transaction ID), pg_sage
proposes `cancel_backend` for that session in the approval queue: one
proposal per session, with the stopping inputs on its card, expiring after
15 minutes. One stop never proposes; a slow or proceed in between starts
the count again.

Nothing is cancelled until a person approves. When someone does, pg_sage
checks again, and cancels only if the job is still registered with
`enforce` (registering it again without `enforce` withdraws its open
proposals), the pressure still reads stop and the same session (pid and start time) is
still writing under the job's name; otherwise it refuses and nothing is
signalled. The cancel goes through the executor like every action: the
standing policy gate authorizes it, the statement rechecks the session's
identity as it signals, roles and applications listed in
`sre.actions.protected_roles` and `protected_applications` are never
signalled, and the action is in the audit log. A cancel ends the job's
current statement and rolls back its open transaction; the job's next
`check_pressure` still says stop.

Once a proposal is run, refused, denied or expired, its finding is
resolved. Only the fleet leader on a writable primary proposes. Expired
jobs are deleted 90 days after they expired.
