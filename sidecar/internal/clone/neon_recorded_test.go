package clone_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05b: create, poll to ready and destroy, in order, against the
// recorded Neon API v2 responses in testdata/neon.

const recordedProject = "shiny-wind-028834"

type recordedRoute struct {
	status int
	files  []string // served in turn; the last one repeats
}

type recordedNeon struct {
	mu     sync.Mutex
	routes map[string]*recordedRoute
	order  []string
}

func (r *recordedNeon) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key := req.Method + " " + req.URL.Path
	r.mu.Lock()
	r.order = append(r.order, key)
	route, ok := r.routes[key]
	var file string
	if ok {
		file = route.files[0]
		if len(route.files) > 1 {
			route.files = route.files[1:]
		}
	}
	r.mu.Unlock()
	if !ok || req.Header.Get("Authorization") != "Bearer "+neonKey {
		http.Error(w, `{"message":"not recorded"}`, http.StatusNotFound)
		return
	}
	body, err := os.ReadFile(filepath.Join("testdata", "neon", file))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = w.Write(body)
}

// recordedRoutes serves the testdata/neon fixtures; each operation is
// running on its first poll and finished on its second.
func recordedRoutes(p, opA, opB, branch string) map[string]*recordedRoute {
	return map[string]*recordedRoute{
		"GET " + p + "/endpoints": {200, []string{"endpoints_list.json"}},
		"POST " + p + "/branches": {201, []string{"create_branch.json"}},
		"GET " + opA: {200, []string{"operation_create_branch_running.json",
			"operation_create_branch_finished.json"}},
		"GET " + opB: {200, []string{"operation_start_compute_running.json",
			"operation_start_compute_finished.json"}},
		"GET " + p + "/connection_uri": {200, []string{"connection_uri.json"}},
		"GET " + branch:                {200, []string{"branch_get.json"}},
		"DELETE " + branch:             {200, []string{"branch_delete.json"}},
	}
}

func recordedProvider(t *testing.T, srv *httptest.Server) *clone.NeonBranchProvider {
	t.Helper()
	prov, err := clone.NewNeonBranchProvider(clone.NeonConfig{ProjectID: recordedProject,
		APIKey: neonKey, EndpointMaxCU: 1, TTL: time.Hour, BaseURL: srv.URL,
		HTTPClient: srv.Client(), PollEvery: time.Millisecond, WaitTimeout: 5 * time.Second,
		Production: func(context.Context) (clone.NeonProduction, error) {
			return clone.NeonProduction{User: "app_owner", Database: "appdb",
				Host: "ep-cool-darkness-123456.us-east-2.aws.neon.tech"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return prov
}

func TestNeonRecorded_CreatePollReadyDestroyInOrder(t *testing.T) {
	p := "/projects/" + recordedProject
	opA := p + "/operations/b6afbc21-2990-4a76-980b-b57d8c2948f2"
	opB := p + "/operations/c7bfcd32-3aa1-4b87-a91e-c68e9d3a59a3"
	branch := p + "/branches/br-wispy-meadow-a5k2x9q1"
	rec := &recordedNeon{routes: recordedRoutes(p, opA, opB, branch)}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	prov := recordedProvider(t, srv)
	c, err := prov.Create(context.Background(), neonSpec("sage-rehearsal-0a1b2c3d"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal("recorded DSN does not parse")
	}
	pw, _ := u.User.Password()
	if c.ID != "br-wispy-meadow-a5k2x9q1" ||
		u.Hostname() != "ep-mute-rain-a5r1k0ew.us-east-2.aws.neon.tech" ||
		u.Query().Get("sslmode") != "require" || pw != "npg_FIXTUREpw" {
		t.Fatalf("clone id %q host %q sslmode %q", c.ID, u.Hostname(),
			u.Query().Get("sslmode"))
	}
	if err := prov.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET " + p + "/endpoints", "POST " + p + "/branches",
		"GET " + opA, "GET " + opB, "GET " + opA, "GET " + opB,
		"GET " + p + "/connection_uri", "GET " + branch, "DELETE " + branch}
	if !reflect.DeepEqual(rec.order, want) {
		t.Fatalf("requests in order:\n got %v\nwant %v", rec.order, want)
	}
}
