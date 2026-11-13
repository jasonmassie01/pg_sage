package cloudtel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeARM stands in for Azure Resource Manager: the flexible server GET,
// its replicas list and Microsoft.Insights/metrics for each server.

var azureNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

const (
	azureSub    = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	azureServer = "/subscriptions/" + azureSub +
		"/resourceGroups/rg-shop/providers/Microsoft.DBforPostgreSQL/flexibleServers/orders"
)

type fakeARM struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	sku      string
	autoGrow string
	replicas []string // replica server names in rg-shop
	// metrics maps server name -> metric -> points.
	metrics  map[string]map[string][]agoPoint
	fail     map[string][]int // "server", "replicas", "metrics:<name>" -> status queue
	body     map[string]string
	paths    []string
	bearers  []string
	inFlight int
	maxConc  int
	delay    time.Duration
}

func newFakeARM(t *testing.T) *fakeARM {
	t.Helper()
	f := &fakeARM{t: t, sku: "Standard_D4ds_v4", autoGrow: "Disabled",
		metrics: map[string]map[string][]agoPoint{}, fail: map[string][]int{},
		body: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// healthy is a 4 vCore / 16 GiB server with one read replica.
func (f *fakeARM) healthy() *fakeARM {
	f.replicas = []string{"orders-ro1"}
	f.metrics["orders"] = map[string][]agoPoint{
		"cpu_percent": {{value: 31.5, ago: 2 * time.Minute},
			{value: 20, ago: 5 * time.Minute}},
		"memory_percent":     {{value: 75, ago: 2 * time.Minute}},
		"storage_used":       {{value: 96 * gibF, ago: 3 * time.Minute}},
		"storage_free":       {{value: 32 * gibF, ago: 3 * time.Minute}},
		"active_connections": {{value: 12, ago: 2 * time.Minute}},
		"read_iops":          {{value: 150, ago: 2 * time.Minute}},
		"write_iops":         {{value: 80, ago: 2 * time.Minute}},
	}
	f.metrics["orders-ro1"] = map[string][]agoPoint{
		"physical_replication_delay_in_seconds": {{value: 7, ago: 2 * time.Minute}}}
	return f
}

func (f *fakeARM) key(r *http.Request) (string, string) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/providers/Microsoft.Insights/metrics"):
		server := strings.TrimSuffix(p, "/providers/Microsoft.Insights/metrics")
		server = server[strings.LastIndex(server, "/")+1:]
		return "metrics:" + server, server
	case strings.HasSuffix(p, "/replicas"):
		return "replicas", ""
	}
	return "server", ""
}

func (f *fakeARM) serve(w http.ResponseWriter, r *http.Request) {
	key, server := f.key(r)
	f.mu.Lock()
	f.inFlight++
	f.maxConc = max(f.maxConc, f.inFlight)
	f.paths = append(f.paths, r.URL.String())
	f.bearers = append(f.bearers, r.Header.Get("Authorization"))
	status := 0
	if q := f.fail[key]; len(q) > 0 {
		status, f.fail[key] = q[0], q[1:]
	}
	body, delay := f.body[key], f.delay
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	time.Sleep(delay)
	w.Header().Set("Date", azureNow.Format(http.TimeFormat))
	if status != 0 {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"no access"}}`))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	var out any
	switch key {
	case "server":
		out = map[string]any{"sku": map[string]any{"name": f.sku, "tier": "GeneralPurpose"},
			"properties": map[string]any{"storage": map[string]any{"storageSizeGB": 128,
				"autoGrow": f.autoGrow}, "replicationRole": "Primary"}}
	case "replicas":
		var list []map[string]any
		for _, name := range f.replicas {
			list = append(list, map[string]any{"name": name, "id": "/subscriptions/" + azureSub +
				"/resourceGroups/rg-shop/providers/Microsoft.DBforPostgreSQL/flexibleServers/" +
				name})
		}
		out = map[string]any{"value": list}
	default:
		out = f.metricsJSON(server, r.URL.Query().Get("metricnames"))
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeARM) metricsJSON(server, names string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var value []map[string]any
	for _, name := range strings.Split(names, ",") {
		var data []map[string]any
		for _, p := range f.metrics[server][name] {
			v := p.value
			data = append(data, map[string]any{
				"timeStamp": azureNow.Add(-p.ago).Format(time.RFC3339), "average": v,
				"maximum": v + 1})
		}
		value = append(value, map[string]any{"name": map[string]any{"value": name},
			"timeseries": []map[string]any{{"data": data}}})
	}
	return map[string]any{"value": value}
}

// azureTokens is a token function whose failure can be switched.
type azureTokens struct {
	mu  sync.Mutex
	err error
}

func (a *azureTokens) token(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return "", a.err
	}
	return "eyJ.azure", nil
}

var errAzureLogin = errors.New("DefaultAzureCredential: failed to acquire a token")

func (f *fakeARM) source(t *testing.T, tokens *azureTokens, sleep *recordSleep) *AzureSource {
	t.Helper()
	if tokens == nil {
		tokens = &azureTokens{}
	}
	if sleep == nil {
		sleep = &recordSleep{}
	}
	src, err := NewAzureSource(AzureOptions{SubscriptionID: azureSub,
		ResourceGroup: "rg-shop", Server: "orders", Token: tokens.token,
		Endpoint: f.srv.URL, Now: func() time.Time { return azureNow }, Sleep: sleep.sleep})
	if err != nil {
		t.Fatal(err)
	}
	return src
}
