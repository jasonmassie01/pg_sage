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

// lab-05e: clone, poll, label and destroy, in order, against the Cloud SQL
// Admin API v1 responses in testdata/cloudsql.

type recordedSQL struct {
	mu     sync.Mutex
	routes map[string]*recordedRoute
	order  []string
}

func (r *recordedSQL) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
	if !ok || req.Header.Get("Authorization") != "Bearer "+sqlToken {
		writeSQLError(w, http.StatusNotFound, "not recorded")
		return
	}
	body, err := os.ReadFile(filepath.Join("testdata", "cloudsql", file))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = w.Write(body)
}

func TestCloudSQLRecorded_ClonePollLabelDestroyInOrder(t *testing.T) {
	p := "/v1/projects/" + sqlProject
	src, dst := p+"/instances/shop", p+"/instances/"+sqlName
	op1 := p + "/operations/3f6c1a2e-7d4b-4c8a-9e1f-000000000001"
	op2 := p + "/operations/3f6c1a2e-7d4b-4c8a-9e1f-000000000002"
	running, done := "operation_clone_running.json", "operation_clone_done.json"
	unlabelled, labelled := "instance_clone.json", "instance_clone_labelled.json"
	rec := &recordedSQL{routes: map[string]*recordedRoute{
		"GET " + src:             {200, []string{"instance_source.json"}},
		"POST " + src + "/clone": {200, []string{"clone_operation.json"}},
		"GET " + op1:             {200, []string{running, done}},
		"GET " + dst:             {200, []string{unlabelled, labelled}},
		"PATCH " + dst:           {200, []string{"patch_operation.json"}},
		"GET " + op2:             {200, []string{"operation_patch_done.json"}},
		"DELETE " + dst:          {200, []string{"delete_operation.json"}},
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	prov := newCloudSQLProvider(t, srv.URL, func(c *clone.CloudSQLCloneConfig) {
		c.HTTPClient = srv.Client()
		c.Now = func() time.Time { return sqlNow }
	})
	c, err := prov.Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal("recorded DSN does not parse")
	}
	pw, _ := u.User.Password()
	if c.ID != sqlName || u.Hostname() != "10.0.0.17" || pw != sqlPassword ||
		u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone id %q host %q sslmode %q", c.ID, u.Hostname(),
			u.Query().Get("sslmode"))
	}
	if err := prov.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET " + src, "GET " + src, "POST " + src + "/clone", "GET " + op1,
		"GET " + op1, "GET " + dst, "PATCH " + dst, "GET " + op2, "GET " + dst,
		"DELETE " + dst}
	if !reflect.DeepEqual(rec.order, want) {
		t.Fatalf("requests in order:\n got %v\nwant %v", rec.order, want)
	}
}
