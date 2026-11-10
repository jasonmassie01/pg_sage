//go:build integration

package advisor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: both tests hold or need the cluster-wide
// alter_system lock (or a private connection), so they run alone by design.

// fatalRecorder keeps the first Fatalf message and stops the calling
// goroutine there, as testing.T does, so a helper's failure text can be
// checked.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// The tenant role's CREATE ROLE carries its password. When that statement
// fails, the failure must name the role and the cause, never the statement.
func TestConfigSQLMatrix_RoleFailureHidesPassword(t *testing.T) {
	testdb.SkipUnlessLive(t)
	admin := matrixConnect(t, os.Getenv("SAGE_DATABASE_URL"), "", "")
	if err := admin.Close(context.Background()); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}
	rec := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		matrixRole(rec, admin)
	}()
	<-done
	if rec.msg == "" {
		t.Fatal("matrixRole created a role on a closed connection")
	}
	hexRun := regexp.MustCompile(`[0-9a-f]{32}`)
	if strings.Contains(strings.ToUpper(rec.msg), "PASSWORD") || hexRun.MatchString(rec.msg) {
		t.Errorf("role failure shows the CREATE ROLE statement or its password: %q", rec.msg)
	}
	for _, want := range []string{"pgsage_matrix_", "conn closed"} {
		if !strings.Contains(rec.msg, want) {
			t.Errorf("role failure %q does not name %q", rec.msg, want)
		}
	}
}

// A reload by another package's test between a case's SET and RESET puts
// the edge value in effect. The matrix's cleanup must reload after its
// resets, so the running server matches postgresql.auto.conf again.
func TestConfigSQLMatrix_CleanupReloadsAfterReset(t *testing.T) {
	ctx := context.Background()
	release, err := testdb.LockCluster(ctx, testdb.SkipUnlessLive(t), "alter_system")
	if err != nil {
		t.Fatalf("config matrix: %v", err)
	}
	t.Cleanup(release)
	admin := matrixConnect(t, os.Getenv("SAGE_DATABASE_URL"), "", "")
	const guc = "checkpoint_timeout"
	boot := runningSetting(t, admin, guc)
	edge := "301"
	if boot == edge {
		edge = "302"
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "ALTER SYSTEM RESET "+guc)
		_, _ = admin.Exec(context.Background(), "SELECT pg_reload_conf()")
	})
	matrixExec(t, admin, "self-managed", guc, "ALTER SYSTEM SET "+guc+" = '"+edge+"'")
	// The foreign reload: the edge value is now in effect.
	matrixExec(t, admin, "self-managed", guc, "SELECT pg_reload_conf()")
	if got := awaitSetting(admin, guc, edge); got != edge {
		t.Fatalf("%s is %q after the reload, want %q", guc, got, edge)
	}
	resetSystemSettings(t, admin)
	if got := awaitSetting(admin, guc, boot); got != boot {
		t.Errorf("%s is still %q after the matrix cleanup, want %q", guc, got, boot)
	}
}

// awaitSetting polls the running value for up to 10s, since a reload
// reaches a backend asynchronously, and returns the last value seen.
func awaitSetting(conn *pgx.Conn, name, want string) string {
	got := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		err := conn.QueryRow(context.Background(),
			"SELECT setting FROM pg_settings WHERE name = $1", name).Scan(&got)
		if err != nil {
			return "error: " + describePGError(err)
		}
		if got == want {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	return got
}

func runningSetting(t *testing.T, conn *pgx.Conn, name string) string {
	t.Helper()
	var got string
	err := conn.QueryRow(context.Background(),
		"SELECT setting FROM pg_settings WHERE name = $1", name).Scan(&got)
	if err != nil {
		t.Fatalf("read %s: %s", name, describePGError(err))
	}
	return got
}
