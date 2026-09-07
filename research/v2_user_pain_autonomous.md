# pg_sage v2 — Community Pain, Reframed Around Autonomous Action

> Research date: 2026-06-10
> Sources: dba.stackexchange / Stack Overflow, r/PostgreSQL & r/devops & r/sysadmin,
> pgsql-general/-performance + bug list, Hacker News, and Postgres vendor blogs
> (Crunchy, EDB, Cybertec, Percona, pganalyze, Stormatics, PostgresAI, incident.io).
> Constraint: research only — no source changes.

---

## Framing: the product thesis is *closing the loop*, not drawing another chart

The three prior research notes (`2026-04-29-community-pain-points.md`,
`user_pain_points.md`, `v010_reddit_community.md`) are thorough but written in
**advisor/observability** voice — "detect X, recommend Y, alert with context." That
work stands. This note deliberately does *not* re-enumerate the pains; it re-scores
each one by a single question the others never asked:

> **Can an LLM-reasoning + gated-executor with rollback actually *finish the job*
> end-to-end, or does the human still have to do the hands-on part?**

That question matters because pg_sage's `REVERSE_SPEC.md` says the live executor is
real but **narrow**: it gates `findings` and runs a SQL-whitelisted set of actions
(`CREATE INDEX CONCURRENTLY`, `VACUUM`, `REINDEX`, `ALTER SYSTEM` config) on dedicated
connections with rollback metadata. The richer `cases`/`ActionContract` model is
API-advisory only and never reaches the executor. So the highest-value roadmap moves
are pains where (a) the community is loud, (b) the remediation is a *small, reversible,
whitelistable SQL action*, and (c) an LLM adds genuine judgment that a static threshold
cannot.

### The adoption gate is trust, and the community is hostile to autonomy

Before ranking pains, correct a false premise that lurks in autonomous-DBA pitches:
that operators *want* an agent with prod write access. They emphatically do not. On a
recent HN thread on agents and databases, the reaction was near-unanimous: *"Giving LLM
agents direct, autonomous access to a real production database with write access seems
insane to me… NO ONE, agent or human, should have direct write access to production
databases outside of emergency break glass scenarios"* (user **dherls**), and *"Who the
hell let agents directly use a database? Even humans don't get this privilege"* (user
**pilgrim0**)
[HN: agentic AI violates database assumptions](https://news.ycombinator.com/item?id=47897140).
A practicing DBRE in the same thread: *"I also operate with a read-only grant, because
manual writes to a prod DB is generally a terrible idea"* (**sgarland**).

This is not a reason to abandon the autonomous thesis — it is the *spec* for it. The
winning posture is exactly what pg_sage already has the bones of: **deterministic
rules decide *whether*, the LLM decides *which/why/how-risky*, and a SQL whitelist +
trust ramp + rollback decides *whether it's allowed to fire*.** Every pain below is
scored on how cleanly it fits that posture. Kendra Little (25-year DBA) framed the
acceptance bar bluntly in prior research: *"An AI Agent Doesn't have to be Perfect —
Just Make Fewer Mistakes Than a Person."* The corollary: the agent must make its few
mistakes *cheaply reversible*. Reversibility is the feature, not autonomy.

---

## 1. Autovacuum / bloat — *the* flagship autonomous win, but the action is per-table tuning, not "run VACUUM"

**The pain (recurs constantly).** Every prior note ranks this VERY HIGH and that holds.
The fresh framing insight: the community's actual fix is almost never "run VACUUM once."
It is **per-table reloptions tuning** — the default 20% dead-tuple scale factor is
catastrophically wrong for high-churn tables (queues, sessions, events). Stormatics/
Bufisa: *"High-churn tables need smaller scale factors (1–5% instead of the default
20%)"*
[Bufisa: why Postgres bloats](https://bufisa.com/2025/08/19/why-postgresql-bloats-over-time-and-how-to-prevent-it/).
A concrete pganalyze-style war story recurs across blogs: *"One table had 14 million dead
tuples and autovacuum hadn't touched it in three days because autovacuum cost delay was
set so conservatively. One configuration change later, dead tuple accumulation dropped
to under 100K, and the latency spikes disappeared"*
[pganalyze: visualizing & tuning autovacuum](https://pganalyze.com/blog/visualizing-and-tuning-postgres-autovacuum).
EDB and Cybertec both stress the subtlety the static rule misses: scale factor is on
*total* rows, so a big table with hot recent rows never crosses the threshold even as
it bloats
[EDB: autovacuum tuning basics](https://www.enterprisedb.com/blog/autovacuum-tuning-basics),
[Cybertec: autovacuum wraparound protection](https://www.cybertec-postgresql.com/en/autovacuum-wraparound-protection/).

**The autonomous action.** LLM call: given per-table churn rate (n_tup_upd/del deltas
from snapshots), size, current reloptions, last (auto)vacuum time, and bloat estimate,
emit a *per-table* `ALTER TABLE … SET (autovacuum_vacuum_scale_factor=0.02,
autovacuum_vacuum_cost_delay=2, …)` plus an optional one-shot `VACUUM (ANALYZE)`.
Executor action: `ALTER TABLE … SET (...)` is whitelistable, instantly reversible
(store prior reloptions as rollback), and far lower-blast-radius than a global config
change. This is the single best fit in the catalog: loud pain, surgical reversible SQL,
real LLM judgment on *which tables and how aggressive*.

**Why it's hard to automate safely.** Over-aggressive scale factors turn one bloated
table into a vacuum storm that starves the autovacuum workers for everything else;
`cost_delay`/`cost_limit` changes interact globally. The agent must reason about *worker
contention budget across the whole instance*, not optimize one table in isolation — a
genuinely LLM-shaped constraint-satisfaction problem. REVERSE_SPEC notes index-bloat/
REINDEX advising is *plumbed to the API but has no Tier-1 rule*; per-table autovacuum
reloptions appear similarly unimplemented as an *action*. That is the gap to close.

## 2. Transaction-ID wraparound — the "must never lose" autonomous freeze

**The pain (rare but catastrophic).** When a table's age crosses
`autovacuum_freeze_max_age` (default 200M), an anti-wraparound autovacuum runs *even if
autovacuum is disabled*, and at ~2B the server **shuts down to protect the data**
[PG docs: routine vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html),
[Cybertec: wraparound protection](https://www.cybertec-postgresql.com/en/autovacuum-wraparound-protection/).
Crunchy and AWS both publish dedicated wraparound runbooks precisely because operators
discover it too late
[Crunchy: managing TXID wraparound](https://www.crunchydata.com/blog/managing-transaction-id-wraparound-in-postgresql),
[AWS: prevent wraparound with postgres_get_av_diag](https://aws.amazon.com/blogs/database/prevent-transaction-id-wraparound-by-using-postgres_get_av_diag-for-monitoring-autovacuum/).

**The autonomous action.** This is the *least* LLM-dependent and *most* defensible
autonomous action in the whole product: a deterministic rule on `age(relfrozenxid)`
crossing a high-water mark fires a manual `VACUUM (FREEZE)` on the offending table via a
dedicated connection — exactly the kind of action pg_sage's executor already runs. The
LLM's only job is *narration and scheduling* (pick a low-traffic window, warn about the
I/O hit, escalate if the table is huge). Highest-trust action class: the downside of
*not* acting is a full outage.

**Why it's hard.** A freeze on a multi-TB cold table is itself an I/O event; the failsafe
VACUUM *ignores cost limits* and can hammer the instance
[Percona: overcoming VACUUM wraparound](https://www.percona.com/blog/overcoming-vacuum-wraparound/).
The agent must weigh "act now and cause a smaller I/O storm" vs "wait for the window and
risk crossing failsafe." And — critically — wraparound is frequently *blocked by an
inactive replication slot or a long-running transaction* (see §9), so the correct action
is often "drop the slot / kill the txn," not "vacuum harder." Reasoning across that causal
chain is the LLM's real contribution.

## 3. Index lifecycle — missing FK indexes (auto-create) + unused/duplicate (auto-drop)

**The pain (HIGH, two-sided).** Prior notes cover missing FK indexes well. The fresh,
*action-shaped* half is **cleanup**: unused and duplicate indexes are a measurable,
ongoing tax. Percona benchmarked *"up to 58% throughput loss due to excessive indexes
competing for cache space"* even at 99.7% cache hit ratio
[Percona: index maintenance queries](https://www.percona.com/blog/useful-queries-for-postgresql-index-maintenance/);
PostgresAI's 2025 "keep your index set lean" marathon makes the same write-amplification
case
[PostgresAI: why keep your index set lean](https://v2.postgres.ai/blog/20251110-postgres-marathon-2-013-why-keep-your-index-set-lean).
Haki Benita's "freed 20GB" piece is the canonical reproducible win
[Haki Benita: unused index size](https://hakibenita.com/postgresql-unused-index-size).
Stormatics' safe-removal guide is, in effect, a script for an autonomous executor:
detect via `pg_stat_user_indexes`, confirm zero scans over a window, and drop — *but
mark invalid/duplicate first and keep a rebuild script*
[Stormatics: unused indexes — risks, detection, safe removal](https://stormatics.tech/blogs/unused-indexes-in-postgresql-risks-detection-and-safe-removal).

**The autonomous action.** Two reversible actions: (a) missing FK index →
`CREATE INDEX CONCURRENTLY` (already in the whitelist); (b) confirmed-unused/duplicate
index → `DROP INDEX CONCURRENTLY`, with the original `CREATE INDEX` statement captured as
exact rollback. Dropping an index is the most cleanly reversible mutation in Postgres —
the definition fully reconstructs it — which makes it a textbook fit for the trust ramp.
The LLM judges *which* of two near-duplicate indexes to keep (column order, partial
predicate, INCLUDE columns) — a real reasoning task a `pg_index` equality check botches.

**Why it's hard.** `pg_stat_user_indexes` resets on crash/restart and is per-replica;
"zero scans" on the primary can hide heavy use on a standby. Unique/PK indexes back
constraints and must never be dropped. An index used only by a quarterly report looks
identical to a dead one over a 30-day window. The agent needs reset-detection and
constraint-awareness — exactly the failure modes REVERSE_SPEC's `unused_index_window_days`
boundary tests are meant to guard.

## 4. Plan regressions / generic-plan flips — detect autonomously, *act* conservatively

**The pain (HIGH, under-observed).** The canonical signature: a prepared statement is
fast for its first five executions, then PostgreSQL switches to a generic plan that is
dramatically slower. Richard Yen's 2026 deep-dive documents *"the query planner silently
changes the execution plan for prepared statements after exactly five executions"* and
the resulting *"sudden performance regressions"*
[Richard Yen: hidden behavior of plan_cache_mode](https://richyen.com/postgres/2026/03/30/plan_cache_mode.html);
a 2025 pgsql bug-list thread asks how to even *observe* the custom→generic transition,
confirming it is hard to see from `pg_stat_statements` alone
[pgsql: observe plan_cache_mode transition](https://www.postgresql.org/message-id/CABR0jERKQzmE7G5nDpHDfA+502OAZaNcYY46KL=w1CtdQ1NcQw@mail.gmail.com).
`auto_explain` is the community's catch tool
[pganalyze: EXPLAIN normalized queries via plan_cache_mode](https://pganalyze.com/blog/5mins-postgres-explain-pg-stat-statements-plan-cache-mode-normalized-query).

**The autonomous action.** Detection is squarely in pg_sage's wheelhouse (queryid
latency-distribution drift across snapshots → finding). The *action* is where caution is
mandatory. The safe, reversible moves in order of preference: (1) `ANALYZE` the relevant
tables if stats are stale (often the real cause); (2) add/refresh **extended statistics**
(see §6); (3) only as a flagged HIGH-risk advisory, suggest `plan_cache_mode` guidance —
which pg_sage should *not* auto-apply, because it is session/role-scoped and easy to make
globally worse.

**Why it's hard.** A generic plan is not always wrong; flipping it can fix the slow
case but regress the fast parameter set. Server-wide `plan_cache_mode = force_custom_plan`
trades CPU planning cost for latency stability and can backfire. This is the clearest
case in the catalog where the LLM's job is to **stop short of acting** and instead
produce a precise, evidence-linked recommendation — autonomous *diagnosis*, human-gated
remediation. Honest scoping here builds the trust that §1–§3 spend.

## 5. Connection exhaustion / pooling — mostly *not* autonomously closeable; correct the pitch

**The pain (HIGH).** Connection storms and pool misconfiguration are mainstream outage
triggers (prior notes cover Clerk's thundering-herd postmortem and Cloud SQL surges).

**Honest verdict on automatability.** This is the pain most over-promised by autonomous-
DBA marketing, and the research says **the durable fix lives outside the database** —
in PgBouncer/RDS-Proxy/app pool config pg_sage cannot reach over the wire. What pg_sage
*can* do autonomously is narrow and worth being precise about:
- **Lower `idle_in_transaction_session_timeout`** via `ALTER SYSTEM`/`ALTER ROLE` to
  shed sessions that hold locks and block vacuum — a real, reversible config action
  [runebook: idle_in_transaction_session_timeout](https://runebook.dev/en/docs/postgresql/runtime-config-client/GUC-IDLE-IN-TRANSACTION-SESSION-TIMEOUT).
- **`pg_terminate_backend()` a specific idle-in-transaction offender** that is provably
  blocking others — but note `pg_cancel_backend` is insufficient for the idle-in-txn
  state; you must *terminate*
  [Postgres Scripts: kill idle sessions](https://www.postgresscripts.com/post/kill-idle-postgresql-sessions-with-pg-terminate-backend/).

Everything else ("raise max_connections," "add a pooler") is advisory text, not an
executor action. Pitch it that way. (See §9 — terminating the *right* backend is a
high-value autonomous action when it unblocks vacuum or a lock queue.)

**Why it's hard.** Terminating a backend can roll back legitimate in-flight work;
choosing *which* PID to kill is a blast-radius judgment (how long idle, what locks held,
what's queued behind it) — good LLM territory, but the cost of a wrong kill is a lost
transaction, so it belongs at the top of the trust ramp with tight evidence requirements.

## 6. Stats / ANALYZE — the highest-ratio autonomous action (tiny blast radius, big wins)

**The pain (HIGH, quietly).** Stale statistics after bulk loads silently produce
catastrophic plans. boringSQL: *"Simple lookup queries can jump from milliseconds to
several seconds right after a large batch load, purely because the planner was still
trusting outdated row count estimates"*
[boringSQL: why queries run slow](https://boringsql.com/posts/postgresql-statistics/).
The 2026 techbuddies "7 ways stats go bad" piece and pganalyze's stale-stats insight both
make ANALYZE the first-line fix
[techbuddies: 7 ways stats go bad](https://www.techbuddies.io/2026/02/28/top-7-ways-postgresql-statistics-go-bad-and-make-queries-slow/),
[pganalyze: stale stats insight](https://pganalyze.com/docs/explain/insights/stale-stats).
The subtler win is **multi-column correlation**: without `CREATE STATISTICS`, the planner
assumes independence between correlated columns (country/state, status/closed_at) and
*"misestimates how many rows match multi-column filters, and that cascades into bad join
choices"* [boringSQL](https://boringsql.com/posts/postgresql-statistics/).

**The autonomous action.** (a) Rule: large `n_mod_since_analyze` relative to table size,
or a detected bulk-load delta, → `ANALYZE table` on a dedicated connection. ANALYZE is
read-mostly, fast, and self-healing — *arguably the safest mutation pg_sage can make*,
worth granting at a low trust level. (b) Higher-judgment LLM action: when a slow plan
shows a multi-column-filter misestimate, emit `CREATE STATISTICS … (ndistinct, dependencies)
ON (col_a, col_b) FROM table` then `ANALYZE`. This is *additive and reversible*
(`DROP STATISTICS`), low-risk, and the column-pair selection is exactly the inference an
LLM does well and a static rule cannot. Note REVERSE_SPEC already has a process-wide
ANALYZE semaphore + table-size cap — the safety scaffolding for this is *already built*.

**Why it's hard.** ANALYZE on a giant table samples and costs I/O; firing it reflexively
after every batch can itself become the load problem (hence the existing semaphore/cap).
Extended-statistics objects add planning overhead if over-applied. Judgment, not reflex.

## 7. Partition maintenance — *don't* build this; it's a solved, owned problem

**Correcting a tempting premise.** Partition lifecycle (create future partitions, detach/
drop old ones) *looks* like ideal autonomous-executor territory, but it is **already
owned by pg_partman**, including a background worker that needs no external scheduler:
*"A background worker process is included to automatically run partition maintenance
without the need of an external scheduler (cron, etc) in most cases"*
[pg_partman README](https://github.com/pgpartman/pg_partman),
[AWS: managing partitions with pg_partman](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL_Partitions.html).
Re-implementing `run_maintenance_proc` in pg_sage is premature abstraction against a
battle-tested incumbent.

**The defensible autonomous slice.** Where pg_partman *isn't* installed (common on RDS/
Cloud SQL where the extension may be unavailable or unmanaged), pg_sage can detect a
declaratively-partitioned table that is **about to run out of future partitions** (insert
trending toward the latest `range_to`) and autonomously `CREATE TABLE … PARTITION OF …`
ahead of the boundary — a reversible, low-risk DDL. The LLM infers the interval and naming
convention from existing partition names. The detach/drop-old half should stay *advisory*
(dropping data is irreversible and outside the rollback model).

**Why it's hard.** Detaching/dropping partitions destroys data — categorically outside a
"reversible action with rollback metadata" executor. Detecting the partition *interval*
from heterogeneous naming schemes is fiddly. Provider differences (managed pg_partman vs
none) mean the agent must reason about *what's already automating this* before acting.

## 8. Online schema change / DDL safety — pre-flight gate, not a mutator

**The pain (HIGH; causes real revenue-loss outages).** `v010_reddit_community.md` already
covers the 4-hour 84M-row CHECK-constraint lockout and GoCardless's 15-second FK outage
exhaustively. The autonomous-action reframe: pg_sage is *over the wire*, so it does not
*run* the migration — but it *can* autonomously **set guardrails and kill blockers in
real time**. incident.io's 2025 deadlock-in-bulk-upsert postmortem shows the live-lock
dimension this addresses
[incident.io: debugging deadlocks in Postgres](https://incident.io/blog/debugging-deadlocks-in-postgres).

**The autonomous action.** (a) Detect an `AccessExclusiveLock` waiter with a growing FIFO
queue behind it (the lock-queue cascade) and autonomously `pg_terminate_backend()` the
*blocking* long-running query — or the stuck DDL itself — to drain the queue. (b) Detect
an **INVALID index** left by a failed `CREATE INDEX CONCURRENTLY` (`pg_index.indisvalid =
false`) and autonomously `DROP INDEX CONCURRENTLY` + re-issue — a clean, reversible
self-heal. (c) Set a session/role `lock_timeout` default via config so future migrations
fail fast instead of queueing
[Bytebase: Postgres timeout explained](https://www.bytebase.com/blog/postgres-timeout/).

**Why it's hard.** Killing the blocker vs killing the DDL is a judgment call with opposite
consequences; getting it backwards extends the outage. The agent has a sub-second window
and must reason about *who is victim vs cause* in the lock graph — high-stakes, high-LLM-
value, top-of-trust-ramp. INVALID-index cleanup is far safer and should ship first.

## 9. Replication slots & WAL — autonomously drop the *inactive* slot before the disk fills

**The pain (MEDIUM-HIGH; spectacular when it hits).** This is an under-appreciated
autonomous win the prior notes mention only in passing. A 2025 postmortem: an inactive
logical slot drove disk from ~70GB of data to **1.06 TB of retained WAL**, and because an
inactive slot's `catalog_xmin` is cluster-wide, *"an inactive replication slot on one
database can prevent Autovacuum from removing dead tuples in all databases"* — autovacuum
ran but silently skipped cleanup. Dropping the two dead slots took disk from **1.06 TB →
40 GB** and CPU from ~80% → <10% instantly
[dev.to: replication-slot/autovacuum almost-outage postmortem](https://dev.to/sasikumart/postgres-almost-outage-postmortem-the-hidden-dangers-of-replication-slots-and-autovacuum-2nem).
Gunnar Morling's "insatiable replication slot" is the canonical explainer
[Morling: the insatiable Postgres replication slot](https://www.morling.dev/blog/insatiable-postgres-replication-slot/).
PG18 adds `idle_replication_slot_timeout`; pre-18, `max_slot_wal_keep_size` is the cap
[Netdata: replication slot bloat](https://www.netdata.cloud/guides/postgres/postgres-replication-slot-bloat/).

**The autonomous action.** Rule: a slot with `active = false`, growing `restart_lsn` lag,
and WAL retention crossing a disk-headroom fraction → after a grace window,
`SELECT pg_drop_replication_slot(name)` (or set `max_slot_wal_keep_size` via `ALTER
SYSTEM`). This directly closes a top disk-emergency cause and *unblocks vacuum fleet-wide*
— a cascade fix. The LLM's contribution is distinguishing "abandoned experiment slot"
(safe to drop) from "temporarily-disconnected real replica/CDC consumer" (do **not** drop).

**Why it's hard, and the sharp safety edge.** Dropping a slot that belongs to a *real but
briefly disconnected* replica or Debezium/CDC connector breaks replication and forces a
full re-seed — irreversible and severe. So this action needs a *long, conservative*
inactivity grace, slot-name/type heuristics, and probably an explicit allowlist. It is a
perfect illustration of the whole product: the *detection* is deterministic, the
*go/no-go* is LLM judgment over ambiguous context, and the *guardrail* (grace window +
trust ramp) is what makes autonomy survivable. Setting `max_slot_wal_keep_size` is the
safer first action — it invalidates the slot rather than silently dropping it.

## 10. Parameter tuning (work_mem / shared_buffers / checkpoint) — config actions, asymmetric risk

**The pain (MEDIUM, affects everything).** `work_mem` is the footgun: it is *per-operation,
per-connection*, so a query with 3 sorts + 2 hash joins can use 5×work_mem, and 50 such
queries can blow past RAM into an OOM kill of the postmaster — *"your Postgres will crash
completely if you've exhausted the memory thanks to having too high work_mem"*
[pganalyze: work_mem tuning](https://pganalyze.com/blog/5mins-postgres-work-mem-tuning).
The 2025 RDS spill-debugging writeup shows the inverse pain (too-low work_mem → temp-file
spill)
[Nataliia Dziubenko: debug RDS spilling to disk](https://nataliiadziubenko.com/2025/07/17/aws-rds-how-to-debug-spilling.html).
Checkpoint spikes are tuned via `max_wal_size`/`checkpoint_completion_target`/
`checkpoint_timeout`, watching the timed-vs-requested ratio in `pg_stat_bgwriter`
[Crunchy: tuning for high write loads](https://www.crunchydata.com/blog/tuning-your-postgres-database-for-high-write-loads),
[EDB: tuning max_wal_size](https://www.enterprisedb.com/blog/tuning-maxwalsize-postgresql).

**The autonomous action.** pg_sage's advisor already produces these recs; the executor
already does `ALTER SYSTEM`. The autonomy refinement is **directional risk asymmetry**:
- *Raising* `max_wal_size` / `checkpoint_timeout` / `checkpoint_completion_target` to
  smooth checkpoint I/O is low-risk and reversible → good autonomous-tier action.
- *Raising* `work_mem` is dangerous (OOM) and should stay advisory or be auto-applied only
  in tiny, monitored increments with the rollback armed. *Lowering* work_mem to stop a
  spill is safer to auto-apply.
The LLM's job is reasoning about the *interaction* (work_mem × max_connections × observed
concurrency) — the exact multiplication the docs warn about — before any change.

**Why it's hard.** `shared_buffers` needs a restart (outside the live-mutate model and
should remain advisory). `work_mem` has no safe global value; the same setting that fixes
a report OOMs a connection storm. Config changes have instance-wide blast radius, which is
why this sits *below* the surgical per-table actions of §1/§3/§6 despite being "easy" SQL.

---

## Lower-tier pains (real, but weaker autonomous fit — keep advisory)

- **Major-version upgrades / EOL.** PG13 hit EOL Nov 13 2025; clouds now *charge* for
  extended support of EOL versions
  [PG versioning policy](https://www.postgresql.org/support/versioning/),
  [Cloud SQL release notes](https://docs.cloud.google.com/sql/docs/postgres/release-notes).
  Extensions (timescaledb, pg_hint_plan, PostGIS, plv8) block in-place `pg_upgrade` and
  stats don't carry over (post-upgrade `vacuumdb --analyze-in-stages`)
  [Percona: upgrading extensions safely](https://www.percona.com/blog/upgrading-postgresql-extensions/).
  **Autonomous fit: low.** The one clean autonomous action is *post-upgrade*: detect
  missing stats and run staged ANALYZE (a §6 action). EOL/compat is advisory checklist
  work. High value as a *briefing*, not an executor action.
- **Role/grant/security hygiene.** PG15 revoked `CREATE` on `public` from `PUBLIC`; the
  least-privilege checklist (`REVOKE CREATE ON SCHEMA public`, `REVOKE ALL ON DATABASE …
  FROM PUBLIC`) is well-trodden
  [Percona: public schema security upgrade in PG15](https://www.percona.com/blog/public-schema-security-upgrade-in-postgresql-15/),
  [Severalnines: locking down the public schema](https://severalnines.com/blog/postgresql-privileges-and-security-locking-down-public-schema/).
  **Autonomous fit: deliberately low.** Auto-`REVOKE` risks breaking an app that quietly
  relied on the grant; security changes should be advisory with a one-click apply, *not*
  trust-ramped autonomy. Getting this wrong is an outage *and* a trust-destroying one.
- **Backup / PITR verification.** *"A backup you've never tested is not a backup"* and
  silent `archive_command` failures grow WAL invisibly
  [Cloud Ingenium: pgBackRest PITR & troubleshooting](https://kx.cloudingenium.com/pgbackrest-postgresql-backup-pitr-recovery-troubleshooting/).
  **Autonomous fit: low over-the-wire.** pg_sage can autonomously *detect* a failing/
  stalled archiver and WAL accumulation (overlaps §9) and alert; it cannot test a restore
  without infra it doesn't control. Detect-and-escalate, not act.

---

## Top 10 most-automatable pains (ranked by recurrence × LLM-closeability)

Ranking weights: how often the community hits it × how cleanly an *LLM-decides + gated
reversible SQL action* closes it end-to-end. "Closeability" penalizes pains whose real fix
lives outside the database or is irreversible.

| # | Pain | Autonomous action (LLM → executor) | Recurrence | Reversibility / safety | Net |
|---|------|------------------------------------|-----------|------------------------|-----|
| 1 | **Per-table autovacuum tuning** (§1) | LLM picks per-table scale factor/cost delay → `ALTER TABLE … SET (...)` + optional `VACUUM` | Very high | High (store prior reloptions) | **Top pick** |
| 2 | **Stats / ANALYZE + extended stats** (§6) | Rule on n_mod_since_analyze → `ANALYZE`; LLM picks correlated cols → `CREATE STATISTICS` | High | Very high (ANALYZE self-heals; DROP STATISTICS) | Highest ratio |
| 3 | **TXID wraparound freeze** (§2) | Deterministic age threshold → `VACUUM (FREEZE)`; LLM schedules window | Low freq / catastrophic | High; cost of inaction = outage | Must-have |
| 4 | **Unused / duplicate index cleanup** (§3) | LLM picks which dup to keep → `DROP INDEX CONCURRENTLY` (CREATE stmt = rollback) | High | Very high (index fully reconstructs) | Strong |
| 5 | **Missing FK / query index** (§3) | LLM workload-aware rec → `CREATE INDEX CONCURRENTLY` | High | High (drop = rollback) | Already partly built |
| 6 | **Inactive replication slot → WAL/vacuum block** (§9) | LLM "experiment vs real consumer" → `max_slot_wal_keep_size` then `pg_drop_replication_slot` | Med-high | Mixed: cap=safe, drop=irreversible (needs grace) | High-impact cascade fix |
| 7 | **Checkpoint-I/O config** (§10) | LLM → raise `max_wal_size`/`checkpoint_*` via `ALTER SYSTEM` | Medium | High (reversible, directional) | Solid |
| 8 | **INVALID index self-heal + lock-queue kill** (§8) | Detect `indisvalid=false` → `DROP/CREATE`; detect lock cascade → `pg_terminate_backend(blocker)` | High | INVALID-cleanup safe; backend-kill high-stakes | Ship the safe half first |
| 9 | **Idle-in-transaction termination** (§5) | LLM picks provably-blocking PID → `pg_terminate_backend` + lower `idle_in_transaction_session_timeout` | High | Medium (kills in-flight work) | Top of trust ramp |
| 10 | **Future-partition pre-creation** (§7, no pg_partman) | LLM infers interval/naming → `CREATE TABLE … PARTITION OF` ahead of boundary | Medium | High (drop empty future partition) | Niche but clean |

*Deliberately ranked low / advisory-only:* plan-regression remediation (auto-act is
unsafe — §4), connection pooling (fix lives outside the DB — §5), work_mem raises (OOM —
§10), partition *drop* (irreversible — §7), grant revokes & backup-restore testing
(§lower-tier). These are where over-promising autonomy *destroys* trust.

---

## Open Questions

1. **Trust ramp by action class, not by calendar day.** REVERSE_SPEC says trust is a
   *manually-set string* with no auto-promotion, and the day-8/day-31 thresholds gate a
   single global level. But the rankings above show risk is *per-action*: ANALYZE is
   safe on day 1; dropping a replication slot should never be calendar-gated at all.
   Should pg_sage replace one global trust level with **per-action-class trust** (ANALYZE
   auto from day 0; index DROP at day 8; backend-kill only ever advisory unless explicitly
   armed)? That seems mandatory for the ranking to be actionable.

2. **Where is the rollback boundary for "drop" actions?** Dropping an index is reversible
   (the CREATE statement reconstructs it). Dropping a replication slot or a partition is
   *not*. Should the executor enforce a hard rule — "no action whose rollback cannot be
   expressed as a stored SQL statement may ever auto-fire" — which would correctly gate
   slot-drop and partition-drop behind human confirmation while freeing index/config/
   ANALYZE/reloptions actions to autonomy?

3. **Causal-chain reasoning vs single-finding actions.** Several top pains are *cascades*:
   inactive slot → vacuum blocked → bloat → wraparound risk. pg_sage's executor gates
   individual `findings` deduped on (Category, ObjectIdentifier). Can it reason that the
   *root* action (drop slot) resolves three downstream findings at once, instead of firing
   three independent remediations? This is the strongest argument for wiring the richer
   `cases`/`ActionContract` model into the live executor.

4. **Replica/standby blindness.** `pg_stat_user_indexes` and slot activity look different
   on primary vs standby; REVERSE_SPEC notes fleet hardcodes `isReplica=false` and HA
   safe-mode is wired to nothing. An "unused index" auto-drop or a "freeze now" decision
   made on primary-only stats can be wrong. Does autonomous action require finishing the
   HA/replica-awareness plumbing *first*?

5. **How conservative must the slot-drop grace window be, and can it be learned?** A real
   CDC consumer can be down for minutes during a deploy; an abandoned experiment is down
   forever. Is there a defensible default (hours? days?), or must this always be an
   allowlist? Can the agent learn each slot's normal reconnect cadence from history?

6. **What's the minimum evidence bar to autonomously `pg_terminate_backend`?** Killing a
   backend is the highest-value *and* highest-regret action (unblocks vacuum/lock queues,
   but discards in-flight work). What concrete predicate — idle-in-txn duration, locks
   held, queue depth behind it, confirmed blocking of a higher-priority action — should
   gate it, and should it *ever* fire without human confirmation?

7. **Does "over the wire, no extension" cap the autonomous ceiling?** The biggest wins in
   connection pooling, partition automation (pg_partman BGW), and PITR testing all assume
   in-cluster extensions or external infra pg_sage explicitly avoids. Is the no-extension
   stance a moat (easy install, broad reach) or a ceiling on how much pg_sage can actually
   *close* vs *advise*? The honest answer shapes which pains to invest in.
