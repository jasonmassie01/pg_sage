package testdb

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// ci-13: a test holding the exclusive server can add databases on it (the
// perf gate's meta store), and only such a test.

// Invalid input: a test that does not hold the exclusive server gets no
// database there, and an error saying why.
func TestExclusiveDatabaseNeedsTheServerHeld(t *testing.T) {
	dsn, err := exclusiveDatabase(t, "orphan")
	if err == nil || dsn != "" || !strings.Contains(err.Error(), "call Exclusive first") {
		t.Fatalf("exclusiveDatabase without Exclusive = %q, %v; want an error naming "+
			"Exclusive", dsn, err)
	}
}

func databaseIdentity(t *testing.T, dsn string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var name string
	var id int64
	if err := conn.QueryRow(ctx, `SELECT current_database(),
		(SELECT system_identifier FROM pg_control_system())`).Scan(&name, &id); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return name, id
}

// Happy path: the extra database is a new one on the held server, and it
// is dropped (before the server is released) when the test ends.
func TestExclusiveDatabaseIsAnotherDatabaseOnTheHeldServer(t *testing.T) {
	SkipUnlessLive(t)
	base := exclusiveURL(t, "extra database probe")
	var extraName string
	t.Run("holder", func(t *testing.T) {
		mainName, mainID := databaseIdentity(t, Exclusive(t, "extra database probe"))
		name, id := databaseIdentity(t, ExclusiveDatabase(t, "extra_probe"))
		if id != mainID || name == mainName || !strings.Contains(name, "extra_probe") {
			t.Fatalf("extra database %s on cluster %d, want a new extra_probe database "+
				"on the held cluster %d (main %s)", name, id, mainID, mainName)
		}
		extraName = name
	})
	if extraName == "" {
		t.Fatal("the holder subtest made no extra database")
	}
	if databaseExists(t, base, extraName) {
		t.Errorf("extra database %s outlived its test", extraName)
	}
}
