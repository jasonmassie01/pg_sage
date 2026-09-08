package agentdb

import (
	"context"
	"testing"
)

type evidenceProviderRunner struct {
	fakeProviderRunner
	result ProvisionResult
}

func (r evidenceProviderRunner) BackupCheck(context.Context, ProvisionRequest) ProvisionResult {
	return r.result
}

func TestHostedBackupAssurancePreservesEvidence(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for _, status := range []string{"unverified", "verified", "", "restore_verified"} {
		t.Run(status, func(t *testing.T) {
			id := "hosted_backup_evidence_" + status
			_, err := st.Provision(ctx, RegisterRequest{DeploymentID: id,
				TenantID: "tenant_agentdb_test", AgentID: "agent_backup", Provider: ProviderNeon,
				ProvisioningLevel: LevelInstance, LeaseSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			runner := evidenceProviderRunner{fakeProviderRunner{ProviderNeon, "neon_test"},
				ProvisionResult{Status: status, Detail: map[string]any{
					"backup_kind": "history", "retention_seconds": 0, "restore_verified": false}}}
			got, err := st.CheckBackupAssuranceLive(ctx, id, runner)
			if err != nil {
				t.Fatal(err)
			}
			want := "unverified"
			if status == "verified" {
				want = "verified"
			}
			if got.BackupStatus != want || got.SafeForDestroy || got.Backup.RestoreVerifiedAt != nil {
				t.Fatalf("status=%q safe=%v restore=%v", got.BackupStatus,
					got.SafeForDestroy, got.Backup.RestoreVerifiedAt)
			}
			if got.Backup.Detail["backup_kind"] != "history" {
				t.Fatal("persisted backup lost provider evidence")
			}
		})
	}
}
