package value

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// D3: each monitored database keeps its own ledger. The fleet reader
// aggregates every source and labels rows with the fleet instance name.
func TestFleetServiceAggregatesSourcesWithInstanceLabels(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	b := newLedgerSource(t, "b")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	seedCredit(t, ctx, b.Pool, "create_index", 30)

	report, err := fleetOf(a, b).Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if report.DBAHoursSaved.AllTime != 0.75 {
		t.Fatalf("all_time = %v, want 0.75", report.DBAHoursSaved.AllTime)
	}
	want := []DatabaseHours{{Name: "b", Hours: 0.5}, {Name: "a", Hours: 0.25}}
	if !reflect.DeepEqual(report.ByDatabase, want) {
		t.Fatalf("by_database = %+v, want %+v", report.ByDatabase, want)
	}
	if report.ByFeature["analyze_table"] != 0.25 ||
		report.ByFeature["create_index"] != 0.5 {
		t.Fatalf("by_feature = %+v", report.ByFeature)
	}
	if report.Partial || report.Unavailable == nil || len(report.Unavailable) != 0 {
		t.Fatalf("partial=%v unavailable=%#v, want complete",
			report.Partial, report.Unavailable)
	}
	assertNoLeakedLabels(t, report)
}

func TestFleetServiceSelectsDatabaseByName(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	b := newLedgerSource(t, "b")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	seedCredit(t, ctx, b.Pool, "analyze_table", 30)
	service := fleetOf(a, b)

	only, err := service.Get(ctx, Filter{Database: "a"})
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if only.DBAHoursSaved.AllTime != 0.25 || len(only.ByDatabase) != 1 ||
		only.ByDatabase[0].Name != "a" {
		t.Fatalf("filtered report = %+v", only)
	}
	all, err := service.Get(ctx, Filter{Database: "all"})
	if err != nil || all.DBAHoursSaved.AllTime != 0.75 {
		t.Fatalf("all report = %+v err=%v", all.DBAHoursSaved, err)
	}
	_, err = service.Get(ctx, Filter{Database: "missing"})
	if !errors.Is(err, ErrUnknownDatabase) {
		t.Fatalf("unknown database error = %v, want ErrUnknownDatabase", err)
	}
}

func TestFleetServiceHonorsTimeWindowPerSource(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	future := time.Now().UTC().Add(time.Hour)

	report, err := fleetOf(a).Get(ctx, Filter{
		Database: "a", Since: future, Until: future.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if report.DBAHoursSaved.AllTime != 0 || len(report.ByDatabase) != 0 {
		t.Fatalf("future window counted credit: %+v", report)
	}
}

// T6: a rollback in one database retracts only that database's credit.
func TestFleetServiceRollbackRetractsInFleetTotal(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	b := newLedgerSource(t, "b")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	bAction := seedCredit(t, ctx, b.Pool, "analyze_table", 30)
	service := fleetOf(a, b)

	result, err := NewPostgresRepository(b.Pool).
		ZeroCreditOnRevert(ctx, bAction, "rolled_back")
	if err != nil || !result.Applied || result.PreviousMinutes != 30 {
		t.Fatalf("retract: %+v err=%v", result, err)
	}
	report, err := service.Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if report.DBAHoursSaved.AllTime != 0.25 {
		t.Fatalf("all_time after rollback = %v, want 0.25",
			report.DBAHoursSaved.AllTime)
	}
}

// T7: two fleet entries for the same physical database count once.
func TestFleetServiceDedupesSamePhysicalDatabase(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	twin := Source{Name: "a-twin", Pool: openPool(t, a.Pool.Config().ConnString())}

	report, err := fleetOf(twin, a).Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if report.DBAHoursSaved.AllTime != 0.25 || len(report.ByDatabase) != 1 {
		t.Fatalf("duplicate database double-counted: %+v", report)
	}
	if report.ByDatabase[0].Name != "a" {
		t.Fatalf("dedupe kept %q, want first name in order %q",
			report.ByDatabase[0].Name, "a")
	}
}

// T8: a database that cannot be read makes the report partial; the
// others still count and nothing is silently dropped.
func TestFleetServicePartialWhenSourceDown(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	closed := openPool(t, a.Pool.Config().ConnString())
	closed.Close()
	service := fleetOf(a, Source{Name: "b", Pool: closed}, Source{Name: "c"})

	report, err := service.Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("partial read must not fail the report: %v", err)
	}
	if !report.Partial || !reflect.DeepEqual(report.Unavailable, []string{"b", "c"}) {
		t.Fatalf("partial=%v unavailable=%v, want true [b c]",
			report.Partial, report.Unavailable)
	}
	if report.DBAHoursSaved.AllTime != 0.25 {
		t.Fatalf("healthy database value lost: %v", report.DBAHoursSaved.AllTime)
	}
	results, err := service.Read(ctx, Filter{Database: "b"})
	if err != nil || len(results) != 1 || results[0].Err == nil {
		t.Fatalf("down source result = %+v err=%v", results, err)
	}
}

func TestFleetServiceUnnamedSourceIsReportedNotLabelledEmpty(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	unnamed := Source{Pool: openPool(t, a.Pool.Config().ConnString())}

	report, err := fleetOf(unnamed).Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !report.Partial || len(report.ByDatabase) != 0 {
		t.Fatalf("unnamed source was credited: %+v", report)
	}
	if !reflect.DeepEqual(report.Unavailable, []string{UnnamedSourceLabel}) {
		t.Fatalf("unavailable = %v", report.Unavailable)
	}
	assertNoLeakedLabels(t, report)
}

func TestFleetServiceNilAndEmptyFleet(t *testing.T) {
	ctx := context.Background()
	var nilService *FleetService
	if _, err := nilService.Get(ctx, Filter{}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("nil service error = %v", err)
	}
	if _, err := NewFleetService(nil).Get(ctx, Filter{}); !errors.Is(
		err, ErrRepositoryUnavailable) {
		t.Fatalf("nil lister error = %v", err)
	}
	empty := NewFleetService(func() []Source { return nil })
	report, err := empty.Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("empty fleet: %v", err)
	}
	if report.Partial || report.DBAHoursSaved.AllTime != 0 ||
		report.ByDatabase == nil || report.Unavailable == nil ||
		report.ByFeature == nil || report.TrendDaily == nil {
		t.Fatalf("empty fleet report = %#v", report)
	}
	if _, err := empty.Get(ctx, Filter{Database: "a"}); !errors.Is(
		err, ErrUnknownDatabase) {
		t.Fatalf("filter on empty fleet error = %v", err)
	}
}

func TestFleetServiceConcurrentReadersAgree(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	b := newLedgerSource(t, "b")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	seedCredit(t, ctx, b.Pool, "analyze_table", 30)
	service := fleetOf(a, b)

	const readers = 16
	reports := make([]Report, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i], errs[i] = service.Get(ctx, Filter{})
		}(i)
	}
	wg.Wait()
	for i := range reports {
		if errs[i] != nil {
			t.Fatalf("reader %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(reports[i], reports[0]) ||
			reports[i].DBAHoursSaved.AllTime != 0.75 {
			t.Fatalf("reader %d disagrees: %+v", i, reports[i])
		}
	}
}

func TestFleetServiceCancelledReadIsPartialWithContextError(t *testing.T) {
	a := newLedgerSource(t, "a")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := fleetOf(a).Read(cancelled, Filter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(results) != 1 || !errors.Is(results[0].Err, context.Canceled) {
		t.Fatalf("cancelled result = %+v", results)
	}
}

func assertNoLeakedLabels(t *testing.T, report Report) {
	t.Helper()
	for _, row := range report.ByDatabase {
		if row.Name == "" || row.Name == "all" {
			t.Fatalf("database label leaked %q: %+v", row.Name, report.ByDatabase)
		}
	}
}

func fleetOf(sources ...Source) *FleetService {
	return NewFleetService(func() []Source { return sources })
}

func newLedgerSource(t *testing.T, name string) Source {
	t.Helper()
	pool := openPool(t, testdb.CreateDatabase(t, "value_"+name))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap source %s: %v", name, err)
	}
	return Source{Name: name, Pool: pool}
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCredit inserts one verified, credited action the way the executor
// leaves it in the monitored database: database_id is unattributed.
func seedCredit(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	actionType string, minutes float64,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved, toil_model_version)
		VALUES ($1, 'SELECT 1', 'success', $2, 1) RETURNING id`,
		actionType, minutes).Scan(&id)
	if err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	return id
}

// A role that may not read pg_control_system() still dedupes by the
// connection target, and still reads its ledger.
func TestFleetServiceDedupesWithoutControlDataPrivilege(t *testing.T) {
	ctx := context.Background()
	a := newLedgerSource(t, "a")
	seedCredit(t, ctx, a.Pool, "analyze_table", 15)
	role := fmt.Sprintf("value_reader_%d", time.Now().UnixNano())
	for _, stmt := range []string{
		"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC",
		"CREATE ROLE " + role + " LOGIN PASSWORD 'reader'",
		"GRANT USAGE ON SCHEMA sage TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA sage TO " + role,
	} {
		if _, err := a.Pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = a.Pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = a.Pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	limited := a.Pool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://%s:reader@%s:%d/%s?sslmode=disable",
		role, limited.Host, limited.Port, limited.Database)
	first := Source{Name: "a", Pool: openPool(t, dsn)}
	second := Source{Name: "b", Pool: openPool(t, dsn)}

	report, err := fleetOf(first, second).Get(ctx, Filter{})
	if err != nil || report.Partial {
		t.Fatalf("limited role read: %+v err=%v", report, err)
	}
	if report.DBAHoursSaved.AllTime != 0.25 || len(report.ByDatabase) != 1 ||
		report.ByDatabase[0].Name != "a" {
		t.Fatalf("fallback identity did not dedupe: %+v", report)
	}
}
