package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// apiauth-30: characterization of the handlers moved out of handlers.go,
// handlers_v09.go, action_handlers.go, config_handlers.go and
// database_handlers.go. The golden files under testdata/handler_golden
// were written on the unsplit code; a pure move into named steps must
// reproduce every status, the headers that matter and every masked body.

var updateHandlerGolden = flag.Bool("update-handler-golden", false,
	"rewrite testdata/handler_golden/*.json from the current handlers")

const handlerGoldenDir = "testdata/handler_golden"

// goldenCall is one request: role "" sends no user.
type goldenCall struct {
	name, role, method, path, body string
}

type goldenRecord struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Request      string `json:"request"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type"`
	CacheControl string `json:"cache_control"`
	Body         any    `json:"body"`
}

var (
	goldenTimePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)
	goldenUUIDPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	goldenUUIDInPath = regexp.MustCompile(
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// goldenVolatileKeys hold values that change run to run (latency,
	// server build) whatever the handler does.
	goldenVolatileKeys = map[string]bool{
		"latency_ms": true, "duration_ms": true, "elapsed_ms": true,
		"version": true, "server_version": true, "pg_version": true,
		"last_resync_at": true,
	}
)

// maskVolatile replaces timestamps, UUIDs and volatile keys so a golden
// body compares across runs and servers.
func maskVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if goldenVolatileKeys[k] && val != nil {
				x[k] = "<volatile>"
				continue
			}
			x[k] = maskVolatile(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = maskVolatile(x[i])
		}
		return x
	case string:
		if goldenTimePattern.MatchString(x) {
			return "<time>"
		}
		if goldenUUIDPattern.MatchString(x) {
			return "<uuid>"
		}
		return x
	}
	return v
}

func serveGolden(h http.Handler, c goldenCall) goldenRecord {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var body *strings.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(c.method, c.path, body).WithContext(ctx)
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.role != "" {
		req.Header.Set("X-Char-Role", c.role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return goldenRecord{
		Name: c.name, Role: c.role,
		Request: c.method + " " + goldenUUIDInPath.ReplaceAllString(c.path, "<uuid>"),
		Status:  rec.Code, ContentType: rec.Header().Get("Content-Type"),
		CacheControl: rec.Header().Get("Cache-Control"),
		Body:         decodeGoldenBody(rec.Body.Bytes()),
	}
}

func decodeGoldenBody(raw []byte) any {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return maskVolatile(parsed)
}

// checkHandlerGolden compares recs with the golden file, or rewrites it
// under -update-handler-golden.
func checkHandlerGolden(t *testing.T, file string, recs []goldenRecord) {
	t.Helper()
	got, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		t.Fatalf("encode records: %v", err)
	}
	got = append(got, '\n')
	path := filepath.Join(handlerGoldenDir, file)
	if *updateHandlerGolden {
		if err := os.MkdirAll(handlerGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (write it with -update-handler-golden on the "+
			"unsplit handlers): %v", path, err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	diffHandlerGolden(t, file, string(want), string(got))
}

func diffHandlerGolden(t *testing.T, file, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	var wantRecs, gotRecs []goldenRecord
	_ = json.Unmarshal([]byte(want), &wantRecs)
	_ = json.Unmarshal([]byte(got), &gotRecs)
	if len(wantRecs) != len(gotRecs) {
		t.Errorf("%s: %d records, golden has %d", file, len(gotRecs), len(wantRecs))
	}
	for i := 0; i < len(wantRecs) && i < len(gotRecs); i++ {
		w, _ := json.Marshal(wantRecs[i])
		g, _ := json.Marshal(gotRecs[i])
		if !bytes.Equal(w, g) {
			t.Errorf("%s record %d (%s):\n got %s\nwant %s",
				file, i, wantRecs[i].Name, g, w)
		}
	}
}

func runGoldenCalls(h http.Handler, calls []goldenCall) []goldenRecord {
	recs := make([]goldenRecord, 0, len(calls))
	for _, c := range calls {
		recs = append(recs, serveGolden(h, c))
	}
	return recs
}

// closedFleetRouter is a fleet router whose databases sit on a closed
// pool: every handler that reads the database takes its error path.
func closedFleetRouter(t *testing.T, names ...string) (http.Handler, *fleet.DatabaseManager) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	pool := closedPool(t)
	for i, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, DatabaseID: i + 1, Pool: pool,
			Config:   config.DatabaseConfig{Name: name},
			Executor: executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {}),
			Status:   &fleet.InstanceStatus{Connected: true},
		})
	}
	actions := &ActionDeps{Fleet: mgr}
	dbDeps := &DatabaseDeps{Store: store.NewDatabaseStore(pool, p2Key), Fleet: mgr}
	return newTestRouter(mgr, cfg, pool, actions, dbDeps, nil, nil, injectCharRole), mgr
}

// Error propagation: a closed pool takes each handler's connection path.
func closedPoolCalls() []goldenCall {
	return []goldenCall{
		{"findings list closed", "viewer", "GET", "/api/v1/findings?database=primary", ""},
		{"findings stats closed", "viewer", "GET", "/api/v1/findings/stats?database=primary", ""},
		{"finding detail closed", "viewer", "GET", "/api/v1/findings/1?database=primary", ""},
		{"suppress named closed", "operator", "POST",
			"/api/v1/findings/1/suppress?database=primary", "{}"},
		{"suppress implicit closed", "operator", "POST", "/api/v1/findings/1/suppress", "{}"},
		{"unsuppress implicit closed", "operator", "POST", "/api/v1/findings/1/unsuppress", "{}"},
		{"actions list closed", "viewer", "GET", "/api/v1/actions?database=primary", ""},
		{"action detail closed", "viewer", "GET", "/api/v1/actions/1?database=primary", ""},
		{"snapshot latest closed", "viewer", "GET",
			"/api/v1/snapshots/latest?database=primary&metric=system", ""},
		{"snapshot history closed", "viewer", "GET",
			"/api/v1/snapshots/history?database=primary&metric=cache_hit_ratio", ""},
		{"fleet health closed", "viewer", "GET", "/api/v1/fleet/health?database=primary", ""},
		{"incidents closed", "viewer", "GET", "/api/v1/incidents?database=primary", ""},
		{"config db get closed", "admin", "GET", "/api/v1/config/databases/1", ""},
		{"pending by finding closed", "operator", "GET",
			"/api/v1/findings/1/pending-actions?database=primary", ""},
		{"managed list closed", "admin", "GET", "/api/v1/databases/managed", ""},
		{"managed test closed", "admin", "POST", "/api/v1/databases/managed/1/test", ""},
	}
}

// Invalid input: each pins today's status and error body.
func invalidInputCalls() []goldenCall {
	put := "/api/v1/config/databases/1"
	return []goldenCall{
		{"config put malformed json", "admin", "PUT", put, "{not json"},
		{"config put no generation", "admin", "PUT", put, `{"trust.level":"advisory"}`},
		{"config put unknown key", "admin", "PUT", put,
			`{"expected_generation":1,"no.such.key":1}`},
		{"config put global-only key", "admin", "PUT", put,
			`{"expected_generation":1,"collector.interval_seconds":30}`},
		{"config put bad exec mode", "admin", "PUT", put,
			`{"expected_generation":1,"execution_mode":"yolo"}`},
		{"config put zero generation", "admin", "PUT", put,
			`{"expected_generation":0,"trust.level":"advisory"}`},
		{"config put negative id", "admin", "PUT", "/api/v1/config/databases/-1",
			`{"expected_generation":1}`},
		{"config put injected id", "admin", "PUT", "/api/v1/config/databases/1;DROP",
			`{"expected_generation":1}`},
		{"config put unknown database", "admin", "PUT", "/api/v1/config/databases/99",
			`{"expected_generation":1}`},
		{"config put empty", "admin", "PUT", put, `{"expected_generation":3}`},
		{"config delete exec mode", "admin", "DELETE",
			"/api/v1/config/databases/1/execution_mode?expected_generation=1", ""},
		{"config delete trust", "admin", "DELETE",
			"/api/v1/config/databases/1/trust.level?expected_generation=1", ""},
		{"config delete unknown key", "admin", "DELETE",
			"/api/v1/config/databases/1/no.such.key?expected_generation=1", ""},
		{"config delete no generation", "admin", "DELETE",
			"/api/v1/config/databases/1/analyzer.disabled_rules", ""},
		{"config db get negative id", "admin", "GET", "/api/v1/config/databases/-1", ""},
	}
}

// requestInvalidInputCalls cover the non-config invalid requests.
func requestInvalidInputCalls() []goldenCall {
	return []goldenCall{
		{"global put malformed", "admin", "PUT", "/api/v1/config", "{not json"},
		{"global put no generation", "admin", "PUT", "/api/v1/config", `{"trust":{}}`},
		{"global put bad trust", "admin", "PUT", "/api/v1/config",
			`{"expected_generation":1,"trust":{"level":"godmode"}}`},
		{"pending by finding negative", "operator", "GET",
			"/api/v1/findings/-1/pending-actions", ""},
		{"pending by finding injected", "operator", "GET",
			"/api/v1/findings/1;DROP/pending-actions", ""},
		{"findings bad database", "viewer", "GET", "/api/v1/findings?database=a;DROP", ""},
		{"findings unknown database", "viewer", "GET", "/api/v1/findings?database=nope", ""},
		{"history bad metric", "viewer", "GET",
			"/api/v1/snapshots/history?database=primary&metric=bogus", ""},
		{"manual exec malformed", "operator", "POST", "/api/v1/actions/execute", "{not json"},
		{"manual exec missing sql", "operator", "POST", "/api/v1/actions/execute",
			`{"finding_id":1,"database":"primary"}`},
		{"manual exec no database", "operator", "POST", "/api/v1/actions/execute",
			`{"finding_id":1,"sql":"SELECT 1"}`},
		{"manual exec unknown database", "operator", "POST", "/api/v1/actions/execute",
			`{"finding_id":1,"sql":"SELECT 1","database":"nope"}`},
		{"managed create malformed", "admin", "POST", "/api/v1/databases/managed", "{not json"},
		{"managed test bad id", "admin", "POST", "/api/v1/databases/managed/abc/test", ""},
		{"managed test malformed", "admin", "POST", "/api/v1/databases/managed/1/test", "{bad"},
		{"incident resolve no database", "operator", "POST",
			"/api/v1/incidents/00000000-0000-0000-0000-000000000000/resolve", "{}"},
		{"emergency stop unknown database", "operator", "POST",
			"/api/v1/emergency-stop?database=nope", "{}"},
		{"resume unknown database", "operator", "POST", "/api/v1/resume?database=nope", "{}"},
		{"metrics unknown database", "viewer", "GET", "/api/v1/metrics?database=nope", ""},
		{"config get unknown database", "viewer", "GET", "/api/v1/config?database=nope", ""},
	}
}

// Authorization boundary: viewer, operator and admin on suppress, config
// put, approve and emergency stop get today's 403 or 2xx.
func authBoundaryCalls() []goldenCall {
	var calls []goldenCall
	targets := []struct{ method, path, body string }{
		{"POST", "/api/v1/findings/1/suppress?database=primary", "{}"},
		{"PUT", "/api/v1/config/databases/1", `{"expected_generation":1}`},
		{"POST", "/api/v1/actions/1/approve?database=primary", "{}"},
		{"POST", "/api/v1/actions/1/reject?database=primary", `{"reason":"no"}`},
		{"POST", "/api/v1/emergency-stop?database=primary", "{}"},
		{"POST", "/api/v1/resume?database=primary", "{}"},
		{"PUT", "/api/v1/config", `{"expected_generation":1}`},
	}
	for _, target := range targets {
		for _, role := range []string{"", "viewer", "operator", "admin"} {
			calls = append(calls, goldenCall{
				name: fmt.Sprintf("auth %s %s", role, target.path), role: role,
				method: target.method, path: target.path, body: target.body,
			})
		}
	}
	return calls
}

// Missing database with more than one instance.
func multiInstanceCalls() []goldenCall {
	return []goldenCall{
		{"finding detail no database", "viewer", "GET", "/api/v1/findings/1", ""},
		{"action detail no database", "viewer", "GET", "/api/v1/actions/1", ""},
		{"suppress no database", "operator", "POST", "/api/v1/findings/1/suppress", "{}"},
		{"unsuppress no database", "operator", "POST", "/api/v1/findings/1/unsuppress", "{}"},
		{"snapshot latest all", "viewer", "GET", "/api/v1/snapshots/latest", ""},
		{"snapshot history all", "viewer", "GET", "/api/v1/snapshots/history?metric=tps", ""},
		{"findings stats all closed", "viewer", "GET", "/api/v1/findings/stats", ""},
		{"findings list all closed", "viewer", "GET", "/api/v1/findings", ""},
		{"approve no database", "operator", "POST", "/api/v1/actions/1/approve", "{}"},
		{"manual exec all", "operator", "POST", "/api/v1/actions/execute",
			`{"finding_id":1,"sql":"SELECT 1","database":"all"}`},
	}
}

func TestHandlerSplitGolden_ClosedPool(t *testing.T) {
	h, _ := closedFleetRouter(t, "primary")
	recs := runGoldenCalls(h, closedPoolCalls())
	checkHandlerGolden(t, "closed_pool.json", recs)
	for _, rec := range recs {
		if rec.Status == http.StatusOK && strings.Contains(rec.Name, "detail") {
			t.Errorf("%s answered 200 on a closed pool", rec.Name)
		}
	}
}

func TestHandlerSplitGolden_InvalidInput(t *testing.T) {
	h, _ := closedFleetRouter(t, "primary")
	recs := runGoldenCalls(h, append(invalidInputCalls(), requestInvalidInputCalls()...))
	checkHandlerGolden(t, "invalid_input.json", recs)
	for _, rec := range recs {
		// An empty put with a generation is today's no-op 200 echo.
		if rec.Name == "config put empty" {
			if rec.Status != http.StatusOK {
				t.Errorf("%s answered %d, want the 200 echo", rec.Name, rec.Status)
			}
			continue
		}
		if rec.Status < 400 {
			t.Errorf("%s answered %d, want an error", rec.Name, rec.Status)
		}
	}
}

func TestHandlerSplitGolden_AuthBoundary(t *testing.T) {
	h, _ := closedFleetRouter(t, "primary")
	recs := runGoldenCalls(h, authBoundaryCalls())
	checkHandlerGolden(t, "auth_boundary.json", recs)
	for _, rec := range recs {
		if rec.Role == "viewer" && rec.Status != http.StatusForbidden {
			t.Errorf("%s: viewer got %d, want 403", rec.Name, rec.Status)
		}
		if rec.Role == "" && rec.Status != http.StatusUnauthorized {
			t.Errorf("%s: no user got %d, want 401", rec.Name, rec.Status)
		}
	}
}

func TestHandlerSplitGolden_MultiInstance(t *testing.T) {
	h, _ := closedFleetRouter(t, "primary", "secondary")
	checkHandlerGolden(t, "multi_instance.json", runGoldenCalls(h, multiInstanceCalls()))
}
