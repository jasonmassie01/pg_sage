package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func roleExists(t *testing.T, ctx context.Context, dsn, role string) bool {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)",
		role).Scan(&ok); err != nil {
		t.Fatalf("look up %s: %v", role, err)
	}
	return ok
}

func execDSN(t *testing.T, ctx context.Context, dsn string, stmts ...string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// The roles a holder creates, with the grants they hold, are gone when its
// test ends.
func TestHoldSupabaseRoles_DropsTheRolesAtCleanup(t *testing.T) {
	dsn := SkipUnlessLive(t)
	ctx := context.Background()
	t.Run("holder", func(t *testing.T) {
		HoldSupabaseRoles(t, dsn)
		execDSN(t, ctx, dsn, "CREATE ROLE anon NOLOGIN",
			"CREATE ROLE authenticated NOLOGIN",
			"CREATE TABLE IF NOT EXISTS sb_hold_probe (id int)",
			"GRANT SELECT ON sb_hold_probe TO anon")
	})
	for _, role := range SupabaseRoles {
		if roleExists(t, ctx, dsn, role) {
			t.Errorf("%s outlived the test that created it", role)
		}
	}
	execDSN(t, ctx, dsn, "DROP TABLE IF EXISTS sb_hold_probe")
}

// A second holder waits until the first one's test has ended and its roles
// are gone.
func TestHoldSupabaseRoles_SerializesHolders(t *testing.T) {
	dsn := SkipUnlessLive(t)
	ctx := context.Background()
	release := make(chan struct{})
	held := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.Run("first", func(t *testing.T) {
			HoldSupabaseRoles(t, dsn)
			execDSN(t, ctx, dsn, "CREATE ROLE anon NOLOGIN")
			close(held)
			<-release
		})
	}()
	<-held
	var sawAnon bool
	second := make(chan struct{})
	go func() {
		defer close(second)
		t.Run("second", func(t *testing.T) {
			HoldSupabaseRoles(t, dsn)
			sawAnon = roleExists(t, ctx, dsn, "anon")
		})
	}()
	select {
	case <-second:
		t.Fatal("second holder got the lock while the first still held it")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	<-done
	<-second
	if sawAnon {
		t.Fatal("second holder saw the first holder's anon role")
	}
}

func TestDropSupabaseRoles_UnreachableServerIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := DropSupabaseRoles(ctx, "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("DropSupabaseRoles against no server returned nil")
	}
}
