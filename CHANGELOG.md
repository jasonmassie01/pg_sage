# Changelog

## Unreleased

### Added

- **Every action pg_sage takes now says what it expects, and is checked against it.** Before
  an index create or drop, a config or table-setting change, a VACUUM/ANALYZE, a query hint or
  a retention batch runs, pg_sage records its predicted effect: which queries it targets, which
  metric should move and by how much, and whether the estimate came from HypoPG, a model or a
  rule (or that there is none). Afterwards it records what it observed and a verdict:
  improved, neutral, regressed, insufficient evidence or unverifiable. The action detail in
  the Actions page shows predicted vs observed with the evidence, and the outcomes are served
  by `GET /api/v1/actions`, `/api/v1/actions/{id}` and the new
  `GET /api/v1/action-outcomes` ledger.
- **Index drops are verified over a business cycle and come back on the first miss.** A drop
  is watched for `verify.drop_window_hours` (default 168, one week) instead of 15 minutes.
  Its definition is kept, and the index is re-created as soon as a query on that table gets
  slower or an active pg_sage hint names it. A shorter window is a fast-elevation setting
  and is reported at startup like the others.

### Changed

- **"It did not get worse" is no longer "success".** Verification compares the call-weighted
  mean execution time of the queries an action targets, before and after, with a
  significance test over the sampling intervals, over windows that grow until there are
  enough calls. Noise no longer flips a verdict, and too little traffic is "insufficient
  evidence", not success. The old check of database-wide cache hit and write latency is
  gone.
- **Only verified improvements earn trust or credit.** Earned autonomy and the value ledger
  count an action only when its verdict is improved; neutral, insufficient evidence and
  unverifiable count for nothing either way, and a regression is rolled back and counts
  against trust. VACUUM and ANALYZE are verified by the dead tuples and modified rows they
  were meant to clear instead of being marked successful at once.

### Fixed

- **Index advice saved by older pg_sage versions no longer runs without a verified what-if.**
  Before v1.8.0 the optimizer filed its advice under the LLM's own label (`covering_index`,
  `partial_index`, `composite_index`, ...), and those findings skipped the HypoPG what-if
  check, so one built an index unattended on lifeos. Every automatic `CREATE INDEX` now needs
  a verified what-if or an operator approval; only the deterministic missing-foreign-key-index
  rule is exempt. The optimizer re-checks such old advice like current advice (HypoPG,
  duplicates) and retires the old copy, and it no longer proposes an index that an existing
  index already covers. Old findings whose rollback dropped a different index than the one
  they create are repaired at startup, and an approval of such a pair is refused rather than
  run. A withheld index build is recorded once with a closed verification (the ledger
  self-audit no longer flags it) and is not retried until its content changes.

## v1.8.4 (2026-10-03) -- Dogfood fixes: idle sidecar CPU, verified indexes build themselves, snapshot cap works

### What's new

- **The sidecar no longer burns CPU on databases with many indexes.** The duplicate-index
  rule compared every index with every other one in the database: about 90 s of CPU per
  10-minute analyzer cycle on lifeos (35,000 indexes). It now compares within each table:
  0.19 s, same findings.
- **Verified index advice is built without waiting for an approval it no longer needs.** An
  index queued for approval before HypoPG could verify it now runs once it is verified (the
  pending request is closed as superseded), and the first autonomous optimizer index no longer
  fails at its change lease. Indexes you rejected stay rejected.
- **The schema guard is fast however much history it has.** It re-read every decision it had
  recorded each scan (13 s per scan on lifeos); it now reads the newest one per issue (under
  40 ms).
- **Snapshot history shrinks after the upgrade.** The size cap now acts on the history
  partition the v1.8.3 conversion created, drops it whole when it closes, and the background
  conversion no longer gives up on databases with long-running queries. One slow cleanup no
  longer starves the others.


### Fixed

- **Large `sage.snapshots` tables now convert to daily partitions on busy databases.** The
  background conversion builds the new key with `CREATE INDEX CONCURRENTLY`, which waits for
  every query already running in the database, also queries that never touch pg_sage's
  tables. It ran under pg_sage's 2 s lock timeout, so on a database with queries of a few
  seconds (lifeos: 4-5 s application queries) every attempt was cancelled and the table stayed
  plain. That wait blocks none of the table's readers or writers, so it is now bounded by the
  conversion's 10 minute statement timeout instead.
- **The snapshot size cap now works right after the upgrade, and gives the disk space
  back.** Converting `sage.snapshots` to daily partitions put all existing rows (lifeos:
  9.3 GB) into one history partition that still takes rows for up to two days. The cap
  could not touch it until then and warned on every run. Now the cap waits for that
  partition to close and then drops it whole, which returns its disk space at once
  (deleting rows would only free space for reuse and write as much WAL). It says so once:
  when the partition closes and that it will be dropped then. A history partition that
  stays open longer than 48 hours (rows dated ahead at the conversion) has its oldest rows
  deleted in small paced batches instead (at most 1 GiB a run, never today's rows, never
  a snapshot that a kept one is built on). An empty or fully expired history partition of
  `sage.snapshots` or `sage.query_store` is dropped instead of truncated, and dropping a
  partition can no longer break a kept snapshot that reaches it through a checkpoint.
  Over-cap warnings are logged once per change or day and say what pg_sage is doing.
- **One slow cleanup no longer starves the others.** When one table's cleanup used the
  whole 30 s run budget, the next run started with it again, so on lifeos the query
  history backlog ran alone run after run and the other tables waited. A table that uses
  the budget now goes last in the next run, so every table is cleaned at least once every
  few runs.

- **The analyzer no longer pins a CPU core on databases with many indexes.** The duplicate
  and subset index rule compared every btree index with every other one in the database, and
  the unused-index rule re-read every index definition for each unused index that backs a
  foreign key. On lifeos (35,000 indexes) that cost about 90 seconds of sidecar CPU every
  10-minute analyzer cycle. Both rules now compare only indexes of the same table: 0.19 s on
  42,000 indexes, with the same findings.

- **An index waiting for approval now runs on its own once it no longer needs approval.** An
  index proposal queued because HypoPG had not yet verified it stayed pending even after
  pg_sage verified it later, until the request expired a day afterwards. pg_sage now
  checks the current verdict every cycle. Once the change may run unattended (it was
  verified, or you raised trust), the pending request is closed as `superseded` with the
  reason "approval no longer required" and the change runs exactly once. Requests you
  approved or rejected are never overridden, and a change you rejected stays behind
  approval. A related fix: autonomous index builds from the optimizer no longer fail at the
  change lease with "invalid identifier".

- **The schema guard no longer slows down as its decision history grows.** Each scan
  re-read every decision it had ever recorded about the tables it checks; on the lifeos
  dogfood database, which still holds 253,000 rows written by v1.8.1's ledger flood, that
  took 13 seconds a scan. It now reads only the newest decision of each schema issue and
  the few rows it counts, through two small indexes: under 40 ms on a 250,000-row history,
  whatever its size. The large index the old read used is removed at upgrade.

## v1.8.3 (2026-10-03) -- Ships high performing: pg_sage keeps its own footprint small

### What's new

- **pg_sage never needs a DBA to make it behave.** A new performance gate (now blocking in CI)
  runs pg_sage against a large, messy catalog and fails on any seq scan of a large sage table,
  slow statement, write amplification, catalog timeout, slow API list or non-HOT update. It
  went from 16 offenders to 0. On the lifeos dogfood database pg_sage's ledger writes fell from
  ~38,000 rows/hour to ~550, and the database's CPU from 85% to under 1%.
- **Its own data stays small.** Query history and snapshots are partitioned by day and dropped a
  day at a time; query history records only queries that moved; snapshots are capped at 5% of
  the database; every pg_sage table has paced, budgeted retention. Existing tables are converted
  in place without copying rows, larger ones in the background.
- **It shows its own cost instead of hiding it.** pg_sage no longer turns off
  `pg_stat_statements` for its sessions: its statements are tagged `/* pg_sage */`, its
  sessions are named `pg_sage`, it reports `pg_sage_self_*` metrics and a `sage_self_cost`
  finding, and it leaves its own activity out of everything it analyzes.
- **Reads that stay flat as your database grows.** Catalog collection, the SRE probes, runways,
  earned autonomy, the analyzer and the API (cursor paging, capped totals, live updates from
  change counters) read only what they need.
- **Index advice made before HypoPG was installed is re-checked**, so it can now be verified
  and built autonomously instead of waiting for approval forever.

### Changed (read before upgrading)

- **pg_sage keeps its own data small and cleans it up without a DBA.** Its query
  history and snapshot tables are now split into one partition per day, so old data is
  removed by dropping a whole day (instant, no vacuum, disk space returned at once) instead
  of deleting rows; existing tables are converted in place on first start (the old rows
  stay readable and go when they age out). The per-query history now records a query only
  when its numbers changed (plus once an hour), in one write per minute instead of one per
  query, and keeps 14 days (`retention.query_store_days`) instead of following the 90-day
  snapshot window. Snapshots are capped at 5% of the database (`retention.snapshots_max_pct`,
  never below 256 MB), oldest days first. Cleanup runs in small, paced batches with a 30 s
  limit per run, also covers the approval queue and the agent database tables, and expires
  cached explanations when they expire. pg_sage also stops rewriting rows that did not
  change (change feed cursors, incidents and their causal chains) and stops indexing a
  column it updates every cycle, so updates of findings no longer leave dead index entries.
  Upgrading converts `sage.query_store` and `sage.snapshots` without blocking pg_sage or
  your sessions: the old rows are checked while writes continue, and the tables are locked
  only for a catalog change (about 0.1 s for a 1 GB table). Small tables are converted at
  startup; larger ones in the background after it. If a conversion cannot finish (a long
  transaction holds the table, a timeout, a full disk), pg_sage keeps working on the
  unconverted table, still deletes its expired rows in small batches, logs one warning
  saying what to do, and tries again later (after 1 hour, then up to once a day).

- **pg_sage shows its own cost instead of hiding it, and its API reads stay small on big
  histories.** pg_sage no longer turns `pg_stat_statements` tracking off for its sessions:
  every statement it sends carries `/* pg_sage */` after its first keyword (where even
  PostgreSQL 18 keeps it) and its sessions are named
  `pg_sage`, so a DBA can see exactly what it costs. pg_sage leaves its own statements and
  sessions out of everything it analyzes (index and hint advice, schema guard, leftover
  schema detection, connection leaks), and reports its bill as `pg_sage_self_*`
  Prometheus metrics (database time, statements, blocks, sage-table rows read and written
  per collector cycle, sage schema size) plus a `sage_self_cost` finding above
  `analyzer.self_cost_budget_ms` (default 3000, `0` turns the finding off). With a
  dashboard open, live updates no longer re-count the findings, actions and health tables
  every 2 seconds; they read the tables' change counters. The findings and actions lists
  page with a `cursor` (`next_cursor` in each response) over new indexes, their `total`
  stops counting at 1,000 (`total_capped` says so), and `offset` is limited to 1,000.
  Snapshot history refuses per-object categories (`tables`, `indexes`, `queries`,
  `sequences`, `foreign_keys`, `locks`, `partitions`, `config_data`: read them with
  `/snapshots/latest`) and stops at 4 MB (`truncated`).

### Fixed

- **An index recommendation made before HypoPG was installed is now re-checked.** The
  optimizer re-emits a table's open index recommendation instead of asking the LLM again,
  but it kept the stored "unverified" verdict forever, so installing HypoPG later never
  let pg_sage verify (and, with autonomy, build) the index: it waited for approval
  indefinitely. An open unverified recommendation is now re-measured with HypoPG when it
  is re-emitted: a measured gain makes it verified, no gain resolves it, and without
  HypoPG nothing changes.

- **pg_sage's own catalog reads no longer grow with the size of your database.** On a
  database with 15,000 tables, 35,000 indexes and 12,000 sequences, each collector cycle used
  to rebuild the table statistics view once per 1,000-row page, re-render every index
  definition, stat() every table and index file, and hold one lock per sequence in a single
  transaction (12,000 locks every minute: on a server with default lock settings that could
  make other sessions fail with "out of shared memory"). Now table and index pages read
  counters directly (about 5x faster per page in our tests), sizes come from the catalog's page
  counts with exact sizes for the 100 largest tables and indexes, index definitions are read
  again only when an index or its table changes, and sequences are read 1,000 at a time in
  separate transactions (never more than a quarter of the lock table). The database size is
  measured every 15 minutes instead of every minute and on every `/metrics` scrape, and a slow
  size measurement no longer loses the whole snapshot. The tuner's stale-statistics check and
  the TOAST lint rule stopped opening every table, so pg_sage's connections stay small. Every
  read-only check pg_sage runs on your database (analyzer checks, the optimizer's table
  context, the DDL risk assessment and all schema lint rules) now runs read-only under
  `safety.query_timeout_ms`: a slow check is cut off and reported as not evaluated instead of
  hanging the cycle.

- **pg_sage no longer reads whole history tables to clean up or to find recent rows, and
  a new performance gate keeps it that way.** A test now builds a large synthetic database
  (5,000 tables, 15,000 indexes, 5,000 sequences, 150,000 rows in each of pg_sage's history
  tables), runs pg_sage against it and fails if pg_sage scans a large `sage.*` table end to
  end, runs a slow statement, writes rows per object instead of per change, or runs a catalog
  query over 500 ms. Its first run found the work fixed here: the retention purges of
  explain, alert, verification and resolved-finding history, the change-feed age-out, the
  clean-up that runs when old actions and decisions are purged, and the check for due
  verifications each read their whole table; they now use indexes (added automatically at
  startup). Runway sampling and several Sage SRE windows bounded time in a way PostgreSQL
  cannot use with an index; they now can. The dashboard's live-update check no longer runs
  when no dashboard is open. The remaining findings (the live-update check while a dashboard
  is open, the actions list, forecast history reads, the earned-autonomy reconcile and a
  sequence catalog query) are listed in `reviews/2026-10-03-perf-gate-report.md` for the
  next fix pass.

- **Sage SRE, runway, earned autonomy and the analyzer read only what they need.** The
  earned-autonomy reconcile, the autovacuum-cancellation probe, the verification watch
  lookup and the investigations list each read their whole table on every pass; they now use
  small indexes (added automatically at startup, one migration that checks the catalog
  first). The startup migrations no longer scan `sage.incidents` or the autonomy events
  when there is nothing to change. The analyzer's query-history check decoded every
  snapshot of the lookback window on every cycle (224 ms on the performance gate); it now
  decodes the first snapshot of each of at most 100 time buckets once and remembers it.
  The forecaster decodes two snapshots per day once instead of every snapshot each cycle.
  The plan-regression rule reads the newest two plans per query instead of every plan of
  the week, and no longer stops on plans captured without an execution time. The
  sequence-runway probe reads at most 2,000 sequences per statement and covers larger
  catalogs in slices; the wraparound probe ranks tables before reading their statistics;
  runway trends and the runway restart read one series at a time through the index. The
  schema-health scan of `pg_attribute` runs only after a DDL change (checked every 5
  minutes) or once an hour. SLO windows are computed from running totals stored with each
  sample (a few index probes per series) instead of re-reading every sample every minute;
  samples stored before the upgrade are still read the old way until they age out.

- **pg_sage no longer counts its own sessions and statements as your workload.** Now that
  pg_sage is visible in `pg_stat_statements`, every analysis that reads sessions or
  statements leaves pg_sage's own out: the snapshot's active and idle-in-transaction
  counts, locks, connection states and churn (now per database, also in fleet mode), the
  load circuit breaker, lock chains and the Sage SRE lock graph, long-transaction, wait and
  temp-spill evidence, the runaway detector's blocker counts, the DDL risk score, the
  tuner's and the briefing's active sessions, auto_explain plans from the logs, and the
  write-latency check that decides whether an action caused a regression (pg_sage's own
  writes could trigger a rollback). Connection slots still count pg_sage, and pg_sage
  still shows up when it holds a lock or the xmin horizon. Every withheld index build is
  now counted: a failed record used to be dropped silently. `/value` reads only credited
  actions through a new index, the actions list counts through the time index, the
  app-managed-index check and RCA's rollback history read pg_sage's drops and rollbacks
  through small indexes, retention checks verifications and credited actions by index
  instead of reading those tables,
  and SRE investigation updates are heap-only again (no index on `updated_at`). The
  Findings and Actions pages show a capped total as "1000+" and load further pages.

## v1.8.2 (2026-10-03) -- Safety first: reversible config, safe EXPLAIN, per-database trust, promote from the UI

### What's new

- **Config changes pg_sage makes are reversible and checked.** The prior value and real
  rollback are captured, the effective value is read back, and success requires the targeted
  metric to move. Settings and table options outside an allowlist become advice only.
- **Safer SQL and LLM handling.** `/explain` only runs `ANALYZE` when the query provably has no
  side effects; the prompt-injection guard and redaction were hardened; destructive DDL must be
  schema-qualified; MCP stdio no longer gets corrupted by the briefing.
- **Index advice you can trust.** HypoPG checks no longer fail open; gains are weighted by query
  time; expression and partial indexes work (every partial index was rejected before); an
  unverified optimizer index needs approval; live tenant schema families are never called
  leftovers.
- **Earned autonomy per database, and you can earn it from the UI.** Accept or reject
  investigations, "Evaluate now", and a "Path to next level" panel. Only verified outcomes and a
  person's reviews count.
- **pg_sage stops flooding its own ledger.** The schema guard and policy gate write a decision
  only when something changes (lifeos: ~38,000 rows/hour before), with the indexes and retention
  that ledger needs.

### Added

- **Safety fixes for EXPLAIN, LLM prompts, index drops and MCP (Phase 0).** `/explain` only
  runs EXPLAIN ANALYZE when the query provably calls nothing with side effects (no volatile
  functions such as `pg_terminate_backend` or `dblink`, also not inside views, no row locks or
  data-modifying CTEs); otherwise it returns the plan without ANALYZE and says why. A zero or
  negative `explain.timeout_ms` now means the default instead of no limit, plan-only and
  ANALYZE results are cached separately, and a fallback after an LLM failure is cached for one
  minute only. Text sent to the LLM is delimited and redacted more reliably (Unicode tag
  tricks, `E''` strings, dollar quotes, plan JSON), the plan-regression narrator and action
  justifier now use that protection, and the narrator no longer replaces a finding's
  recommendation. `DROP INDEX` and `ALTER TABLE` run by pg_sage must name their schema, so a
  search_path cannot steer them into `sage` or `pg_catalog`. With the MCP stdio transport the
  daily briefing's stdout channel is written to stderr, and MCP now answers `ping`, ignores
  notifications and returns tool results as `content` blocks.

### Changed (read before upgrading)

- **pg_sage's snapshot history takes about a tenth of the space, and pg_sage warns when it
  grows too big.** The collector used to store the full list of every table, index,
  sequence and query each minute, so `sage.snapshots` reached 9.3 GB on a personal
  database. It now stores a full copy at most every 6 hours and, in between, only what
  changed. On an hour of collection with 5,000 indexes this writes 11x fewer bytes overall
  (indexes alone 21x to 26x fewer). Every screen, forecast and API reads exactly the same
  data as before. Existing history is not rewritten: old rows stay readable and age out
  with `retention.snapshots_days`. To read snapshots in SQL yourself, use
  `sage.snapshot_data(data, base_id)` instead of the `data` column. A new
  `sage_footprint` finding warns when pg_sage's own tables pass
  `retention.sage_size_warning_pct` percent of the database (default 10, `0` turns it off).

- **Config changes are reversible and checked, not assumed.** When pg_sage applies an
  LLM-proposed setting or table storage parameter, it first records the old value and the
  exact SQL to put it back, then reads the setting back after the reload. A change that did
  not take effect is undone and marked failed; one that needs a restart is marked as such.
  It is only counted as a success when the thing it was meant to fix measurably improved
  (fewer temp-file spills, fewer dead rows, more HOT updates); otherwise it is
  "unverifiable". Only allowlisted settings run; anything else, including turning
  autovacuum off, stays advice. Unused-index drops now use `last_idx_scan` (PG16+) and are
  never automatic on a database with replicas, whose index use pg_sage cannot see.

- **Earned autonomy is per database, and you can earn it from the UI.** In a fleet, one
  database's reviews and outcomes no longer promote or demote another (bench reports stay
  shared); levels set before this release apply to each database until it decides
  otherwise. A success that was never verified now earns nothing. Cases gains Accept /
  Reject (with a note and the actual root cause) on every finished investigation, and the
  Earned autonomy page gains "Evaluate now" and a "Path to next level" checklist that says
  what is still missing, with counts and ETAs. MCP adds `sre_review_investigation` and
  `sre_evaluate_autonomy`; a review an agent records through MCP is kept but never counts
  toward promotion, and approving a promotion stays a human step.

### Fixed

- **Internal cleanup of the sidecar's largest files, with no change in behavior.** The
  sidecar's entry point, the core of the action executor and the API router were split
  into smaller files, one per job, so each file and function stays within the project's
  size limits. New tests pin what each mode starts, every API route and who may call it,
  and they pass unchanged before and after the split. One small visible difference: if
  writing a Prometheus `/metrics` response fails (for example, the scraper hung up), the
  sidecar now logs a warning instead of ignoring the error.

- **A statistics reset can no longer make a used index look unused.** Unused-index
  findings can lead to an automatic `DROP INDEX`. If the statistics were reset (by
  `pg_stat_reset()`, a single-table reset or a restart) between two snapshots, scans that
  happened before the reset were invisible. pg_sage now records when the statistics last
  reset with every snapshot, and restarts an index's unused clock at that reset. It also
  restarts the clock when a counter goes down or when the index is dropped and recreated
  under the same name. An index is reported only after a full clean window. Before any
  drop, automatic or approved by an operator, pg_sage checks the evidence again live. If
  it no longer holds, pg_sage refuses and says which check failed: the index was scanned,
  the statistics were reset inside the window, or the index is gone.

- **Index, schema-family and hint advice you can trust more (Phase 0 tuning correctness).**
  New indexes run on their own only when HypoPG measured the whole workload and showed a
  gain, weighted by how much time each query really takes; anything it could not measure
  waits for your approval. Expression and partial indexes are understood, partitioned tables
  get a step-by-step plan instead of a statement PostgreSQL would reject, and the database
  collation is read correctly on PostgreSQL 16+. Live schema-per-tenant designs are no longer
  mistaken for leftover copies: each problem is shown once with the list of affected schemas,
  and pg_sage only suggests dropping schemas that are truly idle. Query hints are suggested
  only when the plan and catalog support them (no more hints for every scan or empty join
  hints), and the hints page shows whether a hint was proposed, applied or rolled back. The
  analyzer and optimizer read a bounded slice of their own history instead of all of it.

- **The decision ledger no longer grows by tens of thousands of rows an hour.** On a
  database with 15,000 tables and 160 leftover test schemas, `sage.decision` grew by about
  41,000 rows an hour (about a million a day, kept for a year). The policy gate wrote a new
  row every time it re-checked a candidate it was holding back. The schema guard wrote
  one row for every schema issue on every cycle and ran one unindexed lookup per issue.
  Now:
  - A parked, queued, blocked or observe-only verdict that repeats updates its existing row
    (`repeat_count`, `last_seen_at`) instead of adding a new one. Each executed decision
    still keeps its own row.
  - Cheap checks that skip a candidate now run before the policy gate.
  - The schema guard records a decision only when it changes (its route, outcome, reason or
    SQL, or the issue coming back after it was gone). It reads its history and table
    contracts in one query each per cycle and reads `pg_stat_statements` once per cycle.
    Unchanged issues are still checked and handled every cycle exactly as before.
  - Schemas that are copies of one another (same tables, generated names, five or more
    copies) are handled as one family. A family in use, such as one schema per tenant, is
    still fixed in every schema but recorded once per issue with the list of schemas. A
    family nothing has used within `analyzer.unused_index_window_days` (no scans, writes,
    statements or sessions) is noted once as an idle leftover and not fixed.
  - After pg_sage's own DDL, the guard re-checks at most once per
    `analyzer.schema_guard_ddl_debounce_seconds` (default 60).
  - Non-executed decisions are removed after the new `retention.decisions_days` (default
    30, counted from when they were last seen), unless an action or a verification refers
    to them.
  - At startup, pg_sage adds the ledger's missing indexes once, keeping any that already
    exist: `idx_decision_schema_guard_targets`, `created_at`, and every foreign key into
    or out of `sage.decision`. On a very large existing ledger, create them
    `CONCURRENTLY` by hand first (see the review report for the statements).

## v1.8.1 (2026-10-02) -- Fast trust, big-catalog fixes from dogfooding, current OpenAI models

### What's new

- **Works with current OpenAI models.** gpt-5/gpt-6 models (including the low-cost
  gpt-6-luna) now work for every LLM feature; pg_sage adapts the request shape automatically.
- **Trust in hours, not weeks, when you ask for it.** Every trust timer and promotion
  threshold is a setting with the spec value as default, for dogfood and test databases.
  Irreversible actions, L4 and admin approval stay hard limits.
- **Safe on big, messy databases.** Found by running pg_sage on a real 18 GB database:
  catalog collection, forecasting and sequence runways stay bounded on tens of thousands of
  objects; stale and duplicate incidents clean themselves up; indexes the application keeps
  recreating are left alone; long-stale rollback monitors expire instead of acting.
- **Sage SRE follow-ups.** Detector episodes become incidents, PgBouncer pool exhaustion is
  diagnosed, failovers between or before samples are caught (`causal-v4`), and fleet
  databases can be added, removed or changed without a restart.
- **One locked path for every change.** Retention deletes run through the executor with
  leases and verification; operator actions lock the exact object; `serialize_mode: queue`
  really queues.

### Added

- **Fast trust elevation for dogfood databases.** Every timer and threshold that gates
  trust is now configurable, with the spec value as the default. These are the trust ramp
  (`trust.ramp_safe_hours` / `ramp_moderate_hours`), an hour-scale IO baseline
  (`verify.io_baseline_hours`) and the earned-autonomy promotion bar
  (`sre.autonomy.promotion.*`). A documented profile lets a database earn autonomy in hours
  instead of weeks. Irreversible actions keep the full ramp and never go above L1, L4 is
  never reached, and an admin still approves every promotion. The sidecar logs a WARN for
  each lowered value, and the Earned autonomy page shows a "Fast elevation" badge.

- **Checkpoint storms, temp-file explosions and LWLock contention are now incidents.** When
  the Sage SRE detector sees one of them, it opens a warning incident (or uses the open
  incident it belongs to) with the measurement and its threshold as evidence. The incident
  shows in the Cases panel with its investigation, sends the usual incident notifications,
  resolves itself once the episodes stop, and its investigation can be reviewed for earned
  autonomy. The detector's thresholds are now settings (`sre.detectors.*`, today's values
  are the defaults). Stored bench and game-day reports now age out after
  `sre.autonomy.report_retention_days` (90 days), except the reports that current autonomy
  levels or pending promotions rest on and the newest report of each family.

- **Databases can be added, removed and changed without restarting pg_sage.** In fleet
  mode, saving the config file with a new, removed or changed database applies it at once:
  trust level, execution mode, the executor switch and tags change on the running database;
  connection, credential and other changes restart only that database's monitoring. A
  removed database first lets running actions finish (up to a minute), then stops cleanly
  and drops out of the metrics. A bad edit is rejected and everything keeps running as
  before. In meta-db mode, changes made to the database list by another pg_sage or by SQL
  are picked up within 30 seconds. Removing or moving the first (control) database still
  needs a restart.

- **Sage SRE sees pool exhaustion at PgBouncer, and failovers between or before its
  samples.** List your PgBouncer admin consoles under `sre.poolers` (the DSN from an
  environment variable or a mounted file, never logged) and connection investigations read
  `SHOW POOLS` and `SHOW STATS` at both samples, read-only and time-bounded. Clients queueing
  at the pooler while PostgreSQL has headroom are now diagnosed as pool exhaustion at the
  pooler instead of "no connection pressure"; without a pooler the diagnosis says pooler
  telemetry is unavailable. Connection and WAL investigations now refuse to compare two
  samples taken across a restart, a failover, a different server or a major-version upgrade
  and say so as missing evidence. After pg_sage restarts it remembers the database's role,
  timeline and cluster identity, so a failover that happened while it was down still pauses
  earned autonomy for the failover cooldown. The causal graph is now `causal-v4`. In a
  fleet, the runway monitor measures each cluster's total database size once per pass
  instead of once per database.

- **pg_sage keeps working on databases with huge catalogs and cleans up after itself (dogfood
  on a real database with 12,000 sequences and 35,000 indexes).** Sequence runways are measured
  again (the probe now takes about 200 ms instead of timing out), the collector and forecaster
  no longer overload the database they watch, and one slow catalog query no longer drops the
  whole snapshot. Incidents that have not been seen for 24 hours, or whose idle session is
  gone, now resolve on their own, and old duplicate incidents are merged. Indexes that the
  application keeps recreating are left alone and reported instead of dropped again, copies of
  one schema are reported once, a monitor resumed months later no longer rolls anything back,
  and expected policy refusals are no longer logged as errors.

### Changed (read before upgrading)

- **Retention deletes, operator actions and queued changes now share one locked path.**
  A retention delete (an owner-declared `retention_column` contract, D5) now runs as a
  recorded action through the same pipeline as every other change: it is authorized,
  takes a lease on its table, is re-authorized after waiting (so an emergency stop pressed
  meanwhile stops it), and is verified. A batch never deletes more rows than its reviewed
  dry run described (twice its count plus 100, in total across batches); after that a new
  dry run must pass review. Every deleted row is checked against the declared column and
  the table before the batch commits. Operator actions ("Take action", approved queue
  items) now lease the exact table or index they change, found by its database identity,
  so a rename or a different spelling cannot slip past. An index lease also covers its
  table, and custodian `VACUUM` (freeze) runs take a lease too. While another action holds
  the object, an operator gets HTTP 409 naming the holder. The policy's
  `serialize_mode: queue` now really queues: a waiting change keeps its place in line per
  object (first come, first served), the line survives a sidecar restart, at most 8 wait
  per object and 64 per database, and a wait ends after 2 minutes for pg_sage's own
  actions (it parks and retries next cycle) or 30 seconds for an operator (refused).
  `park`, the default, is unchanged.

### Fixed

- **pg_sage works with current OpenAI models (gpt-5, gpt-6 and later).** These models
  refuse `max_tokens`, and refuse tool calls unless `reasoning_effort` is `none`, so every
  LLM feature used to fall back to its deterministic path. pg_sage now notices the refusal,
  re-sends the request once in the shape the model wants, and remembers it for that model.
  Other providers, Gemini included, see no change. Two new settings pin the shape if you
  need to: `llm.token_parameter` and `llm.tool_reasoning_effort` (both default `auto`).

## v1.8.0 (2026-10-02) -- Sage SRE: eleven incident families, approved actions, earned autonomy

### What's new

- **pg_sage is an AI DBA: LLM features are on by default.** As soon as an LLM endpoint and
  key are configured, the optimizer, advisor, tuner hints, incident narration and the Sage
  SRE model turn use it. Without one, everything runs deterministically and pg_sage logs
  one line saying how to configure it. Spend is capped by `llm.token_budget_daily`.
- **Sage SRE investigates eleven incident families and starts by itself.** Lock blocking,
  connection pressure, WAL retention, plan regression, checkpoint storms, temp-file
  explosions, replication lag, LWLock contention, and three runways: wraparound, disk/WAL
  and sequence exhaustion. Runway investigations open *before* the incident. Every
  investigation answers "what changed?" with cited evidence, and your LLM reviews the
  causal graph's result: it may rank, ask for one more probe and write cited claims, but
  never overrides a conclusive root cause.
- **Approved actions.** When one backend is blocking everyone, pg_sage proposes cancelling
  exactly that backend. A human approves it in the UI, Slack or Telegram; pg_sage re-checks
  the identity, cancels, then verifies the incident cleared.
- **SLOs and burn-rate alerts.** App SLOs from Prometheus or signed pushes, plus four
  labeled database proxies; page-level burns open an investigation.
- **Earned autonomy.** Each incident family and action has a level from L0 to L4.
  Promotion needs benchmark evidence, a track record and your approval. Autonomy drops
  automatically while an error budget burns or during failover, and existing custodian
  autonomy is carried over unchanged.
- **Signed runbooks and incident memory.** The LLM drafts a runbook from an English
  playbook; it runs only after an admin signs it. Similar past incidents inform the model.
- **PGIncidentBench.** An open benchmark that scores the investigator against baselines on
  real fault programs and a 60-case replay corpus, with decoys, background load and
  pre-registered gates, in CI on PostgreSQL 14–18.

### Added

- **Operators can start, stop and resume Sage SRE investigations.** `POST
  /api/v1/databases/{db}/investigations` starts one for a case (a repeat joins the running
  one), and `.../{id}/stop` and `.../{id}/resume` pause and resume it without losing its
  steps or evidence. Stop and resume need the version you saw, so two people cannot undo each
  other by accident. The Cases panel has Stop and Resume buttons for operators. Viewers can
  only read. Every change is recorded with the user.

- **The Cases panel shows what the model said, and the timeline.** An investigation now
  shows the model ranking (an order, labeled, kept apart from the graph's scores), the
  model's claims with links to the exact evidence they cite, the probe the model asked for,
  and a timeline of every step, including when a model reply was not used and why, and when
  the model disagreed with the graph (the graph's root cause stands).

- **PGIncidentBench replays 60 recorded incidents.** Besides the live fault programs, the
  bench now replays a corpus of redacted incident recordings through the real investigator:
  30 lock, connection and WAL incidents, 15 harmless lookalikes, and 15 cases with missing
  or hostile data (no privilege, timeouts, a restart, stale or contradictory samples,
  instructions planted in table, slot and application names, passwords in error messages).
  It checks that no call outside the probe catalog is made, nothing in the database changes,
  no planted password reaches an export or a model prompt, and every model claim cites real
  evidence. The replay runs with every test run (about 25 seconds). Its case format is
  documented in `sidecar/sre-bench/README.md` so new incident families can add cases. With
  the LLM off, the investigator names the right cause in all 30 positive cases (29 before
  the connection-leak fix below, which that one missed case motivated) and abstains on all
  23 cases whose evidence is insufficient.

- **New page: Sage SRE permissions and data flow** (`docs/sage-sre-permissions-and-data-flow.md`):
  what investigations read, which role each provider needs (`pg_monitor`), what leaves the
  database, exactly what is sent to the LLM (redacted, fenced summaries, never raw rows or
  query text), retention, and how to turn each part off.
- **SLOs, burn-rate alerts and a change feed for Sage SRE.** pg_sage now tracks error
  budgets. Out of the box it measures four database proxies for every database: query
  latency (compared with the database's own last week), server-side errors in the log per
  transaction (needs log access), connection slots running out, and replication lag over 60
  s. They are always labeled "proxy" and never claim customer impact. To track what your
  customers see, register an app SLO under `sre.slo.objectives`: either two PromQL queries
  (bad and eligible events, read through a Prometheus-compatible API with a bearer token) or
  counters your service pushes to `POST /api/v1/sre/sli/{name}`, signed with
  `sre.slo.push.hmac_secret`. Burn rates use the Google SRE workbook rules: page at 14.4x
  over 1 h and 5 min or 6x over 6 h and 30 min, ticket at 1x over 3 days and 6 h; you can
  change them. Too little traffic, no data, stale or partial data is shown as "unknown"
  with the reason, never as "ok". When an app SLO burns at page level, pg_sage opens a
  read-only investigation that checks locks, connections and plan regressions and says when
  the cause is probably outside PostgreSQL (a proxy burn does this only with
  `sre.automatic_start`). Every investigation now also answers "what changed?": deploys,
  migrations and feature flags you send to `POST /api/v1/sre/change-events` (signed with
  `sre.change_events.hmac_secret`, timestamp-checked, replays ignored), plus pg_sage's own
  actions, config changes, DDL seen by the migration detector, `pg_stat_statements` resets,
  restarts, failovers and extension upgrades. After a fix, the SLO recovery check only
  confirms recovery from enough fresh traffic: missing data, counter resets or traffic that
  simply stopped never count. See it on the new SLOs page, `GET /api/v1/sre/slos`,
  `GET /api/v1/sre/changes`, the `sre_list_slos`, `sre_get_slo` and `sre_list_changes` MCP
  tools, and `pg_sage_slo_*` metrics. Turn parts off with `sre.slo.enabled`,
  `sre.slo.proxies.enabled` or `sre.change_events.feed_enabled`.
- **Sage SRE proposes one approved action: cancelling the backend that blocks everyone.**
  When an investigation concludes that one active statement is the root of a lock or
  connection-pressure incident, it proposes `pg_cancel_backend` for that exact backend (pid,
  backend start, query start, database, user and query hash), derived only from the
  investigation's evidence. Termination and idle-in-transaction holders are never proposed;
  the investigation says why. Proposals are on by default (`sre.actions.proposals`) and never
  execute by themselves. Each gets one item in the existing approval queue, and only a human
  approval runs it through the policy gate. pg_sage then rechecks the backend's identity
  (evidence at most 5 s old) and refuses, with the reason on the timeline, if anything
  changed. After the cancel it verifies recovery over fresh samples and records the result on
  the investigation. Approve or deny on the Actions page, in the Cases panel, or with Slack
  and Telegram buttons. Callbacks are signed, processed once, and attributed to the pg_sage
  user an admin mapped the chat user to. MCP agents get `sre_propose_action` and
  `sre_request_execution`, which create at most one approval item and never execute.
  Telegram is a new notification channel type. Requires `trust.level` `advisory` or
  `autonomous`.

- **Sage SRE investigates four more incident types: checkpoint storms, temp-file
  explosions, replication lag and LWLock contention.** Each one runs its own read-only
  probes and tells apart the usual causes. For checkpoint storms, that is `max_wal_size`
  too small for the write rate, something issuing `CHECKPOINT`, or a short
  `checkpoint_timeout`. For temp files, one runaway query, one statement that spills on
  every call, or `work_mem` small for many statements. For replication lag, where the lag
  sits: not sent, not flushed, or not replayed. On a standby, it also checks paused replay
  and standby queries holding replay back. For LWLock contention, the lock manager, WAL
  writes, buffers, or the subtransaction or multixact caches, attributed to a query. The
  packet also lists the explanations it ruled out and why. Investigations start from the
  matching RCA incidents. Replication lag incidents now start a replication lag
  investigation instead of a WAL retention one. A built-in detector also starts them for
  checkpoint storms, temp-file growth and LWLock contention, with conservative thresholds
  (documented in `docs/configuration.md`). Nothing is executed. PGIncidentBench gains
  fault programs, decoys and background-noise runs for all four types.

- **Runways and pre-incident investigations (Sage SRE).** pg_sage now watches how fast your
  database approaches four hard limits: transaction-ID wraparound, a full disk (when you
  declare `forecaster.disk_capacity_bytes`), the WAL a replication slot may retain, and a
  sequence running out (including a bigint sequence feeding an integer column). Every minute
  it samples these series and projects when each limit is reached. When one comes within its
  horizon (14 days for wraparound, 72 hours for disk and slots, 30 days for sequences), it
  opens a forecast finding and a read-only investigation that explains why: for example a
  forgotten transaction holding back vacuum, a slot nobody consumes, or a column narrower than
  its sequence. The investigation lists the existing freeze or WAL-bound action that fixes it
  with the policy gate's verdict, but never runs it; your custodians still act under your
  autonomy settings. A verified WAL bound now earns an "incident avoided" (disk full, near
  miss) when the measured fill trend shows the disk would have filled within the horizon and
  no longer does; managed providers are never credited. Settings are under `sre.runways`.

- **Sage SRE runbooks and incident memory.** You can now write typed runbooks for a
  database: a small flowchart of read-only catalog probes, yes/no decisions on their
  results or on the causal graph's hypotheses, and a final proposal (a manual step, a typed
  action to request through the normal approval flow, or "escalate"). Write one as JSON, or
  paste an English playbook (for example an imported Xata playbook) and your configured LLM
  turns it into a draft, which is checked against the probe catalog and graph. A runbook
  only ever runs after an admin signs the exact version they reviewed; any edit needs a new
  signature, and a retired runbook never runs. When a signed runbook matches an
  investigation, it adds its probes (within the usual 12-probe and 120 s limits), records
  which version ran and what it proposes, and never executes anything. Investigations also
  look up similar past incidents of the same database, with any outcome an operator
  confirmed or refuted, and show them to the model and in the Cases panel as context only
  (never as evidence, and never anything newer than the investigation itself). Manage
  runbooks under Advanced > Runbooks, the `/api/v1/databases/{db}/runbooks` routes, or the
  new MCP tools (which can draft but never sign).

- **Sage SRE investigations get a model turn (on by default whenever an LLM is
  configured).** After the causal graph diagnoses an incident, your configured LLM reviews
  the result. It can reorder the graph's own hypotheses (shown separately as "model
  ranking", never mixed into the graph's scores). While the graph is inconclusive, it can
  ask for one more catalog probe, after which the graph diagnoses again. It can also write up
  to 5 claims, each citing the evidence it rests on (shown as "model-generated narrative").
  Every reply is checked: known hypotheses only, catalog probes with valid arguments,
  evidence of this investigation, and every number found in that evidence. A bad reply gets
  one retry; after that, or on a timeout, a rate limit or an exhausted budget, the
  investigation keeps its deterministic result and records why (`model_rejected`). When the
  graph has a conclusive root cause it always wins, and a disagreement is recorded
  (`model_disagreed`). Each investigation is limited to 2 model turns, 16k input and 4k
  output tokens, and 120 s. The daily allocation is `llm.token_budget_daily`. Without an LLM,
  investigations stay deterministic and the sidecar logs once why. To turn it off, set
  `sre.llm.enabled: false`. Reasoning models (Gemini 2.5+/3, OpenAI o-series, DeepSeek R1)
  work. Their thinking gets its own 16k-token allowance per investigation, separate from
  the 4k answer limit and counted in the daily budget.

- **Wraparound near misses are credited.** When a table is inside the red wraparound
  buffer and pg_sage's verified `VACUUM (FREEZE)` returns it to green, the value ledger
  records one "incident avoided" (`xid_wraparound`, near miss, 120 minutes). Both sides are
  measured on the table, immediately before and after the action. The credit is separate
  from DBA-hours saved and links to the decision and its verification. Until now
  `sage.incident_avoided` was never written, so the Value page always showed zero
  incidents.
- **PGIncidentBench v1** (`sidecar/sre-bench`). It scores the Sage SRE investigator
  side by side with an "always escalate" baseline and a rules-only baseline. The
  scenarios cover lock blocking, connection pressure, WAL/replication retention and plan
  regression. Each fault program has a clean variant, a variant under unrelated background
  load, and benign decoys. A decoy looks like a fault, and the right answer is
  "inconclusive". Results are reported per family, never only pooled: Safe Pass (correct
  or abstain, with no forbidden action), top-1, top-3, abstention, selective accuracy,
  false diagnoses on decoys, run-to-run consistency, probe count and time to first
  evidence. Each result shows its denominator and a 95% Wilson interval. A safety grader
  checks the database itself for forbidden actions. The release gates are pre-registered
  as code constants: top-1 >= 80%, abstention >= 95% where evidence is insufficient, zero
  forbidden actions, and decoy or noise variants no more than 10 points below clean
  (CHECK-42). The bench fails when the investigator misses a gate. Gates that need the
  LLM-on arm or replay data are reported as "not evaluated", never as passed. The LLM-on
  arm runs the investigator with its model turn. By default it uses a built-in
  adversarial fake model. The fake ranks the graph's last hypothesis first, always asks
  for a probe, and sometimes sends bad replies (fenced JSON, unknown nodes, invented
  numbers, rate limits). The bench fails if, against the fake, the LLM-on arm scores lower
  than the deterministic arm on Safe Pass or top-1 in any family, or changes a root the
  deterministic arm concluded. It can be pointed at a real OpenAI-compatible endpoint
  (`PG_SAGE_BENCH_LLM_URL`, `_MODEL`, `_KEY`) to evaluate the §12 quality gates. The bench writes a JSON result and a Markdown summary to
  `SAGE_BENCH_REPORT_DIR`, and CI uploads them. On PostgreSQL 16 the deterministic
  investigator passes every gate it can be evaluated on, with no false root on any decoy.
  The rules-only baseline names a false root on every decoy that imitates a connection,
  WAL or plan fault. This is an in-distribution seed set, not a held-out measurement. The
  slow-consumer scenario now uses a logical slot, so it runs where `pg_hba.conf` refuses
  physical replication connections (it used to be skipped).
- **Sage SRE earned autonomy (on by default; existing autonomy is kept).** pg_sage keeps a
  level per incident
  family and action class: L0 observe, L1 script only, L2 one-click approval handoff, L3
  auto-execute a reversible single-object action inside the standing-policy window and
  notify. L4 is never reached, and irreversible classes never go above L1. pg_sage proposes
  a promotion one level at a time from the spec's evidence: PGIncidentBench results, 30
  days of accepted shadow reviews, verified recoveries and zero safety violations. Only an
  admin can approve it, and the approver is recorded. Error-budget fast burn, an unknown
  app SLO, a non-primary or recently failed-over node, stale evidence, a concurrent action
  on the same object, or a family safety regression caps actions at L1, and each cap is
  logged. A harmful outcome demotes the whole family until the evidence is earned again.
  A critical XID or disk deadline that the standing policy lets override (a red
  wraparound freeze, for example) is never held back by the ledger. It is still stopped by
  the emergency stop and the rest of the gate, and the ledger records it. Also added: game days on disposable clones (off by default), a fleet canary that halts
  and rolls back on regression, the **Advanced > Earned autonomy** page, and the
  `/api/v1/sre/autonomy` routes. The MCP tools `sre_get_autonomy` and
  `sre_downgrade_autonomy` are also new. See `sre.autonomy.*` in the configuration
  reference.
  On the integrated M5 and M6 code, the ledger reads each database's own SLO engine for
  the error-budget signal. An approved M5 backend cancel is approval-only (never above L2,
  never carried over), and each recovery pg_sage verified is recorded as evidence for its
  family. Executed custodian actions are recorded as outcomes, including mandatory deadline
  overrides, which never count toward promotion. The M6 reactive and runway families are in
  the ledger. `sre.autonomy.bench_results_path` now also reads the per-shard reports CI
  writes under `pgincidentbench/`.

### Changed (read before upgrading)

- **Sage SRE investigations now start by themselves.** `sre.automatic_start` defaults to
  `true`: after the upgrade, each open lock, connection or WAL incident and each plan
  regression gets one read-only investigation (catalog probes only, at most 12 probes and
  120 s, never an action). It works with or without an LLM. To keep starting them only by
  hand, set `sre.automatic_start: false`.
- **LLM features are on by default.** pg_sage is an AI DBA, so `llm.enabled`,
  `llm.optimizer.enabled`, `advisor.enabled`, `tuner.llm_enabled` and
  `rca.narration_enabled` now default to `true` (`explain.enabled` already did). They call
  a provider only once an LLM is configured: `llm.endpoint` and `llm.api_key` (or
  `SAGE_LLM_ENDPOINT` and `SAGE_LLM_API_KEY`). Without one, every feature keeps its
  deterministic path, no LLM request is made, and startup logs one line saying the LLM is
  not configured and how to set it.
  - **Upgrading:** if your config or saved Settings already has an endpoint and key but
    never set these switches, LLM calls start after the upgrade. An explicit `false` in
    YAML or a saved Settings/API value still wins.
  - **Cost:** spend is capped by `llm.token_budget_daily`, 500,000 tokens a day by default
    across all general-LLM features (`0` means no cap).
    [docs/llm-costing.md](docs/llm-costing.md) estimates about 150,000 tokens a day for a
    small-to-medium database and lists per-feature estimates and prices.
    `llm.optimizer_llm.enabled` stays `false`: turning it on adds a second client with its
    own daily budget.
  - **No new autonomy:** trust level, tier flags and execution mode keep their defaults.
    LLM output becomes a finding, a hint or a narrative; any change it proposes runs only
    through the policy gate, trust level and execution mode, like every other
    recommendation. With the default `trust.level: observation` nothing runs unattended.
  - **Turning features off:** `llm.enabled: false` turns every LLM feature off and is a
    hot kill switch. Per feature: `llm.optimizer.enabled`, `advisor.enabled`,
    `tuner.llm_enabled`, `rca.narration_enabled` and `explain.enabled` (the whole EXPLAIN
    endpoint) set to `false`; in fleet mode, `databases[].llm_enabled: false` per
    database. The Settings LLM tab toggles `llm.enabled`, the advisor and the optimizer,
    and `PUT /api/v1/config/global` accepts every key above except `tuner.llm_enabled`,
    which is YAML-only.
- Upgrading keeps the autonomy your configuration already grants. On first start, the
  ledger carries over the custodian freeze and autovacuum tuning, the WAL bound, and
  load-admitted index creation at the level your trust and execution settings let them
  run unattended today (shown as "carried over" with the decision that granted it).
  Everything else starts at L1. Carried-over actions now also pause while an SLO burns,
  the node is not primary or was just failed over, the evidence is stale, another action
  touches the same object, or the family had a harmful outcome. They resume by themselves
  when that clears, and each change is recorded. An operator downgrade ends the carry-over:
  the level then has to be earned with evidence. `sre.autonomy.enforce: false` turns the
  ledger off entirely, and the sidecar warns at startup when you do. At startup, the
  schema upgrade adds the `sage.sre_family_autonomy`, `sre_autonomy_*`,
  `sre_packet_reviews`, `sre_eval_runs`, `sre_game_days` and `rollout_instance` tables,
  plus four nullable columns on `sage.rollout_run`.

- If you already run Sage SRE investigations with an LLM configured, they start using the
  model turn after the upgrade. To keep them deterministic, set `sre.llm.enabled: false`.
  At startup, the schema upgrade replaces the event-type check on `sage.sre_events` with
  `sre_events_event_type_m3`, which allows the three new event types. Existing rows are
  validated without a long table lock.
- `disk_full_slot` and `lock_storm` incidents are still not credited. pg_sage does not yet
  measure disk-fill trend or lock-storm recovery, so it makes no claim for them.

### Fixed

- **A connection leak beside another application's steady pool is now the root cause.** The
  steady pool tied with the leak and was named the cause. A pool that does not grow cannot
  explain pressure that does, so it is now listed as contributing (the baseline the leak
  grows on) and the leaking application is the root. A pool of the leaking application
  itself is still that leak. The graph version stays `causal-v3`. The replay case that
  exposed this is no longer held-out evidence for this shape; a fresh case with different
  numbers, outside the corpus, checks it.

- **Sage SRE no longer reports "no lock waits" when it cannot see them.** A database role
  without `pg_read_all_stats` (included in `pg_monitor`) sees other users' sessions without
  their state or wait events, so investigations reported no blocking, no long transactions
  and too few connections. Those probes now report "no privilege" and the investigation
  says the evidence is missing instead of guessing. Grant `pg_monitor` to the role pg_sage
  uses (the documented setup already does).
- **Sage SRE ignores evidence that is too old or contradictory.** An observation more than
  5 minutes older than the investigation's newest one (for example after an investigation
  was paused and resumed), or two connection samples taken at the same instant, are listed
  as missing evidence instead of supporting a root cause.
- **The new incident families follow the same rule.** LWLock waits, a standby's longest
  query, the statements spilling temp files, the sessions holding back the xmin horizon and
  the busy autovacuum workers also need `pg_read_all_stats`; without it those probes report
  "no privilege" instead of "no contention" or "no holder".
- **`forecaster.disk_capacity_bytes` no longer claims to auto-detect.** Nothing detects disk
  capacity (PostgreSQL cannot report free space over SQL), so `0`, the default, means
  undeclared: no disk runway and no disk-full credit. Set it for self-managed servers.

- Sage SRE connection and WAL investigations no longer fail when
  `sre.sample_interval_seconds` is set close to its maximum of 30. The worker did not
  renew its 30-second claim on an investigation while it waited between the two samples,
  so the claim ran out during the wait. The investigation was then retried and lost again
  until its time budget was used up, and it ended as `failed` (`budget_exhausted`). The
  worker now renews the claim during the wait. If an operator stops the investigation
  during the wait, the worker stops within seconds instead of waiting out the interval.
- **The tuner no longer fails per query without an LLM.** With `tuner.llm_enabled` set and
  no LLM configured (or the circuit open, or the daily budget spent), every tuning
  candidate fetched its plan context, logged "LLM prescribe failed" and wrote an
  `llm_suppression` finding, every cycle. It now uses its deterministic hints silently,
  and a budget refusal no longer suppresses LLM tuning for the query.
- **The deprecated `llm.index_optimizer` block honours explicit values.** It used to turn
  the optimizer on even when `llm.optimizer.enabled: false` was set. Now
  `llm.optimizer.enabled` wins whenever it is present, and an explicit
  `llm.index_optimizer.enabled` (true or false) applies only when it is not.
- The collector no longer reads `pg_settings` and every table's reloptions each cycle for
  a configuration advisor that cannot run (no usable LLM at startup).
- `pg_sage_optimizer_enabled` reports whether the index optimizer can run (enabled and its
  LLM configured), not just the config value, which is now `true` by default.

## v1.7.0 (2026-09-30) -- Sage SRE investigations, earned index autonomy

### What's new

- **Sage SRE investigations (opt-in: `sre.automatic_start: true`).** Incidents and plan
  regressions get a deterministic, evidence-first investigation. It uses 12 bounded
  read-only probes, a causal graph for lock blocking, plan regression, connection pressure
  and WAL/slot problems, and ruled-out alternatives. You can read investigations in the
  Cases panel, the API, MCP and a redacted export. Lock chains page within a minute. LLM
  narration is opt-in and must cite evidence.
- **pg_sage earns index autonomy.** It learns each database's IO baseline from Postgres
  itself (`pg_stat_io`/`pg_stat_wal`). After 7 days it builds the indexes it needs during
  quiet periods. Capacity declared by an operator overrides the learned baseline.
- **Durable recommendations.** Every actionable finding is a recommendation with immutable
  revisions. An approval pins one exact revision and executes once, even when
  operators race.
  Failed applies back off and are then abandoned, and verification verdicts are recorded.
- **The policy means what it says.** The refusal set, lock ceiling and serialization are
  enforced. There is one maintenance-window grammar, with timezones. Approvals are
  attributed and single-use. The emergency stop survives restarts and has a header
  control.
- **One runtime, one pipeline.** Standalone, fleet, meta-db and AgentDB build every database
  the same way, and every change goes through one `Executor.Apply` pipeline. AgentDB
  databases previously had no executor or policy gate at all.

### Added

- Startup logs a notice when `trust.maintenance_window` uses a form whose meaning
  changed with the unified window grammar (for example `weeknights`, a day window
  that crosses midnight, or cron ranges/steps), explaining the new meaning.
- **SSO account linking.** A user signed in with a password can link SSO from their account
  page (`#/profile`). Admins can unlink SSO, issue a one-time link grant (single use,
  15 minutes, stored hashed) for a user who cannot sign in with a password, and create
  SSO-only users with no password. Linking requires a verified provider email that matches
  the account; accounts are never linked on email alone. `GET /api/v1/users` now reports
  `sso_linked`, `sso_issuer` and `password_login`. Link, unlink and grant events are
  written to the new `sage.auth_audit` table. See `docs/security.md`.
- **Readable SSO sign-in errors.** A refused or failed SSO callback now returns the browser
  to the login page (or the account page for a link) with an explanation, instead of
  showing raw JSON. API clients still receive the JSON status.
- **Lock chains open incidents within a minute.** A lock-chain fast path runs every
  `rca.lock_chain_interval_seconds` (default `60`, `0` disables it) instead of waiting for the
  600 s analyzer cycle. It opens or updates the `lock_contention` incident and sends
  `incident_detected`. The incident records each root blocker's identity (pid,
  `backend_start`, query id and a hash of the query text) as structured `causal_chain`
  evidence. Escalation and auto-resolution still count analyzer cycles, so
  `escalation_cycles` and `resolution_cycles` keep their meaning.
- **Plan fingerprints.** Captured plans get a stable `plan_hash` that ignores costs, row
  counts, timings and literals. It is stored in `sage.explain_cache` and copied to every
  `sage.query_store` sample, so a plan flip shows up in the per-query history.
  `plan_regression` findings now say whether the plan shape changed (`plan_changed`).
- **Incident narration (opt-in).** With `rca.narration_enabled: true`, detected and escalated
  incident notifications carry an LLM summary. The model can only read the incident's own
  evidence, must cite it, and may only use numbers that appear in it. Every other case
  (narration off, LLM off, budget, rate limit, timeout, malformed or uncited output) sends
  the deterministic summary. Both are labeled in the notification. `llm.enabled: false`
  cancels narrations in flight.
- **Durable recommendations.** Every actionable finding is now a recommendation in
  `sage.recommendation`, with immutable revisions in `sage.recommendation_revision` (forward
  SQL, inverse SQL, evidence, preconditions, standing-policy version and a content hash) and
  its full history in `sage.recommendation_transition`. States: proposed, approved,
  applying, applied, verifying, then verified, reverted or inconclusive, plus superseded,
  failed and abandoned. Every change is a compare-and-set on (id, state, revision), so two
  workers can never both act on one recommendation. Two candidate indexes on one table are
  now two recommendations (C05). New read-only API: `GET /api/v1/recommendations?database=`
  and `GET /api/v1/recommendations/{id}` (revisions and history). The Actions page has a
  Recommendations tab, and pending approvals show the revision they approve.
- **Sage SRE investigations (opt-in, read-only).** With `sre.automatic_start: true`, each
  open RCA incident of the lock, connection or WAL family and each open `plan_regression`
  finding gets one investigation. It runs its family's fixed catalog probes (connection and
  WAL investigations sample twice, `sre.sample_interval_seconds` apart), matches the causal
  graph and stores the diagnosis: likely explanation, contributing factors, alternatives and
  ruled-out explanations, each citing its evidence, plus missing evidence and an operator
  step. No LLM is used and nothing is executed; "inconclusive" is a normal outcome. Every
  mode (standalone, YAML fleet, meta-db, AgentDB) runs it. A second worker can never commit
  over the first, and a crash resumes at the next step. Every change to an investigation is
  recorded in an append-only hash chain (`sage.sre_events`).
- **New causal families.** Connection pressure: pool fan-out, blocked-query backlog and
  connection leak, with the share of usable connections stated separately. WAL retention:
  inactive slot, slow slot consumer, archiver failure and write surge; a surge is reported
  as contributing to slot retention, never instead of it, and remaining disk space is always
  reported as unknown. Every diagnosis also answers "did pg_sage cause this?" from pg_sage's
  own actions in the last hour. New probes: `archiver` and `sage_actions`.
- **Cases panel investigations.** A case shows its investigation: state (inconclusive is
  never shown as resolved), likely explanation with a score and evidence-strength label,
  other and ruled-out explanations with their reasons, missing evidence, next check, and
  evidence items that open from each claim. Operators and admins can pin an investigation
  (retention keeps it) and export it as JSON or Markdown.
- **Investigation API and MCP tools.** `GET /api/v1/investigations?database=` and per
  database `GET /api/v1/databases/{db}/investigations[/{id}[/events|/evidence/{id}]]` for
  every signed-in role; `GET .../{id}/export` and `POST .../{id}/pin|unpin` for operators and
  admins. MCP adds read-only `sre_list_incidents`, `sre_get_investigation` and
  `sre_get_evidence`. Everything leaving the store is redacted (connection URIs,
  credentials, bearer tokens, SQL literals, raw vectors). See "Sage SRE investigations" in
  `docs/configuration.md`.
- **Investigation retention.** Finished, unpinned investigations lose their evidence after
  `sre.evidence_retention_days` (30) and are deleted after `sre.timeline_retention_days`
  (90). Each delete leaves a tombstone in `sage.sre_tombstones`; a conclusion whose evidence
  is gone is shown as such.
- **PGIncidentBench seed** (`sidecar/sre-bench`): 20 fault programs that create real lock,
  connection, WAL and plan incidents on PostgreSQL, run them through the investigator and
  report precision, recall, top-1 and abstention per family. On PostgreSQL 17 the seed set
  scores 100% on every metric (20 scenarios; an in-distribution seed, not a held-out
  measurement).

### Changed (read before upgrading)

- **The executor acts on durable recommendations, not the last analyzer cycle (C07).**
  A recommendation proposed while pg_sage was observing, or outside the maintenance window,
  now runs once policy allows it, without waiting for a new analyzer finding. Before acting
  the executor checks that the recommendation has not changed and that its finding is still
  open; a closed finding supersedes it.
- **An approval approves one exact revision (C04).** New content (for example new inverse
  SQL) is a new revision: it clears the approval and supersedes queued proposals for the old
  content. Approving such a proposal returns `409 Conflict` and nothing runs; approve the
  current revision instead.
- **Failed applies back off and are abandoned when the retry budget is spent.** A failed
  apply is recorded as `failed` with a backoff (5 minutes, doubling, at most 6 hours). It
  never silently returns to proposed. After the retry budget (two retries) it is `abandoned`
  with the reason. An apply interrupted by a crash is recovered as a failed attempt once its
  claim expires. The action log row and the applied transition are written in one
  transaction.
- **Verification verdicts are recorded apart from their effect (C15).** A recommendation
  whose change regressed shows the verdict `regressed` but stays `verifying` until the
  revert has run; only then is it `reverted`.
- **Upgrade migrates existing work.** On start, each database's open findings with SQL and
  its pending or approved (not yet executed) queued actions are mapped to recommendations.
  An approval keeps its approver and is pinned to a revision holding the exact queued SQL
  (marked `migrated`). The migration is idempotent. Retention purges terminal
  recommendations after `retention.actions_days` and keeps live ones.
- `executor.New` no longer takes an analyzer argument.

- **Emergency stop in the header (D8).** Operators and admins get a Stop control next to
  the database picker. It stops the selected database, or all databases when "All" is
  selected, and needs arm then confirm. Resume appears only when the selection is stopped,
  also needs confirmation, and shows who stopped it and when. A badge visible to every role
  names the stopped database, the person who stopped it and the time. The Settings page
  buttons are replaced by a pointer to the header control.
- **The stop survives restarts.** At startup and on reconnect, every mode (standalone,
  fleet, meta-db) restores the in-memory stop from `sage.config`, so the header badge and
  action readiness show "stopped" on first paint. An unreadable flag restores as stopped,
  attributed to `system`.
- **Attribution and audit.** Stop and resume record the signed-in user (or `system`) in
  `sage.config.updated_by` and append a `sage.config_audit` row in the same statement. The
  fleet API (`GET /api/v1/databases`) exposes `emergency_stopped`, `emergency_stopped_by`
  and `emergency_stopped_at` per database, and `summary.emergency_stopped_count`.
- **Declared IO capacity.** Operators can attest provisioned throughput with
  `verify.io_capacity` (standalone) or `databases[].verify.io_capacity` (fleet):
  `read_write_mbps` and `wal_mbps` in MiB/s. Utilization is the measured rate
  divided by the declared capacity, compared with the IO ceilings. A declaration
  overrides the learned baseline. A fleet-wide `verify.io_capacity` is rejected.
- **Admission evidence in the ledger.** Every decision records which evidence
  mode admitted or withheld the build (`declared_capacity`, `learned_baseline`
  or `unavailable`), with the rates, capacity or baseline, in
  `sage.decision.evidence.load_admission`.
- **Visible withheld reason.** A withheld build is recorded once per finding and
  reason in `sage.admission_withheld` instead of a new failed action every
  cycle. `GET /api/v1/admission` and `GET /api/v1/admission/{name}` report each
  database's mode, reason and baseline progress, and the dashboard shows them on
  the Actions page and in the Overview provider-readiness tab.
- Supabase provider observability now supplies host CPU only; it never reported
  disk utilization.
- **Diagnostic probe catalog (Sage SRE).** Twelve fixed, read-only probes: lock chains and
  the lock wait graph, long and idle-in-transaction transactions, prepared transactions,
  backend identity, connection saturation, replication lag, replication slots,
  WAL/checkpoint, autovacuum/wraparound, vacuum progress and plan regressions. Each runs in
  a read-only transaction with a fixed `search_path`, `statement_timeout` 500 ms and
  `lock_timeout` 100 ms, returns at most 500 rows and 256 KiB, and at most one probe runs
  per database and four per sidecar at a time. Results are typed (`ok`, `empty`, `error`,
  `no_privilege`, `unsupported`), so a missing privilege, extension or table is never
  reported as healthy. Probes return identities, states, counts and ages, never query text.
- **Causal graph for lock blocking and plan regressions.** A deterministic matcher (no LLM)
  tells an idle-in-transaction holder, DDL queued behind a long transaction, hot-row
  contention and a prepared-transaction holder apart, and for a slow query a `plan_hash`
  flip from a slowdown on the same plan. Each hypothesis carries its evidence, a confidence,
  a refutation probe, and the alternatives it ruled out with the evidence that ruled them
  out. A cleared chain, a wait cycle or missing evidence gives "inconclusive", not a guess.
- **Lock incident notifications name the likely cause.** The deterministic summary of a
  `lock_contention` notification adds, for example, `Likely (H1, confidence 0.85):
  idle-in-transaction holder, pid 4242; contributing (H2): DDL queued behind a long
  transaction`, and names probes that could not run (`Missing: lock_graph
  (no_privilege)`). With `rca.narration_enabled`, the model can also read the probe results
  (`get_probe_result`) and hypotheses. It answers with claims, each citing evidence ids, and
  every number in a claim must appear in the evidence that claim cites.
- **Investigation store foundation.** Durable investigations with
  leases and fence tokens (a stale worker cannot commit), idempotent steps, immutable hashed
  probe evidence, and durable per-investigation model budget reservations (2 turns, 16k input
  and 4k output tokens, plus database and deployment daily allocations) that a crash cannot
  reset. A metadata outage puts coordination in an explicit degraded state that blocks
  action handoff.

- **Schema (Sage SRE M2).** Added automatically at startup: `sage.sre_hypotheses`,
  `sage.sre_events` (UPDATE is refused by a trigger, as it now is on `sage.sre_evidence`),
  `sage.sre_tombstones`, and new columns on `sage.sre_investigations` (`source_incident_id`,
  `subject`, `pinned`, `summary`, `concluded_at`, `evidence_purged_at`).
- The `connection_saturation` probe is now `v2` and also returns the server start time.
- RCA narration and the investigator now share one probe runner per database, so at most
  one catalog probe runs on a database at a time across both.
- An investigation whose active-time budget a dead worker used up now ends as `failed`
  (`budget_exhausted`) instead of staying `collecting`.
- Stored probe evidence keeps 64-bit integers exact; a `queryid` above 2^53 was rounded.

- **Every mode builds a database's runtime the same way.** Standalone, YAML fleet,
  meta-db and AgentDB now share one constructor, so the safety wiring (standing policy
  gate, emergency stop, managed-config adapter, trust and executor gates, notifications)
  is identical. What operators will notice:
  - **AgentDB databases** get the full runtime instead of a collector only: the sage
    schema is bootstrapped into the agent database, and findings, notifications,
    retention and actions follow the standing policy gate and a fresh trust ramp.
  - **Meta-db databases** gain the forecaster, auto_explain plan collection, schema lint,
    the migration advisor, log-based RCA, hint revalidation and the rule-based tuner
    without an LLM. The config advisor targets the PostgreSQL database name rather than
    the instance name.
  - **YAML fleet and meta-db:** the index optimizer uses auto_explain plans when the
    extension is available, and databases without an LLM get the scheduled briefing.
  - **Standalone:** the config advisor applies provider-specific rewrites.
  - A YAML fleet database whose schema bootstrap fails is shown as failed instead of
    running without its schema.
  - Shutdown waits, within its deadline, for every database's workers.
  - `--meta-db` with `mode: standalone` no longer registers a phantom database.
  - AgentDB databases also get index-build load admission (D6), the Sage SRE fast path
    and catalog probes, value-ledger reporting (D3) and the restored emergency stop (D8).
- **One execution pipeline.** Every change the executor makes (the background cycle,
  operator-approved actions, custodians, verified indexes and retention authorization)
  goes through the same steps: authorize, take the change lease and a DDL slot,
  re-authorize, then run under a deadline and verify.
  - **Operator-approved actions are re-authorized by the standing gate** right before
    they run, so an emergency stop or policy change made after the approval stops them.
  - **Executor DDL always has a timeout.** `safety.ddl_timeout_seconds: 0` used to mean
    no statement timeout; it now means the 300-second default. A client-side deadline one
    minute longer backs it up.
- **Cloud AgentDB registration needs an approved request.** `POST /api/v1/agent-dbs`
  with a cloud provider now returns `409 approved request required` unless it names an
  approved, unused `request_id`. Provision approved requests with
  `POST /api/v1/agent-dbs/requests/{id}/provision`, which now accepts `size_profile_id`,
  `schema_name`, `secret_ref` and `secret_ref_provider`. Each approval produces exactly one
  deployment; reusing it, or provisioning it for another tenant, agent or provider, returns
  `409`. `local_postgres` schema and database registers are unchanged. Existing deployments
  are left as they are. The dashboard's Provision form uses the request route.
- **Blueprint and Terraform-template provisioning also consume a request.** An approved
  blueprint or template is a reviewed design, not permission to spend: provisioning a cloud
  plan from one now needs a signed-in user and an approved, unused `request_id` matching its
  tenant, agent and provider, and returns `409` without one or on reuse. The dashboard
  requests approval before provisioning from the Blueprints and Terraform panels.
- **AgentDB approvals are attributed.** Approve and deny record the signed-in user in
  `decided_by`/`decided_at` and require a session. A consumed request records
  `consumed_deployment_id`, `consumed_by` and `consumed_at`, and its decision can no longer
  change. Decisions that request policy made at creation show `decided_by: "policy"`.
- **One operator team per install.** AgentDB operators and admins act on every tenant; the
  docs now say so. Do not share an install between teams that must be isolated.
- **Retention deletes need an owner-declared column (D5).** `declare_table_contract`
  now takes `retention: {"interval": "...", "column": "..."}`; an interval without a
  column (including the old bare-string form) is rejected, and the column must exist
  and be `timestamptz`, `timestamp` or `date`. pg_sage no longer guesses `created_at` or
  `occurred_at`. **Existing retention contracts pause** (no deletes) until they are
  re-declared with a column; the ledger park reason shows the suggested column, e.g.
  `retention column not declared; suggested: created_at — re-declare the contract with
  retention.column to enable deletes`. Count affected contracts with
  `SELECT count(*) FROM sage.table_contract WHERE append_only AND retention_interval IS
  NOT NULL AND retention_column IS NULL`. The upgrade adds
  `sage.table_contract.retention_column` without backfilling it.
- **Dry runs are bound to identity, not names.** A reviewed dry run now authorizes
  deletion only for the same table OID, column attnum and type, and contract version.
  Renaming or swapping the column, rebuilding the table, re-declaring the contract, or
  eligible rows growing past 2x + 100 of the dry run's count starts a new 24-hour
  review. Dry runs recorded before this release no longer qualify. Each delete batch
  locks the table `ROW EXCLUSIVE` and re-checks the column before deleting.
- **Value is counted across the whole fleet (D3).** Each monitored database keeps its own
  value ledger, next to the actions it credits. The Value page (`GET /api/v1/value`), the
  Prometheus value metrics and the MCP `get_value` tool now add up every database in
  standalone, YAML fleet and meta-db mode. Before, YAML fleet showed only the first database
  and meta-db mode showed zero, because the meta database holds no ledger. Nothing is
  migrated.
- **Value metrics carry the database name.** `pg_sage_toil_minutes_saved` and
  `pg_sage_incidents_avoided_total` are labelled `database="<instance name>"` (never empty).
  `pg_sage_value_metrics_up` is now reported per database, so update alerts that expect the
  unlabelled series. The value metrics are also exported in YAML fleet mode, where they were
  missing.
- **Partial value is reported, not hidden.** If a database cannot be read, the value response
  sets `"partial": true` and names it in `"unavailable"`, and the other databases still count.
  The Value page shows a warning. `?database=<name>` works in every mode, including
  standalone, and an unknown name returns 404. Two fleet entries for the same physical
  database are counted once.
- **Credited value is kept.** Retention no longer purges credited actions (verified success
  with toil credit) or the verification that earned the credit, so all-time value no longer
  shrinks after `retention.actions_days`. Uncredited, failed and rolled-back rows still age
  out.
- Schema: `sage.explain_cache` gains a nullable `plan_hash` column (added automatically at
  startup).
- Notifications for `incident_detected` and `incident_escalated` now end with a labeled
  `Summary` line, and their payload data adds `narrative` and `narrative_source`.

- **Defined stop/resume races.** Stops latch memory immediately; persisted transitions are
  serialized. A stop that overlaps a resume wins, and memory always ends equal to the
  persisted flag. A stop that lands while a meta-db reconnect swaps in a new runtime
  carries over to the new runtime and is saved. `sage.config_audit` gains a nullable `changed_by_actor` column, added
  automatically at startup.
- **Behaviour change: autonomous index builds can now be admitted.** Until now no
  source produced IO evidence, so load admission withheld every autonomous
  `CREATE INDEX CONCURRENTLY` and every custodian index proposal (such as FK
  supporting indexes) on every deployment. pg_sage now samples IO from Postgres
  every minute (`pg_stat_io` on PG16+, `pg_stat_database` and `pg_stat_bgwriter`
  on PG14/15, `pg_stat_wal` for WAL) and learns each database's baseline in
  `sage.io_rate_sample`. **After 7 days of observation (`verify.io_baseline_days`),
  autonomous index builds are admitted during quiet periods**: when current data
  and WAL rates are at or below the learned median. While the baseline is still
  being learned, builds stay withheld with a reason such as
  `learning IO baseline: 3.0/7 days`. Set `verify.io_baseline_days: 0` to keep
  today's fail-closed behaviour. The trust level, tier flags, standing policy and
  maintenance windows still apply as before.
- **Unknown host CPU narrows admission to maintenance windows.** Without a CPU
  reader (anything except Supabase with an observability token), a build is
  admitted only while `trust.maintenance_window` and the policy windows are open.
- **Separate IO ceilings.** `safety.data_io_ceiling_pct` and
  `safety.wal_io_ceiling_pct` (default 70) no longer inherit
  `safety.cpu_ceiling_pct`.
- **The policy refusal set is enforced.** `refusal_set` was stored but never
  checked. Each token now matches precisely: `rls_change` (row-level security
  or its policies), `grant_expansion` (GRANT, ALTER ROLE, OWNER TO, ALTER
  DEFAULT PRIVILEGES), `major_upgrade` (major or extension upgrades),
  `non_dup_object_drop` (dropping a table, column, constraint, sequence,
  schema or replication slot) and `unrollbackable` (rollback class
  `not_reversible` or `forward_fix_only`). A self-initiated action that
  matches is queued for approval with reason `refused_by_policy`, and the
  token is in the detail. An operator-approved action is not refused.
  - No action pg_sage takes today changes verdict under the built-in
    profiles. Unused, duplicate and invalid index drops are rebuildable and
    keep their earned autonomy. A retention delete on the column the owner
    declared as the contract's `retention.column` (D5) runs under that
    declaration and is not "unrollbackable"; one without a declared column
    is queued for approval.
  - `set_table_autovacuum` is now reversible: it ships `ALTER TABLE ... RESET`.
  - Unknown refusal tokens are rejected when a policy is proposed. A stored
    policy that contains one fails closed (`policy_unavailable`), and startup
    logs which token to fix.
- **The policy lock ceiling is enforced.** For DDL that runs inside a
  transaction, autonomous or operator-approved, `lock_timeout` is the smaller
  of `lock_duration_ceiling_ms` (3000 in both profiles) and
  `safety.lock_timeout_ms` (default 30000). An `ALTER TABLE` queued behind a
  long lock now gives up after 3 s instead of 30 s. `CONCURRENTLY` builds keep
  `safety.lock_timeout_ms`, because they wait on older transactions by design.
  The ceiling also caps `ANALYZE` (autonomous and operator-approved) and
  in-transaction custodian changes.
- **DDL lease conflicts park instead of failing.** When another writer holds the
  change lease for the same object, the action is parked (`ddl_conflict` in the
  decision ledger) and retried next cycle. It used to log a failed action, which
  counted toward the three-failure abandonment and the self-initiated rate
  limit.
- **One maintenance-window grammar.** `trust.maintenance_window` and the
  standing policy's `maintenance_windows` are now parsed by the same engine, so
  a string means the same thing in both. See "Maintenance windows" in
  `docs/configuration.md`.
  - Config windows gain cron lists, ranges and steps (`0 2 * * 1-5`,
    `*/15 2 * * *`). These used to be accepted and silently mean "never".
  - Policy windows gain the config presets (`nights`, `weeknights`,
    `weekdays`, `business-hours`, ...) and day lists (`Mon-Fri`, `sat,sun`,
    `daily`).
  - A cron window is one hour wide from each matching minute, or as long as
    an optional `@<duration>` says (`0 2 * * * @30m`, `@1m` to `@24h`).
    `30 * * * *` therefore covers every hour; it used to cover only :30-:59.
  - A range that crosses midnight belongs to the day it starts on.
    `weeknights` now covers Friday 22:00 to Saturday 06:00 and no longer
    covers Sunday 22:00 to Monday 06:00. Lists behave the same way:
    `Mon,Wed,Fri 22:00-04:00` covers Saturday 02:00 (Friday night), not
    Wednesday 02:00 (Tuesday night).
  - An optional trailing IANA time zone evaluates a window on that zone's
    clock, including DST: `weekdays 01:00-05:00 America/Chicago`. Without one
    the process clock is used, as before (UTC in the container).
- **Invalid `trust.maintenance_window` values are rejected** at config load,
  file reload and API save (HTTP 400 naming the grammar). A typo such as
  `weeknigths` used to be stored and silently close the window. `never`,
  `off`, `none` and `disabled` still close it.
  An override saved before this release that no longer validates is logged at
  startup with its value and how to fix or delete it. It still keeps the
  window closed until it is fixed.
- **Stored policies are migrated to schema version 3.** Each policy cron window
  is rewritten from `<cron>` to `<cron> @1m`, so it keeps its exact
  one-minute meaning. The built-in profiles have no cron windows and are
  unchanged. Superseded history is not rewritten.
- **Downgrade risk.** A release older than this one cannot parse `@<duration>`
  or a time-zone suffix in a policy window. If the active policy contains one
  (including a migrated `@1m`), an older binary fails closed with
  `policy_unavailable` and blocks every action until a policy it can parse is
  active. Before downgrading, ratify a policy version without those suffixes.
- Schema: new tables `sage.sre_deployments`, `sage.sre_database_bindings`,
  `sage.sre_investigations`, `sage.sre_steps`, `sage.sre_evidence` and
  `sage.sre_budget_reservations` (added automatically at startup, in the meta database when
  one is configured). Nothing writes them yet; retention for them lands with the
  investigator.

- An executor that could not get a database connection to take its DDL lease
  (pool exhausted by the change it conflicts with) waited until the other
  change's lock timeout, which then failed it. Taking a lease now waits at most
  5 s; a busy pool parks the action like a lease conflict (retried next cycle,
  never a failed action), and recording the park is bounded the same way.
- A refreshed recommendation could pair new forward SQL with the previous rollback SQL and
  still be approved and run as the old approval (C04). Revisions are now atomic and an
  approval covers exactly one of them.

- A retention contract that cannot act no longer stops the schema scan: later
  invariants (such as missing foreign-key indexes) are still planned in the same cycle.
- Re-declaring a table contract updates the one contract row instead of adding another.
  The upgrade removes existing duplicates (keeping the newest per table) and adds a
  unique index on `(COALESCE(database_id, 0), schema_name, table_name)`.
- A dry run still in its 24-hour review parks with `retention dry run in review until
  <time>` instead of being logged as a failure; the rest of the scan continues.
- In fleet mode, and with several databases on one server, a database reported and paged on
  lock chains that belonged to another database. Lock-chain detection now only starts from
  sessions in the monitored database.
- Incident causal chains that contained control characters or invalid UTF-8 failed to
  save.
- The lock-chain fast path started on the first analyzer cycle that had a snapshot. The
  first cycle usually runs before the collector's first snapshot, so after startup a lock
  chain could take a whole analyzer interval (default 600 s) to page. The fast path now starts
  when the database is registered, and instance shutdown waits for it instead of closing its
  pool under it.

### Deprecated

- **`tuner.analyze_maintenance_threshold_mb` is ignored.** It never had an
  effect. A config file that still sets it loads normally and logs
  "tuner.analyze_maintenance_threshold_mb is no longer used and is ignored;
  remove it". The key is gone from the example configs.

### Fixed

- An operator execution of an approved recommendation could run twice: a second
  call that looked up the recommendation after the first had claimed it found
  nothing claimable and ran the SQL unclaimed. An in-flight or applied
  recommendation for the same finding and SQL now refuses with a conflict.
- Schema lint `lint_mxid_age` computed MultiXact age with the transaction-ID
  `age()` function, reporting a bogus ~2.1B "approaching wraparound" critical
  finding once MultiXact IDs outpaced XIDs. It now uses `mxid_age()`.

## v1.6.0 (2026-09-27) -- Safety gate, SQL parse-tree validation, Azure

### What's new

- **Azure Database for PostgreSQL.** Flexible server runs every portable maintenance
  action. Server parameters are applied through Azure Resource Manager and reported as
  `applied_pending_restart` when a restart is needed. Credentials come only from the Azure
  identity chain (service principal, workload or managed identity, or `az login`). Existing
  Cosmos DB for PostgreSQL clusters are detected, with parameters guidance-only.
  Verified live on flexible server PostgreSQL 14–18, the General Purpose tier and elastic
  clusters.
- **SQL parse-tree validation.** Every statement pg_sage executes is parsed by PostgreSQL's
  own parser (libpg_query) and checked by structure, as a second layer after the text
  validator. It catches quoted and Unicode-escaped schema names, set operations and multiple
  `ALTER TABLE` subcommands. Release binaries and Docker images always include it;
  `pg_sage --version` reports `sql-ast:`.
- **One policy authority.** The standing policy gate now decides every action: unattended,
  operator-approved, and what the dashboard shows as ready. Operator approvals obey the
  policy's change-class allowlist and maintenance windows. Readiness shows the real verdict
  instead of assuming a satisfied trust ramp.
- **Azure test scripts** in `scripts/azure/` provision a throwaway server, run the live
  checklist, run a flavor matrix and tear everything down.
- **CI** runs the race detector, a PostgreSQL 14–18 matrix with pg_hint_plan, a skip
  budget, and the documented quick start as a least-privileged role.

### Changed (read before upgrading)

- **Stricter trust gate.**
  - Safe actions need `tier3_safe` plus 8 ramp days.
  - Moderate actions need `tier3_moderate`, 31 days, and an open policy window and
    `trust.maintenance_window`; otherwise they queue.
  - `alter_table`, `reindex_concurrently`, backend cancel/terminate and `apply_query_hint`
    always need approval.
- **Policy documents are migrated automatically.** The new change classes `backend_signal`,
  `query_hint` and `schema_change` are granted where `index` was, so effective permissions
  are unchanged. Policy history is not rewritten.
- **Builds without a C compiler** leave out parse-tree validation. They log a warning and
  send unattended changes to operator approval (`sql_validation_degraded`).
- **The C extension is removed.** `mode: extension` fails at startup. The default mode is
  standalone, and `--meta-db` implies meta. The last extension source is at tag
  `c-extension-final`.
- **Meta-db trust:** the global `trust.level` is a ceiling for every database.
- **OIDC** requires `email_verified` and matches issuer + subject. Password accounts are
  refused at SSO login until account linking exists.
- **Notifications:**
  - Private and metadata targets are refused unless
    `notification_policy.allow_private_targets: true` (YAML only).
  - Channel secrets are encrypted at rest.
  - Critical alerts bypass quiet hours; others are deferred, not dropped.
- **HTTP MCP:** viewers get read-only tools, and the acting user is recorded.
- **Retention deletes** need a matching dry run 24 hours to 7 days old and go through the
  policy gate.
- **Migrations** never auto-promote until workload capture exists.
- **Metrics:** `pg_sage_toil_minutes_saved_total` is replaced by the gauge
  `pg_sage_toil_minutes_saved`. Added `pg_sage_value_metrics_up` and per-database LLM
  budget metrics.
- **AgentDB:**
  - Agent API tokens are tenant-bound.
  - `secret_ref` must be `env:PG_SAGE_AGENTDB_*`.
  - Local provisioning needs `PG_SAGE_AGENTDB_LOCAL_PROVISIONING`.
- **Building from source** needs a C compiler for parse-tree validation. Release builds use
  cgo: static Linux amd64/arm64 and macOS amd64/arm64.

### Fixed

- **SQL safety**
  - EXPLAIN statement escape.
  - Executor whitelist bypass through multiple `ALTER` subcommands and unusual whitespace.
  - DDL literals and role/subscription DDL are no longer sent to the LLM.
- **Retention:** deletes are bound to one partition and re-check the cutoff; the delete and
  its audit record commit together.
- **Undo and rollback**
  - Reverting an index created with `IF NOT EXISTS` could drop a pre-existing index; revert
    now drops only an index pg_sage recorded creating.
  - Index rollbacks are concurrent.
  - Pending reverts resume after a crash.
  - Rollback by queue id is fixed.
- **Backends and WAL**
  - Backend cancel re-checks the backend's identity, so a reused PID is never cancelled.
  - The WAL custodian no longer shrinks retention below what replication slots hold.
- **LLM configuration advice** uses the correct base unit for memory settings.
- **Emergency stop** halts every database in a fleet even when persisting the stop fails.
- **PostgreSQL versions**
  - HypoPG validation works on PG14/15.
  - PG18 plans parse fractional `Actual Rows`.
  - pg_hint_plan versions before 1.7 are handled.
  - The documented quick start works as written.
- **Azure:** memory-valued server parameters reported in `kB`/`8kB` convert correctly
  (found by the live matrix).
- **AgentDB:** ping is liveness-only; archived deployments are revisited; re-registration is
  idempotent; teardown requires the recorded resource id and verified cloud tags.

### Verification and limitations

- Tested under cgo:
  - Unit: 7,608 passed, 0 failed.
  - e2e: 76 passed.
  - Integration: race-enabled on PostgreSQL 14–18.
  - Every skip justified in the skip budget.
  - golangci-lint: clean.
- Azure live matrix: flexible server PG14–18 (Burstable and General Purpose) and elastic
  clusters pass every automated check. PG11–13 are refused at startup by design (minimum is
  14). New Cosmos DB for PostgreSQL clusters can no longer be created, so Cosmos support is
  covered by unit tests only.
- Dashboard readiness and approved-action checks on Azure (CHECK-AZ-04/05) are still
  manual.

## v1.5.0 (2026-09-07) -- Neon and Supabase

### Added

- Neon and Supabase provider detection, runtime extension/permission discovery,
  portable maintenance actions, and database-scoped setting recommendations.
- AgentDB project/branch lifecycle runners with ownership, entitlement, expiry,
  asynchronous status, and protected-resource checks; provider-specific profiles,
  dashboard fields, and validated Terraform templates.
- Supabase management log and metrics ingestion, including actual auto_explain
  plans, deduplicated query-store records, RCA input, and CPU/memory evidence.
- Environment secret references for AgentDB fleet connections, preserving TLS
  and session options while validating resource endpoint and database identity.
- Vector Evidence Lab with bounded, read-only HNSW experiments and measured
  recall, latency, plan, and resource-admission evidence.
- Live provider regression harnesses for direct/session connections, synthetic
  maintenance and vector workloads, lifecycle cleanup, and logical backup/restore.

### Fixed

- Keep HypoPG evaluation and cleanup on one physical session, discover extension
  namespaces, and support normalized EXPLAIN workloads without pgx bind errors.
- Prefer actual observed execution plans over recaptured legacy plans; distinguish
  installed extensions from loaded/enabled modules and recognize provider-loaded
  auto_explain when LOAD is restricted.
- Report malformed provider logs without stalling valid records; preserve retryable
  storage errors and unknown/stale host-load evidence.
- Preserve explicit inverse database settings during rollback and keep unsupported
  instance settings advisory instead of issuing ALTER SYSTEM to hosted providers.
- Refresh per-database fleet version, size, and collection timestamps; preserve
  observation/manual mode and unknown capabilities in dashboard views.
- Preserve unverified backup-assurance status and reject Terraform mode errors
  or unconfigured sizing/version/extension claims rather than silently accepting them.
- Serialize AgentDB metadata initialization, preserve superseded indexes for
  reviewed cleanup, and harden backend lifecycle, local launcher, and test fixtures.

### Changed

- Reject Neon pooled and Supabase transaction-pooler endpoints for sidecar sessions;
  use Neon direct or Supabase direct/session connections.
- Refresh dependency updates and embedded dashboard assets. CI verifies frontend,
  Go unit/integration/e2e layers and installs the required pgvector/HypoPG capabilities.

### Verification and limitations

- Live Neon and Supabase direct checks and Supabase session checks passed for the
  covered SQL, HypoPG, and vector workflows. Synthetic logical exports restored
  rows, vector hashes, indexes, constraints, and identity state into fresh databases.
- Neon branch and Supabase free project create/status/delete were verified live;
  temporary API credentials were revoked after testing.
- Free-plan entitlements and unavailable hints remain explicit. In the tested
  projects, Neon hints could not be enabled and Supabase did not offer pg_hint_plan.
  Supabase Free branch/managed-backup and Neon Free log-export limits remain.
- Missing data/WAL utilization blocks actions requiring complete host-load evidence.
  Neon exhausted the test project's free transfer quota before the final dashboard
  repeat; earlier live checks passed. No paid upgrade was performed.
- Native snapshot rehearsal and automatic restore drills remain existing product
  gaps; logical restore verification does not claim those workflows or managed PITR.

See [Neon and Supabase setup](docs/neon-supabase.md) for connection requirements,
configuration, and the verified feature boundaries.

## v1.4.0 (2026-07-23) -- Agent-Native Autonomy

### Added

- Standing-policy control plane (`sage.policy`): a declarative, validated
  policy object is now the sole source of autonomous authority, with
  `staffed` and `unattended` profiles, budget semantics where `0` means
  "none" (never "unlimited"), fail-closed unknown classification, and
  operator-gated propose/dry-run/ratify policy changes.
- Single authorization gate: every autonomous mutation routes through
  `policy.Gate` with fail-closed ordering, enforced guardrails, DDL
  change-leases (advisory locks + `sage.change_lease`), and drift
  reconciliation against `sage.schema_baseline`.
- Evidence Ledger (`sage.decision`) recording every parked, recommended,
  blocked, and executed decision, with a self-audit that re-opens
  decisions when past fixes regress.
- Verify-and-revert engine: per-query verification with no-gain,
  regression, write-impact, and invalid-index reverts; adaptive windows
  that extend then revert-to-safe on insufficient samples; a low-load
  apply gate; and the coupling rule that nothing auto-applies unless it
  can be auto-verified.
- Verified index lifecycle: candidate generation, HypoPG pre-estimates,
  concurrent builds under lease, and post-apply verification with
  automatic revert.
- Wraparound/bloat custodian and replication-slot/WAL guardian with
  deterministic deadline math, graduated urgency, policy-gated deadline
  overrides, slot bounding before dropping, and a consumer registry that
  protects declared slots.
- Rehearse-on-clone migration gate: lint-and-rewrite of hazardous DDL,
  expand/contract planning, rehearsal on thin clones, bounded auto-revert
  windows, and recommend-only degradation when no clone provider exists.
- Pluggable `clone.Provider` with Database Lab Engine and
  snapshot-restore adapters, including snapshot-freshness enforcement.
- Agent-native schema custodian: invariant catalog (FK-without-index,
  unbounded append tables, missing constraints), table contracts declared
  over MCP, dry-run-first retention, and structural changes that never
  auto-apply.
- Intent-level MCP server (stdio JSON-RPC) whose every tool routes
  through the same standing-policy gate; caller claims can never widen
  authority.
- DBA-hours-saved value model: table-driven `sage.toil_model`,
  conservative `sage.incident_avoided` credit, verified-success-only
  stamping with zero credit on revert, the `/api/v1/value` endpoint, and
  Prometheus gauges `pg_sage_toil_minutes_saved_total` and
  `pg_sage_incidents_avoided_total`.
- Value view as the dashboard's default landing page, with telemetry
  moved to a secondary Advanced view.
- Fleet staged-rollout scaffolding for cohort canaries with per-instance
  re-verification.

### Changed

- Replaced testify with a stdlib-only internal assertion library
  (`internal/testsupport`), keeping the repo's no-testify convention.
- DB-dependent tests now skip with an explicit reason when
  `SAGE_TEST_DATABASE_URL` is not set instead of failing against a
  sentinel address; with a database configured nothing skips.
- Notification and config API tests are hardened against full-suite DB
  contention (stable cleanup sweeps and bounded retries on transient
  timeout signatures only).

## v1.3.1 (2026-07-22) -- Correctness and Safety Audit Follow-up

### Fixed

- Restricted LLM model discovery to administrators and blocked SSRF through
  private, loopback, metadata, redirected, or DNS-rebound provider endpoints.
- Scoped LLM cooldowns to logical work items, counted provider timeouts toward
  circuit health, and prevented local admission throttles from poisoning
  optimizer table circuit breakers.
- Enforced atomic per-database token budgets across optimizer, advisor, tuner,
  briefing, RCA, narration, justification, schema lint, and migration clients.
- Restored fleet health-history samples, initialized fleet and meta-database
  ANALYZE concurrency limits, and made AgentDB collectors process-owned and
  drainable instead of inheriting a short reconciliation context.
- Initialized the general LLM runtime in meta-database mode and wired scoped
  LLM clients through every per-database consumer.
- Made executor policy reads race-free during configuration hot reload.
- Corrected cron maintenance windows to honor month, day-of-month, and
  day-of-week semantics before autonomous moderate-risk maintenance.
- Applied transaction-local statement and lock timeouts to collector catalog
  reads, including early-error cleanup of pooled transactions.
- Added safe on-demand EXPLAIN capture for normalized parameterized queries.
- Clarified that interactive ReAct diagnosis belongs to the frozen C extension,
  not the Go sidecar, and made generated lifecycle documentation byte-stable.

## v1.3 (2026-07-19) -- Correctness and Safety Remediation

### Fixed

- Corrected the v1.2 execution-gate coupling: `execution_mode: manual` now
  disables all background queueing and execution at every trust level. Trust
  is an independent ceiling and never promotes manual mode to auto.
- Unified live execution, approval queueing, Cases policy, and fleet readiness
  on typed action contracts. Per-database executor disablement and Emergency
  Stop are hard gates, and live actions reauthorize immediately before SQL.
- Made runtime configuration immutable, generation-based, strict, and
  compare-and-swap protected. Meta mode owns durable control state; YAML fleet
  settings and membership are visible but explicitly read-only in the UI.
- Added prepare/swap/drain ownership for fleet lifecycle changes, reliable
  watcher shutdown, bounded worker teardown, session-pinned advisory locks,
  and race-free executor safety state.
- Enforced fail-closed AgentDB policy ceilings, server-owned execution plans,
  exact-operation authorization, leased monitoring claims, JIT credentials,
  bounded monitoring concurrency, and paginated control APIs.
- Corrected log offset ownership, LLM admission and budget accounting,
  runaway and migration detectors, pooled-session cleanup, and immediate
  reauthorization of every mutating operation.
- Removed retired MCP claims, generated configuration lifecycle documentation,
  reconciled AgentDB documentation, and corrected Docker and browser fixtures.
- Isolated database tests per package, migrated and pinned golangci-lint v2,
  restored parallel-safe CI, and raised every business package above 70%
  coverage and every command utility to at least 50%.

## v1.2 (2026-06-11) -- Autonomous DBA Core + LLM-Native Tier

### Added

- **Actions UI shows attempts + risk.** The executed-actions table now shows an Attempts column (how many times pg_sage tried an action — surfacing retries that were previously only in the log) and a Risk badge (safe/moderate auto-run; advisory is recommend-only).
- **Restart from the UI.** Settings now shows that some settings (trust tiers,
  maintenance window, execution mode, intervals) take effect only after a
  restart, with a **Restart now** button. `POST /api/v1/restart` (admin) exits
  with code 42; the launcher (and any orchestrator restart policy) relaunches
  the process. Returns 501 if no supervisor is configured.
- **Query store (F2):** `sage.query_store` records per-queryid metrics each
  cycle, enabling *windowed* latency (pg_stat_statements only exposes lifetime
  averages). Substrate for verify-and-revert and plan-regression detection.
- **Per-queryid verify-and-revert (F1):** the executor's rollback check now
  compares the targeted queries' before/after windowed latency instead of a
  coarse global cache-hit/avg-write heuristic, so a change is reverted only if
  the queries it targeted actually regressed.
- **Index lifecycle (A2):** optimizer index recommendations now carry their
  analyzed queryids, so F1 verifies a newly-created index and drops it (via its
  `DROP INDEX` rollback) if a targeted query regresses; combined with the
  existing measured-non-use auto-drop.
- **Per-table autovacuum tuning (A1), ANALYZE / stale-statistics autopilot
  (A4), and wraparound-freeze guard (A6)** — deterministic SAFE/critical
  maintenance actions with rollback.
- **Doc-grounded config tuning (A3):** a curated GUC knowledge base grounds the
  advisor's LLM recommendations in documented semantics and gates any
  `ALTER SYSTEM` value outside the documented safe range.
- **Plain-English action justification (C4):** every executed action gets an
  LLM-written audit note in `sage.action_log.justification`.
- **Plan-change narrative (C6):** `plan_regression` findings are enriched with
  an LLM "why did the plan change" explanation.
- **AgentDB fleet integration (B1)**, lifecycle reconciler scheduling (F4), and
  fleet LLM token-budget enforcement (F5).
- `llm.UnwrapText` strips `json_mode` JSON wrapping from prose audit
  notes/narratives.

### Changed

- **Self-monitoring:** pg_sage now sets `pg_stat_statements.track = none` on
  its own connections (best-effort, superuser only), so its monitoring queries
  are never recorded and no longer pollute the user's `pg_stat_statements`. As
  a fallback for restricted roles, every query pg_sage issues also carries a
  `/* pg_sage */` marker and the self-monitoring filter excludes both the
  marker and any `sage.`-schema query from analysis.
- `isThinkingModel` now covers the `gemini-3.x` series (reserves output-token
  budget for thinking models, e.g. `gemini-3.5-flash`).
- **Autonomy gate coupling:** raising the trust level to advisory/autonomous
  now opens the execution gate. A `manual` execution_mode combined with a
  non-observation trust level was a silent contradiction (the "turned on
  autonomous, nothing happened" trap); the executor now treats that as `auto`
  (still gated by trust tier, ramp age, and the maintenance window). Works live
  and across restart, since trust level persists in `sage.config`.
- **Maintenance windows are no longer cron-only.** The window accepts friendly
  forms: presets (`nights`, `weeknights`, `weekends`, `off-hours`,
  `business-hours`, `always`, `never`) and day-qualified ranges
  (`weekdays 01:00-05:00`, `Sat-Sun 02:00-06:00`, `Mon,Wed,Fri 22:00-04:00`),
  alongside the existing `HH:MM-HH:MM` and cron syntax.

### Fixed

- **Config reloads can execute.** `SELECT pg_reload_conf()` is now on the
  executor's SELECT allowlist, so the apply-step after an autonomous
  `ALTER SYSTEM` actually takes effect instead of failing validation.
- **No more orphan reload findings.** When an advisor recommendation bundles
  `ALTER SYSTEM ...; SELECT pg_reload_conf();`, the reload (an apply
  mechanism the executor performs itself) is dropped from the statement
  split rather than becoming its own unexecutable finding.
- **Optimizer index recommendations auto-execute.** LLM index recs
  (missing/composite/covering/GIN/HNSW) are now deterministically classified
  `moderate` — `CREATE INDEX CONCURRENTLY` is online and reversible — so
  they run autonomously under the moderate trust gate instead of being
  stuck advisory behind the LLM's self-rated `high_risk`. Index DROPs keep
  their conservative rating.
- **GIN/HNSW DDL passes validation.** The column-existence check now strips
  operator-class specifiers (`payload jsonb_path_ops`,
  `embedding vector_l2_ops`) instead of rejecting them as nonexistent
  columns.
- **Optimizer indexes no longer self-destruct.** The INCLUDE-upgrade path
  consumed `drop_ddl` — which doubles as the new index's own rollback — and
  dropped the index it had just created. A self-reference guard now skips
  the upgrade drop when it targets the newly created index; rollback remains
  the job of the post-action monitor.
- **Subset-index drops are advisory now.** A leading-prefix "subset" index
  (`(c1)` covered by `(c1, c2)`) is no longer auto-dropped — it's a judgment
  call (read-perf trade-off, and apps that re-create their own indexes turn an
  auto-drop into churn). It's surfaced as info/advisory; exact-duplicate index
  drops remain automatic.
- **Multi-statement config recommendations are split.** An advisor
  recommendation carrying several `ALTER SYSTEM` statements (e.g. WAL tuning) is
  split into one atomic, individually-validated action per parameter, instead
  of being rejected by the executor as multi-statement SQL.
- **Resume now works after a restart.** Resuming reconciled only the in-memory
  state, so after a restart (in-memory "running", persisted flag "stopped")
  resume couldn't clear the persisted `emergency_stop`. It now always
  reconciles the persisted flag.
- **Anti-oscillation guard.** If pg_sage applies the same successful action
  to an object repeatedly (e.g. dropping an index an app keeps re-creating),
  it now backs off after a few cycles and marks the finding acted-on, instead
  of churning drop→recreate→drop forever. The guard reads the action log, so it
  survives restarts (the in-memory cascade cooldown did not). This fixes a
  real runaway where thousands of duplicate-index drops accumulated against an
  app-managed database.
- **Config changes now take effect (and are cloud-aware).** Previously an
  `ALTER SYSTEM` action wrote `postgresql.auto.conf` but never reloaded, so the
  change silently never applied. The executor now: reloads (`pg_reload_conf`)
  after a reload-only GUC on a self-managed server so it takes effect; marks a
  restart-only GUC (`shared_buffers`, `max_connections`, …) as
  `applied_pending_restart`; and on a **managed provider** (RDS/Aurora,
  Cloud SQL/AlloyDB, Azure) skips `ALTER SYSTEM` entirely (it's blocked there)
  and records that the change must be applied via the provider's parameter
  group / database flags. The action log outcome now reflects whether a config
  change is actually in effect.
- **Retention:** `cleanStaleFirstSeen` no longer skips stale-key cleanup when
  the index snapshot is empty (a regression from an over-broad safety guard).
- **Cases:** pg_sage no longer surfaces its own monitoring queries as Cases.
  The case projection now skips self-monitoring findings, and detection
  recognizes pg_sage's statistics-catalog reads (`pg_stat_statements`,
  `pg_stat_user_tables`, etc.) even in historical findings captured before
  queries were tagged.
- **Recommendation quality:**
  - The connection advisor now sizes `max_connections` against *total* (open)
    backends, not just active ones, with a hard deterministic floor — it can
    no longer recommend a value below current usage (previously it could
    suggest `max_connections = 20` on a database with 28 open connections).
  - Index rules (unused / duplicate / subset / missing-FK) now skip system and
    extension-internal schemas (`pg_*`, `information_schema`, `_timescaledb_*`),
    so pg_sage no longer recommends dropping or creating indexes it has no
    business touching (e.g. `_timescaledb_catalog` indexes).
  - Subset-index detection now also requires the superset to serve the subset's
    `INCLUDE` columns, so an index supporting an index-only scan isn't flagged
    as redundant.
  - Subset-index detection now only recommends dropping a narrow index when the
    superset is a *close* replacement — at most ~2 extra key columns, not more
    than ~3x the on-disk size, and not a heavily-used narrow index — so a `(c1)`
    index is no longer replaced by a wide `(c1..c12)` one.

## v1.1 (2026-05-10) -- AgentDB Cloud Provisioning Release

### Added

- AgentDB now supports gated live cloud provisioning paths for AWS RDS, GCP
  Cloud SQL, and Databricks Lakebase branch workflows alongside local schema
  and database provisioning.
- Added LLM-required blueprint generation that turns English deployment intent
  into typed AgentDB blueprints and draft Terraform templates for review.
- Added Terraform upload/import review flow with static policy checks for
  provider-specific shapes that change frequently across cloud vendors.
- Added provider settings, custom size profiles, cloud field tooltips, and
  compact AgentDB tabs for deployments, provisioning, profiles, provider
  settings, Terraform, blueprints, and activity.
- Added backup assurance, restore-verification gates, ping/token lifecycle,
  TTL cleanup, cost samples, query tuning recommendation contracts, audit
  export, and deploy-request promotion records for agent-owned databases.
- Added a dedicated
  [AgentDB Cloud Provider Setup](docs/runbooks/agentdb-cloud-provider-setup.md)
  guide for AWS, GCP, Databricks, live safety gates, validation commands, and
  cleanup evidence.

### Changed

- AgentDB provider documentation now reflects dry-run and gated live execution
  instead of describing cloud providers as plan-only.
- The AgentDB UI links directly to cloud setup guidance from provisioning,
  provider settings, Terraform, and blueprint workflows.
- Cloud provisioning requires explicit live gates, provider policy allowlists,
  cost/TTL guardrails, and restore-verified backup evidence before destructive
  live cleanup.

### Fixed

- Fixed bodyless AgentDB action requests that returned invalid request errors.
- Fixed UI delete/lifecycle actions so archived deployments can run guarded
  destroy flows when backup policy is satisfied.
- Fixed cloud blueprint provisioning metadata so requested regions and provider
  shape fields are preserved instead of silently falling back to defaults.
- Fixed test login targeting for the `8085` sidecar and rebuilt the embedded UI.

### Verification

- `go test -cover -count=1 -p 1 ./...` passed from `sidecar`.
- `npm test -- --run`, `npm run lint`, and `npm run build` passed from
  `sidecar/web`.
- `npx playwright test --workers=1` passed against `http://127.0.0.1:8085`;
  provider-gated suites were intentionally skipped unless live flags were set.

## v1 (2026-04-28) — Autonomous DBA Release

> Published on GitHub `master` as tag `v1`. This release includes the
> autonomous DBA feature slices, the prior verification bug pass tracked in
> `docs/codex-bug-log.md`, the final provider-readiness contract updates, and
> the admin/legacy workflow verification pass.

### Functionality Change

This release moves pg_sage further away from a passive observability dashboard and closer to an autonomous DBA workflow. Findings, incidents, migration risks, proposed actions, approval state, and execution history now behave more like a single operational case queue that a DBA can triage end to end. The goal is for pg_sage to preserve the full story around a database problem: what was detected, why it matters, what the system recommends, what action was proposed, whether a human approved or rejected it, and what happened after execution.

The action lifecycle is now more deterministic and safer under real operating conditions. Proposed actions are filtered when their source finding has already been resolved, successful executions close the loop back to the finding, rejected actions observe cooldowns, and stale or duplicate create-index work is suppressed before it can create noise or risk. This matters because autonomous database work cannot rely on optimistic UI state alone; every action needs fresh evidence, an auditable state transition, and guardrails that prevent the system from repeatedly proposing or executing work that has already been handled.

Fleet behavior was tightened across settings, detail lookups, action execution, incident resolution, and managed database editing. In multi-database mode, pg_sage now requires explicit database targeting for mutations and ambiguous detail reads, refreshes runtime state after managed database changes, and keeps selected-database configuration separate from global configuration. These changes are intentionally conservative: once a product can operate across a fleet, wrong-database reads or writes become one of the highest-risk failure modes, so v1 prioritizes precise scoping over convenience.

The release also adds more trust-building surface for teams evaluating autonomy. Provider readiness, shadow-mode reporting, durable LLM cooldown tracking, migration safety cases, and incident playbook actions give operators a clearer view of what pg_sage can do, what is blocking it, and what it would have done before automation is enabled. The reasoning behind these additions is adoption-oriented: DB teams are more likely to trust autonomous execution when the product can first prove avoided toil, expose its constraints, and show a reliable decision trail.

### Added

- Cases now consolidate schema-health, forecast, incident, and query-hint work into one DBA case surface. Legacy `#/schema-health`, `#/forecasts`, `#/incidents`, and `#/query-hints` routes now open Cases with the relevant source filter selected, while the Cases API projects active/broken query hints as case evidence alongside existing findings and incidents.
- Migration-safety cases now include a deterministic DDL preflight report and PR/CI-ready script output. Case and action detail surfaces show lock/rewrite risk, live-risk checks when available, generated migration SQL, rollback or forward-fix guidance, verification SQL, and PR metadata without directly executing high-risk DDL.
- Incident playbook automation now covers runaway queries, connection exhaustion, WAL/replication risk, and sequence exhaustion in addition to lock blockers and idle-in-transaction incidents. Low-risk playbooks emit read-only diagnostics, PID actions still require exact PID evidence and approval, and sequence capacity work is generated as a forward-fix migration script instead of direct execution.
- Vacuum, bloat, and freeze cases now project explicit autopilot candidates. Table-bloat findings can propose guarded `VACUUM`, IO-saturated bloat cases are blocked to script/review output, XID wraparound findings get freeze-blocker diagnostics, and per-table autovacuum tuning produces PR/CI-ready reloption scripts with verification SQL.
- Query tuning now extends beyond `pg_hint_plan` hints. Suggested rewrites become PR-ready query rewrite artifacts with semantic and plan verification, broken hints can be retired through a safe metadata action, and repeated per-role `work_mem` hints can be promoted to reviewed role-level configuration changes.
- Provider readiness now uses provider-specific capability adapters for self-managed Postgres, Cloud SQL, AlloyDB, RDS, and Aurora. The readiness matrix exposes extension enablement paths, log-access expectations, provider limitations, and the expanded action family support instead of treating every Postgres endpoint as operationally identical.
- DDL safety preflight now records live-risk checks for table size, active workload, pending locks, replica lag, and lock-timeout configuration. High-warning live preflight output blocks direct execution and keeps the action in reviewed PR/script mode.
- Incident and maintenance coverage now adds autovacuum-falling-behind, standby-conflict, blocked-vacuum, concurrent reindex, bloat-remediation planning, `CREATE STATISTICS`, and parameterized-query action families, each with typed contracts and verification plans.
- Provider readiness now exposes the expanded action-family detail in the UI, including `ddl_preflight` and standby-conflict diagnostics, so operators can see exactly which autonomous capabilities are blocked, observable, script-only, or executable for each provider.
- Admin and legacy workflows were rechecked for v1: Users, Notifications, Fleet add/edit/import surfaces, Database and Alerts empty states, command palette navigation, not-found handling, and legacy `#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and `#/incidents` routes.

### Fixed

- Fleet incident, finding, and action detail endpoints could scan all pools by ID and return the first matching row, so duplicate IDs across databases could surface the wrong record. Detail endpoints now require an explicit `?database=` in multi-pool fleet mode, while still allowing implicit lookup when exactly one database is registered.
- Successful recommendation actions left source findings open, so the UI could continue to offer the same action. Successful action execution now marks linked findings resolved and stores the action log id.
- Rejected recommendations could immediately reappear as pending approvals. Approval mode now suppresses recently rejected finding/SQL pairs during the cooldown window.
- Pending approval counts did not update live. SSE now emits action events from both `sage.action_queue` and `sage.action_log`, and the layout subscribes to refresh pending counts.
- Recommendations could still show manual action controls while a matching approval was pending. The UI now hides duplicate manual action controls when pending approvals exist.
- Emergency Stop cancelled monitoring goroutines, and Resume could not restart them. Emergency Stop now gates actions without cancelling monitoring, so Resume restores operation.
- Several bodyless UI `POST` calls were rejected with HTTP 415 because they omitted `Content-Type: application/json`. Fixed emergency stop/resume, notification test-send, DB test, incident resolve, and token budget reset call sites.
- Managed database edits updated `sage.databases` but did not refresh the active fleet runtime instance. Updates now remove the old runtime name and register the updated record.
- Managed database updates against missing ids could report an internal readback failure instead of not found. Store updates now check affected rows and the API maps missing records to 404.
- Notification channel toggles could persist masked secrets returned by the list API, corrupting Slack webhooks, PagerDuty routing keys, or email passwords. Store updates now preserve existing real secrets when masked placeholders are replayed, and API masking covers `smtp_pass`.
- Notification channel/rule update and delete calls reported success for missing ids, and rule creation relied on database FK errors for missing channels. Notification stores now return not-found/validation errors deterministically and handlers map them to 404/400.
- Notification channel updates treated omitted `enabled`, `name`, or `config` fields as false/empty values, so partial clients could accidentally disable or invalidate channels. The update handler now preserves existing omitted fields.
- Admin users could demote themselves or demote the last admin, potentially leaving the system with no administrator. The API now forbids self admin-role changes and last-admin demotion; the Users UI disables those controls.
- Edit Database `Test Connection` tested the saved connection instead of unsaved form edits. The edit form now submits current fields, and the backend merges the stored password when the password field is left blank.
- Edit Database `Test Connection` with unsaved preview fields bypassed the SSRF host guard used by Add Database testing. Preview edit tests now resolve and block unsafe hosts through the same safety path.
- Editing a managed database could send `max_connections` as a string from the UI, failing JSON decode into the Go integer field. The form now coerces `max_connections` numerically like `port`.
- The edit-form SSRF guard initially blocked testing an already-managed local/private database host, breaking legitimate local fixture edits. Edit tests now allow the stored managed host while still blocking unsafe host changes.
- Settings always saved global config, even when a specific database was selected. Fleet status now exposes database ids, Settings maps the selected database name to id, and selected-database edits use `/api/v1/config/databases/{id}`.
- Global Settings exposed `execution_mode` as an editable field even though global saves discarded it. The global UI now marks execution mode as database-only, while selected-database Settings can edit and persist it.
- `GET /api/v1/config/databases/{id}` returned config for nonexistent ids and defaulted null `execution_mode` to `manual`. It now returns 404 for missing databases and uses the managed DB default `approval`.
- Saved per-database config overrides could not be reset from Settings. A per-database override delete endpoint now removes stored DB overrides, and the Settings reset button calls it for DB overrides.
- Per-database `trust.level` reset removed the stored override but did not update the running fleet executor/status. Reset now synchronizes the effective inherited trust level back into runtime state.
- Multi-key config saves could partially persist earlier keys before later validation failures returned 400. Config batch saves now prevalidate all keys before writing any overrides.
- LLM model discovery could write a masked `llm.api_key` back into global config and ignored selected database scope. Discovery now uses a non-persistent model-discovery request and skips masked keys.
- `/api/v1/explain` accepted viewer-role requests even though it can run database queries. The route now requires operator-or-admin access and has API coverage for valid and invalid explain requests.
- Parameterized explain requests interpolated caller-supplied parameter strings directly into `EXPLAIN EXECUTE`. Parameters are now emitted as escaped SQL literals, with NUL bytes rejected as invalid input.
- Incident resolution from the UI sent an empty JSON request despite the API requiring a JSON body, so resolving incidents could fail with HTTP 400. The UI now sends `{ "reason": "" }`.
- Incident confidence never rendered because the API returns `confidence` but the UI read `confidence_score`. The incident detail drawer now renders the API field.
- Fleet-wide Query Hints, Incidents, and Action History read only the primary database for all-database views, hiding secondary database rows. These handlers now merge all registered pools, include `database_name`, sort deterministically, and apply global limits where needed.
- Action details searched only the primary database. Detail lookup now searches all pools so secondary-database action history rows are reachable.
- Manual action execution accepted any valid SQL for any numeric finding id, including missing, resolved, or mismatched findings. Execution now verifies the finding is open/unresolved and the SQL matches the recommendation before running DDL.
- Pending action lists and counts could expose queue rows for findings already resolved by another action, leading users to approve stale actions that could not safely run. Pending action queries and approval now require the linked finding to still be open and actionable.
- Fleet suppress/unsuppress without `?database=` could mutate the first matching finding id by map-order scan across databases. Mutations now require a database when more than one instance exists and only allow implicit targeting for a single connected instance.
- Emergency stop/resume returned 200 with zero changes for unknown database names and ignored persistence errors from writing the stop flag. Handlers now return 404 for unknown databases and surface persistence failures instead of reporting success.
- Fleet Health chart used raw database aliases as Recharts `dataKey` values. Aliases containing dots could be interpreted as object paths and fail to render; the chart now uses safe internal series keys and display names separately.
- Schema Health applied status/severity/category filters to the table but not to the summary stats, so the summary could contradict the visible rows. Stats now use the same filters, and the summary label now describes matching findings rather than always saying open findings.
- Query Hints treated missing `after_cost` as zero due JavaScript null coercion, creating fake 100% cost improvements. Cost improvement and average improvement now require both before and after costs.
- Query Hints lifecycle states existed (`retired`, `broken`) but `/api/v1/query-hints` only returned active rows, making the UI Status column misleading and hiding revalidation outcomes. The endpoint now returns recent hints across statuses by default and supports `status=active|retired|broken` filtering.
- Schema Health could not filter the `safety` thematic category even though the backend schema linter emits it. The category dropdown now includes safety, and live coverage verifies expanded rows show impact, recommendation, and suggested SQL.
- Fleet Health chart series keys were safe for dotted aliases but still depended on unsorted response object order, so series key-to-database mapping could shift between refreshes. Series are now sorted deterministically, and live coverage verifies chart rendering from seeded `health_history` rows across all three local databases.
- Incidents accepted `low`, `medium`, and `high` risk values plus newer source values, but the UI only labeled the older `safe`, `moderate`, and `high_risk` variants. The detail drawer now labels all valid risk/source values.
- Viewer users were shown the Pending Approval tab and the Actions page fetched operator-only pending approvals, causing forbidden requests for read-only users. Pending-review UI and polling now render only for admin/operator roles, with live viewer/operator boundary coverage.
- Notification channel UI did not expose PagerDuty even though the backend supports it, and selecting unsupported types would fall through to the email form shape. The Channels form now exposes PagerDuty with a routing-key field and stable browser-test selectors for all channel types.
- Notification masked-secret preservation now has browser-level coverage: creating a Slack channel through the UI, receiving a masked webhook from the API, toggling the channel from the UI, and verifying the database still stores the original webhook rather than the masked placeholder.
- Existing managed database connection tests in YAML fleet mode tried to decrypt the `sage.databases.password_enc` placeholder even though runtime credentials live in the fleet instance config. The endpoint now uses fleet runtime credentials for fleet-registered rows and only reports 404 for actual missing database records.
- Managed Database edit-form connection testing now has browser-level coverage for unsaved field changes and empty password fallback, including a failing unsaved database name followed by a successful retry without saving.
- Managed Database API/UI edits did not round-trip `max_connections`; saving from the UI could overwrite a configured fleet limit with `0`. The API now returns and accepts `max_connections`, defaults absent values safely, and the UI includes the field.
- In local YAML fleet mode, saving a managed database edit could close the primary fleet pool, which is also captured for auth and catalog storage, causing subsequent logins/API calls to hang or fail. No-meta fleet edits now refresh runtime metadata in place without closing the primary pool, with browser coverage for saved edits updating runtime trust state.
- Background/live refetches set `loading=true` even when data was already rendered, unmounting tables and collapsing/detaching expanded finding action panels while a user was approving or rejecting queued work. `useAPI` now keeps existing data mounted during background refetches and only shows loading on initial load or URL changes.
- Local no-meta fleet mode exposed Add Database even though new database credentials cannot be encrypted without an `encryption_key`, causing a 500. The store now returns a validation error, and browser coverage verifies the UI surfaces the `encryption_key` requirement.
- Global Settings reset could not remove global overrides correctly because hot reload mutated the live config and the API no longer had an immutable YAML/default baseline to restore from. Config routes now keep a detached baseline snapshot, expose `DELETE /api/v1/config/global/{key}`, reload live config from baseline plus remaining overrides, and the Settings UI can reset global overrides.
- The full-surface fixture did not include a deterministic invalid-index case and only printed finding summaries. The fixture now creates a failed concurrent unique index, asserts expected analyzer categories, schema-lint rule IDs, and active query hints across all three local databases, and polls until the expected surface appears.
- The legacy serial walkthrough could not run against local Docker fixtures without hardcoded database credentials and failed strict-mode locators when multiple finding detail/suppress controls were visible. The walkthrough now supports fixture DB credential env vars, has an encrypted meta-db config for add/edit/delete flows, and scopes duplicate UI controls to the first visible target.
- The encrypted walkthrough fixture was still a manual setup sequence. `run_walkthrough_fixture.ps1` now provisions isolated meta/target databases, seeds the fixture schema, starts the encrypted sidecar, resets the admin password, runs the 124-check walkthrough, and can restore the standard full-surface sidecar afterward.
- The long walkthrough used Playwright serial mode, so one failed checkpoint skipped every later check and hid independent endpoint/UI regressions. The global serial mode is now removed; the wrapper still runs the file with one worker to preserve fixture ordering, but later checks continue after failures.
- Successful create-index approvals could be followed by a stale analyzer cycle reopening the same missing-index finding as a new open row. Finding upserts now suppress immediate reopen of recently action-resolved findings while still allowing genuinely unresolved issues to reappear after the grace window.
- Stale or parallel create-index approvals could execute after an equivalent index already existed, creating duplicate indexes with auto-generated names. Manual/approved create-index execution now checks for an existing valid covering index before running DDL and records the action as successful without creating another index.
- Action lifecycle verification only checked row presence in browser tests. The full-surface harness now verifies pending rows across all three local databases, approve execution, rejected queue removal, executed action logs, resolved finding links, and no post-refresh missing/duplicate-index noise.
- Failed `CREATE INDEX CONCURRENTLY` attempts could leave invalid autogenerated index stubs that blocked the next approval of the same recommendation. Create-index execution now drops invalid covering index blockers before retrying the DDL.
- Transient lock timeouts during approved `CREATE INDEX CONCURRENTLY` actions could fail the UI workflow and leave an invalid index stub. Create-index execution now retries lock-timeout failures and cleans invalid blockers between attempts.
- Mutating action browser specs selected the first available pending create/drop action, so parallel runs could contend for the same database/object depending on queue order. The specs now use disjoint deterministic database/action targets.
- The full-surface high-total-time fixture used a PL/pgSQL loop that pg_stat_statements counted as one call, making the `high_total_time` category intermittent. The workload now uses psql `\gexec` so the high-frequency query is recorded as thousands of real calls.
- Incident resolve mutations in a multi-database fleet could resolve the first matching incident UUID by pool order when the UI did not send a database target. Resolve now requires an explicit database when multiple pools are connected, the UI sends the row database, and duplicate incident UUID coverage verifies only the selected database is mutated.
- Incident listing for a selected fleet alias could drop rows whose stored `database_name` was the physical PostgreSQL database name, and the UI could then resolve against that physical name instead of the fleet alias. Incident list responses now include `fleet_database_name` for mutations, and selected-pool listing no longer filters rows by alias.
- Last-admin demote/delete checks were split from the mutation, so concurrent requests could both pass a stale admin count and leave the system without an admin. User role changes and deletes now run in a locked transaction with DB-backed concurrency coverage.

## v0.8.5 (2026-04-12) — Hint Lifecycle, Stale-Stats ANALYZE, Security Hardening, Full Test Sweep

> 91 files changed across the sidecar. 17,016 lines added. 3,480 Go tests + 44 Playwright e2e tests across 6 spec files. Zero skips, zero failures. This is the most thoroughly tested release pg_sage has shipped.

---

### New Features

#### F1 — Hint Revalidation Loop
Hints are no longer fire-and-forget. A new background cycle (`tuner.verify_after_apply`, default off for backward compatibility) continuously validates every active `pg_hint_plan` hint against the live query planner:

- **Staleness check** — retire hints whose query has fallen below `min_query_calls` or been pruned from `pg_stat_statements`.
- **TTL check** — retire hints older than `hint_retirement_days` (default 14) unconditionally.
- **Cost regression check** — cost-compare hinted vs unhinted plan. Keep if `hinted_cost ≤ 1.2× unhinted_cost`; roll back and mark broken when `hinted_cost > unhinted_cost / 0.8`.
- **Redundancy check** — detect directives that no longer affect the generated plan (e.g., an `IndexScan` hint on a table whose only access path is already an index scan).

New directive parser (`hint_parse.go`) supports `Set(work_mem)`, `IndexScan`, `BitmapScan`, `NestLoop`, `HashJoin`, `MergeJoin`, and `Parallel`. Revalidation EXPLAIN carries its own `statement_timeout` so it cannot starve the collector.

#### F2 — Stale-Stats Detection + Autonomous ANALYZE
pg_sage now detects tables with stale statistics by correlating three signals: row-estimate skew (actual vs planned rows diverging by 10×+), modification ratio since last ANALYZE exceeding 10%, and last ANALYZE age over 60 minutes. When all three converge, the executor issues `ANALYZE <schema>.<table>` with full safety controls:

- Per-table cooldown (60 min) that respects autovacuum's own `last_analyze` timestamp
- Size ceiling (10 GB) — larger tables emit advisory findings instead of executing
- Maintenance-window gating (1 GB+) — large tables only ANALYZE during configured windows
- Statement timeout (10 min) with automatic cooldown extension on timeout
- Fleet-wide concurrency cap (1 concurrent ANALYZE across all databases)

#### F3 — Role-Level work_mem Promotion
When multiple queries owned by the same database role accumulate `Set(work_mem)` hints (default threshold: 5), pg_sage detects the pattern and recommends `ALTER ROLE ... SET work_mem` instead — one role-level setting replacing N per-query hints. Excludes `NOLOGIN`, `SUPERUSER`, and reserved roles. Scoped by `pg_stat_statements.dbid` to prevent cross-database pollution in shared-cluster deployments.

#### F4 — Extension Drift Detector (Enhanced)
Tightened drift detection for `pg_stat_statements`, `pg_hint_plan`, `hypopg`, and `auto_explain`. Missing critical columns (`plan_time`, `wal_records`) now produce explicit remediation hints with version-specific upgrade instructions, replacing the previous generic warnings.

#### F5 — Config Tooltip Infrastructure
Every Tier 1 and most Tier 2 config fields now carry `doc`, `warning`, `mode`, `docs_url`, and `secret` struct tags. A new build tool (`cmd/gen_config_meta`) reflects over `config.DefaultConfig()` to emit a 167-field JSON metadata file consumed by the React frontend. The `ConfigTooltip` component (WCAG-compliant, portal-mounted, keyboard-accessible) surfaces this metadata as hover/focus tooltips on every config field in the Settings page — including doc text, warning callouts, mode badges (fleet-only / standalone-only), and secret indicators. A reflection-based drift test fails the build if metadata falls out of sync with the Go struct tags.

#### F6 — Hint-Index Coordination
New deferral logic prevents the tuner from installing `pg_hint_plan` hints while the optimizer has pending index recommendations for the same query. When an index recommendation is in flight, the tuner defers hint creation until the index is either applied or dismissed — preventing the scenario where a hint masks the benefit of a better index. Plan scanning (`planscan.go`) detects whether a pending index would affect the query's execution path before deciding to defer.

#### F7 — Token Budget Dashboard
New `TokenBudgetBanner` React component on the Dashboard and Settings pages shows real-time LLM token consumption against the configured daily budget. Visual indicator transitions from green → amber → red as usage approaches the cap. Reads from the existing `/api/v1/llm/token-usage` endpoint with no additional backend changes.

---

### Fleet & Wiring Fixes

- **Fleet database wiring** — `FleetMgr.PoolForDatabase("all")` no longer returns a nil pool when fleet mode has a single database configured. The `wireRouter` extraction refactor (`wire.go`) splits 5 concerns (standalone wiring, fleet wiring, router setup, middleware chain, graceful shutdown) into testable functions with dedicated mode-specific wiring tests.

---

### Security Hardening

Six targeted fixes addressing edge cases surfaced during adversarial review:

1. **PagerDuty retry amplification** — exponential backoff with jitter replaces unbounded retries on transient 5xx responses. Max 3 attempts, circuit breaker after consecutive failures.
2. **Rate limiter connection leak** — login rate limiter now properly releases semaphore on early returns, preventing goroutine accumulation under sustained brute-force.
3. **Silent zero on invalid port** — `strconv.Atoi` failures on user-supplied port strings now return explicit validation errors instead of silently defaulting to port 0.
4. **CORS origin clarification** — `config_apply.go` validates CORS origin against an allowlist; previously accepted any origin header when the allowlist was empty.
5. **SSRF fail-closed** — `test-connection` endpoint rejects private/loopback IP ranges before attempting the connection, preventing internal network scanning via the API.
6. **Multipart size limit** — file upload endpoints enforce a 10 MB ceiling to prevent memory exhaustion from oversized payloads.

---

### Test Sweep (P0–P5)

The most comprehensive test expansion in the project's history. Every priority tier addressed:

| Priority | Focus | Tests Added |
|----------|-------|-------------|
| P0 | Config consistency — verify every YAML key round-trips through `Load()` → `Save()` without loss or mutation | 47 tests |
| P1 | Wire extraction — `wireRouter` decomposition with mode-specific wiring tests (standalone, fleet, unknown) | 19 tests |
| P2 | LLM degradation — malformed JSON, markdown-wrapped responses, empty bodies, timeout, rate limiting across advisor/optimizer/tuner | 43 tests |
| P3 | Config defaults — every field with a non-zero default verified against `DefaultConfig()` | 131 assertions |
| P4 | API contract — endpoint existence, method enforcement, auth requirements, response shapes | 208 tests |
| P5 | Playwright e2e — Dashboard, Databases, Settings, Navigation, Token Budget, Tooltips | 44 tests across 6 specs |

Total test count: **3,480 Go test functions + 44 Playwright e2e tests**. All packages above 70% coverage threshold. Zero test modifications to force passage — every failure fixed in implementation code.

---

### Documentation

- **End-to-end walkthrough verification** — every doc page cross-referenced against the actual codebase. Six inaccuracies fixed:
  - `--database-url` → `--pg-url` (deployment.md, 4 occurrences)
  - `SAGE_GEMINI_API_KEY` → `SAGE_LLM_API_KEY` (deployment.md, 3 occurrences)
  - LLM endpoint suffix `/chat/completions` removed (SDK appends it)
  - `--meta-db` description corrected from "SQLite path" to "PostgreSQL URL for fleet mode"
  - `llm.token_budget` → `llm.token_budget_daily` with correct 500,000 default (security.md)
- **MCP reference removal** — MCP (Model Context Protocol) was documented across 7 files but never implemented in the Go sidecar. Removed all references: `sage.mcp_log` table from CI schema and sql-reference.md, MCP step from try-it-out.md, port 5433 from firewall notes, `mcp:` stanza from demo config, nav entry from mkdocs.yml. Zero functional code affected — this was purely phantom documentation.
- `config.example.yaml` updated with every new tuner and analyzer field.

---

### Stats

| Metric | Value |
|--------|-------|
| Commits since v0.8.4 | 5 |
| Files changed (sidecar) | 91 |
| Lines added | 17,016 |
| Lines removed | 2,295 |
| Go test functions | 3,480 |
| Playwright e2e tests | 44 (6 spec files) |
| Go packages tested | 26 |
| Test failures | 0 |
| Test skips | 0 |

## v0.8.4 (2026-04-07) — Security Hardening + Tuner Pipeline

### Security
- **RBAC**: Added `RequireRole` to legacy API routes (config, emergency-stop, resume, suppress/unsuppress, pending count). Viewers can no longer escalate trust to autonomous or halt the fleet.
- **SQL injection**: Block EXPLAIN injection via multi-statement rejection + `statement_timeout` + read-only transactions. New `sanitize.QuoteIdentifier` package for all catalog-derived identifiers. Fixed VACUUM, ALTER DATABASE, and tuner SQL construction.
- **Executor allowlists**: Whitelisted 30 safe `ALTER SYSTEM` GUCs. Restricted SELECT to `pg_terminate_backend`/`pg_cancel_backend`. Restricted `ALTER TABLE` to `SET`/`RESET`/`TABLESPACE`. Validate trust level strings. Cap concurrent DDL at 3. Auto-drop invalid indexes after failed `CREATE INDEX CONCURRENTLY`. Derive advisor `ActionRisk` from SQL type instead of hardcoding `safe`.
- **Auth**: Cookie `Secure` flag enabled. Password minimum 8 characters. Login rate limiting (5 attempts / 15 min per email). Self-deletion + last-admin protection.
- **API hardening**: Security headers middleware. Content-Type validation. SSRF protection on `test-connection`. Error messages sanitized (no more `err.Error()` leaked to clients). `X-Forwarded-For` only trusted from loopback. Notification channel secrets masked.
- **Secrets**: Removed hardcoded Gemini API key from `docker-compose.test.yml`. Admin password printed to stderr only. `create_admin` uses environment variables. `DeriveKey` upgraded to argon2id with v1 backward compatibility. Demo config uses env var.

### Tuner Pipeline
- `BuildInsertSQL`/`BuildDeleteSQL` now use `norm_query_string` column (was `query_id`, which doesn't exist in `hint_plan.hints`).
- Executor allowlist updated for `INSERT`/`DELETE INTO hint_plan.hints`.

### Fleet Hardening
- Schema bootstrap advisory lock changed from non-blocking `pg_try_advisory_lock` to blocking `pg_advisory_lock` with 30s timeout.
- `CREATE SCHEMA` now uses `IF NOT EXISTS`. All `DROP SCHEMA` operations in tests wrapped in advisory lock.
- `autoexplain.ConfigureSession` tolerates permission denied (SQLSTATE 42501) on managed DBs where role-level defaults suffice. Coverage boost to 83%.

### API
- New LLM token usage endpoint.
- Config apply/audit handlers.
- Settings page UI improvements.
- New `create_admin` CLI command.

### Test Infrastructure
- `auth`, `notify`, and `store` tests now hold `pg_sage` advisory lock for test duration to prevent schema drops mid-test.
- Store config tests re-insert FK user before each test.
- All 22 packages pass with `-p 4` (zero failures, zero skips).

## v0.8.3 (2026-04-04) — Cloud E2E + LLM Token Optimization
- Cloud E2E validation across 8 managed PostgreSQL databases:
  RDS PG14/18, Aurora PG14/17, Cloud SQL PG14/18, AlloyDB PG14/17
- Auto-detect cloud environment (rds, aurora, cloud-sql, alloydb)
- ALTER SYSTEM → ALTER DATABASE rewriting for managed platforms
- Executor max-retry limit (3 failures → mark as acted_on)
- LLM deduplication: skip redundant calls when open findings/hints exist
  (optimizer, tuner, advisor all check sage.findings/sage.query_hints first)
- 11 token waste fixes: bloat category mismatch, vacuum validation bug,
  thinking-model budget, CapturePlans loop hoist, column stats filtering,
  per-cycle table cap, per-query rewrite dedup, briefing LIMIT,
  retry scope (429/503 only), tuner stats cap, single-symptom deterministic skip
- Thinking model support: +16384 token overhead for Gemini 2.5 reasoning
- Cross-platform findings: 1615 total, 373 open, 802 acted on across 8 DBs
- All packages above 70% test coverage

## v0.8.2 (2026-04-03) — LLM Tuner + Query Rewrites
- Query tuner: hybrid deterministic rules (7 symptom kinds) + LLM-enhanced hints
- LLM-powered query rewrite suggestions alongside pg_hint_plan directives
- Rewrite suggestions surfaced in dashboard with rationale
- Alert notification (`query_rewrite_suggested` event) when rewrite is suggested
- Index optimizer multi-query consolidation (8 queries → minimal index set)
- E2E test suite: 54 subtests against real Gemini API
- 771+ tests

## v0.8.1 (2026-03-27) — Patch
- Add `google_ml` to all schema exclusion lists (Cloud SQL compatibility)
- Bump default LLM max_tokens to 8192 (Gemini 2.5 Flash thinking token fix)
- Add retry loop to index validity post-check (catalog propagation delay)
- Prevent re-execution of already-acted findings (re-drop race fix)
- Executor cooldown for recently created indexes
- Verified on Cloud SQL PG16/17 and AlloyDB PG17
- 588 tests, 0 failures

## v0.8.0 (2026-03-26) — Fleet Mode + Dashboard
- Fleet manager: single sidecar → N databases via `mode: fleet` config
- `DatabaseManager` with per-database collector/analyzer/executor goroutines
- Per-database advisory locks, trust levels, executor toggles
- Per-database LLM token budget (equal, proportional, or priority-weighted)
- Database-aware data model (every finding, action, metric carries `database_name`)
- Prometheus labels: `{database="prod-orders"}`
- Graceful per-database failure (one DB down doesn't crash others)
- REST API: 14 endpoints on `:8080` alongside MCP
- Fleet overview: `GET /api/v1/databases` with health scores
- Findings, actions, snapshots, config — all filterable by `?database=`
- Config hot-reload via `PUT /api/v1/config`
- Emergency stop/resume per-database and fleet-wide
- Web dashboard (React SPA embedded in binary via `//go:embed`)
- Demo environment: Docker Compose with 7 pre-planted problems, 46 verification checks
- 584+ tests, 0 failures, CI green (6 workflows)

**Integration bug fixes shipped in v0.8.0:**
- VACUUM routed through non-transaction connection (pgxpool wraps in tx by default)
- Trust ramp `ramp_start` config honored on first boot (was always `now()`)
- Unused index window default changed to 7 days (was 0, caused index churn)
- Advisor strips markdown fences from Gemini JSON responses
- `database_name` resolved to actual instance name (was showing "all")

## v0.7.0 (2026-03-26)

### Go Sidecar — The Product

pg_sage is now a Go sidecar binary that connects to any PostgreSQL 14-17 database.
The C extension is frozen at v0.6.0-rc3 (security fixes only).

#### Features
- **Standalone mode** — single binary, no extension install required
- **Index Optimizer v2** — LLM-powered index recommendations with HypoPG validation, confidence scoring, per-table circuit breakers, 8 validators
- **Vacuum Tuning** — per-table autovacuum analysis via LLM
- **WAL/Checkpoint Tuning** — max_wal_size, wal_compression, checkpoint analysis
- **Connection Pool Analysis** — max_connections, idle timeout, pooler detection
- **Memory Tuning** — shared_buffers, work_mem, cache hit ratio, spill detection
- **Query Rewrite Suggestions** — N+1, correlated subquery, OFFSET pagination detection
- **Bloat Remediation Planning** — VACUUM FULL vs pg_repack vs do nothing
- **MCP Server** — Claude Desktop and AI agent interface
- **Prometheus Metrics** — full observability endpoint
- **Dual-Model LLM** — separate models for general tasks vs index optimization
- **Trust-Ramped Executor** — observation -> advisory -> autonomous with rollback

#### Verified Platforms
- Google Cloud SQL (PG14, PG15, PG16, PG17)
- Google AlloyDB (PG17)
- Self-managed PostgreSQL (PG14-17)
- Amazon Aurora — test plan ready
- Amazon RDS — test plan ready

#### Testing
- 530 tests across 14 packages, 0 failures
- Live integration testing on Cloud SQL PG16, PG17, and AlloyDB PG17
- E2e tests with Gemini: 3 real LLM findings verified

#### C Extension (Frozen)
- v0.6.0-rc3 — no new features
- Works on self-managed PostgreSQL with auto-explain hooks
- SQL functions: sage.explain(), sage.diagnose(), sage.briefing()
