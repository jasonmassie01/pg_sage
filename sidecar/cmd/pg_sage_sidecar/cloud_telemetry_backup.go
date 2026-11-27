package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// Backup results from the provider (market-07): backup_stale when the
// last good backup is older than backup.max_age_hours, and
// backup_retention_below_policy when the retention is below
// backup.min_retention_days. AP-11 keeps reporting the configuration
// (backups or PITR off); these report what the provider says happened.

const (
	categoryBackupStale     = "backup_stale"
	categoryBackupRetention = "backup_retention_below_policy"
)

// backupFindings returns, per category evaluated this cycle, its findings
// (an empty list resolves the open one). A category whose evidence is
// unknown is absent, so its open finding stays: unknown is never "ok".
func backupFindings(b *cloudtel.BackupPosture, resource string, policy config.BackupConfig,
	now time.Time) map[string][]analyzer.Finding {
	out := map[string][]analyzer.Finding{}
	if b == nil {
		return out
	}
	if stale, known := staleBackup(b, resource, policy, now); known {
		out[categoryBackupStale] = stale
	}
	if b.RetentionDays != nil {
		out[categoryBackupRetention] = retentionFinding(*b.RetentionDays, resource, policy)
	}
	return out
}

// staleBackup judges the provider's last good backup; false when there
// is no usable evidence (nothing read, the read withheld, a time in the
// future beyond the clock-skew allowance).
func staleBackup(b *cloudtel.BackupPosture, resource string, policy config.BackupConfig,
	now time.Time) ([]analyzer.Finding, bool) {
	at, source, ok := b.LastGood()
	if !ok {
		if b.NoRecentSuccess {
			return []analyzer.Finding{backupStaleFinding(resource, "warning",
				"No recent backup run of "+resource+" succeeded",
				map[string]any{"source": "backup_runs", "max_age_hours": policy.MaxAgeHours,
					"recent_runs_without_success": true})}, true
		}
		return nil, false
	}
	age := now.Sub(at)
	if age < -cloudtel.MaxClockSkew {
		return nil, false
	}
	limit := policy.MaxAge()
	if age <= limit {
		return []analyzer.Finding{}, true
	}
	severity := "warning"
	if age >= 2*limit {
		severity = "critical"
	}
	hours := math.Round(age.Hours()*10) / 10
	detail := map[string]any{"source": source, "age_hours": hours,
		"max_age_hours": policy.MaxAgeHours, "last_good": at.UTC().Format(time.RFC3339)}
	title := fmt.Sprintf("Last good backup of %s is %.0f hours old", resource, hours)
	return []analyzer.Finding{backupStaleFinding(resource, severity, title, detail)}, true
}

func backupStaleFinding(resource, severity, title string,
	detail map[string]any) analyzer.Finding {
	detail["resource"] = resource
	return analyzer.Finding{Category: categoryBackupStale, Severity: severity,
		ObjectType: "backup", ObjectIdentifier: "backup:" + resource, Title: title,
		Detail: detail, ActionRisk: "safe",
		Recommendation: "The provider reports no good backup within backup.max_age_hours: " +
			"check the instance's automated backups and point-in-time recovery in the " +
			"provider console (failed runs, a paused instance, a lapsed backup window). " +
			"A restore today would lose everything since the last good backup."}
}

// retentionFinding: retention below the policy. 0 days is AP-11's
// "backups off" case and a 0 policy turns the check off.
func retentionFinding(days int, resource string,
	policy config.BackupConfig) []analyzer.Finding {
	if days <= 0 || policy.MinRetentionDays <= 0 || days >= policy.MinRetentionDays {
		return []analyzer.Finding{}
	}
	return []analyzer.Finding{{Category: categoryBackupRetention, Severity: "warning",
		ObjectType: "backup", ObjectIdentifier: "backup:" + resource,
		Title: fmt.Sprintf("%s keeps backups for %d days, below the %d-day policy",
			resource, days, policy.MinRetentionDays),
		Detail: map[string]any{"retention_days": days,
			"min_retention_days": policy.MinRetentionDays, "resource": resource},
		ActionRisk: "safe",
		Recommendation: "Raise the instance's backup retention in the provider console " +
			"(RDS backup retention period, Cloud SQL transaction log retention), or lower " +
			"backup.min_retention_days if the shorter window is intended."}}
}

// reasonLatch reports a reason the first time it is seen, so a withheld
// freshness is logged once per reason rather than every cycle.
type reasonLatch struct {
	mu   sync.Mutex
	last string
}

// changed records reason and reports whether it is a new, non-empty one.
func (l *reasonLatch) changed(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	fresh := reason != "" && reason != l.last
	l.last = reason
	return fresh
}

// upsertBackupFindings refreshes the backup findings from the current
// telemetry; unavailable telemetry changes nothing.
func (rt *databaseRuntime) upsertBackupFindings(ctx context.Context) error {
	if rt.cloud == nil {
		return nil
	}
	st := rt.cloud.Status()
	if !st.Available {
		return nil
	}
	b := rt.cloud.Backup()
	reason := ""
	if b != nil {
		reason = b.FreshnessUnavailable
	}
	if rt.backupNote.changed(reason) {
		logWarn(rt.spec.Scope, "db %q: backup freshness unavailable: %s", rt.spec.Name,
			reason)
	}
	rt.noteIsolation(b)
	evaluated := backupFindings(b, st.Resource, rt.cfg.Backup, time.Now())
	for category, findings := range isolationFindings(b, st.Resource, rt.cfg.Backup,
		productionOf(ctx, rt.spec.Name)) {
		evaluated[category] = findings
	}
	for category, findings := range evaluated {
		if err := replaceFindings(ctx, rt.spec.Pool, category, findings); err != nil {
			return err
		}
	}
	return nil
}

// noteIsolation logs a withheld isolation read once per reason (drill-01b).
func (rt *databaseRuntime) noteIsolation(b *cloudtel.BackupPosture) {
	reason := ""
	if b != nil {
		reason = b.IsolationUnavailable
	}
	if rt.isolationNote.changed(reason) {
		logWarn(rt.spec.Scope, "db %q: backup isolation unavailable: %s", rt.spec.Name,
			reason)
	}
}
