package codehost

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const testToken = "glpat-SECRETtoken-0123456789abcdef"

// seen is one request the recorded host received.
type seen struct {
	Method, Path, RawPath, Query, Body string
	Header                             http.Header
}

// answer is a recorded response.
type answer struct {
	status  int
	fixture string // file under testdata, or "" for body
	body    string
	header  map[string]string
}

// recorder replays recorded answers keyed by "METHOD /escaped/path".
type recorder struct {
	t        *testing.T
	mu       sync.Mutex
	routes   map[string]answer
	reqs     []seen
	srv      *httptest.Server
	fallback answer
}

func newRecorder(t *testing.T, routes map[string]answer) *recorder {
	t.Helper()
	rec := &recorder{t: t, routes: routes,
		fallback: answer{status: http.StatusNotFound, body: `{"message":"Not Found"}`}}
	rec.srv = httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *recorder) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.EscapedPath()
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, seen{Method: r.Method, Path: r.URL.Path,
		RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery, Body: string(body),
		Header: r.Header.Clone()})
	a, ok := rec.routes[key]
	rec.mu.Unlock()
	if !ok {
		a = rec.fallback
	}
	for k, v := range a.header {
		w.Header().Set(k, v)
	}
	out := []byte(a.body)
	if a.fixture != "" {
		raw, err := os.ReadFile(filepath.Join("testdata", a.fixture))
		if err != nil {
			rec.t.Errorf("read fixture %s: %v", a.fixture, err)
		}
		out = raw
	}
	if a.status == 0 {
		a.status = http.StatusOK
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(out)
}

// set adds or replaces one recorded answer.
func (rec *recorder) set(key string, a answer) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.routes == nil {
		rec.routes = map[string]answer{}
	}
	rec.routes[key] = a
}

func (rec *recorder) requests() []seen {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]seen(nil), rec.reqs...)
}

// keys lists "METHOD rawpath" of every request, in order.
func (rec *recorder) keys() []string {
	var out []string
	for _, r := range rec.requests() {
		out = append(out, r.Method+" "+r.RawPath)
	}
	return out
}
