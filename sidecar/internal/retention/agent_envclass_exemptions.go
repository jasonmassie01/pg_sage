package retention

// agentEnvClassExemptions: agent governance state that is current state,
// not history (spec §6.5, §6.12).
var agentEnvClassExemptions = map[string]string{
	"guard_environment_labels": "environment label and identity, one row per database",
	"clone_instances": "clone receipts: the record of what pg_sage created and must " +
		"destroy; a receipt outlives its clone as the audit of that destroy",
}
