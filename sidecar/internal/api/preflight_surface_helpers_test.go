package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/value"
)

type surfaceFixture struct {
	pool   *pgxpool.Pool
	server *httptest.Server
	client *http.Client
}

func surfacePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg == nil {
		t.Fatalf("disposable database required: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if err := agentdb.NewStore(pool).Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func surfaceRouter(t *testing.T, pool *pgxpool.Pool, cfg *config.Config,
	runtime *RuntimeDeps) *surfaceFixture {
	t.Helper()
	// No cloud credentials or live runners are needed or permitted in this suite.
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	handler := NewRouterFullRuntime(nil, cfg, pool, nil, nil, nil, runtime,
		SessionAuthMiddleware(pool))
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &surfaceFixture{pool: pool, server: server, client: client}
}

func (f *surfaceFixture) request(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func (f *surfaceFixture) login(t *testing.T, role string) string {
	t.Helper()
	email := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + "@fixture.invalid"
	const password = "local-disposable-fixture-only"
	if _, err := auth.CreateUser(context.Background(), f.pool, email, password, role); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"email": email, "password": password})
	status, body := f.request(t, "POST", "/api/v1/auth/login", string(data))
	if status != 200 {
		t.Fatalf("real login: status=%d body=%s", status, body)
	}
	status, body = f.request(t, "GET", "/api/v1/auth/me", "")
	if status != 200 || !strings.Contains(body, `"role":"`+role+`"`) {
		t.Fatalf("real session identity: status=%d body=%s", status, body)
	}
	return email
}

func surfaceMCP(t *testing.T, pool *pgxpool.Pool, stopped bool) *RuntimeDeps {
	t.Helper()
	st := policy.NewStore(pool)
	if _, err := st.Bootstrap(context.Background(), policy.Scope{}, "unattended", "fixture"); err != nil {
		t.Fatal(err)
	}
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true, EmergencyStop: stopped,
				TrustLevel: policy.TrustAutonomous, ExecutionMode: policy.ExecutionAuto}, nil
		},
		Policy: func(ctx context.Context, req policy.ActionRequest) (policy.Document, error) {
			stored, err := st.Current(ctx, policy.Scope{DatabaseID: req.DatabaseID})
			if err != nil {
				return policy.Document{}, err
			}
			return policy.ParseDocument(stored.Document)
		},
	})
	access := mcp.NewPostgresAccess(pool)
	backend, err := mcp.NewProductionBackend(mcp.ProductionDependencies{
		Gate: gate, Planner: mcp.DeterministicIntentPlanner{},
		Executor: mcp.NewProductionIntentExecutor(access, plan.NewPlanner(), gate),
		Policy: access, Ledger: access, Guarantees: access,
		Value: mcp.NewValueAccess(value.NewFleetService(func() []value.Source {
			return []value.Source{{Name: "primary", Pool: pool}}
		})),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := mcp.NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"},
		mcp.NewServer(backend), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &RuntimeDeps{MCPHandler: runtime.HTTPHandler()}
}

func surfaceCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func surfaceRPC(tool, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		tool + `","arguments":` + args + `}}`
}
