package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestWave2GlobalConfigHandlersRoundTripRevision(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetWave2GlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	controller := config.NewConfigController(base, nil, apiTrustOwner{})

	put := configGlobalPutHandler(cs, base, nil, controller)
	putResponse := doRequestWithUser(
		put, http.MethodPut, "/api/v1/config/global",
		`{"trust.level":"advisory","expected_generation":1}`,
		user,
	)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200; body=%s",
			putResponse.Code, putResponse.Body.String())
	}
	var putBody config.ApplyResult
	if err := json.NewDecoder(putResponse.Body).Decode(&putBody); err != nil {
		t.Fatalf("decode put response: %v", err)
	}
	if putBody.DesiredGeneration != 2 || putBody.ActiveGeneration != 2 {
		t.Fatalf("put generations = %+v, want desired=2 active=2", putBody)
	}

	getResponse := httptest.NewRecorder()
	configGlobalGetHandler(cs, base, controller).ServeHTTP(
		getResponse, httptest.NewRequest(http.MethodGet, "/api/v1/config/global", nil),
	)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s",
			getResponse.Code, getResponse.Body.String())
	}
	var getBody map[string]any
	if err := json.NewDecoder(getResponse.Body).Decode(&getBody); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	configBody := getBody["config"].(map[string]any)
	trustBody := configBody["trust.level"].(map[string]any)
	if trustBody["value"] != "advisory" || trustBody["source"] != "override" {
		t.Fatalf("global trust response = %+v, want advisory override", trustBody)
	}

	deleteHandler := configGlobalDeleteHandler(
		cs, base, base, nil, controller,
	)
	deleteMux := http.NewServeMux()
	deleteMux.HandleFunc("DELETE /api/v1/config/global/{key}", deleteHandler)
	deleteResponse := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/config/global/trust.level?expected_generation=2", nil,
	)
	deleteMux.ServeHTTP(deleteResponse, withUser(deleteRequest, user))
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s",
			deleteResponse.Code, deleteResponse.Body.String())
	}
	if active := controller.Active(); active.Generation != 3 ||
		active.Config.Trust.Level != config.DefaultTrustLevel {
		t.Fatalf("active after reset = %+v, want generation 3 default trust", active)
	}

	auditResponse := httptest.NewRecorder()
	configAuditHandler(cs).ServeHTTP(
		auditResponse, httptest.NewRequest(http.MethodGet, "/api/v1/config/audit", nil),
	)
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit status = %d, want 200; body=%s",
			auditResponse.Code, auditResponse.Body.String())
	}
}

func TestWave2DatabaseConfigHandlersApplyPolicyCASAndLegacyReset(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	const databaseID = 9910
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	resetWave2DatabaseConfig(t, ctx, pool, databaseID)
	_, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc,
		 sslmode, trust_level, execution_mode)
		VALUES ($1, 'wave2-api-policy', 'localhost', 5432, 'postgres',
		 'postgres', '\x00', 'disable', 'observation', 'approval')`, databaseID)
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	cs := store.NewConfigStore(pool)
	base := config.DefaultConfig()
	mgr := fleet.NewManager(base)
	inst := &fleet.DatabaseInstance{
		Name: "wave2-api-policy", DatabaseID: databaseID,
		Config: config.DatabaseConfig{
			Name: "wave2-api-policy", TrustLevel: "observation",
			TrustLevelExplicit: true,
		},
		Status: &fleet.InstanceStatus{TrustLevel: "observation"},
	}
	mgr.RegisterInstance(inst)

	putMux := http.NewServeMux()
	putMux.HandleFunc(
		"PUT /api/v1/config/databases/{id}",
		configDBPutHandler(cs, base, pool, mgr),
	)
	putResponse := httptest.NewRecorder()
	putRequest := httptest.NewRequest(
		http.MethodPut, "/api/v1/config/databases/9910",
		strings.NewReader(
			`{"expected_generation":1,"trust.level":"advisory","execution_mode":"manual"}`,
		),
	)
	putMux.ServeHTTP(putResponse, withUser(putRequest, user))
	if putResponse.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200; body=%s",
			putResponse.Code, putResponse.Body.String())
	}
	var policyTrust, policyMode string
	if err := pool.QueryRow(ctx, `SELECT trust_level, execution_mode
		FROM sage.databases WHERE id = $1`, databaseID).Scan(
		&policyTrust, &policyMode,
	); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if policyTrust != "advisory" || policyMode != "manual" {
		t.Fatalf("policy = (%q, %q), want (advisory, manual)",
			policyTrust, policyMode)
	}
	if status := inst.SnapshotStatus(); status.TrustLevel != "advisory" {
		t.Fatalf("runtime trust = %q, want advisory", status.TrustLevel)
	}

	staleResponse := httptest.NewRecorder()
	staleRequest := httptest.NewRequest(
		http.MethodPut, "/api/v1/config/databases/9910",
		strings.NewReader(`{"expected_generation":1,"execution_mode":"auto"}`),
	)
	putMux.ServeHTTP(staleResponse, withUser(staleRequest, user))
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale status = %d, want 409; body=%s",
			staleResponse.Code, staleResponse.Body.String())
	}

	getMux := http.NewServeMux()
	getMux.HandleFunc(
		"GET /api/v1/config/databases/{id}",
		configDBGetHandler(cs, base, pool),
	)
	getResponse := httptest.NewRecorder()
	getMux.ServeHTTP(getResponse, httptest.NewRequest(
		http.MethodGet, "/api/v1/config/databases/9910", nil,
	))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s",
			getResponse.Code, getResponse.Body.String())
	}
	var getBody map[string]any
	if err := json.NewDecoder(getResponse.Body).Decode(&getBody); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if getBody["desired_generation"] != float64(2) {
		t.Fatalf("get generation = %v, want 2", getBody["desired_generation"])
	}

	if err := cs.SetOverride(
		ctx, "safety.query_timeout_ms", "4000", databaseID, user.ID,
	); err != nil {
		t.Fatalf("seed legacy override: %v", err)
	}
	deleteMux := http.NewServeMux()
	deleteMux.HandleFunc(
		"DELETE /api/v1/config/databases/{id}/{key}",
		configDBDeleteHandler(cs, base, mgr),
	)
	deleteResponse := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/config/databases/9910/safety.query_timeout_ms?expected_generation=2",
		nil,
	)
	deleteMux.ServeHTTP(deleteResponse, withUser(deleteRequest, user))
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s",
			deleteResponse.Code, deleteResponse.Body.String())
	}
	if generation, err := cs.GetGeneration(ctx, databaseID); err != nil ||
		generation != 3 {
		t.Fatalf("generation after legacy reset = %d, %v; want 3", generation, err)
	}
}

func resetWave2GlobalConfig(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	reset := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit
			WHERE database_id IS NULL AND key = 'trust.level'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key IN
			('trust.level', '__pg_sage_config_generation')`)
	}
	reset()
	t.Cleanup(reset)
}

func resetWave2DatabaseConfig(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, databaseID int,
) {
	t.Helper()
	reset := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.config_audit WHERE database_id = $1", databaseID)
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.config WHERE database_id = $1", databaseID)
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.databases WHERE id = $1", databaseID)
	}
	reset()
	t.Cleanup(reset)
}
