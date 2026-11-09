package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// migration-07: before the first step the rehearsal runner proves its
// target is not the production server. A provider that hands back the
// production DSN, or another spelling of the production server, gets
// ErrTargetIsProduction and no step runs.

const guardTable = "public.migration_rehearsal_guard_probe"

func guardPlan() planpkg.Plan {
	return planpkg.Plan{ExpandSteps: []planpkg.Step{{
		Kind: planpkg.StepCreateUniqueIndex, RequiresTopLevel: true,
		SQL: "CREATE UNIQUE INDEX CONCURRENTLY migration_rehearsal_guard_idx ON " +
			guardTable + " (id)",
	}}}
}

func guardFixture(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := migrationRuntimePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+guardTable+"; CREATE TABLE "+
		guardTable+" (id bigint NOT NULL)"); err != nil {
		t.Fatalf("create guard fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+guardTable)
	})
	return pool, os.Getenv("SAGE_TEST_DATABASE_URL")
}

func assertNoStepRan(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regclass('public.migration_rehearsal_guard_idx') IS NOT NULL`).
		Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("a rehearsal step ran on the production server")
	}
}

func TestRehearsalRefusesTheProductionDSN(t *testing.T) {
	pool, dsn := guardFixture(t)
	runner := NewPostgresRehearsalRunner(time.Second, 10*time.Second).
		WithProduction(PoolIdentity(pool))
	_, err := runner.Run(context.Background(), dsn, guardPlan())
	if !errors.Is(err, rehearsalpkg.ErrTargetIsProduction) {
		t.Fatalf("the production DSN as the clone: %v", err)
	}
	if strings.Contains(err.Error(), "postgres:postgres@") {
		t.Fatalf("refusal leaks the DSN credentials: %v", err)
	}
	assertNoStepRan(t, pool)
}

func TestRehearsalRefusesAnotherSpellingOfTheProductionServer(t *testing.T) {
	pool, dsn := guardFixture(t)
	alias := strings.Replace(dsn, "127.0.0.1", "localhost", 1)
	if alias == dsn {
		t.Fatalf("test DSN %q has no 127.0.0.1 host to respell", "SAGE_TEST_DATABASE_URL")
	}
	// Same server, another database: still the production server.
	parsed, err := pgxpool.ParseConfig(alias)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(alias, "/"+parsed.ConnConfig.Database+"?", "/postgres?", 1)
	for name, target := range map[string]string{"host alias": alias, "other db": other} {
		runner := NewPostgresRehearsalRunner(time.Second, 10*time.Second).
			WithProduction(PoolIdentity(pool))
		if _, err := runner.Run(context.Background(), target, guardPlan()); !errors.Is(err,
			rehearsalpkg.ErrTargetIsProduction) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertNoStepRan(t, pool)
}

func TestRehearsalWithoutProductionIdentityRunsNothing(t *testing.T) {
	pool, dsn := guardFixture(t)
	for name, runner := range map[string]*PostgresRehearsalRunner{
		"no source": NewPostgresRehearsalRunner(time.Second, 10*time.Second),
		"source error": NewPostgresRehearsalRunner(time.Second, 10*time.Second).
			WithProduction(func(context.Context) (ServerIdentity, error) {
				return ServerIdentity{}, errors.New("connection refused")
			}),
		"zero identity": NewPostgresRehearsalRunner(time.Second, 10*time.Second).
			WithProduction(func(context.Context) (ServerIdentity, error) {
				return ServerIdentity{Host: "prod.invalid", Port: 5432}, nil
			}),
		"nil pool": NewPostgresRehearsalRunner(time.Second, 10*time.Second).
			WithProduction(PoolIdentity(nil)),
	} {
		_, err := runner.Run(context.Background(), dsn, guardPlan())
		if !errors.Is(err, rehearsalpkg.ErrTargetUnverified) {
			t.Errorf("%s: %v, want ErrTargetUnverified", name, err)
		}
	}
	assertNoStepRan(t, pool)
}

func TestReadServerIdentityOfTheTestServer(t *testing.T) {
	pool, _ := guardFixture(t)
	id, err := PoolIdentity(pool)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.StartedAt.IsZero() || id.StartedAt.After(time.Now()) || id.ServerPort == 0 ||
		id.Host == "" || id.Port == 0 {
		t.Fatalf("identity %+v", id)
	}
	again, err := PoolIdentity(pool)(context.Background())
	if err != nil || !again.StartedAt.Equal(id.StartedAt) || again != id {
		t.Fatalf("identity is not stable: %+v then %+v (%v)", id, again, err)
	}
}

// No DB: sameServer is pure.
func TestSameServer(t *testing.T) {
	started := time.Date(2026, 11, 9, 8, 0, 0, 123456000, time.UTC)
	prod := ServerIdentity{Host: "db.prod", Port: 5432, StartedAt: started, ServerPort: 5432}
	for name, tc := range map[string]struct {
		clone ServerIdentity
		want  bool
	}{
		"same dsn": {ServerIdentity{Host: "db.prod", Port: 5432}, true},
		"same postmaster": {ServerIdentity{Host: "10.0.0.5", Port: 6432,
			StartedAt: started, ServerPort: 5432}, true},
		"other port": {ServerIdentity{Host: "db.prod", Port: 6000,
			StartedAt: started.Add(time.Microsecond), ServerPort: 6000}, false},
		"dle clone": {ServerIdentity{Host: "dle.lab", Port: 6001,
			StartedAt: started.Add(time.Hour), ServerPort: 6001}, false},
		"same start, other port": {ServerIdentity{Host: "dle.lab", Port: 6001,
			StartedAt: started, ServerPort: 6001}, false},
		"one microsecond later": {ServerIdentity{Host: "x", Port: 1,
			StartedAt: started.Add(time.Microsecond), ServerPort: 5432}, false},
		"unix socket server": {ServerIdentity{Host: "/tmp", Port: 5432,
			StartedAt: started, ServerPort: 0}, false},
	} {
		if got := sameServer(prod, tc.clone); got != tc.want {
			t.Errorf("%s: sameServer = %v, want %v", name, got, tc.want)
		}
	}
	if sameServer(ServerIdentity{}, ServerIdentity{}) {
		t.Error("two unknown identities are not proof of the same server")
	}
}
