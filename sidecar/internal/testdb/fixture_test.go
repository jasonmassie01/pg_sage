package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDesignatedDSNRequiresExplicitTestEnvironment(t *testing.T) {
	t.Setenv(EnvName, "")
	t.Setenv("SAGE_DATABASE_URL",
		"postgres://legacy.example/production?sslmode=require")

	_, err := DesignatedDSN()
	if err == nil {
		t.Fatal("DesignatedDSN accepted the production runtime DSN")
	}
	if !strings.Contains(err.Error(), EnvName) {
		t.Fatalf("error = %q, want %s guidance", err, EnvName)
	}
}

func TestFixtureDatabaseNameIsPackageAndProcessScoped(t *testing.T) {
	first := fixtureDatabaseName("internal/schema", 321)
	second := fixtureDatabaseName("internal/store", 321)
	repeat := fixtureDatabaseName("internal/schema", 321)

	if first == second {
		t.Fatalf("different packages share fixture database %q", first)
	}
	if first != repeat {
		t.Fatalf("fixture name changed: %q then %q", first, repeat)
	}
	if !strings.HasPrefix(first, fixturePrefix) || len(first) > 63 {
		t.Fatalf("unsafe fixture database name %q", first)
	}
}

func TestFixtureDSNPreservesConnectionPolicy(t *testing.T) {
	base := "postgres://tester:secret@db.example:55432/admin" +
		"?sslmode=require&connect_timeout=7"
	database := fixtureDatabaseName("internal/schema", 123)
	got, err := fixtureDSN(base, database)
	if err != nil {
		t.Fatalf("fixtureDSN: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse fixture DSN: %v", err)
	}
	if parsed.Path != "/"+database {
		t.Fatalf("database path = %q", parsed.Path)
	}
	if parsed.Query().Get("sslmode") != "require" ||
		parsed.Query().Get("connect_timeout") != "7" {
		t.Fatalf("connection policy was lost: %q", parsed.RawQuery)
	}
}

func TestFixtureCleanupRejectsNonFixtureTargets(t *testing.T) {
	for _, name := range []string{"postgres", "production", "pgsage_"} {
		if err := validateFixtureDatabaseName(name); err == nil {
			t.Errorf("cleanup accepted non-fixture database %q", name)
		}
	}
	valid := fixtureDatabaseName("internal/schema", 321)
	if err := validateFixtureDatabaseName(valid); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}

func TestRunWithoutDesignationBlocksImplicitLocalhostFallbacks(t *testing.T) {
	t.Setenv(EnvName, "")
	t.Setenv("SAGE_DATABASE_URL", "postgres://production.example/app")
	called := false
	code := Run(func() int {
		called = true
		for _, name := range append([]string{EnvName}, legacyDSNEnvNames...) {
			if got := os.Getenv(name); got != disabledDSN {
				t.Errorf("%s = %q, want disabled test DSN", name, got)
			}
		}
		return 0
	}, "internal/testdb")
	if code != 0 || !called {
		t.Fatalf("Run returned %d, called=%v", code, called)
	}
}

func TestRunRejectsInvalidDesignatedDSNBeforeTests(t *testing.T) {
	t.Setenv(EnvName, "://invalid")
	called := false
	code := Run(func() int {
		called = true
		return 0
	}, "internal/testdb")
	if code == 0 || called {
		t.Fatalf("Run returned %d, called=%v; want setup failure", code, called)
	}
}

func TestRunCreatesAndDropsPackageDatabase(t *testing.T) {
	baseDSN := os.Getenv(EnvName)
	if baseDSN == "" {
		t.Skipf("%s is required for the live fixture lifecycle", EnvName)
	}
	t.Setenv("SAGE_DATABASE_URL", "sentinel-production-dsn")

	var fixtureDatabase string
	code := Run(func() int {
		fixtureDSN, err := DesignatedDSN()
		if err != nil {
			t.Errorf("designated fixture DSN: %v", err)
			return 1
		}
		cfg, err := pgx.ParseConfig(fixtureDSN)
		if err != nil {
			t.Errorf("parse fixture DSN: %v", err)
			return 1
		}
		fixtureDatabase = cfg.Database
		if err := validateFixtureDatabaseName(fixtureDatabase); err != nil {
			t.Errorf("fixture database name: %v", err)
			return 1
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, fixtureDSN)
		if err != nil {
			t.Errorf("connect fixture: %v", err)
			return 1
		}
		defer conn.Close(context.Background())
		var current string
		if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&current); err != nil {
			t.Errorf("query fixture identity: %v", err)
			return 1
		}
		if current != fixtureDatabase {
			t.Errorf("current database = %q, want %q", current, fixtureDatabase)
			return 1
		}
		return 0
	}, "internal/testdb/integration")
	if code != 0 {
		t.Fatalf("Run returned %d", code)
	}
	if got := os.Getenv("SAGE_DATABASE_URL"); got != "sentinel-production-dsn" {
		t.Fatalf("legacy environment restored to %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatalf("connect designated server after cleanup: %v", err)
	}
	defer admin.Close(context.Background())
	var exists bool
	if err := admin.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)",
		fixtureDatabase,
	).Scan(&exists); err != nil {
		t.Fatalf("verify fixture cleanup: %v", err)
	}
	if exists {
		t.Fatalf("fixture database %q still exists after Run", fixtureDatabase)
	}
}
