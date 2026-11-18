package clone_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05g: restore, poll and delete, in order, against the ARM responses
// in testdata/azure (ARM names the region "East US" on a server and
// "eastus" in an operation URL).

type recordedARM struct {
	mu     sync.Mutex
	base   string
	routes map[string]*recordedARMRoute
	order  []string
}

type recordedARMRoute struct {
	status int
	files  []string
	async  string // the Azure-AsyncOperation path, served on this endpoint
}

func (r *recordedARM) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
	if !ok || req.Header.Get("Authorization") != "Bearer "+azToken ||
		req.URL.Query().Get("api-version") != clone.AzureFlexibleServerAPIVersion {
		writeARMError(w, http.StatusNotFound, "NotRecorded", "not recorded")
		return
	}
	body, err := os.ReadFile(filepath.Join("testdata", "azure", file))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if route.async != "" {
		w.Header().Set("Azure-AsyncOperation", r.base+route.async+
			"?api-version="+clone.AzureFlexibleServerAPIVersion)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = w.Write(body)
}

func TestAzureRestoreRecorded_RestorePollDeleteInOrder(t *testing.T) {
	src, dst := azServersID+azServer, azServersID+azName
	op := "/subscriptions/" + azSub + "/providers/Microsoft.DBforPostgreSQL/locations/" +
		"eastus/azureAsyncOperation/4b0d3f29-6c1e-4e7a-9b1d-2a3f4c5d6e7f"
	rec := &recordedARM{routes: map[string]*recordedARMRoute{
		"GET " + src: {status: 200, files: []string{"server_source.json"}},
		"GET " + dst: {status: 200, files: []string{"server_not_found.json",
			"server_clone_ready.json"}},
		"PUT " + dst: {status: 202, files: []string{"put_accepted.json"}, async: op},
		"GET " + op: {status: 200, files: []string{"operation_in_progress.json",
			"operation_succeeded.json"}},
		"DELETE " + dst: {status: 202, files: []string{"delete_accepted.json"}},
	}}
	rec.routes["GET "+dst].status = 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The clone is not found until its PUT, then found.
		rec.mu.Lock()
		if r.Method == http.MethodPut {
			rec.routes["GET "+dst].status = 200
		}
		rec.mu.Unlock()
		rec.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	rec.base = srv.URL
	prov := newAzureProvider(t, srv.URL, nil)
	c, err := prov.Create(context.Background(), clone.CloneSpec{ResourceName: azName})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal("recorded DSN does not parse")
	}
	pw, _ := u.User.Password()
	if c.ID != azName || u.Hostname() != azName+azSuffix || pw != azPassword ||
		u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone id %q host %q sslmode %q", c.ID, u.Hostname(),
			u.Query().Get("sslmode"))
	}
	if err := prov.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET " + src, "GET " + src, "GET " + dst, "PUT " + dst, "GET " + op,
		"GET " + op, "GET " + dst, "GET " + dst, "DELETE " + dst}
	if !reflect.DeepEqual(rec.order, want) {
		t.Fatalf("requests in order:\n got %s\nwant %s", strings.Join(rec.order, "\n     "),
			strings.Join(want, "\n     "))
	}
}
