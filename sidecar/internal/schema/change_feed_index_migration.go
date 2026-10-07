package schema

import "strings"

// Change feed indexes (v2.3.1 perf gate). The SRE change feed polls the
// migration detector's findings past a cursor (category
// 'migration_safety', id > $1, ORDER BY id). Every other findings index
// leading with category is partial on open findings, so the poll scanned
// all of sage.findings. The partial index holds only that category; its
// columns never change after insert, so findings refreshes stay HOT.
var changeFeedIndexes = []ledgerIndex{
	{"idx_findings_migration_feed", "findings",
		"INDEX %I ON sage.findings (id) WHERE category = 'migration_safety'"},
}

// ddlChangeFeedIndexes is the migration: the checked index loop over
// changeFeedIndexes.
func ddlChangeFeedIndexes() string {
	values := make([]string, 0, len(changeFeedIndexes))
	for _, index := range changeFeedIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return "DO $$\nDECLARE spec record;\nBEGIN" + loop + "\nEND $$;"
}
