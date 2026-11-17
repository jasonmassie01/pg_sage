# Fleet Mode

One pg_sage sidecar can watch many PostgreSQL databases. This page explains
how a fleet is put together: the control database, tenant boundaries, fleet
learning and its look-alike priors, leader election between sidecars, the
fleet LLM budget split, the health score, and the three fleet routes. For a
hands-on run, start with the [fleet walkthrough](walkthrough-fleet.md).

PostgreSQL 14 to 18 are supported, for monitored databases and for the
control database alike.

## Modes and the control database

| Mode | How you select it | Where the databases come from | Control database |
|---|---|---|---|
| standalone | the default; `--pg-url` or `postgres:` | the one monitored database | the monitored database |
| YAML fleet | `mode: fleet` with `databases:` | `databases:` in the YAML file | the first fleet database that started |
| meta-db fleet | `meta_db:` or `--meta-db` | rows in `sage.databases`, from the **Fleet** page or the API | the meta database |

With `meta_db` set, the mode is inferred; `mode: fleet` with `meta_db` also
runs a meta-db fleet, as in the walkthrough.

The control database holds what belongs to the fleet rather than to one
database: logins and sessions, MCP tokens, notification channels, standing
policy, the earned-trust ledger, fleet learning fingerprints and outcome
digests, and the leader lease. Each monitored database keeps its own
findings, actions and history (see
[Where pg_sage keeps its data](configuration.md#where-pg_sage-keeps-its-data)).

In meta-db mode, set `encryption_key` (or `SAGE_ENCRYPTION_KEY` /
`SAGE_ENCRYPTION_KEY_FILE`) before adding databases: database passwords are
stored encrypted in the meta database, and pg_sage refuses to store one
without a key. Keep the same key across restarts and across sidecars.

The meta database can be a managed service such as Amazon RDS or Cloud SQL.
It needs no extension: `pg_stat_statements` is only required on the
databases pg_sage monitors.

### Sharing the control database with a monitored database

Use a dedicated control database (the walkthrough creates `sage_meta`). A
meta database that is also monitored is supported, but it means:

- its `sage` schema holds the fleet's control data next to that database's
  own monitoring data;
- pg_sage writes to it continuously: a leader lease renewal every third of
  `fleet_learning.leader_lease_seconds` (every 10 s by default) and a
  fingerprint write every `fleet_learning.interval_minutes`, as well as
  logins, sessions and audit rows;
- its backups and restores carry the control data: a point-in-time restore
  of the application database also rolls back logins, the database registry
  and the trust ledger;
- when it is down, the whole fleet loses its dashboard logins and its
  database registry, not just that one database.

## Tenant boundaries (`tenant:` tags)

Fleet learning only compares databases inside one sharing boundary. A
database tagged `tenant=<name>` belongs to that tenant's boundary (the name
is case-insensitive); untagged databases share one boundary. Fingerprints,
look-alikes and priors never cross a boundary or a control database. Tags
are `databases[].tags` in YAML, or the tags of a database added on the
**Fleet** page.

A tag edit on a running database needs no restart: an edit saved on the
**Fleet** page applies at once, a change written to `sage.databases` by other
means applies at the next reconcile pass, and the next fingerprint cycle
regroups the database. At startup pg_sage logs the count of databases per
boundary, for example `fleet-learning boundaries: shared=1 tenant:acme=2`.

## Fleet learning

Fleet learning is on by default (`fleet_learning.enabled`). Every
`fleet_learning.interval_minutes` the leader sidecar fingerprints each
connected database and digests its verified action outcomes.

- **Fingerprints** hold hashes of shapes only: a table's column types in
  order, an index's method, uniqueness and key positions, and normalized
  query text with every literal and identifier replaced. No data, literals
  or names are stored unless you set `fleet_learning.include_names`.
- **Outcome digests** count each database's verified outcomes (improved,
  neutral, regressed) of the last 90 days by action class and table shape.

### Look-alikes

Two databases of the same boundary are look-alikes when their similarity
reaches `fleet_learning.lookalike_min_similarity` (default 0.6). Similarity
is the weighted overlap of table shapes (weight 0.5), query shapes (0.3)
and index shapes (0.2); a dimension empty on both sides, such as queries
without `pg_stat_statements`, is left out and its weight shared by the
others. A database is never its own look-alike.

### What look-alike priors may and may not do

When look-alikes hold at least `fleet_learning.min_prior_outcomes` verified
outcomes (default 3) for the same action class, first on the same table
shape and otherwise over every table, the proposal carries their evidence
as `lookalike_prior`, labelled "from look-alike databases".

A prior is evidence, never a grant. It may:

- be shown on the proposal, with the number of look-alike databases, the
  lowest similarity among them and their outcome counts;
- add caution: when look-alikes regressed, and at least as often as they
  improved, the proposal requires approval;
- order proposals that have no local calibration yet.

It may not raise confidence, lower the trust level a proposal needs, remove
an approval requirement, or let pg_sage act on a database it could not act
on alone. HIGH-risk actions always need approval, whatever the look-alikes
did.

### When fleet learning idles

Fleet learning idles until two connected databases share a boundary: a
standalone sidecar, a one-database fleet, or a fleet whose every database is
in a different tenant has nothing to compare. While it idles, the cycle
reads no catalog and no `pg_stat_statements` and writes nothing, and
pg_sage logs `fleet learning: skipped, no peer in boundary` once each time
it starts idling. The first cycle runs 2 minutes after startup, so the
fleet's databases can connect first. Within a minute of a second database
joining a boundary, the next cycle runs at once instead of waiting for
`fleet_learning.interval_minutes`. Fingerprints left from a fleet that
shrank stay readable until the next cycle that runs.

### Settings

Every `fleet_learning` key is an operator preference read at startup; none
widens what pg_sage may do or spend.

| Key | Default | Meaning |
|---|---|---|
| `fleet_learning.enabled` | `true` | Fingerprint the fleet, show look-alike evidence and fleet findings |
| `fleet_learning.include_names` | `false` | Also store table names with fingerprints, for drill-down |
| `fleet_learning.interval_minutes` | `60` | Minutes between cycles, 5-1440 |
| `fleet_learning.lookalike_min_similarity` | `0.6` | Similarity at which a peer is a look-alike, above 0 up to 1 |
| `fleet_learning.min_prior_outcomes` | `3` | Look-alike outcomes needed before a prior is shown, 1-1000 |
| `fleet_learning.fleet_finding_min_databases` | `3` | Databases a problem must be open on to be a fleet finding, 2-10000 |
| `fleet_learning.budget_split` | `need` | `need` or `even` (see [LLM budget split](#llm-budget-split)) |
| `fleet_learning.budget_floor_pct` | `50` | Smallest share under the need split, as % of the even share, 0-100 |
| `fleet_learning.budget_ceiling_pct` | `300` | Largest share under the need split, as % of the even share, 100-1000 |
| `fleet_learning.leader_lease_seconds` | `30` | Leader lease, 10-600; `0` turns election off |
| `fleet_learning.control_database` | empty | YAML fleet database that holds the leader lease; must name an entry under `databases:`; not used with `meta_db` (see [Leader election](#leader-election)) |
| `fleet_learning.orphan_scope_days` | `14` | Days after its lease expired that another scope's fingerprints, digests and lease row are deleted (a fleet whose database names changed, or that was retired), 1-3650; a scope without a lease row is kept |

## Leader election

Fleet-wide jobs run on one sidecar at a time: the fleet learning cycle,
audit chain verification, SIEM export, approval card follow-ups and the
agent governance reconcilers. The sidecars of a fleet elect that leader
through a lease row in the control database (`sage.fleet_leader_lease`),
one per fleet scope: `meta` for a meta-db fleet, `fleet:<hash of the
database names>` for a YAML fleet, `standalone:<database>` otherwise.
When `agents.control_database` puts agent governance state in another
database, the governance reconcilers elect their own leader there, under
the scope `agents:<fleet scope>`, so leadership and that state never sit
in different databases.

- The leader renews its lease every third of the lease (10 s by default).
  Each renewal is bounded to that third, so a stalled connection costs one
  renewal, not the TCP timeout.
- A leader that cannot renew stops acting as leader at its local deadline,
  four fifths of the lease (24 s by default), before the lease expires.
- Another sidecar takes over once the lease has expired, at its next
  renewal. The epoch then goes up by one, and a fleet learning write still
  in flight under the old epoch is refused (`fleet learning stopped: the
  leader lease moved to another sidecar mid-cycle`).
- A sidecar that shuts down releases its lease, so a follower takes over at
  its next renewal.
- Each approval card follow-up is claimed under the leader's lease epoch
  right before it is posted (`sage.approval_card_deliveries.followup_claim_epoch`).
  A claim only moves to the same or a newer epoch, or once it is 5 minutes
  old, and only while that lease is current, so a handover in the middle of
  a cycle does not post the same follow-up twice.
- A follower logs once per job that the job `runs on the leader sidecar of
  this fleet, not here (see /api/v1/fleet/leader)`.

Multi-sidecar HA needs a pinned control database. Without one, a YAML
fleet's control database is the first of its databases to connect, so a
sidecar that starts while that database is down takes its lease in another
database, where no other sidecar sees it, and two sidecars lead at once.
Pin it in one of two ways:

- point every sidecar at the same meta database (`--meta-db`) with the same
  `encryption_key`; or
- set `fleet_learning.control_database` to the same entry of `databases:` on
  every sidecar. The lease (and fleet learning state) then lives only in
  that database. While it is down every sidecar follows: none leads from
  another database, and fleet-wide jobs wait. A sidecar that started while
  it was down follows for its whole run (`/api/v1/fleet/leader` shows
  `leadership_refused` and why); restart it once the database is back.
  Logins still fall back to the first database that connects.

Give every sidecar network access to every monitored database. A YAML fleet
with election on and neither pin logs a WARN at startup (`multi-sidecar HA
needs a pinned control database`). If such a sidecar then sees another live
sidecar take or renew the lease, it refuses leadership for the rest of its
run and logs an ERROR naming the other holder; a lease left by a sidecar
that crashed is not live, and is taken over once it expires.
`/api/v1/fleet/leader` reports the control database the lease lives in
(`control_datname`, and `control_system_identifier` where the provider
allows `pg_control_system()`). `fleet_learning.leader_lease_seconds: 0`
turns election off, and every sidecar then runs every fleet-wide job: only
do that with a single sidecar.

The metrics `pg_sage_leader`, `pg_sage_leader_transitions_total` and
`pg_sage_leader_last_renew_age_seconds` show the election per scope; see
the [metrics reference](metrics.md#process-and-scrape).

## LLM budget split

With `llm.fleet_token_budget_daily` above 0, the fleet's daily token cap is
split between its databases, so one noisy database cannot drain it. Every 5
minutes pg_sage weighs each database by need:

```text
weight = 1 + min(open findings, 100) / 10
           + 2 * min(critical findings, 20)
           + 4 * min(unresolved incidents, 10)
```

Under `fleet_learning.budget_split: need`, each database gets a share in
proportion to its weight, but never less than
`fleet_learning.budget_floor_pct` or more than
`fleet_learning.budget_ceiling_pct` of the even share (cap divided by the
number of databases). Capacity freed by a ceiling goes to the others, and
the shares never add up to more than the cap. A database whose incidents
cannot be read is weighed by its findings alone. Under `even`, every
database gets the even share. `pg_sage_llm_fleet_budget_allocation_tokens`
and `pg_sage_llm_fleet_budget_used_tokens` show each database's share and
use.

## Health score

Each database tile shows a health score from 0 to 100 and the reasons
behind it (hover the score, or read the list under it). The same score and
reasons are in `status.health_score` and `status.health_reasons` of
`GET /api/v1/databases` and `GET /api/v1/metrics?database=<name>`.

| Condition | Effect on the score | Reason shown |
|---|---|---|
| Not connected | 0 | `disconnected: <error>` |
| Connected with an error | 0 | `error: <error>` |
| Each critical finding | minus 25 | `2 critical findings` |
| Each warning | minus 5 | `5 warnings` |
| No collector sample for more than 3 collector intervals | capped at 25 | `collector stale 42m` |
| Emergency stop active | none | `emergency stopped by alice` |

The interval is the one the database's collector runs at: its
`collector.interval_seconds` after any reload or self-config promotion, or
the circuit breaker's dormant interval while it is dormant. Before the
collector starts, the database's `collector_interval_seconds` is used, else
the global `collector.interval_seconds`. A database that has never been
sampled counts as stale only once it has been registered for more than 3
intervals, so a new database is not penalised while its first sample runs.
The score never goes below 0.

A database counts as **healthy** (`summary.healthy` and the
`pg_sage_fleet_healthy` gauge) when it is connected, has no error and
scores at least 50; every other database counts as degraded. Databases are
listed worst first, and by name among equal scores, so the tiles keep a
stable order; the `pg_sage_database_health_score` series are listed by
database name. The rule is the
same in every mode and on every provider.

### Overview summary

The Overview's top banner says "No open critical or warning cases" only
when every count below is 0 and no database is degraded. Otherwise it lists
the open work, critical first, with a link to **Cases**, or says that health
is not established and why. The counts are in `summary` of
`GET /api/v1/databases`, summed over every database in every mode:

| Field | Counts |
|---|---|
| `total_critical`, `total_warning` | open critical and warning findings |
| `incidents_critical`, `incidents_warning` | unresolved critical and warning incidents |
| `incidents_unknown` | databases whose incidents could not be read |
| `stale_collection` | databases with no collector sample for more than 3 intervals |
| `freshness_unknown` | databases not yet sampled in their first 3 intervals, or with no interval |

Each database row carries its own `status.incidents_critical`,
`status.incidents_warning` (`-1` while unknown: before the first read or
after a failed one) and `status.collection_freshness` (`fresh`, `stale` or
`unknown`). Incidents are read with the findings after each analyzer cycle,
under a 5 second timeout.

The **Top Cases** tab lists the five highest-ranked open cases of the
selected scope, each linking to its case, with the basis of its rank. It
names every database whose cases could not be read or were cut at the
read cap (the count then reads "at least"), and says "No open cases"
only when every database answered with none. See
[the cases response in a fleet](walkthrough-fleet.md#the-cases-response-in-a-fleet).

## HA role

Each tile also shows the database's HA role: `primary`, `replica`, or
`unknown` when the last check failed or is stale. It shows an `HA safe mode`
badge while role flapping holds autonomous actions; hover the badge to see the
last role change. `GET /api/v1/databases` reports the same values as
`status.ha_role`, `status.ha_safe_mode` and `status.ha_last_change` (the last
one is absent until a change is seen). Each database has one HA monitor, and
every part of pg_sage reads it. See
[HA role checks](configuration.md#ha-role-checks) for the checks and
`ha.check_interval_seconds`.

## Recurring across the fleet

The **Fleet** page lists the problems open on several databases at once,
from `GET /api/v1/fleet/findings` (the MCP `fleet_findings` tool reads the
same data). A problem is one category on one object, for example a
duplicate index on `public.orders`, open on at least
`fleet_learning.fleet_finding_min_databases` databases (3 by default).

pg_sage reads each database's open findings at most once a minute and
serves every viewer, page load and MCP call from that read, so the list can
be up to 60 seconds old. A database that could not be read is retried after
15 seconds and is listed under `errors` ("Not read" on the page) without
hiding the others. Up to 8 databases are read at a time, and a request
waits at most 15 seconds: a database still being read then is listed in
`errors` as `timed out`, and its read finishes in the background for the
next request.

Each database contributes at most its 500 newest open findings. A database
with more is named in `truncated` and on the page as "counts may be low
for", because a problem it shares may be among the findings not read.

## Staged rollouts (fleet canary)

A fix verified on one database can be rolled out to the others one database at a time
through the fleet canary (`POST /api/v1/sre/autonomy/rollouts`, started by an admin). Each
database is changed only where its own analyzer recommends the same statement, through its
own approval path, and judged by a number: its effect on the target queries' blocks per call,
net of the median change of the same queries on the databases not changed yet (the untreated
controls, matched by normalized statement text), or its own verification v2 effect when fewer
than two controls can be measured. The rollout halts and rolls back newest first when that
effect is above `sre.autonomy.canary.regression_limit_pct` (default 10), and waits
`sre.autonomy.canary.stage_wait_hours` (default 24) after each database's verdict before the
next. A database with no v2 number is judged by its verdict flag only, said so in its record.
The full behaviour and the stored records are in
[configuration.md](configuration.md#sage-sre-earned-autonomy).

```yaml
sre:
  autonomy:
    canary:
      canary_instances: 1
      regression_limit_pct: 10
      stage_wait_hours: 24   # 168 covers a full weekly cycle
```

## Finding a database

The Databases page has a filter above its table: it matches a name, a host
or a tag (`key`, `value` or `key=value`), ignoring case and treating every
character literally, and shows at most 100 rows at a time. From six
databases the header scope picker adds a **Find database** box over the same
names: typing narrows the list, and a unique exact name, or Enter on a single
match, selects it.

## API routes

All three routes need a session with the viewer, operator or admin role.
The samples below come from the walkthrough's two databases; your values
will differ.

### `GET /api/v1/fleet/findings`

`?min_databases=<n>` (2-10000) overrides
`fleet_learning.fleet_finding_min_databases` for one request; anything else
is a `400`. Findings are sorted by worst severity, then by how many
databases share them.

```bash
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/findings?min_databases=2'
```

```json
{
  "findings": [
    {
      "key": "duplicate_index|public.idx_orders_status_b",
      "category": "duplicate_index",
      "object_identifier": "public.idx_orders_status_b",
      "title": "Duplicate index public.idx_orders_status_b",
      "severity": "warning",
      "databases": 2,
      "last_seen": "2026-11-09T15:04:05Z",
      "occurrences": [
        {
          "database": "production",
          "id": 41,
          "category": "duplicate_index",
          "severity": "warning",
          "object_identifier": "public.idx_orders_status_b",
          "title": "Duplicate index public.idx_orders_status_b",
          "recommendation": "Drop the duplicate index",
          "last_seen": "2026-11-09T15:04:05Z"
        },
        {
          "database": "staging",
          "id": 17,
          "category": "duplicate_index",
          "severity": "warning",
          "object_identifier": "public.idx_orders_status_b",
          "title": "Duplicate index public.idx_orders_status_b",
          "last_seen": "2026-11-09T15:03:58Z"
        }
      ]
    }
  ],
  "min_databases": 2,
  "databases_scanned": 2,
  "errors": [],
  "truncated": []
}
```

A database that could not be read appears in `errors`, for example
`{"database": "staging", "error": "timed out"}`.

### `GET /api/v1/fleet/lookalikes`

`?database=<name>` is required (`400` without it, `404` for a database the
fleet does not have). The list is most similar first. It is empty before
the database's first fingerprint (so also while fleet learning has idled
since startup) and when `fleet_learning.enabled` is false.

```bash
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/lookalikes?database=production'
```

```json
{
  "database": "production",
  "lookalikes": [
    {"database": "staging", "similarity": 0.83}
  ]
}
```

### `GET /api/v1/fleet/leader`

The election status as this sidecar sees it. `epoch` and `deadline` belong
to this sidecar's own lease: a follower omits `epoch` and shows `deadline`
as `0001-01-01T00:00:00Z`. `last_error` is the last failed lease round
trip.

```bash
curl -b cookies.txt http://localhost:8080/api/v1/fleet/leader
```

```json
{
  "enabled": true,
  "scope": "meta",
  "self": "sage-host-4242-9f1c2a7b",
  "holder": "sage-host-4242-9f1c2a7b",
  "leader": true,
  "epoch": 3,
  "deadline": "2026-11-09T15:04:29Z",
  "lease_ttl": "30s",
  "transitions": 0,
  "last_renew": "2026-11-09T15:04:05Z"
}
```

With `fleet_learning.leader_lease_seconds: 0`, `enabled` is false and
`leader` is true: every sidecar runs the fleet-wide jobs.
