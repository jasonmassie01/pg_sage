package cnpg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// market-12: the read-only Cluster resolver against an httptest Kubernetes
// API. No real cluster is ever contacted.

const clusterPath = "/apis/postgresql.cnpg.io/v1/namespaces/db/clusters/pg-main"

const clusterJSON = `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster",
 "metadata":{"name":"pg-main","namespace":"db"},
 "spec":{"instances":3,"primaryUpdateStrategy":"supervised",
  "postgresql":{"parameters":{"work_mem":"16MB","max_connections":"200"}}}}`

type apiCall struct {
	method, path, auth string
}

func fakeAPI(t *testing.T, status int, body string) (*httptest.Server, *[]apiCall) {
	t.Helper()
	var mu sync.Mutex
	calls := &[]apiCall{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		mu.Lock()
		*calls = append(*calls, apiCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestSource(t *testing.T, srv *httptest.Server, token string) *Source {
	t.Helper()
	s, err := NewSource(Cluster{Name: "pg-main", Namespace: "db"},
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()),
		WithTokenFile(tokenFile(t, token)))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return s
}

func TestTargetReadsClusterParameters(t *testing.T) {
	srv, calls := fakeAPI(t, http.StatusOK, clusterJSON)
	target, err := newTestSource(t, srv, "sa-token").Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.Provider != "cnpg" || target.Cluster != "pg-main" || target.Namespace != "db" ||
		!target.Known || !target.FlagsKnown || target.Unresolved != "" ||
		target.UpdateStrategy != "supervised" || len(target.Flags) != 2 ||
		target.Flags["work_mem"] != "16MB" || target.Flags["max_connections"] != "200" {
		t.Fatalf("target = %+v", target)
	}
	if len(*calls) != 1 || (*calls)[0] != (apiCall{http.MethodGet, clusterPath,
		"Bearer sa-token"}) {
		t.Fatalf("API calls = %+v, want one GET of the Cluster", *calls)
	}
}

// Nil/empty: a Cluster without parameters is a known target with no flags.
func TestTargetEmptyParametersIsKnown(t *testing.T) {
	srv, _ := fakeAPI(t, http.StatusOK, `{"spec":{"instances":1}}`)
	target, err := newTestSource(t, srv, "t").Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if !target.Known || !target.FlagsKnown || target.Flags == nil || len(target.Flags) != 0 ||
		target.UpdateStrategy != "" {
		t.Fatalf("target = %+v", target)
	}
}

// Error propagation: each status says what to fix.
func TestTargetStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   []string
	}{
		{http.StatusForbidden, []string{"service account lacks get on " +
			"clusters.postgresql.cnpg.io", "db/pg-main"}},
		{http.StatusNotFound, []string{"db/pg-main", "not found"}},
		{http.StatusUnauthorized, []string{"token", "401"}},
		{http.StatusInternalServerError, []string{"500", "boom"}},
	} {
		srv, _ := fakeAPI(t, tc.status, `{"kind":"Status","message":"boom"}`)
		target, err := newTestSource(t, srv, "t").Target(context.Background())
		if err == nil || target.Known {
			t.Fatalf("%d: target %+v err %v", tc.status, target, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%d: error %q lacks %q", tc.status, err, w)
			}
		}
		if strings.Contains(err.Error(), "Bearer") {
			t.Errorf("%d: error leaks the token header: %q", tc.status, err)
		}
	}
}

func TestTargetMalformedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>`,
		"numeric value": `{"spec":{"postgresql":{"parameters":{"work_mem":64}}}}`,
		"oversized":     `{"spec":{"x":"` + strings.Repeat("a", 2<<20) + `"}}`,
		"invalid key":   `{"spec":{"postgresql":{"parameters":{"Bad Key":"1"}}}}`,
		"empty body":    ``,
	} {
		srv, _ := fakeAPI(t, http.StatusOK, body)
		_, err := newTestSource(t, srv, "t").Target(context.Background())
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Boundary: the request is bounded at 5 s by default; a shorter bound is
// honoured, and a hung API server gives a timeout error, never a hang.
func TestTargetTimeoutIsBounded(t *testing.T) {
	if RequestTimeout != 5*time.Second {
		t.Fatalf("RequestTimeout = %s, want 5s", RequestTimeout)
	}
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	s, err := NewSource(Cluster{Name: "pg-main", Namespace: "db"}, WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()), WithTokenFile(tokenFile(t, "t")),
		WithTimeout(150*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = s.Target(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out after 150ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Target took %s", elapsed)
	}
}

// Invalid input: names outside the RFC 1123 label pattern are refused
// before any request is built.
func TestNewSourceRejectsInvalidNames(t *testing.T) {
	for _, c := range []Cluster{{"", "db"}, {"pg-main", ""}, {"Pg", "db"},
		{"pg/../x", "db"}, {"pg-main", "db?x=1"}, {strings.Repeat("a", 64), "db"}} {
		if _, err := NewSource(c, WithBaseURL("https://k8s"),
			WithTokenFile("unused")); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

func TestNewSourceOutsideAPodNamesTheEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	_, err := NewSource(Cluster{Name: "pg-main", Namespace: "db"})
	if err == nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Fatalf("err = %v", err)
	}
}

// The projected service-account token rotates: it is read on every call.
func TestTargetRereadsTheToken(t *testing.T) {
	srv, calls := fakeAPI(t, http.StatusOK, clusterJSON)
	path := tokenFile(t, "first")
	s, err := NewSource(Cluster{Name: "pg-main", Namespace: "db"}, WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()), WithTokenFile(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Target(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Target(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[1].auth != "Bearer second" {
		t.Fatalf("calls = %+v", *calls)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Target(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "service-account token") {
		t.Fatalf("missing token: err = %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("no request may be sent without a token: %+v", *calls)
	}
}

func TestTargetConcurrentCalls(t *testing.T) {
	srv, calls := fakeAPI(t, http.StatusOK, clusterJSON)
	s := newTestSource(t, srv, "t")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target, err := s.Target(context.Background())
			if err == nil && target.Flags["work_mem"] != "16MB" {
				err = context.Canceled
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Target: %v", err)
		}
	}
	if len(*calls) != 16 {
		t.Fatalf("%d calls, want 16", len(*calls))
	}
}

func TestTargetHonoursCallerCancel(t *testing.T) {
	srv, calls := fakeAPI(t, http.StatusOK, clusterJSON)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestSource(t, srv, "t").Target(ctx); err == nil {
		t.Fatal("a cancelled context must fail")
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
}
