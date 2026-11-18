package clone

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// lab-04a: one identity guard for every rehearser. A clone whose DSN, or
// whose answering server, is production is refused before any statement.

func identityPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

// elsewhere is a production identity that is not the test server: a
// clone on the test server is then a real, separate clone.
func elsewhere(context.Context) (ServerIdentity, error) {
	return ServerIdentity{Host: "production.invalid", Port: 5432,
		StartedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ServerPort: 5432}, nil
}

func TestGuardedConnectRefusesTheProductionDSN(t *testing.T) {
	pool, dsn := identityPool(t)
	got, err := GuardedConnect(context.Background(), Clone{ID: "c1", DSN: dsn},
		PoolIdentity(pool))
	if got != nil || !errors.Is(err, ErrTargetIsProduction) {
		t.Fatalf("the production DSN as the clone: pool %v, err %v", got, err)
	}
	if strings.Contains(err.Error(), "@") {
		t.Fatalf("refusal leaks the DSN credentials: %v", err)
	}
}

func TestGuardedConnectRefusesAnotherSpellingOfProduction(t *testing.T) {
	pool, dsn := identityPool(t)
	alias := strings.Replace(dsn, "127.0.0.1", "localhost", 1)
	if alias == dsn {
		t.Fatalf("test DSN has no 127.0.0.1 host to respell")
	}
	parsed, err := pgxpool.ParseConfig(alias)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(alias, "/"+parsed.ConnConfig.Database+"?", "/postgres?", 1)
	for name, target := range map[string]string{"host alias": alias, "other db": other} {
		got, err := GuardedConnect(context.Background(), Clone{DSN: target},
			PoolIdentity(pool))
		if got != nil || !errors.Is(err, ErrTargetIsProduction) {
			t.Errorf("%s: pool %v, err %v", name, got, err)
		}
	}
}

func TestGuardedConnectFailsClosedWithoutAProductionIdentity(t *testing.T) {
	_, dsn := identityPool(t)
	for name, src := range map[string]IdentitySource{
		"no source": nil,
		"source error": func(context.Context) (ServerIdentity, error) {
			return ServerIdentity{}, errors.New("connection refused")
		},
		"zero start time": func(context.Context) (ServerIdentity, error) {
			return ServerIdentity{Host: "prod.invalid", Port: 5432}, nil
		},
		"nil pool": PoolIdentity(nil),
	} {
		got, err := GuardedConnect(context.Background(), Clone{DSN: dsn}, src)
		if got != nil || !errors.Is(err, ErrTargetUnverified) {
			t.Errorf("%s: pool %v, err %v, want ErrTargetUnverified", name, got, err)
		}
	}
}

func TestGuardedConnectToASeparateClone(t *testing.T) {
	_, dsn := identityPool(t)
	pool, err := GuardedConnect(context.Background(), Clone{ID: "c1", DSN: dsn}, elsewhere)
	if err != nil {
		t.Fatalf("a clone that is not production connects: %v", err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil ||
		one != 1 {
		t.Fatalf("the guarded pool answers: %d, %v", one, err)
	}
}

// No DB: the DSN is refused before any connection is made.
func TestGuardedConnectInvalidDSN(t *testing.T) {
	for name, tc := range map[string]struct {
		dsn  string
		want error
	}{
		"empty":      {"", ErrTargetUnverified},
		"unparsable": {"postgres://%zz", nil},
		"production host": {"postgres://u:secret@production.invalid:5432/app",
			ErrTargetIsProduction},
	} {
		got, err := GuardedConnect(context.Background(), Clone{ID: "c9", DSN: tc.dsn},
			elsewhere)
		if got != nil || err == nil {
			t.Errorf("%s: pool %v, err %v", name, got, err)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error leaks the password: %v", name, err)
		}
	}
}

func TestGuardedConnectConcurrentCallers(t *testing.T) {
	prodPool, dsn := identityPool(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := IdentitySource(elsewhere)
			if i%2 == 1 {
				src = PoolIdentity(prodPool)
			}
			pool, err := GuardedConnect(context.Background(), Clone{DSN: dsn}, src)
			if pool != nil {
				pool.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if i%2 == 0 && err != nil {
			t.Errorf("caller %d (a separate clone): %v", i, err)
		}
		if i%2 == 1 && !errors.Is(err, ErrTargetIsProduction) {
			t.Errorf("caller %d (production): %v", i, err)
		}
	}
}

func TestReadServerIdentityOfTheTestServerIsStable(t *testing.T) {
	pool, _ := identityPool(t)
	id, err := PoolIdentity(pool)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.StartedAt.IsZero() || id.StartedAt.After(time.Now()) || id.ServerPort == 0 ||
		id.Host == "" || id.Port == 0 {
		t.Fatalf("identity %+v", id)
	}
	again, err := PoolIdentity(pool)(context.Background())
	if err != nil || again != id {
		t.Fatalf("identity is not stable: %+v then %+v (%v)", id, again, err)
	}
}

// No DB: the refusal names hosts and the start time, nothing else.
func TestRefuseProductionMessage(t *testing.T) {
	started := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	prod := ServerIdentity{Host: "db.prod", Port: 5432, StartedAt: started, ServerPort: 5432}
	err := RefuseProduction(prod, ServerIdentity{Host: "10.0.0.5", Port: 6432,
		StartedAt: started, ServerPort: 5432})
	if !errors.Is(err, ErrTargetIsProduction) ||
		!strings.Contains(err.Error(), "10.0.0.5:6432 is production db.prod:5432") ||
		!strings.Contains(err.Error(), "2026-11-09T08:00:00Z") {
		t.Fatalf("refusal = %v", err)
	}
	if err := RefuseProduction(prod, ServerIdentity{Host: "dle.lab", Port: 6001,
		StartedAt: started.Add(time.Hour), ServerPort: 6001}); err != nil {
		t.Fatalf("a separate clone is not refused: %v", err)
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
		// The same postmaster reached over its Unix socket reports no port.
		"unix socket, same postmaster": {ServerIdentity{Host: "/tmp", Port: 5432,
			StartedAt: started, ServerPort: 0}, true},
		"unix socket, other postmaster": {ServerIdentity{Host: "/tmp", Port: 5432,
			StartedAt: started.Add(time.Second), ServerPort: 0}, false},
	} {
		if got := sameServer(prod, tc.clone); got != tc.want {
			t.Errorf("%s: sameServer = %v, want %v", name, got, tc.want)
		}
	}
	if sameServer(ServerIdentity{}, ServerIdentity{}) {
		t.Error("two unknown identities are not proof of the same server")
	}
	socketProd := ServerIdentity{Host: "/var/run/postgresql", Port: 5432, StartedAt: started}
	if !sameServer(socketProd, ServerIdentity{Host: "10.0.0.5", Port: 5432,
		StartedAt: started, ServerPort: 5432}) {
		t.Error("production over its socket and the clone over TCP are the same postmaster")
	}
}
