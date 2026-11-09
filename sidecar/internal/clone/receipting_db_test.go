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

// migration-07: a rehearsal clone holds production data, so its receipt is
// written before the provider is asked for it and is kept current through
// teardown.

// fakeInner is a clone adapter that records what the receipt table said
// when it was called.
type fakeInner struct {
	pool       *pgxpool.Pool
	createErr  error
	destroyErr error
	emptyID    bool

	mu              sync.Mutex
	n               int
	creatingAtCall  []int
	statusAtDestroy []string
	destroyed       []string
}

func (f *fakeInner) Create(ctx context.Context, _ CloneSpec) (Clone, error) {
	var creating int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances
		WHERE status = 'creating' AND ref IS NULL`).Scan(&creating); err != nil {
		return Clone{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creatingAtCall = append(f.creatingAtCall, creating)
	if f.createErr != nil {
		return Clone{}, f.createErr
	}
	f.n++
	id := fmt.Sprintf("clone-%d", f.n)
	if f.emptyID {
		id = ""
	}
	return Clone{ID: id, DSN: "postgres://clone.invalid/app", CreatedFrom: time.Now()}, nil
}

func (f *fakeInner) Destroy(ctx context.Context, c Clone) error {
	var status string
	err := f.pool.QueryRow(ctx, `SELECT COALESCE(max(status), 'none') FROM
		sage.clone_instances WHERE ref = $1`, c.ID).Scan(&status)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		return err
	}
	f.statusAtDestroy = append(f.statusAtDestroy, status)
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, c.ID)
	return nil
}

func (f *fakeInner) SnapshotAge(context.Context) (time.Duration, error) {
	return 3 * time.Minute, nil
}

func staticDeployment(id string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return id, nil }
}

func rehearsalConfig() ReceiptingConfig {
	return ReceiptingConfig{Adapter: "dle", Scope: "https://dle.invalid",
		Deployment: staticDeployment(testDeployment), Purpose: PurposeRehearsal,
		TTL: 75 * time.Minute}
}

func newReceipting(t *testing.T, inner Provider, s *ReceiptStore) *ReceiptingProvider {
	t.Helper()
	p, err := NewReceiptingProvider(inner, s, rehearsalConfig())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func receiptByID(t *testing.T, pool *pgxpool.Pool, id int64) Receipt {
	t.Helper()
	r, err := scanReceipt(pool.QueryRow(context.Background(),
		`SELECT `+receiptColumns+` FROM sage.clone_instances WHERE id = $1`, id))
	if err != nil {
		t.Fatalf("receipt %d: %v", id, err)
	}
	return r
}

func onlyReceipt(t *testing.T, pool *pgxpool.Pool) Receipt {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM sage.clone_instances
		ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("no receipt: %v", err)
	}
	return receiptByID(t, pool, id)
}

func TestReceipting_TransitionsThroughDestroyed(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool}
	p := newReceipting(t, inner, s)
	started := time.Now()
	c, err := p.Create(ctx, CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.creatingAtCall) != 1 || inner.creatingAtCall[0] != 1 {
		t.Fatalf("receipts creating when the adapter ran: %v, want [1]", inner.creatingAtCall)
	}
	r := onlyReceipt(t, pool)
	if r.Status != StatusReady || r.Ref != c.ID || r.Purpose != PurposeRehearsal ||
		r.Adapter != "dle" || r.Scope != "https://dle.invalid" ||
		!strings.HasPrefix(r.Name, "rehearsal-") || r.DeploymentID != testDeployment {
		t.Fatalf("after create: %+v (clone %s)", r, c.ID)
	}
	ttl := r.ExpiresAt.Sub(started)
	if ttl < 74*time.Minute || ttl > 76*time.Minute {
		t.Fatalf("expiry %s after create, want the 75m TTL", ttl)
	}
	if err := p.Destroy(ctx, c); err != nil {
		t.Fatal(err)
	}
	if len(inner.statusAtDestroy) != 1 || inner.statusAtDestroy[0] != StatusDestroying {
		t.Fatalf("receipt status during teardown: %v", inner.statusAtDestroy)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroyed || got.Ref != c.ID {
		t.Fatalf("after destroy: %+v", got)
	}
	if err := p.Destroy(ctx, c); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("second destroy of the same clone: %v", err)
	}
}

func TestReceipting_DestroyFailureLeavesDestroying(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool, destroyErr: errors.New("dle: 503")}
	p := newReceipting(t, inner, s)
	c, err := p.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Destroy(ctx, c)
	if err == nil || !strings.Contains(err.Error(), "dle: 503") ||
		!strings.Contains(err.Error(), "reaper") {
		t.Fatalf("destroy failure: %v", err)
	}
	if got := onlyReceipt(t, pool); got.Status != StatusDestroying || got.Ref != c.ID {
		t.Fatalf("after a failed teardown: %+v", got)
	}
}

func TestReceipting_CreateFailureClosesReceipt(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"refused": {errors.New("DLE create failed with status 400"), StatusFailed},
		"timed out": {fmt.Errorf("DLE create request: %w", context.DeadlineExceeded),
			StatusCreateUncertain},
		"cancelled": {fmt.Errorf("DLE create request: %w", context.Canceled),
			StatusCreateUncertain},
	} {
		t.Run(name, func(t *testing.T) {
			s, pool, ctx := receiptStore(t)
			p := newReceipting(t, &fakeInner{pool: pool, createErr: tc.err}, s)
			if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, tc.err) {
				t.Fatalf("create error %v, want %v", err, tc.err)
			}
			if got := onlyReceipt(t, pool); got.Status != tc.want || got.Ref != "" {
				t.Fatalf("receipt %+v, want status %s and no ref", got, tc.want)
			}
		})
	}
}

func TestReceipting_NoReceiptNoClone(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	cases := map[string]*ReceiptingProvider{
		"nil store":  newReceipting(t, &fakeInner{pool: pool}, nil),
		"nil pool":   newReceipting(t, &fakeInner{pool: pool}, NewReceiptStore(nil)),
		"bad uuid":   nil,
		"deployment": nil,
	}
	badUUID := rehearsalConfig()
	badUUID.Deployment = staticDeployment("not-a-uuid")
	cases["bad uuid"], _ = NewReceiptingProvider(&fakeInner{pool: pool}, s, badUUID)
	noDeployment := rehearsalConfig()
	noDeployment.Deployment = func(context.Context) (string, error) {
		return "", errors.New("relation sage.sre_deployments does not exist")
	}
	cases["deployment"], _ = NewReceiptingProvider(&fakeInner{pool: pool}, s, noDeployment)
	for name, p := range cases {
		if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, ErrNoReceiptStore) {
			t.Errorf("%s: %v, want ErrNoReceiptStore", name, err)
		}
		if inner := p.inner.(*fakeInner); len(inner.creatingAtCall) != 0 {
			t.Errorf("%s: the adapter was asked for a clone without a receipt", name)
		}
	}
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.clone_instances`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("receipts written: %d %v", n, err)
	}
}

func TestReceipting_UnrecordedCloneIsDestroyed(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool, emptyID: true}
	p := newReceipting(t, inner, s)
	if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("a clone without an id cannot complete its receipt: %v", err)
	}
	if len(inner.destroyed) != 1 {
		t.Fatalf("the unrecorded clone was not torn down: %v", inner.destroyed)
	}
	if got := onlyReceipt(t, pool); got.Status != StatusFailed {
		t.Fatalf("receipt of the unrecorded clone: %+v", got)
	}
}

func TestReceipting_DestroyUnknownClone(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool}
	p := newReceipting(t, inner, s)
	if err := p.Destroy(ctx, Clone{ID: "someone-elses"}); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("destroy of a clone this provider never made: %v", err)
	}
	if len(inner.statusAtDestroy) != 0 {
		t.Fatal("the adapter was asked to destroy a clone with no receipt")
	}
}

func TestReceipting_ConcurrentCreatesGetDistinctReceipts(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	p := newReceipting(t, &fakeInner{pool: pool}, s)
	var wg sync.WaitGroup
	clones := make([]Clone, 2)
	errs := make([]error, 2)
	for i := range clones {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clones[i], errs[i] = p.Create(ctx, CloneSpec{})
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || clones[0].ID == clones[1].ID {
		t.Fatalf("clones %+v errors %v", clones, errs)
	}
	var receipts, names int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT name) FROM
		sage.clone_instances WHERE status = 'ready'`).Scan(&receipts, &names); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 || names != 2 {
		t.Fatalf("ready receipts %d with %d names, want 2 and 2", receipts, names)
	}
	for _, c := range clones {
		if err := p.Destroy(ctx, c); err != nil {
			t.Fatalf("destroy %s: %v", c.ID, err)
		}
	}
}

func TestReceiptTransitions_Guarded(t *testing.T) {
	s, _, ctx := receiptStore(t)
	r, err := s.Record(ctx, validReceipt("guard"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDestroying(ctx, r.ID); !errors.Is(err, ErrReceiptState) {
		t.Errorf("creating -> destroying: %v", err)
	}
	if _, err := s.MarkDestroyed(ctx, r.ID); !errors.Is(err, ErrReceiptState) {
		t.Errorf("creating -> destroyed: %v", err)
	}
	if _, err := s.MarkReady(ctx, r.ID, "c-guard"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkCreateFailed(ctx, r.ID, false); !errors.Is(err, ErrReceiptState) {
		t.Errorf("ready -> failed: %v", err)
	}
	for range 2 { // destroying may be marked again (a retried teardown)
		if got, err := s.MarkDestroying(ctx, r.ID); err != nil ||
			got.Status != StatusDestroying {
			t.Fatalf("ready/destroying -> destroying: %+v %v", got, err)
		}
	}
	if got, err := s.MarkDestroyed(ctx, r.ID); err != nil || got.Status != StatusDestroyed {
		t.Fatalf("destroying -> destroyed: %+v %v", got, err)
	}
	if _, err := s.MarkDestroying(ctx, r.ID); !errors.Is(err, ErrReceiptState) {
		t.Errorf("destroyed is terminal: %v", err)
	}
	if _, err := s.MarkDestroyed(ctx, 999999); !errors.Is(err, ErrReceiptNotFound) {
		t.Errorf("unknown receipt: %v", err)
	}
	if _, err := NewReceiptStore(nil).MarkDestroying(ctx, r.ID); !errors.Is(err,
		ErrNoReceiptStore) {
		t.Errorf("nil pool: %v", err)
	}
}

// No DB: constructor validation is pure.
func TestNewReceiptingProvider_Invalid(t *testing.T) {
	inner := &fakeInner{}
	for name, mut := range map[string]func(*ReceiptingConfig){
		"no adapter":    func(c *ReceiptingConfig) { c.Adapter = "" },
		"no scope":      func(c *ReceiptingConfig) { c.Scope = "" },
		"no deployment": func(c *ReceiptingConfig) { c.Deployment = nil },
		"bad purpose":   func(c *ReceiptingConfig) { c.Purpose = "fun" },
		"zero TTL":      func(c *ReceiptingConfig) { c.TTL = 0 },
		"negative TTL":  func(c *ReceiptingConfig) { c.TTL = -time.Minute },
	} {
		cfg := rehearsalConfig()
		mut(&cfg)
		if p, err := NewReceiptingProvider(inner, nil, cfg); err == nil || p != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p, err := NewReceiptingProvider(nil, nil, rehearsalConfig()); err == nil || p != nil {
		t.Error("nil inner provider accepted")
	}
}
