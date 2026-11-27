package api

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// Readers of scanned row maps: the source projections the cases view feeds
// the cases package, and the generic value readers they use.

func sourceFindingFromMap(row map[string]any) cases.SourceFinding {
	return cases.SourceFinding{
		ID:               stringValue(row["id"]),
		DatabaseName:     stringValue(row["database_name"]),
		Category:         stringValue(row["category"]),
		Severity:         cases.Severity(stringValue(row["severity"])),
		ObjectType:       stringValue(row["object_type"]),
		ObjectIdentifier: stringValue(row["object_identifier"]),
		RuleID:           stringValue(row["rule_id"]),
		Title:            stringValue(row["title"]),
		Recommendation:   stringValue(row["recommendation"]),
		RecommendedSQL:   stringValue(row["recommended_sql"]),
		RollbackSQL:      stringValue(row["rollback_sql"]),
		Detail:           detailMap(row["detail"]),
		ObservedAt:       timeFromMap(row, "last_seen"),
	}
}

func sourceIncidentFromMap(row map[string]any) cases.SourceIncident {
	return cases.SourceIncident{
		ID:              stringValue(row["id"]),
		DatabaseName:    stringValue(row["database_name"]),
		Severity:        cases.Severity(stringValue(row["severity"])),
		RootCause:       stringValue(row["root_cause"]),
		CausalChain:     incidentChainFromAny(row["causal_chain"]),
		AffectedObjects: stringSliceFromAny(row["affected_objects"]),
		SignalIDs:       stringSliceFromAny(row["signal_ids"]),
		RecommendedSQL:  stringValue(row["recommended_sql"]),
		ActionRisk:      stringValue(row["action_risk"]),
		Source:          stringValue(row["source"]),
		Confidence:      floatValue(row["confidence"]),
		DetectedAt:      timeFromMap(row, "detected_at"),
		LastDetectedAt:  timeFromMap(row, "last_detected_at"),
		ResolvedAt:      timePtrFromMap(row, "resolved_at"),
		OccurrenceCount: int(floatValue(row["occurrence_count"])),

		PreviousIncidentID: stringValue(row["previous_incident_id"]),
	}
}

func sourceQueryHintFromMap(row map[string]any) cases.SourceQueryHint {
	return cases.SourceQueryHint{
		QueryID:          int64Value(row["queryid"]),
		DatabaseName:     stringValue(row["database_name"]),
		HintText:         stringValue(row["hint_text"]),
		Symptom:          stringValue(row["symptom"]),
		Status:           stringValue(row["status"]),
		CreatedAt:        timeFromMap(row, "created_at"),
		BeforeCost:       floatPtrFromAny(row["before_cost"]),
		AfterCost:        floatPtrFromAny(row["after_cost"]),
		SuggestedRewrite: stringValue(row["suggested_rewrite"]),
		RewriteRationale: stringValue(row["rewrite_rationale"]),
		VerifiedAt:       timePtrFromMap(row, "verified_at"),
		RolledBackAt:     timePtrFromMap(row, "rolled_back_at"),
	}
}

func stringSliceFromAny(v any) []string {
	switch items := v.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s := stringValue(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func timePtrFromMap(row map[string]any, key string) *time.Time {
	if row[key] == nil {
		return nil
	}
	t := timeFromMap(row, key)
	if t.IsZero() {
		return nil
	}
	return &t
}

func detailMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{"raw": value}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func floatValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	default:
		return 0
	}
}

func floatPtrFromAny(value any) *float64 {
	if value == nil {
		return nil
	}
	f := floatValue(value)
	return &f
}

// int64Value extracts an int64 from a row value without a lossy float
// round-trip. Integer types are returned exactly; float types are
// truncated only as a last resort for values that arrived as floats.
func int64Value(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}
