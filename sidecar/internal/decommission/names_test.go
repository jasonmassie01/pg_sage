package decommission

import "testing"

// The expected names were produced by the removed provisioner's own
// ProviderResourceName at 62d27f6e, so an uncertain create (no recorded
// resource id) is looked up under exactly the name the provider received.
func TestDeterministicName_MatchesTheRemovedProvisioner(t *testing.T) {
	long := "x-very-long-deployment-identifier-that-goes-on-and-on-and-on-forever-and-ever"
	tests := []struct{ provider, id, want string }{
		{"aws_rds", "dep-1", "pgsage-dep-1"},
		{"aws_rds", "Tenant_A/Run 42", "pgsage-tenant-a-run-42"},
		{"gcp_cloudsql", "0abc", "pgsage-0abc"},
		{"gcp_cloudsql", long, "pgsage-x-very-long-deployment-identifier-that-goes-on-and-on-an"},
		{"databricks_lakebase", "dep-1", "pgsage-dep-1"},
		{"neon", "dep-1", "pgsage-dep-1-ee592263"},
		{"neon", "Tenant_A/Run 42", "pgsage-tenant-a-run-42-5caab552"},
		{"supabase", "0abc", "pgsage-0abc-d75f07fa"},
		{"supabase", long, "pgsage-x-very-long-deployment-identifier-that-goes-on-1743d276"},
		{" NEON ", "dep-1", "pgsage-dep-1-ee592263"},
	}
	for _, tt := range tests {
		if got := deterministicName(tt.provider, tt.id); got != tt.want {
			t.Errorf("deterministicName(%q, %q) = %q, want %q", tt.provider, tt.id, got, tt.want)
		}
	}
}

func TestDeterministicName_NoNameWithoutAProviderOrID(t *testing.T) {
	for _, tt := range []struct{ provider, id string }{
		{"local_postgres", "dep-1"}, {"", "dep-1"}, {"heroku", "dep-1"},
		{"neon", ""}, {"supabase", "   "},
	} {
		if got := deterministicName(tt.provider, tt.id); got != "" {
			t.Errorf("deterministicName(%q, %q) = %q, want empty", tt.provider, tt.id, got)
		}
	}
}
