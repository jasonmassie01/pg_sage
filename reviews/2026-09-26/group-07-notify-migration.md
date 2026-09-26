# Group 07 — Notifications/Alerting and Schema-Migration Safety

Reviewer: G7 agent, 2026-09-26, worktree `C:/Users/jmass/pg_sage-claude-review` @ `b396595`.
READ-ONLY on source. All paths are relative to `sidecar/` unless noted otherwise.

## Scope (packages/files reviewed, LOC)

| Area | Files | Prod LOC |
|---|---|---|
| notify (DB-configured, event-driven) | `internal/notify/{config,types,dispatcher,events,slack,pagerduty,email}.go` | ~815 |
| alerting (YAML-configured, findings poller) | `internal/alerting/{alerting,throttle,channel,slack,pagerduty,webhook}.go` | ~826 |
| store + API | `internal/store/notification_{store,rules,log,validation}.go`, `internal/api/notification_handlers.go` | ~838 |
| migration advisor | `internal/migration/{advisor,classifier*,detector,log_detector,llm_fallback,llm_scripts,risk*,rules,finding,types}.go` | ~1,870 |
| migration runtime | `internal/migration/{plan,rehearsal,runtime}/*.go`, `cmd/pg_sage_sidecar/mcp_migration_runtime.go` | ~630 |
| Wiring traced | `cmd/pg_sage_sidecar/{main,wire,mcp_runtime}.go`, `internal/api/router.go`, `internal/executor/executor.go`, `internal/analyzer/analyzer.go`, `internal/retention/cleanup.go`, `internal/schema/{bootstrap,notifications}.go`, `internal/cases/projector.go`, `web/src/pages/notifications/*`, `web/src/components/Layout.jsx` | — |

**Status of the REVERSE_SPEC lead ("event-path senders never registered"): FIXED.**
`registerNotifySenders` (`cmd/pg_sage_sidecar/main.go:985-989`) is called for the standalone
dispatcher (`main.go:768-770`) and for every fleet dispatcher (`main.go:1554-1556`). The standalone
chain is now complete: `executor.dispatchEvent` (`internal/executor/executor.go:977-988`) /
`analyzer.dispatchCriticalFindings` (`internal/analyzer/analyzer.go:477-495`) → `Dispatcher.Dispatch`
→ `loadMatchingRules` → `processRule` → `SlackSender/EmailSender/PagerDutySender.Send` →
`logDelivery` INSERT into `sage.notification_log`. **However** the chain is broken in fleet mode
(G7-B05), the default rule never fires (G7-B06), critical findings storm (G7-B07), and the default
email config cannot deliver (G7-B08).

**Why two packages?** `alerting` is the original v0.x YAML feature (poll `sage.findings`, route by
severity, throttle, retry, generic webhook; standalone only). `notify` is the later DB/UI-managed
feature (channels + rules in `sage.notification_*`, event-driven from executor/analyzer, email
support, no throttle/retry/webhook). They share no code: Slack and PagerDuty are implemented twice,
webhook exists only in `alerting`, email only in `notify`. Both can fire for the same critical
finding with different dedup keys, so a user with both configured gets double Slack messages and
two PagerDuty incidents. Recommendation: converge on `notify` (G7-I01).

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G7-B01 | P0 | CONFIRMED (code path; opt-in) | `internal/migration/advisor.go:67-70`, `llm_fallback.go:35-37`, `log_detector.go:140`, `classifier_helpers.go:56-68` | Any `ALTER`/`DROP` the regex doesn't classify (e.g. `ALTER ROLE app PASSWORD 'plaintext'`, `ALTER USER MAPPING ... OPTIONS (password ...)`, `ALTER SUBSCRIPTION ... CONNECTION 'password=...'`) is sent verbatim to the external LLM |
| G7-B02 | P1 | CONFIRMED (PG ARE semantics) | `internal/migration/detector.go:51`, `risk_queries.go:38` | PostgreSQL regex `\b` is *backspace*, not a word boundary → activity-polling query never matches any DDL; `ActiveQueries` is always 0 |
| G7-B03 | P1 | CONFIRMED (computed) | `internal/migration/advisor.go:88`, `risk_score.go:37-57` | Risk formula + `<= 0.3` cut-off makes most rules unreachable: non-concurrent `CREATE INDEX` max score is exactly 0.30; `VACUUM FULL`/`CLUSTER`/`REINDEX`/`REFRESH` ≤ 0.06; `ALTER TYPE` on an idle 10M-row table = 0.28 |
| G7-B04 | P1 | CONFIRMED | `internal/migration/classifier_patterns.go:93-95` | `gen_random_uuid`, `uuid_generate_v4`, `clock_timestamp` are VOLATILE but allowlisted as safe → `ADD COLUMN id uuid DEFAULT gen_random_uuid()` (full rewrite under ACCESS EXCLUSIVE) is classified safe; tests lock the bug in |
| G7-B05 | P1 | CONFIRMED | `cmd/pg_sage_sidecar/main.go:1554-1556` vs `wire.go:52-60`, `internal/api/router.go:181,638-643` | Fleet/meta mode: UI writes channels/rules to the control pool, per-DB dispatchers read `sage.notification_rules` from each monitored DB's pool → notifications silently never fire (YAML fleet: only the primary DB's events fire) |
| G7-B06 | P1 | CONFIRMED | `web/src/pages/notifications/RulesTab.jsx:12-13`, `internal/api/notification_handlers.go:221-223`, `internal/notify/events.go:12`, `types.go:47-49` | The default rule the UI/API creates (`action_executed`, `min_severity=warning`) can never fire: `action_executed` events are always `info` |
| G7-B07 | P1 | CONFIRMED | `internal/analyzer/analyzer.go:444-445,483-494` | "Notify on new critical findings" dispatches **every** open critical finding **every** analyzer cycle (default 600 s); no newness check, no throttle → Slack/email storm, unbounded `notification_log` |
| G7-B08 | P1 | CONFIRMED (code + SMTP RFC 8314/6409) | `internal/notify/email.go:60,94`; UI default `ChannelsTab.jsx:41` | Email uses implicit TLS (`tls.Dial`) but defaults to port 587 (STARTTLS submission port) → default email channel always fails the TLS handshake; STARTTLS not supported at all |
| G7-B09 | P1 | CONFIRMED | `cmd/pg_sage_sidecar/main.go:790-803` (inside `initStandalone`, `:454`) | `alerting.enabled: true` is silently ignored in fleet mode — `alerting.Manager` is only constructed in standalone init; no warning logged |
| G7-B10 | P2 | CONFIRMED (executed scratch repro) | `internal/notify/slack.go:98-100`, `alerting/slack.go:91-93`, `alerting/webhook.go:62-64`, `notify/dispatcher.go:86,160`, `api/notification_handlers.go:180` | `*url.Error` embeds the full webhook URL (the Slack token is the URL path) → secret written to logs, `notification_log.error`, `alert_log.error_message`, returned by `GET /notifications/log` and `POST .../test`, defeating `maskChannelSecrets` |
| G7-B11 | P2 | CONFIRMED | `internal/schema/bootstrap.go:520`, `internal/retention/cleanup.go:33-34,59-78`, `internal/api/handlers.go:1729` | `sage.alert_log.finding_id REFERENCES sage.findings(id)` has no `ON DELETE` → once alerting has logged an alert for a finding, retention's batched DELETE of resolved findings fails with FK violation and aborts the whole purge |
| G7-B12 | P2 | CONFIRMED | `internal/retention/cleanup.go:30-44` | No retention for `sage.notification_log` or `sage.alert_log` (both store full bodies); combined with G7-B07 this grows by rules×channels rows per critical finding per cycle, forever |
| G7-B13 | P2 | CONFIRMED | `internal/analyzer/analyzer.go:451-464`, `internal/migration/finding.go:51` | `migration_safety` findings are never auto-resolved (category never in analyzer's `activeByCategory`) → stay `open` forever, never purged (retention only purges resolved), one row per distinct SQL hash |
| G7-B14 | P2 | CONFIRMED | `internal/notify/pagerduty.go:84-94`, `alerting/pagerduty.go:136` | notify PD `dedup_key = event_type:database` → every distinct critical finding on a DB collapses into one PD incident; neither package ever sends `resolve` → incidents stay open forever |
| G7-B15 | P2 | CONFIRMED (code), impact PLAUSIBLE | `internal/notify/slack.go:46-67`, `alerting/slack.go:134-147` | No truncation: Slack `header` plain_text >150 chars or `section` >3000 chars → 400 `invalid_blocks`; `finding_critical` body is the finding's full JSON detail → critical notifications fail |
| G7-B16 | P2 | PLAUSIBLE | `internal/notify/email.go:86-94` (+ synchronous `Dispatch` at `executor.go:984`, `analyzer.go:490`) | Email ignores `ctx`, sets no dial timeout or conn deadline; a stalled SMTP peer blocks the analyzer / executor cycle indefinitely |
| G7-B17 | P2 | CONFIRMED | `internal/migration/detector.go:45-52`, `risk_queries.go:78-104` | Fleet leak: `pg_stat_activity` and `pg_locks` are cluster-wide; no `datname = current_database()` / `l.database` filter → DDL in DB A is analyzed (against DB B's `pg_class`) and persisted as a finding by every same-cluster DB's detector; `pg_locks.relation` OIDs join across DBs. Latent until G7-B02 is fixed |
| G7-B18 | P2 | CONFIRMED | `internal/alerting/alerting.go:108,118,134` | `lastCheck` is set to `time.Now()` *after* dispatch (can take minutes with 3×10 s retries per channel per finding) and compared against DB-clock `last_seen` → findings updated inside the window and not re-bumped (e.g. migration findings) are never alerted; clock skew widens the hole |
| G7-B19 | P2 | CONFIRMED | `internal/alerting/throttle.go:75` | Quiet hours suppress **critical** alerts too, and suppressed alerts are dropped, not deferred |
| G7-B20 | P2 | CONFIRMED | `internal/store/notification_store.go:60-75` | Channel secrets (`webhook_url`, `routing_key`, `smtp_pass`) stored plaintext in `sage.notification_channels.config`, while DB passwords use `crypto.Encrypt` (`database_store.go:83`) |
| G7-B21 | P2 | CONFIRMED | `internal/store/notification_validation.go:269-303`, `api/notification_handlers.go:179-181` | SSRF/port-scan oracle: `webhook_url`/`smtp_host` unrestricted (any scheme/host incl. 169.254.169.254, RFC1918); test endpoint echoes status code / dial error. Admin-only, so P2 |
| G7-B22 | P2 | CONFIRMED | `internal/migration/runtime/postgres_rehearsal.go:40-58`, `rehearsal/orchestrator.go:87-104`, `runtime/postgres.go:71` | Rehearsal plan-regression gate is dead (`AffectedQueries` never populated → `regressed()` always false) and all measurements are discarded (`measurement` persisted as `'{}'::jsonb`) |
| G7-B23 | P2 | CONFIRMED | `internal/migration/plan/planner.go:73-81`; `sage.migration_run` has no reader | Contract phase is never executed and `migration_run` is write-only: MCP `apply_migration` reports `expanded` for `SET NOT NULL` after adding only the proof CHECK; nothing ever runs `SET NOT NULL` / attaches the UNIQUE constraint |
| G7-B24 | P2 | CONFIRMED | `internal/migration/plan/planner.go:99-104` | Planner handles only the first matching clause of a multi-clause `ALTER TABLE` and ignores the table named in the SQL (uses caller's `table`) → silently partial plan reported as success |
| G7-B25 | P2 | CONFIRMED | `cmd/pg_sage_sidecar/mcp_migration_runtime.go:71-82` | Every rehearsal error (including a genuine DDL failure on the clone, e.g. duplicate keys for the unique index) is recorded as reason `clone_unavailable` |
| G7-B26 | P2 | CONFIRMED | `internal/migration/classifier_patterns.go:19-45`, `classifier_helpers.go:56-68` | Classifier false negatives: optional `COLUMN` keyword (`ADD x ...`, `DROP x`, `ALTER x TYPE`), unnamed `ADD CHECK`/`ADD FOREIGN KEY`, quoted identifiers, `ADD PRIMARY KEY`/`ADD UNIQUE` (index build under AEL), `serial`/`IDENTITY`/`GENERATED ... STORED` columns (rewrite), leading comments and `SET lock_timeout ...; ALTER ...` batches |
| G7-B27 | P2 | CONFIRMED | `internal/migration/advisor.go:67-70`, `detector.go:45-52`, `llm_fallback.go:51,123-127` | LLM fallback fires for every DDL the regex deems *safe* (CONCURRENTLY, NOT VALID) and for pg_sage's own executor DDL (`application_name=pg_sage` not excluded); `risk_score` unclamped; prompt says "PostgreSQL version: 160004" |
| G7-B28 | P2 | PLAUSIBLE | `internal/migration/finding.go:129-137`, `internal/cases/projector.go:92-125` | Prompt injection: a DB user's DDL comment can steer the LLM `safe_alternative` to `DROP INDEX CONCURRENTLY <victim>`, which passes `executableSafeSQL` and becomes a `drop_unused_index` candidate queued for approval |
| G7-B29 | P3 | CONFIRMED | `internal/api/notification_handlers.go:285-293` | `PUT /rules/{id}` with a body missing `enabled` disables the rule (bool zero value) |
| G7-B30 | P3 | CONFIRMED | `internal/notify/email.go:169` | CRLF in `Subject` (finding titles embed identifiers; quoted identifiers may contain newlines) → header injection; no `Date`/`Message-ID` headers |
| G7-B31 | P3 | CONFIRMED | `internal/notify/slack.go:65`, `alerting/slack.go:135-140` | Slack mrkdwn not escaped (`&`, `<`, `>`): query text/comments can inject `<!channel>` or `<https://x|click>` into ops channels |
| G7-B32 | P3 | CONFIRMED | `internal/alerting/slack.go:98`, `pagerduty.go:104`, `throttle.go:14,46,58` | Retries on permanent 4xx; invalid timezone silently → UTC; quiet-hour minutes ignored ("22:30" → 22:00); `sent` map never evicted, in-memory only (restart re-alerts everything) |
| G7-B33 | P3 | CONFIRMED | `internal/migration/rules.go:117`, `risk_score.go:5-18` | `ddl_missing_lock_timeout` has `LockLevel ""` → weight 0 → score 0 → never surfaces |
| G7-B34 | P3 | CONFIRMED | `internal/migration/rules.go:28` (`SafeAltTemplate: "CREATE INDEX CONCURRENTLY %s"`), `finding.go:132` | Template placeholder never formatted; literal `CREATE INDEX CONCURRENTLY %s` passes `executableSafeSQL` into `recommended_sql` |
| G7-B35 | P3 | CONFIRMED | `internal/migration/rules.go:73-77`, `classifier_match.go:58-63` | Rule text wrong ("volatile DEFAULT rewrites table on PG < 11" — it rewrites on all versions); `ALTER TYPE` flagged for binary-coercible changes (varchar(n)→varchar(m≥n), varchar→text) |
| G7-B36 | P3 | CONFIRMED | `internal/migration/runtime/postgres.go:84-92`, `rehearsal/orchestrator.go:100` | `contract_not_before` treats cycles as minutes; `regressed()` second clause is redundant so a plan-hash flip alone is ignored |
| G7-B37 | P3 | CONFIRMED | `internal/store/notification_store.go:227-237` | Test notifications are logged as event `action_executed` (pollutes log); dead `sender` variable |
| G7-B38 | P3 | PLAUSIBLE | `internal/store/migration_safety_findings.go:34-41` | Select-then-insert upsert races between activity and log detectors → unique-index violation logged |

### G7-B01 — Credentials in DDL are shipped to the external LLM (P0, opt-in)
- **Scenario:** `migration.enabled: true`, `migration.log_detection: true`, LLM enabled, and the
  common audit setting `log_statement = 'ddl'`. An operator rotates a password:
  `ALTER ROLE app PASSWORD 'S3cret!'`. PostgreSQL logs the plaintext statement; logwatch hands it to
  `LogDetector.processLogEntry`; `isDDLKeyword` accepts any `ALTER `; the regex classifier returns no
  rule; `Advisor.Analyze` calls `llmFallback`, which embeds the raw SQL in the user prompt
  (`llm_fallback.go:35`) and sends it to the configured provider. Same for `ALTER USER MAPPING ...
  OPTIONS (password ...)`, `ALTER SUBSCRIPTION ... CONNECTION 'password=...'`, `ALTER SERVER`.
  If the LLM returns `risk_score > 0.3` the plaintext is also persisted in `sage.findings.detail`.
- **Root cause:** fallback scope is "anything starting with ALTER/DROP" instead of "table-level DDL";
  no secret redaction before egress.
- **Fix:** (1) only fall back for `ALTER TABLE|ALTER INDEX|DROP TABLE|DROP INDEX|CREATE INDEX|
  REINDEX|CLUSTER|VACUUM|REFRESH` statements; (2) run the statement through a redactor
  (`PASSWORD '...'`, `password=...`, string literals → `'***'`) before prompting and before persisting.
- **Test:** `TestLLMFallback_NeverSendsRoleDDL` with a recording fake LLM client: feed
  `ALTER ROLE x PASSWORD 'p'` and `ALTER USER MAPPING ... password 'p'`; assert zero `Chat` calls and
  that no persisted detail contains `p`.

### G7-B02 — Activity polling can never detect DDL (PG regex `\b`)
- **Scenario:** a developer runs `ALTER TABLE orders ALTER COLUMN total TYPE numeric(12,2)` for
  minutes. `ddlActivitySQL` filters `query ~* '^\s*(ALTER|...)\b'`. In PostgreSQL ARE syntax `\b`
  is a backspace character-entry escape (word boundary is `\y`/`\m`/`\M`), so the predicate
  requires a literal backspace after the keyword and matches nothing. `PollOnce` always returns zero
  rows. Same bug in `fetchActiveQueries` (`risk_queries.go:38`): `\borders\b` → `ActiveQueries`
  always 0, removing 30% of the risk formula. The team already knows this elsewhere:
  `internal/schema/lint/llm_jsonb.go:139` uses `\m ... \M`.
- **Only integration test** (`integration_test.go:279`) asserts the *no-DDL* case, so it passes.
- **Fix:** `\y` in both places (or `[[:>:]]`), and escape the table name
  (`regexp.QuoteMeta`-equivalent for ARE) before building the pattern.
- **Test:** integration test that starts `SELECT pg_sleep(3)` inside a session holding
  `ALTER TABLE t ADD COLUMN c int DEFAULT random()` (use `LOCK` + second conn to hold it active),
  then asserts `PollOnce` returns ≥1 incident. Quick manual check:
  `SELECT 'ALTER TABLE t' ~* '^\s*(ALTER)\b';` → `false`.

### G7-B03 — Risk threshold suppresses most rules
- **Scenario (computed with a copy of `computeRiskScore`):**

  | Case | rows | score |
  |---|---|---|
  | `CREATE INDEX` (SHARE 0.5 × 0.6), every factor maxed | any | **0.300** (cut-off is `<= 0.3`) |
  | `ALTER TYPE` rewrite, idle | 1e7 | 0.280 |
  | `ALTER TYPE` rewrite, idle | 1e8 | 0.320 |
  | `SET NOT NULL`, idle | 1e9 | 0.216 |
  | `ADD FOREIGN KEY`, idle | 1e9 | 0.151 |
  | `VACUUM FULL` / `CLUSTER` / `REINDEX` / `REFRESH` (no table extracted) | — | ≤ 0.060 |

  Combined with G7-B02 (activity factor always 0), the advisor only fires for rewrites on tables
  >~30M rows or when a lock queue is already forming — i.e. after the outage started.
- **Root cause:** multiplicative formula `base × max(0.1, combined)` where `combined` is dominated by
  row count; maintenance matchers never fill `TableName` (`classifier_match.go:158-183`).
- **Fix:** make the rule's intrinsic hazard the floor (e.g. `score = max(base, base × combined)` or
  severity from rule + escalate with size/activity); extract table/index names for maintenance
  statements; honour `migration.ddl_row_threshold` (currently unconsumed, G7-D08) as the "small
  table" cut instead of the global 0.3.
- **Test:** table-driven `TestAdvisor_ReportsEveryRuleOnLargeTable` asserting each rule ID yields a
  non-nil incident for a 5M-row table with zero activity.

### G7-B04 — Volatile defaults treated as fast defaults
- **Scenario:** `ALTER TABLE events ADD COLUMN id uuid DEFAULT gen_random_uuid()` on a 200 GB table.
  PostgreSQL rewrites the table under ACCESS EXCLUSIVE because the default is volatile
  (`pg_proc.provolatile = 'v'`). `isVolatileDefault` returns false because the allowlist
  (`classifier_patterns.go:90-101`) treats `gen_random_uuid`, `uuid_generate_v4` and
  `clock_timestamp` as "immutable/stable". `classifier_test.go:138` and `risk_test.go:187` assert the
  wrong behaviour.
- **Fix:** allowlist only STABLE/IMMUTABLE (`now`, `current_timestamp`, `transaction_timestamp`,
  `statement_timestamp`, `current_date`, `localtimestamp`, ...); treat unknown functions as volatile
  (already the case). Better: resolve `provolatile` from `pg_proc` via the pool.
- **Test:** invert the two tests; add `clock_timestamp()` and `uuid_generate_v4()` positive cases.

### G7-B05 — Fleet notifications read rules from the wrong database
- **Scenario:** meta-DB fleet with databases `orders` and `billing`. Admin creates a Slack channel
  and a `finding_critical` rule in the UI → rows land in the control DB (`authPool`,
  `wire.go:52-60` → `router.go:181`). `billing` raises a critical finding; its dispatcher
  (`main.go:1554`) queries `sage.notification_rules` **in `billing`** → zero rows → nothing sent,
  nothing logged. YAML fleet: `authPool` is the primary DB's pool, so only the primary's events fire.
- **Fix:** construct fleet dispatchers with the control pool (`metaState.Pool` or
  `fleetMgr.PoolForDatabase("all")`) for rule/channel/log access, and keep `databaseName` in the
  event (already present) for routing; optionally add `database` to `notification_rules`.
- **Test:** wire-level unit test asserting the pool handed to `notify.NewDispatcher` in
  `initFleetMultiDB` equals the API's auth pool; integration test with two DBs.

### G7-B06 — The default rule can never fire
- **Scenario:** UI "Add rule" defaults to `event=action_executed`, `min_severity=warning`
  (`RulesTab.jsx:12-13`; API default `warning` at `notification_handlers.go:221-223`; DB default
  `'warning'` in `schema/notifications.go:21`). `ActionExecutedEvent` has fixed severity `info`
  (`events.go:12`); `SeverityMeetsMin("info","warning")` is false. Users believe they subscribed.
- **Root cause:** severity is a function of event type, so per-rule `min_severity` is mostly a trap.
- **Fix:** default `min_severity` to `info`; reject (400) a rule whose event type's fixed severity
  is below `min_severity`, or drop `min_severity` for fixed-severity events.
- **Test:** `TestCreateRule_RejectsUnreachableSeverity`; UI test for default `info`.

### G7-B07 — Critical findings re-notify every cycle
- **Scenario:** a persistent critical finding (e.g. XID wraparound risk) exists for 3 days. Every
  analyzer cycle (600 s) `dispatchCriticalFindings(allFindings)` re-sends it: 432 Slack messages and
  432 emails per rule, 432 `notification_log` rows. The comment says "new" but no newness check
  exists; notify has no throttle.
- **Fix:** dispatch only findings whose `occurrence_count == 1` / `created_at` within this cycle, or
  on severity escalation; add a per-(event, database, object) cooldown in `Dispatcher` (reuse
  `alerting.Throttle`, G7-I01).
- **Test:** analyzer unit test with a fake dispatcher: two cycles with the same critical finding →
  exactly one `Dispatch` call.

### G7-B08 — Default email config cannot deliver
- **Scenario:** admin adds an email channel with host `smtp.gmail.com`, UI default port 587. Port 587
  speaks plaintext then STARTTLS; `tls.Dial` sends a ClientHello to a plaintext greeting →
  `tls: first record does not look like a TLS handshake`. Only port 465 (implicit TLS) works.
- **Fix:** if port is 465 use implicit TLS; otherwise dial plain with a `net.Dialer{Timeout}`,
  `smtp.NewClient`, require `client.StartTLS` (fail closed if not advertised). Set conn deadlines
  from `ctx` (also fixes G7-B16).
- **Test:** `smtpmock`-style test server that advertises STARTTLS on a plain listener; assert
  delivery succeeds and that a server without STARTTLS is rejected.

### G7-B09 — Alerting silently disabled in fleet mode
- **Scenario:** fleet YAML with `alerting.enabled: true`, routes and Slack URL. `initFleetMultiDB`
  never constructs `alerting.Manager`; no log line. `/api/v1/alert-log` (fleet-aware, reads every
  pool) shows an empty table.
- **Fix:** either start one Manager per fleet instance (with `DatabaseName` in the payload) or, per
  G7-I01, retire alerting in favour of notify. At minimum `logWarn` at startup.
- **Test:** startup test asserting a warning or a running manager when fleet + alerting enabled.

### G7-B10 — Webhook secrets leak through error strings
- **Scenario:** Slack is briefly unreachable. `http.Client.Do` returns
  `Post "https://hooks.slack.com/services/T0/B0/<token>": dial tcp ...` (reproduced with a scratch
  program: the full path is included; Go only redacts userinfo passwords). The string goes to the
  structured log, `notification_log.error`, `alert_log.error_message`, `GET /notifications/log`,
  and the `POST /channels/{id}/test` response body.
- **Fix:** in each sender, unwrap `*url.Error` and replace `.URL` with a redacted form
  (scheme+host only) before wrapping; never return raw sender errors from the test endpoint.
- **Test:** sender test against a closed port asserting the error string does not contain the path.

### G7-B11 — `alert_log` FK blocks findings retention
- **Scenario:** alerting sent an alert for finding 42; it resolves; after `findings_days` retention
  runs `DELETE FROM sage.findings WHERE ctid IN (... LIMIT 1000)`. The FK
  `alert_log.finding_id → findings(id)` is `NO ACTION` → the statement errors, `purgeTable` logs and
  returns → no resolved findings are ever purged again while that row exists.
- **Fix:** `ON DELETE SET NULL` (migration: drop/re-add constraint) and add `alert_log` to retention.
- **Test:** retention integration test: insert finding + alert_log row, resolve, age it, run
  `Cleaner.Run`, assert finding deleted and alert_log row kept with NULL `finding_id`.

### G7-B12 / G7-B13 — Unbounded tables
- `notification_log`, `alert_log`: add to `Cleaner.Run` with an `actions_days`-like window.
- `migration_safety` findings: resolve when the DDL is no longer active / after N hours without
  re-detection (e.g. `last_seen < now() - interval '24 hours'`), so retention can purge them.
- **Test:** retention unit test enumerating every `sage.*` table with a timestamp column and failing
  when one lacks a purge rule (guards future tables too).

### G7-B14 — PagerDuty dedup and lifecycle
- notify: include the finding's object identifier (or a hash of the subject) in `dedup_key`.
- Both: send `event_action: "resolve"` when a finding resolves / action succeeds after failure.
- **Test:** payload test: two critical findings on one DB → two distinct dedup keys.

### G7-B15 — Slack block limits
- Truncate header to 150 and section text to 3000 (rune-safe, with "…"), and move large JSON
  detail into a link to the finding page instead of the message body.
- **Test:** 5 KB body → payload blocks within limits.

### G7-B16 — Email can hang the analyzer/executor
- `sendEmail(_ context.Context, ...)` uses `tls.Dial` (no timeout) and never sets deadlines; the
  dispatcher is called synchronously from both loops. Fix: `net.Dialer{Timeout}` +
  `conn.SetDeadline(deadlineFrom(ctx, 30s))`; better, make `Dispatch` enqueue to a bounded worker.
- **Test:** fake SMTP listener that accepts and never writes; assert `Send` returns within timeout.

### G7-B17 — Migration detector fleet leak
- Add `AND datname = current_database()` to `ddlActivitySQL` and `activeQueriesSQL`, and
  `AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())` to
  `pendingLocksSQL`. Exclude `application_name = 'pg_sage'` (also part of G7-B27).
- **Test:** integration with two DBs on one cluster: DDL in A produces a finding only in A.

### G7-B18 / G7-B19 — Alerting window and quiet hours
- Capture `now()` from the DB **before** querying and store that as `lastCheck`; or better, track
  `alert_log` per finding and query "open findings with no alert since cooldown".
- Let `critical` bypass quiet hours (configurable), and queue suppressed alerts for delivery at
  quiet-hours end.
- **Test:** fake clock test: finding updated during a slow dispatch is alerted next evaluate.

### G7-B20 / G7-B21 — Secrets at rest and SSRF
- Encrypt `notification_channels.config` secret keys with the existing `crypto` package and
  `encryption_key` (same as `database_store.go`).
- Validate `webhook_url`: `https` only; for `slack` require host `hooks.slack.com`; for generic
  webhooks use a dialer `Control` hook that rejects loopback, link-local (169.254/16, fe80::/10) and
  (configurably) RFC1918. Return a generic "delivery failed" from the test endpoint.
- **Test:** validation unit tests for `http://169.254.169.254/...`, `file://`, `http://127.0.0.1`.

### G7-B22 / G7-B23 / G7-B24 / G7-B25 — Migration runtime half-built
- B22: have `PostgresRehearsalRunner` capture top-N `pg_stat_statements` queries touching the table,
  EXPLAIN them before/after on the clone, fill `AffectedQueries`, and persist `Measurement` as JSON
  in `migration_run.measurement`. Fix `regressed()` to treat a plan-hash change with any latency
  increase above a lower threshold as regression.
- B23: either implement a continuation worker that reads `migration_run` rows past
  `contract_not_before`, re-verifies proofs, and gates/applies `ContractSteps`, or return verdict
  `expanded_contract_pending` with the contract SQL so the agent/user can finish it.
- B24: parse all clauses (or reject statements with more than one sub-command) and assert the SQL's
  target table equals `request.Table`.
- B25: distinguish provider errors (`clone_unavailable`) from step failures (`rehearsal_failed`,
  carrying the SQLSTATE).
- **Tests:** runtime unit tests with fakes for each; `planner_test` case for
  `ALTER TABLE t ALTER COLUMN a SET NOT NULL, ALTER COLUMN b SET NOT NULL` → error or two plans.

### G7-B26 — Classifier false negatives
- Make `COLUMN` optional in `reAddColumn*`, `reDropColumn`, `reAlterType`; allow
  `ADD (CONSTRAINT ident)? CHECK|FOREIGN KEY|PRIMARY KEY|UNIQUE`; accept `"quoted"` identifiers
  (`(?:"[^"]+"|\w+)`); add rules for `serial/bigserial/GENERATED ... AS IDENTITY/STORED` columns and
  `ADD PRIMARY KEY/UNIQUE` without `USING INDEX`; strip leading `/* */` and `--` comments and split
  on top-level `;` before classifying. Long term: `pg_query_go` behind the existing `SQLParser`
  interface (`types.go:19-21`).
- **Test:** table-driven cases for each form above.

### G7-B27 / G7-B28 — LLM fallback scope and trust
- Only call the LLM when the statement is table-level DDL **and** no deterministic rule matched
  **and** it is not provably safe (CONCURRENTLY / NOT VALID / VALIDATE); skip backends with
  `application_name = 'pg_sage'`; clamp `risk_score` to [0,1]; render the version as `major.minor`.
- Never copy LLM `safe_alternative` into `recommended_sql` as executable; keep it as prose in
  `detail` and only let deterministic templates populate `recommended_sql`.
- **Test:** fake LLM returning `safe_alternative: "DROP INDEX CONCURRENTLY users_pkey"` → finding
  `recommended_sql == ""`.

## B. Dead / unwired / half-built

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G7-D01 | `internal/alerting/alerting.go:73` | `Manager.Throttle()` test accessor | TEST-ONLY → move to `export_test.go` | Only tests use it |
| G7-D02 | `internal/alerting/throttle.go:104` | `Throttle.IsQuietHours` | TEST-ONLY → `export_test.go` | Quiet hours **are** effective: `ShouldAlert` calls unexported `isQuietHours` (`throttle.go:75`); the exported wrapper is test-only |
| G7-D03 | `internal/alerting/throttle.go:123` | `Throttle.Reset` | TEST-ONLY → `export_test.go` | Test helper |
| G7-D04 | `internal/migration/llm_fallback.go:146` | `stripToJSONObject` | DELETE | `parseDDLLLMResponse` uses `llm.ParseJSON`; markdown-fenced JSON is handled there |
| G7-D05 | `internal/migration/log_detector.go:39` | `LogDetector.ProcessLogEntry` | DELETE | Log detection **is live**: `Run → drain → processLogEntry` wired at `main.go:866-880` (standalone) and `:1605-1615` (fleet). Tests can call `processLogEntry` |
| G7-D06 | `internal/migration/classifier_match.go:77,91,150`; rule `ddl_add_column_not_null` | PG<11 / PG<12 branches | DELETE | Startup requires PG14+ (`startup/checks.go:103`) and the value passed is `server_version_num` (140000+), not a major version — the branches are unreachable (and would be wrong if reached) |
| G7-D07 | `internal/migration/llm_scripts.go:298-301` | `looksLikeSQL` branch returns `s` either way | DELETE (or reject prose) | No-op branch |
| G7-D08 | `internal/config/config.go:400,401,405`; consumers only `api/config_apply.go:513,521` | `migration.mode`, `migration.managed_service`, `migration.ddl_row_threshold` | WIRE `ddl_row_threshold` (G7-B03); WIRE or DELETE `managed_service` (filter pg_repack advice on RDS/Cloud SQL); keep `mode` warning | Config keys with no runtime consumer |
| G7-D09 | `internal/migration/rehearsal/orchestrator.go:215-218,263-273` | `RegressionPct`, `AffectedQueries` | WIRE | Never populated in prod (G7-B22) |
| G7-D10 | `internal/migration/plan/planner.go:73-81`; `sage.migration_run` | `StepsForCycle`, `ContractSteps`, `migration_run` | WIRE (continuation) or DELETE contract fields | Write-only table, contract never applied (G7-B23) |
| G7-D11 | `internal/alerting/{slack,pagerduty,webhook}.go` vs `internal/notify/{slack,pagerduty}.go` | Two parallel notification stacks | DELETE alerting senders after porting throttle/retry/webhook into notify | Duplicate implementations with divergent behaviour (G7-I01) |
| G7-D12 | `internal/api/router.go:702-710` vs `cmd/pg_sage_sidecar/main.go:985-989` | Duplicate sender registration helpers | DELETE one (add `notify.NewDefaultDispatcher`) | Two lists to keep in sync |
| G7-D13 | `web/src/components/Layout.jsx` (no entry); `App.jsx:184,197` | `/notifications` and `/alerts` pages have no nav link | WIRE | Reachable only by typing the URL |
| G7-D14 | `web/src/pages/notifications/shared.jsx:13-18` | UI event list omits `query_rewrite_suggested` | WIRE | Event is emitted (`analyzer.go:515`) and valid server-side (`types.go:37`) but UI users cannot subscribe |
| G7-D15 | `internal/store/notification_store.go:235-237`, `notification_validation.go:94-104` | dead `sender` var; trivial `sendTestDirect` wrapper | DELETE | Hygiene |
| G7-D16 | `internal/migration/rules.go:117` | `ddl_missing_lock_timeout` | WIRE as an annotation on the top incident | Always scores 0, never surfaces (G7-B33) |
| G7-D17 | `internal/notify/types.go:31-37` | Event catalog has 5 types | WIRE more events | No events for emergency stop, rollback, RCA incident opened, migration-safety detected, verification failed, forecast breach |

## C. Feature improvements (ranked by impact × effort)

### Notifications / alerting (current: two stacks, UI stack event-driven but storm-prone, YAML stack standalone-only)
1. **G7-I01 (High×M) Converge on `notify`.** Port `alerting.Throttle` (cooldown, escalation,
   quiet hours with critical bypass), retry with backoff on 5xx/429 only, and the generic webhook
   sender into `notify`; add a `Dispatcher` cooldown keyed by (event, database, object). Make the
   YAML `alerting:` block a seed for DB channels/rules on first boot, then delete `alerting`.
   Fixes G7-B07/B09/B14/B19 structurally.
2. **G7-I02 (High×S) Async bounded delivery queue.** `Dispatch` enqueues to a per-dispatcher worker
   with a bounded channel and per-send timeout; analyzer/executor never block on I/O (G7-B16).
3. **G7-I03 (High×S) Fleet-correct dispatch.** One control-plane dispatcher shared by all instances,
   rule filter by `database` (nullable = all). (G7-B05)
4. **G7-I04 (Med×S) Secret hygiene.** Encrypt channel secrets, redact URLs in errors, SSRF-safe
   dialer, generic test-endpoint errors. (G7-B10/B20/B21)
5. **G7-I05 (Med×S) Resolve lifecycle.** Send PagerDuty `resolve` and Slack "resolved" follow-ups
   when findings resolve / actions succeed; include a deep link to the finding/case page.
6. **G7-I06 (Med×M) Richer event catalog** (G7-D17), each with a documented fixed severity; rule
   editor shows only meaningful severities per event.
7. **G7-I07 (Low×S) Retention** for `notification_log`/`alert_log`, `ON DELETE SET NULL` FK. (B11/B12)
8. **G7-I08 (Low×S) UI:** nav entries, `query_rewrite_suggested`, delivery-failure badge, "send test"
   showing a redacted error.

### Migration safety advisor (current: activity path dead, scoring suppresses most rules, LLM over-reach)
1. **G7-I09 (High×S) Make detection work:** `\y` fix, `datname` filter, exclude pg_sage backends
   (G7-B02/B17/B27). Add an integration test that actually runs a blocking DDL.
2. **G7-I10 (High×S) Rule-first scoring:** each rule has an intrinsic severity; size/activity/lock
   queue only escalate. Use `ddl_row_threshold` for "small table, downgrade to info". (G7-B03)
3. **G7-I11 (High×S) Correct volatility:** consult `pg_proc.provolatile` for default expressions
   instead of a hand allowlist. (G7-B04)
4. **G7-I12 (High×M) Replace regex with `pg_query_go`** behind `SQLParser` — eliminates G7-B26
   wholesale and lets lock levels be mapped per sub-command (the correct lock is the strongest
   across sub-commands).
5. **G7-I13 (Med×S) Real-time signal:** emit a `migration_risk` notify event when a live DDL scores
   critical — today a dangerous live DDL only produces a DB row and a log line.
6. **G7-I14 (Med×S) Lock-queue watchdog:** when a DDL is waiting on a lock and `PendingLocks` behind
   it grows, surface "cancel the DDL" guidance (`pg_cancel_backend(pid)`), gated by trust level. This
   is the actual outage mechanism and the detector already has the pid.
7. **G7-I15 (Med×S) LLM hardening:** scope + redaction + clamp + never executable (G7-B01/B27/B28).
8. **G7-I16 (Low×S) Auto-resolve** migration findings after the pid disappears / 24 h. (G7-B13)

### Migration runtime (MCP `apply_migration`, rehearsal on clone)
1. **G7-I17 (High×M) Finish the loop:** persist measurement, populate `AffectedQueries` via
   before/after EXPLAIN on the clone, implement contract continuation or return the pending
   contract SQL explicitly. (G7-B22/B23)
2. **G7-I18 (Med×S) Clone identity guard:** before running any step, assert the rehearsal target is
   not the production endpoint (compare `inet_server_addr()/port` + `pg_postmaster_start_time()`
   against the target pool; DLE clones share `system_identifier`, so that alone is insufficient).
3. **G7-I19 (Med×S) Budget checks:** compare rehearsal step durations to a max-lock budget
   (`MaxLockDuration` is measured but never evaluated) and park when exceeded; enforce that
   production `statement_timeout` (10 min) exceeds the rehearsed duration with margin, otherwise a
   `CREATE UNIQUE INDEX CONCURRENTLY` will time out in prod and leave an INVALID index.
4. **G7-I20 (Low×S) Honest reasons** (G7-B25) and single-clause validation (G7-B24).

## D. Questions the user isn't asking

1. **Do you want two alerting systems?** Today a critical finding can page twice (alerting and
   notify) with different dedup keys, while fleet users get neither. Which is the product?
2. **What data may leave the box?** Notifications ship full SQL, finding JSON and query text to
   Slack/PagerDuty/email; the migration LLM path ships raw DDL (including literals and passwords).
   Is there a documented egress policy, and should redaction be mandatory before any external call?
3. **Has anyone ever seen activity-polling fire in a real environment?** The `\b` bug means no.
   What other raw PG regexes exist that were only tested against "no match" cases?
4. **What is the SLO for "dangerous DDL detected → human notified"?** There is currently no notify
   event for migration risk at all; detection lands in `sage.findings` and a log line.
5. **Should pg_sage ever cancel a DDL that is stalling a lock queue?** That is the highest-value
   intervention in this area and fits the trust ramp (advisory → approve → auto with lock budget).
6. **Is the rehearsal clone guaranteed not to be production?** The only protection is whatever DSN
   the DLE/snapshot provider returns; no identity check exists in `PostgresRehearsalRunner`.
7. **Who owns the contract phase of an online migration?** MCP reports `expanded`; nothing
   finishes `SET NOT NULL`/`USING INDEX`, and nothing reads `sage.migration_run`.
8. **Quiet hours vs. critical pages:** should a wraparound-risk page really be muted at 02:00?
9. **Retention coverage:** is there a test that every `sage.*` table with a timestamp has a purge
   rule? `notification_log`, `alert_log`, and open `migration_safety` findings all grow unbounded.
10. **Does anyone use the `email` channel successfully?** With the 587/implicit-TLS mismatch it
    cannot work with Gmail/SES/Office365 submission ports; if nobody reported it, email may be unused.

## E. Verification notes

- Ran `go vet ./internal/notify/ ./internal/alerting/ ./internal/migration/...` — clean.
- Ran `go test -count=1 -cover` for the same packages — all pass. Coverage: notify 65.0%,
  alerting 69.8%, migration 63.1%, migration/plan 97.4%, migration/rehearsal 89.7%,
  migration/runtime 29.2%. **52 tests SKIPPED** (notify 21, alerting 12, migration 16,
  migration/runtime 3) — all require a live test DB (`127.0.0.1:1` sentinel), not run per brief.
  notify, alerting (69.8% < 70%), migration and migration/runtime are below the 70% business-logic
  floor without the DB-backed tests.
- Executed a scratch program (outside the repo) replicating `computeRiskScore` to produce the
  G7-B03 table, and a scratch HTTP client call to confirm `*url.Error` includes the full URL path
  (G7-B10).
- **Not executed** (per brief: no local Postgres/Docker/servers): G7-B02 (`\b` semantics — based on
  PostgreSQL ARE docs; one-line check `SELECT 'ALTER TABLE t' ~* '^\s*(ALTER)\b'` should return
  `false`), G7-B11 (FK violation), G7-B08 (live SMTP), G7-B15 (live Slack limits: header 150 /
  section 3000 chars per Block Kit reference), G7-B16/B18 timing behaviour.
- Did not review executor internals (`ExecConcurrently` invalid-index cleanup, policy gate,
  multi-statement rejection) beyond confirming `executor/validate.go:351-360` rejects
  multi-statement SQL, which contains G7-B28's blast radius. Config-controller lifecycle for
  `alerting.*` keys: correctly reported as restart-required (no reconfiguration owner registered;
  `config/controller.go:137-147`) — not a bug, but the UI should say so.
