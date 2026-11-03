# Databricks Apps application SLO acceptance — dev.5

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

## Dev.5 post-upgrade pass

The complete new synthetic SLO lifecycle passed on October 30 against temporary
SNAPSHOT `01f1d48c7f281139a492336dbe55b574`, exit zero, `complete: true`, no failure
and empty stderr. Its Linux SHA256 is
`2ca61a1a3fa16ef28a4b63ccb92033538684b6d99462811934b39f89a56e70b0`; all seven
immutable package files match the reviewed stage. The unchanged frozen Go and
embedded source digest is
`bfc22887b3157180c9e39e5bbdd4ac2d4d6515949b36b6337c965107f90cf84d`.
The [dev.5 overall acceptance](2026-10-30-v2.5-dev5-acceptance.md) records final
binary/source/browser/history checks separately.

Fifteen actual current-time samples ran continuously from
**18:06:44.355751 to 18:13:43.927831 UTC**, without timestamp backdating. The
unique objective `acceptance_dev5_6423a1cefef55ce2737d` used target 0.999,
5m/1m burn windows at 14.4x, 15-second evaluation, 90-second staleness and
explicit `pg_sage` binding. Both windows measured **20x** and opened exactly
one synthetic case. New/replay/conflict/unknown-actor/missing-database/origin
checks passed as 202/200/409/400/400/403; legacy signed ingestion remained 501.
Databricks administrator ID 1 authenticated every push; there was no SLI HMAC
or pg_sage password. Observation trust, database-only reads, metadata-only
telemetry, disabled action/approval requests and external sinks were preserved.

Case `db758bc4-0602-4a31-981d-365fa99ccd56` completed with **3 real Gateway
model calls, 8 tools, 10,040 tokens**, advisory inconclusive output and a verified
17-event chain. Its accepted answer cited available `application_slo_status`
evidence `3c966294-1cd2-4ad0-b5a9-f4a4c9c7d090`, SHA256
`9ef1798f99294391bad6b5f352041e2edaff2a6e0cb2295f4553df85a8adb368`.
These generated counters demonstrate this synthetic scenario and establish no
production customer impact, database cause, remediation or performance benefit.

After **123.06 actual seconds without another push**, status was
`unknown [stale]`, both burn rates were null, rule firing was false, and live
customer-impact/fast-burning flags were false. All eight action/approval ledgers
remained unchanged. Exact actor/database/objective cleanup verified zero samples,
status and transitions, with the single cited case retained. No writer remains.
Evidence prefix: `.local-test-results/gap-dev5-app-sli-upgrade-full-clean`, with
its immutable artifact binding in `gap-dev5-app-sli-upgrade-artifact-binding.json`.

Final SNAPSHOT **`01f1d48e0748107d95b5bdd1ffad0e22`** uses the same binary and
empty objectives with `open_investigations: false`. A read-only final check at
19:39:39 UTC verified RUNNING/ACTIVE, objective count zero, an empty SLO API list,
zero exact actor samples/status/transitions, one preserved case and unchanged
ledgers. Evidence: `gap-dev5-app-sli-upgrade-final-baseline.json`. No temporary
objective or source was left active. The historical dev.4 proofs below retain
their original scopes and artifacts.

## Patched artifact pass

The fresh continuous SLO acceptance passed on **2026-10-30** against the
history-filter-patched artifact below: exit status **0**, `complete: true`,
no reported failure and **zero-byte stderr**. This was the dev.4 artifact-bound
SLO proof; the preceding candidate pass and all helper failures remain below.
The final empty-objective deployment/API/browser/history acceptance is tracked
separately in the [overall dev.4 report](2026-10-30-v2.5-dev4-acceptance.md).

| Exact patched run identity | Value |
| --- | --- |
| Temporary accepted App SNAPSHOT | `01f1d4818c2a18468eb4ea1786f2bf05` |
| Linux amd64 binary SHA-256 | `7e5254e097c7b20e9a5a10c7bbfd4b67a3073e037af3f1d33ccb97dfbe47aff1` |
| Unique synthetic objective | `acceptance_dev4_5d57221daad74137ada4` |
| App / principal | development App (deployment URL omitted) / `<app-principal-id>` |
| Workspace / profile | `<deployment-url>` / `<test-workspace-profile>` |
| Dedicated endpoint | `projects/<project>/branches/<branch>/endpoints/<endpoint>` |
| Database resource / PostgreSQL database | `projects/<project>/branches/<branch>/databases/<database-resource>` / `pg_sage` |
| Actor-bound stored series | `databricks_user:1:acceptance.dev4.88a0dc8126d44a3f9b02d608d6f1ed6d` |
| Durable SRE deployment / database UUID | `e76cf8ec-2d44-4b80-b016-2a87180e1e14` / `1dc11514-b41c-4cf7-90c8-53dca4144f21` |

The persisted passwordless Databricks administrator ID `1`, local role,
App-principal identity and exact attached PostgreSQL resource were checked
before any pushes. The App remained RUNNING/compute ACTIVE and on the exact
SNAPSHOT at completion. The immutable deployment artifact directory was
`<workspace-source-directory>>/src/01f1d4818c2a18468eb4ea1786f2bf05`.
Workspace Files AUTO export matched the reviewed package's exact hashes:

- `config.json`: `3cdabb09218fd5a99dfd64df648729f12a4ca5ddafd600fcdc37bbe4892039eb`.
- `launch.py`: `139c3d265e0f8507c5527fb40d381289f44cb5c99e5a8b7a86a114c05fa2b2c4`.

Runtime and startup proof again established observation trust, database-only
read scope, one database-bound push objective, target `0.999`, one-day objective
window, minimum eligible `50`, staleness `90s`, evaluation `15s`, and a single
page rule at `14.4x` over `5m`/`1m`. Proxies, Prometheus, HMAC, automatic incident
starts, runways, change feed, actions/approval requests and external sinks
remained disabled. Existing telemetry consent remained metadata-only
(`allow_payload: false`); the test changed no grants, provider settings, source
code, assets or deployments.

### Fifteen real samples and both burn windows

The new-sample/replay/refusal responses again passed exactly: **202** new,
**200** identical replay, **409** conflicting replay, **400** unknown actor
field, **400** missing database selector, **403** cross-origin mutation and
**501** legacy signed ingestion. All fifteen new samples returned 202.

| Sample | Observed at (UTC) | Cumulative bad | Cumulative eligible |
| --- | --- | ---: | ---: |
| 1 | `2026-10-30T16:48:54.698186Z` | 20 | 1000 |
| 2 | `2026-10-30T16:49:24.231442Z` | 40 | 2000 |
| 3 | `2026-10-30T16:49:54.231498Z` | 60 | 3000 |
| 4 | `2026-10-30T16:50:24.780750Z` | 80 | 4000 |
| 5 | `2026-10-30T16:50:54.233125Z` | 100 | 5000 |
| 6 | `2026-10-30T16:51:24.233481Z` | 120 | 6000 |
| 7 | `2026-10-30T16:51:54.924743Z` | 140 | 7000 |
| 8 | `2026-10-30T16:52:24.233952Z` | 160 | 8000 |
| 9 | `2026-10-30T16:52:54.234244Z` | 180 | 9000 |
| 10 | `2026-10-30T16:53:24.747149Z` | 200 | 10000 |
| 11 | `2026-10-30T16:53:54.234739Z` | 220 | 11000 |
| 12 | `2026-10-30T16:54:24.230793Z` | 240 | 12000 |
| 13 | `2026-10-30T16:54:54.743964Z` | 260 | 13000 |
| 14 | `2026-10-30T16:55:24.231380Z` | 280 | 14000 |
| 15 | `2026-10-30T16:55:54.233793Z` | 300 | 15000 |

These are actual current-time timestamps separated by approximately 30-second
real waits, spanning **419.535607 seconds**. No historical timestamps or
fake clock were used. Unknown/no-data/partial-window withheld a burn until
coverage became sufficient. The page transition occurred at
`2026-10-30T16:53:33.127968Z`; it remained one continuing burn episode through
the final sample. The recorded page evaluation at `2026-10-30T16:55:48.128449Z`
contained:

| Window | Bad increase | Eligible increase | Burn |
| --- | ---: | ---: | --- |
| 5m | 200 | 10,000 | `(200/10000)/(1-0.999) ≈ 20x` |
| 1m | 40 | 2,000 | `(40/2000)/(1-0.999) ≈ 20x` |

Both passed the 14.4x rule, 50-event floor and coverage checks. The independent
one-day budget remained unknown/partial-window with a null remaining-budget
value. This test accepts the configured short burn windows, not a complete
one-day budget or default-hour endurance.

### One durable case and real Gateway evidence

| Record | Patched run result |
| --- | --- |
| Investigation | `7cd0cfdf-ab67-432e-8dc0-aac22715c85f` |
| Exact source case / trigger | `slo:acceptance_dev4_5d57221daad74137ada4` / `slo_burn` |
| Terminal state / transcript stop | `inconclusive` / `final` |
| Real Gateway calls/tools/tokens | `3` / `7` / `9,717` |
| Bound SLO evidence | `ab2a3e39-10be-4cce-9409-9ee94120c1cc`, `application_slo_status` v1 |
| Evidence SHA-256 | `8114c44722ab64927df203f6e9842cec3292306a3463553a11a820aacc567e33` |
| Verified event chain | `17` sequential hash-linked lifecycle events |
| Pinned database binding | `8e28fd3541d22e1a08d65fa12d73575acf29fd6fd2fc3b94decf3be092eafe20` |

The exact fresh-objective filter contained exactly one case. All evidence hashes
verified, individual readback matched the stored digest, and the accepted model
claims and deterministic synthetic-impact summary cited this retained burn
evidence. The final transcript's accounting matched the case. App client tokens
rose **924 → 10,641**, exactly **9,717** additional tokens.

The outcome is advisory inconclusive and does not establish a database cause or
remediation. Two history-tool requests were rejected before execution with
`cost_budget: 4 of 4 probe cost units are used` and elapsed time zero. This is
budget enforcement rather than evidence that the history fix failed or that a
Lakebase source/permission was unavailable. Actual history execution/readback
is accepted separately; it is not inferred from this SLO case.

### Actual stop, stale state, unchanged ledgers and exact cleanup

After sample fifteen, no samples were added. At **124.71 actual seconds**
since the last observation, the API reported `unknown` with reason `stale`,
`fast_burning: false`, `customer_impact: false`, rule `firing: false` and null
current burn rates. The retained evaluation was
`2026-10-30T16:57:48.128331Z`. The earlier page-to-unknown transition at
`2026-10-30T16:56:33.127944Z` reflected lost short-window coverage; the final
read independently established actual source staleness beyond the 90-second
limit. Unknown was not called recovery or a healthy error rate.

The same eight action/approval ledgers listed in the earlier record all
remained empty with unchanged digest `d41d8cd98f00b204e9800998ecf8427e`. No action,
approval or notification was invoked. Before cleanup: **15 actor samples,
zero foreign series, one status, three transitions, one case**. Exact scoped
cleanup used the API-discovered case, verified pinned target and registered
UUIDs, actor series above and internal namespace
`db:dqMXQcQ-7Op0Dyp3M4dCvywPsZf97BXmfDQCZPnd-Fw`. Within the cleanup transaction, all
owned samples/status/transitions were verified absent and **the one case was
preserved**. All earlier cases/evidence remain separate and retained.

A still-configured temporary objective may recreate an unknown status. The
final baseline removes all objectives with SLO evaluation enabled, proxies/open
investigations off, observation trust and unchanged payload consent. Its final
SNAPSHOT and readback belong to the overall report; cleanup does not imply that
the final baseline was already deployed.

This is a task-owned **synthetic application SLI**, not real customer traffic.
A burn/citation is a synthetic signal; it establishes neither production
customer impact nor performance benefit, incident recovery or unrestricted
Lakebase compatibility.

### Patched-run artifacts and command

Local private artifacts retained in this checkout:

- Clean patched acceptance (private local artifact: `gap-dev4-app-sli-history-filter-full-clean.json`).
- All fifteen samples (private local artifact: `gap-dev4-app-sli-history-filter-full-clean-samples.json`).
- Durable transcript (private local artifact: `gap-dev4-app-sli-history-filter-full-clean-transcript.json`).
- Progress log (private local artifact: `gap-dev4-app-sli-history-filter-full-clean.log`); stderr companion is empty.

The historical accepted command was:

```sh
python3 .local-test-results/gap-dev4-app-sli.py --run \
  --expected-deployment 01f1d4818c2a18468eb4ea1786f2bf05 \
  --startup-config .local-test-results/gap-dev4-history-filter-slo-upload/config.json \
  --objective acceptance_dev4_5d57221daad74137ada4 \
  --attempt history-filter-full-clean
```

Do not reuse this objective/case or run it against the final empty-objective
baseline. Source contracts and prior failure attribution are retained below.

## Earlier candidate pass before the history filter fix

The clean continuous acceptance passed on 2026-10-30, with exit status 0,
`complete: true` and empty stderr. Fifteen current-time cumulative synthetic
samples covered **419.447161 real seconds**, produced a page-level burn on the
configured 5m/1m windows, opened exactly one durable case, completed a real Unity
Gateway investigator transcript with cited hashed evidence, and became
`unknown: stale` after pushes stopped. The action/approval ledgers were
unchanged and exact fixture cleanup preserved the case.

**Earlier artifact boundary:** this preceding pass belongs to candidate binary
`6318e250eda5aadbad3e7d4700f082d1c6ae60d0736eed1fa6f827faf6ee644b` and temporary
SNAPSHOT `01f1d47e2eb716cba5f9c6b44ace2b65`. A later history-reader correction
moved query-ID filtering before the raw row limit, preventing monitoring rows
from hiding a requested retained plan. The fresh patched-artifact proof above
is the acceptance for that changed binary. This earlier candidate record and
its helper failures remain preserved separately.

This is a synthetic application-SLI integration test on dedicated development
resources. It establishes neither production customer impact nor an underlying
database cause, remediation, workload benefit, default-hour burn endurance or
one-day SLO compliance. Overall dev.4 deployment/regression acceptance and the
final baseline readback are recorded separately in the
[dev.4 acceptance report](2026-10-30-v2.5-dev4-acceptance.md).

### Exact environment and configuration proof

| Item | Accepted scope |
| --- | --- |
| Profile / workspace | `<test-workspace-profile>`, `<deployment-url>`, workspace `<workspace-id>` |
| App | development App (deployment URL omitted), App RUNNING / compute ACTIVE before and after |
| App principal | `<app-principal-id>` |
| Temporary acceptance SNAPSHOT | `01f1d47e2eb716cba5f9c6b44ace2b65`; unchanged throughout the clean run |
| Lakebase endpoint | `projects/<project>/branches/<branch>/endpoints/<endpoint>` |
| Database resource / SQL database | `projects/<project>/branches/<branch>/databases/<database-resource>` / `pg_sage` |
| Direct endpoint host | `<lakebase-or-workspace-host>` |
| Gateway route/model | Workspace `/ai-gateway/mlflow/v1`, `system.ai.gpt-6-sol`, native App-principal Databricks OAuth; no API key |
| Accepted candidate Linux binary SHA-256 | `6318e250eda5aadbad3e7d4700f082d1c6ae60d0736eed1fa6f827faf6ee644b` |
| Unique objective | `acceptance_dev4_16db38bd122d428fbb67` |
| Persisted sample actor | Databricks-bound passwordless administrator ID `1`; series `databricks_user:1:acceptance.dev4.c704a721573c421b99ea130caa74a0ef` |

The App login/API identity and durable `sage.users` record matched the reviewed
administrator and Databricks Apps issuer/subject, with role `admin` and a NULL
password. Background Gateway operations used the App principal; this was not
user on-behalf-of model access. PostgreSQL metadata checks/cleanup used the selected operator’s
own official short-lived OAuth credential in a subprocess environment. No App
secret, database password, workspace token or API key was exported or recorded.

Before pushes, Workspace Files exported `config.json` and `launch.py` using
`AUTO` from the immutable deployment artifact directory
`<workspace-source-directory>>/src/01f1d47e2eb716cba5f9c6b44ace2b65`.
Both hashes matched the reviewed local package:

- `config.json`: `8f0ed6f0ee4af486008cc8776ff5592e6aa011c257597d1d2ecd29649e44341c`.
- `launch.py`: `139c3d265e0f8507c5527fb40d381289f44cb5c99e5a8b7a86a114c05fa2b2c4`.

Runtime status independently verified observation trust, database-only read
policy, exactly one push objective, SLO evaluation/open investigations enabled,
no database proxies, Prometheus or HMAC secret, and no automatic incident
starts, runways, change feed, action proposals, approval requests or external
notification/result sinks. The reviewed startup objective had target `0.999`,
window `1d`, minimum eligible events `50`, stale limit `90s`, evaluation every
`15s`, and the single page rule `5m / 1m / 14.4x`. These startup values were not
inferred from the limited public global config read model.

The existing metadata-only telemetry consent remained `allow_payload: false`;
no warehouse/UC grants, preview switch or telemetry capture setting was changed.

### Ingestion and continuous samples

The supported route was
`POST /api/v1/sre/slos/acceptance_dev4_16db38bd122d428fbb67/samples?database=pg_sage`.
Every accepted body contained only series, cumulative bad/eligible counters and
its actual current UTC observation time. The server bound the actor and
registered database scope; the test never supplied actor/deployment/database
identity in the body or backfilled observation times.

| Contract | Actual response |
| --- | --- |
| New sample | 202, accepted and not duplicate |
| Identical same-series/time replay | 200, duplicate and not accepted again |
| Different counters at the same identity | 409 |
| Unknown `actor` body field | 400 |
| Missing explicit database selector | 400 |
| Cross-origin mutation | 403 |
| Legacy signed HMAC SLI protocol in App mode | 501 |

All fifteen new samples returned 202. Their actual timestamps and counters were:

| Sample | Observed at (UTC) | Cumulative bad | Cumulative eligible |
| --- | --- | ---: | ---: |
| 1 | `2026-10-30T16:24:46.550232Z` | 20 | 1000 |
| 2 | `2026-10-30T16:25:16.000219Z` | 40 | 2000 |
| 3 | `2026-10-30T16:25:45.994729Z` | 60 | 3000 |
| 4 | `2026-10-30T16:26:16.588938Z` | 80 | 4000 |
| 5 | `2026-10-30T16:26:46.000966Z` | 100 | 5000 |
| 6 | `2026-10-30T16:27:16.001113Z` | 120 | 6000 |
| 7 | `2026-10-30T16:27:46.612424Z` | 140 | 7000 |
| 8 | `2026-10-30T16:28:16.001387Z` | 160 | 8000 |
| 9 | `2026-10-30T16:28:46.001524Z` | 180 | 9000 |
| 10 | `2026-10-30T16:29:16.623090Z` | 200 | 10000 |
| 11 | `2026-10-30T16:29:45.996822Z` | 220 | 11000 |
| 12 | `2026-10-30T16:30:15.996531Z` | 240 | 12000 |
| 13 | `2026-10-30T16:30:46.518234Z` | 260 | 13000 |
| 14 | `2026-10-30T16:31:15.997232Z` | 280 | 14000 |
| 15 | `2026-10-30T16:31:45.997393Z` | 300 | 15000 |

The first sample was `2026-10-30T16:24:46.550232Z`; the last was
`2026-10-30T16:31:45.997393Z`. The intervening schedule used real waits of
approximately 30 seconds. Initial no-data/partial-window states withheld a burn
until coverage was sufficient. The page transition occurred at
`2026-10-30T16:29:27.767366Z` and stayed within the same burn episode through the
final sample.

The final recorded page evaluation at `2026-10-30T16:31:42.766587Z` contained:

| Window | Bad increase | Eligible increase | Burn calculation |
| --- | ---: | ---: | --- |
| 5m | 200 | 10,000 | `(200/10000)/(1-0.999) ≈ 20x` |
| 1m | 40 | 2,000 | `(40/2000)/(1-0.999) ≈ 20x` |

Both exceeded 14.4x and the 50-event floor with sufficient sample coverage.
`fast_burning`, the synthetic application `customer_impact` signal, and rule
`firing` were true. The separate one-day budget remained unknown/partial-window
with no budget-remaining value; seven minutes cannot establish a one-day budget.

### Durable case, real Gateway and evidence chain

| Record | Exact identity / result |
| --- | --- |
| Fresh-objective investigation | `e9d24133-36e0-45d5-9bbb-66041692d4c4` |
| Source case / kind / subject | `slo:acceptance_dev4_16db38bd122d428fbb67` / `slo_burn` / `slo acceptance_dev4_16db38bd122d428fbb67` |
| Durable SRE deployment UUID | `e76cf8ec-2d44-4b80-b016-2a87180e1e14` (distinct from the Apps deployment identifier) |
| Registered database UUID / runtime | `1dc11514-b41c-4cf7-90c8-53dca4144f21` / `startup:pg_sage` |
| Terminal outcome | `inconclusive`, advisory; transcript stop `final` |
| Real model accounting | 3 model calls, 8 tool calls, 9,635 tokens |
| Cited application SLO evidence | `5544fd14-c499-42b9-9915-a4e876f00bdc`, `application_slo_status` v1 |
| Evidence SHA-256 | `b22750707caee5973efd9f93541deb8127097c842478765b6273fe62582e6500` |
| Event chain | 17 events, verified hashes, sequential links and lifecycle events |
| Pinned database binding hash | `8e28fd3541d22e1a08d65fa12d73575acf29fd6fd2fc3b94decf3be092eafe20` |

The exact-objective API list contained one case throughout the continuous burn.
Every stored evidence hash verified; individual SLO evidence readback matched
its SHA-256. The deterministic summary's synthetic burn citation and accepted
model claims referenced the actual retained SLO evidence. The transcript matched
case model/token accounting, had a final outcome, and the created event pinned
the database-only policy and physical target binding. Client token accounting
increased from 967 to 10,602, exactly matching the case's 9,635 tokens.

The model did not establish a database cause. Two `read_history` requests were
rejected before execution (`elapsed_ms: 0`) because `4 of 4 probe cost units`
were already used. This demonstrates the configured budget guard; it is not a
Lakebase permission/source failure and does not accept App-principal history
execution. The terminal advisory inconclusive outcome and those rejections
remain visible. SLO observation/citation, source-health and durable investigator
completion passed independently of that diagnostic limitation.

### Actual stopped-source staleness

After sample fifteen, no new samples were submitted. At **121.04 actual seconds**
since its observation time, the API reported `state: unknown`, reason `stale`,
`fast_burning: false`, `customer_impact: false`, rule `firing: false`, and null
current burn rates. The retained evaluation time was
`2026-10-30T16:33:42.766999Z`.

The first page-to-unknown transition was earlier,
`2026-10-30T16:32:27.767353Z`, as the short window lost coverage; the later
observation explicitly verified staleness beyond the configured 90-second
limit. Unknown was never reinterpreted as recovery or a healthy error rate.
The test used neither a fake clock nor historical sample seeding for this phase.
The same single durable case remained present.

### Unchanged ledgers and exact cleanup

Before and after, all eight tables had zero rows and the same deterministic
empty digest `d41d8cd98f00b204e9800998ecf8427e`:

- `sage.action_log`, `sage.action_queue`, `sage.decision`, `sage.policy_approvals`.
- `sage.sre_action_proposals`, `sage.sre_maintenance_proposals`,
  `sage.sre_autonomy_proposals`, `sage.approval_card_deliveries`.

These fingerprints detect updates to existing rows as well as insertions. No
action, approval or notification call was made. Before cleanup, the exact owned
scope had 15 actor samples, zero other series, one objective status, three
transitions and one case.

Cleanup used the API-discovered case joined to its registered durable scope,
with the pinned target binding independently verified. It deleted only this
series under the exact deployment UUID and internal objective namespace
`db:tkFAXUp2_8JttZmpSsP-xV3n5-izhJytWCN_CamJkxU`, then this objective status under
the exact deployment/database UUIDs and public objective name. Cascaded
transitions were verified absent. The same transaction verified zero remaining
actor samples/status/transitions and **one preserved case**. The earlier
interrupted objective's separate case was preserved too.

The temporary engine can recreate an unknown status while its objective stays
configured. The final prepared baseline therefore keeps SLO evaluation enabled
but has **no objectives**, proxies/open investigations off, observation trust,
no notifications/actions, and unchanged metadata-only payload consent. Root's
final deployment/readback is tracked in the overall dev.4 report; this report
does not infer final configuration from temporary fixture cleanup.

### Earlier harness failures remain separate

| Attempt / artifact | Actual result and classification |
| --- | --- |
| `gap-dev4-app-sli-dev4-final.json` | Read-only preflight failed exporting Workspace Files with RAW; no sample submitted. Both artifacts were accessible; AUTO/SOURCE worked. Helper corrected to AUTO while retaining immutable snapshot hash proof. |
| `gap-dev4-app-sli-dev4-final-retry1.json` | Eleven actual samples over 299.526286s reached page on the earlier objective; helper then tried to parse empty API scope UUIDs. The read-service JSON/redaction roundtrip does not preserve its `Scope` field. Exact API case ID plus verified pinned target and a read-only registered-scope lookup corrected the helper. |
| `gap-dev4-app-sli-dev4-final-continuation.json` | PostgreSQL JSON emitted a five-digit fractional timestamp that Python 3.9 rejected. The failure handler performed exact fixture cleanup and preserved the case. Helper now normalizes fractional precision without changing time. No real stale or seven-minute claim is made for this attempt. |
| `gap-dev4-app-sli-dev4-preserved-case.json` | An ad hoc read-only verifier omitted its stored-series initialization after the case/hash checks. Correctly initialized final verification below passed; no cloud mutation resulted from that read-only diagnostic. |
| `gap-dev4-app-sli-dev4-preserved-case-final.json` | Preserved earlier case `b52a881b-a28b-4074-8889-a13351718192` independently verified: two model calls, five tools, 6,531 tokens, final advisory inconclusive outcome, both 20x burn windows and a 17-event chain. All eight ledgers unchanged; no new samples. |
| `gap-dev4-app-sli-dev4-full-clean.json` | Fresh objective and SNAPSHOT, corrected helper, uninterrupted fifteen-sample schedule plus actual staleness; **passed**. This is the continuous acceptance above. |

The earlier objective was `acceptance_dev4_cb6340e7f2d8472a8142` on temporary
SNAPSHOT `01f1d47c59e517c1aa74b207a3c3d6ab`. Its actual accepted timestamps were
`2026-10-30T16:12:33.180810Z` through `2026-10-30T16:17:32.707096Z`.
The interrupted attempt was not renamed or combined with the clean run to
create a false continuous pass. Between these SLI attempts, no production
Go/frontend code or provider configuration was changed to obtain the clean
result; only the local acceptance helper and unique fixture configuration changed.

### Reproduction and retained artifacts

The task-specific helper requires explicit `--run`, an exact deployment ID,
a reviewed startup package and a unique configured objective. For the accepted
historical deployment, the command was:

```sh
python3 .local-test-results/gap-dev4-app-sli.py --run \
  --expected-deployment 01f1d47e2eb716cba5f9c6b44ace2b65 \
  --startup-config .local-test-results/gap-dev4-staged-upload-retry2/config.json \
  --objective acceptance_dev4_16db38bd122d428fbb67 \
  --attempt dev4-full-clean
```

Do not rerun against a final empty-objective deployment or reuse the accepted
objective/case. A fresh reviewed unique fixture is required. Local private
artifacts retained in this checkout (not release-package contents):

- Clean acceptance report (private local artifact: `gap-dev4-app-sli-dev4-full-clean.json`).
- Fifteen accepted samples (private local artifact: `gap-dev4-app-sli-dev4-full-clean-samples.json`).
- Durable transcript (private local artifact: `gap-dev4-app-sli-dev4-full-clean-transcript.json`).
- Progress log (private local artifact: `gap-dev4-app-sli-dev4-full-clean.log`) and zero-byte stderr companion.
- The separately named failure/continuation/preserved-case artifacts above.

Implementation contracts: [App SLI API](../sidecar/internal/api/sre_app_sli.go),
[scoped cumulative push storage](../sidecar/internal/sre/slo/engine_read.go),
[burn evaluation](../sidecar/internal/sre/slo/evaluate.go),
[coverage/staleness](../sidecar/internal/sre/slo/window.go), and
[SRE page trigger](../sidecar/cmd/pg_sage_sidecar/sre_signals.go).
