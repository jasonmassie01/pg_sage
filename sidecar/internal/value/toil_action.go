package value

// legacyToilActionTypes names the toil model of action types logged under
// their short names before the executor used its contract names.
var legacyToilActionTypes = map[string]string{
	"create_index": "create_index_concurrently",
	"drop_index":   "drop_unused_index",
	"reindex":      "reindex_concurrently",
	"vacuum":       "vacuum_table",
	"analyze":      "analyze_table",
}

// classToilActionTypes names the toil model of each trust-ledger action
// class that has one.
var classToilActionTypes = map[string]string{
	"index_create":      "create_index_concurrently",
	"index_drop":        "drop_unused_index",
	"reindex":           "reindex_concurrently",
	"vacuum":            "vacuum_table",
	"analyze":           "analyze_table",
	"freeze":            "freeze_table",
	"autovacuum_tuning": "set_table_autovacuum",
	"statistics":        "create_statistics",
	"query_hint":        "apply_query_hint",
	"config_guc":        "alter_system_guc",
}

// ToilActionType is the sage.toil_model action type that prices an
// action_log action type: a legacy short name maps to its contract name,
// any other type is its own model.
func ToilActionType(actionType string) string {
	if mapped, ok := legacyToilActionTypes[actionType]; ok {
		return mapped
	}
	return actionType
}

// ToilActionTypeForClass is the sage.toil_model action type that prices a
// trust-ledger action class, or "" when the class has no toil model. There
// is no default: an unmodelled class earns no minutes.
func ToilActionTypeForClass(class string) string {
	return classToilActionTypes[class]
}
