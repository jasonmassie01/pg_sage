# Product decision ledger

Deferred product decisions from `reviews/2026-09-26/MASTER-SPEC.md` §10.5, decided by
Claude (instruction from jmass, 2026-09-27) through one lens:

> **pg_sage gains trust, then becomes autonomous.**

Every decision should help pg_sage demonstrate safety with evidence, earn autonomy, and then
act unattended within what it has earned. Human approval is the route to autonomy, not a
permanent crutch. Prefer precise enforcement of intent over broad bans, and declarations
from owners that let pg_sage act safely over guesses.

Each decision gets an evidence memo (file:line) and a verdict with reasoning. It is then
implemented test-first. One-way doors (data deletion, the meaning of stored policy,
security posture) keep a migration that preserves today's effective behaviour.

| ID | Topic | Memo | Decision | Door | Status |
|---|---|---|---|---|---|
| D1 | Enforce RefusalSet / LockDurationCeilingMS / SerializeMode | D1-policy-refusal-lock-serialize.md | pending evidence | | investigating |
| D2 | Unify maintenance-window grammars | D2-maintenance-window-grammar.md | pending evidence | | investigating |
| D3 | Value ledger topology across fleets | D3-value-ledger-topology.md | Per-database ledger; every reader (Value page, metrics, MCP) collects across the fleet, meta-db included; credited rows are kept, not purged | two-way | implementing |
| D4 | AgentDB human tenant scoping; approval before cloud registration | D4-agentdb-tenancy-and-registration.md | pending evidence | | investigating |
| D5 | Owner-declared retention column | D5-retention-column.md | **Option A:** owner declares `retention_column`; no backfill; legacy contracts park with pg_sage's suggested column until the owner confirms; dry runs bound to OID, attnum, type and contract version | one-way action, two-way schema | implementing |
| D6 | Provider disk/WAL IO as capacity evidence | D6-provider-io-capacity.md | Earned admission: IO/WAL rates from Postgres (`pg_stat_io`/`pg_stat_wal`); after a 7-day learned baseline, admit when current load is at or below the baseline median. Declared capacity overrides the baseline. Unknown CPU needs a maintenance window. The withheld reason is visible | one-way (default loosening; heads-up given) | implementing |
| D7 | OIDC account linking | D7-oidc-account-linking.md | pending evidence | | investigating |
| D8 | E-stop control in the dashboard header | D8-estop-header-control.md | pending evidence | | investigating |

## Reasoning

**D5.** Retention delete is irreversible, and the catalog cannot tell which timestamp the
owner means. pg_sage earns the right to delete by acting on the owner's declaration, not on a
name guess. It still helps: when a contract is parked, the park reason carries pg_sage's
suggested column, so the owner confirms instead of researching (suggest, then confirm,
then act unattended). The failure mode is a pause, never a wrong delete.

**D3.** Measured value is how pg_sage shows it has earned trust. A ledger that shows one
database, or zero in meta-db, or shrinks after a year undermines the case for autonomy.
Keep it next to the action it credits (per-database, the same pattern the Cases API already
uses), read it everywhere, and never purge credited evidence.

**D6.** Today no source produces IO evidence, so autonomous index builds are withheld for
everyone. pg_sage can never earn that autonomy. Declared capacity alone (the memo's option C)
still leaves nearly everyone off. pg_sage should earn admission the way it earns trust:
observe the database's own IO/WAL rates, learn its normal, and act only in its quiet
periods. Where the operator knows the provisioned capacity, their attestation is stronger
evidence and wins. Missing CPU evidence is not ignored: it narrows action to maintenance
windows.
