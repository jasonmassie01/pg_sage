package retention

// auditChainExemptions: the tamper-evidence chain (E2, spec §6.17). A link
// is one small row per audit write; deleting old links would break the
// verification of every later one, so the chain is kept whole.
var auditChainExemptions = map[string]string{
	"audit_chain_link": "tamper-evidence chain of the audit tables; kept whole " +
		"so every link stays verifiable",
	"audit_chain_meta": "one row per chained table: its install boundary",
	"siem_cursor":      "one row per SIEM sink, source and chain: its export position",
}
