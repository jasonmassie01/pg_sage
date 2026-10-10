package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func probeReady(t *testing.T, h http.Handler, method string) (int, readyBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/ready", nil))
	var body readyBody
	if method != http.MethodHead && rec.Code != http.StatusMethodNotAllowed {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	return rec.Code, body
}

func okProbe() ReadinessProbe {
	return ReadinessProbe{
		ConfigLoaded: true,
		ControlDB:    func(context.Context) error { return nil },
		Schema:       func(context.Context) error { return nil },
		Timeout:      time.Second,
	}
}

func TestReadiness_AllChecksPassIs200(t *testing.T) {
	code, body := probeReady(t, NewReadinessHandler(okProbe()), http.MethodGet)
	if code != http.StatusOK || body.Status != "ready" ||
		body.Checks["config"] != "ok" || body.Checks["control_db"] != "ok" ||
		body.Checks["schema"] != "ok" {
		t.Fatalf("code %d body %+v", code, body)
	}
	if code, _ := probeReady(t, NewReadinessHandler(okProbe()), http.MethodHead); code !=
		http.StatusOK {
		t.Fatalf("HEAD = %d", code)
	}
}

func TestReadiness_EachFailingCheckIs503AndNamed(t *testing.T) {
	boom := func(context.Context) error {
		return errors.New("dial tcp: password authentication failed for user sage pw=hunter2")
	}
	cases := map[string]struct {
		probe ReadinessProbe
		check string
		want  string
	}{
		"config": {func() ReadinessProbe { p := okProbe(); p.ConfigLoaded = false; return p }(),
			"config", "not_loaded"},
		"no control db": {func() ReadinessProbe { p := okProbe(); p.ControlDB = nil; return p }(),
			"control_db", "absent"},
		"control db down": {func() ReadinessProbe {
			p := okProbe()
			p.ControlDB = boom
			return p
		}(),
			"control_db", "unreachable"},
		"schema missing": {func() ReadinessProbe {
			p := okProbe()
			p.Schema = func(context.Context) error {
				return schema.ErrSchemaNotReady
			}
			return p
		}(), "schema", "not_migrated"},
		"schema query failed": {func() ReadinessProbe {
			p := okProbe()
			p.Schema = boom
			return p
		}(),
			"schema", "unknown"},
	}
	for name, tc := range cases {
		rec := httptest.NewRecorder()
		NewReadinessHandler(tc.probe).ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: code = %d", name, rec.Code)
		}
		var body readyBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Status != "not_ready" || body.Checks[tc.check] != tc.want {
			t.Errorf("%s: body = %+v, want %s=%s", name, body, tc.check, tc.want)
		}
		if strings.Contains(rec.Body.String(), "hunter2") ||
			strings.Contains(rec.Body.String(), "dial tcp") {
			t.Errorf("%s: unauthenticated body leaks the error: %s", name, rec.Body.String())
		}
	}
}

// The schema check is skipped (reported "skipped") while the control
// database is down: it could only time out a second time.
func TestReadiness_SchemaNotQueriedWhenControlDBDown(t *testing.T) {
	p := okProbe()
	p.ControlDB = func(context.Context) error { return errors.New("down") }
	var schemaCalls atomic.Int32
	p.Schema = func(context.Context) error { schemaCalls.Add(1); return nil }
	code, body := probeReady(t, NewReadinessHandler(p), http.MethodGet)
	if code != http.StatusServiceUnavailable || body.Checks["schema"] != "skipped" ||
		schemaCalls.Load() != 0 {
		t.Fatalf("code %d body %+v schema calls %d", code, body, schemaCalls.Load())
	}
}

func TestReadiness_HungControlDBTimesOut(t *testing.T) {
	p := okProbe()
	p.Timeout = 50 * time.Millisecond
	p.ControlDB = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	code, body := probeReady(t, NewReadinessHandler(p), http.MethodGet)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %s", elapsed)
	}
	if code != http.StatusServiceUnavailable || body.Checks["control_db"] != "unreachable" {
		t.Fatalf("code %d body %+v", code, body)
	}
}

func TestReadiness_ZeroTimeoutUsesDefault(t *testing.T) {
	p := okProbe()
	p.Timeout = 0
	var deadline time.Duration
	p.ControlDB = func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			return errors.New("no deadline")
		}
		deadline = time.Until(d)
		return nil
	}
	if code, _ := probeReady(t, NewReadinessHandler(p), http.MethodGet); code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if deadline <= 0 || deadline > defaultReadinessTimeout {
		t.Fatalf("deadline %s, want within the %s default", deadline, defaultReadinessTimeout)
	}
}

func TestReadiness_RejectsWriteMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	NewReadinessHandler(okProbe()).ServeHTTP(rec, httptest.NewRequest("POST", "/ready", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestReadiness_RecoversWhenDependencyComesBack(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	p := okProbe()
	p.ControlDB = func(context.Context) error {
		if down.Load() {
			return errors.New("down")
		}
		return nil
	}
	h := NewReadinessHandler(p)
	if code, _ := probeReady(t, h, http.MethodGet); code != http.StatusServiceUnavailable {
		t.Fatalf("down: %d", code)
	}
	down.Store(false)
	if code, _ := probeReady(t, h, http.MethodGet); code != http.StatusOK {
		t.Fatalf("recovered: %d", code)
	}
}

func TestReadiness_ConcurrentProbes(t *testing.T) {
	var calls atomic.Int32
	p := okProbe()
	p.ControlDB = func(context.Context) error { calls.Add(1); return nil }
	h := NewReadinessHandler(p)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
			if rec.Code != http.StatusOK {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || calls.Load() != 50 {
		t.Fatalf("bad %d, calls %d", bad.Load(), calls.Load())
	}
}

// The router serves /ready without a session, next to /health.
func TestRouter_ReadyIsRegisteredAndUnauthenticated(t *testing.T) {
	h := NewRouterFullRuntime(nil, &config.Config{}, nil, nil, nil, nil, nil,
		SessionAuthMiddleware(nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), `"control_db":"absent"`) {
		t.Fatalf("/ready without a control pool = %d %s", rec.Code, rec.Body.String())
	}
}

// Integration: a real control database moves /ready from 503 to 200 once
// the schema is bootstrapped.
func TestReadyProbe_RealControlDatabase(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "api_ready")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	h := NewReadinessHandler(ControlPoolReadiness(&config.Config{}, pool))
	code, body := probeReady(t, h, http.MethodGet)
	if code != http.StatusServiceUnavailable || body.Checks["control_db"] != "ok" ||
		body.Checks["schema"] != "not_migrated" {
		t.Fatalf("before bootstrap: %d %+v", code, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if code, body := probeReady(t, h, http.MethodGet); code != http.StatusOK {
		t.Fatalf("after bootstrap: %d %+v", code, body)
	}
}
