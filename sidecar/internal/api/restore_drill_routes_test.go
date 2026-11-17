package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// drill-01d: a database's restore drill history and the fleet's
// recoverability count. The fixture is one real database holding six
// fleet members' drill rows (the governance control database layout):
// orders passed 2 days ago (RPO 4 min, RTO 11 min) after a failure,
// billing failed after a pass 40 days ago, ledger passed exactly 35 days
// ago, archive 35 days and 1 s ago, search has no provider (an open
// recoverability_unknown finding) and reports was never drilled. The
// golden files are what the dashboard's mocked Playwright spec serves
// (SAGE_UPDATE_RESTORE_DRILL_GOLDEN=1 regenerates them).

var drillNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const (
	restoreDrillsGolden = "testdata/restore_drills_golden.json"
	fleetRecoverGolden  = "testdata/fleet_recoverability_golden.json"
)

var drillFleetNames = []string{"archive", "billing", "ledger", "orders", "reports", "search"}

func drillSeconds(v float64) *float64 { return &v }

func drillPass(db string, finished time.Time, rtoSec float64) drill.Result {
	point := finished.Add(-time.Duration(rtoSec)*time.Second - 4*time.Minute)
	ready := finished.Add(-time.Minute)
	return drill.Result{Database: db, Provider: "rds_restore", Kind: clone.KindFullCopy,
		RequestedAt: finished.Add(-time.Duration(rtoSec) * time.Second), ReadyAt: &ready,
		FinishedAt: finished, RecoveryPoint: point, RTOSeconds: drillSeconds(rtoSec),
		RPOSeconds: drillSeconds(240), Passed: true, CostLabel: "about $1.40 (estimate)",
		Invariants: []drill.Invariant{
			{Name: "catalog", Table: "app.orders", Status: drill.StatusPass,
				Evidence: "same 4 columns"},
			{Name: "marker", Status: drill.StatusPass, Evidence: "sage.snapshots 4m0s " +
				"before the recovery point"}}}
}

func drillFail(db string, finished time.Time) drill.Result {
	return drill.Result{Database: db, Provider: "rds_restore", Kind: clone.KindFullCopy,
		RequestedAt: finished.Add(-9 * time.Minute), FinishedAt: finished,
		Failure: "row_count app.orders: 90 rows, source 100", CostLabel: "about $1.40 (estimate)",
		Invariants: []drill.Invariant{{Name: "row_count", Table: "app.orders",
			Status: drill.StatusFail, Evidence: "90 rows, source 100"}}}
}

func seedDrillFleet(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	day := 24 * time.Hour
	rows := []drill.Result{
		drillFail("orders", drillNow.Add(-10*day)),
		drillPass("orders", drillNow.Add(-2*day), 660),
		drillPass("billing", drillNow.Add(-40*day), 600),
		drillFail("billing", drillNow.Add(-day)),
		drillPass("ledger", drillNow.Add(-drill.RecoverabilityWindow), 900),
		drillPass("archive", drillNow.Add(-drill.RecoverabilityWindow-time.Second), 300),
	}
	s := drill.NewStore(pool)
	for _, r := range rows {
		if _, err := s.Record(ctx, r); err != nil {
			t.Fatalf("seed drill %s: %v", r.Database, err)
		}
	}
	if err := drill.ReportUnknown(ctx, pool, nil, "search", "clone.provider is none"); err != nil {
		t.Fatalf("seed recoverability_unknown: %v", err)
	}
}

// drillFleet is the six members on one bootstrapped database, and a mux
// serving the routes at drillNow with clone.provider = provider.
func drillFleet(t *testing.T, provider string) (*fleet.DatabaseManager, http.Handler,
	drillReads) {
	t.Helper()
	pool := auditPool(t, "restore_drills")
	seedDrillFleet(t, pool)
	return drillFleetOn(t, pool, provider, nil)
}

func drillFleetOn(t *testing.T, pool *pgxpool.Pool, provider string,
	results func(string) *pgxpool.Pool) (*fleet.DatabaseManager, http.Handler, drillReads) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	cfg.Clone.Provider = provider
	mgr := fleet.NewManager(cfg)
	for _, name := range drillFleetNames {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name}, Pool: pool,
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	reads := drillReads{cfg: cfg, results: results, now: func() time.Time { return drillNow }}
	mux := http.NewServeMux()
	registerRestoreDrillRoutes(mux, mgr, reads)
	return mgr, mux, reads
}

func drillGet(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	req := withUser(httptest.NewRequest(http.MethodGet, path, nil), testViewerUser())
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func matchGolden(t *testing.T, path string, body []byte) {
	t.Helper()
	got := utcTimestamps(t, body)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("indent %s: %v", got, err)
	}
	if os.Getenv("SAGE_UPDATE_RESTORE_DRILL_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(pretty.Bytes(), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	// Trimmed and newline-normalised: a CRLF checkout is not a diff.
	norm := func(b []byte) []byte {
		return bytes.TrimSpace(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
	}
	if !bytes.Equal(norm(pretty.Bytes()), norm(want)) {
		t.Fatalf("%s changed:\n got %s\nwant %s", path, pretty.Bytes(), want)
	}
}

type drillHistoryBody struct {
	Database   string         `json:"database"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason"`
	Advice     string         `json:"advice"`
	WindowDays int            `json:"window_days"`
	Drills     []drill.Result `json:"drills"`
	Latest     *drill.Result  `json:"latest"`
	LastPass   *drill.Result  `json:"last_pass"`
	NextCost   string         `json:"next_full_copy_cost_label"`
	Isolation  *struct {
		SurvivesDelete *bool `json:"survives_delete"`
	} `json:"isolation"`
}

func decodeDrillHistory(t *testing.T, body []byte) drillHistoryBody {
	t.Helper()
	var out drillHistoryBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestRestoreDrillRoutesDB_HistoryMatchesTheGolden(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	code, body := drillGet(t, mux, "/api/v1/databases/orders/restore-drills?limit=20")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	got := decodeDrillHistory(t, body)
	if got.Database != "orders" || got.Status != drill.RecoverPassed || got.WindowDays != 35 ||
		len(got.Drills) != 2 || got.Drills[0].Passed != true || got.Drills[1].Passed {
		t.Fatalf("orders = %+v, want passed with the pass then the failure", got)
	}
	if *got.Drills[0].RPOSeconds != 240 || *got.Drills[0].RTOSeconds != 660 ||
		got.LastPass == nil || got.Latest == nil || got.Latest.ID != got.Drills[0].ID {
		t.Fatalf("orders RPO/RTO = %+v, want 240 s and 660 s", got.Drills[0])
	}
	if got.NextCost != "about $1.40 (estimate)" || got.Isolation != nil {
		t.Fatalf("cost %q isolation %+v, want the full copy's label and no cloud evidence",
			got.NextCost, got.Isolation)
	}
	matchGolden(t, restoreDrillsGolden, body)
}

func TestRestoreDrillRoutesDB_LimitCapsTheHistoryNewestFirst(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	code, body := drillGet(t, mux, "/api/v1/databases/orders/restore-drills?limit=1")
	got := decodeDrillHistory(t, body)
	if code != http.StatusOK || len(got.Drills) != 1 || !got.Drills[0].Passed {
		t.Fatalf("limit=1 = %d %+v, want the newest drill only", code, got.Drills)
	}
	code, body = drillGet(t, mux, "/api/v1/databases/orders/restore-drills")
	if got := decodeDrillHistory(t, body); code != http.StatusOK || len(got.Drills) != 2 {
		t.Fatalf("default limit = %d %d drills, want 200 and both", code, len(got.Drills))
	}
}

func TestRestoreDrillRoutesDB_EachStatusAndItsReason(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	want := map[string]string{"billing": drill.RecoverFailed, "ledger": drill.RecoverPassed,
		"archive": drill.RecoverStale, "search": drill.RecoverUnknown,
		"reports": drill.RecoverNeverDrilled}
	for db, status := range want {
		code, body := drillGet(t, mux, "/api/v1/databases/"+db+"/restore-drills")
		got := decodeDrillHistory(t, body)
		if code != http.StatusOK || got.Status != status || got.Reason == "" &&
			status != drill.RecoverPassed {
			t.Errorf("%s = %d %q %q, want %q with a reason", db, code, got.Status,
				got.Reason, status)
		}
	}
	_, body := drillGet(t, mux, "/api/v1/databases/search/restore-drills")
	got := decodeDrillHistory(t, body)
	if got.Reason != "clone.provider is none" || !strings.Contains(got.Advice, "grant") ||
		got.Drills == nil || len(got.Drills) != 0 {
		t.Fatalf("search = %+v, want the finding's reason, what to grant, no drills", got)
	}
	_, body = drillGet(t, mux, "/api/v1/databases/billing/restore-drills")
	if got := decodeDrillHistory(t, body); got.Reason != "row_count app.orders: 90 rows, "+
		"source 100" {
		t.Fatalf("billing reason = %q, want the failed invariant", got.Reason)
	}
}

func TestRestoreDrillRoutesDB_NoProviderMakesANeverDrilledDatabaseUnknown(t *testing.T) {
	_, mux, _ := drillFleet(t, "none")
	_, body := drillGet(t, mux, "/api/v1/databases/reports/restore-drills")
	got := decodeDrillHistory(t, body)
	if got.Status != drill.RecoverUnknown || !strings.Contains(got.Reason, "clone.provider") ||
		got.Advice == "" {
		t.Fatalf("reports without a provider = %+v, want unknown naming clone.provider", got)
	}
	_, body = drillGet(t, mux, "/api/v1/databases/orders/restore-drills")
	if got := decodeDrillHistory(t, body); got.Status != drill.RecoverPassed {
		t.Fatalf("orders without a provider = %q, want its recorded pass to stand", got.Status)
	}
}

type fleetRecoverBody struct {
	WindowDays   int      `json:"window_days"`
	Total        int      `json:"total"`
	Passed       int      `json:"passed"`
	Failed       int      `json:"failed"`
	Stale        int      `json:"stale"`
	NeverDrilled int      `json:"never_drilled"`
	Unknown      int      `json:"unknown"`
	MedianRTO    *float64 `json:"median_rto_seconds"`
	Databases    []struct {
		Database string `json:"database"`
		Status   string `json:"status"`
	} `json:"databases"`
	Errors []string `json:"errors"`
}

func decodeFleetRecover(t *testing.T, body []byte) fleetRecoverBody {
	t.Helper()
	var out fleetRecoverBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestRestoreDrillRoutesDB_FleetCountMatchesTheGolden(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	code, body := drillGet(t, mux, "/api/v1/fleet/recoverability")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	got := decodeFleetRecover(t, body)
	if got.Total != 6 || got.Passed != 2 || got.Failed != 1 || got.Stale != 1 ||
		got.Unknown != 1 || got.NeverDrilled != 1 || got.WindowDays != 35 {
		t.Fatalf("fleet = %+v, want 2 of 6 passed, one of each other status", got)
	}
	if got.MedianRTO == nil || *got.MedianRTO != 780 || len(got.Errors) != 0 {
		t.Fatalf("median RTO = %v errors %v, want 780 (660 and 900), none", got.MedianRTO,
			got.Errors)
	}
	if len(got.Databases) != 6 || got.Databases[0].Database != "archive" {
		t.Fatalf("databases = %+v, want all six sorted by name", got.Databases)
	}
	matchGolden(t, fleetRecoverGolden, body)
}

func TestRestoreDrillRoutesDB_ConcurrentRequestsSeeTheSameCounts(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	var wg sync.WaitGroup
	bodies := make([][]byte, 2)
	codes := make([]int, 2)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/recoverability", nil)
			mux.ServeHTTP(w, withUser(req, testViewerUser()))
			codes[i], bodies[i] = w.Code, w.Body.Bytes()
		}(i)
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK ||
		!bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("concurrent = %d %d\n%s\n%s", codes[0], codes[1], bodies[0], bodies[1])
	}
	if got := decodeFleetRecover(t, bodies[0]); got.Passed != 2 || got.Total != 6 {
		t.Fatalf("concurrent counts = %+v, want 2 of 6", got)
	}
}

func TestRestoreDrillRoutesDB_ScopedCallerCountsOnlyVisibleDatabases(t *testing.T) {
	_, mux, _ := drillFleet(t, "rds_restore")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/recoverability", nil)
	req = withUser(req, testViewerUser())
	ctx := context.WithValue(req.Context(), visibleDatabasesKey,
		map[string]bool{"orders": true, "billing": true})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req.WithContext(ctx))
	got := decodeFleetRecover(t, w.Body.Bytes())
	if w.Code != http.StatusOK || got.Total != 2 || got.Passed != 1 || got.Failed != 1 {
		t.Fatalf("scoped = %d %+v, want orders and billing only", w.Code, got)
	}
}

func TestRestoreDrillRoutesDB_UnreadableHistoryIs500AndUnknownInTheFleet(t *testing.T) {
	pool := auditPool(t, "restore_drills_err")
	seedDrillFleet(t, pool)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	results := func(name string) *pgxpool.Pool {
		if name == "orders" {
			return closed
		}
		return pool
	}
	_, mux, _ := drillFleetOn(t, pool, "rds_restore", results)
	code, body := drillGet(t, mux, "/api/v1/databases/orders/restore-drills")
	if code != http.StatusInternalServerError || !strings.Contains(string(body), "orders") ||
		strings.Contains(string(body), "closed pool") {
		t.Fatalf("unreadable = %d %s, want 500 naming orders, not the driver error", code, body)
	}
	code, body = drillGet(t, mux, "/api/v1/fleet/recoverability")
	got := decodeFleetRecover(t, body)
	if code != http.StatusOK || got.Passed != 1 || got.Unknown != 2 || len(got.Errors) != 1 ||
		!strings.Contains(got.Errors[0], "orders") {
		t.Fatalf("fleet with orders unreadable = %d %+v, want orders unknown and named", code, got)
	}
}

func TestRestoreDrillsMCPDB_LatestResultPerDatabaseInScope(t *testing.T) {
	mgr, _, reads := drillFleet(t, "rds_restore")
	b := restoreDrillsMCP{mgr: mgr, reads: reads}
	ctx := context.Background()
	res, err := b.RestoreDrills(ctx, mcp.RestoreDrillsRequest{
		Databases: []string{"orders", "billing"}})
	if err != nil {
		t.Fatal(err)
	}
	fleetRes, ok := res.(drill.FleetRecoverability)
	if !ok || fleetRes.Total != 2 || fleetRes.Passed != 1 || fleetRes.Failed != 1 ||
		fleetRes.Databases[1].Latest == nil || !fleetRes.Databases[1].Latest.Passed {
		t.Fatalf("scoped = %+v, want billing failed and orders' latest pass", res)
	}
	_, err = b.RestoreDrills(ctx, mcp.RestoreDrillsRequest{Database: "search",
		Databases: []string{"orders"}})
	if err != mcp.ErrRestoreDrillsDatabaseNotFound {
		t.Fatalf("out of scope = %v, want ErrRestoreDrillsDatabaseNotFound", err)
	}
	res, err = b.RestoreDrills(ctx, mcp.RestoreDrillsRequest{Database: "search"})
	one, _ := res.(drill.FleetRecoverability)
	if err != nil || one.Total != 1 || one.Unknown != 1 {
		t.Fatalf("one database = %+v %v, want search unknown", res, err)
	}
	if _, err := b.RestoreDrills(ctx, mcp.RestoreDrillsRequest{Database: "nope"}); err !=
		mcp.ErrRestoreDrillsDatabaseNotFound {
		t.Fatalf("unknown database = %v, want not found", err)
	}
}

// The checks below answer before any read, so they need no database.

func drillUnitMux() http.Handler {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders",
		Config: config.DatabaseConfig{Name: "orders"}})
	mux := http.NewServeMux()
	registerRestoreDrillRoutes(mux, mgr, drillReads{cfg: cfg})
	return mux
}

func TestRestoreDrillRoutes_InvalidLimitIs400(t *testing.T) {
	mux := drillUnitMux()
	for _, q := range []string{"limit=0", "limit=1000", "limit=abc", "limit=-1", "limit=101",
		"limit="} {
		code, body := drillGet(t, mux, "/api/v1/databases/orders/restore-drills?"+q)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "limit") {
			t.Errorf("%s = %d %s, want 400 naming limit", q, code, body)
		}
	}
}

func TestRestoreDrillRoutes_UnknownDatabaseIs404(t *testing.T) {
	code, body := drillGet(t, drillUnitMux(), "/api/v1/databases/nope/restore-drills?limit=20")
	if code != http.StatusNotFound || !strings.Contains(string(body), "database not found") {
		t.Fatalf("unknown = %d %s, want 404 database not found", code, body)
	}
}

func TestRestoreDrillRoutes_NotConnectedIs503(t *testing.T) {
	code, body := drillGet(t, drillUnitMux(), "/api/v1/databases/orders/restore-drills")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "connected") {
		t.Fatalf("no pool = %d %s, want 503 not connected", code, body)
	}
}

func TestRestoreDrillRoutes_FleetWithoutAManagerIsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	registerRestoreDrillRoutes(mux, nil, drillReads{})
	code, body := drillGet(t, mux, "/api/v1/fleet/recoverability")
	got := decodeFleetRecover(t, body)
	if code != http.StatusOK || got.Total != 0 || got.MedianRTO != nil || got.WindowDays != 35 {
		t.Fatalf("no fleet = %d %s, want an empty count", code, body)
	}
}

func TestRestoreDrillRoutes_FleetCountsADisconnectedDatabaseUnknown(t *testing.T) {
	code, body := drillGet(t, drillUnitMux(), "/api/v1/fleet/recoverability")
	got := decodeFleetRecover(t, body)
	if code != http.StatusOK || got.Total != 1 || got.Unknown != 1 || got.Passed != 0 ||
		len(got.Errors) != 1 {
		t.Fatalf("disconnected = %d %s, want it unknown and named in errors", code, body)
	}
}
