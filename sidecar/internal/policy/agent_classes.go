package policy

// The six agent change classes (spec §6.3, the contracts' Feature
// column). The default document lists them as allowed and approval
// required (§6.2.2 A6): an agent change, or a change pg_sage makes on an
// agent's behalf, runs only after a human approved it until the agent
// earns more. A stored policy that does not list a class blocks it with
// change_class_not_allowed.
const (
	ChangeAgentAccess       ChangeClass = "agent_access"
	ChangeAgentDataWrite    ChangeClass = "agent_data_write"
	ChangeAgentSchemaChange ChangeClass = "agent_schema_change"
	ChangeAgentMaintenance  ChangeClass = "agent_maintenance"
	ChangeAgentSandbox      ChangeClass = "agent_sandbox"
	ChangeAgentEstate       ChangeClass = "agent_estate"
)

// AgentChangeClasses lists the agent change classes in spec order.
func AgentChangeClasses() []ChangeClass {
	return []ChangeClass{ChangeAgentAccess, ChangeAgentDataWrite, ChangeAgentSchemaChange,
		ChangeAgentMaintenance, ChangeAgentSandbox, ChangeAgentEstate}
}

// IsAgentChangeClass reports whether class is one of the agent classes.
func IsAgentChangeClass(class ChangeClass) bool {
	return containsChangeClass(AgentChangeClasses(), class)
}

// typedInternalActions are the typed actions pg_sage builds itself without
// SQL text a caller could shape: their contract and arguments are the
// request, so SQL validation does not apply (the statements are generated
// from validated identifiers inside the action). The agent role contracts
// are among them (§6.3, §6.6), and so are freeze, unfreeze and the kill
// switch (§6.10), and the agent grant contracts guard_grant and
// guard_revoke (§6.6).
var typedInternalActions = map[string]bool{
	"declare_table_contract": true, "register_consumer": true, "retention_delete": true,
	"guard_role_ensure": true, "guard_role_retire": true,
	"guard_freeze": true, "guard_unfreeze": true, "guard_kill": true,
	"guard_grant": true, "guard_revoke": true,
}
