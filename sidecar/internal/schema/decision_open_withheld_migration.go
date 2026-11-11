package schema

// Open withheld decisions (policy-07). GET /api/v1/decisions reads the
// open rows whose action has not run (resolved_at IS NULL, verdict not
// execute); idx_decision_open_withheld keeps that read off the rest of
// the ledger, which is mostly resolved and executed rows.
//
// The index is built by the ledger index loop, under the bootstrap lock
// and not CONCURRENTLY, for the reason decision_ledger_migration.go gives
// (a concurrent build beside the other sage.decision migrations deadlocked
// a fleet reload). The loop checks the catalog first, so a re-run takes no
// lock, and rebuilds an INVALID index unless a build is still running.
// lock_timeout bounds the first build. Idempotent.

var openWithheldIndexes = []ledgerIndex{
	{"idx_decision_open_withheld", "decision", "INDEX %I ON sage.decision " +
		"(database_id, last_seen_at DESC) WHERE resolved_at IS NULL AND verdict <> 'execute'"},
}

// ddlDecisionOpenWithheldIndex is the open-withheld index step.
func ddlDecisionOpenWithheldIndex() string {
	return "SET LOCAL lock_timeout = '5s';\n" + ledgerIndexBlock("", openWithheldIndexes)
}
