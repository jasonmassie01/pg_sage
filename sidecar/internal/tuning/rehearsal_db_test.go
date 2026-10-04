package tuning

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Clone rehearsal: when a clone provider is configured, an index is built
// for real on a disposable clone and the case statements are planned
// before and after. The monitored database is never touched.

type fakeCloneProvider struct {
	mu        sync.Mutex
	dsn       string
	age       time.Duration
	createErr error
	created   int
	destroyed int
}

func (p *fakeCloneProvider) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return clone.Clone{}, p.createErr
	}
	p.created++
	return clone.Clone{DSN: p.dsn, ID: "c1", CreatedFrom: time.Now()}, nil
}

func (p *fakeCloneProvider) Destroy(context.Context, clone.Clone) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed++
	return nil
}

func (p *fakeCloneProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return p.age, nil
}

// cloneDB is a disposable database holding a copy of a table.
func cloneDB(t *testing.T) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "tuning clone")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect clone: %v", err)
	}
	defer pool.Close()
	mustExec(t, pool, "CREATE TABLE public.orders (id bigint PRIMARY KEY, customer_id bigint)")
	mustExec(t, pool, "INSERT INTO public.orders SELECT g, g % 5000 "+
		"FROM generate_series(1, 100000) g")
	mustExec(t, pool, "ANALYZE public.orders")
	return dsn
}

func rehearsalRequest() RehearsalRequest {
	return RehearsalRequest{
		DDL: "CREATE INDEX CONCURRENTLY orders_customer_idx ON public.orders (customer_id)",
		Statements: []RehearsalStatement{
			{QueryID: 1, Text: "SELECT * FROM public.orders WHERE customer_id = 42"},
			{QueryID: 2, Text: "SELECT * FROM public.orders WHERE customer_id = $1"},
		}}
}

func TestCloneRehearsal_BuildsAndPlansOnTheClone(t *testing.T) {
	pool := dbPool(t)
	provider := &fakeCloneProvider{dsn: cloneDB(t)}
	r := NewCloneRehearser(provider, RehearsalOptions{MaxCloneAge: time.Hour,
		StatementTimeout: time.Minute, LockTimeout: 5 * time.Second})
	res, err := r.Rehearse(context.Background(), rehearsalRequest())
	if err != nil {
		t.Fatalf("rehearse: %v", err)
	}
	if res.BuildMs <= 0 || res.SizeDeltaBytes <= 0 {
		t.Fatalf("the index was really built: %+v", res)
	}
	if len(res.Queries) != 2 {
		t.Fatalf("queries = %+v", res.Queries)
	}
	literal := res.Queries[0]
	if literal.Error != "" || !(literal.AfterCost < literal.BeforeCost/2) {
		t.Fatalf("the index makes the lookup much cheaper: %+v", literal)
	}
	param := res.Queries[1]
	if serverVersion(t, pool) >= 160000 {
		if param.Error != "" || !(param.AfterCost < param.BeforeCost) {
			t.Fatalf("PG16+ plans the parameterized statement generically: %+v", param)
		}
	} else if param.Error == "" {
		t.Fatalf("before PG16 a parameterized statement cannot be planned: %+v", param)
	}
	if provider.created != 1 || provider.destroyed != 1 {
		t.Fatalf("created %d destroyed %d: the clone is always torn down",
			provider.created, provider.destroyed)
	}
}

func TestCloneRehearsal_Refusals(t *testing.T) {
	provider := &fakeCloneProvider{dsn: "postgres://unused", age: 2 * time.Hour}
	r := NewCloneRehearser(provider, RehearsalOptions{MaxCloneAge: time.Hour})
	if _, err := r.Rehearse(context.Background(), rehearsalRequest()); !errors.Is(err,
		ErrCloneStale) {
		t.Fatalf("a stale snapshot: %v", err)
	}
	provider.age, provider.createErr = 0, errFake
	if _, err := r.Rehearse(context.Background(), rehearsalRequest()); !errors.Is(err,
		ErrCloneUnavailable) || !errors.Is(err, errFake) {
		t.Fatalf("a provider failure stays distinguishable: %v", err)
	}
	for _, ddl := range []string{"DROP TABLE public.orders",
		"CREATE INDEX a ON public.orders (id); DROP TABLE public.orders",
		"ALTER SYSTEM SET work_mem = '1GB'"} {
		req := rehearsalRequest()
		req.DDL = ddl
		if _, err := r.Rehearse(context.Background(), req); !errors.Is(err,
			ErrRehearsalRefused) {
			t.Fatalf("%q: %v", ddl, err)
		}
	}
	if provider.created != 0 {
		t.Fatalf("no clone for refused requests: %d", provider.created)
	}
}

func TestCloneRehearsal_FailedBuildStillDestroysTheClone(t *testing.T) {
	provider := &fakeCloneProvider{dsn: cloneDB(t)}
	r := NewCloneRehearser(provider, RehearsalOptions{MaxCloneAge: time.Hour,
		StatementTimeout: time.Minute, LockTimeout: 5 * time.Second})
	req := rehearsalRequest()
	req.DDL = "CREATE INDEX CONCURRENTLY bad_idx ON public.orders (no_such_column)"
	if _, err := r.Rehearse(context.Background(), req); err == nil {
		t.Fatal("a failing build is an error")
	}
	if provider.destroyed != 1 {
		t.Fatalf("destroyed = %d", provider.destroyed)
	}
}
