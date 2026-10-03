package slo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Prometheus-compatible SLI connector (AI-SRE-SPEC §8): read-only
// HTTP API query and query_range with bearer auth and a timeout. Every
// failure is a typed error, and no error message carries the token.

const token = "prom-secret-token-123"

type promStub struct {
	mu      sync.Mutex
	queries []string
	auth    []string
	answer  func(path, query string) (int, string)
	delay   time.Duration
}

func (s *promStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Path+" "+r.Form.Get("query"))
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		answer, delay := s.answer, s.delay
		s.mu.Unlock()
		if delay > 0 {
			select { // a client that gave up ends the wait
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		code, body := answer(r.URL.Path, r.Form.Get("query"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vector(values ...string) string {
	var items []string
	for _, v := range values {
		items = append(items, fmt.Sprintf(`{"metric":{},"value":[1759320000,%q]}`, v))
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(items, ",") + `]}}`
}

func newClient(t *testing.T, url string, timeout time.Duration) *PromClient {
	t.Helper()
	c, err := NewPromClient(PromConfig{URL: url, BearerToken: token, Timeout: timeout})
	if err != nil {
		t.Fatalf("NewPromClient: %v", err)
	}
	return c
}

func TestNewPromClient_Validation(t *testing.T) {
	for name, cfg := range map[string]PromConfig{
		"empty url":        {},
		"no scheme":        {URL: "prometheus:9090"},
		"ftp scheme":       {URL: "ftp://prometheus:9090"},
		"credentials":      {URL: "https://user:pass@prometheus:9090"},
		"negative timeout": {URL: "http://prometheus:9090", Timeout: -time.Second},
		"huge timeout":     {URL: "http://prometheus:9090", Timeout: 2 * time.Minute},
	} {
		_, err := NewPromClient(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "pass") {
			t.Errorf("%s: error leaks the credentials: %v", name, err)
		}
	}
	c, err := NewPromClient(PromConfig{URL: "http://prometheus:9090/"})
	if err != nil || c.Timeout() != DefaultPromTimeout {
		t.Fatalf("defaults: %v timeout %v", err, c.Timeout())
	}
}

func TestPromClient_QueryScalarAndVector(t *testing.T) {
	stub := &promStub{answer: func(_, q string) (int, string) {
		if q == "scalar(1)" {
			return 200, `{"status":"success","data":{"resultType":"scalar",` +
				`"result":[1759320000,"1"]}}`
		}
		return 200, vector("42.5")
	}}
	c := newClient(t, stub.server(t).URL, time.Second)
	v, err := c.Query(context.Background(), "sum(up)", now)
	if err != nil || v != 42.5 {
		t.Fatalf("Query = %v, %v", v, err)
	}
	if v, err := c.Query(context.Background(), "scalar(1)", now); err != nil || v != 1 {
		t.Fatalf("scalar = %v, %v", v, err)
	}
	if stub.auth[0] != "Bearer "+token || !strings.HasPrefix(stub.queries[0],
		"/api/v1/query sum(up)") {
		t.Fatalf("request = %v %v", stub.queries, stub.auth)
	}
}

func TestPromClient_TypedErrors(t *testing.T) {
	cases := map[string]struct {
		code int
		body string
		want error
	}{
		"empty vector": {200, vector(), ErrNoData},
		"two series":   {200, vector("1", "2"), ErrAmbiguous},
		"nan":          {200, vector("NaN"), ErrInvalidValue},
		"inf":          {200, vector("+Inf"), ErrInvalidValue},
		"negative":     {200, vector("-3"), ErrInvalidValue},
		"not a number": {200, vector("abc"), ErrInvalidValue},
		"bad query": {400, `{"status":"error","errorType":"bad_data",` +
			`"error":"parse error"}`, ErrRejected},
		"unauthorized": {401, `unauthorized`, ErrRejected},
		"server error": {503, `<html>down</html>`, ErrUnavailable},
		"garbage 200":  {200, `{not json`, ErrUnavailable},
		"error status": {200, `{"status":"error","error":"x"}`, ErrRejected},
		"matrix": {200, `{"status":"success","data":{"resultType":"matrix",` +
			`"result":[]}}`, ErrUnavailable},
	}
	for name, c := range cases {
		stub := &promStub{answer: func(string, string) (int, string) { return c.code, c.body }}
		cl := newClient(t, stub.server(t).URL, time.Second)
		_, err := cl.Query(context.Background(), "q", now)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("%s: error leaks the bearer token: %v", name, err)
		}
	}
}

func TestPromClient_TimeoutAndUnreachable(t *testing.T) {
	// The server answers after 30 s; the client's 50 ms timeout must end
	// the query. The 3 s budget absorbs a loaded runner and stays below
	// the 5 s default timeout a client ignoring its setting would use.
	stub := &promStub{delay: 30 * time.Second,
		answer: func(string, string) (int, string) { return 200, vector("1") }}
	c := newClient(t, stub.server(t).URL, 50*time.Millisecond)
	start := time.Now()
	_, err := c.Query(context.Background(), "q", now)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: err = %v after %s", err, time.Since(start))
	}
	dead := newClient(t, "http://127.0.0.1:1", time.Second)
	if _, err := dead.Query(context.Background(), "q", now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Query(ctx, "q", now); err == nil {
		t.Fatal("cancelled context succeeded")
	}
}

func TestPromClient_QueryRange(t *testing.T) {
	stub := &promStub{answer: func(path, _ string) (int, string) {
		if path != "/api/v1/query_range" {
			return 404, "no"
		}
		return 200, `{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{},"values":[[1759320000,"1"],[1759320120,"NaN"],[1759320240,"3"]]}]}}`
	}}
	c := newClient(t, stub.server(t).URL, time.Second)
	pts, err := c.QueryRange(context.Background(), "q", now.Add(-4*time.Minute), now,
		2*time.Minute)
	if err != nil || len(pts) != 3 {
		t.Fatalf("QueryRange = %v, %v", pts, err)
	}
	if pts[0].Value != 1 || !math.IsNaN(pts[1].Value) || pts[2].Value != 3 ||
		pts[0].At.Unix() != 1759320000 {
		t.Fatalf("points = %+v", pts)
	}
	empty := &promStub{answer: func(string, string) (int, string) {
		return 200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	}}
	ce := newClient(t, empty.server(t).URL, time.Second)
	if _, err := ce.QueryRange(context.Background(), "q", now.Add(-time.Hour), now,
		time.Minute); !errors.Is(err, ErrNoData) {
		t.Fatalf("empty matrix: err = %v", err)
	}
	if _, err := ce.QueryRange(context.Background(), "q", now, now.Add(-time.Hour),
		time.Minute); !errors.Is(err, ErrRejected) {
		t.Fatalf("end before start: err = %v", err)
	}
}

// PromSource substitutes $window with a Prometheus duration and maps
// connector failures onto unknown reasons.
func TestPromSource_Window(t *testing.T) {
	o := appObjective()
	o.Source = SourcePrometheus
	o.BadQuery = `sum(increase(errors[$window]))`
	o.EligibleQuery = `sum(increase(requests[$window]))`
	o.ResetsQuery = `sum(resets(requests[$window]))`
	stub := &promStub{answer: func(_, q string) (int, string) {
		switch {
		case strings.HasPrefix(q, "sum(increase(errors"):
			return 200, vector("15")
		case strings.HasPrefix(q, "sum(resets"):
			return 200, vector("1")
		}
		return 200, vector("10000")
	}}
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	w := src.Window(context.Background(), o, 72*time.Hour, now)
	if w.Unknown != "" || w.Bad != 15 || w.Eligible != 10000 || w.Resets != 1 ||
		w.Duration != 72*time.Hour || w.Coverage != 1 {
		t.Fatalf("window = %+v", w)
	}
	joined := strings.Join(stub.queries, "\n")
	for _, want := range []string{"errors[3d]", "requests[3d]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("queries %q lack %s", joined, want)
		}
	}
}

func TestPromSource_UnknownMapping(t *testing.T) {
	o := appObjective()
	o.Source = SourcePrometheus
	o.BadQuery = `bad[$window]`
	o.EligibleQuery = `eligible[$window]`
	cases := map[string]struct {
		answer func(string, string) (int, string)
		want   string
	}{
		"bad absent": {func(_, q string) (int, string) {
			if strings.HasPrefix(q, "bad") {
				return 200, vector()
			}
			return 200, vector("1000")
		}, ReasonNoData},
		"eligible ambiguous": {func(_, q string) (int, string) {
			if strings.HasPrefix(q, "eligible") {
				return 200, vector("1", "2")
			}
			return 200, vector("0")
		}, ReasonAmbiguous},
		"server down": {func(string, string) (int, string) { return 502, "" },
			ReasonSourceError},
		"invalid": {func(string, string) (int, string) { return 200, vector("NaN") },
			ReasonInvalidValue},
		"low traffic": {func(_, q string) (int, string) {
			if strings.HasPrefix(q, "bad") {
				return 200, vector("0")
			}
			return 200, vector("10")
		}, ReasonLowTraffic},
	}
	for name, c := range cases {
		stub := &promStub{answer: c.answer}
		src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
		if w := src.Window(context.Background(), o, time.Hour, now); w.Unknown != c.want {
			t.Errorf("%s: unknown = %q, want %q", name, w.Unknown, c.want)
		}
	}
	var none PromSource
	if w := none.Window(context.Background(), o, time.Hour, now); w.Unknown !=
		ReasonConnectorNotConfigured {
		t.Fatalf("no client: %+v", w)
	}
}

// Recovery slices from query_range: one slice per step, a missing or NaN
// point is an unknown slice.
func TestPromSource_Slices(t *testing.T) {
	o := appObjective()
	o.Source = SourcePrometheus
	o.BadQuery = `bad[$window]`
	o.EligibleQuery = `eligible[$window]`
	stub := &promStub{answer: func(_, q string) (int, string) {
		vals := `[[1759320000,"1000"],[1759320120,"1000"],[1759320240,"1000"]]`
		if strings.HasPrefix(q, "bad") {
			vals = `[[1759320000,"0"],[1759320240,"NaN"]]`
		}
		return 200, `{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{},"values":` + vals + `}]}}`
	}}
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	got := src.Slices(context.Background(), o, time.Unix(1759319880, 0), time.Unix(1759320240, 0),
		2*time.Minute)
	if len(got) != 3 || got[0].Unknown != "" || got[0].Eligible != 1000 ||
		got[1].Unknown != ReasonNoData || got[2].Unknown != ReasonInvalidValue {
		t.Fatalf("slices = %+v", got)
	}
	if !strings.Contains(strings.Join(stub.queries, " "), "bad[2m]") {
		t.Fatalf("queries = %v", stub.queries)
	}
}
