package clone_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// fleetpol-01b: the read-only Neon listing discovery uses. The replies
// follow the Neon API v2 shapes (projects, branches with created_at,
// creation_source and created_by, endpoints); a live account is OWNER
// ACTION (CHECK-26).

type neonListingServer struct {
	mu    sync.Mutex
	paths []string
	auth  []string
	reply map[string]struct {
		code int
		body string
	}
}

func newNeonListing(t *testing.T) (*neonListingServer, *httptest.Server) {
	t.Helper()
	f := &neonListingServer{reply: map[string]struct {
		code int
		body string
	}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		rep, ok := f.reply[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(rep.code)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *neonListingServer) set(path string, code int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply[path] = struct {
		code int
		body string
	}{code, body}
}

func lister(t *testing.T, srv *httptest.Server, key string) *clone.NeonLister {
	t.Helper()
	l, err := clone.NewNeonLister(clone.NeonListerConfig{APIKey: key, BaseURL: srv.URL,
		HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewNeonLister: %v", err)
	}
	return l
}

func TestNeonLister_DecodesProjectsBranchesAndEndpoints(t *testing.T) {
	f, srv := newNeonListing(t)
	f.set("/projects", 200, `{"projects":[{"id":"p-one","name":"one"}]}`)
	f.set("/projects/p-one/branches", 200, `{"branches":[{"id":"br-a","name":"main",
		"project_id":"p-one","default":true,"created_at":"2026-11-01T10:00:00Z",
		"creation_source":"console","created_by":{"name":"Ada","image":""}},
		{"id":"br-b","name":"agent-1","parent_id":"br-a","project_id":"p-one",
		"created_at":"2026-11-02T10:00:00Z","creation_source":"api"}]}`)
	f.set("/projects/p-one/endpoints", 200, `{"endpoints":[{"id":"ep-a",
		"host":"ep-a.neon.tech","branch_id":"br-a","type":"read_write"}]}`)
	l := lister(t, srv, "k-123")
	ctx := context.Background()
	projects, err := l.ListProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != "p-one" {
		t.Fatalf("projects = %+v %v", projects, err)
	}
	branches, err := l.ListBranches(ctx, "p-one")
	if err != nil || len(branches) != 2 {
		t.Fatalf("branches = %+v %v", branches, err)
	}
	a, b := branches[0], branches[1]
	if a.ID != "br-a" || !a.Default || a.CreationSource != "console" || a.CreatedBy != "Ada" ||
		!a.CreatedAt.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("branch a = %+v", a)
	}
	if b.ParentID != "br-a" || b.CreationSource != "api" || b.CreatedBy != "" ||
		b.ProjectID != "p-one" {
		t.Fatalf("branch b = %+v", b)
	}
	eps, err := l.ListEndpoints(ctx, "p-one")
	if err != nil || len(eps) != 1 || eps[0].Host != "ep-a.neon.tech" || eps[0].BranchID != "br-a" {
		t.Fatalf("endpoints = %+v %v", eps, err)
	}
	for _, h := range f.auth {
		if h != "Bearer k-123" {
			t.Fatalf("Authorization = %q", h)
		}
	}
	if !strings.Contains(f.paths[0], "limit=") {
		t.Fatalf("project listing is unbounded: %s", f.paths[0])
	}
}

func TestNeonLister_EmptyReplies(t *testing.T) {
	f, srv := newNeonListing(t)
	f.set("/projects", 200, `{"projects":[]}`)
	f.set("/projects/p/branches", 200, `{}`)
	l := lister(t, srv, "k")
	if got, err := l.ListProjects(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("projects = %+v %v", got, err)
	}
	if got, err := l.ListBranches(context.Background(), "p"); err != nil || len(got) != 0 {
		t.Fatalf("branches = %+v %v", got, err)
	}
}

func TestNeonLister_ErrorsAreThrottledOrNamed(t *testing.T) {
	f, srv := newNeonListing(t)
	f.set("/projects", http.StatusTooManyRequests, `{"message":"slow down"}`)
	f.set("/projects/p/branches", http.StatusUnauthorized, `{"message":"bad key k-secret"}`)
	f.set("/projects/p/endpoints", 200, `not json`)
	l := lister(t, srv, "k-secret")
	ctx := context.Background()
	_, err := l.ListProjects(ctx)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 429 || !clone.Throttled(err) {
		t.Fatalf("429: %v, want a throttled ProviderStatusError", err)
	}
	_, err = l.ListBranches(ctx, "p")
	if err == nil || clone.Throttled(err) || strings.Contains(err.Error(), "k-secret") ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("401: %v, want a non-throttled status error without the key", err)
	}
	if _, err := l.ListEndpoints(ctx, "p"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("bad JSON: %v, want a decode error", err)
	}
}

func TestNeonLister_InvalidInput(t *testing.T) {
	if _, err := clone.NewNeonLister(clone.NeonListerConfig{}); err == nil ||
		!strings.Contains(err.Error(), "clone.neon.api_key") {
		t.Fatalf("no key: %v, want an error naming clone.neon.api_key", err)
	}
	_, srv := newNeonListing(t)
	l := lister(t, srv, "k")
	for _, p := range []string{"", "../x", "P_UPPER"} {
		if _, err := l.ListBranches(context.Background(), p); err == nil {
			t.Errorf("project %q listed", p)
		}
	}
}

func TestIsNeonRehearsalBranch(t *testing.T) {
	for name, want := range map[string]bool{"sage-rh-0a1b2c3d": true,
		"sage-migration-ffffffff": true, "main": false, "sage-": false, "agent-1": false,
		"sage-rh-0A1B2C3D": false} {
		if got := clone.IsNeonRehearsalBranch(name); got != want {
			t.Errorf("IsNeonRehearsalBranch(%q) = %v, want %v", name, got, want)
		}
	}
}
