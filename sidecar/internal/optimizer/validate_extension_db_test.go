package optimizer

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

const extSchema = "ext_probe"

// extFixture creates ext_probe.slots and installs pg_trgm (dropped again
// when this test installed it). It skips when the server ships no pg_trgm
// and refuses to run when PostGIS or btree_gist is installed, since the
// tests prove what happens without them.
func extFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := connectTestDB(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	var available, hadTrgm, other bool
	err := pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pg_trgm'),
		EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm'),
		EXISTS (SELECT 1 FROM pg_extension WHERE extname IN ('postgis', 'btree_gist'))`).
		Scan(&available, &hadTrgm, &other)
	if err != nil {
		t.Fatalf("read extensions: %v", err)
	}
	if !available {
		t.Skip("pg_trgm is not available on this server (contrib not installed)")
	}
	if other {
		t.Fatal("postgis or btree_gist is installed in the fixture database; " +
			"these tests need them absent")
	}
	for _, sql := range []string{
		"CREATE EXTENSION IF NOT EXISTS pg_trgm",
		"DROP SCHEMA IF EXISTS " + extSchema + " CASCADE",
		"CREATE SCHEMA " + extSchema,
		"CREATE TABLE " + extSchema + ".slots (id int PRIMARY KEY, name text, during tsrange)",
	} {
		volExec(t, pool, sql)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+extSchema+" CASCADE")
		if !hadTrgm {
			_, _ = pool.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_trgm")
		}
	})
	return pool
}

func extDBContext() TableContext {
	return TableContext{Schema: extSchema, Table: "slots", Columns: []ColumnInfo{
		{Name: "id", Type: "integer"}, {Name: "name", Type: "text"},
		{Name: "during", Type: "tsrange"},
	}}
}

func extDBRec(name, keys string) Recommendation {
	return Recommendation{Table: extSchema + ".slots", IndexType: "gist",
		DDL: fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s.slots USING gist %s",
			name, extSchema, keys)}
}

// Integration (fails at the base: "requires postgis"): on a real server
// with pg_trgm and no PostGIS, Validate admits the trigram GiST index and
// the range index whose name contains "st_", and the DDL builds.
func TestExtensionDB_GistTrigramAdmitted(t *testing.T) {
	pool := extFixture(t)
	v := NewValidator(pool, fnTestOptimizerConfig(), noopLog)
	for _, rec := range []Recommendation{
		extDBRec("idx_slots_name_trgm", "(name gist_trgm_ops)"),
		extDBRec("idx_slots_last_seen", "(during)"),
	} {
		ok, reason := v.Validate(context.Background(), rec, extDBContext())
		if !ok {
			t.Fatalf("%s rejected: %s", rec.DDL, reason)
		}
		volExec(t, pool, rec.DDL)
	}
}

// Integration: a btree_gist operator class on a server without btree_gist
// is refused naming btree_gist, and PostgreSQL agrees.
func TestExtensionDB_MissingBtreeGistRefused(t *testing.T) {
	pool := extFixture(t)
	v := NewValidator(pool, fnTestOptimizerConfig(), noopLog)
	rec := extDBRec("idx_slots_id_gist", "(id gist_int4_ops)")
	ok, reason := v.Validate(context.Background(), rec, extDBContext())
	if ok || !strings.Contains(reason, "btree_gist") ||
		!strings.Contains(reason, "CREATE EXTENSION btree_gist") {
		t.Fatalf("Validate = %v, %q; want refused naming btree_gist", ok, reason)
	}
	if _, err := pool.Exec(context.Background(), rec.DDL); err == nil {
		t.Error("PostgreSQL built the index; the refusal was wrong")
	}
}

// The extension read runs once per Validate in a bounded read-only
// transaction (fails at the base: it ran on the raw pool, unbounded).
func TestExtensionDB_ReadIsBoundedAndSingle(t *testing.T) {
	pool := extFixture(t)
	v := NewValidator(pool, fnTestOptimizerConfig(), noopLog)
	var reads atomic.Int64
	var readOnly, stmt, lock string
	ctx := catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			reads.Add(1)
			return tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only'),
				current_setting('statement_timeout'), current_setting('lock_timeout')`).
				Scan(&readOnly, &stmt, &lock)
		})
	rec := extDBRec("idx_slots_two", "(id gist_int4_ops, name gist_trgm_ops)")
	ok, reason := v.Validate(ctx, rec, extDBContext())
	if ok || !strings.Contains(reason, "btree_gist") || strings.Contains(reason, "pg_trgm") {
		t.Fatalf("Validate = %v, %q; want refused naming only btree_gist", ok, reason)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("bounded reads = %d, want 1", n)
	}
	want := catalogread.Default()
	if readOnly != "on" {
		t.Errorf("transaction_read_only = %q, want on", readOnly)
	}
	if stmt != fmt.Sprintf("%dms", want.Statement.Milliseconds()) &&
		stmt != fmt.Sprintf("%ds", int(want.Statement.Seconds())) {
		t.Errorf("statement_timeout = %q, want %v", stmt, want.Statement)
	}
	if lock == "0" || lock == "" {
		t.Errorf("lock_timeout = %q, want the safety lock timeout %v", lock, want.Lock)
	}
}

// Error propagation on a real server: a canceled read admits and logs.
func TestExtensionDB_CanceledReadAdmitsAndLogs(t *testing.T) {
	pool := extFixture(t)
	logs := &logLines{}
	v := NewValidator(pool, fnTestOptimizerConfig(), logs.log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, reason := v.checkExtensionRequired(ctx,
		extDBRec("idx_slots_id_gist", "(id gist_int4_ops)"), extDBContext())
	if !ok {
		t.Fatalf("rejected on a failed read: %s", reason)
	}
	if got := logs.joined(); !strings.Contains(got, "context canceled") {
		t.Errorf("log %q does not carry the cause", got)
	}
}
