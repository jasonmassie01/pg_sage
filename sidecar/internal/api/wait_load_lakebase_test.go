package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// The wait-load route's Lakebase wait counters (market-01b): preferred
// over the activity samples when the seam returns intervals, never over
// Performance Insights, and invisible when the seam is absent or fails.

// No concurrent access tests: one seam read per request and no shared
// state in the handler; the parallel DB test covers the shared pool.

const waitGoldenPath = "/api/v1/wait-load?database=alpha" +
	"&since=2026-11-10T09:00:00Z&until=2026-11-10T10:00:00Z"

// waitGolden is the body the route answered before market-01b for a
// database with no pool and no provider (CHECK-39).
const waitGolden = `{"classes":[],"coverage":"none","database":"alpha","events":[],` +
	`"preferred_source":"pg_stat_activity",` +
	`"sampling":"sampled each collector cycle (not active session history)",` +
	`"series":[],"since":"2026-11-10T09:00:00Z","source":"pg_stat_activity",` +
	`"truncated":false,"until":"2026-11-10T10:00:00Z"}`

type fakeWaitSeam struct {
	calls     int
	database  string
	from, to  time.Time
	responses []lakebaseobs.WaitLoadSeries
	err       error
}

func (f *fakeWaitSeam) read(_ context.Context, database string, from, to time.Time,
) (lakebaseobs.WaitLoadSeries, error) {
	f.calls++
	f.database, f.from, f.to = database, from, to
	if f.err != nil {
		return lakebaseobs.WaitLoadSeries{}, f.err
	}
	if len(f.responses) == 0 {
		return lakebaseobs.WaitLoadSeries{}, nil
	}
	r := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}
	return r, nil
}

func seamMux(mgr *fleet.DatabaseManager, seam LakebaseWaitLoadFunc) *http.ServeMux {
	mux := http.NewServeMux()
	registerWorkloadRoutes(mux, mgr, seam)
	return mux
}

func rawWaitLoad(t *testing.T, mux *http.ServeMux, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodGet, path, nil), viewerUser()))
	return w.Code, strings.TrimSpace(w.Body.String())
}

var waitT1 = time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)

func lakebaseSeries() lakebaseobs.WaitLoadSeries {
	t0, t2 := waitT1.Add(-time.Minute), waitT1.Add(time.Minute)
	return lakebaseobs.WaitLoadSeries{
		Points: []lakebaseobs.WaitLoadPoint{
			{From: t0, T: waitT1, Sessions: 1.5,
				ByClass: map[string]float64{"CPU": 1, "LWLock": 0.5}},
			{From: waitT1, T: t2, Sessions: 0.2, ByClass: map[string]float64{"LWLock": 0.2}},
		},
		Current: map[string]float64{"LWLock": 0.2}, CurrentAt: t2,
		Classes: []string{"CPU", "LWLock"},
		Events: []lakebaseobs.WaitLoadEvent{
			{WaitClass: "CPU", WaitSeconds: 60, Count: 3},
			{WaitClass: "LWLock", WaitEvent: "WALWrite", WaitSeconds: 42, Count: 8},
		},
		Skipped: 1,
	}
}

func TestWaitLoadLakebase_AbsentOrUnavailableKeepsTheGoldenBody(t *testing.T) {
	unavailable := &fakeWaitSeam{err: fmt.Errorf("wrap: %w", lakebaseobs.ErrHistoryUnavailable)}
	empty := &fakeWaitSeam{}
	for name, seam := range map[string]LakebaseWaitLoadFunc{
		"nil seam": nil, "unavailable": unavailable.read, "no intervals": empty.read,
	} {
		code, body := rawWaitLoad(t, seamMux(noPoolMgr(), seam), waitGoldenPath)
		if code != http.StatusOK || body != waitGolden {
			t.Errorf("%s: %d %s\nwant 200 %s", name, code, body, waitGolden)
		}
	}
	if unavailable.calls != 1 || empty.calls != 1 {
		t.Fatalf("seam calls = %d/%d, want one read per request", unavailable.calls, empty.calls)
	}
}

func TestWaitLoadLakebase_SeriesIsPreferredAndNamed(t *testing.T) {
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{lakebaseSeries()}}
	code, body := getWorkload(t, seamMux(noPoolMgr(), seam.read), waitGoldenPath)
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	if seam.calls != 1 || seam.database != "alpha" ||
		!seam.from.Equal(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)) ||
		!seam.to.Equal(time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("seam got %d calls for %q %v..%v, want alpha over the request window",
			seam.calls, seam.database, seam.from, seam.to)
	}
	if body["preferred_source"] != "lakebase_wait_counters" ||
		body["source"] != waitSourceActivity || body["coverage"] != "none" {
		t.Fatalf("body %v, want lakebase preferred beside the activity source", body)
	}
	if s, ok := body["series"].([]any); !ok || len(s) != 0 {
		t.Fatalf("activity series = %#v, want the (empty) activity list kept", body["series"])
	}
	p, _ := body["provider"].(map[string]any)
	load, _ := p["load_by_wait"].(map[string]any)
	if p["source"] != "lakebase" || p["collected_at"] != "2026-11-10T09:31:00Z" ||
		len(load) != 1 || load["LWLock"] != 0.2 {
		t.Fatalf("provider %v, want source lakebase with the newest interval's load", p)
	}
	series, _ := p["series"].([]any)
	events, _ := p["events"].([]any)
	classes, _ := p["classes"].([]any)
	if len(series) != 2 || len(events) != 2 || len(classes) != 2 || p["skipped"] != float64(1) {
		t.Fatalf("provider %v, want 2 points, 2 events, 2 classes, 1 skipped", p)
	}
	if p["wait_event_time_unit"] != lakebaseobs.WaitEventTimeUnitName ||
		p["unit_confirmed"] != false || p["truncated"] != false {
		t.Fatalf("provider %v, want the assumed, unconfirmed time unit named", p)
	}
}

func TestWaitLoadLakebase_ReadErrorIsLoggedAndRouteStillAnswers(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	seam := &fakeWaitSeam{err: errors.New("inbox read: connection refused")}
	code, body := rawWaitLoad(t, seamMux(noPoolMgr(), seam.read), waitGoldenPath)
	if code != http.StatusOK || body != waitGolden {
		t.Fatalf("%d %s, want 200 with the activity body only", code, body)
	}
	out := logs.String()
	for _, want := range []string{"database=alpha", "stream=wait_event_counters",
		"connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Contains(body, "connection refused") {
		t.Fatalf("storage error echoed to the client: %s", body)
	}
}

func TestWaitLoadLakebase_SwitchesOnlyOnceIntervalsExist(t *testing.T) {
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{{}, lakebaseSeries()}}
	mux := seamMux(noPoolMgr(), seam.read)
	_, before := getWorkload(t, mux, waitGoldenPath)
	_, after := getWorkload(t, mux, waitGoldenPath)
	if before["preferred_source"] != waitSourceActivity || before["provider"] != nil {
		t.Fatalf("before the stream started: %v, want the activity source alone", before)
	}
	if after["preferred_source"] != "lakebase_wait_counters" || after["provider"] == nil {
		t.Fatalf("after intervals arrived: %v, want lakebase preferred", after)
	}
}

type piWaitSource struct{ database string }

func (s piWaitSource) Database() string { return s.database }

func (s piWaitSource) Status() cloudtel.Status {
	return cloudtel.Status{Database: s.database, Provider: "aws", Available: true,
		Withhold: []string{}, Sample: &cloudtel.Sample{Provider: "aws",
			CollectedAt: time.Now(), DBLoadByWait: map[string]float64{"CPU": 1.2}}}
}

func TestWaitLoadLakebase_PerformanceInsightsKeepsPriority(t *testing.T) {
	src := piWaitSource{database: "alpha"}
	cloudtel.Register(src)
	t.Cleanup(func() { cloudtel.Unregister(src) })
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{lakebaseSeries()}}
	_, body := getWorkload(t, seamMux(noPoolMgr(), seam.read), waitGoldenPath)
	p, _ := body["provider"].(map[string]any)
	if body["preferred_source"] != waitSourcePI || p["source"] != waitSourcePI {
		t.Fatalf("body %v, want Performance Insights preferred", body)
	}
	if seam.calls != 0 {
		t.Fatalf("seam read %d times beside Performance Insights, want 0", seam.calls)
	}
}

func TestWaitLoadLakebase_ValidationAnd404NeverReadTheSeam(t *testing.T) {
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{lakebaseSeries()}}
	mux := seamMux(noPoolMgr(), seam.read)
	for path, want := range map[string]int{
		"/api/v1/wait-load?database=all":                  http.StatusBadRequest,
		"/api/v1/wait-load":                               http.StatusBadRequest,
		"/api/v1/wait-load?database=alpha&since=noon":     http.StatusBadRequest,
		"/api/v1/wait-load?database=alpha&until=tomorrow": http.StatusBadRequest,
		"/api/v1/wait-load?database=gamma":                http.StatusNotFound,
	} {
		if code, body := rawWaitLoad(t, mux, path); code != want {
			t.Errorf("%s: %d %s, want %d", path, code, body, want)
		}
	}
	if seam.calls != 0 {
		t.Fatalf("seam read %d times for rejected requests, want 0", seam.calls)
	}
}

// The full router passes RuntimeDeps.LakebaseWaitLoad to the route.
func TestWaitLoadLakebase_RouterPassesRuntimeSeam(t *testing.T) {
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{lakebaseSeries()}}
	asViewer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, viewerUser()))
		})
	}
	h := newTestRouter(noPoolMgr(), config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{LakebaseWaitLoad: seam.read}, asViewer)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, waitGoldenPath, nil))
	if w.Code != http.StatusOK || seam.calls != 1 ||
		!strings.Contains(w.Body.String(), `"preferred_source":"lakebase_wait_counters"`) {
		t.Fatalf("router: %d %s (seam calls %d), want lakebase preferred",
			w.Code, w.Body.String(), seam.calls)
	}
}

// load_by_wait is the series' current load (every compute's newest
// interval), not just the newest point, which may hold one compute only.
func TestWaitLoadLakebase_LoadByWaitIsTheCurrentLoadOfAllComputes(t *testing.T) {
	series := lakebaseSeries()
	series.Current = map[string]float64{"LWLock": 0.2, "IO": 1.5}
	series.CurrentAt = waitT1.Add(65 * time.Second)
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{series}}
	_, body := getWorkload(t, seamMux(noPoolMgr(), seam.read), waitGoldenPath)
	p, _ := body["provider"].(map[string]any)
	load, _ := p["load_by_wait"].(map[string]any)
	if len(load) != 2 || load["IO"] != 1.5 || load["LWLock"] != 0.2 ||
		p["collected_at"] != "2026-11-10T09:31:05Z" {
		t.Fatalf("provider %v, want the current load of both computes at 09:31:05", p)
	}
	if _, leaked := p["current"]; leaked {
		t.Fatalf("provider %v names the current load twice", p)
	}
}
