package managedparam

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisors-03: an Azure server describes parameters one at a time, so the
// worker asks a ParameterResolver for exactly the parameters its intents
// name, and caches the answer per parameter set.

type fakeParamResolver struct {
	mu     sync.Mutex
	target Target
	err    error
	asked  [][]string
	plain  int
}

func (f *fakeParamResolver) Target(context.Context) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain++
	return f.target, f.err
}

func (f *fakeParamResolver) TargetFor(_ context.Context, names []string) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(names))
	return f.target, f.err
}

func (f *fakeParamResolver) calls() ([][]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked), f.plain
}

func newAzureWorker(t *testing.T, pool *pgxpool.Pool, r Resolver) *Worker {
	t.Helper()
	w, err := NewWorker(WorkerOptions{Database: "orders", Provider: "azure-flexible",
		Pool: pool, Store: NewStore(pool), Resolver: r})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

func TestWorkerAzureDescribesTheIntentParameters(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeParamResolver{target: azureTarget()}
	w := newAzureWorker(t, pool, r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "azure", "work_mem", "64MB"))
	insertIntentFinding(t, pool, "instance:shared_buffers",
		intent(t, "azure", "shared_buffers", "2GB"))
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 2 || res.TargetError != "" || len(res.BuildErrors) != 0 {
		t.Fatalf("cycle = %+v, %v", res, err)
	}
	asked, plain := r.calls()
	if plain != 0 || len(asked) != 1 ||
		!slices.Equal(asked[0], []string{"shared_buffers", "work_mem"}) {
		t.Fatalf("resolver asked %v (plain Target calls %d), want the two intents", asked,
			plain)
	}
	recs, err := w.store.List(ctx, nil, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records = %+v, %v", recs, err)
	}
	for _, rec := range recs {
		p := rec.Proposal
		if p.Provider != "azure" || p.Mechanism != MechanismServerParameter ||
			!strings.HasPrefix(p.CLI, "az postgres flexible-server parameter set") ||
			rec.Status != StatusPending || p.AutoApply || !p.RequiresApproval {
			t.Fatalf("record = %+v", rec)
		}
	}
	// Same intents: the cached target is reused.
	if _, err := w.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if asked, _ = r.calls(); len(asked) != 1 {
		t.Fatalf("same parameter set re-resolved: %v", asked)
	}
	// A new intent changes the set, so the server is asked again.
	insertIntentFinding(t, pool, "instance:max_wal_size",
		intent(t, "azure", "max_wal_size", "4GB"))
	if _, err := w.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	asked, _ = r.calls()
	if len(asked) != 2 ||
		!slices.Equal(asked[1], []string{"max_wal_size", "shared_buffers", "work_mem"}) {
		t.Fatalf("resolver asked %v, want the three intents second", asked)
	}
}

// A 403 from the server surfaces in TargetError, naming the missing
// permission, and the proposals fall back to fill-in templates.
func TestWorkerAzureForbiddenSurfacesInTargetError(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeParamResolver{err: errors.New("azure: read parameter work_mem: ARM 403 " +
		"AuthorizationFailed: denied; the identity needs Microsoft.DBforPostgreSQL/" +
		"flexibleServers/configurations/read on server orders-flex")}
	w := newAzureWorker(t, pool, r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "azure", "work_mem", "64MB"))
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 1 || !strings.Contains(res.TargetError,
		"configurations/read") {
		t.Fatalf("cycle = %+v, %v", res, err)
	}
	recs, err := w.store.List(ctx, nil, 10)
	if err != nil || len(recs) != 1 || !strings.HasPrefix(recs[0].Proposal.CLI, "#") ||
		len(recs[0].Proposal.Blockers) == 0 {
		t.Fatalf("records = %+v, %v", recs, err)
	}
}

// Azure may describe a parameter in another unit than pg_settings: the
// worker still sees the running value as the proposed one and marks it
// applied (max_wal_size 1GB runs by default; Azure reports it in KB).
func TestWorkerAzureMarksAppliedAcrossUnits(t *testing.T) {
	pool, ctx := testPool(t)
	target := azureTarget()
	target.Params["max_wal_size"] = GroupParam{Value: "1048576", Default: "1048576",
		Source: "system", Unit: "KB", ApplyType: "dynamic", Modifiable: true}
	w := newAzureWorker(t, pool, &fakeParamResolver{target: target})
	var running string
	if err := pool.QueryRow(ctx, `SELECT setting FROM pg_settings
		WHERE name = 'max_wal_size'`).Scan(&running); err != nil || running != "1024" {
		t.Fatalf("needs the default max_wal_size of 1024 MB, got %q (%v)", running, err)
	}
	insertIntentFinding(t, pool, "instance:max_wal_size",
		intent(t, "azure", "max_wal_size", "1GB"))
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 1 || res.Applied != 1 {
		t.Fatalf("cycle = %+v, %v; want one proposal marked applied", res, err)
	}
	recs, err := w.store.List(ctx, nil, 10)
	if err != nil || len(recs) != 1 || recs[0].Status != StatusApplied ||
		recs[0].Proposal.Value != "1048576" {
		t.Fatalf("records = %+v, %v", recs, err)
	}
}

// No intents: a parameter resolver is asked for nothing at all.
func TestWorkerAzureWithoutIntentsAsksNothing(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeParamResolver{target: azureTarget()}
	w := newAzureWorker(t, pool, r)
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 0 {
		t.Fatalf("cycle = %+v, %v", res, err)
	}
	if asked, plain := r.calls(); plain != 0 || len(asked) != 1 || len(asked[0]) != 0 {
		t.Fatalf("asked %v plain %d, want one empty description", asked, plain)
	}
}
