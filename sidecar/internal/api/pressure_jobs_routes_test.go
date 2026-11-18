package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// guard-01b: /api/v1/databases/{db}/pressure-jobs registers, lists and
// ends backfill jobs. An operator session or an operator token with the
// propose scope registers; a viewer or a read token lists. The store's SQL
// is tested in internal/pressure (jobs_db_test.go). No concurrent access
// tests here: the handlers keep no state; the store serializes.

type fakeJobRegistry struct {
	mu    sync.Mutex
	specs []pressure.JobSpec
	jobs  []pressure.Job
	err   error
}

func (f *fakeJobRegistry) Register(_ context.Context, s pressure.JobSpec) (pressure.Job,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := s.Validate(); err != nil {
		return pressure.Job{}, err
	}
	if f.err != nil {
		return pressure.Job{}, f.err
	}
	f.specs = append(f.specs, s)
	now := time.Now()
	j := pressure.Job{ID: int64(len(f.specs)), Database: s.Database,
		ApplicationName: s.ApplicationName, Table: s.Table, RegisteredBy: s.RegisteredBy,
		Enforce: s.Enforce, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	f.jobs = append(f.jobs, j)
	return j, nil
}

func (f *fakeJobRegistry) Active(_ context.Context, database string) ([]pressure.Job,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pressure.Job
	for _, j := range f.jobs {
		if j.Database == database {
			out = append(out, j)
		}
	}
	return out, f.err
}

func (f *fakeJobRegistry) End(_ context.Context, database string, id int64) (pressure.Job,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, j := range f.jobs {
		if j.ID == id && j.Database == database {
			f.jobs[i].ExpiresAt = time.Now()
			return f.jobs[i], nil
		}
	}
	return pressure.Job{}, pressure.ErrJobNotFound
}

func jobsFleet(t *testing.T, reg pressure.JobRegistry) *fleet.DatabaseManager {
	t.Helper()
	mgr := pressureFleet(t, nil)
	for _, name := range []string{"orders", "billing"} {
		mgr.GetInstance(name).PressureJobs = reg
	}
	return mgr
}

func jobsMux(mgr *fleet.DatabaseManager, tokens lintTokenValidator) *http.ServeMux {
	mux := http.NewServeMux()
	registerPressureJobRoutes(mux, mgr, tokens, false)
	return mux
}

func callJobs(t *testing.T, h http.Handler, method, path, body, authz string,
	user *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if user != nil {
		req = withUser(req, user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const jobsPath = "/api/v1/databases/orders/pressure-jobs"

func TestPressureJobRoutes_OperatorRegisters(t *testing.T) {
	reg := &fakeJobRegistry{}
	h := jobsMux(jobsFleet(t, reg), nil)
	rec := callJobs(t, h, http.MethodPost, jobsPath, `{"application_name":"backfill_orders",`+
		`"table":"public.orders","enforce":true,"ttl_seconds":7200}`, "", testOperatorUser())
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
	var j pressure.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	s := reg.specs[0]
	if j.ID != 1 || j.ApplicationName != "backfill_orders" || !j.Enforce ||
		s.Database != "orders" || s.Table != "public.orders" || s.TTL != 2*time.Hour ||
		s.RegisteredBy != fmt.Sprintf("user:%d", testOperatorUser().ID) {
		t.Fatalf("job %+v from spec %+v", j, s)
	}
	list := callJobs(t, h, http.MethodGet, jobsPath, "", "", testViewerUser())
	var body struct {
		Jobs []pressure.Job `json:"jobs"`
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &body) != nil ||
		len(body.Jobs) != 1 || body.Jobs[0].ID != 1 {
		t.Fatalf("list = %d %s", list.Code, list.Body)
	}
	empty := callJobs(t, h, http.MethodGet, "/api/v1/databases/billing/pressure-jobs", "",
		"", testViewerUser())
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"jobs":[]`) {
		t.Fatalf("empty list = %d %s, want an empty array", empty.Code, empty.Body)
	}
}

func TestPressureJobRoutes_RefusesBadInput(t *testing.T) {
	reg := &fakeJobRegistry{}
	h := jobsMux(jobsFleet(t, reg), nil)
	cases := map[string]struct {
		path, body string
		code       int
	}{
		"no name":         {jobsPath, `{"table":"t"}`, http.StatusBadRequest},
		"unknown field":   {jobsPath, `{"application_name":"a","pid":1}`, http.StatusBadRequest},
		"not json":        {jobsPath, `application_name=a`, http.StatusBadRequest},
		"negative ttl":    {jobsPath, `{"application_name":"a","ttl_seconds":-1}`, 400},
		"pg_sage name":    {jobsPath, `{"application_name":"pg_sage_x"}`, http.StatusBadRequest},
		"trailing data":   {jobsPath, `{"application_name":"a"}{}`, http.StatusBadRequest},
		"unknown db":      {"/api/v1/databases/nope/pressure-jobs", `{"application_name":"a"}`, 404},
		"end bad id":      {jobsPath + "/x/end", ``, http.StatusBadRequest},
		"end unknown job": {jobsPath + "/77/end", ``, http.StatusNotFound},
	}
	for name, c := range cases {
		rec := callJobs(t, h, http.MethodPost, c.path, c.body, "", testOperatorUser())
		if rec.Code != c.code {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, c.code)
		}
	}
	if len(reg.specs) != 0 {
		t.Fatalf("refused requests registered %+v", reg.specs)
	}
}

func TestPressureJobRoutes_ViewerCannotRegisterOrEnd(t *testing.T) {
	reg := &fakeJobRegistry{}
	h := jobsMux(jobsFleet(t, reg), nil)
	for _, path := range []string{jobsPath, jobsPath + "/1/end"} {
		rec := callJobs(t, h, http.MethodPost, path, `{"application_name":"a"}`, "",
			testViewerUser())
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d, want 403", path, rec.Code)
		}
	}
	if rec := callJobs(t, h, http.MethodPost, jobsPath, `{"application_name":"a"}`, "",
		nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", rec.Code)
	}
}

func TestPressureJobRoutes_EndAndStoreErrors(t *testing.T) {
	reg := &fakeJobRegistry{}
	h := jobsMux(jobsFleet(t, reg), nil)
	callJobs(t, h, http.MethodPost, jobsPath, `{"application_name":"a"}`, "",
		testOperatorUser())
	other := callJobs(t, h, http.MethodPost, "/api/v1/databases/billing/pressure-jobs/1/end",
		"", "", testOperatorUser())
	if other.Code != http.StatusNotFound {
		t.Fatalf("end from another database = %d, want 404", other.Code)
	}
	if rec := callJobs(t, h, http.MethodPost, jobsPath+"/1/end", "", "",
		testOperatorUser()); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"application_name":"a"`) {
		t.Fatalf("end = %d %s", rec.Code, rec.Body)
	}
	reg.err = errors.New("relation sage.pressure_jobs does not exist")
	rec := callJobs(t, h, http.MethodPost, jobsPath, `{"application_name":"b"}`, "",
		testOperatorUser())
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(),
		"relation") {
		t.Fatalf("store failure = %d %s, want 503 without the SQL error", rec.Code, rec.Body)
	}
	unwired := jobsMux(pressureFleet(t, nil), nil)
	if rec := callJobs(t, unwired, http.MethodGet, jobsPath, "", "",
		testViewerUser()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no job store = %d, want 503", rec.Code)
	}
}

func TestPressureJobRoutes_TokenAuth(t *testing.T) {
	reg := &fakeJobRegistry{}
	propose := readGrant("p1")
	propose.Scopes = []string{mcptoken.ScopeRead, mcptoken.ScopePropose}
	agent := propose
	agent.TokenID, agent.Kind = "a1", mcptoken.KindAgent
	elsewhere := propose
	elsewhere.TokenID, elsewhere.Databases = "p2", []string{"billing"}
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): propose,
		lintSecret(2): readGrant("r1"), lintSecret(3): agent, lintSecret(4): elsewhere})
	h := jobsMux(jobsFleet(t, reg), tokens)
	body := `{"application_name":"ci_backfill"}`
	cases := []struct {
		name, method, authz string
		code                int
	}{
		{"propose token registers", http.MethodPost, "Bearer " + lintSecret(1), 201},
		{"read token cannot register", http.MethodPost, "Bearer " + lintSecret(2), 403},
		{"agent token cannot register", http.MethodPost, "Bearer " + lintSecret(3), 403},
		{"token for another database", http.MethodPost, "Bearer " + lintSecret(4), 404},
		{"unknown token", http.MethodPost, "Bearer " + lintSecret(9), 401},
		{"read token lists", http.MethodGet, "Bearer " + lintSecret(2), 200},
	}
	for _, c := range cases {
		if rec := callJobs(t, h, c.method, jobsPath, body, c.authz, nil); rec.Code != c.code {
			t.Errorf("%s = %d %s, want %d", c.name, rec.Code, rec.Body, c.code)
		}
	}
	if len(reg.specs) != 1 || reg.specs[0].RegisteredBy != "token:p1" {
		t.Fatalf("token registrations = %+v, want one by token:p1", reg.specs)
	}
	for name, req := range map[string]*http.Request{
		"register": bearerProbe(http.MethodPost, jobsPath),
		"list":     bearerProbe(http.MethodGet, jobsPath),
		"end":      bearerProbe(http.MethodPost, jobsPath+"/3/end"),
	} {
		if !skipsSessionAuth(req) {
			t.Errorf("a Bearer %s must skip session auth", name)
		}
	}
	for name, req := range map[string]*http.Request{
		"deeper":    bearerProbe(http.MethodPost, jobsPath+"/3/end/x"),
		"traversal": bearerProbe(http.MethodPost, "/api/v1/databases/x/../pressure-jobs"),
		"delete":    bearerProbe(http.MethodDelete, jobsPath),
	} {
		if skipsSessionAuth(req) {
			t.Errorf("%s: skipped session auth", name)
		}
	}
}
