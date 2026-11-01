package lakebaselive

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/service/sql"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// This read-only follow-up uses exact prior owned markers to separate absent
// capture from missing application attribution and payload masking. No query,
// plan or DDL payload is returned, including when the current role can see it.
func TestLakebaseTelemetryAttribution(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_TELEMETRY_ATTRIBUTION") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_TELEMETRY_ATTRIBUTION=1 and an owned prior telemetry report")
	}
	var prior telemetryNativeReport
	raw, err := os.ReadFile(os.Getenv("SAGE_LAKEBASE_TELEMETRY_PRIOR_REPORT"))
	if err != nil || json.Unmarshal(raw, &prior) != nil {
		t.Fatal("valid prior owned telemetry report required")
	}
	if !regexp.MustCompile(`^(lakebaseobs_acceptance_[0-9a-f]{32}|lbo_[0-9a-f]{24})$`).MatchString(prior.SyntheticApplication) || !regexp.MustCompile(`^sage_telemetry_[0-9a-f]{32}$`).MatchString(prior.OwnedSchema) || prior.SyntheticQueryID == 0 || time.Since(prior.StartedAt) < 0 || time.Since(prior.StartedAt) > 24*time.Hour || prior.FinishedAt.Before(prior.StartedAt) || prior.FinishedAt.Sub(prior.StartedAt) > time.Hour {
		t.Fatal("prior owned markers or bounded observation interval invalid")
	}
	if os.Getenv("SAGE_LAKEBASE_ENDPOINT") != "projects/search/branches/pg-sage-test/endpoints/primary" || os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID") == "" {
		t.Fatal("exact isolated endpoint and warehouse required")
	}
	p, ctx := livePool(t)
	resource, err := databricksauth.ResolveTelemetryResource(ctx, p.Config().ConnConfig.Host, os.Getenv("SAGE_LAKEBASE_ENDPOINT"))
	if err != nil || resource.Workspace != prior.Scope.Workspace || resource.ProjectID != prior.Scope.ProjectID || resource.BranchID != prior.Scope.BranchID || resource.EndpointID != prior.Scope.EndpointID {
		t.Fatal("prior resource does not match the API-verified test endpoint")
	}
	var oid int64
	if err = p.QueryRow(ctx, "SELECT oid::bigint FROM pg_database WHERE datname=current_database()").Scan(&oid); err != nil || oid != prior.Scope.DatabaseOID || prior.Scope.DatabaseName != "pg_sage_test" {
		t.Fatal("prior database binding mismatch")
	}
	settings := map[string]string{}
	// Public prerequisites and internally researched diagnostic values are read
	// only. An internal setting's visibility is not a customer support contract.
	for _, name := range []string{"application_name", "databricks.o11y_exporter_enabled", "neon_monitor.log_capture_enabled", "neon_monitor.plan_enabled", "neon_monitor.plan_exclude_appname_prefix", "neon_monitor.plan_exclude_pattern", "neon_monitor.plan_include_pattern", "neon_monitor.plan_min_duration", "neon_monitor.plan_sample_rate", "neon_monitor.ddl_enabled"} {
		var value *string
		if err = p.QueryRow(ctx, "SELECT current_setting($1,true)", name).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != nil {
			settings[name] = *value
		}
	}
	rows, err := p.Query(ctx, `SELECT name,setting FROM pg_settings WHERE name LIKE 'neon_monitor.plan_%' AND (name LIKE '%duration%' OR name LIKE '%sample%' OR name LIKE '%dedup%' OR name LIKE '%capture%') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, value string
		if err = rows.Scan(&name, &value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		settings[name] = value
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	api, err := databricksauth.StatementExecution()
	if err != nil {
		t.Fatal(err)
	}
	params := []sql.StatementParameterListItem{{Name: "project", Value: prior.Scope.ProjectID}, {Name: "branch", Value: prior.Scope.BranchID}, {Name: "endpoint", Value: prior.Scope.EndpointID}, {Name: "database_oid", Type: "BIGINT", Value: strconv.FormatInt(oid, 10)}, {Name: "from", Value: prior.StartedAt.Format(time.RFC3339Nano)}, {Name: "to", Value: prior.FinishedAt.Add(time.Minute).Format(time.RFC3339Nano)}, {Name: "application", Value: prior.SyntheticApplication}, {Name: "queryid", Type: "BIGINT", Value: strconv.FormatInt(prior.SyntheticQueryID, 10)}, {Name: "owned_schema", Value: prior.OwnedSchema}}
	base := " WHERE project_id=:project AND branch_id=:branch AND endpoint_id=:endpoint AND dbid=:database_oid AND ts>=CAST(:from AS TIMESTAMP) AND ts<CAST(:to AS TIMESTAMP)"
	counts := "SELECT CAST(count(*) AS STRING) matched_rows, CAST(count_if(application_name=:application) AS STRING) exact_app_rows, CAST(count_if(application_name IS NULL OR application_name='') AS STRING) missing_app_rows, CAST(count_if(query_text IS NULL OR query_text='<REDACTED>') AS STRING) masked_statement_rows, date_format(min(ts),'yyyy-MM-dd HH:mm:ss.SSSSSSXXX') first_event, date_format(max(ts),'yyyy-MM-dd HH:mm:ss.SSSSSSXXX') latest_event"
	results := []telemetryRoutingResult{
		runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.Plans, "exact_owned_queryid_without_application_filter", counts+", CAST(count_if(plan_json IS NULL OR plan_json='<REDACTED>') AS STRING) masked_plan_rows FROM system.lakebase.plan_history"+base+" AND queryid=:queryid", params),
		runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.Plans, "exact_owned_schema_token_without_application_filter", counts+", CAST(count_if(queryid=:queryid) AS STRING) exact_queryid_rows FROM system.lakebase.plan_history"+base+" AND instr(query_text,:owned_schema)>0", params),
		runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.Plans, "selected_resource_metadata_completeness_counts", "SELECT CAST(count(*) AS STRING) total_rows, CAST(count_if(application_name IS NULL OR application_name='') AS STRING) missing_app_rows, CAST(count_if(queryid IS NULL OR queryid=0) AS STRING) missing_queryid_rows, CAST(count_if(query_text IS NULL OR query_text='<REDACTED>') AS STRING) masked_statement_rows, CAST(count_if(duration_ms>=1000) AS STRING) at_least_one_second_rows FROM system.lakebase.plan_history"+base, params),
		runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.DDL, "exact_owned_schema_without_application_filter", counts+" FROM system.lakebase.ddl_history"+base+" AND (schema_name=:owned_schema OR object_name=:owned_schema)", params),
	}
	if prior.SyntheticBackendPID > 0 {
		params = append(params, sql.StatementParameterListItem{Name: "backend_pid", Type: "BIGINT", Value: strconv.FormatInt(int64(prior.SyntheticBackendPID), 10)})
		for _, stream := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.DDL} {
			results = append(results, runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), stream, "exact_owned_backend_without_application_filter", counts+" FROM system.lakebase."+string(stream)+base+" AND backend_pid=:backend_pid", params))
		}
		// These names belong to an exact unique owned object on the verified
		// backend. Return no customer SQL, plans, DDL text or other app names.
		nameCounts := "SELECT CAST(application_name AS STRING), CAST(length(application_name) AS STRING), CAST(count(*) AS STRING) FROM system.lakebase."
		results = append(results,
			runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.Plans, "exact_owned_schema_and_backend_application_names", nameCounts+string(lakebaseobs.Plans)+base+" AND backend_pid=:backend_pid AND instr(query_text,:owned_schema)>0 GROUP BY application_name LIMIT 20", params),
			runTelemetryDiagnostic(ctx, api, os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID"), lakebaseobs.DDL, "exact_owned_schema_and_backend_application_names", nameCounts+string(lakebaseobs.DDL)+base+" AND backend_pid=:backend_pid AND (schema_name=:owned_schema OR object_name=:owned_schema) GROUP BY application_name LIMIT 20", params))
	}
	writeProviderCompatibilityReport(t, struct {
		ObservedAt time.Time                `json:"observed_at"`
		Settings   map[string]string        `json:"read_only_settings"`
		Results    []telemetryRoutingResult `json:"exact_owned_marker_metadata"`
	}{time.Now().UTC(), settings, results}, "SAGE_LAKEBASE_TELEMETRY_ATTRIBUTION_REPORT")
	for _, result := range results {
		t.Logf("stream=%s selector=%s aggregate_counts=%v error=%s", result.Stream, result.Selector, result.Rows, result.Error)
		if result.Error != "" {
			t.Error("owned attribution diagnostic query failed")
		}
	}
}
