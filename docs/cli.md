# Command-line client

`pg_sage_sidecar` is also a small command-line client of a running
sidecar. The decision-loop commands (`plan`, `approve`, `apply`,
`explain`) call the sidecar's API; `ledger` and `doctor` read a database
directly. None of them changes a monitored database itself: an approval
goes through the sidecar's approval route, and so through the executor's
policy gate, trust, verification, rollback and audit, exactly as an
approval in the dashboard does.

## Connecting

| Variable | Meaning |
|---|---|
| `SAGE_API_URL` (or `--url`) | The sidecar's API, for example `https://sage.example.com:8080`. Plain `http` is accepted only to `localhost` or a loopback address, so the token never crosses a network in clear. |
| `SAGE_API_TOKEN` (or `SAGE_API_TOKEN_FILE`) | An operator API token (Admin, API tokens). The read scope reads; the approve scope approves. |

The token is sent only as `Authorization: Bearer`, never in a URL, and is
never printed: a sidecar message that repeats it is shown as `[redacted]`.
Each request has a 30 second timeout and names the surface
(`X-Sage-Surface: cli`), so a decision made here is recorded with
`decided_from=cli` in the access log and the decision-latency metric, and
with the token's owner as the approver.

Agent tokens are refused on every route the client calls; on the approve
route the refusal is `approval_reserved_for_humans` (agents can never
hold the approve scope). A token limited to some databases must name one
with `--database`. In a Databricks App the platform owns the
`Authorization` header, so these routes take a signed-in session only.

## plan

With no command, `plan` answers "what needs a person now?" for a terminal
or a CI job. It lists, in order:

1. the Inbox items waiting on a person (change approvals, plans, autonomy
   promotions, facts, runbook signatures, agent requests, policy
   exceptions);
2. the plans and changes due, which run without you: approved or running
   plans (the plan engine runs them step by step) and changes approved for
   their maintenance window;
3. held-back changes, each with the first failing link of its why-not
   chain.

If the Inbox could not read one of its sources and lists nothing, `plan`
cannot say nothing needs you, so it exits 1 and names the source.
`--json` prints the same as stable JSON (`needs_person`, `waiting`,
`due`, `held_back`).

```text
usage: pg_sage_sidecar plan [--database D] [--url U] [--json]
  With no command: list what needs a person now (Inbox items), the plans
  and changes due, and held-back changes with their first why-not link.
  Exit 0 nothing needs you, 2 something does, 1 error, 3 auth, 64 usage.
usage: pg_sage_sidecar plan <command> [flags]
  list    [--database D] [--status S] [--json]
  show    <id> [--database D] [--json]
  approve <id> --hash H (--all | --up-to N | --step N) [--database D] [--json]
  reject  <id> --reason R [--database D] [--json]
  resume  <id> --hash H [--database D] [--json]
  cancel  <id> [--reason R] [--database D] [--json]
SAGE_API_URL is the sidecar's API (https, or http to localhost);
SAGE_API_TOKEN is an operator API token (read scope to read, approve to decide).
```

Exit codes of the status form (2 is taken by "a person is needed", so a
CI gate never mistakes a typo for a pending approval):

| Exit | Meaning |
|---|---|
| 0 | Nothing needs you. |
| 1 | An error: the sidecar could not be reached or read, or an Inbox source failed. |
| 2 | Something needs a person. |
| 3 | The token was rejected, or lacks the read scope. |
| 64 | A usage error (an unknown flag, a missing value). |

The plan commands (`plan list`, `plan show`, `plan approve`, `plan
reject`, `plan resume`, `plan cancel`) are listed under the same usage and
keep their exit codes: 0 decided or read, 1 refused, 2 usage, 3 auth.

A CI job that waits for approvals:

```sh
pg_sage_sidecar plan --database orders
case $? in
  0) echo "nothing waiting" ;;
  2) echo "a person must decide first"; exit 1 ;;
  *) echo "could not ask pg_sage"; exit 1 ;;
esac
```

## approve

`approve <change-id>` prints the change's approval card: what it does,
its action, risk tier, SQL and card hash. With `--yes`, or after you type
the database name, it approves exactly that hash. If the change was edited
after it was shown, the sidecar refuses it: run `plan` again. The token
needs the approve scope.

A token approval is a remote approval, capped on the server like a chat
approval: a high-risk change, and any change above
`notification_policy.chat_approval_max_tier` (default `moderate`; `none`
refuses all), is refused with a link to approve it in pg_sage
(`#/inbox?kind=change_approval&queue=N`). A modified client cannot skip
the check. Two approvals of one change run it once; the second is told
it is no longer awaiting approval.

`approve --plan <plan-id>` prints the plan and its plan hash and approves
every step, or steps 1 to N with `--up-to N` (from 1).

```text
usage: pg_sage_sidecar approve <change-id> [--yes] [--database D] [--url U]
       pg_sage_sidecar approve --plan <plan-id> [--up-to N] [--yes] [--database D] [--url U]
  Prints the approval card (or the plan) with its hash, then approves that
  hash after --yes or the typed database name. High-risk changes, and any
  above notification_policy.chat_approval_max_tier, are approved in pg_sage.
  Exit 0 approved, 1 refused, 2 usage, 3 auth.
```

Exit codes:

| Exit | Meaning |
|---|---|
| 0 | Approved (and run, or scheduled for its window). |
| 1 | Refused: not confirmed, edited since shown, no longer awaiting approval, above the remote approval cap, or the sidecar could not be reached. |
| 2 | A usage error (a missing or non-numeric id, an unknown flag, `--up-to 0`). |
| 3 | The token was rejected, lacks the approve scope, or is an agent token. |

## apply

`apply` adds no way to run a change. `apply --plan <plan-id>` resumes a
halted plan, bound to the plan hash it printed (an approved plan is
already run step by step). `apply <change-id>` reports an approved
change: outside its maintenance window it exits 1 and names the next
window and the why-not chain's window link; inside the window it says the
change is due, and the executor's own scheduler runs it.

```text
usage: pg_sage_sidecar apply <change-id> [--database D] [--url U] [--json]
       pg_sage_sidecar apply --plan <plan-id> [--yes] [--database D] [--url U]
  A change: says when the executor's scheduler runs it (exit 1 outside its
  window, naming the next one). A plan: resumes a halted plan after --yes
  or the typed database name; an approved plan already runs step by step.
  Exit 0 applied or due, 1 refused or not yet, 2 usage, 3 auth.
```

Exit codes:

| Exit | Meaning |
|---|---|
| 0 | Resumed, due, running or done. |
| 1 | Not yet (outside its window, awaiting approval) or refused. |
| 2 | A usage error. |
| 3 | The token was rejected or lacks a scope. |

## explain

`explain` prints the why-not chain from `GET /api/v1/whynot`: ten links,
from observing the database to the emergency stop, each with its status,
sentence and fix, and the first failing link. With only `--database` it
explains the whole database (why nothing has been proposed).

```text
usage: pg_sage_sidecar explain --database D [--finding N | --object S.T]
                               [--url U] [--json]
  Prints why pg_sage has not acted: the why-not chain's ten links, from
  observing the database to the emergency stop, with the first failing one.
  Exit 0 printed, 1 refused, 2 usage, 3 auth.
```

Exit codes:

| Exit | Meaning |
|---|---|
| 0 | The chain was printed. |
| 1 | The sidecar refused or could not be reached. |
| 2 | A usage error (no `--database`, both `--finding` and `--object`). |
| 3 | The token was rejected or lacks the read scope. |

## ledger

`ledger` prints what pg_sage changed in one database over a window, one
line per change. It reads the database directly (no API token).

```text
usage: pg_sage ledger [--since 24h] [--database NAME]
           [--verdict V] [--limit N] [--json] [--dsn DSN]
Reads the pg_sage tables of one database: --dsn, else SAGE_HISTORY_MONITORED_DSN
(the monitored database), else SAGE_META_DB. --database names it in the output
(default: the DSN's database name).
```

Exit codes:

| Exit | Meaning |
|---|---|
| 0 | The ledger was read. |
| 2 | A usage error, or the database could not be read. |

## doctor

`doctor` runs the read-only preflight against one database
(`--pg-url`, `--config`, `--json`, `--send-test`) and prints each check
with its fix. See the quickstart.

Exit codes:

| Exit | Meaning |
|---|---|
| 0 | No check failed. |
| 1 | A check failed. |
| 2 | A usage error. |
