package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/pg-sage/sidecar/internal/rca"
)

const SafetyFindingCategory = "migration_safety"

// SQLSourceDeterministic marks an incident's RecommendedSQL as one of
// pg_sage's own templates. Model text never carries it (migration-10).
const SQLSourceDeterministic = "deterministic"

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

// FindingFromIncident builds the finding for incident. risk is the
// advisor's winning assessment (nil when there is none, as for an LLM
// fallback or lock-queue-only incident); its detection-time measurements
// go into the detail, where the case preflight reads them (migration-01).
func FindingFromIncident(
	pid int,
	originalSQL string,
	incident *rca.Incident,
	risk *DDLRisk,
) (MigrationSafetyFinding, bool) {
	if incident == nil {
		return MigrationSafetyFinding{}, false
	}
	// Never persist observed literals (G7-B01): they carry passwords,
	// connection strings and business data.
	originalSQL = sanitizeDDL(originalSQL)
	ruleID := ruleIDFromIncident(incident)
	object := objectFromIncident(incident)
	detail := detailFromIncident(pid, originalSQL, incident)
	addRiskMeasurements(detail, risk)
	safeSQL := executableSafeSQL(deterministicSQL(incident))
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
		"safe_alternative": deterministicSQL(incident),
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
		detail["causal_chain"] = sanitizedChain(incident.CausalChain)
	}
	return detail
}

func sanitizedChain(chain []rca.ChainLink) []rca.ChainLink {
	out := make([]rca.ChainLink, len(chain))
	for i, link := range chain {
		link.Evidence = sanitizeDDL(link.Evidence)
		out[i] = link
	}
	return out
}

// deterministicSQL is the incident's recommendation when pg_sage's own
// template wrote it, else "": a finding never recommends model text.
func deterministicSQL(incident *rca.Incident) string {
	if incident.RecommendedSQLSource != SQLSourceDeterministic {
		return ""
	}
	return incident.RecommendedSQL
}

func recommendationForIncident(incident *rca.Incident) string {
	if deterministicSQL(incident) != "" {
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

// executableSafeSQL keeps sql only when it is one statement of a kind the
// executor runs alone; the prefix alone let a batch through (migration-10).
func executableSafeSQL(sql string) string {
	trimmed := strings.TrimSpace(sql)
	if len(splitStatements(sanitizeDDL(trimmed))) != 1 {
		return ""
	}
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "DROP INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "ANALYZE ") {
		return trimmed
	}
	return ""
}
