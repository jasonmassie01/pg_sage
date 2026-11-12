package ddlrules

// swapFormRules covers the DDL an online column swap runs (migration-09b):
// the explicit lock of the swap transaction and the cleanup of the sync
// trigger. Like formRules, only the parse-tree classifier recognizes them.
func swapFormRules() []Rule {
	return []Rule{
		{
			ID: "ddl_lock_table", LockLevel: "ACCESS EXCLUSIVE",
			Description: "LOCK TABLE holds the named lock until the transaction ends " +
				"and queues every conflicting query behind it while it waits",
			SafeAltTemplate: "SET lock_timeout first and keep the transaction short",
		},
		{
			ID: "ddl_drop_trigger", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "DROP TRIGGER takes ACCESS EXCLUSIVE on the trigger's table",
			SafeAltTemplate: "SET lock_timeout and drop the trigger in a short transaction",
		},
		{
			ID: "ddl_drop_function", LockLevel: "ACCESS EXCLUSIVE",
			Description: "DROP FUNCTION takes ACCESS EXCLUSIVE on the function (not a " +
				"table) and fails while a trigger, default or view still uses it",
			SafeAltTemplate: "Verify nothing still calls the function before dropping it",
		},
	}
}
