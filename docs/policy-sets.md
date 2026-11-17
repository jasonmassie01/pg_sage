# Policy sets and exceptions

A fleet's standing policy is written once per **policy set**, not once per database. A set
is a named policy document with **selectors** that pick its databases and a **priority**.
Each registered database follows the highest-priority set that matches it. A database that
needs something different has an **exception**: its own document, listed with how it
differs from its set, who set it, why, and when it expires.

The [policy gate](policy-gate.md) reads the document in force on every decision, through
the set. The version and SHA-256 that the decision ledger and approval cards record are the
set's document (or the exception's), so they still name exactly what the gate evaluated.

## The three sets you start with

| Set | Priority | Selector | Matches |
|---|---|---|---|
| `default` | 0 | `{all: true}` | every database |
| `prod` | 10 | `{tags: {tier: prod}}` | databases tagged `tier=prod` |
| `agent-created` | 20 | `{creator: "agent:*"}` | databases whose creator is an agent (filled in by discovery) |

On first start each set gets version 1 of its document. `default` gets the configured
profile (`policy.profile`, staffed or unattended). The others get a copy of `default`'s, so a
new set changes nothing until you ratify a different document. You can't delete `default`,
change its priority (0) or change its selector (`{all: true}`): every database must always
resolve to a document.

## Selectors

A set matches a database when **any** of its selectors matches. A selector matches when
**every** field it names matches:

| Field | Matches |
|---|---|
| `all: true` | every database (no other field allowed) |
| `tags: {key: value}` | each listed tag on the database (`tags` in fleet YAML as `key=value`, or the database record's tags) |
| `provider`, `provider_project` | the database's provider and project, when a cloud adapter records them (none does yet) |
| `discovery_source` | the discovery source that enrolled the database |
| `creator` | the recorded creator, exactly, or as a prefix when it ends in `*` (`agent:*`) |

A selector with no field is refused, and a set with no selectors matches nothing. The API
refuses an unknown selector key. It also refuses a set that would tie with another set at
the same priority on a registered database, so ties can't reach the gate. If a tie does
appear later (for example, after a database's tags change), the gate fails closed for that
database with `policy_unavailable` until you change one of the priorities.

## Requirements

A set also stores requirements beyond its document:

```yaml
requirements:
  grades:                       # review rubric dimensions (docs/review.md)
    - dimension: recoverability
      min_letter: C
      within_hours: 24
  owner_required: true
  ttl_hours: 72
  enrollment_deadline_hours: 24
  trust_ceiling:                # per change class: observation, advisory or autonomous
    index: advisory
```

Requirements are validated when you save the set: an unknown rubric dimension, grade letter,
change class or trust level is refused. They aren't enforced yet. Enrollment by policy set
will enforce them when it ships.

## Changing a set

- **Selectors, priority and requirements:** an admin saves them with
  `PUT /api/v1/policy-sets/{name}`. Every save leaves a `set_saved` row in
  `sage.policy_set_events`.
- **The document:** only through the usual propose-then-ratify flow:
  `POST /api/v1/policy/proposals` with `"set_name": "<name>"`, then ratification by an admin.
  There is no direct write.

## Exceptions

A database's own document is an exception against the set it matches. You get one in two
ways:

- An admin sets one with `POST /api/v1/policy-exceptions`
  (`{"database_id": 7, "reason": "...", "expires_at": "2026-12-01T00:00:00Z"}`). The
  database gets a copy of its set's current document. From then on, changes go through
  proposals on the database's scope.
- A proposal on a database's scope is ratified (`database_id` in the proposal). The ratified
  document becomes the database's own, and the set and its other members keep the set's
  document.

An exception with `expires_at` holds until that moment. At `expires_at` the gate goes back to
the set's document. Within a minute the sweep supersedes the exception's document and writes
an `exception_expired` row in `sage.policy_set_events`. An exception without `expires_at`
holds until it is replaced.

`GET /api/v1/policy-exceptions` lists every exception with its live diff against the set
the database matches now, field by field.

**Upgrading:** per-database documents from earlier releases are listed as exceptions against
`default` with their content unchanged. The gate keeps evaluating exactly the same documents
(same hashes) after the upgrade.

## API

| Route | Role | Does |
|---|---|---|
| `GET /api/v1/policy-sets` | viewer | sets with their selectors, requirements, and their document's version and SHA-256 |
| `PUT /api/v1/policy-sets/{name}` | admin | saves selectors, priority and requirements (a new set starts with a copy of `default`'s document) |
| `GET /api/v1/policy-sets/{name}/members` | viewer | the databases that follow the set, each with its exception if it has one |
| `GET /api/v1/policy-sets/{name}/export` | viewer | the set as YAML (selectors, priority, requirements and document with its version) |
| `POST /api/v1/policy-sets/import` | admin | saves a YAML set. If its document differs, files a proposal against the file's version, and the document still needs ratification |
| `GET /api/v1/policy-exceptions` | viewer | every exception with its diff, who set it, why and when it expires |
| `POST /api/v1/policy-exceptions` | admin | gives a database its own document (a copy of its set's) |

`GET /api/v1/policy?database_id=<id>` returns the document in force for that database. When
the document comes from a set, `set` names the set.

## Fail-closed behaviour

- A control-database read failure during resolution is the gate's `policy_unavailable`. A
  failed read never falls back to a wider document.
- A tie between two sets at the same priority on one database is `policy_unavailable` for
  that database.
- Resolution reads the sets, the database's own document and its exception in one
  read-only, repeatable-read transaction, so a set change lands for a decision whole or not
  at all.
