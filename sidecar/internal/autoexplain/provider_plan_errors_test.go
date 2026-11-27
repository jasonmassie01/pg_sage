package autoexplain

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// unreachablePool never connects until a statement runs, so validation
// failures prove no statement was attempted.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=2&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func validProviderPlan() ProviderPlan {
	now := time.Now().UTC()
	return ProviderPlan{ObservedPlan: ObservedPlan{QueryID: 7, Query: "SELECT 1",
		JSON: []byte(`[{"Plan":{"Node Type":"Result","Total Cost":1}}]`), TotalCost: 1,
		ExecutionMS: 2, CapturedAt: now.Add(-time.Minute)}, ScopeHash: "scope", ComputeID: "c",
		EventID: "e", ReceivedAt: now}
}

func TestStoreProviderPlanTypesInvalidPlans(t *testing.T) {
	pool := unreachablePool(t)
	cases := map[string]func(*ProviderPlan){
		"missing event":  func(p *ProviderPlan) { p.EventID = "" },
		"zero query id":  func(p *ProviderPlan) { p.QueryID = 0 },
		"invalid json":   func(p *ProviderPlan) { p.JSON = []byte(`{`) },
		"empty json":     func(p *ProviderPlan) { p.JSON = nil },
		"nan cost":       func(p *ProviderPlan) { p.TotalCost = math.NaN() },
		"negative time":  func(p *ProviderPlan) { p.ExecutionMS = -1 },
		"oversized json": func(p *ProviderPlan) { p.JSON = make([]byte, 1<<20+1) },
		// The root normalizes, but planhash rejects the tree below it.
		"child without node type": func(p *ProviderPlan) {
			p.JSON = []byte(`{"Plan":{"Node Type":"Result","Total Cost":1,` +
				`"Plans":[{"Total Cost":1}]}}`)
		},
		"plans not an array": func(p *ProviderPlan) {
			p.JSON = []byte(`{"Plan":{"Node Type":"Result","Total Cost":1,"Plans":"x"}}`)
		},
		"empty plan array": func(p *ProviderPlan) { p.JSON = []byte(`[]`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := validProviderPlan()
			mutate(&p)
			err := StoreProviderPlan(context.Background(), pool, p)
			if !errors.Is(err, ErrInvalidProviderPlan) {
				t.Fatalf("want ErrInvalidProviderPlan, got %v", err)
			}
			var connect *pgconn.ConnectError
			if errors.As(err, &connect) {
				t.Fatal("validation failure reached the database")
			}
		})
	}
}

func TestStoreProviderPlanOtherErrorsAreNotInvalidPlans(t *testing.T) {
	p := validProviderPlan()
	if err := StoreProviderPlan(context.Background(), nil, p); err == nil ||
		errors.Is(err, ErrInvalidProviderPlan) {
		t.Fatalf("a missing sink pool is not a property of the plan: %v", err)
	}
	p.Query = "/* pg_sage */ SELECT 1"
	err := StoreProviderPlan(context.Background(), unreachablePool(t), p)
	if !errors.Is(err, ErrSelfStatement) || errors.Is(err, ErrInvalidProviderPlan) {
		t.Fatalf("self statement classified as %v", err)
	}
	// A valid plan against an unreachable sink is a transient failure.
	err = StoreProviderPlan(context.Background(), unreachablePool(t), validProviderPlan())
	if err == nil || errors.Is(err, ErrInvalidProviderPlan) {
		t.Fatalf("sink outage classified as an invalid plan: %v", err)
	}
}
