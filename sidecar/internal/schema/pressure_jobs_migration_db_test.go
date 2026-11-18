package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// guard-01b on a real server: bootstrap twice is clean and
// sage.pressure_jobs refuses a row without its name, registrant or a
// future expiry. One active job per database and name is the store's rule
// (pressure.TestPressureJobStore_ConcurrentRegistrationsMakeOneJob).
func TestPressureJobsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "pressurejobs")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'pressure_jobs'
		  AND column_name IN ('id', 'database', 'application_name', 'table_name',
		      'registered_by', 'enforce', 'created_at', 'expires_at')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 8 {
		t.Fatalf("sage.pressure_jobs has %d of the 8 columns", cols)
	}
	ok := `INSERT INTO sage.pressure_jobs (database, application_name, registered_by,
		expires_at) VALUES ('orders', 'backfill', 'user:1', now() + interval '1 hour')`
	if _, err := pool.Exec(ctx, ok); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var enforce bool
	if err := pool.QueryRow(ctx, `SELECT enforce FROM sage.pressure_jobs
		WHERE application_name = 'backfill'`).Scan(&enforce); err != nil || enforce {
		t.Fatalf("enforce defaults to %v (%v), want false", enforce, err)
	}
	refusePressureJobRows(t, pool, pressureJobsInvalidRows)
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'sage'
		AND indexname = 'pressure_jobs_active'`).Scan(&def); err != nil {
		t.Fatalf("active index: %v", err)
	}
	if !strings.Contains(def, "database") || !strings.Contains(def, "expires_at") {
		t.Fatalf("active index = %s", def)
	}
}

// guard-02a's ddlMultixactMembers merged after this step, so the pin is its
// place: right after ddlChangeFeedProviderEvents.
func TestPressureJobsStep_IsPlacedIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	if i := stepIndex(steps, "ddlPressureJobs"); i < 1 ||
		steps[i-1][0] != "ddlChangeFeedProviderEvents" {
		t.Fatalf("T6 steps = %v, want ddlPressureJobs after ddlChangeFeedProviderEvents",
			steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.pressure_jobs",
		"CREATE INDEX IF NOT EXISTS pressure_jobs_active"} {
		if !strings.Contains(ddlPressureJobs, want) {
			t.Errorf("ddlPressureJobs lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlPressureJobs) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// pressureJobsInvalidRows break one CHECK each: an empty or over-long
// name, no database, no registrant, an expiry in the past.
var pressureJobsInvalidRows = []string{
	`INSERT INTO sage.pressure_jobs (database, application_name, registered_by, expires_at)
		 VALUES ('orders', '', 'user:1', now() + interval '1 hour')`,
	`INSERT INTO sage.pressure_jobs (database, application_name, registered_by, expires_at)
		 VALUES ('orders', repeat('a', 64), 'user:1', now() + interval '1 hour')`,
	`INSERT INTO sage.pressure_jobs (database, application_name, registered_by, expires_at)
		 VALUES ('', 'backfill', 'user:1', now() + interval '1 hour')`,
	`INSERT INTO sage.pressure_jobs (database, application_name, registered_by, expires_at)
		 VALUES ('orders', 'backfill', '', now() + interval '1 hour')`,
	`INSERT INTO sage.pressure_jobs (database, application_name, registered_by, expires_at)
		 VALUES ('orders', 'backfill', 'user:1', now() - interval '1 second')`,
}

func refusePressureJobRows(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, rows []string) {
	t.Helper()
	for _, bad := range rows {
		if _, err := pool.Exec(context.Background(), bad); err == nil {
			t.Fatalf("stored an invalid row: %s", bad)
		}
	}
}
