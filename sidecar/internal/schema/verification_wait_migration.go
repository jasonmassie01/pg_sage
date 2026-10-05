package schema

import "strings"

// One self-initiated change per object (2026-10-04): before it runs a
// change, the policy gate reads the changes still being verified on the
// change's objects. The executed-action half reads idx_action_log_outcome;
// this partial index serves the pending verdicts of sage.action_outcome,
// a small set inside a table that grows with every action, so the lookup
// never scans the outcome ledger. A verdict is decided once, leaving the
// index. Same checked loop as the decision ledger migration. Idempotent.
var verificationWaitIndexes = []ledgerIndex{
	{"idx_action_outcome_pending", "action_outcome", "INDEX %I ON " +
		"sage.action_outcome (action_log_id) WHERE verdict = 'pending'"},
}

// ddlVerificationWait is the migration: the checked index loop.
func ddlVerificationWait() string {
	values := make([]string, 0, len(verificationWaitIndexes))
	for _, index := range verificationWaitIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return "DO $$\nDECLARE spec record;\nBEGIN" + loop + "\nEND $$;"
}
