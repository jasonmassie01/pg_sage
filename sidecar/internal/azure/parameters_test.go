package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// fakeARM serves the Flexible Server configurations API for one server.
type fakeARM struct {
	mu            sync.Mutex
	params        map[string]*configProps
	pendingAfter  map[string]bool // parameter -> becomes pending-restart on set
	asyncPolls    int             // polls before the async operation succeeds
	asyncFail     bool
	putBodies     []string
	authHeaders   []string
	putStatusCode int
}

func newFakeARM() *fakeARM {
	return &fakeARM{params: map[string]*configProps{
		"work_mem":       {Value: "4096", DefaultValue: "4096", Unit: "KB", IsDynamicConfig: true},
		"shared_buffers": {Value: "16384", DefaultValue: "16384", Unit: "8KB"},
		"max_wal_size":   {Value: "1024", DefaultValue: "1024", Unit: "MB", IsDynamicConfig: true},
		"log_timezone":   {Value: "UTC", DefaultValue: "UTC", IsReadOnly: true},
	}, pendingAfter: map[string]bool{"shared_buffers": true}}
}

const serverPath = "/subscriptions/sub-1/resourceGroups/rg-1/providers/" +
	"Microsoft.DBforPostgreSQL/flexibleServers/srv-1/configurations/"

func (f *fakeARM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	if r.URL.Query().Get("api-version") != apiVersion {
		http.Error(w, `{"error":{"code":"InvalidApiVersion","message":"bad"}}`, 400)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/async/") {
		f.serveAsync(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, serverPath)
	param, ok := f.params[name]
	if !ok || name == r.URL.Path {
		http.Error(w, `{"error":{"code":"ResourceNotFound","message":"no such parameter"}}`, 404)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(configResource{Properties: *param})
	case http.MethodPut:
		f.servePut(w, r, name, param)
	default:
		http.Error(w, "method", 405)
	}
}

func (f *fakeARM) servePut(w http.ResponseWriter, r *http.Request, name string, p *configProps) {
	var body configResource
	raw := new(strings.Builder)
	_ = json.NewDecoder(teeReader{r, raw}).Decode(&body)
	f.putBodies = append(f.putBodies, raw.String())
	if f.putStatusCode != 0 {
		http.Error(w, `{"error":{"code":"Conflict","message":"server is busy"}}`, f.putStatusCode)
		return
	}
	p.Value = body.Properties.Value
	p.IsConfigPendingRestart = f.pendingAfter[name]
	if f.asyncPolls > 0 || f.asyncFail {
		w.Header().Set("Azure-AsyncOperation", "http://"+r.Host+"/async/op-1?api-version="+apiVersion)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	_ = json.NewEncoder(w).Encode(configResource{Properties: *p})
}

func (f *fakeARM) serveAsync(w http.ResponseWriter) {
	if f.asyncPolls > 0 {
		f.asyncPolls--
		_, _ = w.Write([]byte(`{"status":"InProgress"}`))
		return
	}
	if f.asyncFail {
		_, _ = w.Write([]byte(`{"status":"Failed","error":{"code":"Oops","message":"apply failed"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"Succeeded"}`))
}

type teeReader struct {
	r   *http.Request
	out *strings.Builder
}

func (t teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Body.Read(p)
	t.out.Write(p[:n])
	return n, err
}

func testAdapter(t *testing.T, arm *fakeARM) *ParameterAdapter {
	t.Helper()
	srv := httptest.NewServer(arm)
	t.Cleanup(srv.Close)
	adapter, err := NewParameterAdapter(
		Server{SubscriptionID: "sub-1", ResourceGroup: "rg-1", Name: "srv-1"},
		func(context.Context) (string, error) { return "tok-123", nil },
		WithBaseURL(srv.URL), WithPolling(time.Millisecond, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func change(param, value string) executor.ManagedConfigChange {
	return executor.ManagedConfigChange{Provider: "azure",
		Mechanism: executor.ManagedServerParameter, Parameter: param, Value: value}
}

func TestApplyParameterConvertsToAzureUnitAndIsInEffect(t *testing.T) {
	arm := newFakeARM()
	adapter := testAdapter(t, arm)

	got, err := adapter.ApplyParameter(context.Background(), change("work_mem", "64MB"))

	if err != nil || !got.InEffect {
		t.Fatalf("apply work_mem: result=%+v err=%v", got, err)
	}
	if arm.params["work_mem"].Value != "65536" {
		t.Fatalf("work_mem set to %q, want 65536 (KB)", arm.params["work_mem"].Value)
	}
	if !strings.Contains(arm.putBodies[0], `"source":"user-override"`) {
		t.Fatalf("PUT body = %s", arm.putBodies[0])
	}
	for _, h := range arm.authHeaders {
		if h != "Bearer tok-123" {
			t.Fatalf("Authorization = %q", h)
		}
	}
}

func TestApplyParameterReportsPendingRestart(t *testing.T) {
	arm := newFakeARM()
	adapter := testAdapter(t, arm)

	got, err := adapter.ApplyParameter(context.Background(), change("shared_buffers", "1GB"))

	if err != nil || got.InEffect || !strings.Contains(got.Note, "restart") {
		t.Fatalf("shared_buffers: result=%+v err=%v, want pending restart", got, err)
	}
	if arm.params["shared_buffers"].Value != "131072" {
		t.Fatalf("shared_buffers = %q, want 131072 (8KB pages)", arm.params["shared_buffers"].Value)
	}
}

func TestApplyParameterPollsAsyncOperation(t *testing.T) {
	arm := newFakeARM()
	arm.asyncPolls = 3
	got, err := testAdapter(t, arm).ApplyParameter(context.Background(), change("max_wal_size", "2GB"))
	if err != nil || !got.InEffect || arm.params["max_wal_size"].Value != "2048" {
		t.Fatalf("async apply: result=%+v err=%v value=%q", got, err, arm.params["max_wal_size"].Value)
	}

	failing := newFakeARM()
	failing.asyncFail = true
	_, err = testAdapter(t, failing).ApplyParameter(context.Background(), change("work_mem", "8MB"))
	if err == nil || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("failed async operation: err = %v", err)
	}
}

func TestApplyParameterResetUsesDefault(t *testing.T) {
	arm := newFakeARM()
	arm.params["work_mem"].Value = "65536"
	reset := change("work_mem", "")
	reset.Reset = true
	got, err := testAdapter(t, arm).ApplyParameter(context.Background(), reset)
	if err != nil || !got.InEffect || arm.params["work_mem"].Value != "4096" {
		t.Fatalf("reset: result=%+v err=%v value=%q", got, err, arm.params["work_mem"].Value)
	}
	if !strings.Contains(arm.putBodies[0], `"source":"system-default"`) {
		t.Fatalf("reset body = %s", arm.putBodies[0])
	}
}

func TestApplyParameterRefusals(t *testing.T) {
	arm := newFakeARM()
	adapter := testAdapter(t, arm)
	cases := map[string]struct {
		change executor.ManagedConfigChange
		want   string
		is     error
	}{
		"read-only":       {change("log_timezone", "5"), "read-only", executor.ErrManagedConfigUnsupported},
		"unknown":         {change("no_such_param", "5"), "no such parameter", nil},
		"not a multiple":  {change("shared_buffers", "12kB"), "multiple", nil},
		"bad value":       {change("work_mem", "lots"), "invalid", nil},
		"injection":       {change("work_mem/../x", "5"), "invalid parameter", nil},
		"wrong mechanism": {executor.ManagedConfigChange{Mechanism: executor.ManagedParameterGroup, Parameter: "work_mem", Value: "5"}, "server_parameter", nil},
	}
	for name, tc := range cases {
		_, err := adapter.ApplyParameter(context.Background(), tc.change)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%s: err = %v, want errors.Is %v", name, err, tc.is)
		}
	}
	if len(arm.putBodies) != 0 {
		t.Fatalf("refused changes still issued %d PUTs", len(arm.putBodies))
	}
}

func TestApplyParameterSurfacesARMErrorsAndTokenFailures(t *testing.T) {
	arm := newFakeARM()
	arm.putStatusCode = http.StatusConflict
	_, err := testAdapter(t, arm).ApplyParameter(context.Background(), change("work_mem", "8MB"))
	if err == nil || !strings.Contains(err.Error(), "server is busy") ||
		!strings.Contains(err.Error(), "409") {
		t.Fatalf("ARM conflict: err = %v", err)
	}

	adapter, err := NewParameterAdapter(
		Server{SubscriptionID: "sub-1", ResourceGroup: "rg-1", Name: "srv-1"},
		func(context.Context) (string, error) { return "", fmt.Errorf("no credential") })
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ApplyParameter(context.Background(), change("work_mem", "8MB"))
	if err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("token failure: err = %v", err)
	}
}

func TestNewParameterAdapterValidatesServer(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	for _, server := range []Server{
		{},
		{SubscriptionID: "s", ResourceGroup: "rg"},
		{SubscriptionID: "s/../x", ResourceGroup: "rg", Name: "n"},
		{SubscriptionID: "s", ResourceGroup: "rg", Name: "bad name"},
	} {
		if _, err := NewParameterAdapter(server, token); err == nil {
			t.Errorf("server %+v accepted", server)
		}
	}
	if _, err := NewParameterAdapter(Server{SubscriptionID: "s", ResourceGroup: "rg", Name: "n"}, nil); err == nil {
		t.Error("nil token source accepted")
	}
}

func TestConvertToUnit(t *testing.T) {
	for _, tc := range []struct{ value, unit, want string }{
		{"64MB", "KB", "65536"}, {"1GB", "8KB", "131072"}, {"2GB", "MB", "2048"},
		{"512", "KB", "512"}, {"1TB", "GB", "1024"}, {"512", "", "512"},
	} {
		got, err := convertToUnit(tc.value, tc.unit)
		if err != nil || got != tc.want {
			t.Errorf("convertToUnit(%q, %q) = %q, %v; want %q", tc.value, tc.unit, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ value, unit string }{
		{"12kB", "8KB"}, {"0MB", "KB"}, {"10XB", "KB"}, {"5MB", "furlongs"}, {"64kB", ""},
	} {
		if got, err := convertToUnit(tc.value, tc.unit); err == nil {
			t.Errorf("convertToUnit(%q, %q) = %q, want error", tc.value, tc.unit, got)
		}
	}
}

func TestServerFromHost(t *testing.T) {
	for host, want := range map[string]string{
		"srv-1.postgres.database.azure.com":             "srv-1",
		"SRV-1.postgres.database.azure.com.":            "srv-1",
		"srv-1.privatelink.postgres.database.azure.com": "srv-1",
		"db.example.com":                                "",
		"":                                              "",
	} {
		if got := ServerNameFromHost(host); got != want {
			t.Errorf("ServerNameFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}
