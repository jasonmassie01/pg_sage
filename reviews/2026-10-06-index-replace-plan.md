# Plan: executor "replace" action (index_replace), v2.3.0

Branch `claude/v23-index-replace`. Goal: when a tuning candidate subsumes an existing index,
propose one action that creates the wider index and soft-drops the subsumed one, instead of
refusing it (v2.1.0 behaviour, `tuning/inflight.go overlapReason`).

## Product decisions

- **Representation.** One statement pair, carried everywhere as text:
  `CREATE INDEX CONCURRENTLY <new> ...;\nDROP INDEX CONCURRENTLY <old>;` with the undo
  `CREATE INDEX CONCURRENTLY <old definition>;\nDROP INDEX CONCURRENTLY IF EXISTS <new>;`.
  The finding, the approval queue row, the card and its content hash (approve-expecting)
  therefore bind to both statements. A typed parser (`ParseIndexReplaceSQL`) splits it and
  validates each statement with `ValidateExecutorSQL`; nothing ever runs it as one string.
- **Action type** `replace_index` (contract), action_log label `replace_index`, earned class
  and verification class `index_replace`.
- **Never unattended (L2).** Class cap L2 and the contract carries the `approval_required`
  guardrail, so the gate queues it even without a ledger. Components are L3 each, but a
  two-step non-atomic change has partial states with no live record; L2 is the one-click
  handoff. Consistent with the class model: the cap binds the self-initiated pair too
  (`CapForPair` honours a class cap set below its reversibility cap, only index_replace
  uses this). Its level is the lower of the index_create (tuning) and index_drop (hygiene)
  levels when it has no ledger row of its own, capped at L2: a database that hands creates
  and drops to a human gets replace cards; one that only observes keeps observing.
- **Refusals (preflight, catalog, at approval time):** the old index backs a constraint
  (pg_constraint.conindid, any contype), is unique/primary/exclusion, or leads an FK's
  columns that the new index would not lead (or the new index is partial); the old index's
  OID or definition changed since the proposal; the new index does not subsume the old one;
  binding facts (`owned_by_app_migrations`: the facts filter attaches the two-statement
  migration and its down instead; the gate refuses the DDL).
- **Two steps, durable state machine** (`sage.index_replace`): creating → created →
  dropping → completed; terminal create_failed (nothing dropped, INVALID remnant removed),
  drop_failed (partial: the new index stays, the old index is kept and reported),
  rolled_back, rollback_failed, old_restored. Undo: rollback_recreating →
  rollback_dropping → rolled_back.
- **Crash between steps:** each cycle `ResumeIndexReplaces` reads open rows and the catalog:
  creating + new valid → created; creating + new invalid/absent → remnant dropped,
  create_failed; created/dropping → the drop is resumed under a fresh lease and a fresh
  operator-approved authorization (emergency stop and policy still bind; refused → retried
  next cycle); dropping + old gone → completed. Rollback states resume idempotently.
- **Identity binding for the drop:** immediately before `DROP INDEX CONCURRENTLY` the old
  index's OID and `pg_get_indexdef` must equal the recorded ones (the residual race against
  external DDL is the same as any name-based drop; the kept definition re-creates it).
- **Lease:** the operator-typed lease on the table and the old index is held by Apply for the
  whole two-step Execute; resume takes it again.
- **Verification** (own monitor, durable in `sage.index_replace.verify_phase`):
  targeted queries (the finding's queryids) must improve; guarded queries (the table's
  most-called statements, the old index's users) must not regress. Phase A (first window,
  extended to the cap): regression, no gain or unverifiable → full rollback (re-create old,
  drop new), consistent with index_create. Kept → verdict improved, credited. Phase B (until
  `verify.drop_window_hours`): guarded regression or an active hint naming the old index →
  soft-drop re-create of the old index only (outcome old_restored).
- **Approval card:** SQL shows both statements, rollback shows both, lock text names the
  SHARE UPDATE EXCLUSIVE build and the brief ACCESS EXCLUSIVE on the old index; targets
  include the old index.
- No new config keys (windows reuse trust/verify keys).

## Steps

- [x] Plan (this file).
- [x] Phase 1 tests (commit before implementation): executor unit (parse, state machine,
      contract, classification), executor DB (success, create failure, drop failure, crash
      between steps resume, rollback, constraint-backed refusal, FK refusal, identity
      change, facts refusal through the gate, monitor phase A/B), earned (class, caps,
      composite level), tuning (replace emitted instead of refusal; in-flight still
      refused), approvalcard (both statements, undo, lock), facts (migration with both
      statements), schema migration idempotent.
- [x] Implement: schema migration; executor (replace files, routing in ExecuteManual,
      RollbackAction, RunCycle resume, classification switches); earned; policy budget/
      drop kind; facts; tuning; approvalcard; docs + CHANGELOG Unreleased.
- [x] Run: build, vet, golangci-lint, `-count=1 -cover` on PG17, touched packages on PG14
      and `-race`, e2e suite; post-test audit and mutation checks.
- [x] Commit, push, PR (not merged). Report in CLAUDE.md format.
