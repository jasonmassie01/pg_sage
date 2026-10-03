package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/gameday"
)

// Roadmap 1.1 (2026-10-03): "Run bench locally" from the promotion coach.
// GET /sre/autonomy/bench-runs says whether a disposable target exists
// and how the last local run went; POST starts one on a clone for the
// families asked (admin only, like a game day or a bench upload). Its
// report counts as bench evidence for the families it covered, marked
// "local run".

type benchClone struct{}

func (benchClone) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	return clone.Clone{ID: "c1", DSN: "postgres://x:secret@clone:5432/orders"}, nil
}
func (benchClone) Destroy(context.Context, clone.Clone) error         { return nil }
func (benchClone) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

type benchFaults struct {
	mu       sync.Mutex
	families [][]string
	release  chan struct{}
}

func (f *benchFaults) Run(ctx context.Context, _ string, families []string) ([]byte, error) {
	f.mu.Lock()
	f.families = append(f.families, families)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	return []byte(`{}`), nil
}

type benchLedger struct{}

func (benchLedger) IngestBench(context.Context, []byte, earned.BenchIngest) (earned.EvalRun,
	error) {
	return earned.EvalRun{ID: "eval-local", Origin: earned.OriginLocalRun}, nil
}

func (f *autonomyAPIFixture) benchRouter(user *auth.User,
	benches *gameday.BenchRegistry) http.Handler {
	f.t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(f.mgr, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{Autonomy: &AutonomyDeps{Ledgers: f.reg, LocalBench: benches}}, inject)
}

func localBenches(t *testing.T, faults *benchFaults) *gameday.BenchRegistry {
	t.Helper()
	b, err := gameday.NewLocalBench(gameday.LocalBenchConfig{Database: "orders",
		Provider: "dle"}, benchClone{}, faults, benchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	reg := gameday.NewBenchRegistry()
	reg.Register("orders", b)
	return reg
}

func waitIdle(t *testing.T, reg *gameday.BenchRegistry) {
	t.Helper()
	b, _ := reg.Lookup("orders")
	deadline := time.Now().Add(5 * time.Second)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Running() {
		t.Fatal("the local bench run did not finish")
	}
}

func TestBenchRunsAPI_StartAndStatus(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	faults := &benchFaults{}
	reg := localBenches(t, faults)
	admin := f.benchRouter(testAdminUser(), reg)
	code, body := autonomyCall(t, admin, "GET",
		"/api/v1/sre/autonomy/bench-runs?database=orders", "")
	if code != 200 || body["enabled"] != true || body["provider"] != "dle" ||
		body["running"] != false || body["last"] != nil {
		t.Fatalf("status before a run = %d %v", code, body)
	}
	code, body = autonomyCall(t, admin, "POST",
		"/api/v1/sre/autonomy/bench-runs?database=orders", `{"families":["lock_blocking"]}`)
	run, _ := body["run"].(map[string]any)
	if code != http.StatusAccepted || run["status"] != "running" || run["id"] == "" {
		t.Fatalf("start = %d %v", code, body)
	}
	waitIdle(t, reg)
	code, body = autonomyCall(t, f.benchRouter(testViewerUser(), reg), "GET",
		"/api/v1/sre/autonomy/bench-runs?database=orders", "")
	last, _ := body["last"].(map[string]any)
	if code != 200 || last["status"] != "completed" || last["eval_run_id"] != "eval-local" ||
		last["id"] != run["id"] {
		t.Fatalf("status after the run = %d %v", code, body)
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(last)), "secret") {
		t.Fatal("the clone DSN leaked")
	}
	if len(faults.families) != 1 || strings.Join(faults.families[0], ",") != "lock_blocking" {
		t.Fatalf("families run = %v", faults.families)
	}
}

func TestBenchRunsAPI_Refusals(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	faults := &benchFaults{release: make(chan struct{})}
	reg := localBenches(t, faults)
	admin := f.benchRouter(testAdminUser(), reg)
	post := "/api/v1/sre/autonomy/bench-runs?database=orders"
	cases := []struct {
		name   string
		router http.Handler
		path   string
		body   string
		status int
		code   string
	}{
		{"an operator", f.benchRouter(testOperatorUser(), reg), post, `{}`, 403, ""},
		{"a viewer", f.benchRouter(testViewerUser(), reg), post, `{}`, 403, ""},
		{"an unknown family", admin, post, `{"families":["shell"]}`, 400, "invalid_request"},
		{"an unknown field", admin, post, `{"dsn":"postgres://prod"}`, 400, "invalid_request"},
		{"an unknown database", admin,
			"/api/v1/sre/autonomy/bench-runs?database=nope", `{}`, 404, "not_found"},
	}
	for _, c := range cases {
		code, body := autonomyCall(t, c.router, "POST", c.path, c.body)
		if code != c.status || (c.code != "" && body["code"] != c.code) {
			t.Errorf("%s: %d %v, want %d %s", c.name, code, body, c.status, c.code)
		}
	}
	if len(faults.families) != 0 {
		t.Fatalf("a refused request ran fault programs: %v", faults.families)
	}
	if code, _ := autonomyCall(t, admin, "POST", post, `{}`); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	code, body := autonomyCall(t, admin, "POST", post, `{}`)
	if code != http.StatusConflict || body["code"] != "running" {
		t.Fatalf("second start = %d %v, want 409 running", code, body)
	}
	if _, status := autonomyCall(t, admin, "GET", post, ""); status["running"] != true {
		t.Fatalf("status while running = %v", status)
	}
	close(faults.release)
	waitIdle(t, reg)
}

func TestBenchRunsAPI_NotConfigured(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	for name, reg := range map[string]*gameday.BenchRegistry{"no registry": nil,
		"no target for this database": gameday.NewBenchRegistry()} {
		admin := f.benchRouter(testAdminUser(), reg)
		code, body := autonomyCall(t, admin, "GET",
			"/api/v1/sre/autonomy/bench-runs?database=orders", "")
		how, _ := body["how"].(string)
		if code != 200 || body["enabled"] != false || !strings.Contains(how, "clone.provider") ||
			!strings.Contains(how, "local_dsn") {
			t.Errorf("%s: status = %d %v", name, code, body)
		}
		code, body = autonomyCall(t, admin, "POST",
			"/api/v1/sre/autonomy/bench-runs?database=orders", `{}`)
		if code != http.StatusConflict || body["code"] != "not_configured" {
			t.Errorf("%s: start = %d %v, want 409 not_configured", name, code, body)
		}
	}
}

// The view carries each family's bench provenance for the coach.
func TestAutonomyAPI_ViewShowsBenchProvenance(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	if _, err := f.ledger.IngestEvalRun(context.Background(), []byte(`{"schema":
		"pg_sage.pgincidentbench.v1", "generated_at": "`+
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)+`", "gated_arms":
		["causal-graph"], "cells": [{"arm": "causal-graph", "family": "lock_blocking",
		"runs": 12, "safe_pass": {"k": 12, "n": 12}, "top1": {"k": 11, "n": 12},
		"mechanism_precision": 0.95, "forbidden_actions": 0}]}`), earned.SourceBench,
		"user:1:admin@test.com", ""); err != nil {
		t.Fatal(err)
	}
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET",
		"/api/v1/sre/autonomy?database=orders", "")
	view, _ := body["view"].(map[string]any)
	families, _ := view["families"].([]any)
	var lock map[string]any
	for _, fam := range families {
		if m := fam.(map[string]any); m["family"] == "lock_blocking" {
			lock = m
		}
	}
	bench, _ := lock["bench"].(map[string]any)
	if code != 200 || bench["provenance"] != "unsigned (operator-provided)" ||
		bench["origin"] != "operator" || bench["signed"] != false {
		t.Fatalf("lock_blocking bench = %v (view %d)", bench, code)
	}
}
