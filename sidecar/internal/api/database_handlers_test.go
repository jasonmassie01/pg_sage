package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// testManagedRouter creates a router with DatabaseDeps set to nil
// (no real DB). We test route registration and request parsing.
// Full integration tests require a live database.
func testManagedRouter() http.Handler {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	// dbDeps is nil: handlers that need the store will panic,
	// but route registration and middleware still work.
	return NewRouterFull(mgr, cfg, nil, nil, nil, nil)
}

func TestDatabaseManagedRoutes_NoStore(t *testing.T) {
	// Without DatabaseDeps, the /managed routes should 404.
	r := testManagedRouter()
	w := get(t, r, "/api/v1/databases/managed")
	if w.Code != 404 {
		t.Errorf("expected 404 without store, got %d", w.Code)
	}
}

func TestDBRecordToMap(t *testing.T) {
	rec := dummyRecord()
	m := dbRecordToMap(rec)
	if m["name"] != "test-db" {
		t.Errorf("name: %v", m["name"])
	}
	if m["host"] != "localhost" {
		t.Errorf("host: %v", m["host"])
	}
	if m["port"] != 5432 {
		t.Errorf("port: %v", m["port"])
	}
	if m["max_connections"] != 5 {
		t.Errorf("max_connections: %v", m["max_connections"])
	}
	// Password must never appear in the map.
	if _, ok := m["password"]; ok {
		t.Error("password should not be in map")
	}
}

func TestDBCreateRequest_ToInput(t *testing.T) {
	req := dbCreateRequest{
		Name:           "prod",
		Host:           "db.example.com",
		Port:           5432,
		DatabaseName:   "orders",
		Username:       "sage_agent",
		Password:       "secret",
		SSLMode:        "require",
		MaxConnections: 7,
		TrustLevel:     "observation",
		ExecutionMode:  "approval",
	}
	input := req.toInput()
	if input.Name != "prod" {
		t.Errorf("name: %v", input.Name)
	}
	if input.Password != "secret" {
		t.Errorf("password not copied")
	}
	if input.TrustLevel != "observation" {
		t.Errorf("trust_level: %v", input.TrustLevel)
	}
	if input.MaxConnections != 7 {
		t.Errorf("max_connections: %v", input.MaxConnections)
	}
}

func TestDBCreateRequest_ToInputDefaultsMaxConnections(t *testing.T) {
	req := dbCreateRequest{
		Name:          "prod",
		Host:          "db.example.com",
		Port:          5432,
		DatabaseName:  "orders",
		Username:      "sage_agent",
		Password:      "secret",
		SSLMode:       "require",
		TrustLevel:    "observation",
		ExecutionMode: "approval",
	}
	input := req.toInput()
	if input.MaxConnections != 2 {
		t.Errorf("max_connections: got %d, want 2",
			input.MaxConnections)
	}
}

func TestBuildManagedTestConnString_UsesUnsavedFields(t *testing.T) {
	req := dbCreateRequest{
		Host:         "unsaved.example.com",
		Port:         15432,
		DatabaseName: "unsaved_db",
		Username:     "unsaved_user",
		Password:     "new-secret",
		SSLMode:      "disable",
	}

	connStr := buildManagedTestConnString(req, "old-secret")

	if !strings.Contains(connStr, "unsaved.example.com:15432") {
		t.Fatalf("connStr did not use unsaved host/port: %s", connStr)
	}
	if !strings.Contains(connStr, "unsaved_db") {
		t.Fatalf("connStr did not use unsaved database: %s", connStr)
	}
	if !strings.Contains(connStr, "unsaved_user:new-secret") {
		t.Fatalf("connStr did not use unsaved credentials: %s", connStr)
	}
	if !strings.Contains(connStr, "sslmode=disable") {
		t.Fatalf("connStr did not use unsaved sslmode: %s", connStr)
	}
}

func TestBuildManagedTestConnString_FallsBackToStoredPassword(t *testing.T) {
	req := dbCreateRequest{
		Host:         "edit.example.com",
		DatabaseName: "edit_db",
		Username:     "edit_user",
		SSLMode:      "require",
	}

	connStr := buildManagedTestConnString(req, "stored-secret")

	if !strings.Contains(connStr, "edit_user:stored-secret") {
		t.Fatalf("connStr did not use stored password: %s", connStr)
	}
	if !strings.Contains(connStr, "edit.example.com:5432") {
		t.Fatalf("connStr did not default port: %s", connStr)
	}
}

func TestFleetManagedDBConnection_UsesRuntimeFleetConfig(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "fleet-db",
		DatabaseID: 42,
		Config: config.DatabaseConfig{
			Name:     "fleet-db",
			Host:     "127.0.0.1",
			Port:     15432,
			User:     "postgres",
			Password: "runtime-secret",
			Database: "app",
			SSLMode:  "disable",
		},
	})

	connStr, password, ok := fleetManagedDBConnection(
		&DatabaseDeps{Fleet: mgr}, 42, "")

	if !ok {
		t.Fatal("expected fleet config to be found by database id")
	}
	if password != "runtime-secret" {
		t.Fatalf("password = %q, want runtime-secret", password)
	}
	if !strings.Contains(connStr, "runtime-secret@127.0.0.1:15432/app") {
		t.Fatalf("connStr did not use runtime fleet config: %s", connStr)
	}
}

func TestFleetManagedDBConnection_FallsBackToName(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "named-db",
		Config: config.DatabaseConfig{
			Name:     "named-db",
			Host:     "db.example.com",
			Port:     5432,
			User:     "sage",
			Password: "named-secret",
			Database: "named",
			SSLMode:  "require",
		},
	})

	_, password, ok := fleetManagedDBConnection(
		&DatabaseDeps{Fleet: mgr}, 999, "named-db")

	if !ok {
		t.Fatal("expected fleet config to be found by name")
	}
	if password != "named-secret" {
		t.Fatalf("password = %q, want named-secret", password)
	}
}

func TestDBCreateRequest_InvalidJSON(t *testing.T) {
	// Test that the handler returns 400 on bad JSON.
	// We need a mock handler that doesn't need a real store.
	handler := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		var req dbCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{"ok": "true"})
	})

	req := httptest.NewRequest("POST",
		"/api/v1/databases/managed",
		strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestValidCSVHeader_Valid(t *testing.T) {
	header := []string{
		"name", "host", "port", "database_name",
		"username", "password", "sslmode",
	}
	if !validCSVHeader(header) {
		t.Error("expected valid header")
	}
}

func TestValidCSVHeader_Invalid(t *testing.T) {
	header := []string{"wrong", "columns"}
	if validCSVHeader(header) {
		t.Error("expected invalid header")
	}
}

func TestValidCSVHeader_ExtraColumns(t *testing.T) {
	header := []string{
		"name", "host", "port", "database_name",
		"username", "password", "sslmode", "extra",
	}
	if !validCSVHeader(header) {
		t.Error("extra columns should be accepted")
	}
}

func TestCSVImport_BadHeader(t *testing.T) {
	csv := "wrong,header\nval1,val2\n"
	result := processCSVImport(
		nil, nil, strings.NewReader(csv), 0)
	if len(result.Errors) == 0 {
		t.Error("expected error for bad header")
	}
	if result.Imported != 0 {
		t.Errorf("imported: %d", result.Imported)
	}
}

func TestCSVImport_EmptyFile(t *testing.T) {
	result := processCSVImport(
		nil, nil, strings.NewReader(""), 0)
	if len(result.Errors) == 0 {
		t.Error("expected error for empty file")
	}
}

func TestCSVImport_MultipartParsing(t *testing.T) {
	// Test multipart form file upload parsing.
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "dbs.csv")
	if err != nil {
		t.Fatal(err)
	}
	csvData := "name,host,port,database_name," +
		"username,password,sslmode\n" +
		"db1,localhost,5432,mydb,user,pass,require\n"
	part.Write([]byte(csvData))
	writer.Close()

	req := httptest.NewRequest("POST",
		"/api/v1/databases/managed/import", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Just verify the multipart can be parsed.
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	file, _, err := req.FormFile("file")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	file.Close()
}

func TestConnectionTestResult_JSON(t *testing.T) {
	r := ConnectionTestResult{
		Status:     "ok",
		PGVersion:  "PostgreSQL 17.0",
		Extensions: []string{"pg_stat_statements"},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	json.Unmarshal(data, &decoded)
	if decoded["status"] != "ok" {
		t.Errorf("status: %v", decoded["status"])
	}
	exts := decoded["extensions"].([]any)
	if len(exts) != 1 || exts[0] != "pg_stat_statements" {
		t.Errorf("extensions: %v", exts)
	}
}

func TestConnectionTestResult_ErrorJSON(t *testing.T) {
	r := ConnectionTestResult{
		Status: "error",
		Error:  "connection refused",
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	json.Unmarshal(data, &decoded)
	if decoded["status"] != "error" {
		t.Errorf("status: %v", decoded["status"])
	}
	if decoded["error"] != "connection refused" {
		t.Errorf("error: %v", decoded["error"])
	}
	// pg_version should be omitted.
	if decoded["pg_version"] != nil && decoded["pg_version"] != "" {
		t.Errorf("pg_version should be omitted: %v",
			decoded["pg_version"])
	}
}

// --- helpers ---

func dummyRecord() store.DatabaseRecord {
	return store.DatabaseRecord{
		ID:             1,
		Name:           "test-db",
		Host:           "localhost",
		Port:           5432,
		DatabaseName:   "myapp",
		Username:       "admin",
		SSLMode:        "require",
		MaxConnections: 5,
		Enabled:        true,
		Tags:           map[string]string{"env": "test"},
		TrustLevel:     "observation",
		ExecutionMode:  "approval",
	}
}
