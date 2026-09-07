package autoexplain

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No parallel runs: these error probes use the package's isolated fixture database.
func TestAuditCollectCycleLogPreservesError(t *testing.T) {
	pool := auditLogPool(t)
	pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var message string
	c := NewCollector(pool, CollectorConfig{CollectIntervalSeconds: 1},
		&Availability{}, func(level, format string, args ...any) {
			message = level + ":" + fmt.Sprintf(format, args...)
			cancel()
		})
	c.Run(ctx)
	assertAuditLog(t, message, "WARN:autoexplain: collect cycle:", "closed pool")
}

func TestAuditCaptureLogPreservesQueryIDAndError(t *testing.T) {
	pool := auditLogPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TEMP TABLE pg_stat_statements (
		queryid bigint, query text, mean_exec_time double precision, calls bigint, dbid oid);
		INSERT INTO pg_stat_statements
		SELECT 991234567, 'SELECT * FROM audit_missing_log_relation', 100, 20, oid
		FROM pg_database WHERE datname = current_database()`)
	if err != nil {
		t.Fatal(err)
	}
	var message string
	c := NewCollector(pool, CollectorConfig{MaxPlansPerCycle: 1}, &Availability{},
		func(level, format string, args ...any) {
			message = level + ":" + fmt.Sprintf(format, args...)
		})
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("one bad candidate must not fail the collection: %v", err)
	}
	assertAuditLog(t, message, "WARN:autoexplain: capture queryid=991234567:", "42P01")
}

func auditLogPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertAuditLog(t *testing.T, message, prefix, detail string) {
	t.Helper()
	if !strings.HasPrefix(message, prefix) || !strings.Contains(message, detail) ||
		strings.Contains(message, "%!") {
		t.Fatalf("diagnostic lost severity, context, or detail: %s", message)
	}
}
