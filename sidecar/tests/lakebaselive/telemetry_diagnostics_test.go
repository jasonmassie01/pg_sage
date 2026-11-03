package lakebaselive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/databricks/databricks-sdk-go/service/sql"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// Diagnostics contain only counts and identifiers. The application query is
// restricted to the UUID-tagged session created by this test, never customer SQL.
type telemetryRoutingResult struct {
	Stream      lakebaseobs.Stream `json:"stream"`
	Selector    string             `json:"selector"`
	StatementID string             `json:"statement_id,omitempty"`
	Rows        [][]string         `json:"rows,omitempty"`
	Error       string             `json:"error,omitempty"`
}

func diagnoseTelemetryRouting(ctx context.Context, api lakebaseobs.StatementClient, warehouse string, scope lakebaseobs.Scope, tag string, from time.Time) []telemetryRoutingResult {
	var out []telemetryRoutingResult
	for _, stream := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.DDL} {
		// Names are diagnostic alternatives only. They never authorize a
		// production scope or a payload read; API-verified IDs remain pinned.
		params := []sql.StatementParameterListItem{
			{Name: "project_uid", Value: scope.ProjectID}, {Name: "branch_uid", Value: scope.BranchID}, {Name: "endpoint_uid", Value: scope.EndpointID},
			{Name: "project_name", Value: "search"}, {Name: "branch_name", Value: "pg-sage-test"},
			{Name: "from", Value: from.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)},
		}
		alternative := "project_id=:project_uid OR project_id=:project_name OR branch_id=:branch_uid OR endpoint_id=:endpoint_uid OR (project_name=:project_name AND branch_name=:branch_name)"
		if stream == lakebaseobs.Plans {
			alternative += " OR neon_branch_id=:branch_uid OR neon_endpoint_id=:endpoint_uid"
		}
		statement := "SELECT CAST(count(*) AS STRING) matched_rows, CAST(count_if(project_id=:project_uid) AS STRING) project_uid_rows, CAST(count_if(project_id=:project_name) AS STRING) project_slug_rows, CAST(count_if(branch_id=:branch_uid) AS STRING) branch_uid_rows, CAST(count_if(endpoint_id=:endpoint_uid) AS STRING) endpoint_uid_rows, date_format(max(ts),'yyyy-MM-dd HH:mm:ss.SSSSSSXXX') latest_event FROM system.lakebase." + string(stream) + " WHERE ts>=CAST(:from AS TIMESTAMP) AND (" + alternative + ")"
		out = append(out, runTelemetryDiagnostic(ctx, api, warehouse, stream, "selected_ids_or_names_last_24h", statement, params))
		params = []sql.StatementParameterListItem{{Name: "application", Value: tag}, {Name: "from", Value: from.UTC().Format(time.RFC3339Nano)}}
		statement = "SELECT CAST(project_id AS STRING), CAST(branch_id AS STRING), CAST(endpoint_id AS STRING), CAST(count(*) AS STRING), date_format(max(ts),'yyyy-MM-dd HH:mm:ss.SSSSSSXXX') FROM system.lakebase." + string(stream) + " WHERE ts>=CAST(:from AS TIMESTAMP) AND application_name=:application GROUP BY project_id,branch_id,endpoint_id LIMIT 20"
		out = append(out, runTelemetryDiagnostic(ctx, api, warehouse, stream, "owned_application_without_resource_filter", statement, params))
	}
	return out
}

func runTelemetryDiagnostic(parent context.Context, api lakebaseobs.StatementClient, warehouse string, stream lakebaseobs.Stream, selector, statement string, params []sql.StatementParameterListItem) telemetryRoutingResult {
	result := telemetryRoutingResult{Stream: stream, Selector: selector}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	response, err := api.ExecuteStatement(ctx, sql.ExecuteStatementRequest{WarehouseId: warehouse, Statement: statement, Parameters: params, Disposition: sql.DispositionInline, Format: sql.FormatJsonArray, WaitTimeout: "0s", OnWaitTimeout: sql.ExecuteStatementRequestOnWaitTimeoutContinue, ByteLimit: 64 << 10, RowLimit: 21})
	if err == nil && response != nil {
		result.StatementID = response.StatementId
	}
	complete := false
	defer func() {
		if !complete && result.StatementID != "" {
			cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_ = api.CancelExecution(cleanup, sql.CancelExecutionRequest{StatementId: result.StatementID})
		}
	}()
	for err == nil && response != nil && response.Status != nil {
		switch response.Status.State {
		case sql.StatementStateSucceeded:
			complete = true
			if response.Manifest != nil && response.Manifest.TotalRowCount == 0 && !response.Manifest.Truncated {
				return result
			}
			if response.Manifest == nil || response.Manifest.Truncated || response.Manifest.TotalRowCount > 20 || response.Result == nil || len(response.Result.ExternalLinks) > 0 || response.Result.NextChunkIndex != 0 {
				err = errors.New("diagnostic response exceeded inline bounds")
			} else {
				result.Rows = response.Result.DataArray
			}
			if err != nil {
				result.Error = err.Error()
			}
			return result
		case sql.StatementStatePending, sql.StatementStateRunning:
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(250 * time.Millisecond):
				response, err = api.GetStatement(ctx, sql.GetStatementRequest{StatementId: result.StatementID})
			}
		default:
			complete = true
			err = fmt.Errorf("diagnostic state %s", response.Status.State)
		}
	}
	if err == nil {
		err = errors.New("diagnostic response unavailable")
	}
	result.Error = err.Error()
	return result
}
