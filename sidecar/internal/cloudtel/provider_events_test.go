package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Provider events (guard-02b): RDS DescribeEvents (maintenance and
// failover categories) and Cloud SQL operations become maintenance,
// failover and version events for the change feed. Served by local test
// servers; no real cloud API is called.

var eventsNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

type rdsEventsServer struct {
	mu    sync.Mutex
	forms []url.Values
	pages []string // DescribeEvents bodies, served in order
	fail  int      // HTTP status to fail with (0: none)
}

func (s *rdsEventsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forms = append(s.forms, form)
	w.Header().Set("Content-Type", "text/xml")
	if s.fail != 0 {
		w.WriteHeader(s.fail)
		_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>`+
			`AccessDenied</Code><Message>not authorized to rds:DescribeEvents</Message>`+
			`</Error></ErrorResponse>`)
		return
	}
	if form.Get("Action") != "DescribeEvents" || len(s.pages) == 0 {
		http.Error(w, "unexpected "+form.Get("Action"), http.StatusBadRequest)
		return
	}
	page := s.pages[0]
	s.pages = s.pages[1:]
	_, _ = io.WriteString(w, page)
}

func rdsEventXML(category, message string, at time.Time) string {
	return fmt.Sprintf(`<Event><SourceIdentifier>orders-db</SourceIdentifier>`+
		`<SourceType>db-instance</SourceType><Message>%s</Message><EventCategories>`+
		`<EventCategory>%s</EventCategory></EventCategories><Date>%s</Date>`+
		`<SourceArn>arn:aws:rds:us-east-1:123456789012:db:orders-db</SourceArn></Event>`,
		message, category, at.UTC().Format(time.RFC3339))
}

func rdsEventsPage(marker string, events ...string) string {
	m := ""
	if marker != "" {
		m = "<Marker>" + marker + "</Marker>"
	}
	return `<DescribeEventsResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">` +
		`<DescribeEventsResult>` + m + `<Events>` + strings.Join(events, "") +
		`</Events></DescribeEventsResult></DescribeEventsResponse>`
}

func rdsEventsSource(t *testing.T, srv *rdsEventsServer, opts AWSOptions) *AWSSource {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	opts.Region, opts.Credentials = "us-east-1", staticCreds()
	opts.RDSEndpoint, opts.CloudWatchEndpoint, opts.PIEndpoint = ts.URL, ts.URL, ts.URL
	opts.Now = func() time.Time { return eventsNow }
	src, err := NewAWSSource(opts)
	if err != nil {
		t.Fatalf("NewAWSSource: %v", err)
	}
	return src
}

func formHas(form url.Values, prefix, value string) bool {
	for k, vs := range form {
		if strings.HasPrefix(k, prefix) {
			for _, v := range vs {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

// Happy path: failover, patch and offline maintenance map to failover,
// version_change and restart, across two pages; the request names the
// instance, the db-instance source type and both categories.
func TestProviderEvents_RDSMapsMaintenanceAndFailover(t *testing.T) {
	since := eventsNow.Add(-time.Hour)
	srv := &rdsEventsServer{pages: []string{
		rdsEventsPage("next",
			rdsEventXML("failover", "A Multi-AZ failover has completed.",
				since.Add(10*time.Minute)),
			rdsEventXML("maintenance", "Database instance patched", since.Add(20*time.Minute))),
		rdsEventsPage("", rdsEventXML("maintenance",
			"Offline maintenance of the DB instance is complete", since.Add(30*time.Minute))),
	}}
	src := rdsEventsSource(t, srv, AWSOptions{InstanceID: "orders-db"})
	evs, err := src.ProviderEvents(context.Background(), since)
	if err != nil {
		t.Fatalf("ProviderEvents: %v", err)
	}
	want := []string{EventFailover, EventVersionChange, EventRestart}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v, want %v", evs, want)
	}
	for i, e := range evs {
		if e.Category != want[i] || e.ID == "" || !strings.HasPrefix(e.ID, "rds:orders-db:") ||
			e.Message == "" || e.At.IsZero() {
			t.Errorf("event %d = %+v, want category %s", i, e, want[i])
		}
	}
	if evs[0].ID == evs[1].ID {
		t.Fatal("two events share an id")
	}
	if len(srv.forms) != 2 || srv.forms[1].Get("Marker") != "next" {
		t.Fatalf("requests = %v, want two pages", srv.forms)
	}
	f := srv.forms[0]
	if f.Get("SourceIdentifier") != "orders-db" || f.Get("SourceType") != "db-instance" ||
		!formHas(f, "EventCategories", "maintenance") ||
		!formHas(f, "EventCategories", "failover") || f.Get("StartTime") == "" {
		t.Fatalf("DescribeEvents request = %v", f)
	}
}

// An Aurora cluster reads the cluster's events; ids are stable across
// reads, so the change feed records each once.
func TestProviderEvents_RDSClusterAndStableIDs(t *testing.T) {
	at := eventsNow.Add(-5 * time.Minute)
	page := rdsEventsPage("", rdsEventXML("failover", "Completed failover to DB instance",
		at))
	srv := &rdsEventsServer{pages: []string{page, page}}
	src := rdsEventsSource(t, srv, AWSOptions{ClusterID: "orders-cluster"})
	first, err := src.ProviderEvents(context.Background(), at.Add(-time.Minute))
	if err != nil || len(first) != 1 {
		t.Fatalf("first read = %+v, err %v", first, err)
	}
	second, err := src.ProviderEvents(context.Background(), at.Add(-time.Minute))
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second read = %+v (first %+v), err %v", second, first, err)
	}
	if f := srv.forms[0]; f.Get("SourceType") != "db-cluster" ||
		f.Get("SourceIdentifier") != "orders-cluster" {
		t.Fatalf("cluster request = %v", f)
	}
}

// Error propagation: AccessDenied is ErrAuth naming the API; nothing is
// returned as if no event happened.
func TestProviderEvents_RDSAccessDenied(t *testing.T) {
	srv := &rdsEventsServer{fail: http.StatusForbidden}
	src := rdsEventsSource(t, srv, AWSOptions{InstanceID: "orders-db"})
	evs, err := src.ProviderEvents(context.Background(), eventsNow.Add(-time.Hour))
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "DescribeEvents") ||
		evs != nil {
		t.Fatalf("events = %v, err = %v, want ErrAuth naming DescribeEvents", evs, err)
	}
}

// Invalid input and boundaries: an unknown category or an empty message
// is dropped; a since in the future reads nothing and sends nothing.
func TestProviderEvents_RDSDropsUnmappedAndFuture(t *testing.T) {
	since := eventsNow.Add(-time.Hour)
	srv := &rdsEventsServer{pages: []string{rdsEventsPage("",
		rdsEventXML("notification", "Instance renamed", since.Add(time.Minute)),
		rdsEventXML("maintenance", "", since.Add(2*time.Minute)))}}
	src := rdsEventsSource(t, srv, AWSOptions{InstanceID: "orders-db"})
	evs, err := src.ProviderEvents(context.Background(), since)
	if err != nil || len(evs) != 0 {
		t.Fatalf("unmapped events = %+v, err %v, want none", evs, err)
	}
	evs, err = src.ProviderEvents(context.Background(), eventsNow.Add(time.Minute))
	if err != nil || len(evs) != 0 || len(srv.forms) != 1 {
		t.Fatalf("future since: events %+v err %v requests %d", evs, err, len(srv.forms))
	}
}

type staticGCPToken struct{}

func (staticGCPToken) Token(context.Context) (string, error) { return "test-token", nil }

func cloudSQLOpsSource(t *testing.T, body string, status int) (*GCPSource, *[]*url.URL) {
	t.Helper()
	var mu sync.Mutex
	var seen []*url.URL
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", eventsNow.Format(http.TimeFormat))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	src, err := NewGCPSource(GCPOptions{Project: "orders-project", Instance: "orders-sql",
		Token: staticGCPToken{}, SQLAdminEndpoint: ts.URL, MonitoringEndpoint: ts.URL,
		Now: func() time.Time { return eventsNow }})
	if err != nil {
		t.Fatalf("NewGCPSource: %v", err)
	}
	return src, &seen
}

// Cloud SQL operations: maintenance, failover and a major upgrade map; an
// operator UPDATE and an operation before since are dropped.
func TestProviderEvents_CloudSQLOperations(t *testing.T) {
	since := eventsNow.Add(-time.Hour)
	op := func(name, typ string, at time.Time) string {
		return fmt.Sprintf(`{"name":%q,"operationType":%q,"status":"DONE",`+
			`"startTime":%q,"insertTime":%q}`, name, typ, at.Format(time.RFC3339),
			at.Format(time.RFC3339))
	}
	body := `{"items":[` + strings.Join([]string{
		op("op-1", "MAINTENANCE", since.Add(10*time.Minute)),
		op("op-2", "FAILOVER", since.Add(20*time.Minute)),
		op("op-3", "MAJOR_VERSION_UPGRADE", since.Add(30*time.Minute)),
		op("op-4", "UPDATE", since.Add(40*time.Minute)),
		op("op-5", "MAINTENANCE", since.Add(-time.Minute)),
	}, ",") + `]}`
	src, seen := cloudSQLOpsSource(t, body, http.StatusOK)
	evs, err := src.ProviderEvents(context.Background(), since)
	if err != nil {
		t.Fatalf("ProviderEvents: %v", err)
	}
	want := map[string]string{"cloudsql:orders-sql:op-1": EventRestart,
		"cloudsql:orders-sql:op-2": EventFailover,
		"cloudsql:orders-sql:op-3": EventVersionChange}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v, want %v", evs, want)
	}
	for _, e := range evs {
		if want[e.ID] != e.Category || !strings.Contains(e.Message, "Cloud SQL") {
			t.Errorf("event %+v, want category %q", e, want[e.ID])
		}
	}
	u := (*seen)[0]
	if !strings.HasSuffix(u.Path, "/v1/projects/orders-project/operations") ||
		u.Query().Get("instance") != "orders-sql" {
		t.Fatalf("operations request = %v", u)
	}
}

// Error propagation and malformed input on Cloud SQL.
func TestProviderEvents_CloudSQLErrors(t *testing.T) {
	src, _ := cloudSQLOpsSource(t, `{"items":[{"name":`, http.StatusOK)
	if _, err := src.ProviderEvents(context.Background(), eventsNow.Add(-time.Hour)); !errors.Is(
		err, ErrMalformed) {
		t.Fatalf("malformed operations: err = %v, want ErrMalformed", err)
	}
	denied, _ := cloudSQLOpsSource(t, `{"error":{"code":403,"message":"forbidden",`+
		`"status":"PERMISSION_DENIED"}}`, http.StatusForbidden)
	if _, err := denied.ProviderEvents(context.Background(), eventsNow.Add(-time.Hour)); !errors.Is(
		err, ErrAuth) {
		t.Fatalf("denied operations: err = %v, want ErrAuth", err)
	}
	empty, _ := cloudSQLOpsSource(t, `{}`, http.StatusOK)
	evs, err := empty.ProviderEvents(context.Background(), eventsNow.Add(-time.Hour))
	if err != nil || len(evs) != 0 {
		t.Fatalf("no operations: events %+v err %v", evs, err)
	}
}

// The runtime offers provider events only when its source reads them.
func TestProviderEvents_RuntimeCapability(t *testing.T) {
	var nilRT *Runtime
	if nilRT.HasProviderEvents() || Unavailable("orders", "rds", "no creds").HasProviderEvents() {
		t.Fatal("a nil or unavailable runtime offers provider events")
	}
	srv := &rdsEventsServer{pages: []string{rdsEventsPage("")}}
	rt, err := NewRuntime(rdsEventsSource(t, srv, AWSOptions{InstanceID: "orders-db"}),
		RuntimeOptions{Database: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.HasProviderEvents() {
		t.Fatal("an RDS runtime does not offer provider events")
	}
	if evs, err := rt.ProviderEvents(context.Background(), eventsNow.Add(-time.Hour)); err != nil ||
		len(evs) != 0 {
		t.Fatalf("runtime read: %+v %v", evs, err)
	}
	if _, err := Unavailable("orders", "rds", "x").ProviderEvents(context.Background(),
		eventsNow); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable runtime read: err = %v", err)
	}
}

// No concurrent-access test: ProviderEvents holds no state between reads
// beyond the source's resolved name, which GCPSource already guards.
