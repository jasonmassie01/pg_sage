package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestLakebaseConnectionTestsLive exercises the dashboard's real preview and
// saved-connection test paths. These read version/extension catalogs only.
func TestLakebaseConnectionTestsLive(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE=1 and SAGE_LAKEBASE_LIVE_DATABASE_URL")
	}
	dsn := os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || dsn == "" || cfg.Password != "" {
		t.Fatal("live dashboard test requires a valid passwordless Lakebase DSN")
	}
	ssl := "require"
	if cfg.TLSConfig != nil && !cfg.TLSConfig.InsecureSkipVerify {
		ssl = "verify-full"
	}
	req := dbCreateRequest{Host: cfg.Host, Port: int(cfg.Port), DatabaseName: cfg.Database,
		Username: cfg.User, SSLMode: ssl}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	testConnectionPreviewHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/api/v1/databases/test", bytes.NewReader(body)))
	var result ConnectionTestResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Status != "ok" {
		t.Fatalf("native dashboard preview failed: status=%d result=%+v decode=%v", w.Code, result, err)
	}
	if result := testFromConnString(t.Context(), dsn); result.Status != "ok" {
		t.Fatalf("native saved-connection test failed: %+v", result)
	}
	t.Log("native OAuth dashboard preview and saved-connection tests passed")
}
