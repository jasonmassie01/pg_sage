# Research: AI SRE Substrate Map (what exists, what is wired, what is missing)

Date: 2026-09-26. Base: `origin/master` @ `b396595` (v1.5.0), worktree
`C:/Users/jmass/pg_sage-claude-review`. Read-only research; every claim below was checked
against code unless marked **PLAUSIBLE**. Paths are relative to `sidecar/` unless noted.

Maturity legend:
- **WIRED**: reached from `main` in production and its output is consumed.
- **PARTIAL**: reached from `main`, but part of its output is dropped, or it works only in some
  runtime modes (standalone / static fleet / meta-db).
- **TEST-ONLY**: exists, but no production caller.

Runtime modes matter throughout: *standalone* (`initStandalone`, `cmd/pg_sage_sidecar/main.go:454`),
*static fleet* (YAML `databases:`, `main.go` ~1180-1600), and *meta-db* (databases managed
from the UI/store, `cmd/pg_sage_sidecar/metadb.go`). Several incident-path components are
wired in only one or two of the three modes.

---

## 0. The ten facts that most affect the AI SRE design

1. **Incident detection runs only once per analyzer tick, which defaults to 10 minutes.** RCA
   runs inside `Analyzer.Run` (`internal/analyzer/analyzer.go:146-147,416-427`). The default is
   `DefaultAnalyzerInterval = 600s` (`internal/config/defaults.go:19`). Log signals are tailed
   every 1 s but buffered until the next tick (`rca.go:152-155`). Lock-chain and runaway
   detection also run on this tick, and so does the executor (`main.go:993`). A 60 s collector
   does not give 60 s incident response.
2. **Tier 2 (LLM correlation) cannot fire in production.** It needs ≥3 signals that no Tier 1
   tree consumed (`internal/rca/tier2.go:41-47`, default 3 at `defaults.go:146`). Every metric
   signal except `idle_in_tx_elevated` has a tree (`trees.go:25-66`), and all 18 log signals
   have trees (`log_trees.go:24-110`). So at most one signal can be uncovered. The Tier 2 e2e
   test only passes because it injects made-up signal IDs (`rca/tier2_e2e_test.go:92-111`).
3. **Incidents never page anyone.** `rcaAdapter.Analyze` throws away the returned incidents
   (`cmd/pg_sage_sidecar/rca_adapter.go:22-29`). No `notify` event type exists for incidents
   (`internal/notify/events.go:7,26,47,68,88`). The YAML alerting manager only reads
   `sage.findings` (`internal/alerting/alerting.go:14-28`). The SSE broker publishes only
   findings, actions and health (`internal/api/events.go:22-26,177-234`). The v0.9 spec §3.11
   promised `IncidentDetected`/`IncidentEscalated`, and they were never built.
4. **Manually resolving an incident is undone on the next cycle.** `POST
   /incidents/{id}/resolve` sets `resolved_at` in the DB (`internal/api/handlers_v09.go:420-438`).
   The engine's in-memory copy stays unresolved, and `PersistIncidents` upserts
   `resolved_at = EXCLUDED.resolved_at` (NULL) for every tracked incident on every cycle
   (`rca/rca.go:368-381,385-409`). The resolve reason is dropped (`handlers_v09.go:436`,
   `_ = reason`). **CONFIRMED by reading.**
5. **Incident state lives in memory only.** The engine never reads `sage.incidents` back:
   `rca` issues no SELECT. After a restart, open incidents stay open in the DB forever, and a
   recurrence gets a new UUID. The in-memory `e.incidents` slice only grows (`rca.go:256`), and
   all of it, resolved rows included, is re-upserted every cycle. `sage.incidents` is not in
   retention (`internal/retention/cleanup.go:30-44`).
6. **Nothing turns an incident into an executed action.** Incident `RecommendedSQL` and
   `cases.ActionCandidates` are built at read time for `GET /api/v1/cases`
   (`internal/api/cases_handlers.go:147-164`) and consumed nowhere else. Only `api` imports
   `internal/cases`. Manual execute requires a `finding_id` (`api/action_handlers.go:564-577`).
   The v0.9 §3.11 promise ("converted to a Finding… trust-gated execution") is not built.
7. **The LLM client cannot call tools.** It is single-turn, OpenAI-compatible
   `/chat/completions` with `{system, user}` string messages only, with no `tools`, no
   `tool_calls`, and no streaming (`internal/llm/client.go:68-99,141-260`). It already has the
   guardrails an agent loop needs: daily budget, per-DB fleet budget, circuit breaker,
   per-work-item cooldown, and a 1 MB response cap.
8. **An external agent can act through MCP but cannot investigate.** There are 12 tools:
   policy, ledger, value, guarantees, and 5 mutation intents (`internal/mcp/server.go:132-171`).
   There are no tools for incidents, findings, snapshots, logs, query stats, EXPLAIN or
   activity. The server has no resources, no prompts and no notifications (`server.go:47-58`).
9. **The UI has no working incident view.** `IncidentsPage.jsx`, the one page with a Resolve
   button, is not imported anywhere. `/incidents` routes to a read-only `CasesPage` that shows
   active items only (`web/src/App.jsx:187-190`). The API never returns `rollback_sql`
   (`handlers_v09.go:347-352`).
10. **Several value and forecast loops were built but never called.**
    `value.RecordIncident` has no production caller, so `incidents_avoided` is always 0
    (`internal/value/postgres.go:110`). `Forecaster.ForecastGrowth` has no caller, so
    `sage.size_history` is never written and `/forecasts/growth` is empty
    (`internal/forecaster/forecaster.go:90`). `query_store.plan_hash` is never written
    (`querystore/querystore.go:35-37`), so the "plan store" does not exist yet.

---

## 1. Substrate inventory

| # | Capability | Package / file:line | Maturity | Reuse for AI SRE |
|---|---|---|---|---|
| 1 | Snapshot collector (11 categories every 60 s) | `internal/collector/collector_helpers.go:69-111`, `snapshot.go:6-21`, `defaults.go:15` | WIRED | Primary time-series evidence. Investigator tools read `sage.snapshots` by category and time range. |
| 2 | Per-queryid time series (calls, total and mean time, rows) | `internal/querystore/querystore.go:27-50` (written from `collector/collector.go:112-125`) | WIRED (`plan_hash` never written; `WindowedLatencyMs` and `Prune` are TEST-ONLY per deadcode) | "Which query regressed, and when." Windowed latency comes from `WindowedLatencyMsBetween` (`executor/rollback.go:317-322`). |
| 3 | RCA Tier 1 metric signals (8) | `internal/rca/signals.go:16-39` | WIRED (10 min cadence) | Deterministic signal layer. The AI SRE should consume these, not reimplement them. |
| 4 | RCA Tier 1 decision trees | `rca/trees.go:14-66` (connections, cache, vacuum_blocked, prepared-xact trees; lag, lock and WAL are "simple") | WIRED | Deterministic first hypothesis with a causal chain and diagnostic SQL. |
| 5 | Log-based trees (18 log signals) | `rca/log_trees.go:11-110`; classifier `internal/logwatch/classifier.go:25-67` | WIRED in standalone and static fleet. Meta-db mode gets only the Supabase provider sink (`metadb.go:447-453`, no `SetLogSource`) | Log evidence (OOM, PANIC, disk full, wraparound, deadlock). |
| 6 | Cross-signal trees (log + metric; v0.9.1 §5.1, §12) | not present (`trees.go` never references `log_*`) | MISSING | Needed for "refused connections + connections_high" style reasoning. An LLM investigator could cover this. |
| 7 | Tier 2 LLM correlation | `rca/tier2.go:31-95` | PARTIAL: wired, but cannot trigger (see §0.2) | Replace with a tool-grounded investigator. Keep its prompt/parse and confidence fields as a fallback. |
| 8 | Self-action correlation (did pg_sage cause this?) | `rca/self_action.go:32-38,63-102`, `rca.go:325-362`; store `internal/store/action_queries.go:28-62` | PARTIAL: 3 of 5 causal paths can never match (`set_work_mem`, `vacuum_full`, `log_lock_timeout` path) because `categorizeAction` emits `alter` and `vacuum` (`executor/executor.go:1270-1291`); annotations (`RecentSageActions`, spec v0.9.1 §13.1) are discarded (`rca.go:348`) | Key trust feature. The AI SRE should always check "recent sage actions" as its first hypothesis. |
| 9 | Dedup, escalation, auto-resolve | `rca/rca.go:204-257,259-293,295-317` | WIRED, but in-memory only (see §0.4, §0.5) | Needs to move to a durable incident state machine. |
| 10 | `sage.incidents` table | `internal/schema/bootstrap.go:826-852`, constraint migration `schema/incident_migration.go` | WIRED (no ack/assignee/resolution fields; `related_findings` never written; `database_name` never set by RCA) | Base row for AI SRE incidents. Extend it rather than add a new table. |
| 11 | Incident REST API | `internal/api/router.go:352-364`, `handlers_v09.go:24-208,347-438` | PARTIAL (resolve is overwritten; reason dropped; `rollback_sql` not selected; `LIMIT 100`, no paging or `since`) | Read surface for UI and MCP. |
| 12 | Cases projection (incident → evidence + playbook candidates) | `internal/cases/incident_projector.go:38-125` (idle-tx and lock, runaway, connection exhaustion, WAL/replication, standby conflicts, sequence, autovacuum); `api/cases_handlers.go:147-164` | PARTIAL: view-only, recomputed per request, never executed | Existing runbook library. Each candidate has action type, risk tier, SQL, expiry, rollback class and verification plan, which is exactly the shape an investigator's remediation proposal needs. |
| 13 | Typed action contracts (≈27 action types incl. `cancel_backend`, `terminate_backend`, `diagnose_*`) | `internal/executor/action_contract.go:78-220` | WIRED for the finding-driven executor | Allowlist of actions the AI SRE may propose. Do not let the LLM emit raw SQL. |
| 14 | Executor cycle and trust ramp | `executor/executor.go:447` (`RunCycle`), called from `main.go:1017` every analyzer interval + 5 s | WIRED | Execution path. Incidents must enter it (as a finding or a queued action) to be acted on. |
| 15 | Post-action regression rollback | `executor/rollback.go:53-148` (window 15 min, `defaults.go:50`) | WIRED | Auto-revert primitive. |
| 16 | Verify engine (durable watches) | `internal/verify/engine.go`, `evaluate.go:19` (only `per_query_latency`), used by `executor/index_verification_runtime.go:134-137` | PARTIAL: index-create only | Reusable harness for a new "incident-cleared" criterion. |
| 17 | Standing-policy gate (budgets, windows, tiers, hard stops, deadlines) | `internal/policy/gate.go:17-62,200-222,247-271`; windows `policy/window.go`; leases `policy/lease.go`, `policy/postgres_lease.go` | WIRED (executor and MCP) | Every AI SRE action must go through `Gate.Authorize`. Leases stop two remediations touching the same object. |
| 18 | Evidence ledger (`sage.decision`) | `schema/ddl_agent_ledger.go:4-34`; `internal/ledger/*` | WIRED | Record the investigator's decisions here, with `evidence_id` linked to the incident. |
| 19 | Autonomy supervisor (freeze custodian, WAL/slot custodian, schema guard, self-audit) | `internal/autonomy/supervisor.go`, `types.go:51-62`; wiring `cmd/pg_sage_sidecar/autonomy_runtime.go:85-101` (interval = analyzer interval) | WIRED | Existing prevention loops. The AI SRE should read their state (`get_guarantee_status`), not duplicate them. |
| 20 | Runaway query detector and lock-chain detector | `executor/runaway_detector.go`, `analyzer/rules_lockchain.go:284-325` | WIRED (as findings, 10 min cadence) | Remediation primitives for lock storms and runaways. The cadence is too slow for incident response. |
| 21 | Notify dispatcher (Slack, email, PagerDuty via DB-configured rules) | `internal/notify/dispatcher.go:42-57`, senders `main.go:985-989` | PARTIAL: standalone and static fleet only (`main.go:768,1554`); meta-db mode has no dispatcher. No throttle or dedup, so critical findings re-dispatch every cycle (`analyzer.go:445,477-495`). PagerDuty is `trigger` only with a coarse `type:db` dedup key (`notify/pagerduty.go:83-94`) | Paging channel. Needs incident events, resolve events and per-incident dedup keys. |
| 22 | Alerting manager (YAML routes, cooldown, quiet hours) | `internal/alerting/alerting.go:12-35,76-140`, `throttle.go` | PARTIAL: standalone only (`main.go:789-803`); findings only; PagerDuty `trigger` only (`alerting/pagerduty.go:135`) | Throttle and quiet-hours logic worth reusing. Two notification systems exist; merge them before adding a third. |
| 23 | SSE event bus | `internal/api/events.go:20-27,103-119,177-275` (polls every 2 s) | WIRED | Add an `incidents` resource so the UI and investigator transcripts can stream. |
| 24 | LLM client and manager | `internal/llm/client.go:141-260`, `client_controls.go:118-190`, `manager.go:9-49` | WIRED | Transport for the investigator. Needs a tool-calling extension (§4). |
| 25 | SQL literal redaction and comment stripping | `internal/llm/sanitize.go:5-73` | WIRED (explain, advisors) | Must be applied to any log line or query sent to the LLM. Tier 2 does not do this today (**PLAUSIBLE** leak once Tier 2 becomes reachable, because log `message` and `query` are in `Signal.Metrics`; `logwatch/classifier.go:309-340`). |
| 26 | Natural-language EXPLAIN | `POST /api/v1/explain` (`router.go:373`), `internal/explain` | WIRED | Ready-made investigator tool: "explain the plan of the regressed query." |
| 27 | auto_explain plan capture | `internal/autoexplain/collector.go:230`, `provider_log.go:99` → `sage.explain_cache` | WIRED (every 300 s) | Plan evidence for slow-query incidents. |
| 28 | Forecaster (disk, connections, cache, sequences, query volume, checkpoints) | `internal/forecaster/rules.go:14-292` via `analyzer.go:366` | WIRED (findings) | Predictive incidents. |
| 29 | Storage growth forecasting and `size_history` | `forecaster/forecaster.go:90`, `growth.go:131-172` | TEST-ONLY (no caller) | Needed for disk-full prediction. |
| 30 | Provider observability (Supabase node exporter and logs) | `internal/providerobs/runtime.go:41-72` (60 s), `metrics.go:60-63` (CPU idle, MemTotal, MemAvailable only), `supabase_logs.go` | PARTIAL: Supabase only; telemetry is in-memory and used only as the executor host-load gate (`provider_observability.go:95-97`); never persisted or fed to RCA | Only host-metric source today. |
| 31 | Logwatch tailer, parser, classifier, fanout | `internal/logwatch/*`, fanout priority buffer `fanout.go:12,103-140`; wiring `main.go:707-735,1485-1508` | WIRED (standalone, static fleet) | Log evidence stream. Raw entries are available via `SubscribeEntries` (`entry_bus.go:19`), but log lines are never persisted. |
| 32 | HA role monitor | `internal/ha/ha.go:39-105`, `main.go:1006-1011` | WIRED (gates the executor only) | Failover detection exists but is not an incident or notification. |
| 33 | Fleet health score and `health_history` | `internal/fleet/manager.go:150-164,171-240` | WIRED | Score counts findings only (−25 per critical, −5 per warning), not incidents. Health history is not in retention. |
| 34 | Daily briefing (LLM summary to Slack) | `internal/briefing/briefing.go:168-270,369-404` | WIRED | Should include incidents and MTTR (v0.9 §3.11 promise, unbuilt). |
| 35 | Value, toil and incident-avoided accounting | `internal/value/service.go:40`, `postgres.go:36-126`; table `schema/ddl_agent_value.go:46-61` | PARTIAL: toil credit wired from verification (`executor/rollback.go:380`, `index_verification_runtime.go:90`); incident credit TEST-ONLY | ROI for the AI SRE (MTTR avoided, incidents prevented). |
| 36 | MCP server | `internal/mcp/server.go`, `runtime.go`; wiring `cmd/pg_sage_sidecar/mcp_runtime.go:20-60`; HTTP at `POST /api/v1/mcp` behind API auth (`router.go:156-159`) | WIRED | Agent control plane. Needs read and investigate tools (§5). |
| 37 | selfmonitor | `internal/selfmonitor/selfmonitor.go` | WIRED | Filters pg_sage's own queries out of findings. Not sidecar health monitoring (name is misleading). |
| 38 | Emergency stop and resume | `router.go:323-327`, `fleet/manager.go:243-310` | WIRED | Kill switch for the AI SRE and for any auto-remediation. |
| 39 | Prometheus `/metrics` | `main.go:2224-2425`, `value_metrics.go` | WIRED | No incident metrics (open, MTTA, MTTR). |
| 40 | Chaos and smoke tests | `internal/smoke/chaos_test.go:33-259`, `e2e/incidents.spec.ts` | test-only | Starting point for an evaluation harness. There is no labeled incident corpus. |

---

## 2. Data an investigator can read today

| Source | Where | Freshness / cadence | Granularity | Retention | Notes |
|---|---|---|---|---|---|
| `sage.snapshots` (queries, tables, indexes, foreign_keys, system, locks, sequences, replication, io, partitions, config_data) | `collector_helpers.go:80-92` | 60 s (`defaults.go:15`) | One JSON blob per category per tick. Queries are capped at the top 500 (`DefaultCollectorMaxQueries`) | 90 d (`defaults.go:91`, `cleanup.go:31`) | `system` = backends, idle-in-tx, max_conn, cache hit, deadlocks, blk times, checkpoints, db size (`snapshot.go:109-122`). `locks` = pg_locks ⋈ activity, including wait events for lock rows only. `config_data` = pg_settings (a change-detection source), connection states, churn, WAL LSN. pg_stat_io on PG16+. |
| In-memory current/previous snapshot | `collector.LatestSnapshot/PreviousSnapshot` (`analyzer.go:237-238`) | 60 s | Full structs | 2 snapshots | What RCA actually uses. Deltas cover one collector interval, while RCA runs every 10 min, so it compares the last 60 s pair, not the 10 min window. |
| `sage.query_store` | `querystore.go:27-50` | 60 s | Per queryid: cumulative calls, total/mean time, rows | 90 d (`cleanup.go:41-42`) | Windowed latency via deltas. `plan_hash` is always NULL. |
| `sage.explain_cache` | autoexplain | 300 s (`defaults.go:106`) | Plan JSON per queryid | 90 d | Plus on-demand `explain_results`. |
| `sage.findings` | analyzer | 600 s | Per rule and object | Resolved rows purged after 180 d; open rows kept | Includes forecaster, lock-chain and runaway findings. |
| `sage.incidents` | RCA | 600 s | Per correlated root cause | **Never purged** | See §3 breaks. |
| `sage.action_log`, `sage.action_queue` | executor | Per action | Before/after state, outcome, rollback | action_log 365 d; queue not in retention | Self-action correlation source. |
| `sage.decision`, `sage.verification`, `sage.change_lease` | policy, verify | Per decision | Evidence JSON, verdict | **Never purged** | Ledger. |
| `sage.health_history` | `fleet/manager.go:171-240` | ~605 s | Score and finding counts per DB | **Never purged** | Trend line. |
| `sage.alert_log`, `sage.notification_log` | alerting, notify | Per send | — | **Never purged** | Notification audit. |
| `sage.config_audit` | config API | Per change | Who changed which pg_sage key | not purged | pg_sage config changes only, not app or DB deploys. |
| `sage.size_history` | forecaster | **Never written** | — | — | See §0.10. |
| `sage.briefings` | briefing | Daily (`0 6 * * *`) | Text | not purged | — |
| Log signals (18 classified) | logwatch | Tail poll 1 s (`defaults.go:164`), 60 s classifier dedup, **drained every 600 s** | One signal per matched line (after dedup) | **Memory only**; only incidents persist | Raw lines are never stored, so an investigator cannot grep past logs. |
| Supabase logs | `providerobs/runtime_logs.go` | 60 s | Entries → classifier + auto_explain plan capture | Memory only | Supabase only. |
| Host telemetry (CPU %, mem total/avail) | `providerobs/telemetry.go:14-21` | 60 s | Instant | **Memory only** | Supabase only. DataIO/LogIO fields exist but are not populated by the Supabase parser. |
| HA role | `ha/ha.go` | ~605 s | primary/replica, flip count | Memory only | — |
| Live DB (any catalog) | pools via `fleet.DatabaseManager` | On demand | — | — | The richest source. The investigator needs a read-only, allowlisted query tool (see §5). |

**Not available at all:** OS disk free space or volume capacity (the v0.9 `disk_pressure`
signal was never built); host metrics outside Supabase; pgbouncer/pooler stats;
continuous `pg_stat_activity` / wait-event sampling (only lock rows); `pg_stat_wal`,
`pg_stat_archiver`, `pg_stat_database_conflicts`, temp-file counters; persisted raw logs;
deploy/migration events from CI or the app; application SLIs (latency, error rate);
backup/PITR status; cloud provider events (RDS/Cloud SQL maintenance, failover events).

---

## 3. Incident lifecycle today, end to end, with the breaks

```
collector (60s) ──► analyzer tick (600s) ──► rca.Engine.Analyze ──► PersistIncidents (upsert)
                         │                        │                         │
                         │ lock chain / runaway   │ log signals drained     ▼
                         ▼                        │ (buffered since last    sage.incidents
                     findings ──► notify           │  tick)                  │
                     (critical, every cycle)       ▼                         ▼
                                            Tier1 trees → Tier2 (never)   GET /incidents, /cases
                                            → self-action → dedup           (read-only UI)
                                            → autoResolve → escalate
                                            (return value DISCARDED)
```

| Stage | What happens | Break(s) |
|---|---|---|
| **Detect** | `Analyzer.Run` ticker (`analyzer.go:146-147`) → `rcaEngine.Analyze` (`analyzer.go:416-424`) → `detectSignals` (`signals.go:16-39`) + `logSource.Drain()` (`rca.go:152-155`) | **B1** latency up to 10 min, for logs too. **B18** v0.9 `query_regression` and `disk_pressure` signals were never built (spec lines 64-65). Cross-signal log+metric trees were never built. In meta-db mode no file logwatch is attached (`metadb.go:447-453`). |
| **Correlate** | Tier 1 trees (`trees.go:14`, `log_trees.go:11`) → Tier 2 (`tier2.go:31`) → self-action (`rca.go:325`) | **B2** Tier 2 unreachable. **B11** 3 of 5 self-action causal paths dead; annotations dropped. **B12** `DatabaseName` never set on RCA incidents (only copied in `self_action.go:185,215`); spec v0.9.1 §5.4 unmet. |
| **Record** | `dedup` (`rca.go:204`), `PersistIncidents` upsert (`rca.go:368-409`) | **B5** no rehydrate after restart, so zombie open rows remain. **B7** unbounded in-memory slice and table; every cycle re-upserts all historical incidents. |
| **Notify** | Nothing | **B3** no incident events in notify, alerting, SSE or Prometheus. Separately: **B14** meta-db mode has no notify dispatcher at all (`WithDispatcher` only at `main.go:772,1557`). **B15** notify has no throttle, critical findings re-page every cycle, PagerDuty is trigger-only with a coarse dedup key, and the YAML alerting manager is standalone-only. |
| **Escalate** | warning → critical after 5 occurrences (`rca.go:295-317`) | Only changes a severity string. No human escalation, no ack timer, no on-call routing. |
| **Investigate** | None beyond the tree's static causal chain and diagnostic SQL text | No investigation loop, no evidence gathering, no hypothesis ranking. |
| **Act** | Incidents → `cases.ProjectIncident` candidates, recomputed per `GET /cases` (`cases_handlers.go:147-164`) | **B8** candidates and `RecommendedSQL` are never executable: manual execute requires `finding_id` (`action_handlers.go:564-577`); the executor only consumes findings. Diagnostic `diagnose_*` contracts exist (`action_contract.go:87-111`) but nothing runs them and stores their output. |
| **Verify** | Only for index create (`verify/evaluate.go:19`) and finding-action regression rollback (`rollback.go:53`) | **B9** no "did the incident clear because of the action" check. |
| **Resolve** | Auto: signals absent for 2 cycles (~20 min, `rca.go:259-293`). Manual: `POST /resolve` | **B4** manual resolve is overwritten next cycle; reason discarded; no `resolved_by`. **B10** UI has no resolve path (`IncidentsPage.jsx` orphaned; `/incidents` → `CasesPage` shows active only). |
| **Learn / report** | Briefing, value | **B13** `incident_avoided` never written. **B16** briefing ignores incidents. No postmortem artifact. **B17** HA failover is not an incident. **B19** growth forecast is empty. |

---

## 4. LLM client: can it call tools, and how does Tier 2 prompt today

**Client capabilities** (`internal/llm/client.go`, `client_controls.go`, `manager.go`):

- Wire format: OpenAI-compatible `POST {endpoint}/chat/completions` (`client.go:190-196`), with a
  Bearer key. Any provider with an OpenAI-compatible endpoint works (Gemini's compat endpoint,
  llama-server, OpenAI, and similar).
- `ChatRequest{Model, Messages, MaxTokens, ResponseFormat}` (`client.go:68-73`).
  `ChatMessage{Role, Content string}` (`:83-86`). The response parses only
  `choices[].message.content`, `finish_reason` and `usage.total_tokens` (`:88-98`).
- **No tool/function calling**: there is no `tools` or `tool_choice` field, and no
  `tool_calls` or `role:"tool"` messages. **No streaming. No multi-turn**: `Chat(ctx, system,
  user, maxTokens)` builds exactly two messages (`:141-184`).
- Optional JSON mode (`response_format: json_object`, `:180-182`). JSON extraction and repair
  use `llm.ParseJSON`/`StripJSON` (`stripjson.go`), and `RepairTruncatedJSON` handles
  `finish_reason=length` (`:252-258`).
- Guardrails already present:
  - daily token budget with reservation and reconciliation (`client_controls.go:158-190`,
    default 500k/day at `defaults.go:57`)
  - optional external per-database fleet budget (`client.go:57-66`; wired at `main.go:1343`,
    `fleet_runtime_helpers.go:37`)
  - circuit breaker (`client.go:125-139`)
  - per-work-item cooldown keyed by hash(endpoint, model, key, system+user) (`client_controls.go:118-156`, default 300 s)
  - 1 MB response cap (`client.go:217`)
  - hot `Reconfigure` with generation fencing (`client_controls.go:93-116`)
  - General vs Optimizer client with fallback (`manager.go:26-49`).
- Token accounting is per client per day. There is no per-incident or per-investigation budget.

**Tier 2 today** (`internal/rca/tier2.go`):

- It fires only if the uncovered signal count is ≥ `llm_correlation_threshold` (default 3),
  which production cannot reach (§0.2).
- System prompt (`tier2.go:98-110`): "You are a PostgreSQL root cause analysis engine… N signals
  fired simultaneously but do not match any known deterministic pattern… return ONLY a JSON
  object: root_cause, severity (warning|critical), causal_chain (arrow notation),
  recommended_sql (array), action_risk (low|medium|high). No markdown fences."
- User prompt (`:113-125`): one line per signal, `ID=… severity=… fired_at=… metrics=<json>`.
  It includes no snapshot context, history, schema, recent actions, settings or logs beyond
  the signal metrics.
- Call: 30 s timeout, 2048 max tokens (`:22,82`). Parse: `llm.ParseJSON` (fence-tolerant). The
  result is an `Incident{Source:"llm", Confidence:0.6}` (`:25,151-176`). `recommended_sql` is
  joined with `"; "` and stored unvalidated, but never executed (§3 B8).
- Nothing records whether the LLM diagnosis was right.

**What an AI SRE needs from the client**: an additive
`ChatWithTools(ctx, messages []Message, tools []ToolSpec, opts)` that returns content or
tool_calls. Keep `Chat` unchanged for existing callers. Add a per-investigation budget via the
existing `Budgeter` interface (`client.go:57`), and a hard cap on tool-loop iterations. Tool
execution itself must happen in-process against the read-only toolset in §5. The LLM never
sees credentials and never emits executable DDL; remediation goes out as typed contract IDs.

---

## 5. MCP as an agent control plane

**Transport and protocol** (`internal/mcp/server.go:37-58`, `runtime.go:22-83`): JSON-RPC 2.0,
`protocolVersion 2025-03-26`, methods `initialize`, `tools/list` and `tools/call` only. The
`capabilities` field advertises tools only. Transports are stdio (the default,
`defaults.go:194-195`, started at `mcp_runtime.go:53-58`) or HTTP at `POST /api/v1/mcp`
behind API auth middleware (`router.go:156-159`, `200-203`). Errors collapse to generic
`internal error` (`server.go:109-114`).

**Tools today** (`server.go:132-171`):

| Tool | Kind | Behavior |
|---|---|---|
| `get_policy` | read | Standing policy for a DB. |
| `propose_policy_change` | propose | Dry-run proposal; ratification is operator-only (`api/policy_handlers.go:59`). |
| `request_change` | mutate | `intent.kind` ∈ {optimize_query, ensure_fk_indexes, apply_migration, declare_table_contract, register_consumer} (`intent_adapters.go:40-63`) → `Gate.Authorize` → execute. |
| `optimize_query`, `ensure_fk_indexes`, `apply_migration` | "concrete" | Always return `recommend_only` with candidates or a rehearsed plan (`production_backend.go:117-131`). |
| `declare_table_contract`, `register_consumer` | mutate (internal control) | Gate, then write the contract or slot registry. |
| `set_maintenance_policy` | propose | Policy patch dry-run. |
| `get_guarantee_status` | read | XID, WAL and schema invariants (custodians). |
| `get_value`, `get_ledger` | read | ROI and evidence ledger. |

**What an external agent (Claude Code, a PagerDuty AI agent, and so on) can do today**: read
policy, guarantees, value and ledger; propose policy; request 5 intent kinds under the gate.
**What it cannot do**:
- list or read incidents, findings, cases or actions
- read snapshot series, query_store deltas, locks or activity
- read logs
- run EXPLAIN (`/api/v1/explain` exists over REST only)
- ack or resolve an incident
- approve or reject a queued action (REST only, `router.go:584-634`)
- trigger emergency stop
- subscribe to events

There are no MCP `resources` (a natural fit for incidents and snapshots), no `prompts` (a
natural fit for runbooks), and no notifications.

**Implication**: build the AI SRE's tool layer once as an internal Go interface. Expose it:
1. to the in-sidecar investigator (LLM tool calling);
2. as MCP read tools, so external agents get the same evidence;
3. with every mutation still going through `request_change` and the policy gate.

This follows the agent-native spec's rule, "every tool routes through the §3 gate"
(`specs/agent-native-autonomy-build-spec.md` §10).

---

## 6. Specced vs built (incident and RCA scope)

| Spec item | Source | Built? |
|---|---|---|
| Tier 1 signals: connections, idle-tx, cache, replication lag, vacuum_blocked, lock_contention, wal_spike | v0.9 §3.2 | Yes (`signals.go`) |
| `query_regression`, `disk_pressure` signals | v0.9 §3.2 | **No** |
| Orphaned prepared-xact signal and tree (Gemini review 2.1) | rca_gemini31_review | Yes (`signals.go:272`, `trees.go:278`) |
| OOM advice reversed (decrease work_mem) (Gemini 1.2) | review | Yes (`log_trees.go:165-195`) |
| XID wraparound: diagnose xmin holders, not a blind FREEZE (Gemini 2.2) | review | Yes (`log_trees.go:196-238`) |
| Static first-detected plus sliding last-detected (Gemini 3.2) | review | Yes (`detected_at` static, `last_detected_at` sliding) |
| Priority log buffer (Gemini 3.1) | review | Yes (`logwatch/fanout.go:90-140`) |
| Incident notify events `IncidentDetected`/`Escalated` | v0.9 §3.11 | **No** |
| Briefing incident summary | v0.9 §3.11 | **No** |
| Incident → Finding → trust-gated execution | v0.9 §3.11 | **No** |
| Fleet `DatabaseName` tagging | v0.9 §3.11, v0.9.1 §5.4 | **No** (the API annotates the fleet alias at read time instead: `handlers_v09.go:338`) |
| API `?since`, `limit`, `offset` | v0.9 §3.10 | **No** (fixed `LIMIT 100`) |
| Enhanced cross-signal trees | v0.9.1 §5.1, §12 | **No** |
| Log signals in Tier 2 | v0.9.1 §2.3 item 5 | Nominally, but unreachable |
| `RecentSageActions` on every incident | v0.9.1 §13.1 | **No** (computed, then discarded) |
| Known causal paths (5) | v0.9.1 §13.2 | Partial (2 of 5 can match) |
| Anti-oscillation → `manual_review_required` | v0.9.1 §13.3 | Yes |
| Incidents-avoided credit | agent-native §4.2 | Table and repo only; **no caller** |
| MCP intent surface | agent-native §10 | Yes (12 tools as specced) |
| Alerts as side-effects of "incident avoided" events | agent-native §5 | **No** |
| C1 "NL root cause, tool-grounded (D-Bot style)" | ROADMAP_2026H2 §6 | **Not started**. Tier 2 is signal-only and unreachable. |
| C4 action justification | ROADMAP C4 | Yes (`exec.WithJustifier`, `main.go:774-776`) |
| F2 plan store / C6 plan-change narratives | ROADMAP | **No** (`plan_hash` unwritten) |

`research/v010_incidents_postmortems.md` is a catalog of public schema incidents (migration
locks, int overflow, wraparound, bloat, FK indexes, plan regressions, runaways). It is a
ready-made source for an **evaluation scenario corpus**: each entry maps to a reproducible
fault with a known root cause.

---

## 7. Gaps for an AI SRE

### 7a. Missing signals / evidence
1. **Host and OS metrics** beyond Supabase CPU and memory: disk free %, IOPS/latency, memory
   pressure, OOM-killer events. `disk_pressure` is unbuilt, and `log_disk_full` fires only
   after the outage. (Candidates: node_exporter scrape config generalizing
   `providerobs/metrics.go`; RDS/Cloud SQL CloudWatch/Monitoring adapters behind the existing
   `providerobs.API` interface, `runtime.go:13`.)
2. **Pooler visibility** (pgbouncer `SHOW POOLS/STATS`). The Gemini review names this as the
   source of 50% of connection storms. There is no code today.
3. **Wait-event sampling** (a pg_stat_activity sample every N s with wait_event, state and
   query_id). Only lock rows carry wait events today.
4. **WAL, archiver, conflicts and temp-file counters**: `pg_stat_wal`, `pg_stat_archiver`,
   `pg_stat_database_conflicts`, `pg_stat_database.temp_bytes`.
5. **Change events**: deploys and migrations from CI or the app (webhook ingest), DDL events
   (event trigger or schema-baseline diff), and pg_settings diffs, which can already be derived
   from `config_data` snapshots but nobody diffs them. Change correlation is the top RCA prior,
   and pg_sage only does it for its own actions.
6. **SLOs and application SLIs**: nothing defines "healthy" beyond thresholds. An SLO object
   (per-query p95 target or error-rate budget) would give incidents an impact measure and a
   natural resolve criterion.
7. **Persisted raw logs** (bounded, redacted ring table) so the investigator can search around
   an incident timestamp.
8. **Plan history** (`plan_hash` on query_store) for plan-flip RCA.
9. **Backup/PITR and replica health** beyond lag: last successful backup, archive lag.
10. **Cloud provider event feeds** (maintenance windows, failovers, instance-class changes).

### 7b. Missing loops
1. **Fast-path detection loop** (≤60 s) decoupled from the 10 min analyzer. At minimum, run RCA
   on each collector tick, or on any critical log signal.
2. **Durable incident state machine**: open → acknowledged → investigating → mitigating →
   monitoring → resolved/false_positive. It needs rehydrate on start, `resolved_by`, reason,
   and assignment. It fixes B4, B5 and B7 together.
3. **Investigation loop**: incident → bounded tool-calling LLM session over read-only tools →
   ranked hypotheses with cited evidence IDs → stored transcript. Tier 1 trees seed it, and
   self-action correlation is always the first hypothesis.
4. **Remediation loop**: hypothesis → typed `ActionContract` candidate (reuse `cases`
   playbooks) → `policy.Gate.Authorize` → executor → `verify` watch with an "incident-cleared"
   criterion → resolve, or revert and escalate.
5. **Notification loop**: incident events with per-incident dedup keys, `resolve` events to
   PagerDuty, throttle and quiet hours (reuse `alerting/throttle.go`), and one unified
   dispatcher across all three runtime modes.
6. **Human escalation**: ack timeout → re-page, and an on-call handoff with an investigation
   summary.
7. **Feedback and evaluation loop**: operator labels each diagnosis (correct / wrong / partial);
   measure precision, time-to-correct-hypothesis, MTTA/MTTR, and auto-action regression rate;
   run an offline replay harness over recorded snapshots and a fault-injection corpus (seeded
   from `research/v010_incidents_postmortems.md` and extending `internal/smoke/chaos_test.go`).
8. **Postmortem generation**: timeline from incident, ledger, action_log and notification_log,
   plus an LLM narrative, stored and linkable.
9. **Value credit**: call `value.RecordIncident` when a remediation demonstrably moved a signal
   back from red (the agent-native §4.2 rules).

### 7c. Missing UI
1. A working incident list and detail page (revive or merge `IncidentsPage.jsx` into Cases),
   with resolved history, ack/resolve/assign, and resolution reason.
2. An incident timeline: signals, log lines, sage actions, notifications and state changes on
   one axis.
3. An investigation transcript panel: hypotheses, tool calls and cited evidence, with links into
   snapshots, query_store and EXPLAIN.
4. Approve/reject remediation from the incident (today only the finding-bound action queue).
5. A live update via an SSE `incidents` event type.
6. A postmortem view, plus MTTR/MTTA and incidents-avoided on the Value page (the Value page
   already has the slot; the data is always 0).

### 7d. Pre-requisite bug fixes before building on this substrate
B4 (resolve overwritten), B5 (no rehydrate), B7 (unbounded growth / no retention), B2 (Tier 2
unreachable, or delete it in favor of the investigator), B3 (no notification), B11 (dead
self-action paths: align `categorizeAction` families or the `causalPaths` keys), B12
(`DatabaseName`), B14 (meta-db mode has no dispatcher or logwatch), B10 (orphaned UI page,
`rollback_sql` missing from the API).
