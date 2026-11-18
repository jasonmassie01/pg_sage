package api

import (
	"bytes"
	"flag"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// ux-10a: the fleet list's sort keys, their default directions, the
// cursor across a fleet reload, and the no-parameter golden.

var updateDatabasesGolden = flag.Bool("update-databases-golden", false,
	"rewrite testdata/databases_no_params.golden.json from the current handler")

const databasesGoldenPath = "testdata/databases_no_params.golden.json"

// listModeOf is the fixture's execution mode by name.
func listModeOf(n int) map[string]string {
	out := make(map[string]string, n)
	for i := 0; i < n; i++ {
		out[listName(i)] = listModes[i%3]
	}
	return out
}

// checkOrdered fails unless rows are ordered by less, then by name.
func checkOrdered(t *testing.T, label string, rows []listRow, cmp func(a, b listRow) int) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		c := cmp(a, b)
		if c > 0 || (c == 0 && a.Name >= b.Name) {
			t.Fatalf("%s: %s before %s is out of order", label, a.Name, b.Name)
		}
	}
}

func intCmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestDatabasesList_SortKeysAndDirections(t *testing.T) {
	const n = 60
	h := listHandler(listFixture(t, n))
	mode := listModeOf(n)
	cases := []struct {
		query string
		cmp   func(a, b listRow) int
	}{
		{"sort=name", func(a, b listRow) int { return strings.Compare(a.Name, b.Name) }},
		{"sort=health", func(a, b listRow) int {
			return intCmp(a.Status.HealthScore, b.Status.HealthScore)
		}},
		{"sort=health&order=desc", func(a, b listRow) int {
			return intCmp(b.Status.HealthScore, a.Status.HealthScore)
		}},
		{"sort=open_issues", func(a, b listRow) int {
			return intCmp(b.Status.FindingsOpen, a.Status.FindingsOpen)
		}},
		{"sort=open_issues&order=asc", func(a, b listRow) int {
			return intCmp(a.Status.FindingsOpen, b.Status.FindingsOpen)
		}},
		{"sort=last_snapshot", func(a, b listRow) int {
			return a.Status.CollectorLastRun.Compare(b.Status.CollectorLastRun)
		}},
		{"sort=mode", func(a, b listRow) int { return strings.Compare(mode[a.Name], mode[b.Name]) }},
		{"sort=mode&order=desc", func(a, b listRow) int {
			return strings.Compare(mode[b.Name], mode[a.Name])
		}},
	}
	for _, tc := range cases {
		var rows []listRow
		pages := 0
		cursor := ""
		for {
			q := tc.query + "&limit=7"
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			code, b := listGet(t, h, q)
			if code != http.StatusOK {
				t.Fatalf("?%s = %d: %s", q, code, b.Error)
			}
			rows = append(rows, b.rows...)
			pages++
			if *b.NextCursor == "" {
				break
			}
			cursor = *b.NextCursor
		}
		if len(rows) != n || pages != 9 {
			t.Fatalf("%s: %d rows over %d pages, want %d over 9", tc.query, len(rows), pages, n)
		}
		checkOrdered(t, tc.query, rows, tc.cmp)
	}
}

func TestDatabasesList_HealthSortPutsTheFailedDatabaseFirst(t *testing.T) {
	_, b := listGet(t, listHandler(listFixture(t, 20)), "sort=health&limit=1")
	if len(b.rows) != 1 || b.rows[0].Name != "db-009" {
		t.Fatalf("worst database = %v, want the disconnected db-009", b.names())
	}
}

// A reload between pages: the cursor is a name position, so a removed
// database is skipped, an added one before the cursor is not shown, one
// after it is, and no row repeats.
func TestDatabasesList_ReloadDuringPagingSkipsRemovedRepeatsNone(t *testing.T) {
	mgr := listFixture(t, 30)
	h := listHandler(mgr)
	_, first := listGet(t, h, "sort=name&limit=10")
	seen := map[string]bool{}
	for _, n := range first.names() {
		seen[n] = true
	}
	last := first.names()[9]
	mgr.RemoveInstance("db-019")
	mgr.RemoveInstance(first.names()[3])
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	early := listInstance(31, base)
	early.Name, early.Config.Name = "db-000a", "db-000a"
	late := listInstance(33, base)
	mgr.RegisterInstance(early)
	mgr.RegisterInstance(late)
	var rest []string
	pages, err := listWalkFrom(h, "sort=name&limit=10", *first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		rest = append(rest, p...)
	}
	for _, n := range rest {
		if seen[n] {
			t.Fatalf("%s repeated after the reload", n)
		}
		if n <= last {
			t.Fatalf("%s is at or before the cursor %s", n, last)
		}
		if n == "db-019" {
			t.Fatal("removed db-019 still listed")
		}
		seen[n] = true
	}
	if !seen["db-033"] || seen["db-000a"] {
		t.Fatalf("after the reload: db-033 listed %v, db-000a listed %v; want true false",
			seen["db-033"], seen["db-000a"])
	}
}

// Concurrent paging while the fleet reloads: every page is consistent
// (sorted, no repeats within one walk). Run with -race where cgo exists.
func TestDatabasesList_ConcurrentReloadAndPaging(t *testing.T) {
	mgr := listFixture(t, 80)
	h := listHandler(mgr)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		base := time.Now()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			inst := listInstance(100+i%10, base)
			mgr.RegisterInstance(inst)
			mgr.RemoveInstance(inst.Name)
		}
	}()
	errs := make(chan string, 4)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := map[string]bool{}
			pages, err := listWalk(h, "sort=name&limit=9")
			if err != nil {
				errs <- err.Error()
				return
			}
			for _, p := range pages {
				for _, n := range p {
					if seen[n] {
						errs <- n + " repeated"
						return
					}
					seen[n] = true
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// goldenFleet is a deterministic fleet: zero times, fixed findings.
func goldenFleet() (*fleet.DatabaseManager, *config.Config) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", DatabaseID: 1,
		Config: config.DatabaseConfig{Name: "orders", Host: "orders.internal",
			Tags: []string{"tier=prod"}},
		Status: &fleet.InstanceStatus{Connected: true, FindingsOpen: 3, FindingsCritical: 1}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "billing", DatabaseID: 2,
		Config: config.DatabaseConfig{Name: "billing", Host: "billing.internal"},
		Status: &fleet.InstanceStatus{Connected: false, Error: "connection refused"}})
	return mgr, cfg
}

// CHECK-02: without list parameters the response is byte-for-byte the one
// the handler gave before ux-10a (the golden was written from that code).
func TestDatabasesList_NoParamsMatchesGolden(t *testing.T) {
	mgr, cfg := goldenFleet()
	router := NewRouter(mgr, cfg, nil, fakeAdminMiddleware)
	for _, path := range []string{"/api/v1/databases", "/api/v1/databases?database=orders"} {
		w := get(t, router, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status %d", path, w.Code)
		}
		if *updateDatabasesGolden && !strings.Contains(path, "?") {
			if err := os.WriteFile(databasesGoldenPath, w.Body.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(databasesGoldenPath)
		if err != nil {
			t.Fatalf("read %s (run with -update-databases-golden on the base): %v",
				databasesGoldenPath, err)
		}
		if !bytes.Equal(bytes.TrimSpace(w.Body.Bytes()), bytes.TrimSpace(want)) {
			t.Fatalf("%s differs from the golden:\n got %s\nwant %s", path, w.Body, want)
		}
	}
}

func TestDatabasesList_ListResponseKeepsOverviewFields(t *testing.T) {
	mgr, _ := goldenFleet()
	w := httptest.NewRecorder()
	listHandler(mgr).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases?sort=name", nil))
	body := w.Body.String()
	for _, want := range []string{`"mode":"fleet"`, `"summary":{`, `"total_databases":2`,
		`"total":2`, `"next_cursor":""`, `"name":"billing"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "orders.internal") {
		t.Errorf("list body leaks the host it searched on: %s", body)
	}
	if strings.Index(body, `"billing"`) > strings.Index(body, `"orders"`) {
		t.Errorf("sort=name did not put billing first: %s", body)
	}
}

// A database never collected (db-009, zero collector time) is the oldest:
// first ascending, last descending, and its cursor pages on.
func TestDatabasesList_NeverCollectedIsOldestSnapshot(t *testing.T) {
	if got := snapshotKey(time.Time{}); got != math.MinInt64 {
		t.Fatalf("snapshotKey(zero) = %d, want MinInt64", got)
	}
	h := listHandler(listFixture(t, 20))
	_, first := listGet(t, h, "sort=last_snapshot&limit=1")
	if strings.Join(first.names(), ",") != "db-009" || *first.NextCursor == "" {
		t.Fatalf("oldest snapshot = %v, want db-009 with a cursor", first.names())
	}
	pages := listPages(t, h, "sort=last_snapshot&limit=1")
	if len(pages) != 20 || pages[1][0] != "db-011" || pages[2][0] != "pay-000" {
		t.Fatalf("pages after db-009 = %v, want db-011 then pay-000", pages[:3])
	}
	_, desc := listGet(t, h, "sort=last_snapshot&order=desc&limit=20")
	if names := desc.names(); len(names) != 20 || names[19] != "db-009" {
		t.Fatalf("descending = %v, want db-009 last", names)
	}
}
