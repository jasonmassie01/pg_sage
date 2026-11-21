# The Postgres specialist other agents call

pg_sage serves a stable, versioned investigation contract,
`pg_sage.specialist.v1`, so that other agents (AWS DevOps Agent, PagerDuty,
Datadog, your own automation) can ask it **what is wrong with this Postgres
and why**, and **request** (never force) a remediation.

- Contract document (OpenAPI 3.1, no token needed):
  `GET /api/v1/specialist/openapi.json`; version: `GET /api/v1/specialist/contract`.
- Every response, errors included, carries `contract_version` and the header
  `X-Sage-Contract-Version`. A breaking change is a new version (`v2`) served
  beside `v1`, never an edit of `v1`. Clients must ignore unknown fields and
  treat unknown enum values as unknown; pg_sage only adds fields and values
  within `v1` (a golden test enforces this against the frozen v1 baseline).
- The document's `info.version` counts revisions within `v1`: the minor
  number goes up with every additive change (new optional fields, values,
  paths). Revisions: `1.0.0` (v2.1.0), `1.1.0` (investigator results,
  `query_id` / `query_hash`, the transcript path), `1.2.0`
  (`summary_for_agent`, the ignored-event `code`, the incident.io and Azure
  Monitor adapters). A mapped result without the new data is byte for byte
  what `1.0.0` served; `summary_for_agent` is added to every result the
  contract serves.
- The same contract is available over MCP (`specialist_*` tools, see
  [MCP](mcp.md)).

## Identity and scopes

Each external system is a **named MCP token** (Settings > MCP tokens, admin
only): name it after the system, e.g. `PagerDuty prod`. Tokens are hashed at
rest, expire (1-90 days), and may be restricted to databases.

| Scope | Allows |
| --- | --- |
| `read` | open or attach an investigation, poll or stream its status, read the result |
| `propose` | request one of a concluded investigation's candidate remediations |

There is no approve: an external caller can never approve, bypass or force
anything. Send the token as `Authorization: Bearer pgs_mcp_...`; a dashboard
session cookie never authenticates the contract. The audit trail names the
agent (`agent:<token-name>:<token-id>`) on the investigation's event chain and
in **External agent requests**: the Inbox lists the ones whose approval
still waits and the outcome ledger (`#/ledger?section=agents`) keeps every
request, each linking to the other.
Each row there links its investigation to Cases and, when a cancel
remediation was queued for approval, the approval itself to **Needs your
review** (`queue_id` on `GET /api/v1/specialist-requests`, read from the
remediation's proposal). A custodian remediation's queued approval is not
linked yet; find it on Actions.

Limits (configurable under `specialist.*`): 30 opens/remediation requests and
240 reads per token per minute (`429` with `Retry-After`), at most 3 live
investigations opened per token and 10 in total (attaching never counts).
An opened investigation pg_sage cannot read right now (its database is
reconnecting, reloading or gone from the fleet, or the read fails) still
counts as live: the bound fails closed. Only a read that shows it ended
frees its slot, or, for one that stays unreadable, reaching
`specialist.max_live_age_hours` (24 by default) since it was opened.

## Calls

All paths are under `/api/v1/specialist`.

```bash
TOKEN=pgs_mcp_...
BASE=https://sage.example.com/api/v1/specialist
# open (201) or attach (200)
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"symptom":{"summary":"checkout p99 4 s"},"family":"lock_blocking",
       "window":{"start":"2026-10-04T11:40:00Z"},
       "external_ref":{"system":"datadog","id":"monitor-123"}}' \
  $BASE/databases/orders/investigations
# poll (or GET .../stream for server-sent events)
curl -s -H "Authorization: Bearer $TOKEN" $BASE/databases/orders/investigations/$ID
# result: 202 while running, 200 when terminal
curl -s -H "Authorization: Bearer $TOKEN" $BASE/databases/orders/investigations/$ID/result
# 1.1.0: a plan regression of one statement, and its redacted investigator transcript
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"symptom":{"summary":"report query 10x slower"},"family":"plan_regression",
       "query_id":"-6386917413405316221"}' \
  $BASE/databases/orders/investigations
curl -s -H "Authorization: Bearer $TOKEN" $BASE/databases/orders/investigations/$ID/transcript
# request a candidate remediation (propose scope)
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"reason":"PD incident Q1ABC"}' \
  $BASE/databases/orders/investigations/$ID/remediations/$REMEDIATION_ID/request
```

**Open or attach.** `family` (one of `lock_blocking`, `connection_pressure`,
`wal_retention`, `plan_regression`, `checkpoint_storm`, `temp_file_explosion`,
`replication_lag`, `lwlock_contention`) chooses the probe plan; without it the
operator triage runs. `attach` takes exactly one of `investigation_id` or
`incident_id`; with a `window`, an investigation of the same family that
started inside it is attached instead of opening another. Repeats with the
same `idempotency_key` (or `external_ref`) return the same investigation.

**Result.** `causal_chain` (root cause first, then contributing factors; each
link cites evidence ids, the fact text and the numbers read from that
evidence), `root_cause` with `source` (`graph` or `model`) and `authority`
(`deterministic`, or `model_earned` when the family's root authority was
earned on the held-out bench), `confidence` (the graph's support score, which
is not a probability, labelled `uncalibrated` unless a bench report gives the
family's top-1 rate with its Wilson lower bound: `bench_top1`),
`missing_evidence` (probes that failed or were unavailable, a missing probe
plan, a caller window that ended before the probes ran), and `remediations`.

**Investigator (1.1.0).** When the model investigator ran (an LLM is
configured), the result has an `investigator` section, labelled model
output:

- `verdict`: its answer normalized against the causal graph: `agree` (the
  graph's root), `conclude` (a root for an inconclusive graph), `contest`
  (another root than the graph's), `unmodeled` (a cause the graph has no
  node for, in `cause`), `inconclusive`, or `no_answer` (it stopped without
  one; `run.stop` says why). `root` and `graph_root` name the nodes.
- `adoption`: whether a concluded or contested root was `adopted` or stayed
  `advisory`, the `family` whose root authority decided and the `reason`
  (the trust ledger's model-lift verdict for that family on the held-out
  bench, or why no authority applied). Only an adopted root becomes
  `root_cause`, which then says `source: model`, `authority: model_earned`.
  An unmodeled cause is always advisory. Agreement and no answer adopt
  nothing (`not_applicable`).
- `claims`: the claims that survived citation checks, each citing its
  evidence with the numbers that evidence holds (`dropped_claims` counts
  the rest).
- `missing_evidence`: the reads the investigator asked for that returned
  nothing usable (refused, failed, not permitted), `source: investigator`.
- `run` (plan, protocol, stop, model and tool calls, probes) and
  `transcript`: the link to the redacted transcript,
  `GET .../investigations/{id}/transcript` (read scope; MCP
  `specialist_investigation_transcript`). The transcript holds the plan,
  every read with its stored result and digest, the claims and the
  outcome, redacted like everything else (identifiers hashed with a fresh
  key per view unless `specialist.keep_identifiers`); `404 not_found`
  when the investigator did not run.

**Scope to one statement (1.1.0).** `query_id` (a `pg_stat_statements`
queryid: a non-zero signed 64-bit integer, preferably sent as a string so
no client rounds it) and/or `query_hash` (lowercase hex SHA-256 of the
statement text exactly as `pg_stat_statements` shows it, UTF-8) scope an
open or attach to one statement:

- `plan_regression`: the investigation is about the statement (subject
  `queryid N`): its `plan_regressions` probe reads only that statement, its
  diagnosis is of it, and opens for different statements never share an
  investigation. Attaching (by id, incident or window) to a plan
  investigation of another statement is refused (`invalid_request`).
- Any other family: the probes still read the whole database; the result
  names the evidence that mentions the statement.

The result (and the open response) echo it as `query_scope` to the caller
that sent it (`applied`: `probes_and_diagnosis` or `evidence`;
`root_matches`; `evidence_ids`). A `query_hash` is resolved in the
database's `pg_stat_statements` with a parameter: no statement or two
statements for one hash is `invalid_request` (send `query_id` to choose),
a hash and an id naming different statements is `invalid_request`, and a
database without `pg_stat_statements` is `unavailable`. pg_sage's role
needs `pg_read_all_stats` to see other roles' statement text. Neither
field ever reaches SQL as text, a prompt or an instruction. Subjects in
the result are redacted like all text (a long number may be hidden), so
use `root_matches` rather than parsing `root_cause.subject`.

**Remediations** are pg_sage's own candidates, typed, with predicted effect,
rollback, risk tier and the gate's *preview*: the evidence-matched cancel of a
lock investigation's root blocker and the custodian actions (freeze, WAL
bound) a runway investigation attached. A request becomes an ordinary
proposal, decided by the policy gate, trust ledger, budgets and binding facts
exactly as pg_sage's own initiative: a cancel is queued for a person's
approval; a custodian action is re-scanned (stale if the custodian no longer
proposes it) and submitted through the executor. The response is the gate's
`verdict` (`queued_for_approval`, `executed` only where earned autonomy
already allows it, `parked`, `blocked`, `already_requested`,
`not_requestable`, `stale`) and `reason`. The request body has one field,
`reason`, which is recorded and never acted on.

**Summary for agents (1.2.0).** Every result read through the contract
(HTTP, MCP and the result posts) carries `summary_for_agent`, the result in
the shape generalist SRE agents check before they act on a specialist's
answer. It is derived from the result; nothing is read or invented for it:

- `status` and `reason`: the outcome and its reason. A failed investigation
  is a `200` result with `status: failed` and the failure code, never a
  `500`.
- `root_cause`: one sentence (label, mechanism, subject); empty without one.
- `evidence`: every evidence id the causal chain cites, root first, with the
  numbers that evidence holds.
- `actions`: every candidate remediation (`remediation_id`, `title`,
  `risk_tier`, `requestable`, the gate preview's `gate_verdict`,
  `evidence_ids`).
- `why_no_action`: when the investigation is over and no action can be
  requested, the first failing link of the why-not chain
  ([policy-gate.md](policy-gate.md), "Why not?"): `link`, `sentence`, the
  gate's `reason`, a typed `fix` for a person or `no_fix`. The chain is run
  over the result alone (the diagnosis is the finding, the remediations the
  candidates, their gate previews the verdicts), so links it cannot see
  stay unknown and are never reported as the cause; without a root cause
  the `finding` link fails with the outcome. `null` while an action can be
  requested or the investigation runs.

**Caller text is data.** The symptom never becomes an investigation subject,
a prompt or an instruction; it is stored with secrets and PII removed and
returned only to the caller who sent it, fenced (`caller_supplied.fenced`).

**Redaction.** Every string leaving pg_sage has credentials, connection URIs,
tokens, SQL literals, raw vectors and PII-like literals removed (the replay
export's rules). `specialist.keep_identifiers: false` also replaces schema and
object names with keyed hashes.

## Errors

`{"contract_version", "error", "code"}` with `code` one of
`unauthenticated` (401), `scope_required` (403), `database_not_permitted`
(403; the same for an existing and a missing database), `not_found` (404),
`invalid_request` (400), `payload_too_large` (413), `rate_limited` and
`too_many_investigations` (429), `not_requestable` (409), `signature_invalid`
(401), `unavailable` (503), `disabled` (404, adapter not configured).

## PagerDuty

1. Create a token (read, or read + propose) for PagerDuty.
2. In PagerDuty, add a v3 webhook subscription to
   `https://<sage>/api/v1/specialist/adapters/pagerduty` with the custom header
   `Authorization: Bearer pgs_mcp_...` and copy its signing secret.
3. Configure:

```yaml
specialist:
  pagerduty:
    signing_secret: ${PAGERDUTY_WEBHOOK_SECRET}
    services: ["PXXXXXX=orders:lock_blocking", "PYYYYYY=billing"]
    # Optional: post the result back as an incident note.
    api_url: https://api.pagerduty.com
    api_token: ${PAGERDUTY_API_TOKEN}
    from_email: oncall-bot@example.com
```

`incident.triggered` and `incident.reopened` of a mapped service open (or
attach to) an investigation; other events and unmapped services answer `202`
ignored. Both the bearer token and `X-PagerDuty-Signature` must be valid.
When the investigation finishes, pg_sage posts the diagnosis (never the
incident's own text) as a note, with retries; only the configured `api_url`
is ever called. The note is the same in 1.1.0: the investigator's claims
and a query scope are not posted (the adapter never scopes by statement).

## Generic webhook

`POST /api/v1/specialist/adapters/webhook` with a bearer token,
`X-Sage-Timestamp` (unix seconds, within
`specialist.webhook.timestamp_tolerance_seconds`) and
`X-Sage-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">` with
`specialist.webhook.signing_secret`. The body is
`{"database": "...", "request": <open request>}`. With
`specialist.webhook.result_url` set, the result JSON is posted there when the
investigation finishes, signed the same way.

## incident.io

```yaml
specialist:
  incidentio:
    signing_secret: ${INCIDENTIO_WEBHOOK_SECRET}   # whsec_... of the endpoint
    database_field: Database                        # the routing custom field
    routes: ["orders=orders:lock_blocking", "billing=billing"]
```

`POST /api/v1/specialist/adapters/incidentio` with a bearer token. Deliveries
are verified the Svix way: `webhook-id`, `webhook-timestamp` (unix seconds,
within 5 minutes) and `webhook-signature` (`v1,<base64 HMAC-SHA256 of
"id.timestamp.body">` with the decoded `whsec_` secret; any one of several
signatures passes). `public_incident.incident_created_v2` opens (or
attaches to) an investigation of the database the incident's
`database_field` custom field value names in `routes` (matched without
case); other events answer `202` ignored. An incident with no value, or one
no route names, answers `202` with `code: unknown_database` and the setting
to add it to. Redeliveries of one incident share one investigation. Nothing
is posted back to incident.io; the agent reads the result. The request is
audited with transport `http` and `external_ref.system: incident_io`.

Unverified: the payload fields pg_sage reads (`event_type`, the incident
under a key named like the event type, `id`, `name`, `permalink`,
`created_at`, `custom_field_entries[].custom_field.name` and
`values[].value_option.value`, `value_catalog_entry.name` for a
catalog-backed field, or `value_text`) and the Svix header names
follow incident.io's public webhook documentation as of 2026-11 and were not
checked against a live account.

## Azure Monitor

```yaml
specialist:
  azure_monitor:
    shared_secret: ${AZURE_ALERT_KEY}
    resources: ["/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.DBforPostgreSQL/flexibleServers/orders-pg=orders:connection_pressure"]
```

`POST /api/v1/specialist/adapters/azure-monitor` with a bearer token and the
shared key in `X-Sage-Azure-Key` or the `key` query parameter (compared in
constant time; a header that is present decides alone). The body is an
action group webhook in the common alert schema (enable it on the action
group): a `Fired` alert whose `data.essentials.alertTargetIDs` names a
mapped resource (matched without case) opens an investigation; `Resolved`
answers `202` ignored, an unmapped target `202` with `code:
unknown_database`. `data.essentials.alertId` is required (`400` naming the
field); its last segment is the external reference, so repeated firings of
one alert share one investigation. An action group cannot add an
`Authorization` header, so deliveries reach pg_sage through a relay that
adds the token (an Azure Logic App or Function), or the Azure SRE Agent
calls the contract itself (below). Nothing is posted back.

## Packaging for generalist SRE agents

`deploy/specialist/` holds what a generalist agent needs to find and call
pg_sage:

- `server.json`: an MCP server manifest (the MCP registry format): the
  streamable HTTP endpoint `https://{sage_host}/api/v1/mcp`, bearer-token
  auth, and the tool list with each tool's scope, generated from the tool
  registry (`go test ./internal/mcp -run
  TestSpecialistManifestMatchesTheRegistry -update-manifest`; a test fails
  when it drifts).
- `pg_sage-skill.md`: the agent instructions: when to call pg_sage, the
  session order (open, poll status, read the result, request a remediation,
  wait for the decision) and what pg_sage never does. Load it as the
  agent's skill, runbook or knowledge item.

Give every agent its own MCP token (its identity in the audit trail), with
`read` first and `propose` once it should request remediations. No token
holds approve: a person approves in pg_sage. A conformance test
(`internal/specialist/conformance_test.go`) replays the session a
generalist agent runs over MCP and fails if an agent could gain approval.

The setups below are unverified against the vendors' current consoles
(written from their public documentation as of 2026-11, without an
account); the pg_sage side is tested.

### Datadog Bits AI SRE

Add pg_sage as an MCP server (Bits AI's custom MCP integration) with
`server.json`'s endpoint and an `Authorization: Bearer <token>` header, and
add `pg_sage-skill.md` to the agent's instructions. Without MCP, a Workflow
Automation HTTP action (or a monitor webhook through the generic webhook
adapter) calls `openInvestigation` with
`external_ref: {"system": "datadog", "id": "<monitor id>"}`, then polls
`getInvestigationResult` and attaches `summary_for_agent` to the incident.

### AWS DevOps Agent

Where it accepts a custom MCP server, register `https://<sage>/api/v1/mcp`
with the bearer token and let it use the `specialist_*`,
`get_request_status` and `wait_for_decision` tools, with
`pg_sage-skill.md` as its runbook. Where it accepts HTTP actions, import
`/api/v1/specialist/openapi.json`.

### Azure SRE Agent

Connect pg_sage as an MCP server (streamable HTTP, bearer token) and load
`pg_sage-skill.md` as a custom instruction. Its hooks can require a root
cause, evidence and actions before an answer is accepted:
`summary_for_agent` carries exactly those. For alert-driven investigations
without the agent, route Azure Monitor alerts to the adapter above.

### incident.io

Point an incident.io webhook endpoint at the incident.io adapter above (it
opens the investigation when the incident is declared) and, where its AI
agent accepts MCP servers, add `server.json`'s endpoint so it can read the
result and request remediations itself.

In every case the agent sees evidence-cited diagnoses and candidate
remediations; what runs is still decided by pg_sage's gate.
