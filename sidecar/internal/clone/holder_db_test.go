package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lab-05a: a receipt names the process holding its clone and when it last
// said so. A clone whose holder stopped heartbeating (a killed sidecar) is
// reaped by the next process before its TTL; a live holder's clone never
// is, whatever the timing.

// holderFake is a provider that keeps its resources by name, so a test
// can see what exists, and can find one by the name a receipt gave it.
type holderFake struct {
	kind       Kind
	mu         sync.Mutex
	resources  map[string]string // ref -> resource name
	creates    int
	createErr  error // returned after the resource is made (an uncertain create)
	failCreate error // returned before anything is made
	destroyErr []error
	findErr    error
	finds      int
}

func newHolderFake(kind Kind) *holderFake {
	return &holderFake{kind: kind, resources: map[string]string{}}
}

func (f *holderFake) Create(_ context.Context, spec CloneSpec) (Clone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return Clone{}, f.failCreate
	}
	f.creates++
	ref := fmt.Sprintf("ref-%d", f.creates)
	f.resources[ref] = spec.ResourceName
	if f.createErr != nil {
		return Clone{}, f.createErr
	}
	return Clone{ID: ref, DSN: "postgres://clone.invalid/app", CreatedFrom: time.Now()}, nil
}

func (f *holderFake) Destroy(_ context.Context, c Clone) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.destroyErr) > 0 {
		err := f.destroyErr[0]
		f.destroyErr = f.destroyErr[1:]
		return err
	}
	delete(f.resources, c.ID)
	return nil
}

func (f *holderFake) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

func (f *holderFake) Describe(context.Context) (Capability, error) {
	return Capability{Kind: f.kind, TypicalReady: time.Minute, MaxTTL: time.Hour}, nil
}

func (f *holderFake) FindByName(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if f.findErr != nil {
		return "", false, f.findErr
	}
	for ref, n := range f.resources {
		if n == name {
			return ref, true, nil
		}
	}
	return "", false, nil
}

func (f *holderFake) exists(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.resources[ref]
	return ok
}

func (f *holderFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resources)
}

func holderConfig(holder string, lifetime context.Context) ReceiptingConfig {
	cfg := rehearsalConfig()
	cfg.Holder, cfg.Lifetime = holder, lifetime
	cfg.Backoff = NewCreateBackoff(time.Now)
	return cfg
}

func holderReceipting(t *testing.T, inner Provider, s *ReceiptStore,
	cfg ReceiptingConfig) *ReceiptingProvider {
	t.Helper()
	p, err := NewReceiptingProvider(inner, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func holderReaper(s *ReceiptStore, inner Provider, holder string) Reaper {
	return Reaper{Store: s, Provider: inner, Scope: reaperScope, Purpose: PurposeRehearsal,
		Holder: holder}
}

func ageHeartbeat(t *testing.T, pool *pgxpool.Pool, id int64, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - $2::interval WHERE id = $1`, id,
		fmt.Sprintf("%d milliseconds", age.Milliseconds())); err != nil {
		t.Fatal(err)
	}
}

func heartbeatOf(t *testing.T, pool *pgxpool.Pool, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(), `SELECT heartbeat_at FROM
		sage.clone_instances WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestHolder_CreateHeartbeatDestroyKeepsTheEstimate(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	cfg := holderConfig("holder-a", ctx)
	cfg.HeartbeatEvery = 20 * time.Millisecond
	p := holderReceipting(t, inner, s, cfg)
	c, err := p.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	r := onlyReceipt(t, pool)
	if r.HolderID != "holder-a" || r.HeartbeatAt.IsZero() || r.Estimate == nil {
		t.Fatalf("receipt %+v lacks its holder, heartbeat or estimate", r)
	}
	if r.Estimate.Kind != KindBranch || r.Estimate.InstanceHours != 1.25 ||
		!strings.Contains(r.Estimate.Label, "unpriced") {
		t.Fatalf("estimate %+v, want an unpriced branch of 1.25 instance-hours (75m)",
			*r.Estimate)
	}
	ageHeartbeat(t, pool, r.ID, 10*time.Minute)
	old := heartbeatOf(t, pool, r.ID)
	deadline := time.Now().Add(5 * time.Second)
	for !heartbeatOf(t, pool, r.ID).After(old.Add(9 * time.Minute)) {
		if time.Now().After(deadline) {
			t.Fatal("the holder did not heartbeat its ready clone")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Destroy(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroyed || inner.count() != 0 {
		t.Fatalf("after destroy: receipt %s, %d resources", got.Status, inner.count())
	}
}

func TestHolder_KilledSidecarIsReapedByTheNextProcessBeforeTTL(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	life, kill := context.WithCancel(ctx)
	a := holderReceipting(t, inner, s, holderConfig("holder-a", life))
	c, err := a.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	kill() // the process dies: no destroy, no more heartbeats
	r := onlyReceipt(t, pool)
	ageHeartbeat(t, pool, r.ID, DefaultHolderStaleAfter+time.Second)
	if !r.ExpiresAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("receipt expires %s: the test needs a clone well before its TTL", r.ExpiresAt)
	}
	report, err := holderReaper(s, inner, "holder-b").Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0].ID != r.ID || inner.exists(c.ID) {
		t.Fatalf("first pass of the next process: %+v; clone exists %v", report,
			inner.exists(c.ID))
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroyed {
		t.Fatalf("receipt %s, want destroyed", got.Status)
	}
}

func TestHolder_LiveOrOwnClonesAreNotReaped(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	life, kill := context.WithCancel(ctx)
	defer kill()
	a := holderReceipting(t, inner, s, holderConfig("holder-a", life))
	c, err := a.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	r := onlyReceipt(t, pool)
	// A fresh heartbeat: another process leaves it alone.
	if rep, err := holderReaper(s, inner, "holder-b").Reap(ctx); err != nil ||
		len(rep.Destroyed)+len(rep.Unresolved)+len(rep.Failed) != 0 {
		t.Fatalf("a live clone was acted on: %+v, %v", rep, err)
	}
	// A stale heartbeat of this very process: the holder is alive, so only
	// the TTL applies (today's rule).
	ageHeartbeat(t, pool, r.ID, time.Hour)
	if rep, err := holderReaper(s, inner, "holder-a").Reap(ctx); err != nil ||
		len(rep.Destroyed) != 0 {
		t.Fatalf("a process reaped its own clone: %+v, %v", rep, err)
	}
	// No holder at all (a receipt from before lab-05a): only the TTL applies.
	legacy := seedReceipt(t, s, pool, "rehearsal-legacy", seeded{scope: reaperScope,
		purpose: PurposeRehearsal, ref: "legacy-ref"})
	if legacy.HolderID != "" {
		t.Fatalf("a receipt written by Record has holder %q", legacy.HolderID)
	}
	if rep, err := holderReaper(s, inner, "holder-b").Reap(ctx); err != nil ||
		len(rep.Destroyed) != 1 || rep.Destroyed[0].ID != r.ID {
		t.Fatalf("stale foreign clone reaped, legacy kept: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, legacy.ID); got.Status != StatusReady {
		t.Fatalf("legacy receipt %s, want ready (no holder: TTL only)", got.Status)
	}
	if inner.exists(c.ID) {
		t.Fatal("the stale clone still exists")
	}
	// Without a holder id the reaper does no liveness reaping at all.
	if rep, err := holderReaper(s, inner, "").Reap(ctx); err != nil || len(rep.Destroyed) != 0 {
		t.Fatalf("a reaper without a holder id reaped: %+v, %v", rep, err)
	}
}

func TestHolder_HeartbeatBoundaryIsFiveMinutes(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	life, kill := context.WithCancel(ctx)
	a := holderReceipting(t, inner, s, holderConfig("holder-a", life))
	if _, err := a.Create(ctx, CloneSpec{}); err != nil {
		t.Fatal(err)
	}
	kill()
	r := onlyReceipt(t, pool)
	beat := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances SET heartbeat_at = $2
		WHERE id = $1`, r.ID, beat); err != nil {
		t.Fatal(err)
	}
	reaper := holderReaper(s, inner, "holder-b")
	reaper.AsOf = beat.Add(5 * time.Minute)
	if rep, err := reaper.Reap(ctx); err != nil || len(rep.Destroyed) != 0 {
		t.Fatalf("a heartbeat exactly 5 minutes old was reaped: %+v, %v", rep, err)
	}
	reaper.AsOf = beat.Add(5*time.Minute + time.Second)
	if rep, err := reaper.Reap(ctx); err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("a heartbeat 5m1s old was not reaped: %+v, %v", rep, err)
	}
}

func TestHolder_FifthCreateInAnHourIsRefused(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	rehearsal := holderReceipting(t, inner, s, holderConfig("holder-a", ctx))
	sandboxCfg := holderConfig("holder-a", ctx)
	sandboxCfg.Purpose = PurposeSandbox
	sandbox := holderReceipting(t, inner, s, sandboxCfg)
	for i, p := range []*ReceiptingProvider{rehearsal, sandbox, rehearsal, sandbox} {
		if _, err := p.Create(ctx, CloneSpec{}); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
	}
	_, err := rehearsal.Create(ctx, CloneSpec{})
	if !errors.Is(err, ErrCreateRateLimited) ||
		!strings.Contains(err.Error(), "clone.max_creates_per_hour") ||
		!strings.Contains(err.Error(), "4") {
		t.Fatalf("fifth create: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 || inner.creates != 4 {
		t.Fatalf("%d receipts and %d provider creates, want 4 of each", n, inner.creates)
	}
	// An hour-old create no longer counts.
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET created_at = now() - interval '1 hour 1 second'
		WHERE id = (SELECT min(id) FROM sage.clone_instances)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rehearsal.Create(ctx, CloneSpec{}); err != nil {
		t.Fatalf("create after the oldest aged out: %v", err)
	}
}

func TestHolder_ConcurrentCreatesHonourTheHourlyLimit(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	p := holderReceipting(t, inner, s, holderConfig("holder-a", ctx))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.Create(ctx, CloneSpec{})
		}(i)
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrCreateRateLimited):
			limited++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if ok != 4 || limited != 4 || n != 4 {
		t.Fatalf("%d created, %d limited, %d receipts; want 4, 4, 4", ok, limited, n)
	}
}

func TestHolder_ConcurrentHeartbeatAndReapNeverReapALiveClone(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	life, kill := context.WithCancel(ctx)
	defer kill()
	cfg := holderConfig("holder-a", life)
	cfg.HeartbeatEvery = 10 * time.Millisecond
	c, err := holderReceipting(t, inner, s, cfg).Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	reaper := holderReaper(s, inner, "holder-b")
	reaper.StaleAfter = 300 * time.Millisecond
	var wg sync.WaitGroup
	var mu sync.Mutex
	var reaped []Receipt
	stop := time.Now().Add(1500 * time.Millisecond)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				rep, err := reaper.Reap(ctx)
				if err != nil {
					t.Errorf("reap: %v", err)
					return
				}
				mu.Lock()
				reaped = append(reaped, rep.Destroyed...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(reaped) != 0 || !inner.exists(c.ID) {
		t.Fatalf("a live, heartbeating clone was reaped: %+v", reaped)
	}
	kill()
	time.Sleep(reaper.StaleAfter + 200*time.Millisecond)
	rep, err := reaper.Reap(ctx)
	if err != nil || len(rep.Destroyed) != 1 || inner.exists(c.ID) {
		t.Fatalf("after the holder stopped: %+v, %v", rep, err)
	}
	_ = pool
}

func TestHolder_DestroyFailureStaysDestroyingAndIsRetried(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	life, kill := context.WithCancel(ctx)
	c, err := holderReceipting(t, inner, s, holderConfig("holder-a", life)).Create(ctx,
		CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	kill()
	r := onlyReceipt(t, pool)
	ageHeartbeat(t, pool, r.ID, time.Hour)
	inner.destroyErr = []error{errors.New("provider 503")}
	reaper := holderReaper(s, inner, "holder-b")
	rep, err := reaper.Reap(ctx)
	if err != nil || rep.Failed[r.ID] == nil || len(rep.Destroyed) != 0 {
		t.Fatalf("failing teardown: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroying {
		t.Fatalf("after a failed teardown the receipt is %s, want destroying", got.Status)
	}
	if rep, err = reaper.Reap(ctx); err != nil || len(rep.Destroyed) != 1 || inner.exists(c.ID) {
		t.Fatalf("retry: %+v, %v", rep, err)
	}
}

func TestHolder_ProviderThrottlingBacksOffBeforeAnyReceipt(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	inner.failCreate = &ProviderStatusError{Op: "fake create", StatusCode: 429}
	p := holderReceipting(t, inner, s, holderConfig("holder-a", ctx))
	if _, err := p.Create(ctx, CloneSpec{}); !Throttled(err) {
		t.Fatalf("first create: %v, want the provider's 429", err)
	}
	if got := onlyReceipt(t, pool); got.Status != StatusFailed {
		t.Fatalf("the throttled create's receipt is %s, want failed", got.Status)
	}
	inner.failCreate = nil
	_, err := p.Create(ctx, CloneSpec{})
	if !errors.Is(err, ErrProviderBackoff) || inner.creates != 0 {
		t.Fatalf("create during backoff: %v after %d creates", err, inner.creates)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d receipts: a create in backoff wrote one", n)
	}
}

func TestHolder_FullCopyNeedsOptInAndRecordsItsEstimate(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindFullCopy)
	p := holderReceipting(t, inner, s, holderConfig("holder-a", ctx))
	if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, ErrFullCopyNotAllowed) {
		t.Fatalf("full copy without opt-in: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || inner.creates != 0 {
		t.Fatalf("refused full copy left %d receipts and %d creates", n, inner.creates)
	}
	cfg := holderConfig("holder-a", ctx)
	cfg.AllowFullCopy = true
	cfg.Rate = &Rate{PerHour: 2, Currency: "USD", AsOf: "2026-11-01", Source: "orders"}
	if _, err := holderReceipting(t, inner, s, cfg).Create(ctx, CloneSpec{}); err != nil {
		t.Fatal(err)
	}
	r := onlyReceipt(t, pool)
	if r.Estimate == nil || r.Estimate.Kind != KindFullCopy || !r.Estimate.Priced ||
		r.Estimate.Amount != 2.5 || r.Estimate.Source != "orders" {
		t.Fatalf("full copy estimate %+v, want 2.50 USD from orders", r.Estimate)
	}
}
