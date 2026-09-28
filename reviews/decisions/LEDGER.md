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
| D3 | Value ledger topology across fleets | D3-value-ledger-topology.md | pending evidence | | investigating |
| D4 | AgentDB human tenant scoping; approval before cloud registration | D4-agentdb-tenancy-and-registration.md | pending evidence | | investigating |
| D5 | Owner-declared retention column | D5-retention-column.md | **Option A:** owner declares `retention_column`; no backfill; legacy contracts park with pg_sage's suggested column until the owner confirms; dry runs bound to OID, attnum, type and contract version | one-way action, two-way schema | implementing |
| D6 | Provider disk/WAL IO as capacity evidence | D6-provider-io-capacity.md | pending evidence | | investigating |
| D7 | OIDC account linking | D7-oidc-account-linking.md | pending evidence | | investigating |
| D8 | E-stop control in the dashboard header | D8-estop-header-control.md | pending evidence | | investigating |

## Reasoning

**D5.** Retention delete is irreversible, and the catalog cannot tell which timestamp the
owner means. pg_sage earns the right to delete by acting on the owner's declaration, not on a
name guess. It still helps: when a contract is parked, the park reason carries pg_sage's
suggested column, so the owner confirms instead of researching (suggest, then confirm,
then act unattended). The failure mode is a pause, never a wrong delete.
