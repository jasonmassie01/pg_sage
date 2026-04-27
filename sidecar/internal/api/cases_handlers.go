package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func casesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		if mgr == nil {
			writeCasesResponse(w, database, []cases.Case{})
			return
		}

		projected, err := queryProjectedCases(r.Context(), mgr, database)
		if err != nil {
			slog.Error("query cases failed", "error", err)
			jsonError(w, "failed to query cases", http.StatusInternalServerError)
			return
		}
		writeCasesResponse(w, database, projected)
	}
}

func writeCasesResponse(w http.ResponseWriter, database string, projected []cases.Case) {
	if projected == nil {
		projected = []cases.Case{}
	}
	jsonResponse(w, map[string]any{
		"database": database,
		"cases":    projected,
		"total":    len(projected),
	})
}

func queryProjectedCases(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	database string,
) ([]cases.Case, error) {
	filters := fleet.FindingFilters{Status: "open", Limit: 500, Sort: "severity", Order: "desc"}
	pools := poolsForDatabaseSelection(mgr, database)
	out := make([]cases.Case, 0)
	for _, selected := range pools {
		rows, _, err := queryFindings(ctx, selected.pool, filters, selected.name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, cases.ProjectFinding(sourceFindingFromMap(row)))
		}
	}
	return out, nil
}

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
		Detail:           detailMap(row["detail"]),
	}
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
