package retention

// auditChainExemptions: the tamper-evidence chain (E2, spec §6.17). A link
// is one small row per audit write; deleting old links would break the
// verification of every later one, so the chain is kept whole.
var auditChainExemptions = map[string]string{
	"audit_chain_link": "tamper-evidence chain of the audit tables; kept whole " +
		"so every link stays verifiable",
	"audit_chain_meta": "one row per chained table: its install boundary",
	"audit_chain_checkpoint": "bounded by auditchain.StoreCheckpoint: the newest 100 " +
		"verified heads per chain plus the newest full one",
	"guard_identity_bindings": "external identity to principal bindings, current " +
		"state (deleted with the principal)",
	"siem_cursor": "one row per SIEM sink, source and chain: its export position",
	"evidence_signing_key": "one row: the sealed key that signs evidence packs; " +
		"deleting it would stop packs from verifying under the published key",
}
