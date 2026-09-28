package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
)

// D4: approvals are attributed to the signed-in user, and a cloud instance
// is only registered by consuming an approved, unused request.

const d4APITenant = "tenant_d4_api"

type d4Fixture struct {
	st   *agentdb.Store
	ctx  context.Context
	pool *pgxpool.Pool
	mux  *http.ServeMux
}

func newD4Fixture(t *testing.T) *d4Fixture {
	t.Helper()
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_requests WHERE tenant_id=$1", d4APITenant)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_deployments WHERE tenant_id=$1", d4APITenant)
	}
	clean()
	t.Cleanup(clean)
	mux := http.NewServeMux()
	registerAgentDBRoutesWithAuthority(mux, st, nil, nil)
	return &d4Fixture{st: st, ctx: ctx, pool: pool, mux: mux}
}

func (f *d4Fixture) do(
	user *auth.User, method, path, body string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

// createRequest posts a cloud instance request; budget > 0 is approved by
// request policy at creation, budget 0 waits for a human decision.
func (f *d4Fixture) createRequest(t *testing.T, id, agent string, budget float64) {
	t.Helper()
	body := fmt.Sprintf(`{"request_id":%q,"tenant_id":%q,"agent_id":%q,`+
		`"requested_isolation_type":"instance","provider":"aws_rds",`+
		`"database_name":"d4_api","budget_usd":%v}`, id, d4APITenant, agent, budget)
	rr := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs/requests", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("create request %s: %d %s", id, rr.Code, rr.Body.String())
	}
}

func (f *d4Fixture) decidedBy(t *testing.T, id string) string {
	t.Helper()
	var by string
	if err := f.pool.QueryRow(f.ctx, `SELECT decided_by FROM sage.agent_db_requests
		WHERE request_id=$1`, id).Scan(&by); err != nil {
		t.Fatalf("read decided_by: %v", err)
	}
	return by
}

func (f *d4Fixture) deploymentCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.agent_db_deployments
		WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	return n
}

func TestRequestApprovalRecordsSessionActor(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_approve", "agent_d4_api", 0)
	op := &auth.User{ID: 41, Email: "op@x.test", Role: auth.RoleOperator}
	rr := f.do(op, http.MethodPost,
		"/api/v1/agent-dbs/requests/req_d4_api_approve/approve", `{"reason":"ok"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	var got agentdb.Request
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DecidedBy != "op@x.test" || f.decidedBy(t, "req_d4_api_approve") != "op@x.test" {
		t.Fatalf("approver not recorded: body=%+v", got)
	}
}

func TestRequestDenialRecordsSessionActor(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_deny", "agent_d4_api", 0)
	lead := &auth.User{ID: 42, Email: "lead@x.test", Role: auth.RoleAdmin}
	rr := f.do(lead, http.MethodPost, "/api/v1/agent-dbs/requests/req_d4_api_deny/deny", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rr.Code, rr.Body.String())
	}
	if by := f.decidedBy(t, "req_d4_api_deny"); by != "lead@x.test" {
		t.Fatalf("decided_by = %q, want lead@x.test", by)
	}
}

func TestRequestApprovalIgnoresBodyApprovedBy(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_spoof", "agent_d4_api", 0)
	rr := f.do(testOperatorUser(), http.MethodPost,
		"/api/v1/agent-dbs/requests/req_d4_api_spoof/approve",
		`{"approved_by":"someone","decided_by":"someone","actor_id":"someone"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	if by := f.decidedBy(t, "req_d4_api_spoof"); by != testOperatorUser().Email {
		t.Fatalf("decided_by = %q, want the session user", by)
	}
}

func TestRequestApprovalWithoutUserRejected(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_anon", "agent_d4_api", 0)
	handler := agentDBSubrouterWithRegistry(f.st, agentdb.DefaultRunnerRegistry(), nil)
	for _, action := range []string{"approve", "deny"} {
		rr := doRequest(handler, http.MethodPost,
			"/api/v1/agent-dbs/requests/req_d4_api_anon/"+action, `{}`)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s without user: %d %s", action, rr.Code, rr.Body.String())
		}
	}
	if by := f.decidedBy(t, "req_d4_api_anon"); by != "" {
		t.Fatalf("anonymous decision recorded: %q", by)
	}
}

func TestRegisterCloudInstanceWithoutRequestRejected(t *testing.T) {
	f := newD4Fixture(t)
	rr := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs",
		`{"deployment_id":"dep_d4_api_direct","tenant_id":"`+d4APITenant+`",`+
			`"agent_id":"agent_d4_api","provider":"aws_rds",`+
			`"provisioning_level":"instance","budget_usd":50}`)
	if rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "approved request required") {
		t.Fatalf("direct cloud register: %d %s", rr.Code, rr.Body.String())
	}
	if n := f.deploymentCount(t, "deployment_id='dep_d4_api_direct'"); n != 0 {
		t.Fatalf("rejected register left %d rows", n)
	}
}

func TestRegisterLocalSchemaWithoutRequestStillAllowed(t *testing.T) {
	f := newD4Fixture(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS d4_api_local CASCADE`)
	})
	rr := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs",
		`{"deployment_id":"dep_d4_api_local","tenant_id":"`+d4APITenant+`",`+
			`"agent_id":"agent_d4_api","provider":"local_postgres",`+
			`"provisioning_level":"schema","schema_name":"d4_api_local"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("local schema register: %d %s", rr.Code, rr.Body.String())
	}
	if n := f.deploymentCount(t, "deployment_id='dep_d4_api_local'"); n != 1 {
		t.Fatalf("local schema rows = %d", n)
	}
}

func TestRegisterCloudInstanceConsumesApprovedRequest(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_register", "agent_d4_api", 25)
	body := `{"request_id":"req_d4_api_register","deployment_id":"dep_d4_api_reg",` +
		`"tenant_id":"` + d4APITenant + `","agent_id":"agent_d4_api",` +
		`"provider":"aws_rds","provisioning_level":"instance",` +
		`"secret_ref":"env:PG_SAGE_AGENTDB_D4_API_DSN","secret_ref_provider":"env"}`
	rr := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("register with request: %d %s", rr.Code, rr.Body.String())
	}
	var dep agentdb.Deployment
	if err := json.Unmarshal(rr.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Metadata["request_id"] != "req_d4_api_register" ||
		dep.SecretRef != "env:PG_SAGE_AGENTDB_D4_API_DSN" {
		t.Fatalf("deployment not linked: %+v", dep)
	}
	again := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs",
		strings.Replace(body, "dep_d4_api_reg", "dep_d4_api_reg_2", 1))
	if again.Code != http.StatusConflict {
		t.Fatalf("reuse of consumed request: %d %s", again.Code, again.Body.String())
	}
}

func TestRegisterCloudInstanceWithRequestForOtherTenantOrAgentRejected(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_owner", "agent_d4_api", 25)
	for _, owner := range []string{
		`"tenant_id":"tenant_d4_other","agent_id":"agent_d4_api"`,
		`"tenant_id":"` + d4APITenant + `","agent_id":"agent_d4_other"`,
	} {
		rr := f.do(testOperatorUser(), http.MethodPost, "/api/v1/agent-dbs",
			`{"request_id":"req_d4_api_owner","deployment_id":"dep_d4_api_owner",`+owner+
				`,"provider":"aws_rds","provisioning_level":"instance"}`)
		if rr.Code != http.StatusConflict {
			t.Fatalf("register for %s: %d %s", owner, rr.Code, rr.Body.String())
		}
	}
	if n := f.deploymentCount(t, "deployment_id='dep_d4_api_owner'"); n != 0 {
		t.Fatalf("mismatched owner produced %d deployments", n)
	}
}

func TestProvisionEndpointCarriesSizeProfileAndSecretRef(t *testing.T) {
	f := newD4Fixture(t)
	profile := seedLiveAPIProfile(t, f.ctx, f.pool, f.st, "dep_d4_api_fields")
	f.createRequest(t, "req_d4_api_fields", "agent_d4_api", 25)
	rr := f.do(testOperatorUser(), http.MethodPost,
		"/api/v1/agent-dbs/requests/req_d4_api_fields/provision",
		`{"deployment_id":"dep_d4_api_fields","size_profile_id":"`+profile+`",`+
			`"secret_ref":"env:PG_SAGE_AGENTDB_D4_FIELDS","secret_ref_provider":"env",`+
			`"lease_seconds":1800}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rr.Code, rr.Body.String())
	}
	dep, err := f.st.Get(f.ctx, "dep_d4_api_fields")
	if err != nil {
		t.Fatal(err)
	}
	if dep.SizeProfileID != profile || dep.SecretRef != "env:PG_SAGE_AGENTDB_D4_FIELDS" {
		t.Fatalf("fields dropped: profile=%q secret=%q", dep.SizeProfileID, dep.SecretRef)
	}
	req, err := f.st.GetRequest(f.ctx, "req_d4_api_fields")
	if err != nil {
		t.Fatal(err)
	}
	if req.ConsumedDeploymentID != "dep_d4_api_fields" ||
		req.ConsumedBy != testOperatorUser().Email {
		t.Fatalf("request not consumed by session user: %+v", req)
	}
}

func TestProvisionEndpointRejectsRequestForOtherAgent(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_agent", "agent_d4_api", 25)
	rr := f.do(testOperatorUser(), http.MethodPost,
		"/api/v1/agent-dbs/requests/req_d4_api_agent/provision",
		`{"deployment_id":"dep_d4_api_agent","agent_id":"agent_d4_other"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("provision for another agent: %d %s", rr.Code, rr.Body.String())
	}
}

func TestProvisionEndpointRequiresUser(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_prov_anon", "agent_d4_api", 25)
	handler := agentDBSubrouterWithRegistry(f.st, agentdb.DefaultRunnerRegistry(), nil)
	rr := doRequest(handler, http.MethodPost,
		"/api/v1/agent-dbs/requests/req_d4_api_prov_anon/provision", `{}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous provision: %d %s", rr.Code, rr.Body.String())
	}
}

func TestProvisionEndpointConcurrentExactlyOne(t *testing.T) {
	f := newD4Fixture(t)
	f.createRequest(t, "req_d4_api_race", "agent_d4_api", 25)
	const racers = 6
	codes := make(chan int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := f.do(testOperatorUser(), http.MethodPost,
				"/api/v1/agent-dbs/requests/req_d4_api_race/provision",
				fmt.Sprintf(`{"deployment_id":"dep_d4_api_race_%d"}`, i%2))
			codes <- rr.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	ok := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected racer status %d", code)
		}
	}
	if ok != 1 {
		t.Fatalf("successful provisions = %d, want 1", ok)
	}
	if n := f.deploymentCount(t, "metadata->>'request_id'=$1", "req_d4_api_race"); n != 1 {
		t.Fatalf("deployments from one approval = %d", n)
	}
}
