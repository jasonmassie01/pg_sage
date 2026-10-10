# External review: Gemini on the Agent Guard spec (draft 2)

**Date:** 2026-10-05.
**Model:** `gemini-3.1-pro-preview`, through Gemini CLI 0.39.1, as `/write-spec` Phase 4 requires.
**Input:** `AGENTDB-SPEC.md` draft 2, before the last self-fixes (citations, the `sage_guard`
schema, restore points). Those self-fixes don't touch the points below.

**How it was run.** The house command, with one change:
- The CLI's configured sign-in (`oauth-personal`, Gemini Code Assist for individuals) now fails
  with `IneligibleTierError: UNSUPPORTED_CLIENT`.
- This run pointed `GEMINI_CLI_HOME` at an empty scratch folder and passed `--skip-trust`, so the
  CLI used the `GEMINI_API_KEY` already in the environment.
- `~/.gemini/settings.json` was not changed. Future `/write-spec` runs need the same workaround,
  or a switch of the configured auth type to `gemini-api-key`.

```
cat AGENTDB-SPEC.md | GEMINI_CLI_HOME=<empty dir> gemini --skip-trust -m gemini-3.1-pro-preview \
  -p "Review this PostgreSQL tool spec critically. … (Postgres semantics, security bypasses,
      internal inconsistencies, failure modes, scope for a solo-maintained project; P0/P1/P2
      with section, problem and fix; end with the five most important changes)"
```

## Disposition

Each finding was checked against the code at `72646ab1` and against PostgreSQL behaviour before
it was accepted.

| # | Gemini finding (severity) | Verdict | Change in the spec |
|---|---|---|---|
| GR-01 | The kill switch can't reach replicas from `pg_stat_replication` alone: there's no routable DSN and no credentials (P0) | **Accepted.** pg_sage has no replica DSN configuration today | §6.10 step 6: terminate on configured replicas only (`databases[].replicas`). `NOLOGIN` replicates to standbys, so new logins fail everywhere. Residual sessions are bounded by `idle_session_timeout` and `transaction_timeout` on agent roles. The brokered lane routes only to configured replicas. G1-05 rewritten (the direct lane is G3); new G3-11 |
| GR-02 | Daily point-in-time drills are costly and flap on wall-clock skew (P0) | **Partly accepted.** Drills ran only on clone substrates, never on full-instance restores, and compared only quiet tables. But the timing window and the cost of a daily run were real | §6.12: drills are weekly (`clone.drill_interval_days: 7`), copy-on-write substrates only (`neon`, `lakebase`, `dle`), and T is 10 minutes in the past, with tables quiet from T − 1 h until the checksum. `require_restore_drill_days` is 14. Without such a substrate, `prod` writes cap at L2 (unchanged). Drills are **kept**: SAFE-BAK-04 comes from incidents |
| GR-03 | `EXPLAIN` of `pg_stat_statements` text fails on `$1` placeholders before PG16 (P1) | **Valid concern, already handled in code** | §6.13 step 4 names the mechanism: PG16+ `EXPLAIN (GENERIC_PLAN)` (`internal/autoexplain/collector.go:218`); PG14/15 `PREPARE` plus `EXPLAIN EXECUTE` with NULLs under `force_generic_plan` (`internal/optimizer/hypopg_generic.go:15-16`) |
| GR-04 | `DROP CONSTRAINT` and `DROP INDEX` aren't classified (P1) | **Accepted** | §5.2 and §6.13: dropping a constraint or a unique index is `ddl_destructive`. Dropping a non-unique index is `ddl_locking`. The change path records `pg_get_constraintdef` and `pg_get_indexdef` as the `down` step |
| GR-05 | Taint expires after 60 minutes, so an agent can wait it out (P0) | **Accepted, modified.** "Until an operator clears it" alone would make every reader of a ticket table permanently approval-bound | §6.10: taint has no time expiry. It clears on a trusted context boundary (a new stdio process, the end of an E2 trusted task), an operator clear (recorded), or a credential rotation. `agents.taint_ttl_minutes` is replaced by `agents.taint.clear_on`. G2-05 adds the waiting case |
| GR-06 | The capture function is an injection risk if it builds SQL from input (P0) | **Accepted** as a normative rule (the draft didn't say how the query is built) | §6.9: identifiers come only from the catalog through the `regclass` OID and the primary key's `pg_index.indkey`, quoted with `format('%I')`. Key values are cast to the column types and bound with `EXECUTE … USING`. New check G2-18 fuzzes the function |
| GR-07 | Direct-lane credentials can leave through a compromised runtime (P1) | **Accepted** | §5.1: the direct lane is stated to be lower assurance than the brokered lane. It uses cloud IAM database auth by default, or a credential the operator's own tooling manages. pg_sage never holds or delivers a direct-lane password (see GR-11) |
| GR-08 | `columns` NULL in `branch`/`dev` grants re-exposes `secret` columns (P1) | **Accepted**: a real inconsistency with "secrets are never selectable" | §6.7 and §7: `columns` is never NULL. `branch`/`dev` grants default to every column not classified `secret`, extended by the reconciler on catalog change |
| GR-09 | An L0 outcome could hide the D-step reason (P2) | **Accepted** (clarification) | §6.2.3: a D-step reason always wins. `agent_level0` is used only when the ledger itself yields L0 |
| GR-10 | G4 chargeback is a FinOps engine a solo maintainer can't sustain (P0, scope) | **Accepted, modified.** FO-1 (cost attribution) is a MUST in the enterprise research | §6.14: G4 records provider-reported usage and cost as reported, never prices anything, and exports it for FinOps tools. Budgets use the provider's own units. `guard_estate_usage` keeps only reported fields |
| GR-11 | Secrets-manager integrations for the direct lane are a distraction (P1, scope) | **Accepted, modified.** Gemini's fix (credentials in the control DB, synced by the customer) would break SAFE-ID-03 | §6.4 and E3: no Vault, AWS SM or GCP SM integration for agent credentials. Direct-lane login is IAM auth or an operator-managed credential. E1's own-credential reads use `*_FILE`, which works with Vault Agent, CSI drivers and External Secrets |
| GR-12 | LLM narratives add latency and hallucination risk; keep only classification (P2, scope) | **Rejected in part.** The product principle (LLM features on by default) is your decision | §6.16: LLM output is generated asynchronously, never in the gate path, never blocks a card, and the structured card stays primary |

## Raw review (verbatim)

Here is a critical, adversarial review of the Agent Guard specification. As requested, this focuses on Postgres semantics, security boundaries, operational realities, and scope management for a solo-maintained open-source project.

### 1. PostgreSQL Semantics & Operations

*   **P0: Replicas and the Kill Switch (§6.10).**
    *   *Problem:* Step 6 mandates terminating backends "on the primary and on each replica... enumerated through `pg_stat_replication`." You cannot dynamically connect to a physical read replica (like an Aurora Reader or RDS standby) just by looking at `pg_stat_replication`. It only provides the internal IP of the replication stream, not a routable DSN, port, or connection pool endpoint. Furthermore, `pg_sage` would need stored credentials to authenticate against every replica.
    *   *Fix:* Drop the requirement to dynamically hunt down replica connections. Rely on the `ALTER ROLE ... NOLOGIN` (which automatically replicates to standbys to prevent *new* connections). For existing connections, document that they will bleed out via `idle_in_transaction_session_timeout` or connection pool TTLs, or allow users to explicitly configure replica DSNs in `agents.replica_dsns`.
*   **P0: Restore Drills are Flaky and Financially Ruinous (§6.12).**
    *   *Problem:* You propose running daily point-in-time restores (PITR) for every `prod` database to verify checksums at `T = now()`. First, restoring an RDS/Cloud SQL instance takes 15–40+ minutes and provisions a brand-new billable instance (costing compute + I/O). Doing this daily will generate massive cloud bills for users. Second, provider wall-clock PITR (e.g., AWS RDS) is often only precise to the nearest second. A Postgres `REPEATABLE READ` snapshot at `now()` will not perfectly align with the provider's restored wall-clock snapshot. The checksums will constantly flap, locking agents out of L3.
    *   *Fix:* Remove automated daily provider PITR drills (G3-06). Rely on the metadata check (AP-11: backup posture is enabled). If drills must exist, make them strictly manual/opt-in via UI, and do not use exact checksum matching against `now()`.
*   **P1: `EXPLAIN` on `pg_stat_statements` for Rehearsals (§6.13, Step 4).**
    *   *Problem:* The B1 evidence requires replaying the top 20 queries from `pg_stat_statements` via `EXPLAIN`. `pg_stat_statements` stores parameterized queries (e.g., `SELECT * FROM t WHERE id = $1`), but *not* the parameter values. Running `EXPLAIN` on these in PG14/PG15 will throw syntax errors (`there is no parameter $1`). `EXPLAIN (GENERIC_PLAN)` is only available in PG16+.
    *   *Fix:* Restrict this feature to PG16+ using `GENERIC_PLAN`, or drop it entirely and rely solely on the Atlas/Bytebase handoff for change-review metrics.
*   **P1: Missing Destructive DDL Classification (§6.13).**
    *   *Problem:* The delayed drop (trash) logic covers `DROP TABLE`, `DROP SCHEMA`, and `DROP COLUMN`. It ignores `DROP CONSTRAINT` and `DROP INDEX`. An agent dropping a `UNIQUE` constraint or foreign key can silently cause massive data corruption, and it bypasses the trash map completely.
    *   *Fix:* Explicitly add `DROP CONSTRAINT` to the `ddl_destructive` class (requiring two humans). Add `DROP INDEX` to `ddl_locking`.

### 2. Security Holes & Bypasses

*   **P0: Taint Bypass via Sleep (§6.10).**
    *   *Problem:* "An `agent_query` that touches an `untrusted_input` table taints the principal for `agents.taint_ttl_minutes` (60)." An adversarial agent can ingest a prompt injection, realize it is tainted, deliberately sleep for 61 minutes, and then execute a malicious L3 write autonomously once the taint expires.
    *   *Fix:* Taint must **not** be time-bound. It must persist until explicitly cleared by an operator, or until the specific trusted `TaskID` is completed/rotated by the upstream orchestrator.
*   **P0: SQL Injection in Pre-image Capture (§6.9).**
    *   *Problem:* The `sage.guard_capture` function takes a `jsonb` array of primary keys and executes a dynamic `SELECT` inside a `SECURITY DEFINER` function owned by a privileged role. Safely unpacking dynamic JSONB into a dynamic `WHERE (col1, col2) IN (...)` clause in PL/pgSQL is a notorious vector for SQL injection if the column names aren't strictly validated against `pg_attribute`.
    *   *Fix:* Explicitly mandate in the spec that `guard_capture` uses the `format('%I', colname)` function against securely looked-up catalog attributes, or abandon dynamic JSONB queries in favor of matching via `WHERE ctid = ANY(...)` if captured within the same transaction.
*   **P1: Direct Lane Secrets Exfiltration (§6.4).**
    *   *Problem:* "Direct-lane credentials... Delivered only through a secrets manager... The agent runtime reads them." If the agent runtime is compromised or goes rogue, it holds the raw credentials. It can connect outside of the `application_name` wrapper and bypass the L0 watchdog if it figures out the DB host.
    *   *Fix:* This isn't entirely solvable if you give out passwords, but you must document that direct-lane is intrinsically lower security than the brokered lane. If direct-lane is strictly required, strongly recommend IAM database auth (short-lived tokens) over Vault/SM static passwords.

### 3. Inconsistencies & Logic Flaws

*   **P1: Column Grants in Branch/Dev vs. Secret Masking (§6.7 & §7).**
    *   *Problem:* §7 states that `columns text[]` in `sage.guard_grants` is "NULL only in branch/dev" (meaning all columns are granted). However, §6.7 says masking is just a convenience, and "the control is column privilege," meaning a `secret` column must be blocked by privileges. If the grant array is NULL, Postgres will allow access to the secret column.
    *   *Fix:* `pg_sage` must dynamically intersect the table's columns with classification facts at evaluation time (D5), or explicitly expand the column list for `branch/dev` grants, omitting `secret` columns. Do not use NULL for branch/dev grants.
*   **P2: Gate Order Conflict (§6.2.2 vs §6.2.3).**
    *   *Problem:* In the normative evaluation order, Step A5 caps the level. Step D10 handles lease expiry mapping to `agent_lease_expired`. If a request evaluates to L0 because of an expired lease, the verdict mapping (§6.2.3) says L0 maps to `agent_level0`, swallowing the `agent_lease_expired` reason code.
    *   *Fix:* Specify that D-step failure reasons (like `agent_lease_expired`) take precedence over the generic `agent_level0` reason code in the final response.

### 4. Over-engineering & Scope Creep (Solo Developer Context)

*   **P0 (Scope): G4 Agent Estate Chargeback & FinOps (§6.14).**
    *   *Problem:* Polling provider APIs for USD costs, tracking `storage_bytes`, and implementing a cross-cloud FinOps chargeback engine is massive scope creep. You are building an AI Postgres guard, not an alternative to Datadog/CloudZero. Keeping up with AWS/GCP/Neon billing API changes will drown a solo maintainer.
    *   *Fix:* **Cut `guard_estate_usage` entirely.** Leave cost attribution and USD calculation to the cloud providers. Keep estate operations limited strictly to lifecycle (suspend/reclaim via TTL) and discovery.
*   **P1 (Scope): Secrets Manager Integrations for Direct Lane (§6.4 & E3).**
    *   *Problem:* Integrating Vault KV, Vault Dynamic, AWS SM, and GCP SM just to hand an agent a direct connection string is a distraction from core gate logic.
    *   *Fix:* Drop the built-in Secrets Manager sync. Just generate the credentials in the control DB. It is the enterprise's job to use External Secrets Operator or custom scripts to sync `sage.guard_cluster_roles` to their Vault.
*   **P2 (Scope): LLM Narratives (§6.16).**
    *   *Problem:* Generating Ask Sage narratives and anomaly triage notes using an LLM inside the hot path of UI rendering or evaluation is prone to latency and hallucination.
    *   *Fix:* Keep the LLM strictly to classification proposals. Rely on the "deterministic fallbacks" (structured cards and tables) as the permanent primary UI, not just a fallback.

---

### Top 5 Most Important Changes

1.  **Cut the G4 Chargeback/FinOps Scope:** Remove all USD tracking and cloud billing API integrations. It will kill the project's momentum and maintenance capacity.
2.  **Fix the Kill Switch on Replicas:** Drop the requirement to dynamically connect to replicas via `pg_stat_replication`. It is operationally impossible without explicit user-provided DSNs for every replica endpoint.
3.  **Drop Automated Daily PITR Drills:** Rely on API metadata (backups enabled). Daily restores will cost users thousands of dollars and fail constantly due to wall-clock vs. snapshot misalignment.
4.  **Fix the Taint Bypass:** Change `agents.taint_ttl_minutes: 60` to indefinite until manually cleared or the specific upstream `TaskID` is rotated. Do not let agents sleep off their taint.
5.  **Fix B1 Rehearsal EXPLAIN:** `EXPLAIN` on `pg_stat_statements` queries will fail without parameter values. Explicitly require PG16+ `GENERIC_PLAN` for this feature, or drop it.
