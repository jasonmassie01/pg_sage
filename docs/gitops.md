# GitOps directory

pg_sage can hold its [policy sets](policy-sets.md), its agent principals
and its per-database grants in a directory of YAML files, so a team reviews
those changes in git like any other code. The sidecar reconciles the
directory into its own tables (never into a monitored database) at start
and whenever a file changes.

```yaml
policy:
  directory: policy            # relative to the config file
  directory_mode: dry_run      # dry_run (default) or apply
```

Both keys take effect at restart. Leave `directory` empty to turn the
feature off.

## Layout

```text
policy/
  policy-sets/prod.yaml        kind: PolicySet
  principals/ci-bot.yaml       kind: Principal
  grants/orders.yaml           kind: DatabaseGrants
```

Only `*.yaml` and `*.yml` files in the three subdirectories are read. Each
file names its `kind:` (the one its subdirectory holds) and
`schema_version: 1`, and is at most 1 MiB.

A policy-set file is the set's export format (`GET
/api/v1/policy-sets/{name}/export`, read by the same parser) plus `kind`
and `schema_version`:

```yaml
kind: PolicySet
schema_version: 1
name: prod-strict
priority: 50
selectors:
  - tags: {tier: prod}
document:                      # optional; without it the set keeps its document
  profile: unattended
  body: { ... }                # the policy document, as in the export
```

```yaml
kind: Principal
schema_version: 1
name: ci-bot
profile: readonly-analyst
env_ceiling: dev               # branch, dev (default), stage or prod
tenant: ""                     # optional; it never changes
sponsor: ops@example.com       # optional: an enabled operator or admin
```

```yaml
kind: DatabaseGrants
schema_version: 1
grants:
  - user: alice@example.com    # a pg_sage user's email
    database: orders           # a monitored database
    role: viewer               # viewer or operator
```

## What the reconciler owns

It creates, changes and retires only the rows it made or adopted: policy
sets and principals marked `managed_by = config`, grants with `source =
config`. Rows made in the app or the API are never changed. A file that
names one fails the reconcile until it says `adopt: true` (a top-level key
of a set or principal file, or a key of one grant entry), which moves that
row to the files. The admin grant editor refuses a config grant (409
`config_managed`); edit its file instead. Settings shows the directory's
last reconcile.

Removing a file retires its row: a set is removed, a principal is retired
(its tokens stop working; its database roles are dropped by upkeep only
under a person's recorded approval), a grant is revoked. An empty directory
retires every config-owned row. A missing directory is a startup warning
and retires nothing.

A file never mints a token: a person mints one in the app.

## Widening waits for a person

A change that narrows what an agent or pg_sage may do applies with actor
`config:<file>@<sha256>`: an admin-owned directory is operator authority,
like the config file. A change that widens it is never applied from a file
alone:

| Change in a file | What happens |
|---|---|
| A policy-set document narrower than the one in force | Ratified at once |
| A policy-set document that widens | Filed as a policy proposal; a person ratifies it |
| A principal's lower `env_ceiling`, a narrower profile, a new sponsor | Applied at once |
| A principal's higher `env_ceiling` or a profile that adds a class or the direct lane | Filed as the principal's pending change; an admin who is not its sponsor confirms it |
| A set's selectors, priority, creation or removal that would move any database to a wider document | Held: nothing is written, and Settings names the databases |
| A grant created, changed or revoked | Applied at once (it changes what a person may see) |

A widening already waiting is not filed again; one that lapses (the
principal's pending change waits 24 hours) is asked again at the next
reconcile. To land a held set change, make it in the app as a person, or
first narrow the documents of the databases it names.

## Modes

`dry_run` (the default) reads the directory and the tables, logs the diff
and shows it in Settings, and writes nothing. `apply` writes it. Switching
back to `dry_run` (and restarting) stops writes. In a fleet only the leader
writes; other replicas reconcile read only.

Each reconcile is one transaction: an invalid file (unknown kind, a
selector that does not parse, a grant naming an unknown database or user,
a profile or `env_ceiling` pg_sage does not accept, a name the API owns
without `adopt: true`) fails the whole reconcile with the file and line
and writes nothing, and a database error rolls it back so the previous
state stands. A reconcile that writes anything records one
`config_directory_reconciled` entry in the auth audit, with the directory
hash and every change, beside each store's own events.

## Checking a directory

```text
pg_sage_sidecar config validate policy/
pg_sage_sidecar config diff policy/
```

See [cli](cli.md#config). `GET /api/v1/gitops` (admin) returns the last
reconcile: mode, hash, the changes, or the error with every problem.

## Approval requests as pull requests

With `policy.approval_prs.repo` set, pg_sage also asks for approvals as pull requests on that
config repository, beside the inbox and chat. Each change that waits for approval gets one pull
request (never a second one), labelled `pg_sage-approval`, adding
`<path><database>/<queue_id>.yaml`:

```yaml
kind: ApprovalRequest
schema_version: 1
database: orders
queue_id: 412
title: Index recommendation for public.orders
card_hash: 3f1c...
risk_tier: safe
sql: CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders (customer_id)
rollback: DROP INDEX CONCURRENTLY public.idx_orders_customer
ladder:
  - {tier: 1, name: clone, result: passed, at: 2026-11-18T09:00:00Z}
expires_at: 2026-11-21T09:00:00Z
```

pg_sage polls the pull request every minute (no inbound webhook, so no new public route):

| On the code host | In pg_sage |
|---|---|
| Merged by a login mapped to an active user with the operator role (or admin) on the database | Approved and run as that user, through the same single-use approval as the inbox: the change runs only if its SQL is still the SQL in the file. A comment reports the outcome. |
| Merged with the file edited, after the change was changed in pg_sage, or by an unmapped login or a viewer | Refused with a comment; the change stays waiting in pg_sage. |
| Closed without merging | Rejected with the reason `closed in <repo>#<n>`. |
| Still open when the change is decided in the inbox or chat | Closed with a comment ("already decided"). |
| Still open at its expiry (`expiry_hours`, or sooner when the change expires) | Closed as expired; the change stays waiting in pg_sage and gets no second PR. |

High-risk changes, and any tier above `notification_policy.chat_approval_max_tier`, are never
opened as pull requests: the inbox says so and they are approved in pg_sage. An approval in the
app and a merge at the same moment run the change once.

`principals/users.yaml` maps code-host logins to pg_sage users; the GitOps reconcile ignores
this file, so `policy.directory` is required:

```yaml
kind: CodeHostUsers
schema_version: 1
users:
  - login: octo-dba
    email: alice@example.com
```

The write token comes from `SAGE_APPROVAL_PRS_TOKEN` (or `SAGE_APPROVAL_PRS_TOKEN_FILE`); it is
sent only in a request header and never logged, written to a pull request or audited. A
code-host error while opening is shown on the card ("PR not opened: HTTP 503") and retried with
backoff (30 seconds, doubling to 30 minutes). A refused token (401 or 403) turns approval pull
requests off until the token is fixed and pg_sage restarts; the Settings GitOps banner says so.
Every state change is recorded in `sage.approval_prs` and audited once as
`approval_pr_transition`. Only the fleet leader polls.

```yaml
policy:
  directory: ./gitops
  approval_prs:
    provider: github      # or gitlab
    repo: acme/pg-config
    base_branch: ""       # empty: the repository's default branch
    path: approvals/
    expiry_hours: 72
```
