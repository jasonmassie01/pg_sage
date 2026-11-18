package tuning

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-04a: every rehearsal clone is receipted, and its receipt moves
// creating, ready, destroying, destroyed, including a rehearsal whose
// context is cancelled; two databases rehearsing at once get two receipts
// and two clones.

const lab04aDeployment = "6a1e04a0-0000-4000-8000-000000000a4a"

// statusProbe is an inner provider that reads its receipt's status at each
// step of the clone's life.
type statusProbe struct {
	pool  *pgxpool.Pool
	scope string
	dsn   string
	mu    sync.Mutex
	n     int
	seen  map[string][]string // clone id -> statuses observed
}

func (p *statusProbe) status(ctx context.Context) string {
	var s string
	if err := p.pool.QueryRow(context.WithoutCancel(ctx), "SELECT string_agg(status, ',' "+
		"ORDER BY id) FROM sage.clone_instances WHERE scope = $1 AND status <> 'destroyed'",
		p.scope).Scan(&s); err != nil {
		return "error: " + err.Error()
	}
	return s
}

func (p *statusProbe) Create(ctx context.Context, _ clone.CloneSpec) (clone.Clone, error) {
	st := p.status(ctx)
	p.mu.Lock()
	p.n++
	id := fmt.Sprintf("%s-%d", p.scope, p.n)
	p.seen[id] = append(p.seen[id], "create:"+st)
	p.mu.Unlock()
	return clone.Clone{ID: id, DSN: p.dsn, CreatedFrom: time.Now()}, nil
}

func (p *statusProbe) Destroy(ctx context.Context, c clone.Clone) error {
	st := p.status(ctx)
	p.mu.Lock()
	p.seen[c.ID] = append(p.seen[c.ID], "destroy:"+st)
	p.mu.Unlock()
	return nil
}

func (p *statusProbe) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

func receiptedRehearser(t *testing.T, pool *pgxpool.Pool, scope, dsn string,
	src clone.IdentitySource) (Rehearser, *statusProbe) {
	t.Helper()
	probe := &statusProbe{pool: pool, scope: scope, dsn: dsn, seen: map[string][]string{}}
	rp, err := clone.NewReceiptingProvider(probe, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: "dle", Scope: scope,
			Deployment: func(context.Context) (string, error) { return lab04aDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.clone_instances WHERE scope = $1", scope)
	})
	return analyzeRehearser(rp, src), probe
}

func finalStatuses(t *testing.T, pool *pgxpool.Pool, scope string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT status FROM sage.clone_instances "+
		"WHERE scope = $1 ORDER BY id", scope)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestRehearsalReceipts_EveryStateIncludingACancelledContext(t *testing.T) {
	pool := dbPool(t)
	dsn := cloneDB(t)
	scope := fmt.Sprintf("https://dle-states-%d.invalid", time.Now().UnixNano())
	var readyAt string
	var probe *statusProbe
	src := func(ctx context.Context) (clone.ServerIdentity, error) {
		readyAt = probe.status(ctx)
		return notProduction(ctx)
	}
	var reh Rehearser
	reh, probe = receiptedRehearser(t, pool, scope, dsn, src)
	res, err := reh.Rehearse(context.Background(), rehearsalRequest())
	if err != nil || res.ReceiptID == 0 {
		t.Fatalf("rehearse: receipt %d, %v", res.ReceiptID, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := func(ctx context.Context) (clone.ServerIdentity, error) {
		cancel()
		return clone.ServerIdentity{}, ctx.Err()
	}
	cancelled, _ := receiptedRehearser(t, pool, scope+"-c", dsn, cancelling)
	if _, err := cancelled.Rehearse(ctx, rehearsalRequest()); err == nil {
		t.Fatal("a cancelled rehearsal is an error")
	}
	if readyAt != "ready" {
		t.Fatalf("on the clone the receipt is ready, got %q", readyAt)
	}
	for id, seen := range probe.seen {
		if len(seen) != 2 || seen[0] != "create:creating" || seen[1] != "destroy:destroying" {
			t.Fatalf("clone %s: statuses %v", id, seen)
		}
	}
	for _, s := range []string{scope, scope + "-c"} {
		if got := finalStatuses(t, pool, s); len(got) != 1 || got[0] != clone.StatusDestroyed {
			t.Fatalf("scope %s: final receipts %v, want one destroyed", s, got)
		}
	}
}

func TestRehearsalReceipts_TwoDatabasesAtOnce(t *testing.T) {
	pool := dbPool(t)
	dsns := []string{cloneDB(t), cloneDB(t)}
	base := fmt.Sprintf("https://dle-pair-%d.invalid", time.Now().UnixNano())
	type outcome struct {
		res RehearsalResult
		err error
	}
	outs := make([]outcome, 2)
	probes := make([]*statusProbe, 2)
	var wg sync.WaitGroup
	for i := range outs {
		reh, probe := receiptedRehearser(t, pool, fmt.Sprintf("%s/%d", base, i), dsns[i],
			notProduction)
		probes[i] = probe
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each database rehearses on its own clone.
			req := rehearsalRequest()
			req.DDL = fmt.Sprintf("CREATE INDEX CONCURRENTLY orders_pair_%d_idx ON "+
				"public.orders (customer_id)", i)
			outs[i].res, outs[i].err = reh.Rehearse(context.Background(), req)
		}(i)
	}
	wg.Wait()
	if outs[0].err != nil || outs[1].err != nil {
		t.Fatalf("rehearsals: %v / %v", outs[0].err, outs[1].err)
	}
	if outs[0].res.ReceiptID == 0 || outs[0].res.ReceiptID == outs[1].res.ReceiptID {
		t.Fatalf("two receipts: %d and %d", outs[0].res.ReceiptID, outs[1].res.ReceiptID)
	}
	for i, p := range probes {
		if p.n != 1 {
			t.Fatalf("database %d made %d clones, want 1", i, p.n)
		}
		got := finalStatuses(t, pool, fmt.Sprintf("%s/%d", base, i))
		if len(got) != 1 || got[0] != clone.StatusDestroyed {
			t.Fatalf("database %d receipts %v", i, got)
		}
	}
}
