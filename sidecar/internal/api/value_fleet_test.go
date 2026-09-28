package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/value"
)

// T1: GET /api/v1/value aggregates the ledger of every fleet database and
// labels each row with its instance name; ?database= selects one.
func TestValueHandlerAggregatesFleetPools(t *testing.T) {
	ctx := context.Background()
	pools := valueFleetPools(t, "a", "b")
	seedValueCredit(t, ctx, pools["a"], 15)
	seedValueCredit(t, ctx, pools["b"], 30)
	router := valueFleetRouter("fleet", pools)

	report := getValueReport(t, router, "/api/v1/value", http.StatusOK)
	if report.DBAHoursSaved.AllTime != 0.75 {
		t.Fatalf("all_time = %v, want 0.75", report.DBAHoursSaved.AllTime)
	}
	want := []value.DatabaseHours{{Name: "b", Hours: 0.5}, {Name: "a", Hours: 0.25}}
	if !reflect.DeepEqual(report.ByDatabase, want) {
		t.Fatalf("by_database = %+v, want %+v", report.ByDatabase, want)
	}
	if report.Partial || len(report.Unavailable) != 0 {
		t.Fatalf("complete fleet reported partial: %+v", report)
	}
	one := getValueReport(t, router, "/api/v1/value?database=a", http.StatusOK)
	if one.DBAHoursSaved.AllTime != 0.25 || len(one.ByDatabase) != 1 ||
		one.ByDatabase[0].Name != "a" {
		t.Fatalf("?database=a report = %+v", one)
	}
	all := getValueReport(t, router, "/api/v1/value?database=all", http.StatusOK)
	if all.DBAHoursSaved.AllTime != 0.75 {
		t.Fatalf("?database=all all_time = %v", all.DBAHoursSaved.AllTime)
	}
}

// T4: in standalone mode rows carry no database_id; filtering by the
// configured database name still counts them.
func TestValueStandaloneDatabaseFilterMatchesUnattributedRows(t *testing.T) {
	ctx := context.Background()
	pools := valueFleetPools(t, "primary")
	seedValueCredit(t, ctx, pools["primary"], 15)
	router := valueFleetRouter("standalone", pools)

	report := getValueReport(t, router, "/api/v1/value?database=primary", http.StatusOK)
	if report.DBAHoursSaved.AllTime != 0.25 {
		t.Fatalf("standalone filtered all_time = %v, want 0.25",
			report.DBAHoursSaved.AllTime)
	}
	if len(report.ByDatabase) != 1 || report.ByDatabase[0].Name != "primary" {
		t.Fatalf("standalone label = %+v, want primary", report.ByDatabase)
	}
}

// T6: retracting one database's credit lowers the fleet total.
func TestValueRollbackRetractsInFleetTotal(t *testing.T) {
	ctx := context.Background()
	pools := valueFleetPools(t, "a", "b")
	seedValueCredit(t, ctx, pools["a"], 15)
	bAction := seedValueCredit(t, ctx, pools["b"], 30)
	router := valueFleetRouter("fleet", pools)

	if _, err := value.NewPostgresRepository(pools["b"]).
		ZeroCreditOnRevert(ctx, bAction, "rolled_back"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	report := getValueReport(t, router, "/api/v1/value", http.StatusOK)
	if report.DBAHoursSaved.AllTime != 0.25 {
		t.Fatalf("all_time after rollback = %v, want 0.25",
			report.DBAHoursSaved.AllTime)
	}
}

// T7: two fleet entries that point at the same database count once.
func TestValueFleetDedupesSamePhysicalDatabase(t *testing.T) {
	ctx := context.Background()
	pools := valueFleetPools(t, "a")
	seedValueCredit(t, ctx, pools["a"], 15)
	pools["a-alias"] = openValuePool(t, pools["a"].Config().ConnString())
	router := valueFleetRouter("fleet", pools)

	report := getValueReport(t, router, "/api/v1/value", http.StatusOK)
	if report.DBAHoursSaved.AllTime != 0.25 || len(report.ByDatabase) != 1 {
		t.Fatalf("same database counted twice: %+v", report)
	}
}

// T8: a database that is down makes the response partial, not a 500, and
// names the missing database; a failed-at-startup instance (nil pool) too.
func TestValuePartialWhenOnePoolDown(t *testing.T) {
	ctx := context.Background()
	pools := valueFleetPools(t, "a")
	seedValueCredit(t, ctx, pools["a"], 15)
	closed := openValuePool(t, pools["a"].Config().ConnString())
	closed.Close()
	pools["b"] = closed
	pools["c"] = nil
	router := valueFleetRouter("fleet", pools)

	report := getValueReport(t, router, "/api/v1/value", http.StatusOK)
	if !report.Partial || !reflect.DeepEqual(report.Unavailable, []string{"b", "c"}) {
		t.Fatalf("partial=%v unavailable=%v, want true [b c]",
			report.Partial, report.Unavailable)
	}
	if report.DBAHoursSaved.AllTime != 0.25 {
		t.Fatalf("healthy database value lost: %v", report.DBAHoursSaved.AllTime)
	}
}

func TestValueHandlerUnknownAndMalformedDatabase(t *testing.T) {
	router := valueFleetRouter("fleet", map[string]*pgxpool.Pool{"a": nil})
	if w := get(t, router, "/api/v1/value?database=missing"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown database status = %d body=%s", w.Code, w.Body.String())
	}
	if w := get(t, router, "/api/v1/value?database=bad'db"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed database status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestValueHandlerEmptyFleetIsCompleteZero(t *testing.T) {
	router := valueFleetRouter("fleet", map[string]*pgxpool.Pool{})
	w := get(t, router, "/api/v1/value")
	if w.Code != http.StatusOK {
		t.Fatalf("empty fleet status = %d body=%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["partial"]) != "false" || string(raw["unavailable"]) != "[]" ||
		string(raw["by_database"]) != "[]" {
		t.Fatalf("empty fleet body = %s", w.Body.String())
	}
}

func TestValueRouteWithoutFleetFailsClosed(t *testing.T) {
	router := NewRouter(nil, &config.Config{Mode: "fleet"}, nil, fakeAdminMiddleware)
	if w := get(t, router, "/api/v1/value"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil fleet status = %d body=%s", w.Code, w.Body.String())
	}
}

func getValueReport(
	t *testing.T, router http.Handler, path string, wantCode int,
) value.Report {
	t.Helper()
	w := get(t, router, path)
	if w.Code != wantCode {
		t.Fatalf("GET %s status = %d body=%s", path, w.Code, w.Body.String())
	}
	var report value.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode value report: %v", err)
	}
	for _, row := range report.ByDatabase {
		if row.Name == "" || row.Name == "all" {
			t.Fatalf("database label leaked %q in %s", row.Name, path)
		}
	}
	return report
}

func valueFleetRouter(mode string, pools map[string]*pgxpool.Pool) http.Handler {
	cfg := &config.Config{Mode: mode}
	mgr := fleet.NewManager(cfg)
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Pool: pool,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: pool != nil, LastSeen: time.Now()},
		})
	}
	return NewRouter(mgr, cfg, nil, fakeAdminMiddleware)
}

func valueFleetPools(t *testing.T, names ...string) map[string]*pgxpool.Pool {
	t.Helper()
	pools := make(map[string]*pgxpool.Pool, len(names))
	for _, name := range names {
		pool := openValuePool(t, testdb.CreateDatabase(t, "api_value_"+name))
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err := schema.Bootstrap(ctx, pool)
		cancel()
		if err != nil {
			t.Fatalf("bootstrap %s: %v", name, err)
		}
		pools[name] = pool
	}
	return pools
}

func openValuePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedValueCredit(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, minutes float64,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved, toil_model_version)
		VALUES ('analyze_table', 'SELECT 1', 'success', $1, 1) RETURNING id`,
		minutes).Scan(&id)
	if err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	return id
}
