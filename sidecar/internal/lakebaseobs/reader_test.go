package lakebaseobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/service/sql"
)

func testScope() Scope {
	return Scope{DeploymentID: "5a4d6cd4-9fbe-49ed-a33f-c6b707462101", DatabaseID: "5a4d6cd4-9fbe-49ed-a33f-c6b707462102", Workspace: "https://test.cloud.databricks.com", ProjectID: "project", BranchID: "br-test", EndpointID: "ep-test", DatabaseName: "fixture", DatabaseOID: 123}
}
func testOptions() Options { return Options{WarehouseID: "test", AllowPayload: true}.defaults() }
func testFields(stream Stream, at time.Time) map[string]string {
	f := map[string]string{"project_id": "project", "branch_id": "br-test", "endpoint_id": "ep-test", "compute_id": "compute-1", "compute_mode": "read-write", "ts": at.Format(time.RFC3339Nano), "dbid": "123", "datid": "123", "datname": "fixture", "row_position": "1", "sample_seq": "1", "queryid": "17", "query_text": "SELECT 42", "plan_json": `{"Plan":{"Node Type":"Result","Total Cost":1.25}}`, "total_cost": "1.25", "duration_ms": "12.5", "analyze_enabled": "false", "buffers_enabled": "true", "truncated": "false", "backend_pid": "91", "truncated_mask": "0", "message": "synthetic deadlock", "severity": "ERROR", "sqlstate": "40P01", "pid": "91", "previous_ts": at.Add(-time.Minute).Format(time.RFC3339Nano)}
	out := map[string]string{}
	for _, col := range specs[stream].columns {
		out[col] = f[col]
	}
	return out
}
func eventFor(t *testing.T, stream Stream, at time.Time) Event {
	t.Helper()
	e, err := NewEvent(stream, testScope(), testFields(stream, at), at.Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type fakeSDK struct {
	submit   func(sql.ExecuteStatementRequest) (*sql.StatementResponse, error)
	poll     func(sql.GetStatementRequest) (*sql.StatementResponse, error)
	chunk    func(sql.GetStatementResultChunkNRequest) (*sql.ResultData, error)
	canceled int
}

func (f *fakeSDK) ExecuteStatement(_ context.Context, q sql.ExecuteStatementRequest) (*sql.StatementResponse, error) {
	return f.submit(q)
}
func (f *fakeSDK) GetStatement(_ context.Context, q sql.GetStatementRequest) (*sql.StatementResponse, error) {
	return f.poll(q)
}
func (f *fakeSDK) GetStatementResultChunkN(_ context.Context, q sql.GetStatementResultChunkNRequest) (*sql.ResultData, error) {
	return f.chunk(q)
}
func (f *fakeSDK) CancelExecution(context.Context, sql.CancelExecutionRequest) error {
	f.canceled++
	return nil
}
func sdkResult(stream Stream, fields []map[string]string) *sql.StatementResponse {
	cols := specs[stream].columns
	schema := &sql.ResultSchema{ColumnCount: len(cols)}
	for _, col := range cols {
		schema.Columns = append(schema.Columns, sql.ColumnInfo{Name: col, TypeName: sql.ColumnInfoTypeNameString})
	}
	rows := [][]string{}
	for _, f := range fields {
		values := []string{}
		for _, col := range cols {
			values = append(values, f[col])
		}
		rows = append(rows, values)
	}
	return &sql.StatementResponse{StatementId: "synthetic-statement", Status: &sql.StatementStatus{State: sql.StatementStateSucceeded}, Manifest: &sql.ResultManifest{Format: sql.FormatJsonArray, Schema: schema, TotalRowCount: int64(len(rows))}, Result: &sql.ResultData{DataArray: rows}}
}

func TestQueryScopeAndMetadata(t *testing.T) {
	s := testScope()
	s.ProjectID = "selected' OR true --"
	w := Window{time.Now().Add(-time.Hour), time.Now()}
	o := testOptions()
	o.AllowPayload = false
	q, params, _, err := query(Plans, s, w, o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, s.ProjectID) || !strings.Contains(q, "project_id = :project") || !strings.Contains(q, "AND `dbid` = :database_oid") || !strings.Contains(q, "CAST(NULL AS STRING) AS `plan_json`") {
		t.Fatal("query does not contain bounded parameterized scope/projection")
	}
	if params[0].Value != s.ProjectID || strings.Contains(q, "workspace_id") || strings.Contains(q, "SELECT *") {
		t.Fatal("resource identity was guessed or interpolated")
	}
	if _, _, _, err = query(ComputeGauges, s, w, o); !errors.Is(err, ErrScope) {
		t.Fatalf("compute scope: %v", err)
	}
	for _, bad := range []string{"http://test.cloud.databricks.com", "https://user@test.cloud.databricks.com", "https://test.cloud.databricks.com/path"} {
		s.Workspace = bad
		if s.Validate() == nil {
			t.Fatalf("accepted invalid origin %s", bad)
		}
	}
}

func TestTimestampOffsetsAndGovernedSourceNarrowing(t *testing.T) {
	a, err := parseTime("2026-10-29 12:00:00.123456-05:00")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseTime("2026-10-29 17:00:00.123456Z")
	if err != nil || !a.Equal(b) {
		t.Fatal("warehouse timezone changed event time")
	}
	if _, err = parseTime("2026-10-29 12:00:00.123456"); err == nil {
		t.Fatal("ambiguous offset-free timestamp accepted")
	}
	o := testOptions()
	o.SourceObjects = map[Stream]string{Plans: "governed.lakebase.plan_view"}
	s := testScope()
	s.ApplicationName = "owned synthetic app"
	now := time.Now()
	q, params, _, err := query(Plans, s, Window{now.Add(-time.Hour), now}, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "FROM `governed`.`lakebase`.`plan_view`") || !strings.Contains(q, "date_format(`ts`") || !strings.Contains(q, "application_name = :application") || params[len(params)-1].Value != s.ApplicationName {
		t.Fatal("source/time/application boundary absent")
	}
	if _, _, _, err = query(StatementCounters, s, Window{now.Add(-time.Hour), now}, o); !errors.Is(err, ErrScope) {
		t.Fatal("unattributable payload read permitted")
	}
	o.SourceObjects[Plans] = "governed.lakebase.view;SELECT"
	if _, _, _, err = query(Plans, s, Window{now.Add(-time.Hour), now}, o); !errors.Is(err, ErrSchema) {
		t.Fatal("SQL expression accepted as source")
	}
}

func TestReaderChunksAndNulls(t *testing.T) {
	now := time.Now().UTC()
	f := testFields(Plans, now.Add(-time.Minute))
	response := sdkResult(Plans, []map[string]string{f, f})
	second := response.Result.DataArray[1:]
	response.Result.DataArray = response.Result.DataArray[:1]
	response.Result.NextChunkIndex = 1
	client := &fakeSDK{submit: func(q sql.ExecuteStatementRequest) (*sql.StatementResponse, error) {
		if q.Disposition != sql.DispositionInline || q.Format != sql.FormatJsonArray || q.WaitTimeout != "0s" || q.ByteLimit == 0 {
			t.Fatal("unbounded/external request")
		}
		return response, nil
	}, chunk: func(q sql.GetStatementResultChunkNRequest) (*sql.ResultData, error) {
		if q.StatementId != response.StatementId || q.ChunkIndex != 1 {
			t.Fatal(q)
		}
		return &sql.ResultData{ChunkIndex: 1, DataArray: second}, nil
	}}
	r, err := NewSDKReader(client, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.ReadWindow(context.Background(), Plans, testScope(), Window{now.Add(-time.Hour), now})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 || b.Events[0].ID != b.Events[1].ID || client.canceled != 0 {
		t.Fatal("chunk/duplicate identity handling failed")
	}
	// Metadata mode defensively erases payload even if a stub/provider returns it.
	o := testOptions()
	o.AllowPayload = false
	r, _ = NewSDKReader(client, o)
	b, err = r.ReadWindow(context.Background(), Plans, testScope(), Window{now.Add(-time.Hour), now})
	if err != nil {
		t.Fatal(err)
	}
	if b.Events[0].Fields["plan_json"] != "" {
		t.Fatal("metadata mode persisted payload")
	}
}

func TestReaderRefusesTruncationDriftScopeAndExternalResults(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]struct {
		mutate func(*sql.StatementResponse)
		want   error
	}{
		"truncated": {func(r *sql.StatementResponse) { r.Manifest.Truncated = true }, ErrBudget},
		"schema":    {func(r *sql.StatementResponse) { r.Manifest.Schema.Columns[0].Name = "changed" }, ErrSchema},
		"type":      {func(r *sql.StatementResponse) { r.Manifest.Schema.Columns[0].TypeName = sql.ColumnInfoTypeNameLong }, ErrSchema},
		"scope":     {func(r *sql.StatementResponse) { r.Result.DataArray[0][0] = "another-project" }, ErrScope},
		"external": {func(r *sql.StatementResponse) {
			r.Result.ExternalLinks = []sql.ExternalLink{{ExternalLink: "https://untrusted.invalid"}}
		}, ErrSchema},
		"cycle": {func(r *sql.StatementResponse) {
			r.Result.NextChunkInternalLink = "/opaque"
			r.Result.NextChunkIndex = 0
		}, ErrSchema},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			response := sdkResult(Plans, []map[string]string{testFields(Plans, now.Add(-time.Minute))})
			c.mutate(response)
			client := &fakeSDK{submit: func(sql.ExecuteStatementRequest) (*sql.StatementResponse, error) { return response, nil }}
			r, _ := NewSDKReader(client, testOptions())
			_, err := r.ReadWindow(context.Background(), Plans, testScope(), Window{now.Add(-time.Hour), now})
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
}

func TestReaderCancelsPendingStatement(t *testing.T) {
	client := &fakeSDK{submit: func(sql.ExecuteStatementRequest) (*sql.StatementResponse, error) {
		return &sql.StatementResponse{StatementId: "pending", Status: &sql.StatementStatus{State: sql.StatementStateRunning}}, nil
	}}
	r, _ := NewSDKReader(client, testOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	now := time.Now()
	_, err := r.ReadWindow(ctx, Plans, testScope(), Window{now.Add(-time.Hour), now})
	if !errors.Is(err, context.DeadlineExceeded) || client.canceled != 1 {
		t.Fatalf("cancel: %v calls %d", err, client.canceled)
	}
}

func TestEventIdentityAndSourceIsolation(t *testing.T) {
	now := time.Now().UTC()
	f := testFields(Plans, now)
	e, _ := NewEvent(Plans, testScope(), f, now, true)
	again, _ := NewEvent(Plans, testScope(), f, now.Add(time.Hour), true)
	if e.ID != again.ID {
		t.Fatal("receipt time changed identity")
	}
	f["compute_id"] = "resumed-compute"
	next, _ := NewEvent(Plans, testScope(), f, now, true)
	if e.ID == next.ID {
		t.Fatal("compute epochs merged")
	}
	f["dbid"] = "124"
	if _, err := NewEvent(Plans, testScope(), f, now, true); !errors.Is(err, ErrScope) {
		t.Fatal("cross-database row accepted")
	}
}
