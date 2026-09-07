# Spec Review: v0.9.1 Log-Based RCA

**Reviewer**: Claude (spec review)
**Date**: 2026-04-13
**Spec**: `specs/v0.9.1-log-based-rca.md`
**Verdict**: Good foundation, but 25+ gaps that would cause two developers to
implement different things. Most are fixable with a paragraph each.

---

## 1. Ambiguity (Under-Specified Behavior)

### 1.1 LogSignal embeds `Signal` but `Signal` lives in `internal/rca`

The spec defines `LogSignal` in `internal/logwatch/` embedding `rca.Signal`.
This creates a circular dependency: `logwatch` imports `rca` for the embedded
type, but `rca` needs to call `LogWatcher.Drain()` which returns
`[]*LogSignal`. The spec's own interface definition (`LogSource.Drain()
[]*Signal`) contradicts the `LogSignal` struct.

**Amendment**: Clarify that `Signal` is the only type that flows across the
package boundary. `LogSignal` either (a) lives in `internal/rca/` alongside
`Signal`, or (b) `Drain()` returns `[]*Signal` with log-specific metadata
stuffed into the `Metrics` map (which is `map[string]any`). Option (b) is
simpler and avoids a new type entirely. The `SourcePID`, `SQLState`, and
`RawMessage` fields become Metrics entries: `Metrics["source_pid"]`,
`Metrics["sql_state"]`, `Metrics["raw_message"]`.

### 1.2 Dedup window semantics unspecified

The spec says "Suppress duplicate log signals within this window. Default: 60."
But it never defines what makes two signals "duplicates." Is it:

- Same signal ID only? (e.g., two `log_deadlock_detected` events 30s apart)
- Same signal ID + same PID?
- Same signal ID + same database + same user?
- Same signal ID + same SQLState + same message prefix?

This matters enormously for the deadlock tree, which asks "Same PIDs/tables
involved in > 1 deadlock in dedup window?" If we dedup on signal ID alone, the
tree never sees the second deadlock.

**Amendment**: Define dedup key explicitly. Recommended:
`(signal_id, database, user)`. Same signal from the same database+user within
the window is collapsed; different databases or users produce distinct signals.
The dedup ring buffer entry should also track a counter so the decision tree can
see "3 occurrences in window" even though only one signal was emitted.

### 1.3 `Drain()` timing relative to `Analyze()` cycle

The spec says "Drain() []*LogSignal for the RCA engine to pull per cycle" but
does not specify:

- Who calls `Drain()`? The RCA engine? The analyzer loop? The main loop?
- Is `Drain()` destructive (clears the buffer) or idempotent?
- If destructive, what happens if the RCA engine is temporarily disabled or
  crashes mid-cycle? Signals are lost.
- What is the maximum buffer size between drains?

The existing `Analyze()` signature is
`Analyze(current, previous *collector.Snapshot, cfg *config.Config,
lockChainFindings []analyzer.Finding)`. Log signals must be injected somewhere.

**Amendment**: Add a section specifying:
1. `Drain()` is destructive (returns and clears).
2. `rca.Engine` gains a `SetLogSource(LogSource)` method.
3. `Analyze()` calls `e.logSource.Drain()` internally at the start of each
   cycle, before `detectSignals()`.
4. Buffer cap is 10,000 signals; overflow drops oldest. Log a warning on
   overflow.

### 1.4 `log_temp_file_created` threshold is hard-coded

The spec says "Temp file > 10MB created" but 10MB is not in `LogWatchConfig`
or any config struct. Two developers would either hard-code 10MB or invent
different config keys.

**Amendment**: Add `temp_file_min_bytes` to `LogWatchConfig` with default
10485760 (10MB). Or document that ALL temp file log lines fire and let the
decision tree filter by size.

### 1.5 `RawMessage` truncation length (500 chars) is not in config

The spec says "truncated to 500 chars" for `RawMessage`. The `query` field in
the Metrics map is "truncated to 200 chars." Neither is configurable.

**Amendment**: Document these as constants (not config) with justification.
Add constants `maxRawMessageLen = 500` and `maxQueryLen = 200` in
`classifier.go`. These are reasonable defaults that should not be configurable
to avoid people setting them to 50KB and blowing up incident storage.

### 1.6 `Source` field for log-based incidents

Existing incidents have `Source: "deterministic"` or `Source: "llm"`. The spec
doesn't say what `Source` value log-only decision trees produce. Is it
`"deterministic"` (since the tree logic is deterministic)? Or a new value like
`"log"` or `"log_deterministic"`?

**Amendment**: Log-only trees should use `Source: "deterministic"`. The
`SignalIDs` array (containing `log_*` prefixed IDs) already distinguishes them.
No new source value needed.

---

## 2. Tailer Edge Cases

### 2.1 Hundreds of old log files in `log_directory`

PostgreSQL retains old log files indefinitely unless `log_rotation_age` or
`log_rotation_size` is set with `log_truncate_on_rotation = on`, or an
external log rotation tool cleans them up. A production `log_directory` can
easily have 500+ files.

The spec says the tailer uses fsnotify on the directory, but does not say:
- On startup, do we tail ALL files or just the latest?
- How do we identify "latest"?
- Do we process historical files at all?

**Amendment**: On startup, the tailer should:
1. List files matching `log_filename` pattern (query `SHOW log_filename` to
   get the pattern).
2. Sort by modification time.
3. Open ONLY the most recent file, seeking to the END (do not process
   historical content -- see 2.5).
4. When fsnotify fires a CREATE event for a new file matching the pattern,
   finish reading the old file, then switch to the new one.

### 2.2 `logging_collector = off`

The auto-detection queries `SHOW logging_collector` and says "must be 'on'."
But the spec doesn't handle the case where `logging_collector = off` AND
`log_destination = 'csvlog'`. PostgreSQL will still write csvlog to stderr,
just not to files. The sidecar can't tail stderr of another process.

**Amendment**: Already covered (Section 6, Phase 4: "If log_destination
doesn't include jsonlog or csvlog, log a warning and disable"). But also add:
"If `logging_collector = off`, disable with warning message: 'logwatch
requires logging_collector = on. Set in postgresql.conf and restart.'" The
current phrasing only checks log_destination, not logging_collector.

### 2.3 Log rotation: age-based vs size-based vs external

PostgreSQL supports:
- `log_rotation_age` (default: 24h) -- time-based rotation
- `log_rotation_size` (default: 0 = disabled) -- size-based rotation
- Both simultaneously
- External rotation (logrotate) via SIGHUP

The spec says "fsnotify watches directory (not file), re-open on CREATE event."
This handles PG's internal rotation (new file created). But external rotation
via `logrotate` with `copytruncate` will TRUNCATE the current file to 0 bytes
without creating a new one. The tailer's file offset will be past EOF.

**Amendment**: The tailer must detect truncation: if a read returns EOF and the
file size is less than the last read offset, reset offset to 0. Also watch for
fsnotify WRITE events after a size decrease. Add a test case for
`copytruncate` behavior.

### 2.4 Symlinks in `log_directory`

Some deployments symlink `log_directory` to a different volume (e.g.,
`/var/log/postgresql` -> `/mnt/logs/postgresql`). `fsnotify` follows symlinks
for the watched directory but NOT for individual files on all platforms.

**Amendment**: On startup, resolve `log_directory` via `filepath.EvalSymlinks()`
before passing to fsnotify. Document this behavior.

### 2.5 Sidecar starts with a 500MB current log file

The spec does not address cold-start behavior. If the sidecar restarts and the
current log file is 500MB, processing it from the beginning would:
- Consume significant CPU/memory
- Generate thousands of dedup-suppressed signals
- Emit stale incidents for events that may have already resolved

**Amendment**: On startup, seek to the END of the current file. Only process
NEW lines appended after the tailer starts. Document this as an explicit
design decision. Add a `startup_mode` note: "pg_sage processes log events
in real-time only. Historical log analysis is out of scope."

For crash recovery, consider persisting the last-read file path and byte
offset to disk (or to the `sage.` schema) so that on restart the tailer can
resume from where it left off rather than re-reading or skipping. This could
be a v0.9.2 enhancement; for v0.9.1, seek-to-end is sufficient.

### 2.6 Finding the active log file by `log_filename` pattern

PostgreSQL's default `log_filename` is `postgresql-%Y-%m-%d_%H%M%S.log`. The
spec doesn't say how the tailer matches files to this pattern. Two approaches:
- Parse the `strftime` pattern from `SHOW log_filename` and convert to a glob
- Just sort by mtime and take the newest

**Amendment**: Use mtime-based detection. Query `SHOW log_filename` for the
extension (`.log`, `.csv`, `.json`) to filter files, then sort by mtime.
Parsing strftime patterns adds complexity with no benefit since we only need
the latest file.

### 2.7 `log_file_mode` permissions

PostgreSQL's `log_file_mode` defaults to `0600` (owner-only). If the sidecar
runs as a different OS user than the PostgreSQL server, it cannot read the log
files.

**Amendment**: Add a startup check: attempt to open the current log file for
reading. If `EACCES`, log a clear error message:
"Cannot read log file {path}: permission denied. Ensure the pg_sage sidecar
user is in the postgres group, or set log_file_mode = 0640 in
postgresql.conf." Gracefully disable logwatch.

---

## 3. Parser Edge Cases

### 3.1 csvlog multi-line messages: buffering strategy unspecified

The spec acknowledges this in the risk table ("Buffer lines until PG's
line-continuation pattern completes") but never defines the buffering
strategy. PG's csvlog uses RFC 4180 quoting: a field containing newlines is
wrapped in double quotes. The continuation is signaled by an odd number of
unescaped double quotes on the current accumulated buffer.

Key questions:
- Max buffer size for a single multi-line entry?
- Timeout for incomplete entries (what if the file ends mid-message)?
- Does the parser track state across `Read()` calls or require complete
  entries?

**Amendment**: Use Go's `encoding/csv` reader which handles RFC 4180 natively,
including quoted newlines. Set `csv.Reader.LazyQuotes = true` for robustness.
Buffer limit: discard any single CSV record exceeding `max_line_len_bytes`.
Timeout: if a file read returns no data for 5 seconds while a partial record
is buffered, emit a warning and discard the partial record. Add this to the
parser section.

### 3.2 jsonlog and `log_line_prefix`

PostgreSQL's `log_line_prefix` setting does NOT affect `jsonlog` output. The
jsonlog format always outputs a fixed set of JSON fields regardless of
`log_line_prefix`. However, `csvlog` format also ignores `log_line_prefix`
(the prefix is a separate CSV column).

**Amendment**: Document this explicitly: "Neither jsonlog nor csvlog are
affected by log_line_prefix. The parser ignores this setting." This prevents
future confusion when someone reports that their `log_line_prefix` values are
missing from parsed entries.

### 3.3 `log_statement = 'all'` flood

If a busy database has `log_statement = 'all'`, every SQL statement is logged.
A database doing 10,000 QPS generates 10,000 log lines per second. The
classifier only cares about ~10 signal patterns (all error/warning level).
The parser would parse all 10,000 lines, and the classifier would discard
9,990 of them.

**Amendment**: Add an early-exit filter in the parser or between parser and
classifier. For jsonlog, check the `"error_severity"` field BEFORE full
parsing. For csvlog, check the error_severity column (column 12, 0-indexed
11). Discard lines with severity `LOG` that don't match any known pattern
keyword (`deadlock`, `temporary file`, `checkpoint`, `archive`, `autovacuum`,
`replication slot`). This avoids full struct allocation for 99%+ of lines.

Add a config option or document the design decision: "Lines with
error_severity = LOG are pre-filtered by keyword match before full parsing.
Only LOG lines containing signal-relevant keywords are fully parsed. Lines
with severity WARNING, ERROR, FATAL, or PANIC are always fully parsed."

### 3.4 Timezone handling in log timestamps

PostgreSQL jsonlog uses ISO 8601 timestamps with timezone offset. csvlog uses
`log_timezone` setting (which defaults to the server's timezone). The spec's
`LogEntry.Timestamp` is `time.Time` which is timezone-aware, but the spec
doesn't say how to handle:

- Server in UTC vs sidecar in America/New_York
- `log_timezone` set to a non-UTC value
- jsonlog timestamps that include timezone offset vs csvlog timestamps that
  may or may not include timezone info

**Amendment**: All timestamps are parsed with their embedded timezone info
(jsonlog includes offset; csvlog includes offset in PG14+). After parsing,
convert to UTC immediately. `LogEntry.Timestamp` is always UTC. The FiredAt
field on the Signal is set from `LogEntry.Timestamp`, not from
`time.Now()`. Add this to the parser spec.

### 3.5 Character encoding (UTF-8 vs SQL_ASCII)

PostgreSQL log files are always written in the server encoding. If the
database uses `SQL_ASCII` encoding, log messages may contain arbitrary byte
sequences that are not valid UTF-8. Go strings are UTF-8, and `json.Unmarshal`
will reject invalid UTF-8 in jsonlog.

**Amendment**: The parser should sanitize non-UTF-8 bytes before JSON
parsing. Use `strings.ToValidUTF8(line, "\uFFFD")` to replace invalid
sequences with the Unicode replacement character. Document this behavior.

### 3.6 csvlog column count varies by PG version

PG14 added the `query_id` column (column 27) to csvlog. PG versions before
14 have 26 columns. The spec targets csvlog support but doesn't specify which
column layout to expect.

**Amendment**: The csvlog parser should handle both 26 and 27+ column rows.
Parse by known column positions up to the columns that exist. If a row has
fewer columns than expected, the missing fields are empty strings. The parser
should NOT fail on unexpected column counts -- it should parse what it can
and log a warning if columns are fewer than 23 (the minimum for meaningful
data).

---

## 4. Classifier Edge Cases

### 4.1 Deadlock involving pg_sage's own PID

If pg_sage's own queries participate in a deadlock (e.g., during schema
migration or concurrent snapshot persistence), the `log_deadlock_detected`
signal would fire for pg_sage itself. This creates noise and could trigger
remediation actions against the sidecar's own sessions.

**Amendment**: The classifier should check `entry.Application` against
`"pg_sage"` (the sidecar's `application_name`). If the deadlocked session is
pg_sage itself, emit the signal with an additional
`Metrics["self_inflicted"] = true` flag. The decision tree should note this
in the root cause: "Deadlock involved pg_sage's own session. This may indicate
a schema migration conflict or concurrent sidecar restart. No application
action needed."

### 4.2 pg_sage's own queries hitting `statement_timeout`

If `statement_timeout` is set at the role or database level and a pg_sage
query exceeds it, the sidecar will see `log_statement_timeout` in the logs
for its own queries. This is a legitimate operational issue but should not be
reported as an application incident.

**Amendment**: Same as 4.1 -- filter on `application_name`. For self-inflicted
timeouts, log internally at WARN level ("pg_sage query exceeded
statement_timeout: {query}") but do NOT emit a `log_statement_timeout` signal.
Add `pg_sage` to a default exclusion list for `application_name`, which is
also configurable: `logwatch.exclude_applications: ["pg_sage"]`.

### 4.3 Dedup window: per signal ID or per signal ID + PID?

Related to 1.2 but specific to PID handling. If 50 different backend PIDs all
hit `statement_timeout` in the same 60-second window, are they:
- 1 signal (dedup on signal ID)? This loses the count of affected sessions.
- 50 signals (no dedup on PID)? This floods the RCA engine.
- 1 signal with `occurrence_count: 50`?

**Amendment**: Dedup key should be `(signal_id, database)`. The dedup entry
tracks `count` and `pids` (set of unique PIDs). The emitted signal includes
`Metrics["occurrence_count"]` and `Metrics["affected_pids"]` (list, capped
at 20). This gives the decision tree enough information without flooding.

### 4.4 Rate limiting: 1000 `connection_refused` events in 1 second

If PostgreSQL hits `max_connections` and every new connection attempt logs
FATAL, the log file can produce thousands of lines per second. Even with
dedup, the parser must still read and parse each line.

**Amendment**: Add a per-signal-ID rate limiter in the classifier. After N
occurrences of the same signal ID within the dedup window, stop incrementing
the counter and set a `Metrics["rate_limited"] = true` flag. The parser
should also have a global rate limit: if more than 10,000 lines are parsed in
a single Drain cycle, stop parsing and emit a synthetic
`log_watcher_overloaded` warning signal. Add `max_lines_per_cycle` to
`LogWatchConfig` with default 10,000.

### 4.5 Signal pattern matching: substring vs exact match

The signal catalog uses patterns like "deadlock detected" and "too many
connections for role". The spec doesn't say whether pattern matching is:
- Exact string match on the `message` field
- Substring match (contains)
- Regex match
- Case-sensitive or case-insensitive

PostgreSQL messages are locale-dependent. A non-English locale could produce
different message text.

**Amendment**: Primary matching should be on `SQLState` (error code) where
available, since these are locale-independent. SQLState-based signals:
`log_deadlock_detected` (40P01), `log_connection_refused` (53300),
`log_out_of_memory` (53200), `log_lock_timeout` (55P03),
`log_statement_timeout` (57014). For signals without a unique SQLState
(`log_temp_file_created`, `log_checkpoint_too_frequent`,
`log_archive_failed`, `log_replication_slot_inactive`,
`log_autovacuum_cancel`), use case-insensitive substring match on the English
message text as a fallback, AND add a note that these signals are
English-locale-only in v0.9.1. Internationalized matching is out of scope.

---

## 5. RCA Integration Gaps

### 5.1 Temporal misalignment between log and metric signals

Metric signals have `FiredAt` set to `collector.Snapshot.CollectedAt`, which
is the time the snapshot was taken. Log signals have `FiredAt` set to the
timestamp from the log file. If the collector runs every 60 seconds and a
deadlock happened 45 seconds ago, the log signal timestamp and the metric
signal timestamp will be ~45 seconds apart.

The decision tree in Section 5.1 says "Check: log_connection_refused fired in
same cycle?" But "same cycle" is not defined for log signals that arrived
between metric collection cycles.

**Amendment**: Define "same cycle" as: log signals drained during this
`Analyze()` call. Since `Drain()` is called at the start of each `Analyze()`
cycle and is destructive, all returned log signals are considered part of the
current cycle regardless of their embedded timestamp. The `FiredAt` timestamp
on log signals is for display/investigation only -- it does NOT affect
whether they are considered "in the same cycle" as metric signals. Add this
definition to Section 5.1.

### 5.2 Log watcher starts mid-file: stale events

Even with seek-to-end on startup (from 2.5), there's a race: if the tailer
starts, seeks to end, but the file has received 100 new lines in the last
second that haven't been flushed yet, those lines appear "new" even though
they may describe events that happened before the sidecar started.

More importantly, if the sidecar crashes and restarts, it will miss all
events between the crash and the restart.

**Amendment**: Accept this as a known limitation for v0.9.1. Document:
"Log signals are best-effort. Events that occur while the sidecar is not
running are not captured. On startup, a brief burst of signals from
recently-written log lines is expected and will be deduplicated normally."
For v0.9.2, consider persisting the last-read byte offset.

### 5.3 Log signals feeding Tier 2 (LLM correlation)

The spec doesn't address whether log signals participate in Tier 2. The
existing `runTier2Correlation()` finds uncovered signals (not consumed by any
Tier 1 tree). If a `log_archive_failed` fires but has its own log-only tree,
it's covered. But what if `log_temp_file_created` fires alone (no
`log_out_of_memory`)? Is it uncovered? Does it go to Tier 2?

**Amendment**: Yes, log signals participate in Tier 2 like any other signal.
They are included in `findUncoveredSignals()`. Document this explicitly. The
`buildTier2UserPrompt()` function already handles any signal with an ID and
Metrics map. No code change needed, but the spec should state the intent.

### 5.4 `Incident.DatabaseName` for log-based incidents

Metric-based incidents do not set `DatabaseName` (it's set at the analyzer
level via `WithDatabaseName()`). Log entries include `database_name` from the
log line. If a log signal fires for database "orders" but the sidecar is
monitoring database "postgres", what `DatabaseName` goes on the incident?

**Amendment**: Log-based incidents should set `DatabaseName` from the
`LogEntry.Database` field. If the log entry's database is empty (e.g.,
background worker logs), use the sidecar's configured database name. Add
`DatabaseName` to the signal `Metrics` map so the decision tree can access
it.

### 5.5 Auto-resolve for log-only incidents

The existing auto-resolve logic checks whether any of the incident's
`SignalIDs` are still firing in the current cycle. For metric signals, the
detectors run every cycle, so a signal either fires or doesn't. For log
signals, a `log_deadlock_detected` may fire once and never again. After two
clear cycles, the incident auto-resolves -- which is correct for deadlocks
but wrong for `log_archive_failed` (which may still be failing but the log
line only appears once per failed archive attempt).

**Amendment**: Add a `StickyUntilCleared` flag to specific log signal
definitions. `log_archive_failed` should be sticky: it does not auto-resolve
until a `log_archive_success` line is seen (or a manual clear). Most other
log signals (deadlock, timeout) are ephemeral and auto-resolve normally.
Alternatively, the `log_archive_failed` tree can set a higher
`ResolutionCycles` override on its incident.

---

## 6. Config / Operational Edge Cases

### 6.1 Auto-detect queries require PG connectivity

The auto-detection runs `SHOW log_destination`, `SHOW log_directory`, etc.
If PostgreSQL is down when the sidecar starts, these queries fail. The spec
says "disable the watcher" but doesn't say whether it retries.

**Amendment**: Auto-detection should retry on the next analyzer cycle if it
failed on startup. Add a state: `logwatch_state` = `pending_autodetect |
active | disabled_no_format | disabled_no_permission | disabled_error`.
On each analyzer cycle, if state is `pending_autodetect`, attempt
auto-detection. Once configured, switch to `active`. If auto-detect finds
an unsupported format, switch to `disabled_no_format`. This avoids a hard
dependency on PG being up at sidecar startup time.

### 6.2 File permissions on Windows vs Linux

On Linux, log files default to `0600` owned by the `postgres` user. The
sidecar user needs read access (group membership or relaxed `log_file_mode`).

On Windows, PostgreSQL logs go to `%PGDATA%\log\` with ACLs inherited from
the data directory. The sidecar service likely runs as `NETWORK SERVICE` or a
custom service account, which won't have access to the postgres data
directory by default.

**Amendment**: Add a platform-specific note in the config example and startup
log:
- **Linux**: "Ensure sidecar user is in the postgres group. Set
  `log_file_mode = 0640` in postgresql.conf."
- **Windows**: "Grant Read permission on the log directory to the sidecar
  service account. The directory is typically `%PGDATA%\\log\\`."
- Both: The startup permission check (from 2.7) handles detection; this is
  about documenting the fix.

### 6.3 SELinux / AppArmor blocking file reads

On RHEL/CentOS with SELinux enforcing, a non-PostgreSQL process reading
files in the PostgreSQL data directory will be denied by SELinux policy even
if Unix file permissions allow it.

**Amendment**: Add a troubleshooting note: "If running on SELinux-enabled
systems, the sidecar process may need a custom SELinux policy or the log
directory must be labeled with a context readable by the sidecar. Check
`ausearch -m avc -ts recent` for denials." This is documentation, not code.

### 6.4 Relative `log_directory` paths

PostgreSQL's `log_directory` can be relative (default: `log`), in which case
it's relative to `data_directory`. The auto-detect code queries both
`SHOW log_directory` and `SHOW data_directory`, but the spec doesn't specify
the path resolution logic.

**Amendment**: Add to Phase 4: "If `log_directory` does not start with `/`
(Linux) or does not contain `:` (Windows drive letter), resolve it relative
to `data_directory` using `filepath.Join(data_directory, log_directory)`.
Always normalize the result with `filepath.Clean()`."

### 6.5 Hot reload of `logwatch.log_directory`

The spec mentions `hotReloadLogWatch()` in `config_apply.go` but doesn't
specify what happens when the log directory changes at runtime. Does the
tailer:
- Stop watching the old directory and start watching the new one?
- Require a sidecar restart?
- Validate the new directory before switching?

**Amendment**: Hot reload should validate the new directory (exists, readable,
contains files matching expected format), then atomically swap the tailer's
watched directory. If validation fails, log a warning and keep the old
directory. The watcher goroutine should accept directory changes via a channel.

### 6.6 `logwatch.enabled: true` but `rca.enabled: false`

The spec doesn't address this configuration. If RCA is disabled, log signals
have nowhere to go. `Drain()` would accumulate signals indefinitely.

**Amendment**: If `rca.enabled` is false, the LogWatcher should either:
(a) not start at all, or (b) start but discard signals from `Drain()`. Option
(a) is simpler. Document: "logwatch requires rca.enabled = true. If RCA is
disabled, logwatch is automatically disabled regardless of logwatch.enabled."

### 6.7 Fleet mode: one LogWatcher per database or one per host?

In fleet mode, pg_sage monitors multiple databases on the same host (or
multiple hosts). PostgreSQL writes ONE set of log files per cluster, not per
database. If the fleet targets 5 databases on the same cluster, should there
be 1 LogWatcher or 5?

**Amendment**: One LogWatcher per PostgreSQL cluster (host:port), not per
database. The LogWatcher's log entries contain `database_name`, so signals
can be routed to the correct per-database RCA engine. In fleet mode, the
main loop should deduplicate LogWatcher instances by resolved
`log_directory` path. Add a section addressing fleet mode.

---

## 7. Missing from Acceptance Criteria

### 7.1 No acceptance criterion for early-exit filtering (3.3)

If `log_statement = 'all'` is set, the parser must handle high volume without
excessive CPU. There should be a benchmark acceptance criterion.

**Amendment**: Add: "11. [ ] Parser handles 10,000 LOG-level lines/second
with < 5% CPU on a single core (benchmark test with `log_statement = 'all'`
traffic)."

### 7.2 No acceptance criterion for Windows

The spec says "Use filepath.Join(), test on Windows" in the risk table but
has no acceptance criterion for Windows.

**Amendment**: Add: "12. [ ] Tailer correctly follows log files on Windows
(backslash paths, NTFS ACLs, no symlink resolution issues). Tested on
Windows with both jsonlog and csvlog."

### 7.3 No acceptance criterion for the `LogSource` interface

The spec defines the interface for v0.10 pluggability but doesn't test it.

**Amendment**: Add: "13. [ ] `LogSource` interface is satisfied by a mock
implementation in tests, proving the interface is sufficient for cloud
backends."

---

## 8. Summary of Recommended Spec Amendments

| # | Section | Priority | Effort |
|---|---------|----------|--------|
| 1.1 | Signal type / package boundary | P0 | Design decision |
| 1.2 | Dedup key definition | P0 | 1 paragraph |
| 1.3 | Drain() lifecycle | P0 | 1 paragraph |
| 1.4 | temp_file threshold config | P1 | 1 line |
| 1.5 | Truncation constants | P2 | 2 lines |
| 1.6 | Source field value | P1 | 1 line |
| 2.1 | Startup file selection | P0 | 1 paragraph |
| 2.2 | logging_collector check | P1 | 1 line |
| 2.3 | External rotation / truncation | P0 | 1 paragraph |
| 2.4 | Symlink resolution | P2 | 1 line |
| 2.5 | Cold-start seek-to-end | P0 | 1 paragraph |
| 2.6 | Active file detection | P1 | 1 paragraph |
| 2.7 | Permission check on startup | P1 | 1 paragraph |
| 3.1 | csvlog multi-line buffering | P0 | 1 paragraph |
| 3.2 | log_line_prefix irrelevance | P2 | 1 line |
| 3.3 | High-volume filtering | P0 | 1 paragraph |
| 3.4 | Timezone normalization | P1 | 1 paragraph |
| 3.5 | Non-UTF-8 sanitization | P1 | 1 line |
| 3.6 | csvlog column count by PG version | P1 | 1 paragraph |
| 4.1 | Self-inflicted deadlocks | P1 | 1 paragraph |
| 4.2 | Self-inflicted timeouts | P1 | 1 paragraph |
| 4.3 | Dedup PID handling | P0 | 1 paragraph |
| 4.4 | Rate limiting | P0 | 1 paragraph |
| 4.5 | Pattern matching strategy | P0 | 1 paragraph |
| 5.1 | Temporal alignment definition | P0 | 1 paragraph |
| 5.2 | Mid-file start behavior | P1 | 1 line |
| 5.3 | Tier 2 participation | P2 | 1 line |
| 5.4 | DatabaseName for log incidents | P1 | 1 paragraph |
| 5.5 | Auto-resolve for sticky signals | P1 | 1 paragraph |
| 6.1 | Auto-detect retry on PG down | P0 | 1 paragraph |
| 6.2 | Platform permission docs | P1 | 1 paragraph |
| 6.3 | SELinux note | P2 | 1 line |
| 6.4 | Relative path resolution | P1 | 2 lines |
| 6.5 | Hot reload behavior | P1 | 1 paragraph |
| 6.6 | logwatch without RCA | P1 | 1 line |
| 6.7 | Fleet mode dedup | P0 | 1 paragraph |

**P0 count**: 12 items -- these MUST be resolved before implementation.
**P1 count**: 16 items -- these should be resolved before implementation.
**P2 count**: 5 items -- can be addressed during implementation.
