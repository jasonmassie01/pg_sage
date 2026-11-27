package pgconf

// databaseScopeApprovalOnly are the settings pg_sage changes only for one
// database (ALTER DATABASE ... SET), and only with an operator's approval
// (advisors-07). At instance scope one value would end idle transactions
// in every database of the server, and a timeout ends sessions, which no
// metric pg_sage watches can prove harmless.
var databaseScopeApprovalOnly = map[string]bool{
	"idle_in_transaction_session_timeout": true,
}

// DatabaseScopeApprovalOnly reports a setting that is executable only as
// ALTER DATABASE, never as ALTER SYSTEM, and never without approval.
func DatabaseScopeApprovalOnly(name string) bool {
	return databaseScopeApprovalOnly[norm(name)]
}
