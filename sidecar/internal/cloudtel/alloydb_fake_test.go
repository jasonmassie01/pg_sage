package cloudtel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeAlloyDB stands in for Cloud Monitoring timeSeries.list serving the
// alloydb.googleapis.com/Instance resource. series maps a metric suffix
// (cpu/average_utilization) to its time series.

var alloyNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

type alloySeries struct {
	labels map[string]string
	points []agoPoint
	int64v bool
}

type fakeAlloyDB struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	series   map[string][]alloySeries
	status   []int // per-request status queue (consumed first); 0 = 200
	body     string
	filters  []string
	bearers  []string
	inFlight int
	maxConc  int
	delay    time.Duration
}

var alloyMetricFromFilter = regexp.MustCompile(
	`metric\.type = "alloydb\.googleapis\.com/instance/([^"]+)"`)

func newFakeAlloyDB(t *testing.T) *fakeAlloyDB {
	t.Helper()
	f := &fakeAlloyDB{t: t, series: map[string][]alloySeries{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAlloyDB) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inFlight++
	f.maxConc = max(f.maxConc, f.inFlight)
	filter := r.URL.Query().Get("filter")
	f.filters = append(f.filters, filter)
	f.bearers = append(f.bearers, r.Header.Get("Authorization"))
	status := 0
	if len(f.status) > 0 {
		status, f.status = f.status[0], f.status[1:]
	}
	delay, body := f.delay, f.body
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	time.Sleep(delay)
	w.Header().Set("Date", alloyNow.Format(http.TimeFormat))
	if status != 0 && status != http.StatusOK {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED","message":"denied"}}`))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	m := alloyMetricFromFilter.FindStringSubmatch(filter)
	if m == nil {
		http.Error(w, "bad filter "+filter, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	list := f.series[m[1]]
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"timeSeries": alloyJSON(m[1], list)})
}

func alloyJSON(metric string, list []alloySeries) []map[string]any {
	out := []map[string]any{}
	for _, s := range list {
		var points []map[string]any
		for _, p := range s.points {
			end := alloyNow.Add(-p.ago)
			value := map[string]any{"doubleValue": p.value}
			if s.int64v {
				value = map[string]any{"int64Value": strconv.FormatInt(int64(p.value), 10)}
			}
			points = append(points, map[string]any{"value": value, "interval": map[string]any{
				"startTime": end.Add(-time.Minute).Format(time.RFC3339),
				"endTime":   end.Format(time.RFC3339)}})
		}
		labels := map[string]string{"cluster_id": "orders", "instance_id": "orders-primary"}
		for k, v := range s.labels {
			labels[k] = v
		}
		out = append(out, map[string]any{
			"metric":   map[string]any{"type": alloyDBMetric + metric},
			"resource": map[string]any{"type": "alloydb.googleapis.com/Instance", "labels": labels},
			"points":   points})
	}
	return out
}

// healthy fills every metric with a fresh point.
func (f *fakeAlloyDB) healthy() *fakeAlloyDB {
	f.series["cpu/average_utilization"] = []alloySeries{{points: []agoPoint{
		{value: 42.5, ago: 2 * time.Minute}, {value: 30, ago: 4 * time.Minute}}}}
	f.series["cpu/vcpus"] = []alloySeries{{int64v: true,
		points: []agoPoint{{value: 4, ago: 2 * time.Minute}}}}
	f.series["memory/min_available_memory"] = []alloySeries{{int64v: true,
		points: []agoPoint{{value: 8 * gibF, ago: 3 * time.Minute}}}}
	f.series["postgres/total_connections"] = []alloySeries{{int64v: true,
		points: []agoPoint{{value: 57, ago: 2 * time.Minute}}}}
	f.series["postgres/replication/maximum_lag"] = []alloySeries{
		{int64v: true, labels: map[string]string{"replica_instance_id": "rp-1"},
			points: []agoPoint{{value: 1500, ago: 2 * time.Minute}}},
		{int64v: true, labels: map[string]string{"replica_instance_id": "rp-2"},
			points: []agoPoint{{value: 4200, ago: 3 * time.Minute}}}}
	return f
}

// recordSleep records retry waits instead of sleeping.
type recordSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *recordSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (f *fakeAlloyDB) source(t *testing.T, sleep *recordSleep) *AlloyDBSource {
	t.Helper()
	if sleep == nil {
		sleep = &recordSleep{}
	}
	src, err := NewAlloyDBSource(AlloyDBOptions{Project: "shop-prod", Cluster: "orders",
		Instance: "orders-primary", Token: staticToken("ya29.alloy"),
		MonitoringEndpoint: f.srv.URL, Now: func() time.Time { return alloyNow },
		Sleep: sleep.sleep})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// agoPoint is a fake datapoint measured ago before the test clock.
type agoPoint struct {
	value float64
	ago   time.Duration
}
