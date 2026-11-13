package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// destRequest is one request a destination fake received (market-14).
type destRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// destFake answers every request with status (and Retry-After when set)
// and records what it received.
type destFake struct {
	mu         sync.Mutex
	status     int
	retryAfter string
	reqs       []destRequest
}

func newDestFake(t *testing.T, status int) (*destFake, *httptest.Server) {
	t.Helper()
	f := &destFake{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		req := destRequest{Method: r.Method, Path: r.URL.EscapedPath(),
			Query: r.URL.RawQuery, Header: r.Header.Clone(), Raw: raw}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.Body); err != nil {
				t.Errorf("body is not a JSON object: %v (%s)", err, raw)
			}
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		status, retryAfter := f.status, f.retryAfter
		f.mu.Unlock()
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *destFake) requests() []destRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]destRequest(nil), f.reqs...)
}

func (f *destFake) set(status int, retryAfter string) {
	f.mu.Lock()
	f.status, f.retryAfter = status, retryAfter
	f.mu.Unlock()
}

// assertJSONEqual compares got with the golden want after a JSON round trip.
func assertJSONEqual(t *testing.T, got map[string]any, want string) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("golden is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("payload mismatch\n got: %s\nwant: %s", g, want)
	}
}

func destEvent(resolve bool) Event {
	return Event{Type: "finding_opened", Severity: "critical",
		Subject: "Replication lag on db1", Body: "lag is 12 minutes",
		DedupKey: "finding:db1:replication:lag", Resolve: resolve,
		Data: map[string]any{"database": "db1", "category": "replication"}}
}
