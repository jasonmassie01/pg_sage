# Glossary

pg_sage uses one set of words in the dashboard, the docs, the CLI and the MCP tools. Five
nouns carry most of the meaning: **Issue**, **Plan**, **Change**, **Decision** and
**Autonomy**. The other terms below are the few words that need a definition of their own.

Each entry gives a one-line definition, the internal names the API and the code use for the
same idea, and the words to avoid in text a person reads. API fields, enums, URLs and test
ids keep their internal names; only the words a person reads change.

This file is the source of the dashboard tooltips. After editing it, regenerate them from
`sidecar/` with `go run ./cmd/gen_glossary/`; a Go test fails while the generated
`web/src/generated/glossary.json` is stale, and a dashboard test fails when a page uses an
avoided word. An entry is a `###` heading (the term), one definition line of at most 280
characters, and optional `- Internal names:` and `- Avoid:` lines with comma-separated values.

## The five nouns

### Issue

Something on a database that needs attention, with the evidence pg_sage gathered for it and what is still unknown.

- Internal names: `finding`, `case`, `incident`
- Avoid: finding, findings, case, cases

### Plan

An ordered set of Changes pg_sage proposes together for one Issue, with a check after each step; you decide on the whole Plan or on each step.

- Internal names: `change plan`, `plan stage`

### Change

One thing pg_sage proposes to do to a database, such as creating an index, with its SQL, its risk and how it is undone.

- Internal names: `recommendation`, `action`, `queue item`, `candidate`, `managed change`
- Avoid: recommendation, recommendations, proposal, proposals, candidate, candidates, proposed action, managed change, managed changes, stored revision, queue item, approval request

### Decision

A choice about a Change, always with who made it: a person (approve or reject), the policy (allow or hold back) or an agent, recorded with its reason.

- Internal names: `verdict`, `policy decision`, `approval`
- Avoid: verdict, verdicts, policy decision

### Autonomy

How much pg_sage may do on its own for one kind of Change on one database: ask first, act then tell you, or act in the maintenance window.

- Internal names: `trust_level`, `execution_mode`, `L0`, `L1`, `L2`, `L3`, `tier`, `standing policy`
- Avoid: trust level, trust levels, execution mode, trust ramp

## Other terms

### Waiting on you

Changes and Plans that need a person's Decision before they can run.

- Internal names: `pending`, `approval queue`
- Avoid: needs your review

### Held back

A Change the policy did not let run, shown with the reason and what would let it run.

- Internal names: `parked`, `withheld`, `policy decision`
- Avoid: parked

### Policy check

The rules every Change passes before it runs: its risk, the maintenance window, its reach and the Autonomy you gave.

- Internal names: `policy gate`, `standing gate`, `standing policy`
- Avoid: policy gate, standing gate

### Observation mode

pg_sage watches the database and proposes Changes, but changes nothing on it.

- Internal names: `observation`, `observe-only`
- Avoid: observe-only

### Shadow decision

What pg_sage would have done with more Autonomy, recorded without acting so it can be compared with what a person decided.

- Internal names: `shadow`, `shadow ledger`

### Risk tier

How much harm a Change could do if it went wrong: safe, moderate or high; a high-risk Change always waits on you.

- Internal names: `risk_tier`, `contract_tier`, `tier3_safe`
- Avoid: contract tier, tier3_safe

### Lifecycle

Where an Issue or a Change stands: open, in progress, resolved, rolled back or dismissed.

- Internal names: `lifecycle state`, `status`
- Avoid: lifecycle state

### Custodian

The person or team who owns a database and is told about its Issues.

- Internal names: `custodian`, `owner`

### Canary

A first run of a Change on one database or a small part of the fleet, checked before it runs anywhere else.

- Internal names: `canary`, `rollout`

### Grandfathered

Allowed because it was in place before a rule was added; new ones must follow the rule.

- Internal names: `grandfathered`

### Minimum evidence

The verified outcomes pg_sage needs before it may propose more Autonomy for a kind of Change.

- Internal names: `promotion floor`, `min_successes`
- Avoid: promotion floor

### Fast elevation

Settings that let pg_sage earn Autonomy in hours instead of weeks; meant for test systems, and high-risk Changes still wait on you.

- Internal names: `trust elevation`, `fast_elevation`

### Verification

The check after a Change runs that it did what was expected; when it did not, pg_sage rolls the Change back.

- Internal names: `verify`, `outcome`

### What-if check

A test of an index that does not exist yet (with HypoPG) to see whether a query would use it, without building it.

- Internal names: `HypoPG`, `hypothetical index`

### Binding fact

A Fact a person confirmed that pg_sage must respect, for example that an index belongs to the application.

- Internal names: `binding fact`, `confirmed fact`

### Source fix

A Change to the application or to the query text rather than the database; pg_sage can suggest it but cannot apply it.

- Internal names: `source fix`, `source_fix`

### Specialist

A narrow agent inside pg_sage for one kind of work, such as vacuum or indexes, with a written limit on what it may propose.

- Internal names: `specialist contract`, `specialist`
- Avoid: specialist contract

### Incident

An Issue that is hurting the database now, such as an outage or a pile-up of locks.

- Internal names: `incident`

### Investigation

The work pg_sage does to explain an Issue: collecting evidence, testing ideas and saying what is still unknown.

- Internal names: `investigation`, `rca`
