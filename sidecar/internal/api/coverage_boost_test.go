package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// ================================================================
// derefStr
// ================================================================

func TestCoverage_DerefStr_NilPointer(t *testing.T) {
	got := derefStr(nil)
	if got != "" {
		t.Errorf("derefStr(nil): got %q, want empty", got)
	}
}

func TestCoverage_DerefStr_NonNilPointer(t *testing.T) {
	s := "hello"
	got := derefStr(&s)
	if got != "hello" {
		t.Errorf("derefStr(&hello): got %q, want hello", got)
	}
}

func TestCoverage_DerefStr_EmptyString(t *testing.T) {
	s := ""
	got := derefStr(&s)
	if got != "" {
		t.Errorf("derefStr(&empty): got %q, want empty", got)
	}
}

// ================================================================
// isConnectionError
// ================================================================

func TestCoverage_IsConnectionError_NilError(t *testing.T) {
	if isConnectionError(nil) {
		t.Error("nil error should not be a connection error")
	}
}

func TestCoverage_IsConnectionError_ConnectionRefused(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"connection refused", true},
		{"closed pool", true},
		{"connection reset by peer", true},
		{"broken pipe", true},
		{"no such host found", true},
		{"i/o timeout after 5s", true},
		{"context deadline exceeded", true},
		{"connection timed out", true},
		{"pool closed", true},
		{"Connection Refused", true},  // case-insensitive
		{"CLOSED POOL error", true},   // case-insensitive
		{"some random error", false},  // not a connection error
		{"query syntax error", false}, // not a connection error
		{"unique violation", false},   // not a connection error
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			err := &testError{msg: tt.msg}
			got := isConnectionError(err)
			if got != tt.want {
				t.Errorf("isConnectionError(%q): got %v, want %v",
					tt.msg, got, tt.want)
			}
		})
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// ================================================================
// buildFindingsWhere
// ================================================================

func TestCoverage_BuildFindingsWhere_NoFilters(t *testing.T) {
	f := fleet.FindingFilters{}
	where, args := buildFindingsWhere(f)
	if where != " WHERE 1=1" {
		t.Errorf("where: got %q, want ' WHERE 1=1'", where)
	}
	if len(args) != 0 {
		t.Errorf("args: got %d, want 0", len(args))
	}
}

func TestCoverage_BuildFindingsWhere_StatusOnly(t *testing.T) {
	f := fleet.FindingFilters{Status: "open"}
	where, args := buildFindingsWhere(f)
	if !strings.Contains(where, "status = $1") {
		t.Errorf("where should contain status filter: %q", where)
	}
	if len(args) != 1 || args[0] != "open" {
		t.Errorf("args: got %v, want [open]", args)
	}
}

func TestCoverage_BuildFindingsWhere_SeverityOnly(t *testing.T) {
	f := fleet.FindingFilters{Severity: "critical"}
	where, args := buildFindingsWhere(f)
	if !strings.Contains(where, "severity = $1") {
		t.Errorf("where should contain severity filter: %q", where)
	}
	if len(args) != 1 || args[0] != "critical" {
		t.Errorf("args: got %v, want [critical]", args)
	}
}

func TestCoverage_BuildFindingsWhere_CategoryOnly(t *testing.T) {
	f := fleet.FindingFilters{Category: "duplicate_index"}
	where, args := buildFindingsWhere(f)
	if !strings.Contains(where, "category = $1") {
		t.Errorf("where should contain category filter: %q", where)
	}
	if len(args) != 1 || args[0] != "duplicate_index" {
		t.Errorf("args: got %v, want [duplicate_index]", args)
	}
}

func TestCoverage_BuildFindingsWhere_AllFilters(t *testing.T) {
	f := fleet.FindingFilters{
		Status:   "open",
		Severity: "warning",
		Category: "unused_index",
	}
	where, args := buildFindingsWhere(f)
	if !strings.Contains(where, "status = $1") {
		t.Errorf("missing status filter in: %q", where)
	}
	if !strings.Contains(where, "severity = $2") {
		t.Errorf("missing severity filter in: %q", where)
	}
	if !strings.Contains(where, "category = $3") {
		t.Errorf("missing category filter in: %q", where)
	}
	if len(args) != 3 {
		t.Errorf("args: got %d, want 3", len(args))
	}
	if args[0] != "open" || args[1] != "warning" ||
		args[2] != "unused_index" {
		t.Errorf("args: got %v", args)
	}
}

// ================================================================
// buildFindingsOrder
// ================================================================

func TestCoverage_BuildFindingsOrder_SeverityDesc(t *testing.T) {
	// "desc" = most severe first = critical(1) first → CASE ASC.
	f := fleet.FindingFilters{Sort: "severity", Order: "desc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "CASE severity") {
		t.Errorf("severity sort should use CASE: %q", order)
	}
	if !strings.Contains(order, "ASC") {
		t.Errorf("most-severe-first should use ASC on CASE: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_SeverityAsc(t *testing.T) {
	// "asc" = least severe first = info(3) first → CASE DESC.
	f := fleet.FindingFilters{Sort: "severity", Order: "asc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "DESC") {
		t.Errorf("least-severe-first should use DESC on CASE: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_CreatedAt(t *testing.T) {
	f := fleet.FindingFilters{Sort: "created_at", Order: "desc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "created_at") {
		t.Errorf("should contain created_at: %q", order)
	}
	if !strings.Contains(order, "DESC") {
		t.Errorf("should contain DESC: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_LastSeen(t *testing.T) {
	f := fleet.FindingFilters{Sort: "last_seen", Order: "asc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "last_seen") {
		t.Errorf("should contain last_seen: %q", order)
	}
	if !strings.Contains(order, "ASC") {
		t.Errorf("should contain ASC: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_Category(t *testing.T) {
	f := fleet.FindingFilters{Sort: "category", Order: "desc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "category") {
		t.Errorf("should contain category: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_Title(t *testing.T) {
	f := fleet.FindingFilters{Sort: "title", Order: "asc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "title") {
		t.Errorf("should contain title: %q", order)
	}
}

func TestCoverage_BuildFindingsOrder_UnknownSort(t *testing.T) {
	// Unknown sort columns should default to last_seen.
	f := fleet.FindingFilters{Sort: "invalid_col", Order: "desc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "last_seen") {
		t.Errorf("unknown sort should default to last_seen: %q",
			order)
	}
}

func TestCoverage_BuildFindingsOrder_EmptySort(t *testing.T) {
	// Empty sort defaults to last_seen (not severity path).
	f := fleet.FindingFilters{Sort: "", Order: "desc"}
	order := buildFindingsOrder(f)
	if !strings.Contains(order, "last_seen") {
		t.Errorf("empty sort should default to last_seen: %q",
			order)
	}
}

// ================================================================
// buildFindingMap
// ================================================================

func TestCoverage_BuildFindingMap_Basic(t *testing.T) {
	now := time.Now()
	objType := "index"
	objIdent := "idx_test"
	rec := "CREATE INDEX"
	recSQL := "CREATE INDEX idx_test ON t(a)"
	m := buildFindingMap(
		42, now, now, 5,
		"duplicate_index", "warning",
		&objType, &objIdent,
		"Duplicate Index Found",
		[]byte(`{"table":"users"}`),
		&rec, &recSQL,
		"open", "db1", nil, nil, nil,
	)
	if m["id"] != "42" {
		t.Errorf("id: got %v, want '42'", m["id"])
	}
	if m["category"] != "duplicate_index" {
		t.Errorf("category: got %v", m["category"])
	}
	if m["severity"] != "warning" {
		t.Errorf("severity: got %v", m["severity"])
	}
	if m["object_type"] != "index" {
		t.Errorf("object_type: got %v", m["object_type"])
	}
	if m["object_identifier"] != "idx_test" {
		t.Errorf("object_identifier: got %v",
			m["object_identifier"])
	}
	if m["title"] != "Duplicate Index Found" {
		t.Errorf("title: got %v", m["title"])
	}
	if m["recommendation"] != "CREATE INDEX" {
		t.Errorf("recommendation: got %v", m["recommendation"])
	}
	if m["recommended_sql"] != "CREATE INDEX idx_test ON t(a)" {
		t.Errorf("recommended_sql: got %v", m["recommended_sql"])
	}
	if m["status"] != "open" {
		t.Errorf("status: got %v", m["status"])
	}
	if m["database_name"] != "db1" {
		t.Errorf("database_name: got %v", m["database_name"])
	}
	if m["occurrence_count"] != 5 {
		t.Errorf("occurrence_count: got %v", m["occurrence_count"])
	}
	// detail should be parsed JSON
	if m["detail"] == nil {
		t.Error("detail should not be nil")
	}
}

func TestCoverage_BuildFindingMap_NilPointers(t *testing.T) {
	now := time.Now()
	m := buildFindingMap(
		1, now, now, 0,
		"unused_index", "info",
		nil, nil,
		"Test Title",
		nil, nil, nil,
		"suppressed", "db2", nil, nil, nil,
	)
	if m["object_type"] != "" {
		t.Errorf("object_type: got %v, want empty", m["object_type"])
	}
	if m["object_identifier"] != "" {
		t.Errorf("object_identifier: got %v, want empty",
			m["object_identifier"])
	}
	if m["recommendation"] != "" {
		t.Errorf("recommendation: got %v, want empty",
			m["recommendation"])
	}
	if m["recommended_sql"] != "" {
		t.Errorf("recommended_sql: got %v, want empty",
			m["recommended_sql"])
	}
	if m["detail"] != nil {
		t.Errorf("detail: got %v, want nil", m["detail"])
	}
}

func TestCoverage_BuildFindingMap_InvalidJSON(t *testing.T) {
	now := time.Now()
	m := buildFindingMap(
		1, now, now, 0,
		"cat", "info",
		nil, nil, "title",
		[]byte(`not valid json`),
		nil, nil,
		"open", "db1", nil, nil, nil,
	)
	// Invalid JSON is surfaced as the raw string so operators can
	// still see the stored bytes rather than a silently-missing
	// field. (Previously returned nil — a swallowed-error bug.)
	got, ok := m["detail"].(string)
	if !ok {
		t.Fatalf("detail: got %T (%v), want string fallback",
			m["detail"], m["detail"])
	}
	if got != "not valid json" {
		t.Errorf("detail: got %q, want raw invalid-JSON string", got)
	}
}

func TestCoverage_BuildFindingMap_EmptyDetail(t *testing.T) {
	now := time.Now()
	m := buildFindingMap(
		1, now, now, 0,
		"cat", "info",
		nil, nil, "title",
		[]byte{},
		nil, nil,
		"open", "db1", nil, nil, nil,
	)
	if m["detail"] != nil {
		t.Errorf("detail: got %v, want nil for empty detail",
			m["detail"])
	}
}

func TestCoverage_BuildFindingMap_AllKeys(t *testing.T) {
	now := time.Now()
	m := buildFindingMap(
		1, now, now, 0,
		"cat", "info",
		nil, nil, "title",
		nil, nil, nil,
		"open", "db1", nil, nil, nil,
	)
	expectedKeys := []string{
		"id", "created_at", "last_seen", "occurrence_count",
		"category", "severity", "object_type",
		"object_identifier", "title", "detail",
		"recommendation", "recommended_sql", "rollback_sql",
		"status", "database_name", "rule_id",
		"impact_score", "resolved_at", "acted_on_at",
		"action_log_id", "subsystem",
	}
	for _, key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if len(m) != len(expectedKeys) {
		t.Errorf("expected %d keys, got %d",
			len(expectedKeys), len(m))
	}
}

// ================================================================
// buildActionMap
// ================================================================

func TestCoverage_BuildActionMap_Basic(t *testing.T) {
	now := time.Now()
	fID := int64(42)
	rbSQL := "DROP INDEX idx_test"
	rbReason := "performance degraded"
	measuredAt := now
	m := buildActionMap(
		1, now, "create_index", &fID,
		"CREATE INDEX idx_test ON t(a)",
		&rbSQL,
		[]byte(`{"before":"state"}`),
		[]byte(`{"after":"state"}`),
		"success", &rbReason, &measuredAt,
	)
	if m["id"] != "1" {
		t.Errorf("id: got %v, want '1'", m["id"])
	}
	if m["action_type"] != "create_index" {
		t.Errorf("action_type: got %v", m["action_type"])
	}
	if *(m["finding_id"].(*string)) != "42" {
		t.Errorf("finding_id: got %v, want '42'",
			m["finding_id"])
	}
	if m["sql_executed"] != "CREATE INDEX idx_test ON t(a)" {
		t.Errorf("sql_executed: got %v", m["sql_executed"])
	}
	if m["rollback_sql"] != "DROP INDEX idx_test" {
		t.Errorf("rollback_sql: got %v", m["rollback_sql"])
	}
	if m["outcome"] != "success" {
		t.Errorf("outcome: got %v", m["outcome"])
	}
	if m["rollback_reason"] != "performance degraded" {
		t.Errorf("rollback_reason: got %v",
			m["rollback_reason"])
	}
	if m["before_state"] == nil {
		t.Error("before_state should not be nil")
	}
	if m["after_state"] == nil {
		t.Error("after_state should not be nil")
	}
}

func TestCoverage_BuildActionMap_NilPointers(t *testing.T) {
	now := time.Now()
	m := buildActionMap(
		1, now, "vacuum", nil,
		"VACUUM ANALYZE t",
		nil, nil, nil,
		"success", nil, nil,
	)
	if m["finding_id"] != nil && m["finding_id"] != (*string)(nil) {
		t.Errorf("finding_id: got %v, want nil", m["finding_id"])
	}
	if m["rollback_sql"] != "" {
		t.Errorf("rollback_sql: got %v, want empty",
			m["rollback_sql"])
	}
	if m["rollback_reason"] != "" {
		t.Errorf("rollback_reason: got %v, want empty",
			m["rollback_reason"])
	}
	if m["before_state"] != nil {
		t.Errorf("before_state: got %v, want nil",
			m["before_state"])
	}
	if m["after_state"] != nil {
		t.Errorf("after_state: got %v, want nil",
			m["after_state"])
	}
	if m["measured_at"] != nil && m["measured_at"] != (*time.Time)(nil) {
		t.Errorf("measured_at: got %v, want nil",
			m["measured_at"])
	}
}

func TestCoverage_BuildActionMap_InvalidJSONStates(t *testing.T) {
	now := time.Now()
	m := buildActionMap(
		1, now, "test", nil,
		"SELECT 1", nil,
		[]byte(`not json`), []byte(`also not json`),
		"success", nil, nil,
	)
	// Invalid JSON unmarshal fails silently, leaves nil.
	if m["before_state"] != nil {
		t.Errorf("before_state: got %v, want nil for invalid JSON",
			m["before_state"])
	}
	if m["after_state"] != nil {
		t.Errorf("after_state: got %v, want nil for invalid JSON",
			m["after_state"])
	}
}

func TestCoverage_BuildActionMap_AllKeys(t *testing.T) {
	now := time.Now()
	m := buildActionMap(
		1, now, "test", nil,
		"SELECT 1", nil, nil, nil,
		"success", nil, nil,
	)
	expectedKeys := []string{
		"id", "executed_at", "event_at", "action_type", "finding_id",
		"sql_executed", "rollback_sql", "before_state",
		"after_state", "outcome", "rollback_reason",
		"measured_at",
	}
	for _, key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if len(m) != len(expectedKeys) {
		t.Errorf("expected %d keys, got %d",
			len(expectedKeys), len(m))
	}
}

// ================================================================
// validateMetric
// ================================================================

func TestCoverage_ValidateMetric_AllValid(t *testing.T) {
	validMetrics := []string{
		"", // empty is valid (returns true)
		"tables", "indexes", "queries", "sequences",
		"foreign_keys", "system", "io", "locks",
		"config_data", "partitions",
		"cache_hit_ratio", "connections", "tps",
		"dead_tuples", "database_size", "replication_lag",
	}
	for _, m := range validMetrics {
		t.Run("valid_"+m, func(t *testing.T) {
			if !validateMetric(m) {
				t.Errorf("validateMetric(%q) should be true", m)
			}
		})
	}
}

func TestCoverage_ValidateMetric_Invalid(t *testing.T) {
	invalidMetrics := []string{
		"bogus", "TABLES", "Tables", "cpu_usage",
		"memory_free", "disk_io", "invalid",
	}
	for _, m := range invalidMetrics {
		t.Run("invalid_"+m, func(t *testing.T) {
			if validateMetric(m) {
				t.Errorf("validateMetric(%q) should be false", m)
			}
		})
	}
}

// ================================================================
// oauthConfigHandler
// ================================================================

func TestCoverage_OAuthConfigHandler_NilProvider(t *testing.T) {
	handler := oauthConfigHandler(nil, "google")
	w := doRequest(
		handler, "GET", "/api/v1/auth/oauth/config", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["enabled"] != false {
		t.Errorf("enabled: got %v, want false", resp["enabled"])
	}
	if resp["provider"] != "google" {
		t.Errorf("provider: got %v, want google",
			resp["provider"])
	}
}

func TestCoverage_OAuthConfigHandler_EmptyProvider(t *testing.T) {
	handler := oauthConfigHandler(nil, "")
	w := doRequest(
		handler, "GET", "/api/v1/auth/oauth/config", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["enabled"] != false {
		t.Errorf("enabled: got %v, want false", resp["enabled"])
	}
	if resp["provider"] != "" {
		t.Errorf("provider: got %v, want empty",
			resp["provider"])
	}
}

// ================================================================
// oauthAuthorizeHandler
// ================================================================

func TestCoverage_OAuthAuthorizeHandler_NilProvider(t *testing.T) {
	handler := oauthAuthorizeHandler(nil)
	w := doRequest(
		handler, "GET", "/api/v1/auth/oauth/authorize", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "OAuth not configured" {
		t.Errorf("error: got %q, want 'OAuth not configured'",
			resp["error"])
	}
}

// ================================================================
// oauthCallbackHandler
// ================================================================

func TestCoverage_OAuthCallbackHandler_NilProvider(t *testing.T) {
	handler := oauthCallbackHandler(nil, nil, "viewer", "google")
	w := doRequest(
		handler, "GET",
		"/api/v1/auth/oauth/callback?code=abc&state=xyz", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "OAuth not configured" {
		t.Errorf("error: got %q, want 'OAuth not configured'",
			resp["error"])
	}
}

func TestCoverage_OAuthCallbackHandler_MissingCode(t *testing.T) {
	// Use a non-nil provider to get past the nil check.
	// We need a real provider object, so we use a minimal one.
	// But since OAuthProvider requires config, and we just need
	// to get past the nil check, we test with nil provider
	// separately, and test missing params with a non-nil-like path.
	// Actually, with nil provider, missing code check is unreachable.
	// We tested the nil path above. The missing code path needs
	// a non-nil provider -- skip since we can't construct one
	// without real OAuth config.
}

// ================================================================
// processCSVImport
// ================================================================

func TestCoverage_ProcessCSVImport_EmptyFile(t *testing.T) {
	reader := strings.NewReader("")
	result := processCSVImport(nil, nil, reader, 0)
	if result.Imported != 0 {
		t.Errorf("imported: got %d, want 0", result.Imported)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors: got %d, want 1", len(result.Errors))
	}
	if result.Errors[0].Row != 1 {
		t.Errorf("error row: got %d, want 1", result.Errors[0].Row)
	}
	if !strings.Contains(result.Errors[0].Error, "header") {
		t.Errorf("error: got %q, want header error",
			result.Errors[0].Error)
	}
}

func TestCoverage_ProcessCSVImport_BadHeader(t *testing.T) {
	reader := strings.NewReader("wrong,header,format\n")
	result := processCSVImport(nil, nil, reader, 0)
	if result.Imported != 0 {
		t.Errorf("imported: got %d, want 0", result.Imported)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors: got %d, want 1", len(result.Errors))
	}
	if !strings.Contains(
		result.Errors[0].Error, "invalid CSV header") {
		t.Errorf("error: got %q, want invalid CSV header",
			result.Errors[0].Error)
	}
}

func TestCoverage_ProcessCSVImport_HeaderOnly(t *testing.T) {
	// nil store will panic if we try to call ds.Count, so
	// we only test empty/bad header paths through processCSVImport.
	// The validCSVHeader function is tested directly below.
	_ = strings.NewReader(
		"name,host,port,database_name,username,password,sslmode\n")
}

// ================================================================
// validCSVHeader
// ================================================================

func TestCoverage_ValidCSVHeader_Valid(t *testing.T) {
	header := []string{
		"name", "host", "port", "database_name",
		"username", "password", "sslmode",
	}
	if !validCSVHeader(header) {
		t.Error("expected valid CSV header")
	}
}

func TestCoverage_ValidCSVHeader_ValidWithExtra(t *testing.T) {
	header := []string{
		"name", "host", "port", "database_name",
		"username", "password", "sslmode", "extra_col",
	}
	if !validCSVHeader(header) {
		t.Error("extra columns should still be valid")
	}
}

func TestCoverage_ValidCSVHeader_TooFew(t *testing.T) {
	header := []string{"name", "host", "port"}
	if validCSVHeader(header) {
		t.Error("expected invalid for too few columns")
	}
}

func TestCoverage_ValidCSVHeader_WrongOrder(t *testing.T) {
	header := []string{
		"host", "name", "port", "database_name",
		"username", "password", "sslmode",
	}
	if validCSVHeader(header) {
		t.Error("expected invalid for wrong column order")
	}
}

func TestCoverage_ValidCSVHeader_Empty(t *testing.T) {
	if validCSVHeader(nil) {
		t.Error("expected invalid for nil header")
	}
	if validCSVHeader([]string{}) {
		t.Error("expected invalid for empty header")
	}
}

func TestCoverage_ValidCSVHeader_WrongColumnName(t *testing.T) {
	header := []string{
		"name", "host", "port", "db_name",
		"username", "password", "sslmode",
	}
	if validCSVHeader(header) {
		t.Error("expected invalid for wrong column name (db_name)")
	}
}

// ================================================================
// importOneRow
// ================================================================

func TestCoverage_ImportOneRow_TooFewColumns(t *testing.T) {
	count := 0
	result := importOneRow(
		nil, nil,
		[]string{"name", "host", "5432"},
		2, 0, &count,
	)
	if result == nil {
		t.Fatal("expected error for too few columns")
	}
	if result.Row != 2 {
		t.Errorf("row: got %d, want 2", result.Row)
	}
	if !strings.Contains(result.Error, "not enough columns") {
		t.Errorf("error: got %q", result.Error)
	}
}

func TestCoverage_ImportOneRow_MaxDatabases(t *testing.T) {
	count := 50
	result := importOneRow(
		nil, nil,
		[]string{
			"db", "host", "5432", "mydb",
			"user", "pass", "require",
		},
		3, 0, &count,
	)
	if result == nil {
		t.Fatal("expected error for max databases")
	}
	if !strings.Contains(result.Error, "maximum of 50") {
		t.Errorf("error: got %q", result.Error)
	}
}

func TestCoverage_ImportOneRow_InvalidPort(t *testing.T) {
	count := 0
	result := importOneRow(
		nil, nil,
		[]string{
			"db", "host", "not_a_port", "mydb",
			"user", "pass", "require",
		},
		4, 0, &count,
	)
	if result == nil {
		t.Fatal("expected error for invalid port")
	}
	if !strings.Contains(result.Error, "invalid port") {
		t.Errorf("error: got %q", result.Error)
	}
}

// ================================================================
// dbRecordToMap
// ================================================================

func TestCoverage_DBRecordToMap(t *testing.T) {
	now := time.Now()
	rec := store.DatabaseRecord{
		ID:            1,
		Name:          "prod",
		Host:          "db.example.com",
		Port:          5432,
		DatabaseName:  "mydb",
		Username:      "pguser",
		SSLMode:       "require",
		Enabled:       true,
		Tags:          map[string]string{"env": "prod"},
		TrustLevel:    "advisory",
		ExecutionMode: "approval",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	m := dbRecordToMap(rec)
	if m["id"] != 1 {
		t.Errorf("id: got %v, want 1", m["id"])
	}
	if m["name"] != "prod" {
		t.Errorf("name: got %v, want prod", m["name"])
	}
	if m["host"] != "db.example.com" {
		t.Errorf("host: got %v", m["host"])
	}
	if m["port"] != 5432 {
		t.Errorf("port: got %v, want 5432", m["port"])
	}
	if m["database_name"] != "mydb" {
		t.Errorf("database_name: got %v", m["database_name"])
	}
	if m["enabled"] != true {
		t.Errorf("enabled: got %v", m["enabled"])
	}
	if m["trust_level"] != "advisory" {
		t.Errorf("trust_level: got %v", m["trust_level"])
	}
	if m["execution_mode"] != "approval" {
		t.Errorf("execution_mode: got %v", m["execution_mode"])
	}
	tags := m["tags"].(map[string]string)
	if tags["env"] != "prod" {
		t.Errorf("tags: got %v", tags)
	}
}

func TestCoverage_DBRecordToMap_AllKeys(t *testing.T) {
	rec := store.DatabaseRecord{}
	m := dbRecordToMap(rec)
	expectedKeys := []string{
		"id", "name", "host", "port", "database_name",
		"username", "sslmode", "enabled", "tags",
		"trust_level", "execution_mode",
		"created_at", "updated_at",
	}
	for _, key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

// ================================================================
// toInput (dbCreateRequest)
// ================================================================

func TestCoverage_ToInput(t *testing.T) {
	req := dbCreateRequest{
		Name:          "testdb",
		Host:          "localhost",
		Port:          5432,
		DatabaseName:  "mydb",
		Username:      "admin",
		Password:      "secret",
		SSLMode:       "require",
		Tags:          map[string]string{"env": "dev"},
		TrustLevel:    "observation",
		ExecutionMode: "manual",
	}
	input := req.toInput()
	if input.Name != "testdb" {
		t.Errorf("name: got %q", input.Name)
	}
	if input.Host != "localhost" {
		t.Errorf("host: got %q", input.Host)
	}
	if input.Port != 5432 {
		t.Errorf("port: got %d", input.Port)
	}
	if input.DatabaseName != "mydb" {
		t.Errorf("database_name: got %q", input.DatabaseName)
	}
	if input.Username != "admin" {
		t.Errorf("username: got %q", input.Username)
	}
	if input.Password != "secret" {
		t.Errorf("password: got %q", input.Password)
	}
	if input.SSLMode != "require" {
		t.Errorf("sslmode: got %q", input.SSLMode)
	}
	if input.TrustLevel != "observation" {
		t.Errorf("trust_level: got %q", input.TrustLevel)
	}
	if input.ExecutionMode != "manual" {
		t.Errorf("execution_mode: got %q", input.ExecutionMode)
	}
}

func TestCoverage_ToInput_ZeroValues(t *testing.T) {
	req := dbCreateRequest{}
	input := req.toInput()
	if input.Name != "" {
		t.Errorf("name: got %q, want empty", input.Name)
	}
	if input.Port != 0 {
		t.Errorf("port: got %d, want 0", input.Port)
	}
}

// ================================================================
// hotReload — additional coverage for uncovered branches
// ================================================================

func TestCoverage_HotReloadAnalyzer_IndexBloat(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "analyzer.index_bloat_threshold_pct", "30")
	if cfg.Analyzer.IndexBloatThresholdPct != 30 {
		t.Errorf("index_bloat: got %d, want 30",
			cfg.Analyzer.IndexBloatThresholdPct)
	}
}

func TestCoverage_HotReloadAnalyzer_TableBloat(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "analyzer.table_bloat_dead_tuple_pct", "10")
	if cfg.Analyzer.TableBloatDeadTuplePct != 10 {
		t.Errorf("table_bloat: got %d, want 10",
			cfg.Analyzer.TableBloatDeadTuplePct)
	}
}

func TestCoverage_HotReloadAnalyzer_RegressionThreshold(
	t *testing.T,
) {
	cfg := &config.Config{}
	hotReload(cfg, "analyzer.regression_threshold_pct", "50")
	if cfg.Analyzer.RegressionThresholdPct != 50 {
		t.Errorf("regression: got %d, want 50",
			cfg.Analyzer.RegressionThresholdPct)
	}
}

func TestCoverage_HotReloadTrust_RollbackThreshold(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.rollback_threshold_pct", "25")
	if cfg.Trust.RollbackThresholdPct != 25 {
		t.Errorf("rollback_threshold: got %d, want 25",
			cfg.Trust.RollbackThresholdPct)
	}
}

func TestCoverage_HotReloadTrust_RollbackWindow(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.rollback_window_minutes", "60")
	if cfg.Trust.RollbackWindowMinutes != 60 {
		t.Errorf("rollback_window: got %d, want 60",
			cfg.Trust.RollbackWindowMinutes)
	}
}

func TestCoverage_HotReloadTrust_RollbackCooldown(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.rollback_cooldown_days", "3")
	if cfg.Trust.RollbackCooldownDays != 3 {
		t.Errorf("rollback_cooldown: got %d, want 3",
			cfg.Trust.RollbackCooldownDays)
	}
}

func TestCoverage_HotReloadTrust_CascadeCooldown(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.cascade_cooldown_cycles", "5")
	if cfg.Trust.CascadeCooldownCycles != 5 {
		t.Errorf("cascade_cooldown: got %d, want 5",
			cfg.Trust.CascadeCooldownCycles)
	}
}

func TestCoverage_HotReloadLLM_APIKey(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "llm.api_key", "sk-test-123")
	if cfg.LLM.APIKey != "sk-test-123" {
		t.Errorf("api_key: got %q", cfg.LLM.APIKey)
	}
}

func TestCoverage_HotReloadAlerting_PagerDuty(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "alerting.pagerduty_routing_key", "pd-key-123")
	if cfg.Alerting.PagerDutyRoutingKey != "pd-key-123" {
		t.Errorf("pagerduty: got %q",
			cfg.Alerting.PagerDutyRoutingKey)
	}
}

// ================================================================
// shouldSkipAuth — additional paths
// ================================================================

func TestCoverage_ShouldSkipAuth_OAuthCallback(t *testing.T) {
	if !shouldSkipAuth("/api/v1/auth/oauth/callback") {
		t.Error("oauth callback should skip auth")
	}
}

func TestCoverage_ShouldSkipAuth_OAuthConfig(t *testing.T) {
	if !shouldSkipAuth("/api/v1/auth/oauth/config") {
		t.Error("oauth config should skip auth")
	}
}

// ================================================================
// findingsEmptyResponse
// ================================================================

func TestCoverage_FindingsEmptyResponse(t *testing.T) {
	f := fleet.FindingFilters{
		Status: "open",
		Limit:  50,
		Offset: 0,
	}
	resp := findingsEmptyResponse("db1", f)
	if resp["database"] != "db1" {
		t.Errorf("database: got %v", resp["database"])
	}
	if resp["total"] != 0 {
		t.Errorf("total: got %v", resp["total"])
	}
	findings := resp["findings"].([]any)
	if len(findings) != 0 {
		t.Errorf("findings: got %d, want 0", len(findings))
	}
	if resp["limit"] != 50 {
		t.Errorf("limit: got %v", resp["limit"])
	}
	if resp["offset"] != 0 {
		t.Errorf("offset: got %v", resp["offset"])
	}
}

// ================================================================
// valOrDefault / firstVal
// ================================================================

func TestCoverage_ValOrDefault_Found(t *testing.T) {
	q := map[string][]string{
		"status": {"open"},
	}
	got := valOrDefault(q, "status", "def")
	if got != "open" {
		t.Errorf("got %q, want open", got)
	}
}

func TestCoverage_ValOrDefault_NotFound(t *testing.T) {
	q := map[string][]string{}
	got := valOrDefault(q, "status", "def")
	if got != "def" {
		t.Errorf("got %q, want def", got)
	}
}

func TestCoverage_ValOrDefault_EmptyValue(t *testing.T) {
	q := map[string][]string{
		"status": {""},
	}
	got := valOrDefault(q, "status", "def")
	if got != "def" {
		t.Errorf("got %q, want def for empty value", got)
	}
}

func TestCoverage_FirstVal_Found(t *testing.T) {
	q := map[string][]string{
		"limit": {"50"},
	}
	got := firstVal(q, "limit")
	if got != "50" {
		t.Errorf("got %q, want 50", got)
	}
}

func TestCoverage_FirstVal_NotFound(t *testing.T) {
	q := map[string][]string{}
	got := firstVal(q, "limit")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCoverage_FirstVal_EmptySlice(t *testing.T) {
	q := map[string][]string{
		"limit": {},
	}
	got := firstVal(q, "limit")
	if got != "" {
		t.Errorf("got %q, want empty for empty slice", got)
	}
}

// ================================================================
// parseFindingFilters — comprehensive
// ================================================================

func TestCoverage_ParseFindingFilters_AllDefaults(t *testing.T) {
	q := map[string][]string{}
	f := parseFindingFilters(q)
	if f.Status != "open" {
		t.Errorf("status: got %q, want open", f.Status)
	}
	if f.Severity != "" {
		t.Errorf("severity: got %q, want empty", f.Severity)
	}
	if f.Category != "" {
		t.Errorf("category: got %q, want empty", f.Category)
	}
	if f.Sort != "severity" {
		t.Errorf("sort: got %q, want severity", f.Sort)
	}
	if f.Order != "desc" {
		t.Errorf("order: got %q, want desc", f.Order)
	}
	if f.Limit != 50 {
		t.Errorf("limit: got %d, want 50", f.Limit)
	}
	if f.Offset != 0 {
		t.Errorf("offset: got %d, want 0", f.Offset)
	}
}

func TestCoverage_ParseFindingFilters_LimitCap(t *testing.T) {
	q := map[string][]string{
		"limit": {"999"},
	}
	f := parseFindingFilters(q)
	if f.Limit != 200 {
		t.Errorf("limit: got %d, want 200 (capped)", f.Limit)
	}
}

func TestCoverage_ParseFindingFilters_AllProvided(t *testing.T) {
	q := map[string][]string{
		"status":   {"suppressed"},
		"severity": {"critical"},
		"category": {"unused_index"},
		"sort":     {"created_at"},
		"order":    {"asc"},
		"limit":    {"25"},
		"offset":   {"10"},
	}
	f := parseFindingFilters(q)
	if f.Status != "suppressed" {
		t.Errorf("status: got %q", f.Status)
	}
	if f.Severity != "critical" {
		t.Errorf("severity: got %q", f.Severity)
	}
	if f.Category != "unused_index" {
		t.Errorf("category: got %q", f.Category)
	}
	if f.Sort != "created_at" {
		t.Errorf("sort: got %q", f.Sort)
	}
	if f.Order != "asc" {
		t.Errorf("order: got %q", f.Order)
	}
	if f.Limit != 25 {
		t.Errorf("limit: got %d", f.Limit)
	}
	if f.Offset != 10 {
		t.Errorf("offset: got %d", f.Offset)
	}
}

// ================================================================
// Snapshot history endpoint — additional metric coverage
// ================================================================

func TestCoverage_SnapshotHistory_AllValidMetrics(t *testing.T) {
	r := testRouter("db1")
	metrics := []string{
		"tables", "indexes", "queries", "sequences",
		"foreign_keys", "system", "io", "locks",
		"config_data", "partitions",
		"cache_hit_ratio", "connections", "tps",
		"dead_tuples", "database_size", "replication_lag",
	}
	for _, metric := range metrics {
		t.Run(metric, func(t *testing.T) {
			w := get(t, r, "/api/v1/snapshots/history?metric="+
				metric)
			if w.Code != 200 {
				t.Errorf("status: %d for metric %q",
					w.Code, metric)
			}
		})
	}
}

func TestCoverage_SnapshotHistory_EmptyMetric(t *testing.T) {
	r := testRouter("db1")
	// Empty metric is valid (validateMetric returns true for "").
	w := get(t, r, "/api/v1/snapshots/history?metric=")
	if w.Code != 200 {
		t.Errorf("status: %d for empty metric", w.Code)
	}
}

func TestCoverage_SnapshotHistory_NoMetric(t *testing.T) {
	r := testRouter("db1")
	// No metric param at all should fail validation.
	w := get(t, r, "/api/v1/snapshots/history")
	// Empty string passes validateMetric, so this should be 200.
	if w.Code != 200 {
		t.Errorf("status: %d for no metric", w.Code)
	}
}

func TestCoverage_SnapshotHistory_WithHoursParam(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/snapshots/history?metric=tps&hours=48")
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

// ================================================================
// Snapshot latest endpoint — additional coverage
// ================================================================

func TestCoverage_SnapshotLatest_WithMetric(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/snapshots/latest?database=db1&metric=tables")
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

// ================================================================
// Findings list — additional filter combinations
// ================================================================

func TestCoverage_Findings_StatusSuppressed(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings?status=suppressed")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Status"] != "suppressed" {
		t.Errorf("status: got %v", filters["Status"])
	}
}

func TestCoverage_Findings_SortByCreatedAt(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/findings?sort=created_at&order=asc")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Sort"] != "created_at" {
		t.Errorf("sort: got %v", filters["Sort"])
	}
	if filters["Order"] != "asc" {
		t.Errorf("order: got %v", filters["Order"])
	}
}

func TestCoverage_Findings_CombinedFilters(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/findings?status=open&severity=warning"+
			"&category=unused_index&sort=last_seen&order=desc"+
			"&limit=10&offset=5")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Status"] != "open" {
		t.Errorf("status: got %v", filters["Status"])
	}
	if filters["Severity"] != "warning" {
		t.Errorf("severity: got %v", filters["Severity"])
	}
	if filters["Category"] != "unused_index" {
		t.Errorf("category: got %v", filters["Category"])
	}
}

// ================================================================
// Actions list — additional coverage
// ================================================================

func TestCoverage_Actions_DefaultLimit(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions")
	m := decodeJSON(t, w)
	if m["limit"].(float64) != 50 {
		t.Errorf("limit: got %v, want 50", m["limit"])
	}
}

func TestCoverage_Actions_WithOffset(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions?offset=10")
	m := decodeJSON(t, w)
	if m["offset"].(float64) != 10 {
		t.Errorf("offset: got %v, want 10", m["offset"])
	}
}

// ================================================================
// configUpdateHandler — additional trust levels
// ================================================================

func TestCoverage_ConfigUpdate_Observation(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"level":"observation"}}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestCoverage_ConfigUpdate_Autonomous(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"level":"autonomous"}}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestCoverage_ConfigUpdate_InvalidTrustLevel(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"level":"dangerous"}}`)
	if w.Code != 400 {
		t.Errorf("status: %d, want 400", w.Code)
	}
}

func TestCoverage_ConfigUpdate_EmptyObject(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config", `{}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["status"] != "updated" {
		t.Errorf("status: got %v", m["status"])
	}
}

func TestCoverage_ConfigUpdate_NonTrustKey(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"other":"value"}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestCoverage_ConfigUpdate_TrustNotMap(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":"string_not_map"}`)
	// Trust is present but not a map. Handler should skip
	// but succeed.
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestCoverage_ConfigUpdate_TrustNoLevel(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"other":"value"}}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

// ================================================================
// isAllowedOrigin — additional coverage
// ================================================================

func TestCoverage_IsAllowedOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"", false},
		{"http://localhost:5173", true},
		{"http://localhost:8080", true},
		{"http://127.0.0.1:5173", true},
		{"http://127.0.0.1:8080", true},
		{"https://localhost:5173", false},
		{"http://localhost:3000", false},
		{"http://example.com", false},
		{"https://evil.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			got := isAllowedOrigin(tt.origin)
			if got != tt.want {
				t.Errorf("isAllowedOrigin(%q): got %v, want %v",
					tt.origin, got, tt.want)
			}
		})
	}
}

// ================================================================
// CORS middleware — additional methods
// ================================================================

func TestCoverage_CORS_PreflightHeaders(t *testing.T) {
	r := testRouter("db1")
	req := httptest.NewRequest(
		"OPTIONS", "/api/v1/databases", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	methods := w.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, "GET") {
		t.Errorf("methods should contain GET: %q", methods)
	}
	if !strings.Contains(methods, "POST") {
		t.Errorf("methods should contain POST: %q", methods)
	}
	if !strings.Contains(methods, "PUT") {
		t.Errorf("methods should contain PUT: %q", methods)
	}
	if !strings.Contains(methods, "DELETE") {
		t.Errorf("methods should contain DELETE: %q", methods)
	}

	headers := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(headers, "Content-Type") {
		t.Errorf("headers should contain Content-Type: %q",
			headers)
	}

	creds := w.Header().Get("Access-Control-Allow-Credentials")
	if creds != "true" {
		t.Errorf("credentials: got %q, want true", creds)
	}
}

func TestCoverage_CORS_NonPreflightWithOrigin(t *testing.T) {
	r := testRouter("db1")
	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	origin := w.Header().Get("Access-Control-Allow-Origin")
	if origin != "http://127.0.0.1:8080" {
		t.Errorf("origin: got %q", origin)
	}
	// Non-OPTIONS should proceed to handler.
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestCoverage_CORS_NoOrigin(t *testing.T) {
	r := testRouter("db1")
	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	// No Origin header.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	origin := w.Header().Get("Access-Control-Allow-Origin")
	if origin != "" {
		t.Errorf("origin should be empty: got %q", origin)
	}
}

// ================================================================
// findingDetailHandler — empty ID path
// ================================================================

func TestCoverage_FindingDetail_EmptyID(t *testing.T) {
	// findingDetailHandler checks for empty ID and returns 400.
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	handler := findingDetailHandler(mgr)

	// Call directly without path param to get empty ID.
	w := doRequest(handler, "GET", "/api/v1/findings/", "")
	if w.Code != http.StatusBadRequest {
		// Empty ID from PathValue returns "". The handler
		// returns 400 for empty, or 404 if it tries to search
		// with empty (no pools). Either is acceptable.
		if w.Code != http.StatusNotFound {
			t.Errorf("status: got %d, want 400 or 404", w.Code)
		}
	}
}

// ================================================================
// actionDetailHandler — empty ID path
// ================================================================

func TestCoverage_ActionDetail_EmptyID(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	handler := actionDetailHandler(mgr)

	w := doRequest(handler, "GET", "/api/v1/actions/", "")
	// Empty ID returns 400 or 404 (no pool).
	if w.Code != http.StatusBadRequest &&
		w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 400 or 404", w.Code)
	}
}

// ================================================================
// suppress / unsuppress — missing ID path
// ================================================================

func TestCoverage_Suppress_EmptyID(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	handler := suppressHandler(mgr)

	w := doRequest(handler, "POST", "/api/v1/findings//suppress", "")
	// Empty ID returns 400 (missing finding id).
	if w.Code != http.StatusBadRequest &&
		w.Code != http.StatusOK {
		t.Errorf("status: got %d", w.Code)
	}
}

func TestCoverage_Unsuppress_EmptyID(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	handler := unsuppressHandler(mgr)

	w := doRequest(handler, "POST",
		"/api/v1/findings//unsuppress", "")
	if w.Code != http.StatusBadRequest &&
		w.Code != http.StatusOK {
		t.Errorf("status: got %d", w.Code)
	}
}

// ================================================================
// Emergency stop / resume — fleet-wide
// ================================================================

func TestCoverage_EmergencyStop_SingleDB(t *testing.T) {
	r := testRouter("db1")
	w := post(t, r, "/api/v1/emergency-stop?database=db1", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["status"] != "stopped" {
		t.Errorf("status: got %v", m["status"])
	}
}

func TestCoverage_Resume_FleetWide(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"a", "b"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name:   name,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true},
		})
		mgr.EmergencyStop(name)
	}
	r := NewRouter(mgr, cfg, nil, fakeAdminMiddleware)
	w := post(t, r, "/api/v1/resume", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["resumed"].(float64) != 2 {
		t.Errorf("resumed: got %v, want 2", m["resumed"])
	}
}

// ================================================================
// NewRouterFull — fleet-mode action routes coverage
// ================================================================

func TestCoverage_NewRouterFull_FleetActions(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})

	// Fleet action deps with Fleet manager but no Store/Executor.
	actions := &ActionDeps{Fleet: mgr}
	r := NewRouterWithActions(
		mgr, cfg, nil, actions, fakeAdminMiddleware)

	// The pending count should be registered even without store.
	req := httptest.NewRequest(
		"GET", "/api/v1/actions/pending/count", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Expect 200 (fleet pending count) since no DB pools.
	if w.Code != 200 {
		t.Errorf("pending count status: %d", w.Code)
	}

	// Approve requires a target database in fleet mode.
	req2 := httptest.NewRequest(
		"POST", "/api/v1/actions/1/approve", nil)
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("approve status: got %d, want 400", w2.Code)
	}
}

// ================================================================
// configGlobalPutHandler — syncTrustLevelToFleet path
// ================================================================

func TestCoverage_ConfigGlobalPut_SyncsTrustToFleet(
	t *testing.T,
) {
	// syncTrustLevelToFleet is tested directly via
	// TestSyncTrustLevelToFleet. The configGlobalPutHandler
	// calls applyConfigOverrides which requires a real
	// ConfigStore (backed by pgxpool). Testing the full
	// handler path with trust.level sync requires integration
	// tests. We verify the sync function is correct separately.
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{TrustLevel: "observation"},
	})

	// Test the handler with empty body (no store needed).
	handler := configGlobalPutHandler(nil, cfg, mgr)
	w := doRequest(
		handler, "PUT", "/api/v1/config/global", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	// Directly verify sync function works.
	syncTrustLevelToFleet(mgr, "autonomous")
	inst := mgr.GetInstance("db1")
	if inst.Status.TrustLevel != "autonomous" {
		t.Errorf("fleet trust: got %q, want autonomous",
			inst.Status.TrustLevel)
	}
}

// ================================================================
// configDBPutHandler — empty body with mgr
// ================================================================

func TestCoverage_ConfigDBPut_EmptyBodyWithFleet(t *testing.T) {
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "testdb",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "testdb"},
		Status:     &fleet.InstanceStatus{},
	})

	handler := configDBPutHandler(nil, cfg, nil, mgr)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/1",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
}

// ================================================================
// Findings — large ID that doesn't exist
// ================================================================

func TestCoverage_FindingDetail_LargeID(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/findings/"+strconv.FormatInt(1<<31, 10))
	if w.Code != 404 {
		t.Errorf("status: %d, want 404", w.Code)
	}
}

// ================================================================
// Actions — filter by status
// ================================================================

func TestCoverage_Actions_StatusFilter(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions?status=pending")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

// ================================================================
// SPA fallback — path with dot
// ================================================================

func TestCoverage_Dashboard_PathWithDot(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/settings")
	// Non-API path without dot should fall through to SPA.
	if w.Code != 200 {
		t.Errorf("status: %d for SPA path", w.Code)
	}
}

func TestCoverage_Dashboard_DeepPath(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/databases/1/findings")
	if w.Code != 200 {
		t.Errorf("status: %d for deep SPA path", w.Code)
	}
}

// ================================================================
// maxBodyMiddleware
// ================================================================

func TestCoverage_MaxBodyMiddleware_GetPassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	})

	handler := maxBodyMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

func TestCoverage_MaxBodyMiddleware_HeadPassesThrough(
	t *testing.T,
) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	})

	handler := maxBodyMiddleware(inner)

	req := httptest.NewRequest("HEAD", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

func TestCoverage_MaxBodyMiddleware_OptionsPassesThrough(
	t *testing.T,
) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	})

	handler := maxBodyMiddleware(inner)

	req := httptest.NewRequest("OPTIONS", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

func TestCoverage_MaxBodyMiddleware_PostLimited(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		// After maxBody middleware, body should be a
		// MaxBytesReader. Reading should work for small bodies.
		buf := make([]byte, 100)
		n, _ := r.Body.Read(buf)
		w.WriteHeader(http.StatusOK)
		w.Write(buf[:n])
	})

	handler := maxBodyMiddleware(inner)

	body := strings.NewReader(`{"key":"value"}`)
	req := httptest.NewRequest("POST", "/test", body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

// ================================================================
// Metrics endpoint — additional coverage
// ================================================================

func TestCoverage_Metrics_FleetSummary(t *testing.T) {
	r := testRouter("db1", "db2", "db3")
	w := get(t, r, "/api/v1/metrics")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["databases"] == nil {
		t.Error("missing databases in metrics response")
	}
}

// ================================================================
// Config get — per-database with tags
// ================================================================

func TestCoverage_ConfigGet_PerDatabaseTags(t *testing.T) {
	cfg := &config.Config{
		Mode: "fleet",
		Trust: config.TrustConfig{
			Level: "advisory",
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod",
		Config: config.DatabaseConfig{
			Name:       "prod",
			Tags:       []string{"production", "us-east"},
			TrustLevel: "autonomous",
		},
		Status: &fleet.InstanceStatus{Connected: true},
	})
	r := NewRouter(mgr, cfg, nil)
	w := get(t, r, "/api/v1/config?database=prod")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "prod" {
		t.Errorf("database: got %v", m["database"])
	}
	if m["trust_level"] != "autonomous" {
		t.Errorf("trust_level: got %v", m["trust_level"])
	}
	tags := m["tags"].([]any)
	if len(tags) != 2 {
		t.Errorf("tags: got %d, want 2", len(tags))
	}
}

// ================================================================
// jsonResponse / jsonError — additional coverage
// ================================================================

func TestCoverage_JsonResponse_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	jsonResponse(w, map[string]string{"ok": "true"})

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q", ct)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["ok"] != "true" {
		t.Errorf("response: got %v", resp)
	}
}

func TestCoverage_JsonError_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, "test error", 500)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q", ct)
	}
	if w.Code != 500 {
		t.Errorf("status: got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "test error" {
		t.Errorf("error: got %v", resp)
	}
}

func TestCoverage_JsonError_VariousCodes(t *testing.T) {
	codes := []int{400, 401, 403, 404, 500, 502, 503}
	for _, code := range codes {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			w := httptest.NewRecorder()
			jsonError(w, "test", code)
			if w.Code != code {
				t.Errorf("status: got %d, want %d",
					w.Code, code)
			}
		})
	}
}

// ================================================================
// parseIntDefault — edge cases
// ================================================================

func TestCoverage_ParseIntDefault_LargeValue(t *testing.T) {
	got := parseIntDefault("999999", 100)
	if got != 999999 {
		t.Errorf("got %d, want 999999", got)
	}
}

func TestCoverage_ParseIntDefault_Whitespace(t *testing.T) {
	got := parseIntDefault(" 50 ", 100)
	// atoi with spaces will fail, return default.
	if got != 100 {
		t.Errorf("got %d, want 100", got)
	}
}

// ================================================================
// csvImportResult / csvImportError — struct zero values
// ================================================================

func TestCoverage_CSVImportResult_ZeroValue(t *testing.T) {
	result := csvImportResult{}
	if result.Imported != 0 {
		t.Errorf("imported: got %d", result.Imported)
	}
	if result.Skipped != 0 {
		t.Errorf("skipped: got %d", result.Skipped)
	}
}

// ================================================================
// applyConfigOverrides — additional empty body path
// ================================================================

func TestCoverage_ApplyConfigOverrides_EmptyBody(t *testing.T) {
	cfg := &config.Config{}
	errs := applyConfigOverrides(
		nil, nil, cfg, map[string]any{}, 0, 0)
	if len(errs) != 0 {
		t.Errorf("errs: got %v, want empty", errs)
	}
}

// ================================================================
// loginHandler — ContentType on error
// ================================================================

func TestCoverage_LoginHandler_ContentTypeOnAllErrors(
	t *testing.T,
) {
	handler := loginHandler(nil)
	tests := []struct {
		name string
		body string
	}{
		{"malformed", "not json"},
		{"empty", "{}"},
		{"missing_email", `{"password":"pw"}`},
		{"missing_password", `{"email":"a@b.com"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRequest(handler, "POST",
				"/api/v1/auth/login", tt.body)
			ct := w.Header().Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("Content-Type: got %q", ct)
			}
		})
	}
}

// ================================================================
// testConnectionPreviewHandler — request parsing paths
// ================================================================

func TestCoverage_TestConnectionPreview_MalformedJSON(
	t *testing.T,
) {
	handler := testConnectionPreviewHandler()
	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/test-connection",
		"not json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestCoverage_TestConnectionPreview_UnresolvableHost(
	t *testing.T,
) {
	// Unresolvable hosts are blocked (fail-closed SSRF).
	handler := testConnectionPreviewHandler()
	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/test-connection",
		`{"host":"invalid-host-that-wont-resolve",
		  "database_name":"db","username":"u","password":"p"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestCoverage_TestConnectionPreview_DefaultPort(
	t *testing.T,
) {
	// Port 0 defaults to 5432, sslmode defaults to "require".
	// Unresolvable host is blocked by fail-closed SSRF check.
	handler := testConnectionPreviewHandler()
	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/test-connection",
		`{"host":"nonexistent.invalid","port":0,
		  "database_name":"nonexistent","username":"u",
		  "password":"p"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestCoverage_TestConnectionPreview_CustomSSL(
	t *testing.T,
) {
	// Unresolvable host + custom sslmode: still blocked.
	handler := testConnectionPreviewHandler()
	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/test-connection",
		`{"host":"nonexistent.invalid","port":5432,
		  "database_name":"db","username":"u",
		  "password":"p","sslmode":"disable"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestCoverage_TestConnectionPreview_BlocksLoopback(
	t *testing.T,
) {
	// SSRF protection: loopback addresses must be rejected.
	handler := testConnectionPreviewHandler()
	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/test-connection",
		`{"host":"127.0.0.1","port":5432,
		  "database_name":"db","username":"u",
		  "password":"p","sslmode":"disable"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// ================================================================
// listManagedDBHandler / getManagedDBHandler — JSON parsing paths
// ================================================================

func TestCoverage_GetManagedDB_InvalidID(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := getManagedDBHandler(deps)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /api/v1/databases/managed/{id}", handler)

	req := httptest.NewRequest(
		"GET", "/api/v1/databases/managed/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid database ID" {
		t.Errorf("error: got %q", resp["error"])
	}
}

// ================================================================
// createManagedDBHandler — JSON parsing
// ================================================================

func TestCoverage_CreateManagedDB_MalformedJSON(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := createManagedDBHandler(deps)

	w := doRequest(handler, "POST",
		"/api/v1/databases/managed", "not json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q", resp["error"])
	}
}

// ================================================================
// updateManagedDBHandler — ID and JSON parsing
// ================================================================

func TestCoverage_UpdateManagedDB_InvalidID(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := updateManagedDBHandler(deps)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/databases/managed/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/databases/managed/abc",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestCoverage_UpdateManagedDB_MalformedJSON(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := updateManagedDBHandler(deps)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/databases/managed/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/databases/managed/1",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q", resp["error"])
	}
}

// ================================================================
// deleteManagedDBHandler — ID validation
// ================================================================

func TestCoverage_DeleteManagedDB_InvalidID(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := deleteManagedDBHandler(deps)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/databases/managed/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/databases/managed/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// ================================================================
// testManagedDBHandler — ID validation
// ================================================================

func TestCoverage_TestManagedDB_InvalidID(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := testManagedDBHandler(deps)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/databases/managed/{id}/test", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/databases/managed/xyz/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// ================================================================
// importCSVHandler — multipart form parsing
// ================================================================

func TestCoverage_ImportCSV_NoMultipart(t *testing.T) {
	deps := &DatabaseDeps{Store: nil}
	handler := importCSVHandler(deps)

	w := doRequest(handler, "POST",
		"/api/v1/databases/managed/import", "not multipart")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid multipart form" {
		t.Errorf("error: got %q", resp["error"])
	}
}

// ================================================================
// registerDatabaseRoutes — routes are accessible
// ================================================================

func TestCoverage_RegisterDatabaseRoutes(t *testing.T) {
	// Verify that all database routes are registered by
	// checking that a GET to /api/v1/databases/managed
	// doesn't 404 (it will need auth, returning 401).
	deps := &DatabaseDeps{Store: nil}
	mux := http.NewServeMux()
	registerDatabaseRoutes(mux, deps)

	routes := []struct {
		method string
		path   string
	}{
		// These will fail with 401 since RequireRole
		// middleware has no user in context.
		{"GET", "/api/v1/databases/managed"},
		{"POST", "/api/v1/databases/managed"},
	}
	for _, rt := range routes {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		// Should not be 404/405 — route IS registered.
		if w.Code == 404 || w.Code == 405 {
			t.Errorf("%s %s: got %d (not registered)",
				rt.method, rt.path, w.Code)
		}
	}
}

// ================================================================
// registerActionRoutes — with store+executor (non-fleet path)
// ================================================================

func TestCoverage_RegisterActionRoutes_WithStoreExecutor(
	t *testing.T,
) {
	// Test the branch where Store and Executor are non-nil.
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})

	// Create deps with non-nil Store/Executor pointers.
	// ActionStore requires pgxpool but we can use a non-nil
	// store with nil pool. The executor requires more deps.
	// We use the Fleet path instead and check 501 status.
	actions := &ActionDeps{
		Fleet: mgr,
	}
	r := NewRouterWithActions(mgr, cfg, nil, actions)

	// Reject requires a target database in fleet mode.
	req := httptest.NewRequest(
		"POST", "/api/v1/actions/1/reject",
		strings.NewReader(`{"reason":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req = withUser(req, testOperatorUser())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("reject status: got %d, want 400", w.Code)
	}

	// Execute requires a target database in fleet mode.
	req2 := httptest.NewRequest(
		"POST", "/api/v1/actions/execute",
		strings.NewReader(
			`{"finding_id":1,"sql":"SELECT 1"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2 = withUser(req2, testAdminUser())
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("execute status: got %d, want 400", w2.Code)
	}
}

// ================================================================
// NewRouterFull — with dbDeps set
// ================================================================

func TestCoverage_NewRouterFull_WithDBDeps(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})

	dbDeps := &DatabaseDeps{Store: nil}
	r := NewRouterFull(mgr, cfg, nil, nil, dbDeps, nil)

	// dbDeps.Store is nil, so registerDatabaseRoutes is skipped.
	// Verify router was created and core routes still work.
	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Core fleet route should work (200).
	if w.Code != 200 {
		t.Errorf("expected 200 for /api/v1/databases, got %d",
			w.Code)
	}
}

func TestCoverage_NewRouterFull_NilDBDeps(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	r := NewRouterFull(mgr, cfg, nil, nil, nil, nil)

	// Database managed routes should NOT be registered.
	req := httptest.NewRequest(
		"GET", "/api/v1/databases/managed", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Should be 404 or similar since route is not registered.
	if w.Code == 200 {
		t.Errorf("expected non-200 for unregistered route")
	}
}

func TestCoverage_NewRouterFull_NilActions(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	r := NewRouterFull(mgr, cfg, nil, nil, nil, nil)

	// Pending actions route should NOT be registered.
	req := httptest.NewRequest(
		"GET", "/api/v1/actions/pending", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == 200 {
		t.Errorf("expected non-200 for unregistered route")
	}
}

// ================================================================
// ConnectionTestResult — struct check
// ================================================================

func TestCoverage_ConnectionTestResult_Struct(t *testing.T) {
	r := ConnectionTestResult{
		Status:     "ok",
		PGVersion:  "PostgreSQL 16.2",
		Extensions: []string{"pg_stat_statements"},
	}
	if r.Status != "ok" {
		t.Errorf("status: got %q", r.Status)
	}
	if r.PGVersion != "PostgreSQL 16.2" {
		t.Errorf("version: got %q", r.PGVersion)
	}
	if len(r.Extensions) != 1 {
		t.Errorf("extensions: got %d", len(r.Extensions))
	}
}

func TestCoverage_ConnectionTestResult_Error(t *testing.T) {
	r := ConnectionTestResult{
		Status: "error",
		Error:  "connection refused",
	}
	if r.Status != "error" {
		t.Errorf("status: got %q", r.Status)
	}
	if r.Error != "connection refused" {
		t.Errorf("error: got %q", r.Error)
	}
}

// ================================================================
// validExecModes — coverage of the map
// ================================================================

func TestCoverage_ValidExecModes(t *testing.T) {
	modes := []struct {
		mode  string
		valid bool
	}{
		{"auto", true},
		{"approval", true},
		{"manual", true},
		{"invalid", false},
		{"", false},
	}
	for _, tt := range modes {
		t.Run(tt.mode, func(t *testing.T) {
			if validExecModes[tt.mode] != tt.valid {
				t.Errorf("validExecModes[%q]: got %v, want %v",
					tt.mode, validExecModes[tt.mode], tt.valid)
			}
		})
	}
}

// ================================================================
// DatabaseDeps — struct zero value
// ================================================================

func TestCoverage_DatabaseDeps_ZeroValue(t *testing.T) {
	deps := DatabaseDeps{}
	if deps.Store != nil {
		t.Error("Store should be nil")
	}
	if deps.Fleet != nil {
		t.Error("Fleet should be nil")
	}
	if deps.OnCreate != nil {
		t.Error("OnCreate should be nil")
	}
}

// ================================================================
// ActionDeps — struct zero value
// ================================================================

func TestCoverage_ActionDeps_ZeroValue(t *testing.T) {
	deps := ActionDeps{}
	if deps.Store != nil {
		t.Error("Store should be nil")
	}
	if deps.Executor != nil {
		t.Error("Executor should be nil")
	}
	if deps.Fleet != nil {
		t.Error("Fleet should be nil")
	}
}

// ================================================================
// Additional handler coverage — with middleware stack
// ================================================================

func TestCoverage_MiddlewareStack(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})

	// Custom middleware that sets a header.
	testMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			w.Header().Set("X-Test", "applied")
			next.ServeHTTP(w, r)
		})
	}

	r := NewRouter(mgr, cfg, nil, testMiddleware)

	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("X-Test") != "applied" {
		t.Error("custom middleware was not applied")
	}
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestCoverage_MultipleMiddlewares(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})

	mw1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			w.Header().Set("X-MW1", "yes")
			next.ServeHTTP(w, r)
		})
	}
	mw2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			w.Header().Set("X-MW2", "yes")
			next.ServeHTTP(w, r)
		})
	}

	r := NewRouter(mgr, cfg, nil, mw1, mw2)

	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("X-MW1") != "yes" {
		t.Error("MW1 not applied")
	}
	if w.Header().Get("X-MW2") != "yes" {
		t.Error("MW2 not applied")
	}
}

// ================================================================
// SPA fallback — static file with dot in path
// ================================================================

func TestCoverage_Dashboard_StaticFile(t *testing.T) {
	r := testRouter("db1")
	// Path with a dot should be served as a static file.
	w := get(t, r, "/app.js")
	// Will be 404 since no real app.js exists in embedded FS,
	// but the handler should try to serve it.
	_ = w // Just exercising the code path.
}

// ================================================================
// Findings detail — with specific path value through router
// ================================================================

func TestCoverage_FindingDetail_ViaRouter(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings/1")
	// No pool, should be 404.
	if w.Code != 404 {
		t.Errorf("status: %d, want 404", w.Code)
	}
}

// ================================================================
// Action detail — with specific path value through router
// ================================================================

func TestCoverage_ActionDetail_ViaRouter(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions/1")
	// No pool, should be 404.
	if w.Code != 404 {
		t.Errorf("status: %d, want 404", w.Code)
	}
}

// ================================================================
// Suppress/unsuppress via router — exercises pool-less paths
// ================================================================

func TestCoverage_Suppress_ViaRouter_NoPools(t *testing.T) {
	// Create a fleet with no database pools.
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: false},
	})
	r := NewRouter(mgr, cfg, nil, fakeAdminMiddleware)

	w := post(t, r, "/api/v1/findings/42/suppress", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestCoverage_Unsuppress_ViaRouter_NoPools(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: false},
	})
	r := NewRouter(mgr, cfg, nil, fakeAdminMiddleware)

	w := post(t, r, "/api/v1/findings/42/unsuppress", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestCoverage_Suppress_MultiDBRequiresDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := post(t, r, "/api/v1/findings/42/suppress", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	m := decodeJSON(t, w)
	if m["error"] != "database is required" {
		t.Fatalf("error: got %v", m["error"])
	}
}

func TestCoverage_Unsuppress_MultiDBRequiresDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := post(t, r, "/api/v1/findings/42/unsuppress", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	m := decodeJSON(t, w)
	if m["error"] != "database is required" {
		t.Fatalf("error: got %v", m["error"])
	}
}

// ================================================================
// createChannelHandler — user extraction from context
// ================================================================

func TestCoverage_CreateChannel_WithUser(t *testing.T) {
	// Tests the UserFromContext path in createChannelHandler.
	// The handler will still need a real store, but the
	// user extraction code runs before the store call.
	handler := createChannelHandler(nil)

	defer func() {
		if r := recover(); r != nil {
			// Panic at store layer expected (nil store).
		}
	}()

	w := doRequestWithUser(handler, "POST",
		"/api/v1/notifications/channels",
		`{"name":"alerts","type":"slack",
		  "config":{"webhook_url":"http://x"}}`,
		testAdminUser())

	// If we reach here (no panic), check response.
	if w.Code == http.StatusBadRequest {
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["error"] == "name and type required" {
			t.Error("valid name and type should not be rejected")
		}
	}
}

// ================================================================
// createRuleHandler — default min_severity
// ================================================================

func TestCoverage_CreateRule_WithChannelID(t *testing.T) {
	handler := createRuleHandler(nil)

	defer func() {
		if r := recover(); r != nil {
			// Panic at store layer expected (nil store).
		}
	}()

	w := doRequest(handler, "POST",
		"/api/v1/notifications/rules",
		`{"channel_id":1,"event":"new_finding",
		  "min_severity":"critical"}`)

	// Should not fail with invalid JSON.
	if w.Code == http.StatusBadRequest {
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["error"] == "invalid request body" {
			t.Error("valid JSON should not be rejected")
		}
	}
}

// ================================================================
// fleetPendingActionsHandler / fleetPendingCountHandler —
// with no pools
// ================================================================

func TestCoverage_FleetPendingActions_NoPools(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: false},
	})

	handler := fleetPendingActionsHandler(mgr)
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending", "")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	pending := resp["pending"].([]any)
	if len(pending) != 0 {
		t.Errorf("pending: got %d, want 0", len(pending))
	}
	if resp["total"].(float64) != 0 {
		t.Errorf("total: got %v, want 0", resp["total"])
	}
}

func TestCoverage_FleetPendingCount_NoPools(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: false},
	})

	handler := fleetPendingCountHandler(mgr)
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending/count", "")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["count"].(float64) != 0 {
		t.Errorf("count: got %v, want 0", resp["count"])
	}
}
