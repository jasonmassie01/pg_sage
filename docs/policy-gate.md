# Policy gate: verdicts and reasons

Every change pg_sage could make passes one policy gate before it runs: index builds and
drops, setting changes, vacuums, Sage SRE cancels, reverts, operator-approved actions and
agent requests alike. The gate answers with a **verdict** and a **reason**, records both in
the decision ledger (`sage.decision`), and the executor acts only on an `execute` verdict.

This page is the reference for what each verdict and reason means, what clears it and where
you see it. Configuration lives in [Configuration](configuration.md): the trust settings in
[Trust & Actions](configuration.md#trust-actions), the standing policy in
[Agent-native autonomy](configuration.md#agent-native-autonomy), budgets in
[Blast-radius budgets](configuration.md#blast-radius-budgets) and the verification wait in
[One change per object](configuration.md#one-change-per-object). Agent posture, which
reports how exposed a database is to AI agents, is a separate check: see
[Agent governance](agent-governance.md).

In a fleet, the standing policy a database's gate reads comes from its
[policy set](policy-sets.md), or from its own document when it has an exception.

The gate fails closed. Whatever it cannot read (the runtime state, the standing policy, the
usage counters, the confirmed facts, the autonomy ledger, the in-flight verifications) turns
into a `blocked` verdict, never into an `execute`.

## Verdicts

| Verdict | Ledger spelling | Meaning | What happens next |
|---|---|---|---|
| `execute` | `execute` | The action may run now. | The executor takes the change lease and runs it. Each execute keeps its own ledger row. |
| `queue_approval` | `queue_approval` | A person must decide. | The action gets one item in the approval queue, with an approval card in the UI and in ChatOps. |
| `park` | `parked` | Not now; retry on a later cycle. | Nothing runs. A park is not a failure: it does not count toward the retry limit or the self-initiated rate budget. |
| `blocked` | `blocked` | Refused until the condition in the reason changes. | Nothing runs. The reason and detail say what to change. |
| `observe_only` | `observe_only` | pg_sage only watches and proposes. | Nothing runs. The proposal is shown as a manual script. |

The ledger spells the park verdict `parked`; everywhere else (API, logs, metrics labels) the
verdict is `park`. A recorded decision is often written `verdict/reason`, for example
`parked/no_typed_contract`.

## Evaluation order

The gate evaluates in this order and stops at the first step that decides. Later steps can
only restrict an earlier `execute` or `queue_approval`; nothing after step 1 turns a
withheld verdict into `execute`.

1. **Runtime state.** Trust level, execution mode, emergency stop and replica status are
   read first. If they cannot be read: `blocked` `policy_unavailable`.
2. **Hard stops.** A disabled executor (`executor_disabled`), the emergency stop
   (`emergency_stop`) and a mutation on a replica (`replica_mutation`) block everything,
   except a narrowing contract (step 6).
3. **Request validation.** No typed contract, or SQL that fails parse-tree validation,
   parks with `no_typed_contract`. A guardrail the gate does not know blocks with
   `unknown_guardrail`.
4. **Provider.** A contract that lists the providers it supports blocks on any other with
   `provider_unsupported`.
5. **Confirmed facts.** A mutation that a confirmed fact binds blocks with `bound_by_fact`;
   facts that cannot be read block with `facts_unavailable`. Read-only diagnostics and
   reverts of pg_sage's own changes are never bound. See [Binding facts](facts.md).
6. **Narrowing.** A contract that only takes access away runs at any trust level, outside
   the standing policy, budgets and windows: `execute` `narrowing`, or
   `narrowing_during_stop` when a hard stop holds everything else back.
7. **Agent requests.** A request an agent made (through MCP) takes the agent path: the
   agent decider's checks, then the agent's level mapped to a verdict (see
   [Agent requests](#agent-requests)).
8. **Reverts.** The revert of a change that already ran is a mitigation: `execute`
   `rollback_mitigation`, outside windows, budgets, per-class autonomy and the change-class
   allowlist. The observation trust level, the rollback contract's approval guardrail and,
   for an automatic revert, the approval classes and refusal set still apply.
8a. **Exposure mode.** A forward `revoke_public_grant` or `enable_row_level_security`
   (see [Exposure remediation](#exposure-remediation)) is `observe_only`
   `exposure_shadow` unless the policy document's `exposure.mode` is `enforce`, an
   operator-approved one included. Their re-GRANT or RLS disable is refused
   (`blocked` `refused_by_policy`) unless it is the revert of a recorded action (step 8).
9. **Operator-approved actions.** A person's approval replaces the tier flags, the trust
   ramp, the execution mode and the self-initiated budgets. Trust, the change-class
   allowlist and the maintenance windows still bind: `execute` `operator_approved`.
10. **Soft-drop restore.** Re-creating an index pg_sage dropped itself runs past a trust
    downgrade: `execute` `drop_restore_bypass`. The standing policy, the refusal set and
    degraded SQL validation still bind.
11. **Observation.** `trust.level: observation` or `execution_mode: manual` gives
    `observe_only` `observe_only`. Any trust level other than `observation`, `advisory` or
    `autonomous` blocks with `unknown_trust_level`.
12. **Standing policy.** An unreadable or invalid policy document blocks with
    `policy_unavailable`. Then: a change class outside `allowed_change_classes` blocks with
    `change_class_not_allowed`; the `approval_required` guardrail, a backend cancel or
    terminate, or a class in `approval_required_classes` queues with `approval_required`;
    the usage limits park with `budget_exceeded`, `blast_radius_exceeded` or
    `rate_limit_exceeded`. Read-only diagnostics need a readable policy and skip the rest.
13. **Trust and tier.** `execution_mode: approval` queues with `approval_required`. Then by
    risk tier: `read_only` runs (`authorized`); `safe` runs when `trust.tier3_safe` is on
    and the trust ramp has elapsed, otherwise `trust_ramp_not_satisfied`; `moderate` runs
    under `autonomous` trust with `trust.tier3_moderate` on and the ramp elapsed, and
    queues under `advisory`; `high` always queues. A tier outside the four blocks with
    `unknown_risk_tier`.
14. **Restrictions on an execute.** The refusal set queues with `refused_by_policy`; a build
    without parse-tree SQL validation queues with `sql_validation_degraded`; a `moderate` or
    `high` action outside every maintenance window blocks with `outside_maintenance_window`,
    and one inside a window that closes within `window_start_margin_minutes` blocks with
    `window_closing`, unless an allowed XID or disk deadline overrides it (`execute`
    `deadline_override`).
15. **Earned autonomy.** For an action class the autonomy ledger governs, the earned level
    restricts an execute or approval verdict: `autonomy_level` and `autonomy_downgraded`
    (observe only), `autonomy_handoff` (approval), `autonomy_l3` (execute, a human is
    notified). A ledger that cannot answer blocks with `autonomy_unavailable`. A critical XID
    or disk deadline the policy lets override skips this step.
16. **One change per object.** A self-initiated execute on an object whose previous change
    is still being verified parks with `awaiting_verification`; an approval stays queued with
    the wait in its detail; an operator approval runs and records the override.
17. **Record.** The decision is written to `sage.decision`. If it cannot be: `blocked`
    `policy_unavailable`, with a detail naming the decision it meant to record.

After an `execute`, the executor takes the change lease on the action's objects. A lease
another writer holds parks a self-initiated action with `ddl_conflict` and refuses an
operator action (`blocked` `ddl_conflict`); see `serialize_mode` in
[Agent-native autonomy](configuration.md#agent-native-autonomy).

## Reasons

"Shows" says where the reason appears:

- **Ledger**: every decision, in `sage.decision.reason`, with the free-text `detail` in its
  evidence. Every reason below is recorded there.
- **Card**: the approval card of a queued action lists the reason as `gate:<reason>`
  ("Policy gate: ...").
- **Metric**: `pg_sage_policy_parks_total{database,reason}` counts recorded parks;
  `pg_sage_policy_record_failures_total{database,reason}` counts decisions that could not
  be recorded, labelled with the reason they meant to record (any reason can appear there).

### Allowed to run

| Reason | Verdict | Meaning | Shows |
|---|---|---|---|
| `authorized` | `execute` | Trust, tier and ramp allow the action unattended. | Ledger |
| `operator_approved` | `execute` | A person approved the action. | Ledger |
| `deadline_override` | `execute` | Outside every window, but an XID or disk deadline the policy lets override is near. | Ledger |
| `narrowing` | `execute` | The contract only takes access away. | Ledger |
| `narrowing_during_stop` | `execute` | A narrowing contract ran while a hard stop held everything else. | Ledger |
| `rollback_mitigation` | `execute` | The revert of a change that already ran. | Ledger |
| `drop_restore_bypass` | `execute` | The soft-drop restore of an index pg_sage dropped itself ran past a trust downgrade; the detail names each downgrade it passed. | Ledger |
| `autonomy_l3` | `execute` | The class has earned L3: a reversible, single-object action inside the window; a human is notified. | Ledger |

### Withheld until a person decides

| Reason | Verdict | Meaning | What clears it | Shows |
|---|---|---|---|---|
| `approval_required` | `queue_approval` | The tier, `execution_mode: approval`, the `approval_required` guardrail, an approval class or a backend signal needs a human. | Approve or reject the card. | Ledger, Card |
| `refused_by_policy` | `queue_approval` | The statement matches a `refusal_set` token; the detail names it. | Approve the card, or remove the token. | Ledger, Card |
| `sql_validation_degraded` | `queue_approval` | The build lacks parse-tree SQL validation, so unattended mutations go to a person. | Approve the card, or run a cgo build. | Ledger, Card |
| `autonomy_handoff` | `queue_approval` | The class is at L2, or an L3 condition does not hold; the detail says which. | Approve the card; approvals earn the class trust. | Ledger, Card |

### Parked: retried on a later cycle

| Reason | Meaning | What clears it | Shows |
|---|---|---|---|
| `no_typed_contract` | No typed contract, or the SQL failed validation; recorded at risk tier `high`. | Nothing unattended: such a candidate never runs on its own. | Ledger, Metric |
| `budget_exceeded` | The storage budget is spent. | Usage falling below `budgets.storage_bytes`. | Ledger, Metric |
| `blast_radius_exceeded` | Rows rewritten, or tables changed in the window, are at the limit; the detail says when a slot frees. | The rolling window moving on. | Ledger, Metric |
| `rate_limit_exceeded` | Self-initiated changes in the window are at the limit; the detail says when a slot frees. | The rolling window moving on. | Ledger, Metric |
| `awaiting_verification` | Another change to the same object is still being verified; the detail names it and until when. | Its verdict, its hard deadline, or an operator approval. | Ledger, Metric |
| `ddl_conflict` | Another writer holds the change lease on the object; `evidence.lease_queue` says how a queue wait ended. | The holder finishing. | Ledger |

`ddl_conflict` is recorded by the executor after the gate, so it is not counted in
`pg_sage_policy_parks_total`. The same reason refuses an operator action (`blocked`), with
`evidence.refused_decision_id` naming the decision it replaced.

### Blocked: refused until the condition changes

| Reason | Meaning | What clears it | Shows |
|---|---|---|---|
| `executor_disabled` | The executor is off. | Enabling the executor. | Ledger |
| `emergency_stop` | The emergency stop is engaged. | Releasing it ([Emergency stop](security.md#emergency-stop)). | Ledger |
| `replica_mutation` | The target is a replica; only read-only diagnostics run there. | Nothing: mutations run on the primary. | Ledger |
| `policy_unavailable` | Something the gate must read could not be read, or the decision could not be recorded; the detail says which. | Fixing the read (connection, `sage` schema, policy document). | Ledger, Metric |
| `unknown_guardrail` | The contract names a guardrail the gate does not know; the detail names it. | A contract fix (a pg_sage bug). | Ledger |
| `unknown_trust_level` | `trust.level` is not `observation`, `advisory` or `autonomous`; the detail has the value. | Setting a valid trust level. | Ledger |
| `unknown_risk_tier` | The contract's tier is not `read_only`, `safe`, `moderate` or `high`. | A contract fix (a pg_sage bug). | Ledger |
| `change_class_not_allowed` | The change class is not in `allowed_change_classes`. | Adding the class to the standing policy. | Ledger |
| `trust_ramp_not_satisfied` | `trust.tier3_safe` or `trust.tier3_moderate` is off, or the trust ramp has not elapsed. | The flag on and the ramp elapsed. | Ledger |
| `outside_maintenance_window` | A `moderate` or `high` action outside every maintenance window. | The next window. | Ledger |
| `window_closing` | A `moderate` or `high` action inside its window, but the window closes in less than the policy's `window_start_margin_minutes`, so the change would run past it. | The next window. | Ledger |
| `provider_unsupported` | The action does not support the database's provider; the detail names it. | Nothing on this provider. | Ledger |
| `target_mismatch` | A self-initiated finding or custodian proposal whose statement changes another relation than the one it names; the detail names both. | Nothing: such a statement never runs unattended (a pg_sage bug or a tampered proposal). | Ledger |
| `bound_by_fact` | A confirmed fact binds the request; the detail names the fact, who confirmed it and where the work goes instead. | Rejecting or expiring the fact. | Ledger |
| `facts_unavailable` | The confirmed facts could not be read. | Fixing the read. | Ledger |
| `autonomy_unavailable` | The autonomy ledger could not answer. | Fixing the read. | Ledger |

### Observe only

| Reason | Meaning | What clears it | Shows |
|---|---|---|---|
| `observe_only` | `trust.level: observation` or `execution_mode: manual`. | Raising trust or the mode. | Ledger |
| `exposure_shadow` | An exposure remediation while the policy document's `exposure.mode` is `shadow` (the default). The finding records the would-be action. | Setting `exposure.mode: enforce`. | Ledger |
| `autonomy_level` | The class has not earned L2; the proposal is shown as a manual script. | The class earning its level. | Ledger |
| `autonomy_downgraded` | A downgrade signal holds (error budget, failover, stale evidence, concurrency). | The signal clearing. | Ledger |

### Agent requests

An agent's request runs the agent decider first, in steps D1 to D10. The first step that
refuses decides, and its reason is recorded the same way as the gate's; the detail ends with
the fix when there is one.

| Reason | Step | Verdict | Meaning | What clears it | Shows |
|---|---|---|---|---|---|
| `agent_retired` | D1 | `blocked` | The agent is retired, its status is unknown, or it does not exist. | Nothing: register the agent again. | Ledger |
| `agent_frozen` | D1 | `blocked` | The agent is frozen, or agent changes are frozen for the database or the fleet. Reads still work. | An admin lifting the freeze. | Ledger |
| `agent_level0` | D1 | `blocked` | The tool is neither a proposal nor an `agent_*` capability. | Nothing: use a proposal or capability tool. | Ledger |
| `agent_unsponsored` | D2 | `blocked` | An `agent_*` request from an agent with no active sponsor. Its proposals still queue at L2. | An admin assigning an active sponsor. | Ledger |
| `agent_env_ceiling` | D3 | `blocked` | The database's environment is above the agent's ceiling (the lower of the agent's and its profile's); an unknown environment counts as prod. | Correcting the environment label, or two admins raising `env_ceiling`. | Ledger |
| `agent_capability` | D4 | `blocked` | The capability class is unknown, or the agent's profile is not configured or does not allow it. | An admin adding the class to the profile (two people). | Ledger |
| `agent_classification` | D5 | `blocked` | An object does not resolve, or its classification or grants refuse the request; the detail says which. | The fix in the detail. | Ledger |
| `agent_change_freeze` | D6 | `blocked` | A change freeze is on for the database. | The operator lifting the change freeze. | Ledger |
| `agent_no_pitr` | D7 | `blocked` | A write or DDL in prod on a database with no point-in-time recovery, or whose recovery posture is unknown. | Enabling PITR (WAL archiving or the provider's). | Ledger |
| `agent_budget` | D9 | `blocked` | The agent already has its limit of approvals pending. | People deciding the pending approvals. | Ledger |
| `agent_rate` | D9 | `park` | More requests in the last hour than the agent's limit; the detail has `retry_after`. | The hour moving on. | Ledger, Metric |
| `agent_lease_expired` | D10 | `blocked` | A brokered use of a grant whose lease has expired, or with no lease registry. | Requesting the capability again; a person approves the new grant. | Ledger |
| `agent_governance_unavailable` | any | `blocked` | Agent governance could not read what it needs (principal, freezes, objects, recovery, leases). | Fixing the control database connection. | Ledger |

Freezes and the kill switch are described in
[Agent governance](agent-governance.md#kill-switch-freeze-and-unfreeze). If the decider allows
the request, the gate maps the agent's level, capped by the operator's trust level and the
action's rollback class, to a verdict:

| Reason | Verdict | Meaning | Shows |
|---|---|---|---|
| `agent_level0` | `blocked` | The agent's level for this request is L0. | Ledger |
| `agent_proposal_recorded` | `observe_only` | L1: the request is recorded as a proposal. | Ledger |
| `approval_required` | `queue_approval` | L2 or above: a person approves it. | Ledger, Card |
| `agent_l3_envelope` | `execute` | L3 inside its envelope. Reserved: this release has no L3 envelope, so an L3 request queues as L2. | Ledger |

`change_class_not_allowed`, `policy_unavailable` and `unknown_trust_level` apply to agents
as above. An operator-approved agent request at L2 or above takes the operator path.

## Plan caps

The gate checks one action at a time. A plan runs many, so the standing policy document's
`plans` block bounds what plans concentrate. Every step still goes through the gate as above.

```json
"plans": {
  "max_new_indexes_per_table": 2,
  "max_new_index_bytes_pct": 10,
  "max_restart_bound_steps": 1,
  "peak_windows": ["weekdays 09:00-17:00 America/Chicago"]
}
```

| Field | Default | Range | What it bounds |
|---|---|---|---|
| `max_new_indexes_per_table` | 2 | 1-10 | Indexes the running plans of a database build on one table. |
| `max_new_index_bytes_pct` | 10 | 1-50 | Bytes of the indexes the running plans built, as a percentage of `pg_database_size`. |
| `max_restart_bound_steps` | 1 | 1 only | Steps in one plan that take effect only after a restart, operator steps included (one whose setting is unknown, such as a managed change with no landing setting, counts). A document asking for more is refused. |
| `peak_windows` | none | window expressions | Times when no plan step starts. They use the same grammar as `maintenance_windows`. |

- **Defaults.** A document without `plans`, or with `"plans": {}` or `null`, gets the
  defaults. A field you leave out takes its default. An explicit `0` or an out-of-range
  value is refused when the document is parsed, and the error names the field. The block is
  written back only when it differs from the defaults, so the stored documents keep their
  bytes and their `policy_doc_sha256`.
- **Widening.** Raising a cap, or dropping or changing a peak window, widens the policy, so
  an agent's proposal that does it needs two people (see Agent policy proposals below).
- **When a plan is proposed and approved**, it is refused if it holds more index steps on
  one table than the cap, or more than one restart-bound step. A setting whose
  `pg_settings.context` cannot be read counts as restart-bound. The refusal names the cap,
  the table and the numbers.
- **Before each step**, the engine counts the started steps of every running plan on the
  database (`approved`, `running` or `halted`; pending, skipped and reverted steps do not
  count). The step waits, and its `state_reason` says why, when one of these holds:
  - `max_new_indexes_per_table`: an index step would put its table over the cap.
  - `max_new_index_bytes_pct`: the indexes the running plans built already exceed the cap,
    using live `pg_relation_size`. The next index's own size is unknown until it is built,
    so the cap limits what is already built when the next index starts.
  - `peak_window`: a peak window is open. A window `09:00-17:00` holds steps from 09:00 and
    lets them start at 17:00.
  - `caps_unavailable` or `size_unavailable`: the policy or a size could not be read. The
    step waits and never passes the cap.

## Restore drills

The standing policy document's `drill` block sets the database's restore drill
([managed clouds](managed-clouds.md#restore-drills)). A passed drill is what lets D7 lift
production agent writes above L2 ([agent governance](agent-governance.md)).

```json
"drill": {
  "enabled": true,
  "cadence_days": 30,
  "kinds": ["thin_clone", "branch", "copy_on_write"],
  "max_duration_minutes": 240,
  "marker_query": "SELECT max(created_at) FROM app.orders"
}
```

| Field | Default | Range | What it sets |
|---|---|---|---|
| `enabled` | `true` | | `false` stops drills for the database (D7 then stays at L2). |
| `cadence_days` | 30 | 1-365 | Days between drills. With the agent gate on for a production database, a cheap kind drills at least every `require_restore_drill_days` - 2 days. A failed cheap drill is retried after a day. |
| `kinds` | `thin_clone`, `branch`, `copy_on_write` | those and `full_copy` | Clone kinds a drill may use. `full_copy` (RDS, Cloud SQL, AlloyDB, Azure restore) also needs `clone.allow_full_copy: true`; an empty list is refused. |
| `max_duration_minutes` | 240 | 1-1440 | The whole drill, the clone's creation included. The clone's receipt lives an hour longer, so the reaper never destroys a clone in use. |
| `marker_query` | none | one `SELECT` or `WITH`, no `;`, at most 2,000 characters | Run on the clone only, read-only, to find the newest committed timestamp when the sage schema is not in the database. Without it and without the sage schema, RPO is reported unknown. |

A document without the block, or with a field left out, gets the defaults; an explicit `0`
or an out-of-range value is refused when the document is parsed, and the error names the
field. The block is written back only when it differs from the defaults.

The drill reads the same standing policy the executor enforces: the database's own
policy in meta mode, the global policy in standalone and YAML fleet mode, where one
`drill` block covers every database. When the configured clone kind is not allowed (not in
`kinds`, or `full_copy` without `clone.allow_full_copy`), no drill runs and the database
gets the info finding `recoverability_unknown` with the reason.

  A step that waits past its `deadline_minutes`, counted from when it first waited, ends
  `skipped` with verdict `expired`, and its `on_expired` decides. `halt_ask` (the default)
  halts the plan. `keep` and `revert_step` let it go on, since nothing ran.
  `revert_to_checkpoint` reverts the kept steps back to the checkpoint, then halts the plan.
- **Steps that are never automatic.** Approving a plan never covers some steps, at any trust
  level. The engine stops before each one with step state `awaiting_approval` and sends one
  notification. The step is listed with the plans that need a person. These are the steps
  and the reason each one records:
  - `rollback_class`: the contract is not `reversible` or `no_rollback_needed`.
  - `rollback_missing`: the contract is reversible, but the step has no rollback SQL.
  - `no_contract`: the executor has no contract for the step's SQL or action type.
  - `table_rewrite`: VACUUM FULL, CLUSTER, SET TABLESPACE, SET ACCESS METHOD or SET LOGGED.
  - `partition_conversion`, `type_change` and `dropped_data`: these include DROP TABLE,
    DROP COLUMN, TRUNCATE and DELETE.
  - `restart_bound_setting`: `pg_settings.context = 'postmaster'`.
  - `setting_context_unknown`: the setting's context could not be read.
  - `instance_or_topology_change`: promotion, replication slots, subscriptions or
    publications, or instance actions.
  - `privilege_revoke` and `pooler_mode_change`.

  The statements are read with the migration statement classifier. Such a step runs only
  with its own approval (see Plan approval coverage below).

## Backfill pressure

The standing policy document's `pressure` block sets the limits `check_pressure` answers a
backfill or bulk write against (`GET /api/v1/databases/{db}/pressure` and the MCP tool
`check_pressure`, see [API](api.md#backfill-pressure)). The check only reads and never
acts: the job asks between batches and decides. Without the block, each input stops at half
of its matching alert threshold, so the signal stops a job before the alert fires:

```json
"pressure": {
  "limits": {
    "replica_lag_seconds": {"slow": 5, "stop": 10},
    "free_storage_pct": {"stop": 25}
  },
  "required": ["replica_lag_seconds", "xid_age", "multixact_age", "vacuum_running",
               "lock_queue"]
}
```

| Input | Default slow | Default stop | Read from |
|---|---|---|---|
| `replica_lag_seconds` | half the stop | half of `rca.replication_lag_threshold_seconds` (15 s) | `pg_stat_replication`, raised to the WAL position history's lower bound |
| `xid_age` | `autovacuum_freeze_max_age` when under the stop, else half the stop | half of `analyzer.xid_wraparound_warning` (250 million) | `age(datfrozenxid)`, oldest database |
| `multixact_age` | `autovacuum_multixact_freeze_max_age` when under the stop, else half the stop | the same as `xid_age` | `mxid_age(datminmxid)`, oldest database |
| `vacuum_running` | 1 (a vacuum on the target table) | none | `pg_stat_progress_vacuum` |
| `wal_bytes_per_second` | 32 MiB/s | 64 MiB/s | WAL insert position over the last 60 s |
| `lock_queue` | 1 waiter | 3 waiters | `pg_locks` on the target table |
| `free_storage_pct` | three times the cloud free storage floor (30%) | twice the floor (20%) | managed-cloud telemetry; unknown without it |

No alert covers the vacuum, WAL rate and lock queue inputs, so their defaults are fixed.
For `free_storage_pct` lower is worse: `slow` must be above `stop`, both at most 100. For
every other input `slow` must be below `stop`. A bound left out keeps its default, except
that a default `slow` at or past a `stop` you set moves to half that stop (for
`free_storage_pct`, 1.5 times it, at most 100%), so a stricter stop still slows a job first.
A limit with neither bound, an unknown input or a negative value is refused when the
document is parsed. `required` replaces the default list (`[]` requires none): a required input that
cannot be read (a timeout, a missing privilege) stops the answer. Any other unknown input
is reported and never decides, and an answer with no input known at all is `stop`. An input
that does not exist here, such as replica lag without replicas, is `not_applicable`.
The block is written back only when it is set.

## Plan approval coverage

A person approves a plan at the plan hash they were shown, in one of three modes:

| Mode | Covers |
|---|---|
| `all` | Every step except the never-automatic ones above. |
| `up_to` N | Steps 1 to N except the never-automatic ones. N equal to the step count is `all`; N past it is refused (`invalid_up_to`). |
| `step` | No step by itself. |

- **The rule.** A step starts only when the plan's live approval covers it, or the step has
  its own live approval at the current plan hash. Check and operator steps change nothing,
  so any current plan approval covers them. Without a plan approval at the current hash,
  nothing is covered.
- **A step that is not covered** stops with `awaiting_approval` and its reason, and one
  notification asks for it. Its own approval (`POST /api/v1/plans/{id}/steps/{step}/approve`
  or `pg_sage_sidecar plan approve <id> --hash <h> --step N`) sets it back to pending. The
  executor then runs it as the person who approved the step. To decline it instead, cancel
  the plan (`POST /api/v1/plans/{id}/cancel` or `pg_sage_sidecar plan cancel <id>`): a
  running plan with no step in flight can be cancelled.
- **An edit voids every approval** of the plan, step approvals included, and the plan goes
  back to `proposed`. Two people approving with the same hash and mode make one live
  approval; an approval racing an edit is either voided by it or refused as stale.
- **Approval is never automatic.** Every plan needs a person's approval, whatever the trust
  level. A never-automatic step always needs its own approval as well.
- **A model-origin step needs its own approval** too (review-03b): in a plan whose origin is
  `model`, an action step the model proposed is not covered by `all` or `up_to` until a person
  grants that model authority (review-05a's G-M1, `sage.review_model_authority`); its reason
  starts with `model_origin`. See [review.md](review.md#from-review-to-plan).
## Exposure remediation

The exposure detectors (AP-03 tables reachable by exposed roles without RLS, AP-15
relations granted to `PUBLIC`; see [Agent governance](agent-governance.md#exposure-remediation))
can be acted on by two typed contracts. Both run only when the policy document says
`"exposure": {"mode": "enforce"}`; the default is `shadow`, which changes nothing.
Setting `enforce` widens the policy. Setting `shadow` again withdraws the exposure
proposals still pending (queue status `superseded`); an action that already ran keeps its
rollback.

| Contract | Tier | What it runs | Rollback |
|---|---|---|---|
| `revoke_public_grant` | `moderate` | `REVOKE <privileges> ON TABLE s.t FROM PUBLIC`, in one transaction under `lock_timeout` 2 s, for only the privileges PUBLIC holds (read with `aclexplode`, `acldefault` when the ACL is NULL). | The exact re-`GRANT ... TO PUBLIC` of what was revoked, in `sage.action_log.rollback_sql`; the prior ACL is in `sage.exposure_remediations`. |
| `enable_row_level_security` | `high` | `ALTER TABLE s.t ENABLE ROW LEVEL SECURITY`. Always approval, at every trust level. | `ALTER TABLE s.t DISABLE ROW LEVEL SECURITY`. |

Both act only on a user table (not a view or a system catalog) that an open exposure
finding names, and only when pg_sage's action role owns the table or is a member of its
owner. Otherwise the proposal ends once with "pg_sage cannot revoke: grant ownership or
run as the owner" and is not retried; the owner must run the finding's script. Nothing
to revoke (or RLS already on) records no action. A lock not granted within 2 s is retried
next cycle.

`revoke_public_grant` change class is `exposure`. With the class granted at `moderate`
(`trust.level: autonomous`, `trust.tier3_moderate`) it runs unattended; under `advisory`
it is proposed for approval. Spec section 3 keeps privilege revokes approval-only; this
one is the exception because it is reversible by its stored re-GRANT, and only for a
table an exposure finding names.

After a revoke, pg_sage verifies the readers: every `pg_stat_statements` statement that
references the table and ran in the hour before must keep running in the window after
(15 minutes). pg_stat_statements counts only statements that succeed, so a reader that
relied on PUBLIC and now gets "permission denied" stops; the revoke is then reverted
through the rollback path (`rollback_mitigation`). Without statement history, or when
`pg_stat_statements` cannot be read, the revoke is kept and marked `unverifiable`. The
revoke holds the table's change lease, so two cycles on one table record one action, and
a statement that fails `maxActionRetries` (3) times is not retried: the count is per
table, so failures on one relation of an AP-15 finding do not hold back the others.

The approval card of `enable_row_level_security` (in the queuing decision's evidence,
`exposure_card`) lists the top 20 statements by calls that reference the table
(fingerprints only), the roles that would lose access (grantees that neither own the
table nor bypass RLS, PUBLIC included) and an optional `CREATE POLICY` template. The
template is card text; pg_sage never runs it.

## The decision ledger

Each decision is one row in `sage.decision`:

| Column | Holds |
|---|---|
| `verdict`, `reason` | The verdict (ledger spelling) and reason. |
| `risk_tier` | The contract's tier; `high` for a request with no contract or an unknown tier. |
| `feature`, `intent`, `target_objects` | What the action is and which objects it touches. A statement that maps to no change class records under `unclassified`. |
| `evidence`, `evidence_id` | The decision's evidence (detail, budget kind, lease outcome, verification wait) and its stable ID. |
| `policy_version`, `policy_doc_sha256` | The standing policy the gate decided under. |
| `guardrails`, `off_window_ok` | The contract's guardrails, and whether a deadline let it run off-window. |
| `deadline_kind`, `deadline_hard_at` | The XID or disk deadline that drove the request, if any. |
| `principal_id`, `task_id`, `artifact_hash` | The agent and task an agent request came from. |
| `fingerprint`, `repeat_count`, `last_seen_at` | Repeat folding (below). |
| `candidate_key`, `resolved_at` | The episode a withheld decision belongs to (below). |
| `action_log_id`, `queue_id` | The action that ran, or the approval queue item. |

**Fingerprint folding.** The gate evaluates every candidate every cycle. A withheld verdict
(`queue_approval`, `park`, `blocked`, `observe_only`) carries a fingerprint: a hash of the
database, feature, intent, targets (in any order), verdict, policy version, proposed SQL and,
for an agent, the principal. A repeat of an open fingerprint updates the one row: it adds to
`repeat_count`, stamps `last_seen_at` and records the latest reason, risk tier, evidence,
guardrails and deadline. The reason is not part of the fingerprint, so a candidate that
moves from `rate_limit_exceeded` to `blast_radius_exceeded` keeps its row and shows the
latest reason. An `execute` has no fingerprint: each one backs its own action and row.

**Episodes.** `candidate_key` identifies the candidate whatever the verdict (database,
feature, targets, SQL and principal). When the candidate executes, its open withheld rows
are resolved (`resolved_at`) in the same statement, and the next withheld verdict starts a
new row.

**Metrics.** `pg_sage_policy_parks_total{database,reason}` counts each park the gate
records, one per evaluation, so repeats folded into one row each count and a rate shows how
long candidates wait. A park that fails to record is answered `blocked`
`policy_unavailable` and counted in `pg_sage_policy_record_failures_total` instead; any
series there above zero means the ledger is missing decisions.

## Reading decisions

"Why hasn't this run?" is answered from the ledger, not by SQL. Both routes need the
operator or admin role, read one database at a time (all of them by default), return no SQL
text and record nothing.

**Actions > Held back** (formerly Decisions) and `GET /api/v1/decisions` list the open
withheld decisions, with each verdict and reason in words and a link to the exact approval
request (`#/actions?tab=pending&queue=<id>`) or finding (`#/cases?finding=<id>`): rows
with a `queue_approval`, `parked`, `blocked` or `observe_only` verdict that no execute has
resolved and no approval queue item has taken over. A withheld row recorded outside the gate's
repeats (a lease conflict's `ddl_conflict` park, a `custodian_backoff` escalation, a schema
guard verdict) leaves the list once its candidate's next execute is recorded. Each entry is
one candidate (its
`candidate_key`; a row written before candidate keys by its fingerprint), with the latest
row's verdict and reason and every open row's repeats:

| Field | Holds |
|---|---|
| `database` | The monitored database the entry was read from. |
| `verdict`, `reason`, `human_reason` | The latest verdict and reason, and the reason as the approval card words it, with the budget or verification-wait detail. |
| `repeat_count`, `open_rows`, `first_seen`, `last_seen_at` | How many evaluations withheld it, in how many open rows (one per verdict and policy version), since when and most recently. |
| `budget_detail` | For a budget park, which budget is full and when it next frees. |
| `verification_wait`, `verification_wait_detail` | The changes in flight it waits on, and until when. |
| `deadline_kind`, `deadline_hard_at` | The XID or disk deadline behind it, if any. |
| `policy_version`, `policy_doc_sha256` | The standing policy it was decided under. |
| `queue_id`, `finding_id` | The approval queue item or finding it belongs to, when recorded. |

Parameters, each optional: `database` (a configured database name), `verdict` (one of the
four withheld verdicts), `reason` (a reason on this page, an agent reason, or
`custodian_backoff`; the schema guard records free text, which is not a filter), `since`
(RFC 3339; entries last
seen at or after it) and `limit` (1-500, default 100). A bad value is `400` naming the
parameter. A database that cannot be read is listed in `partial` with its error class
(`connection`, `timeout`, `query`) and the others are still returned; when every selected
database fails the status is `503`. `truncated` says the list was cut at `limit`.

**Why hasn't this run?** on a finding's case, and
`GET /api/v1/policy/explain?database=&finding_id=`, ask the gate how it would decide the
finding's candidates now, through the same path the cases view and the executor use. Each
candidate has its `decision`, `blocked_reason`, the gate's `detail` (only when the reason
is the gate's, not a candidate's own refusal) and `human_reason`. `status` is `explained`,
or `no_candidate` when the finding has nothing the executor could run. Explaining records
no decision and takes no budget slot.

## Approval flow

1. A `queue_approval` verdict creates one item in the database's approval queue. Its
   approval card shows the SQL, rollback, evidence, risk, blast radius and why a person must
   decide (including `gate:<reason>`), and expires.
2. A person approves or rejects it on the Actions page, in the Cases panel (Sage SRE) or
   with a ChatOps button. The approval is single use, records who approved, and is bound to
   the card's content: an item whose SQL changed since the card was shown is not approved.
3. The executor asks the gate again with the approval (step 9 of the evaluation order).
   Trust, the change-class allowlist and the maintenance windows still bind, so an approved
   action can still be `blocked` `outside_maintenance_window` (or `window_closing`) until
   the window opens. Approved incident mitigations (a Sage SRE cancel, a revert) are not
   held by windows.
   When the window is the only refusal (the gate re-runs the operator path with the window
   treated as open), the approval is scheduled instead of refused: the item becomes
   `scheduled` with the next opening (`run_after`) and that window's close. Each executor
   cycle asks the gate again about each due item under the policy then in force, then
   claims it (one sidecar wins) and runs it. A stop, a narrowed policy, a revised
   recommendation, a resolved finding or a missed window returns the item to `pending`
   with the reason, before its recommendation is approved, and sends a new approval
   request. A schedule covers one window only; a failed run is recorded as
   `failed` and never rescheduled.
4. The executor takes the change lease and runs the action. An operator action that cannot
   get the lease is refused (`blocked` `ddl_conflict`) rather than parked: a person is
   waiting on it.

An approval that overrides a pending verification says so on the card first ("approving
overrides pending verification of action N") and records `evidence.verification_override`.

**Chat.** Slack and Telegram approval cards carry the same content as the UI card, with
one-click buttons that go through the same approval path. A Sage SRE cancel is decided
through its approval card only, and a signed Slack callback older than
`sre.actions.chatops_tolerance_seconds` is refused; see
[Sage SRE approved actions](configuration.md#sage-sre-approved-actions).

**Plan cards in chat.** A plan stored as proposed (an agent proposes one with MCP
`propose_plan`) sends a plan card to the channels that already receive approval cards, with
no new configuration. The card gives the goal, the step count, each step's expected effect
and risk tier, and how to review the plan in the app. From chat a person may approve steps
of risk tier `safe` and `moderate` (decision D7, `changeplan.ChatApprovableTiers`): a plan
of such steps offers "Approve all" and "Approve step 1 only"; a plan whose first `high` or
never-automatic step is step K offers "Approve up to step K-1" and says the rest needs the
app; at step 1 the card offers only "Reject", which every card offers. The approval is
plan-03a's coverage rule with `via` `chat`. A press is checked like an approval card: a
signed callback from a chat user mapped to an operator or admin, a single-use token issued
to this channel, unused and unexpired, offering that decision, on a plan still `proposed` at
the hash the card showed ("Refused: this plan changed" otherwise). What chat may approve is
checked again against the plan when the button is pressed. Two clicks make one decision; if
pg_sage cannot record the decision the card stays usable. No MCP tool approves a plan.

**Agent policy proposals.** An agent can only propose a change to its own policy. A
proposal that widens the current policy needs two different people to approve it, and the
agent's sponsor is not one of them, unless `agents.single_operator_mode` lets one person
approve with a recorded reason, queued for review afterwards.
