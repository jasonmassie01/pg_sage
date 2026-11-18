package specialist

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/sre"
)

// End to end on PostgreSQL (mcp-02): the incident.io and Azure Monitor
// adapters open investigations with real MCP tokens, the real
// investigation store and the request audit table (whose transport CHECK
// they must satisfy), redeliveries coalesce, and the result carries the
// agent summary.

func liveAdapterHandler(t *testing.T, f *liveFixture) http.Handler {
	t.Helper()
	io, err := ParseAlertRoutes("specialist.incidentio.routes", []string{"orders=orders:" +
		"lock_blocking"}, true)
	if err != nil {
		t.Fatal(err)
	}
	az, err := ParseAlertRoutes("specialist.azure_monitor.resources",
		[]string{azResource + "=orders:lock_blocking"}, true)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(f.spec, NewTokenAuthenticator(mcptoken.NewStore(f.pool)),
		HandlerOptions{IncidentIO: &IncidentIOAdapter{Secret: ioSecret, Field: "Database",
			Routes: io}, AzureMonitor: &AzureMonitorAdapter{Secret: azSecret,
			Resources: az}})
}

func liveIOHeaders(body []byte) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return map[string]string{"webhook-id": "msg_live", "webhook-timestamp": ts,
		"webhook-signature": ioSign(ioKey, "msg_live", ts, body)}
}

// liveRequestRow is the audit row of an adapter-opened investigation.
func liveRequestRow(t *testing.T, f *liveFixture, invID string) (string, string, int) {
	t.Helper()
	var transport, system string
	var rows int
	err := f.pool.QueryRow(context.Background(), `SELECT min(transport),
		min(external_ref->>'system'), count(*) FROM sage.specialist_requests
		WHERE investigation_id = $1`, invID).Scan(&transport, &system, &rows)
	if err != nil {
		t.Fatal(err)
	}
	return transport, system, rows
}

func TestLive_IncidentIOAdapterOpensAndCoalesces(t *testing.T) {
	f := newLiveFixture(t)
	h := liveAdapterHandler(t, f)
	body := ioEvent(ioCreated, "01HLIVEINCIDENT0000000001", "Database", "orders")
	codes, ids := liveConcurrentPosts(h, f, ioPath, body, liveIOHeaders(body))
	assertOneInvestigation(t, codes, ids)
	transport, system, rows := liveRequestRow(t, f, ids[0])
	if transport != "http" || system != "incident_io" || rows != 10 {
		t.Fatalf("audit rows: transport %q system %q rows %d", transport, system, rows)
	}
	assertLiveSummary(t, f, ids[0])
}

func TestLive_AzureMonitorAdapterOpensAndCoalesces(t *testing.T) {
	f := newLiveFixture(t)
	h := liveAdapterHandler(t, f)
	body := azAlert(azAlertID(azAlertGUID), "Fired", azResource)
	codes, ids := liveConcurrentPosts(h, f, azPath, body, azKey)
	assertOneInvestigation(t, codes, ids)
	transport, system, rows := liveRequestRow(t, f, ids[0])
	if transport != "http" || system != "azure_monitor" || rows != 10 {
		t.Fatalf("audit rows: transport %q system %q rows %d", transport, system, rows)
	}
	w := postAdapter(h, azPath, f.tokens["propose"], azAlert("", "Fired", azResource), azKey)
	if w.Code != 400 || errorBody(t, w).Code != "invalid_request" {
		t.Fatalf("missing alertId on the live path: %d %s", w.Code, w.Body.String())
	}
	assertLiveSummary(t, f, ids[0])
}

// liveConcurrentPosts sends 10 identical deliveries with the propose token.
func liveConcurrentPosts(h http.Handler, f *liveFixture, path string, body []byte,
	headers map[string]string) ([]int, []string) {
	codes, ids := make([]int, 10), make([]string, 10)
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			w := postAdapter(h, path, f.tokens["propose"], body, headers)
			var open OpenResponse
			_ = json.Unmarshal(w.Body.Bytes(), &open)
			codes[i], ids[i] = w.Code, open.Investigation.ID
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	return codes, ids
}

func assertLiveSummary(t *testing.T, f *liveFixture, invID string) {
	t.Helper()
	if err := f.coord.Investigate(context.Background(), sre.UUID(invID)); err != nil {
		t.Fatal(err)
	}
	w := call(t, f.handler, "GET", base+"/databases/orders/investigations/"+invID+"/result",
		f.tokens["propose"], "")
	var r Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("result %d %s", w.Code, w.Body.String())
	}
	s := r.SummaryForAgent
	if s == nil || s.Status != "concluded" || s.RootCause == "" || len(s.Evidence) == 0 ||
		len(s.Evidence[0].Numbers) == 0 {
		t.Fatalf("live summary %+v", s)
	}
	if s.WhyNoAction != nil && s.WhyNoAction.Link == "" {
		t.Fatalf("a why-not names its link: %+v", s.WhyNoAction)
	}
}
