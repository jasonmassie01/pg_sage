package schema

// Decision episodes (policy-02). A withheld verdict folds its repeats into
// one open row (decision_ledger_migration.go); candidate_key names the
// candidate across verdicts (database, feature, targets, SQL, principal),
// so an execute resolves the candidate's open withheld rows and the next
// park starts a new row. idx_decision_candidate_open finds those rows.
//
// The column is nullable and additive: rows written before the upgrade
// gain their key at their next repeat. The index loop is the ledger's: a
// catalog check first, so a re-run takes no lock, and an INVALID index is
// rebuilt unless a build is still running. lock_timeout bounds the first
// run's ALTER TABLE and index build. Idempotent.

var candidateIndexes = []ledgerIndex{
	{"idx_decision_candidate_open", "decision", "INDEX %I ON sage.decision " +
		"(candidate_key) WHERE resolved_at IS NULL AND candidate_key IS NOT NULL"},
}

const ddlDecisionCandidateColumn = `
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.decision'::regclass AND NOT attisdropped
           AND attname = 'candidate_key') THEN
        ALTER TABLE sage.decision ADD COLUMN candidate_key text;
    END IF;`

// ddlDecisionCandidateKey is the candidate-key migration step.
func ddlDecisionCandidateKey() string {
	return "SET LOCAL lock_timeout = '5s';\n" +
		ledgerIndexBlock(ddlDecisionCandidateColumn, candidateIndexes)
}
