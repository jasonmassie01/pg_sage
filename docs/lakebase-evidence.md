# Lakebase and Databricks acceptance evidence

This appendix holds the dated acceptance results behind the
[Lakebase guide](lakebase.md), the
[Databricks-native guide](databricks-native.md) and the
[Databricks Apps guide](databricks-apps.md). Those guides describe how to
operate pg_sage today; this page records what each development build was tested
against, when, and what stayed unproven. Each result keeps its original
revision, date and failures. A later build that closes a gap does not rewrite
an earlier result.

## Lakebase guide

The Lakebase guide was last validated against the native Lakebase integration
in **pg_sage `2.5.0-dev.10`**. Database and App acceptance evidence was collected
October 29–30, 2026; public platform documentation was cross-checked November 1,
2026. These are scoped development results, **not full Lakebase compatibility
certification** or a production reliability guarantee. A passing local
PostgreSQL suite is separate from a passing native Lakebase test.

The [dev.11–12 follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev12-acceptance.md) adds an
optional App email-domain audience gate and classic meme images. It does not
extend the version-specific database compatibility evidence in this guide.
It also corrects telemetry inbox contention, App request-limit isolation and
event-stream lifetime behavior; those availability checks are reported
separately from feature compatibility.

The current [dev.10 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev10-acceptance.md)
verifies Databricks-only App sign-in, native Gateway OAuth M2M, connected Fleet,
database-preserving Cases/Actions/Ask workflows and desktop/mobile rendering.
Cases retain exact technical evidence while distinguishing unknown urgency,
missing measurements and unproven impact. Unnamed indexes no longer receive a
guessed inverse or catalog target; proposals without captured recovery remain
unknown. Inapplicable Lakebase `ALTER SYSTEM` procedures are withheld, including
older saved SQL when current applicability guidance rejects it. Saved and current
procedures retain separate risk/recovery claims; matching a source does not
validate an older proposal.

The [dev.8 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev8-acceptance.md)
also preserves invalid-index findings during restart warmup and exact query
identities in finding responses and Cases artifacts. The
[dev.6 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev6-acceptance.md)
fixed singleton Ask Sage selection and Fleet inventory and added hint-free spill
advice. Nine controlled finding families have real Lakebase proof. The
[scenario matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-nerdlab-findings.md) distinguishes positive
finding/recovery tests from readable sources, provider restrictions and incident
conditions that were not induced.

The [dev.5 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev5-acceptance.md) passed
one complete 142-package Go invocation, the native PG18 suite and final App
SNAPSHOT `01f1d48e0748107d95b5bdd1ffad0e22`. All five historical SRE adapters
returned positive observations and accepted citations, including exact controlled
plan and DDL events. The same binary separately passed a real-time synthetic
application SLI lifecycle. The App remains in observation mode with metadata-only
consent; App-principal workload-table maintenance execution is unproven.

### Live read-only check results

All five full advisor workflows passed with
real native Unity Gateway calls and billed-token assertions on both isolated
local PG17 and native Lakebase PG18. The native advisor fixture used a fresh
prefixed database and an in-memory short-lived credential for its raw test
connections; the production OAuth refresh hook is tested separately. Post-run
database inventory confirmed cleanup. No real application query text was sent
to the model.

The real credential-expiry retry passed in 3,634.55 seconds: the held connection
remained healthy through 30 seconds beyond expiry, then a fresh backend used a
second minted credential with a later expiry. This verifies constant-query
connection liveness and production credential refresh for the native user role;
it does not test table grants or UC-only claims across expiry. The first attempt
lost network continuity before expiry and remains recorded separately. Native
idle/resume also passed: the API reported ACTIVE → IDLE → ACTIVE, followed by
production OAuth reconnect and a read-only check of the existing marker,
77 snapshots and 63 findings. The two race-enabled tests passed in 8.077 seconds.
The App's separate service-principal hour-boundary check is tracked in its
acceptance report; failover and multi-day reliability remain unverified.

## Databricks-native guide

The October 29–30, 2026 acceptance results establish specific paths and
identities. Public platform documentation was cross-checked November 1, 2026.

The [November 2 dev.11–12 follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev12-acceptance.md)
adds the optional organization email-domain audience gate and recognizable,
locally bundled meme images. Its new validation covers auth, packaging, telemetry storage and UI;
the broader database/telemetry compatibility evidence retains its original
revision and date.

Dev.12 corrects an availability failure found during that live UI check:
telemetry inbox delivery and expiry scans competed with authentication in the
standalone App's two-connection pool. It adds query bounds/indexes and an Apps
default of five shared connections, preserving explicit configuration. Account
storage outages return retryable 503; organization/identity refusals remain 403.
This does not establish unlimited viewer concurrency or a production load test.

### Validation by revision

| Revision | Evidence and limits |
| --- | --- |
| dev.5 | [Latest uninterrupted whole-repository run](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev5-acceptance.md): 142 packages, 16,394 passing test/subtest events, 55 allowed skips, 141 package passes and one without tests. Native Lakebase: 79 pass, two explicit skips. Controlled telemetry: 24 records across all ten streams. Separate App/history/SLO acceptance |
| dev.8 | [Scenario/restart acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev8-acceptance.md): 627 analyzer race/integration/e2e tests/subtests; nine controlled finding families and invalid-index restart continuity |
| dev.10 | [Workflow acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev10-acceptance.md): 726 frontend tests/102 files; 46 browser tests including 22 desktop/mobile routes; 1,434 API/Cases race passes plus one opt-in live skip; lint/build and actual App auth/Gateway/Fleet/context/mobile checks |
| dev.11 | [Audience and meme follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev12-acceptance.md): 755 frontend tests/102 files, 46 browser tests, 1,627 auth/API race passes plus one opt-in live skip, 24 packaging tests, scoped deployed-App checks and retained live browser failures. No new whole-repository or live database compatibility run |
| dev.12 | [App availability correction](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev12-acceptance.md): 69 telemetry race/integration/e2e passes, zero skips/failures; 25 packaging tests; focused auth/API, HTTP/2 stream and limiter race checks. Final live desktop/mobile tour: 12 page checks, six decoded meme templates, six protected API checks, five clean stream completions and zero protocol errors. Earlier failed attempts remain documented |
| dev.13 | [Meme shuffle and standalone Settings](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev13-acceptance.md): 777 frontend tests/103 files, 49 browser tests, all 50 captioned memes at desktop/mobile widths, 25 packaging tests; no new database compatibility run |
| dev.14 | [Descriptions beside each meme](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev14-acceptance.md): 777 frontend tests/103 files, 49 browser tests including desktop alignment and mobile visibility of introductions across 22 routes; scoped UI follow-up |

The latest whole-repository result remains **dev.5**. Later reports identify their narrower validation scope.
The final dev.10 workflow-baseline App SNAPSHOT is `01f1d4c0a6af15be95fc0632b1a607e1`; the
acceptance report retains exact macOS/Linux artifact hashes and its source
fingerprint scope. The November 1 documentation publication changed guides, packaging
exclusions, the portable fixture helper and test-only credential setup; it did not rebuild/redeploy that
runtime or re-label its acceptance.

Publication checks on November 1 passed all 22 existing Python packaging tests,
325 relative links/anchors across 35 changed/new Markdown files, whitespace
checks and fixture-helper configuration/refusal checks. Mocked helper checks
verified that an endpoint-host mismatch is refused before minting a credential
or invoking SQL. The native-auth race package also passed after its fixture URL
was made passwordless while preserving the bootstrap-credential check.
No new live workload or compatibility soak was run for that
November 1 documentation follow-up.

Additional evidence:

- [Native SQL/action tests](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-native-tests.md)
- [Native and App SRE source matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-native-sre-source-matrix.md)
- [Provider compatibility failures and alternatives](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md)
- [Application SLO acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-app-slo-acceptance.md)
- [Statistics-maintenance acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-sre-maintenance.md)
- [First-time-user review](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-first-time-user-ux-review.md) and [dev.9 delivery](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev9-acceptance.md)

Reports preserve failures and the scope of later fixes. Private credentials,
screenshots/raw evidence, internal research and workspace/endpoint/principal identifiers are
excluded from the public repository; reviewed reports retain outcomes and hashes.

## Databricks Apps guide

These results are for release **2.5.0**. The acceptance report for its last development
build, [2.5.0-dev.14](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev14-acceptance.md), covers the
descriptions beside each tab's meme. The [dev.13 report](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev13-acceptance.md)
records the 50-meme shuffle and standalone Settings correction. The
[dev.11–12 report](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-11-02-v2.5-dev12-acceptance.md) retains the
earlier authentication, telemetry and event-stream evidence. The App remains in
Observation mode with metadata-only telemetry. Acceptance is scoped to each deployment;
it does not certify every Lakebase extension, cloud, privilege or long-duration
scenario. Older release evidence remains linked from the matrix and reports.
