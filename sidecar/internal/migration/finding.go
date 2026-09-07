package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/pg-sage/sidecar/internal/rca"
)

const SafetyFindingCategory = "migration_safety"

type FindingSink interface {
	UpsertMigrationSafetyFinding(
		ctx context.Context,
		input MigrationSafetyFinding,
	) (int64, error)
}

type MigrationSafetyFinding struct {
	DatabaseName     string
	RuleID           string
	OriginalSQL      string
	ObjectIdentifier string
	ObjectType       string
	Title            string
	Severity         string
	Detail           map[string]any
	Recommendation   string
	RecommendedSQL   string
	ImpactScore      float64
}

func FindingFromIncident(
	pid int,
	originalSQL string,
	incident *rca.Incident,
) (MigrationSafetyFinding, bool) {
	if incident == nil {
		return MigrationSafetyFinding{}, false
	}
	ruleID := ruleIDFromIncident(incident)
	object := objectFromIncident(incident)
	detail := detailFromIncident(pid, originalSQL, incident)
	safeSQL := executableSafeSQL(incident.RecommendedSQL)
	return MigrationSafetyFinding{
		DatabaseName:     incident.DatabaseName,
		RuleID:           ruleID,
		OriginalSQL:      originalSQL,
		ObjectIdentifier: migrationFindingIdentity(incident.DatabaseName, ruleID, object, originalSQL),
		ObjectType:       "migration",
		Title:            incident.RootCause,
		Severity:         incident.Severity,
		Detail:           detail,
		Recommendation:   recommendationForIncident(incident),
		RecommendedSQL:   safeSQL,
		ImpactScore:      incident.Confidence,
	}, true
}

func ruleIDFromIncident(incident *rca.Incident) string {
	if len(incident.SignalIDs) > 0 {
		return incident.SignalIDs[0]
	}
	return "ddl_risk"
}

func objectFromIncident(incident *rca.Incident) string {
	if len(incident.AffectedObjects) > 0 {
		return incident.AffectedObjects[0]
	}
	return "unknown"
}

func detailFromIncident(
	pid int,
	originalSQL string,
	incident *rca.Incident,
) map[string]any {
	detail := map[string]any{
		"pid":              pid,
		"database_name":    incident.DatabaseName,
		"original_sql":     originalSQL,
		"safe_alternative": incident.RecommendedSQL,
		"risk_score":       incident.Confidence,
		"action_risk":      incident.ActionRisk,
		"source":           incident.Source,
	}
	if len(incident.SignalIDs) > 0 {
		detail["rule_id"] = incident.SignalIDs[0]
	}
	if len(incident.AffectedObjects) > 0 {
		detail["affected_objects"] = incident.AffectedObjects
	}
	if len(incident.CausalChain) > 0 {
		detail["causal_chain"] = incident.CausalChain
	}
	return detail
}

func recommendationForIncident(incident *rca.Incident) string {
	if incident.RecommendedSQL != "" {
		return "Review the safer migration path before continuing live DDL."
	}
	return "Review the migration risk and produce a safe rollout plan."
}

func migrationFindingIdentity(
	databaseName string,
	ruleID string,
	object string,
	originalSQL string,
) string {
	parts := strings.Join([]string{
		databaseName,
		ruleID,
		object,
		normalizeMigrationSQL(originalSQL),
	}, "\n")
	sum := sha256.Sum256([]byte(parts))
	return object + ":" + ruleID + ":" + hex.EncodeToString(sum[:8])
}

func normalizeMigrationSQL(sql string) string {
	return strings.Join(strings.Fields(strings.ToLower(sql)), " ")
}

func executableSafeSQL(sql string) string {
	trimmed := strings.TrimSpace(sql)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "DROP INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "ANALYZE ") {
		return trimmed
	}
	return ""
}
