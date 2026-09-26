# Surface findings independent challenge pass

Date: 2026-09-26. Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Scope: independently challenge SURF-01, SURF-02 and SURF-03 in
`surface-audit.md` by tracing production source. References are relative to
`audit-repo`. No tests, source changes, live identity flow or live mutations
were performed in this pass. Root owns runtime verification results.

## SURF-01: retain P1, with deployment and policy prerequisites

**Confirmed.** `sidecar/internal/api/router.go:155-159` mounts the HTTP MCP
handler without `RequireRole`, while `policy_handlers.go:49` role-gates REST
policy proposal creation. The middleware wiring in
`cmd/pg_sage_sidecar/wire.go` supplies session authentication but does not turn
it into an MCP role check. `internal/mcp/production_backend.go:75` routes a
proposal to `postgres_access.go:200`, which persists it with a generic actor.
The typed intent planner and executor also do not consume the authenticated
user's role. The runtime has no later viewer-role check that closes this gap.

Counterevidence and limits:

- Authentication is still required: this is an authorization gap for a valid
  viewer session, not anonymous access.
- `internal/config/defaults.go:194-195` defaults MCP to `stdio`. The affected
  HTTP surface requires an explicit transport configuration.
- Policy proposal creation does not ratify the policy. The proposal path must
  not be described as direct policy activation or unrestricted execution.
- Actual table-contract and slot-consumer mutations still pass the standing
  policy gate and runtime checks. `internal/mcp/production_intent_executor.go:68`
  checks for an executable verdict; `internal/policy/gate.go` checks runtime,
  mode, allowed classes, approvals and other restrictions. These checks limit
  impact but do not establish caller authorization.

**Judgment:** P1 remains justified for HTTP MCP deployments because roles are
an advertised authorization boundary and the bypass reaches both persistent
proposals and permitted mutations. Retain the concrete policy prerequisites
already present in the original finding. Add default-stdio scope to avoid
implying every default installation exposes it.

## SURF-02: retain conditional P1; no OAuth protocol bypass established

**Confirmed.** `internal/auth/oauth.go:303-340` decodes the OIDC userinfo into
an email-only structure and accepts any nonempty email, ignoring verification
and durable issuer/subject identity. `internal/auth/auth.go:349-390` selects
an existing local user by that email and preserves the existing role. The
callback in `internal/api/auth_handlers.go:479-537` then creates a session for
that user. No intervening verified-email or authorized-linking check was found.

Counterevidence and limits:

- The authorization-code exchange and bound, single-use state validation are
  present in `oauth.go:149-171`. The finding does not bypass those checks.
- The attacker needs a valid identity/token accepted through the configured
  issuer whose email claim can collide with a privileged local account.
  Exploitability is therefore issuer-dependent, as the original report states.
- OAuth is disabled by default (`internal/config/config.go:933` initializes
  the default role but leaves enabled false).
- The GitHub fallback email path explicitly requires primary and verified
  email (`oauth.go:419-429`). Do not generalize this OIDC finding into a claim
  that every provider's every email path accepts unverified addresses.

**Judgment:** retain P1 with the stated issuer precondition. Persist
issuer+subject as the identity key and require deliberate authorized account
linking. Verified email is necessary for email-based linking but alone should
not grant arbitrary cross-issuer linkage. No evidence here supports weakening
the finding merely because CSRF or code-exchange checks exist.

## SURF-03: retain defect; recommend P2 for both reporting failures

**Confirmed.** Normal executor inserts in `internal/executor/executor.go:1243`
and `manual.go:325` omit database identity; the added column is nullable with
no default (`internal/schema/ddl_agent_value.go:42-44`). No attribution trigger
or production repair write was found. `internal/value/postgres.go:68-82` stamps
credit without repairing identity. Its realized-value read at `:139-163`
LEFT JOINs the missing identity and scans `d.name` into a string. The resulting
NULL scan error propagates through `value/service.go` and becomes an HTTP 500
at `internal/api/value_handlers.go:30-34`. A database filter instead excludes
that unattributed row.

The distinct topology issue is also supported: `internal/api/router.go:183-184`
constructs Value using one supplied auth/meta pool; `cmd/pg_sage_sidecar/wire.go`
selects that pool independently of target executor pools. Value has no fleet
pool aggregation analogous to the Cases handler. Separate-meta deployments
can therefore omit credit persisted on targets even after NULL scanning is
fixed.

Counterevidence and limits:

- Not every normal action immediately breaks the Value endpoint. The query
  requires both successful outcome and non-NULL toil credit. Credit itself
  requires a completed successful verification and a valid toil model.
- Manual inserts also omit `decision_id`; normal verification finalization
  requires that ID. A generic manual action is therefore not a reliable
  reproduction of the credited-row scan defect. Use a standing-policy action
  that actually completes and receives credit, or a clearly labeled fixture.
- With a distinct meta pool, the API may read no target row and show incomplete
  data rather than produce the NULL scan error. These are separate failure
  modes whose prerequisites should remain explicit.
- This finding demonstrates reporting failure and incorrect attribution; it
  does not itself show unsafe remediation, data loss, or an auth bypass.

**Judgment:** recommend splitting into two P2 findings (nullable attribution
read and fleet storage/read topology), or retaining one P2 with both causes.
P1 is warranted only with a documented critical billing/compliance dependency
or a release-specific requirement that makes a Value outage a blocker. The
original report's P1 classification is stronger than the demonstrated impact
relative to the two identity/authorization findings.

## Result

No counterevidence overturns any of the three source findings. Scope SURF-01
to HTTP deployments, preserve SURF-02's issuer-dependent exploitation condition,
and reduce SURF-03 to a functional/reporting priority unless additional product
criticality is documented. The original source files and original finding
report were not modified.
