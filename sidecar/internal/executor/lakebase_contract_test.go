package executor

import (
	"slices"
	"testing"
)

func TestLakebaseActionSupportIsLimitedToNativeVerifiedFamilies(t *testing.T) {
	for _, action := range []string{"analyze_table", "vacuum_table", "create_index_concurrently", "revert_created_index", "reindex_concurrently", "create_statistics", "revert_created_statistics", "alter_database_guc"} {
		c, ok := ContractForActionType(action)
		if !ok || !slices.Contains(c.ProviderSupport, "lakebase") {
			t.Errorf("native verified family %s lacks Lakebase support", action)
		}
	}
	for _, action := range []string{"alter_system_guc", "alter_table", "drop_unused_index", ActionTypeReplaceIndex, ActionTypeGuardRoleEnsure, ActionTypeGuardGrant, "apply_query_hint", "cancel_backend", "terminate_backend", "retention_delete", "set_table_autovacuum"} {
		c, ok := ContractForActionType(action)
		if !ok || slices.Contains(c.ProviderSupport, "lakebase") {
			t.Errorf("unverified family %s must remain provider-refused", action)
		}
	}
	if slices.Contains(portableActionProviders(), "lakebase") {
		t.Fatal("general portable provider list must not grant unvalidated Lakebase families")
	}
}
