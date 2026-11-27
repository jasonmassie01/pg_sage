package main

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// Backup isolation (drill-01b): backup_shares_delete_path when no backup
// the provider shows outlives a delete of the instance,
// deletion_protection_off on a production database, and
// pitr_window_below_policy when the point-in-time window is shorter than
// backup.min_pitr_hours. Unknown evidence is not evaluated: the category
// is absent and an open finding stays open.

const (
	categoryBackupDeletePath   = "backup_shares_delete_path"
	categoryDeletionProtection = "deletion_protection_off"
	categoryPITRWindow         = "pitr_window_below_policy"
)

// productionState is what pg_sage knows of a database's environment.
type productionState int

const (
	prodUnknown productionState = iota // no agent gate: no environment evidence
	prodNo
	prodYes
)

// productionOf reads the database's environment through the agent gate.
// With the gate on but no environment bindings, or an unreadable
// environment, the database counts as production (the safe side).
func productionOf(ctx context.Context, database string) productionState {
	state := agentGate.Load()
	if state == nil {
		return prodUnknown
	}
	if state.envs == nil {
		return prodYes
	}
	b, err := envSource{svc: state.envs}.Environment(ctx, database)
	if err != nil || b.Env == envbind.EnvProd {
		return prodYes
	}
	return prodNo
}

// isolationFindings returns, per category evaluated, its findings (an
// empty list resolves the open one).
func isolationFindings(b *cloudtel.BackupPosture, resource string, policy config.BackupConfig,
	prod productionState) map[string][]analyzer.Finding {
	out := map[string][]analyzer.Finding{}
	if b == nil {
		return out
	}
	if b.SurvivesDelete != nil {
		out[categoryBackupDeletePath] = deletePathFinding(b, resource, policy, prod)
	}
	if b.DeletionProtection != nil && prod != prodUnknown {
		out[categoryDeletionProtection] = []analyzer.Finding{}
		if !*b.DeletionProtection && prod == prodYes {
			out[categoryDeletionProtection] = []analyzer.Finding{
				deletionProtectionFinding(resource)}
		}
	}
	if b.PITRWindow != nil {
		out[categoryPITRWindow] = pitrWindowFinding(*b.PITRWindow, resource, policy)
	}
	return out
}

// deletePathFinding: critical on production with the agent gate on, since
// pg_sage cannot see which agent holds a cloud credential able to delete
// the instance. A declared backup.external_source resolves it.
func deletePathFinding(b *cloudtel.BackupPosture, resource string, policy config.BackupConfig,
	prod productionState) []analyzer.Finding {
	if *b.SurvivesDelete || policy.ExternalSource != "" {
		return []analyzer.Finding{}
	}
	severity := "warning"
	if prod == prodYes {
		severity = "critical"
	}
	return []analyzer.Finding{{Category: categoryBackupDeletePath, Severity: severity,
		ObjectType: "backup", ObjectIdentifier: "backup:" + resource,
		Title: "Every backup of " + resource + " is deleted with the instance",
		Detail: map[string]any{"resource": resource, "survives_delete": false,
			"isolation_evidence": b.IsolationEvidence, "production": prod == prodYes},
		ActionRisk: "safe",
		Recommendation: "One delete call (an agent's, a script's or a person's) would " +
			"remove the database and its backups together. Keep a copy outside that " +
			"path: a recent manual snapshot (RDS, Aurora), retained backups on delete " +
			"(Cloud SQL), an AWS Backup vault or cross-region backup replication, and " +
			"turn on deletion protection. If such a copy exists where pg_sage cannot " +
			"see it, name it in backup.external_source."}}
}

func deletionProtectionFinding(resource string) analyzer.Finding {
	return analyzer.Finding{Category: categoryDeletionProtection, Severity: "warning",
		ObjectType: "backup", ObjectIdentifier: "backup:" + resource,
		Title:      "Deletion protection is off on production " + resource,
		Detail:     map[string]any{"resource": resource, "deletion_protection": false},
		ActionRisk: "safe",
		Recommendation: "Turn on deletion protection for the instance (on Aurora, the " +
			"cluster) in the provider console or its infrastructure code, so a delete " +
			"needs a deliberate change first."}
}

// pitrWindowFinding: a 0 policy turns the check off (and resolves).
func pitrWindowFinding(window time.Duration, resource string,
	policy config.BackupConfig) []analyzer.Finding {
	limit := policy.MinPITRWindow()
	if limit <= 0 || window >= limit {
		return []analyzer.Finding{}
	}
	minutes := int64(window / time.Minute)
	return []analyzer.Finding{{Category: categoryPITRWindow, Severity: "warning",
		ObjectType: "backup", ObjectIdentifier: "backup:" + resource,
		Title: fmt.Sprintf("Point-in-time recovery of %s reaches back %.1f hours, below "+
			"the %d-hour policy", resource, window.Hours(), policy.MinPITRHours),
		Detail: map[string]any{"resource": resource, "pitr_window_minutes": minutes,
			"min_pitr_hours": policy.MinPITRHours},
		ActionRisk: "safe",
		Recommendation: "Raise the backup retention (RDS and Aurora backup retention " +
			"period, Cloud SQL transaction log retention) so a bad change found late " +
			"can still be undone, or lower backup.min_pitr_hours if the shorter " +
			"window is intended."}}
}
