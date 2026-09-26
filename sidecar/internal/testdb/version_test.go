package testdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// recordingTB captures Skipf/Fatalf instead of stopping the goroutine.
type recordingTB struct {
	testing.TB
	skipped, fatal string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func TestRequireServerVersion(t *testing.T) {
	dsn := SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}

	at := &recordingTB{TB: t}
	RequireServerVersion(at, pool, version, "exact")
	if at.skipped != "" || at.fatal != "" {
		t.Fatalf("server at the minimum was skipped: %+v", at)
	}
	above := &recordingTB{TB: t}
	RequireServerVersion(above, pool, version+10000, "pg_stat_io")
	if !strings.Contains(above.skipped, "pg_stat_io") ||
		!strings.Contains(above.skipped, fmt.Sprintf("PostgreSQL %d+", (version+10000)/10000)) {
		t.Fatalf("skip reason %q does not name the feature and minimum version", above.skipped)
	}

	pool.Close()
	closed := &recordingTB{TB: t}
	RequireServerVersion(closed, pool, 140000, "x")
	if closed.fatal == "" || closed.skipped != "" {
		t.Fatalf("an unreadable server version must fail, not skip: %+v", closed)
	}
}
