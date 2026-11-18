package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guard-01b: `pg_sage_sidecar pressure` asks a running sidecar's
// check_pressure route and turns the verdict into an exit code a job can
// branch on between batches: 0 proceed, 10 slow, 20 stop, 2 usage and 3
// no answer (which a job must treat as stop).

type pressureServer struct {
	srv      *httptest.Server
	verdicts map[string]map[string]any
	status   int
	gotAuth  string
	gotQuery string
	gotPath  string
}

func newPressureServer(t *testing.T) *pressureServer {
	t.Helper()
	ps := &pressureServer{status: http.StatusOK, verdicts: map[string]map[string]any{
		"public.idle": {"verdict": "proceed", "batch_factor": 1, "suggested_batch": 1000,
			"sleep_seconds": 0, "reasons": []string{}},
		"public.busy": {"verdict": "slow", "batch_factor": 0.5, "suggested_batch": 500,
			"sleep_seconds": 4.5, "reasons": []string{"wal_rate 40 MiB/s >= slow 32 MiB/s"}},
		"public.hot": {"verdict": "stop", "batch_factor": 0, "sleep_seconds": 30,
			"reasons": []string{"replica_lag 16.8 s >= stop 15 s"}},
	}}
	ps.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		ps.gotAuth, ps.gotQuery, ps.gotPath = r.Header.Get("Authorization"), r.URL.RawQuery,
			r.URL.Path
		if ps.status != http.StatusOK {
			w.WriteHeader(ps.status)
			_, _ = w.Write([]byte(`{"error":"table \"public.nope\" not found in orders",` +
				`"code":"table_not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(ps.verdicts[r.URL.Query().Get("table")])
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *pressureServer) env(name string) string {
	switch name {
	case "SAGE_API_URL":
		return ps.srv.URL
	case "SAGE_API_TOKEN":
		return "sage_test_token"
	}
	return ""
}

func runPressure(t *testing.T, getenv func(string) string, args ...string) (int, string,
	string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runPressureCommand(context.Background(), args, getenv, nil, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPressureCmd_ExitCodePerVerdict(t *testing.T) {
	ps := newPressureServer(t)
	cases := []struct {
		table string
		code  int
		line  string
	}{
		{"public.idle", pressureExitProceed, "proceed batch=1000 factor=1 sleep=0"},
		{"public.busy", pressureExitSlow, "slow batch=500 factor=0.5 sleep=4.5"},
		{"public.hot", pressureExitStop, "stop batch=0 factor=0 sleep=30"},
	}
	for _, tc := range cases {
		code, out, errOut := runPressure(t, ps.env, "--database", "orders", "--table",
			tc.table, "--batch", "1000", "--job", "backfill_orders")
		if code != tc.code || strings.TrimSpace(out) != tc.line {
			t.Errorf("%s: exit %d %q (stderr %q), want %d %q", tc.table, code, out, errOut,
				tc.code, tc.line)
		}
	}
	if ps.gotAuth != "Bearer sage_test_token" || ps.gotPath != "/api/v1/databases/orders/pressure" {
		t.Fatalf("request auth %q path %q", ps.gotAuth, ps.gotPath)
	}
	for _, want := range []string{"table=public.hot", "batch=1000", "job=backfill_orders"} {
		if !strings.Contains(ps.gotQuery, want) {
			t.Fatalf("query %q lacks %q", ps.gotQuery, want)
		}
	}
}

// The reasons go to stderr, so stdout stays one parseable line.
func TestPressureCmd_ReasonsOnStderr(t *testing.T) {
	ps := newPressureServer(t)
	_, out, errOut := runPressure(t, ps.env, "--database", "orders", "--table", "public.hot")
	if strings.Count(out, "\n") != 1 || !strings.Contains(errOut, "replica_lag 16.8 s") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
}

func TestPressureCmd_JSONPrintsTheAnswer(t *testing.T) {
	ps := newPressureServer(t)
	code, out, _ := runPressure(t, ps.env, "--database", "orders", "--table", "public.busy",
		"--json")
	var v map[string]any
	if code != pressureExitSlow || json.Unmarshal([]byte(out), &v) != nil ||
		v["verdict"] != "slow" || v["suggested_batch"] != float64(500) {
		t.Fatalf("--json exit %d out %q", code, out)
	}
}

// A database or table name with URL metacharacters is escaped, not spliced.
func TestPressureCmd_EscapesNames(t *testing.T) {
	ps := newPressureServer(t)
	runPressure(t, ps.env, "--database", "orders", "--table", `public."a&b=c"`)
	if !strings.Contains(ps.gotQuery, "table=public.%22a%26b%3Dc%22") {
		t.Fatalf("query %q does not escape the table name", ps.gotQuery)
	}
	code, _, errOut := runPressure(t, ps.env, "--database", "a/b")
	if code != pressureExitUsage || !strings.Contains(errOut, "--database") {
		t.Fatalf("database with a slash = %d %q, want a usage error", code, errOut)
	}
}

func TestPressureCmd_UsageErrors(t *testing.T) {
	ps := newPressureServer(t)
	cases := map[string][]string{
		"missing --database": {"--table", "public.idle"},
		"empty --database":   {"--database", " "},
		"negative --batch":   {"--database", "orders", "--batch", "-1"},
		"unknown flag":       {"--database", "orders", "--force"},
		"stray argument":     {"--database", "orders", "extra"},
	}
	for name, args := range cases {
		code, out, errOut := runPressure(t, ps.env, args...)
		if code != pressureExitUsage || out != "" || !strings.Contains(errOut, "usage:") {
			t.Errorf("%s: exit %d out %q stderr %q, want 2 with usage", name, code, out, errOut)
		}
	}
	noURL := func(name string) string {
		if name == "SAGE_API_TOKEN" {
			return "tok"
		}
		return ""
	}
	if code, _, errOut := runPressure(t, noURL, "--database", "orders"); code !=
		pressureExitUsage || !strings.Contains(errOut, "SAGE_API_URL") {
		t.Fatalf("no SAGE_API_URL = %d %q", code, errOut)
	}
	noToken := func(name string) string {
		if name == "SAGE_API_URL" {
			return ps.srv.URL
		}
		return ""
	}
	if code, _, errOut := runPressure(t, noToken, "--database", "orders"); code !=
		pressureExitUsage || !strings.Contains(errOut, "SAGE_API_TOKEN") {
		t.Fatalf("no SAGE_API_TOKEN = %d %q", code, errOut)
	}
	plainRemote := func(name string) string {
		if name == "SAGE_API_URL" {
			return "http://sage.example.com:8080"
		}
		return "tok"
	}
	if code, _, errOut := runPressure(t, plainRemote, "--database", "orders"); code !=
		pressureExitUsage || !strings.Contains(errOut, "https") {
		t.Fatalf("plain http to a remote host = %d %q", code, errOut)
	}
}

// Error propagation: an unreachable sidecar exits 3 naming its address.
func TestPressureCmd_UnreachableSidecarExits3WithTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	env := func(name string) string {
		if name == "SAGE_API_URL" {
			return "http://" + addr
		}
		return "tok"
	}
	code, out, errOut := runPressure(t, env, "--database", "orders")
	if code != pressureExitNoAnswer || out != "" || !strings.Contains(errOut, addr) ||
		!strings.Contains(errOut, "stop") {
		t.Fatalf("unreachable = %d out %q stderr %q, want 3 naming %s", code, out, errOut, addr)
	}
}

// A refusal (unknown table, bad token, unreadable inputs) is no answer:
// exit 3 with the sidecar's message, never a verdict.
func TestPressureCmd_RefusalIsNoAnswer(t *testing.T) {
	ps := newPressureServer(t)
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized,
		http.StatusServiceUnavailable} {
		ps.status = status
		code, out, errOut := runPressure(t, ps.env, "--database", "orders", "--table",
			"public.nope")
		if code != pressureExitNoAnswer || out != "" ||
			!strings.Contains(errOut, `public.nope`) {
			t.Errorf("HTTP %d = %d out %q stderr %q, want 3", status, code, out, errOut)
		}
	}
}

// A 200 whose verdict is missing or unknown is no answer either.
func TestPressureCmd_MalformedAnswerIsNoAnswer(t *testing.T) {
	ps := newPressureServer(t)
	ps.verdicts["public.odd"] = map[string]any{"verdict": "maybe"}
	for _, table := range []string{"public.odd", "public.absent"} {
		code, out, errOut := runPressure(t, ps.env, "--database", "orders", "--table", table)
		if code != pressureExitNoAnswer || out != "" || errOut == "" {
			t.Errorf("%s = %d out %q stderr %q, want 3", table, code, out, errOut)
		}
	}
}
