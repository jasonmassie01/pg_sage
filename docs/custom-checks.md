# Owner checks

An owner check is a house rule you write as one `SELECT`: "every table needs a primary key",
"no table without an owner comment", "no role may log in without a password expiry". pg_sage
runs it on a schedule, read-only, and turns each row it returns into a finding you can triage,
suppress and track like any other. Checks never change the database and never carry SQL to
run: they report, people decide.

## Defining a check

Checks live in the YAML config file under the top-level `checks:` list and are read when
pg_sage starts: restart to change them (the key is restart-bound; a fleet reconfigure keeps
the checks loaded at startup).

| Field | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | | Lower case letters, digits and `_`, starting with a letter, at most 63. The finding category is `custom:<id>`. Unique across the list |
| `title` | yes | | Finding title; `: <object>` is appended |
| `severity` | yes | | `info`, `warning` or `critical` |
| `sql` | yes | | One plain `SELECT` (`WITH`, `VALUES` and `TABLE` forms included) without parameters |
| `object_column` | yes | | Result column that names each finding's object; rows with an empty or repeated value are skipped |
| `message` | no | empty | Recommendation text on each finding |
| `databases` | no | `["*"]` | Database name globs (`report_*`) the check runs on. In a fleet the globs are matched against each instance's name |
| `interval_minutes` | no | `60` | Minutes between runs, at least `5` |
| `max_rows` | no | `100` | Rows read per run, `1`-`100` |
| `timeout_ms` | no | `1000` | The check's `statement_timeout`, `1`-`5000` |

An example is in [configuration](configuration.md#rule-packs-and-owner-checks).

## What is refused at load

The config load fails, naming the check, when a field is out of bounds or the id repeats, and
when the SQL is not a single plain read. The statement is parsed with PostgreSQL's own parser
(libpg_query) and refused when it:

- is not exactly one statement, or is not a `SELECT`;
- writes anywhere, including a data-modifying CTE (`WITH d AS (DELETE ...) SELECT ...`);
- takes row locks (`FOR UPDATE`, `FOR SHARE` and the key variants) or creates a table (`SELECT ... INTO`);
- references a parameter (`$1`);
- calls a function from a denied family: `pg_sleep*`, `dblink*`, `lo_*`, `pg_read_*`, `pg_ls_*`.

A build without the parser (`CGO_ENABLED=0`) refuses every check at load with "parse-tree
inspection unavailable": pg_sage never runs SQL it did not parse. Release builds include the
parser.

## How a check runs

1. **First run: functions.** The names the statement calls (functions and `x.name` attribute
   calls) are looked up in `pg_proc`. If any entry with such a name is volatile
   (`provolatile = 'v'`) the check is refused for good, except `now` and `clock_timestamp`.
   The reason is logged once. If `pg_proc` cannot be read, that run is skipped (logged once)
   and the lookup is retried at the next interval.
2. **The statement.** pg_sage runs the parser's canonical form of your SQL (comments and case
   normalised, the same statement it checked) on a connection from that database's own pool,
   inside `BEGIN READ ONLY` with `SET LOCAL statement_timeout = <timeout_ms>`,
   `lock_timeout = 100ms` and `application_name = pg_sage`. It reads at most `max_rows + 1`
   rows and always rolls back. A write that slipped through any other way fails in the
   read-only transaction. Like every pg_sage session, the connection runs with
   `search_path = pg_catalog, pg_temp` ([Security](security.md)): name your own tables and
   functions with their schema (`public.orders`), or the check fails with "relation does not
   exist". Catalog objects (`pg_class`, `pg_stat_activity`) need no schema.
3. **Findings.** Each row becomes a `custom:<id>` finding keyed by `object_column`, with every
   column in `detail.row` as text, `detail.fingerprint` and `detail.truncated`
   ([findings](findings.md#custom)). `recommended_sql` is always empty.

| Outcome | Open findings of the check |
|---|---|
| Complete run | Rows no longer returned resolve; new rows open findings |
| More than `max_rows` rows | The first `max_rows` are reported with `truncated = true`; nothing resolves |
| Error, timeout, lock timeout, missing `object_column` | Nothing resolves (logged at WARN) |
| Refused at the first run | Nothing resolves; the check does not run again until restart |
| Not due yet | Untouched |

Checks of different databases run in their own analyzer cycles, each on its own pool, so a
slow check on one database never holds another.

## Permissions and managed providers

Checks run as the pg_sage role. A catalog or table the role cannot read surfaces as a check
error in the log ("permission denied"), not as a finding; grant `SELECT` (or `pg_read_all_data`
where your provider allows it) on what the check reads. PostgreSQL 14-18 behave the same.

## Switching rules off instead

To silence a built-in rule rather than add one, use `analyzer.disabled_rules`, globally or per
database ([configuration](configuration.md#rule-packs-and-owner-checks)).
