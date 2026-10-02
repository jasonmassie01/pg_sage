package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/signed"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// Sage SRE M5 routes (AI-SRE-SPEC §8/§9): signed change-event and SLI
// ingestion (HMAC + timestamp tolerance + replay protection, no session),
// and the SLO, change-feed reads for signed-in roles.

const (
	ceSecret  = "change-events-secret-0123456789abcdef"
	sliSecret = "sli-push-secret-0123456789abcdef0123"
)

type signalDB struct {
	name  string
	scope sre.Scope
	feed  *changefeed.Feed
	slo   *slo.Engine
}

func signalInstance(t *testing.T, mgr *fleet.DatabaseManager, name string) signalDB {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: lockChainRunner{},
		Config: sre.DefaultCoordinatorConfig(fmt.Sprintf("api-signal:%s:%d", name,
			time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scopeFn := func(context.Context) (sre.Scope, error) { return scope, nil }
	cs, err := changefeed.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := slo.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	obj := slo.Objective{Name: "checkout-" + name, Kind: slo.KindApp, Source: slo.SourcePush,
		Target: 0.999, Window: 30 * 24 * time.Hour, MinEligible: 50,
		StaleAfter: 5 * time.Minute}
	eng, err := slo.NewEngine(slo.EngineDeps{Database: name, Objectives: []slo.Objective{obj},
		Store: ss, Scope: scopeFn, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	db := signalDB{name: name, scope: scope, feed: changefeed.NewFeed(cs, name, scopeFn),
		slo: eng}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st),
		SLO: eng, Changes: db.feed})
	return db
}

func signalConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.SRE.ChangeEvents.HMACSecret = ceSecret
	cfg.SRE.ChangeEvents.AllowedSources = []string{"github-actions", "argo"}
	cfg.SRE.SLO.Push.HMACSecret = sliSecret
	return cfg
}

func signalRouter(t *testing.T, mgr *fleet.DatabaseManager, cfg *config.Config,
	user *auth.User) http.Handler {
	t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(mgr, cfg, nil, nil, nil, nil, nil, inject)
}

type signedCall struct {
	secret string
	path   string
	body   any
	at     time.Time
	tamper func(r *http.Request)
}

func (c signedCall) do(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(c.body)
	if err != nil {
		t.Fatal(err)
	}
	at := c.at
	if at.IsZero() {
		at = time.Now()
	}
	ts, sig := signed.Sign([]byte(c.secret), "POST", c.path, at, raw)
	req := httptest.NewRequest("POST", c.path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signed.HeaderTimestamp, ts)
	req.Header.Set(signed.HeaderSignature, sig)
	if c.tamper != nil {
		c.tamper(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func changeBody(database, eventID string) map[string]any {
	return map[string]any{"source": "github-actions", "event_id": eventID, "kind": "deploy",
		"database": database, "service": "checkout", "summary": "deploy checkout v1.2.3",
		"occurred_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)}
}

func TestShouldSkipAuth_SignedIngestionOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/sre/change-events":   true,
		"/api/v1/sre/sli/checkout":    true,
		"/api/v1/sre/sli/":            false,
		"/api/v1/sre/sli/a/b":         false,
		"/api/v1/sre/slos":            false,
		"/api/v1/sre/changes":         false,
		"/api/v1/sre/change-events/x": false,
	} {
		if got := shouldSkipAuth(path); got != want {
			t.Errorf("shouldSkipAuth(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestChangeEventIngestion(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := signalInstance(t, mgr, "orders")
	h := signalRouter(t, mgr, signalConfig(), nil)
	id := "run-" + string(sre.NewUUID())
	call := signedCall{secret: ceSecret, path: "/api/v1/sre/change-events",
		body: changeBody("orders", id)}
	code, out := call.do(t, h)
	if code != http.StatusAccepted || out["duplicate"] != false || out["id"] == "" {
		t.Fatalf("ingest = %d %v", code, out)
	}
	code, out = call.do(t, h)
	if code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("replay = %d %v, want 200 duplicate", code, out)
	}
	evs, err := orders.feed.Recent(context.Background(), time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.EventID == id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d stored rows for one event", n)
	}
	other := changeBody("orders", id)
	other["summary"] = "something else"
	if code, out := (signedCall{secret: ceSecret, path: call.path, body: other}).do(t,
		h); code != http.StatusConflict || out["code"] != "conflict" {
		t.Fatalf("same id other content = %d %v", code, out)
	}
}

func TestChangeEventIngestion_Refusals(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	signalInstance(t, mgr, "orders")
	h := signalRouter(t, mgr, signalConfig(), nil)
	path := "/api/v1/sre/change-events"
	fresh := func() map[string]any { return changeBody("orders", "r-"+string(sre.NewUUID())) }
	badSource := fresh()
	badSource["source"] = "jenkins"
	invalid := fresh()
	invalid["kind"] = "restart"
	cases := map[string]struct {
		call     signedCall
		wantCode int
		wantErr  string
	}{
		"tampered": {signedCall{secret: ceSecret, path: path, body: fresh(),
			tamper: func(r *http.Request) { r.Header.Set(signed.HeaderSignature, "v1="+strings.Repeat("a", 64)) }},
			401, "invalid_signature"},
		"wrong secret": {signedCall{secret: sliSecret, path: path, body: fresh()}, 401,
			"invalid_signature"},
		"stale": {signedCall{secret: ceSecret, path: path, body: fresh(),
			at: time.Now().Add(-10 * time.Minute)}, 401, "stale_timestamp"},
		"unsigned": {signedCall{secret: ceSecret, path: path, body: fresh(),
			tamper: func(r *http.Request) { r.Header.Del(signed.HeaderSignature) }},
			401, "missing_signature"},
		"unknown db": {signedCall{secret: ceSecret, path: path,
			body: changeBody("nope", "x-1")}, 404, "not_found"},
		"not allowed": {signedCall{secret: ceSecret, path: path, body: badSource}, 403,
			"source_not_allowed"},
		"internal kind": {signedCall{secret: ceSecret, path: path, body: invalid}, 400,
			"invalid_request"},
		"not json": {signedCall{secret: ceSecret, path: path, body: "just a string"}, 400,
			"invalid_request"},
	}
	for name, c := range cases {
		code, out := c.call.do(t, h)
		if code != c.wantCode || out["code"] != c.wantErr {
			t.Errorf("%s: %d %v, want %d %s", name, code, out, c.wantCode, c.wantErr)
		}
	}
	unconfigured := signalRouter(t, mgr, config.DefaultConfig(), nil)
	code, out := (signedCall{secret: "", path: path, body: fresh()}).do(t, unconfigured)
	if code != http.StatusServiceUnavailable || out["code"] != "not_configured" {
		t.Fatalf("no secret configured = %d %v", code, out)
	}
}

// A deployment-wide event (no database) reaches every database's feed,
// stored once per store.
func TestChangeEventIngestion_DeploymentWide(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := signalInstance(t, mgr, "orders")
	billing := signalInstance(t, mgr, "billing")
	h := signalRouter(t, mgr, signalConfig(), nil)
	id := "wide-" + string(sre.NewUUID())
	code, out := (signedCall{secret: ceSecret, path: "/api/v1/sre/change-events",
		body: changeBody("", id)}).do(t, h)
	if code != http.StatusAccepted {
		t.Fatalf("ingest = %d %v", code, out)
	}
	for _, db := range []signalDB{orders, billing} {
		evs, _ := db.feed.Recent(context.Background(), time.Hour, 100)
		n := 0
		for _, e := range evs {
			if e.EventID == id {
				n++
				if e.Scoped {
					t.Fatalf("%s: deployment-wide event stored as scoped", db.name)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s sees the event %d times", db.name, n)
		}
	}
}

func TestSLIPush(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := signalInstance(t, mgr, "orders")
	h := signalRouter(t, mgr, signalConfig(), nil)
	path := "/api/v1/sre/sli/checkout-orders"
	body := map[string]any{"series": "pod-a", "bad": 2, "eligible": 1000,
		"observed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	call := signedCall{secret: sliSecret, path: path, body: body}
	code, out := call.do(t, h)
	if code != http.StatusAccepted || out["accepted"] != float64(1) {
		t.Fatalf("push = %d %v", code, out)
	}
	if code, out := call.do(t, h); code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("replayed push = %d %v", code, out)
	}
	if !orders.slo.HasPush("checkout-orders") {
		t.Fatal("fixture SLO is not a push SLO")
	}
	cases := map[string]struct {
		call     signedCall
		wantCode int
		wantErr  string
	}{
		"unknown slo": {signedCall{secret: sliSecret, path: "/api/v1/sre/sli/nope",
			body: body}, 404, "not_found"},
		"change secret": {signedCall{secret: ceSecret, path: path, body: body}, 401,
			"invalid_signature"},
		"invalid sample": {signedCall{secret: sliSecret, path: path,
			body: map[string]any{"bad": 5, "eligible": 1,
				"observed_at": time.Now().UTC().Format(time.RFC3339Nano)}}, 400,
			"invalid_request"},
	}
	for name, c := range cases {
		code, out := c.call.do(t, h)
		if code != c.wantCode || out["code"] != c.wantErr {
			t.Errorf("%s: %d %v, want %d %s", name, code, out, c.wantCode, c.wantErr)
		}
	}
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestSLOAndChangeReads(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := signalInstance(t, mgr, "orders")
	signalInstance(t, mgr, "billing")
	if _, err := orders.slo.EvaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := signalRouter(t, mgr, signalConfig(), testViewerUser())
	code, out := getJSON(t, h, "/api/v1/sre/slos")
	items, _ := out["slos"].([]any)
	if code != 200 || len(items) < 1 {
		t.Fatalf("slo list = %d %v", code, out)
	}
	first, _ := items[0].(map[string]any)
	if first["database"] == "" || first["state"] == "" || first["kind"] != "app" {
		t.Fatalf("slo item = %v", first)
	}
	code, out = getJSON(t, h, "/api/v1/sre/slos?database=orders")
	items, _ = out["slos"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("orders slos = %d %v", code, out)
	}
	code, out = getJSON(t, h, "/api/v1/sre/slos/checkout-orders?database=orders&since="+
		time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339))
	if code != 200 || out["status"] == nil || out["recovery"] == nil ||
		out["transitions"] == nil {
		t.Fatalf("slo detail = %d %v", code, out)
	}
	if code, _ := getJSON(t, h, "/api/v1/sre/slos/nope?database=orders"); code != 404 {
		t.Fatalf("unknown slo = %d", code)
	}
	if code, _ := getJSON(t, h, "/api/v1/sre/slos/checkout-orders?database=orders&since=bad"); code != 400 {
		t.Fatalf("bad since = %d", code)
	}
	code, out = getJSON(t, h, "/api/v1/sre/changes?database=orders&window_minutes=60")
	if code != 200 || out["changes"] == nil {
		t.Fatalf("changes = %d %v", code, out)
	}
	if code, _ := getJSON(t, h, "/api/v1/sre/changes?database=orders&window_minutes=0"); code != 400 {
		t.Fatalf("zero window = %d", code)
	}
	anon := signalRouter(t, mgr, signalConfig(), nil)
	for _, p := range []string{"/api/v1/sre/slos", "/api/v1/sre/changes?database=orders"} {
		if code, _ := getJSON(t, anon, p); code != http.StatusUnauthorized {
			t.Errorf("%s without a user = %d", p, code)
		}
	}
}
