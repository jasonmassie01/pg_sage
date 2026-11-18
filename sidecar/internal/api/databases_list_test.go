package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// ux-10a: GET /api/v1/databases searches, filters by tag, sorts and pages
// the fleet when asked (q, tag, sort, order, limit, cursor); with none of
// them the response is today's. No PostgreSQL integration test: the list
// is built in Go over FleetStatus and reads nothing from any database.

var listModes = []string{"auto", "approval", "manual"}

// listName is fixture database i: every fourth is a "pay-" database.
func listName(i int) string {
	if i%4 == 0 {
		return fmt.Sprintf("pay-%03d", i)
	}
	return fmt.Sprintf("db-%03d", i)
}

// listFixture registers n databases. Even ones are tier=prod (bare tag
// "critical" on 2), odd ones tier=dev; database 6's host names payments;
// database 9 failed its status read.
func listFixture(t testing.TB, n int) *fleet.DatabaseManager {
	t.Helper()
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		mgr.RegisterInstance(listInstance(i, base))
	}
	return mgr
}

func listInstance(i int, base time.Time) *fleet.DatabaseInstance {
	name := listName(i)
	host := fmt.Sprintf("h%03d.example.com", i)
	if i == 6 {
		host = "PAYMENTS.internal"
	}
	tags := []string{"tier=dev"}
	if i%2 == 0 {
		tags = []string{"tier=prod", "team=core"}
	}
	if i == 2 {
		tags = append(tags, "critical")
	}
	st := &fleet.InstanceStatus{Connected: true, FindingsOpen: i % 7,
		FindingsCritical: i % 3, CollectorLastRun: base.Add(time.Duration(i%11) * time.Minute)}
	if i == 9 {
		st = &fleet.InstanceStatus{Connected: false, Error: "connection refused"}
	}
	return &fleet.DatabaseInstance{Name: name, DatabaseID: i + 1,
		Config: config.DatabaseConfig{Name: name, Host: host, Tags: tags,
			ExecutionMode: listModes[i%3]},
		Status: st}
}

type listRow struct {
	Name   string   `json:"name"`
	Tags   []string `json:"tags"`
	Status struct {
		Connected        bool      `json:"connected"`
		Error            string    `json:"error"`
		HealthScore      int       `json:"health_score"`
		FindingsOpen     int       `json:"findings_open"`
		CollectorLastRun time.Time `json:"collector_last_run"`
	} `json:"status"`
}

type listBody struct {
	Mode        string          `json:"mode"`
	Summary     map[string]any  `json:"summary"`
	Databases   json.RawMessage `json:"databases"`
	Total       *int            `json:"total"`
	TotalCapped *bool           `json:"total_capped"`
	NextCursor  *string         `json:"next_cursor"`
	Error       string          `json:"error"`
	rows        []listRow
}

func listGet(t testing.TB, h http.Handler, query string) (int, listBody) {
	t.Helper()
	return listServe(t, h, httptest.NewRequest("GET", "/api/v1/databases?"+query, nil))
}

func listServe(t testing.TB, h http.Handler, req *http.Request) (int, listBody) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var b listBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %s: %v (%s)", req.URL, err, w.Body.String())
	}
	if len(b.Databases) > 0 {
		if err := json.Unmarshal(b.Databases, &b.rows); err != nil {
			t.Fatalf("decode databases: %v", err)
		}
	}
	return w.Code, b
}

func (b listBody) names() []string {
	out := make([]string, len(b.rows))
	for i, r := range b.rows {
		out[i] = r.Name
	}
	return out
}

// listPages follows next_cursor from query to the end and returns every
// page's names.
func listPages(t testing.TB, h http.Handler, query string) [][]string {
	t.Helper()
	pages, err := listWalk(h, query)
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

// listWalk is listPages without a testing.TB, for use off the test goroutine.
func listWalk(h http.Handler, query string) ([][]string, error) {
	return listWalkFrom(h, query, "")
}

// listWalkFrom pages query from cursor ("" for the first page) to the end.
func listWalkFrom(h http.Handler, query, cursor string) ([][]string, error) {
	var pages [][]string
	for i := 0; i < 100; i++ {
		q := query
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases?"+q, nil))
		var b struct {
			Databases  []listRow `json:"databases"`
			NextCursor string    `json:"next_cursor"`
		}
		if w.Code != http.StatusOK {
			return pages, fmt.Errorf("GET ?%s = %d: %s", q, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			return pages, fmt.Errorf("decode ?%s: %w", q, err)
		}
		names := make([]string, len(b.Databases))
		for j, r := range b.Databases {
			names[j] = r.Name
		}
		pages = append(pages, names)
		if b.NextCursor == "" {
			return pages, nil
		}
		cursor = b.NextCursor
	}
	return pages, fmt.Errorf("paging ?%s did not end after 100 pages", query)
}

func listHandler(mgr *fleet.DatabaseManager) http.Handler {
	return aggregateRead(mgr, aggregateHandler(databasesHandler))
}

// wantPayProd is the fixture's q=pay&tag=tier=prod match set by name.
func wantPayProd(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		if i%2 == 0 && (i%4 == 0 || i == 6) {
			out = append(out, listName(i))
		}
	}
	return sortedCopy(out)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestDatabasesList_SearchTagSortPagesWithoutOverlap(t *testing.T) {
	h := listHandler(listFixture(t, 500))
	code, b := listGet(t, h, "q=pay&tag=tier=prod&sort=name&limit=50")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, b.Error)
	}
	want := wantPayProd(500)
	if len(want) != 126 {
		t.Fatalf("fixture match set = %d, want 126", len(want))
	}
	if got := b.names(); strings.Join(got, ",") != strings.Join(want[:50], ",") {
		t.Fatalf("first page = %v, want %v", got, want[:50])
	}
	if b.Total == nil || *b.Total != 126 || b.TotalCapped == nil || *b.TotalCapped {
		t.Fatalf("total = %v capped = %v, want 126 false", b.Total, b.TotalCapped)
	}
	if b.NextCursor == nil || *b.NextCursor == "" {
		t.Fatal("first page of 126 matches has no next_cursor")
	}
	if b.Summary["total_databases"] != float64(500) {
		t.Fatalf("summary.total_databases = %v, want the whole fleet 500",
			b.Summary["total_databases"])
	}
	pages := listPages(t, h, "q=pay&tag=tier=prod&sort=name&limit=50")
	if len(pages) != 3 || len(pages[2]) != 26 {
		t.Fatalf("pages = %d (last %d), want 3 pages ending with 26", len(pages),
			len(pages[len(pages)-1]))
	}
	var all []string
	for _, p := range pages {
		all = append(all, p...)
	}
	if strings.Join(all, ",") != strings.Join(want, ",") {
		t.Fatalf("paged rows differ from the match set:\n got %v\nwant %v", all, want)
	}
}

func TestDatabasesList_DefaultLimitIsFifty(t *testing.T) {
	_, b := listGet(t, listHandler(listFixture(t, 120)), "q=")
	if len(b.rows) != 50 || b.Total == nil || *b.Total != 120 {
		t.Fatalf("rows = %d total = %v, want 50 of 120", len(b.rows), b.Total)
	}
}

func TestDatabasesList_InvalidInputIs400(t *testing.T) {
	h := listHandler(listFixture(t, 5))
	_, page := listGet(t, h, "sort=name&limit=2")
	nameCursor := url.QueryEscape(*page.NextCursor)
	cases := map[string]string{
		"limit=0":                          "limit",
		"limit=501":                        "limit",
		"limit=-1":                         "limit",
		"limit=ten":                        "limit",
		"cursor=!!!":                       "cursor",
		"cursor=bm9wZQ":                    "cursor",
		"sort=size":                        "sort",
		"sort=name&order=sideways":         "order",
		"tag=":                             "tag",
		"tag=%3Dprod":                      "tag",
		"sort=health&cursor=" + nameCursor: "cursor",
	}
	forged := map[string]listCursor{
		"non-numeric health key": {Sort: "health", Order: "asc", Keys: []string{"abc", "db-001"}},
		"one key":                {Sort: "name", Order: "asc", Keys: []string{"db-001"}},
		"empty name":             {Sort: "name", Order: "asc", Keys: []string{"db-001", ""}},
	}
	for label, c := range forged {
		sortOrder := "sort=" + c.Sort + "&order=" + c.Order
		cases[sortOrder+"&cursor="+encodeListCursor(c)+"&x="+url.QueryEscape(label)] = "cursor"
	}
	for query, word := range cases {
		code, b := listGet(t, h, query)
		if code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", query, code)
			continue
		}
		if !strings.Contains(b.Error, word) {
			t.Errorf("?%s error %q does not name %q", query, b.Error, word)
		}
	}
}

func TestDatabasesList_NoMatchIsEmptyNotNull(t *testing.T) {
	h := listHandler(listFixture(t, 20))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases?q=zzz-nothing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"databases":[]`, `"total":0`, `"next_cursor":""`,
		`"total_capped":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
}

func TestDatabasesList_EmptyFleetPages(t *testing.T) {
	code, b := listGet(t, listHandler(listFixture(t, 0)), "sort=health")
	if code != http.StatusOK || b.Total == nil || *b.Total != 0 || len(b.rows) != 0 {
		t.Fatalf("empty fleet = %d total %v rows %d", code, b.Total, len(b.rows))
	}
}

func TestDatabasesList_FailedStatusRowKeepsItsError(t *testing.T) {
	_, b := listGet(t, listHandler(listFixture(t, 20)), "q=db-009")
	if len(b.rows) != 1 || b.rows[0].Name != "db-009" {
		t.Fatalf("rows = %v, want db-009", b.names())
	}
	if b.rows[0].Status.Error != "connection refused" || b.rows[0].Status.Connected {
		t.Fatalf("db-009 status = %+v, want its connection error", b.rows[0].Status)
	}
}

func TestDatabasesList_ExactlyFiveHundredInOnePage(t *testing.T) {
	h := listHandler(listFixture(t, 500))
	_, b := listGet(t, h, "sort=name&limit=500")
	if len(b.rows) != 500 || *b.NextCursor != "" || *b.Total != 500 {
		t.Fatalf("rows %d cursor %q total %d, want 500 rows and no cursor",
			len(b.rows), *b.NextCursor, *b.Total)
	}
	pages := listPages(t, h, "sort=name&limit=499")
	if len(pages) != 2 || len(pages[1]) != 1 || pages[1][0] != "pay-496" {
		t.Fatalf("limit 499 pages = %d, last %v; want a second page of pay-496",
			len(pages), pages[len(pages)-1])
	}
}

func TestDatabasesList_TagForms(t *testing.T) {
	h := listHandler(listFixture(t, 12))
	cases := map[string][]string{
		"tag=critical":                   {"db-002"},
		"tag=CRITICAL":                   {"db-002"},
		"tag=team":                       {"db-002", "db-006", "db-010", "pay-000", "pay-004", "pay-008"},
		"tag=Tier%3DDev&q=db-00":         {"db-001", "db-003", "db-005", "db-007", "db-009"},
		"tag=tier%3Dprod&tag=critical":   {"db-002"},
		"tag=tier%3Dprod&tag=tier%3Ddev": nil,
		"tag=prod":                       nil,
	}
	for query, want := range cases {
		code, b := listGet(t, h, query+"&sort=name")
		if code != http.StatusOK {
			t.Fatalf("?%s = %d: %s", query, code, b.Error)
		}
		if strings.Join(b.names(), ",") != strings.Join(want, ",") {
			t.Errorf("?%s = %v, want %v", query, b.names(), want)
		}
	}
}

func TestDatabasesList_SearchIsCaseInsensitiveOverNameAndHost(t *testing.T) {
	h := listHandler(listFixture(t, 12))
	cases := map[string][]string{
		"q=PAY":          {"db-006", "pay-000", "pay-004", "pay-008"},
		"q=payments":     {"db-006"},
		"q=h011.EXAMPLE": {"db-011"},
		"q=%20db-011%20": {"db-011"},
		"q=.*":           nil,
	}
	for query, want := range cases {
		_, b := listGet(t, h, query+"&sort=name")
		if strings.Join(b.names(), ",") != strings.Join(want, ",") {
			t.Errorf("?%s = %v, want %v", query, b.names(), want)
		}
	}
}

func TestDatabasesList_ScopedCallerListsOnlyVisible(t *testing.T) {
	h := listHandler(listFixture(t, 12))
	req := httptest.NewRequest("GET", "/api/v1/databases?q=pay&sort=name", nil)
	visible := map[string]bool{"pay-004": true, "db-001": true}
	req = req.WithContext(context.WithValue(req.Context(), visibleDatabasesKey, visible))
	_, b := listServe(t, h, req)
	if strings.Join(b.names(), ",") != "pay-004" || *b.Total != 1 {
		t.Fatalf("scoped rows = %v total %d, want only pay-004", b.names(), *b.Total)
	}
	if b.Summary["total_databases"] != float64(2) {
		t.Fatalf("scoped summary = %v, want the 2 visible", b.Summary["total_databases"])
	}
}
