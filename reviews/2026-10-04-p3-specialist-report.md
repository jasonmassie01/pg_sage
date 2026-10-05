# P3: The Postgres specialist other agents call (roadmap phase 3)

Branch `claude/p3-specialist-api` (from `origin/master` = v1.10.0). Agent report, 2026-10-04.

## What was built

Other agents (AWS DevOps Agent, PagerDuty, Datadog, any HTTP or MCP client) can now ask
pg_sage "what is wrong with this Postgres and why" through a stable, versioned contract,
and request (never force) a remediation.

1. **Contract** (`internal/specialist`, `pg_sage.specialist.v1`). OpenAPI 3.1 document
   published at `GET /api/v1/specialist/openapi.json` (embedded `openapi.json`), version at
   `GET /contract`; `contract_version` in every response, errors included, plus the
   `X-Sage-Contract-Version` header. Routes under `/api/v1/specialist`:
   `POST /databases/{db}/investigations` (open 201 / attach 200), `GET .../{id}` (status),
   `GET .../{id}/stream` (SSE: `status` per change, then `end`; closes at 25 s, below the
   API deadline), `GET .../{id}/result` (202 while running), `POST
   .../{id}/remediations/{rid}/request`, `POST /adapters/pagerduty`, `POST /adapters/webhook`.
2. **Result** (`result.go`, `result_chain.go`, `remediation.go`): cited causal chain (root
   first, then contributing; each link's citations carry evidence id, fact text and the
   numbers read from that evidence row(s)), root cause with `source` graph/model and
   `authority` deterministic/model_earned (from W3-A's `ModelContest`), alternatives and
   ruled-out hypotheses, confidence = the graph's support score labelled `uncalibrated`
   unless the newest counting bench report gives the family's top-1 (`bench_top1`, weakest
   gated arm, Wilson lower bound), missing evidence (diagnosis, unavailable probes, missing
   probe plan, a caller window that ended before the probes ran), evidence index, typed
   remediations (cancel proposals and runway custodian actions) with predicted effect
   (quantified for cancel: waiting sessions baseline -> 0), rollback from the typed
   contracts, risk tier and the gate's *preview*. **Adapter seam for W3-B**: `Snapshot`
   (sre.Detail + proposals + caller record) -> `MapResult`; a new investigator result type
   adds a field and a mapping step, the published contract stays the same.
3. **Remediation requests** (`service_remediate.go`, cmd `specialist_custodian.go`):
   propose scope; only a concluded investigation's own candidates by id (no SQL, target or
   verdict from the caller; the body has one field, `reason`, audited only). A cancel goes
   through `ActionService.RequestExecution` (approval queue, a person approves). A custodian
   action is re-scanned now and submitted only if the custodian still proposes the same
   SQL and targets (`stale` otherwise), through the executor's `Apply` exactly like the
   custodian worker (fresh evidence time, HA replica state, no approval/owner/rollback
   flag; `requested_by`/`requested_via` in the evidence). New
   `executor.SubmitCustodianProposalDecision` reports the gate's decision (the old entry
   point delegates to it). Verdicts: `queued_for_approval`, `executed` (only where earned
   autonomy already allows it), `parked`, `blocked` (also any unknown decision: fail
   closed), `already_requested`, `not_requestable`, `stale`.
4. **Identity and scopes**: v1.10.0 MCP tokens (hashed, expiring, per database); the
   token's name is the agent identity: actor `agent:<name>:<token-id>` on the
   investigation's event chain and in `sage.specialist_requests`; dashboard panel
   "External agent requests" on the MCP tokens page (`GET /api/v1/specialist-requests`,
   operator/admin, never shows caller text). Token-only like MCP over HTTP (the session
   middleware leaves `/api/v1/specialist/` to the bearer check).
5. **Safety**: per-identity token buckets (writes 30/min, reads 240/min), bounded live
   investigations (3 per identity, 10 total; serialized check-start-record, attaching never
   counts), scope -> database -> rate limit checked before any backend call, the same 403
   for an existing and a missing database, strict bounded decoding (unknown fields refused,
   16 KiB bodies), caller text never becomes a subject/prompt (subject is a fixed
   "external request"), stored scrubbed, returned only to its caller and fenced with
   `llm.UntrustedData`; every outgoing string goes through the replay export's redaction
   (`sre.Scrub`, optional identifier hashing via `specialist.keep_identifiers: false`).
6. **Adapters**: PagerDuty v3 (bearer token as custom header AND `X-PagerDuty-Signature`,
   rotation-aware; `incident.triggered`/`reopened` of mapped services open, others 202
   ignored; idempotent per incident; result posted as an incident note via the REST API
   only when `api_url` is configured; note never contains incident text). Generic webhook
   (bearer + `X-Sage-Timestamp` + `sha256=` HMAC of `timestamp.body`, tolerance window;
   result posted signed to `result_url`). Outbound worker: durable queue in
   `sage.specialist_requests`, `FOR UPDATE SKIP LOCKED` lease, waits for the result, backoff,
   5 attempts, gives up after 2 h; unconfigured systems are marked failed, never called.
   AWS DevOps Agent / Datadog: documented (MCP server or OpenAPI HTTP action; Datadog
   workflow HTTP action or webhook). No SDKs, no live calls in tests.
7. **MCP**: `specialist_open_investigation`, `specialist_investigation_status`,
   `specialist_investigation_result` (read), `specialist_request_remediation` (propose);
   same service and limits as HTTP; new codes -32011 rate limited, -32012 too many
   investigations. `docs/mcp.md` regenerated.
8. **Schema/config/docs**: `internal/schema/specialist_migration.go` (idempotent, one
   bootstrap line), retention rule (actions_days, pending posts kept), `specialist.*` config
   (`internal/config/specialist.go`, https-only outbound URLs except loopback), regenerated
   config lifecycle doc and `config_meta.json`, `docs/specialist.md`, configuration section,
   mkdocs nav, CHANGELOG bullet under a new `## Unreleased` (released sections unchanged).

## Contract tests

- `testdata/openapi.golden.json`: the served document must equal the reviewed golden.
- `testdata/contract.v1.baseline.json` (frozen v1): a compatibility checker requires the
  current document to keep every path, operation, response code, schema and property with
  the same type/ref, every guaranteed response field, every enum value, no new required
  request field and no shrunk request limit. The checker is itself tested with 11 breaking
  mutations (all detected) and one additive change (accepted).
- Every Go response/request type serves exactly the documented properties with matching
  types and `required` = non-omitempty.

## Product calls (by the AI-DBA lens; please confirm)

1. **Opening an investigation needs only read.** Investigations only read catalog and
   statistics views; the cost is bounded by per-token rate limits and live-investigation
   caps. Requesting a remediation needs propose. (Inconsistency noted: a *person* with the
   viewer role cannot start an investigation in the UI.)
2. **A requested remediation is judged exactly like pg_sage's own initiative**: it can
   execute where earned autonomy already allows that class (same as MCP v2's
   `request_change`), otherwise it queues or is blocked. Callers can never set approval.
3. **Only pg_sage's own candidates are requestable**, by id, and only for a *concluded*
   investigation; custodian actions are re-scanned and must match exactly (stale otherwise).
4. **No family = operator triage.** Without W3-B's broad operator plan this ends
   `failed: no_probe_plan`, which the result says honestly (missing evidence "plan" tells
   the caller to name a family). PagerDuty services should be mapped with a family until
   W3-B lands. `plan_regression` without a query id cannot target one query (open item).
5. **Identifiers kept by default** (`keep_identifiers: true`): responders must be able to
   name the blocked table; secrets and PII are always removed (replay export's
   keep-identifiers mode). Operators can switch to hashed identifiers.
6. **Caller text is private to its caller** and never in notes or the audit panel.
7. **Contract on by default** (token-only; no token, no access).
8. **Window attach** only within the same family (avoids attaching a disk alert to a
   lock investigation); a window that ended >15 min before the investigation is reported as
   missing evidence rather than guessed.
9. **Remediation audit failure after the action** is logged, not returned: the request
   already reached pg_sage's own path (event chain, approval queue) and the caller must
   learn the verdict.

## Test Results

**Command:** `go test -count=1 -p 2 -cover -timeout 3000s -json ./...` (Docker
`golang:1.25`, `--cpus=2`, own PG17 `pgsage-ag8` :55478, repo root mounted, label
`owner=p3-specialist`)
**Total:** 13,375 passed, 2 failed, 22 skipped (top-level and subtests)

Failures in the full run, both resolved:
- `internal/executor TestEveryExecutionEntryPointCallsApply`: a structural test named
  `SubmitCustodianProposal` as the function calling `Apply`; after my refactor the call is
  in `SubmitCustodianProposalDecision`. The test now checks that entry point plus the
  delegation edge (invariant kept, commit explains). Passes.
- `internal/migration TestAssess_SlowReadsCutOffAndScoreIntrinsicHazard`: timing (5
  statement timeouts logged, want 4) in an untouched package; passes on rerun.

**Coverage (touched packages, PG17):**

| Package | PG17 | PG14 (-race) | PG18 (-race) |
|---|---|---|---|
| internal/specialist (new) | 87.3% | ok 87.3% | ok 87.3% |
| internal/mcp | 84.6% | ok | ok |
| internal/config | 91.5% | ok | ok |
| internal/executor | 87.8% | ok (rerun) | ok |
| internal/sre | 87.8% | ok (rerun) | ok |
| internal/schema | 84.2% | ok | ok |
| internal/retention | 87.3% | ok | ok |
| internal/store | 76.1% | ok | ok |
| internal/api | 79.4% | ok (specialist/MCP/router tests) | ok |
| cmd/pg_sage_sidecar | 79.8% | ok (specialist/MCP tests) | ok |

PG14's first `-race` run reported `[build failed]` for sre and executor (shared build cache
under concurrent agents; the same code built on PG18); both pass on rerun.

Also: `go test -tags=e2e -count=1 -timeout 900s ./e2e/` ok (239 s);
`PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$'`: 9 runs on this
branch, 6 ok and 3 with one offender, always `/api/v1/trust` latency (1.2-1.9 s vs 1 s
budget), a route this branch does not touch. Interleaved runs of `origin/master` show the
same offender (master run 2: 2085 ms; branch and master both ~80-240 ms when the VM is
quiet), so it is a pre-existing load-sensitive gate, not a regression here (worth its own
look: the Trust view under shared-VM load);
`golangci-lint run ./...` 0 issues; web `vitest` 76 files, 447 passed, 0 failed, 0 skipped
(4 new); eslint clean; dist rebuilt.

### Skipped Tests (must be zero or justified)
None in touched packages. The 22 are the pre-existing environment-gated ones: live cloud
provisioning and AgentDB gauntlets (6), Azure live parameters (2), live LLM providers (3),
container failover/restart fixtures (3), PgBouncer fixtures (3), the Windows-only path test,
the RCA child-process fixture, the plan fixture generator, `TestPGIncidentBench` and
`TestLiveModelArm`.

### Failures
None in the final runs.

### Coverage Gaps
All packages meet coverage thresholds (lowest touched: store 76.1%, api 79.4%, cmd 79.8%).

### Bugs Found This Session
1. [BUG, test] `pdEvent` built JSON with Go's `%q` (writes `\a`, not JSON) so the
   prompt-injection title test sent an unparseable event; fixed (commit explains).
2. [BUG, test] the live fixture created an all-databases token without `["*"]`
   (mcptoken's documented contract).
3. [BUG, mine] outbound "not configured" message did not match the spec'd wording.
4. [Gap] mutation run: "any terminal investigation is requestable" survived; added the test.

## Post-test audit

- **Mutation testing** (36 mutants, each applied, targeted suite run, restored): 34
  killed: scope checks (Has, approve never, propose for remediation, MCP scope table, MCP
  caller scopes), database permission, gate routing (blocked/parked/stale verdicts, policy
  blocked, manual-only, foreign proposal, running investigation), bounds (`>=`, the mutex),
  caller text as subject, both signature checks, tolerance, bearer scheme, rate limiter,
  model authority, calibration, caller-text redaction, record isolation, custodian SQL match,
  replica flag, requester evidence, session skip, audit role. Survivors: `HandedOff` case
  (equivalent: a handoff's decision is already `queue_for_approval`) and "any terminal state
  is requestable" (test added, now killed).
- **Untested inputs**: a PagerDuty payload over 256 KiB (413 path only by unit size check);
  SSE behind proxies that buffer; clock skew between sidecars for the outbound lease.
- **Assertions that could pass when broken**: the stream test's timing (100 ms state
  change, 10 ms polls) could miss an intermediate state; it asserts the count of status
  events, so a missed change would fail rather than pass.
- **Fakes hiding real failures**: unit tests use a fake backend; the real chain (MCP token
  store, sre store and coordinator, fleet directory, PG request store, handler) runs in
  `integration_db_test.go` on PG14/17/18. The cancel path's real `ActionService` and the
  custodian path's real executor are exercised in cmd tests with a real executor and stub
  gate, not end to end with a live blocker (the existing SRE action e2e covers the queue).

### Manual Checks Remaining
- CHECK-S1: MANUAL: a real PagerDuty webhook subscription and note post.
- CHECK-S2: MANUAL: "External agent requests" panel in a browser (vitest covers it).

## Open questions

1. W3-B: map its result type into `Snapshot`/`MapResult` and give `operator` a broad plan;
   until then, PagerDuty services should carry a family.
2. Should opening require propose instead of read (product call 1)?
3. `plan_regression` opens cannot name a query id (a typed `query_id` field would be
   additive to v1).
4. Calibration uses the deployment's newest counting bench report, not per-database
   history; per-database calibration would need outcome data the ledger does not keep yet.
