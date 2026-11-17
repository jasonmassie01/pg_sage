package clone_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// neonFake is a stateful stand-in for the Neon API v2 (lab-05b): one
// project with a production branch "main" (and a "dev" branch), whose
// branch creates start an endpoint and two operations that finish after
// pollsToFinish polls. The knobs inject the failures the provider must
// handle; set them before the provider is used.
type neonFake struct {
	project, key string
	srv          *httptest.Server

	mu        sync.Mutex
	n         int
	branches  map[string]fakeNeonBranch
	endpoints map[string]fakeNeonEndpoint
	ops       map[string]*fakeNeonOp
	requests  []string
	bodies    []neonCreateBody

	createStatus   int    // answer every create with this status
	failAction     string // the operation with this action ends failed
	opsNeverFinish bool
	pollsToFinish  int
	noURI          bool
	uriQuery       string // query of every connection URI; "": Neon's
	endpointHost   string // every new endpoint answers on this host
	deleteStatus   int    // answer every delete with this status
	// createThenStatus makes the branch, then answers the create with this
	// status (a gateway error after Neon committed it).
	createThenStatus int
}

type fakeNeonBranch struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id,omitempty"`
	Default   bool   `json:"default"`
	Protected bool   `json:"protected"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type fakeNeonEndpoint struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	BranchID string `json:"branch_id"`
	Type     string `json:"type"`
}

type fakeNeonOp struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Status   string `json:"status"`
	BranchID string `json:"branch_id"`
	polls    int
}

type neonCreateBody struct {
	Branch struct {
		Name      string `json:"name"`
		ParentID  string `json:"parent_id"`
		ExpiresAt string `json:"expires_at"`
	} `json:"branch"`
	Endpoints []struct {
		Type  string  `json:"type"`
		MinCU float64 `json:"autoscaling_limit_min_cu"`
		MaxCU float64 `json:"autoscaling_limit_max_cu"`
	} `json:"endpoints"`
}

const (
	neonProject      = "shiny-wind-028834"
	neonKey          = "napi_test_key_not_real"
	neonProdBranch   = "br-main-100"
	neonProdHost     = "ep-prod-100.us-east-2.aws.neon.tech"
	neonDevBranch    = "br-dev-200"
	neonDevHost      = "ep-dev-200.us-east-2.aws.neon.tech"
	neonFakePassword = "npg_s3cr3tPW"
)

func newNeonFake(t *testing.T) *neonFake {
	t.Helper()
	f := &neonFake{project: neonProject, key: neonKey, pollsToFinish: 1,
		branches: map[string]fakeNeonBranch{
			neonProdBranch: {ID: neonProdBranch, Name: "main", Default: true},
			neonDevBranch:  {ID: neonDevBranch, Name: "dev", ParentID: neonProdBranch},
		},
		endpoints: map[string]fakeNeonEndpoint{
			"ep-prod-100": {ID: "ep-prod-100", Host: neonProdHost, BranchID: neonProdBranch,
				Type: "read_write"},
			"ep-dev-200": {ID: "ep-dev-200", Host: neonDevHost, BranchID: neonDevBranch,
				Type: "read_write"},
		},
		ops: map[string]*fakeNeonOp{}}
	mux := http.NewServeMux()
	base := "/projects/{project}/"
	mux.HandleFunc("GET "+base+"endpoints", f.listEndpoints)
	mux.HandleFunc("GET "+base+"branches", f.listBranches)
	mux.HandleFunc("POST "+base+"branches", f.createBranch)
	mux.HandleFunc("GET "+base+"branches/{id}", f.getBranch)
	mux.HandleFunc("DELETE "+base+"branches/{id}", f.deleteBranch)
	mux.HandleFunc("GET "+base+"operations/{id}", f.operation)
	mux.HandleFunc("GET "+base+"connection_uri", f.connectionURI)
	f.srv = httptest.NewServer(f.guard(mux))
	t.Cleanup(f.srv.Close)
	return f
}

// guard records the request and answers 401 without the key and 404 for
// another project, as Neon does.
func (f *neonFake) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.key {
			neonError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/projects/"+f.project+"/") {
			neonError(w, http.StatusNotFound, "project not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func neonError(w http.ResponseWriter, status int, msg string) {
	writeNeonJSON(w, status, map[string]string{"code": "", "message": msg})
}

func writeNeonJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *neonFake) listEndpoints(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakeNeonEndpoint{}
	for _, e := range f.endpoints {
		out = append(out, e)
	}
	writeNeonJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

func (f *neonFake) listBranches(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakeNeonBranch{}
	for _, b := range f.branches {
		if strings.Contains(b.Name, search) || strings.Contains(b.ID, search) {
			out = append(out, b)
		}
	}
	writeNeonJSON(w, http.StatusOK, map[string]any{"branches": out})
}

func (f *neonFake) createBranch(w http.ResponseWriter, r *http.Request) {
	var body neonCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		neonError(w, http.StatusBadRequest, "bad body")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	if f.createStatus != 0 {
		neonError(w, f.createStatus, "refused by the fake")
		return
	}
	if _, ok := f.branches[body.Branch.ParentID]; !ok {
		neonError(w, http.StatusBadRequest, "parent branch not found")
		return
	}
	f.n++
	b := fakeNeonBranch{ID: fmt.Sprintf("br-fake-%d", f.n), Name: body.Branch.Name,
		ParentID: body.Branch.ParentID, ExpiresAt: body.Branch.ExpiresAt}
	host := fmt.Sprintf("ep-fake-%d.us-east-2.aws.neon.tech", f.n)
	if f.endpointHost != "" {
		host = f.endpointHost
	}
	e := fakeNeonEndpoint{ID: fmt.Sprintf("ep-fake-%d", f.n), Host: host, BranchID: b.ID,
		Type: "read_write"}
	f.branches[b.ID], f.endpoints[e.ID] = b, e
	if f.createThenStatus != 0 {
		neonError(w, f.createThenStatus, "upstream timed out")
		return
	}
	ops := []*fakeNeonOp{
		{ID: fmt.Sprintf("op-%d-a", f.n), Action: "create_branch", Status: "running",
			BranchID: b.ID},
		{ID: fmt.Sprintf("op-%d-b", f.n), Action: "start_compute", Status: "scheduling",
			BranchID: b.ID},
	}
	for _, op := range ops {
		f.ops[op.ID] = op
	}
	writeNeonJSON(w, http.StatusCreated, map[string]any{"branch": b,
		"endpoints": []fakeNeonEndpoint{e}, "operations": ops})
}

func (f *neonFake) getBranch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[r.PathValue("id")]
	if !ok {
		neonError(w, http.StatusNotFound, "branch not found")
		return
	}
	writeNeonJSON(w, http.StatusOK, map[string]any{"branch": b})
}

func (f *neonFake) deleteBranch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteStatus != 0 {
		neonError(w, f.deleteStatus, "delete refused by the fake")
		return
	}
	id := r.PathValue("id")
	b, ok := f.branches[id]
	if !ok {
		neonError(w, http.StatusNotFound, "branch not found")
		return
	}
	delete(f.branches, id)
	for eid, e := range f.endpoints {
		if e.BranchID == id {
			delete(f.endpoints, eid)
		}
	}
	writeNeonJSON(w, http.StatusOK, map[string]any{"branch": b, "operations": []any{}})
}

func (f *neonFake) operation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[r.PathValue("id")]
	if !ok {
		neonError(w, http.StatusNotFound, "operation not found")
		return
	}
	op.polls++
	if !f.opsNeverFinish && op.polls >= f.pollsToFinish {
		op.Status = "finished"
		if op.Action == f.failAction {
			op.Status = "failed"
		}
	}
	writeNeonJSON(w, http.StatusOK, map[string]any{"operation": op})
}

func (f *neonFake) connectionURI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	var host string
	for _, e := range f.endpoints {
		if e.BranchID == q.Get("branch_id") {
			host = e.Host
		}
	}
	if host == "" || q.Get("database_name") == "" || q.Get("role_name") == "" {
		neonError(w, http.StatusNotFound, "endpoint, database or role not found")
		return
	}
	query := "sslmode=require&channel_binding=require"
	if f.uriQuery != "" {
		query = f.uriQuery
	}
	uri := fmt.Sprintf("postgresql://%s:%s@%s/%s?%s", q.Get("role_name"),
		neonFakePassword, host, q.Get("database_name"), query)
	if f.noURI {
		uri = ""
	}
	writeNeonJSON(w, http.StatusOK, map[string]string{"uri": uri})
}

// exists reports whether branch ref is still in the project.
func (f *neonFake) exists(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.branches[ref]
	return ok
}

func (f *neonFake) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *neonFake) countRequests(prefix string) int {
	n := 0
	for _, r := range f.requestLog() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *neonFake) branchNamed(name string) (fakeNeonBranch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.branches {
		if b.Name == name {
			return b, true
		}
	}
	return fakeNeonBranch{}, false
}

// neonProd is the monitored (production) database the fake's clones copy.
var neonProd = clone.NeonProduction{Host: neonProdHost, User: "app_owner", Database: "appdb"}

var neonNow = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

// neonConfig is a provider config against f; mutate adjusts it.
func neonConfig(f *neonFake, mutate func(*clone.NeonConfig)) clone.NeonConfig {
	cfg := clone.NeonConfig{ProjectID: f.project, APIKey: f.key, EndpointMaxCU: 1,
		TTL: time.Hour, BaseURL: f.srv.URL, HTTPClient: f.srv.Client(),
		WaitTimeout: 5 * time.Second, PollEvery: 5 * time.Millisecond,
		Production: func(context.Context) (clone.NeonProduction, error) {
			return neonProd, nil
		},
		Now: func() time.Time { return neonNow }}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func newNeonProvider(t *testing.T, f *neonFake,
	mutate func(*clone.NeonConfig)) *clone.NeonBranchProvider {
	t.Helper()
	p, err := clone.NewNeonBranchProvider(neonConfig(f, mutate))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
