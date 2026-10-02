package policy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// serialize_mode queue (debt item 3): a request whose target is leased
// waits its turn, FIFO per target, in a durable queue (sage.lease_queue)
// that survives a restart, is bounded in depth and in wait, and ends in a
// timeout the caller parks or refuses on.

type queueHarness struct {
	t        *testing.T
	store    *Store
	pool     *pgxpool.Pool
	decision int64
	targets  []TypedTarget
	table    string
}

func newQueueHarness(t *testing.T) *queueHarness {
	t.Helper()
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	h := &queueHarness{t: t, store: store, pool: f.pool, table: f.table,
		decision: typedDecision(t, store)}
	h.targets = mustResolve(t, f.pool, "public."+f.table)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM sage.lease_queue WHERE intent LIKE $1", "%"+f.table+"%")
	})
	return h
}

// boundedCtx bounds a wait so a broken queue fails the test instead of
// hanging it.
func boundedCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testQueueConfig() LeaseQueueConfig {
	cfg := DefaultLeaseQueueConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.StaleAfter = 200 * time.Millisecond
	cfg.MaxWait = 10 * time.Second
	return cfg
}

func (h *queueHarness) manager() *PostgresLeaseManager {
	return NewPostgresLeaseManager(h.pool, nil, h.decision, time.Minute)
}

// hold leases the target directly, as a running writer would.
func (h *queueHarness) hold() func() {
	h.t.Helper()
	manager := h.manager()
	id, err := manager.AcquireTyped(context.Background(), "holder", h.targets, "hold "+h.table)
	if err != nil {
		h.t.Fatalf("hold: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = manager.ReleaseLease(context.Background(), id) }) }
	h.t.Cleanup(release)
	return release
}

func (h *queueHarness) request(name string, manager *PostgresLeaseManager) QueuedLeaseRequest {
	return QueuedLeaseRequest{Manager: manager, Kind: "finding", Actor: "executor:" + name,
		Intent: name + " on " + h.table, Targets: h.targets}
}

func (h *queueHarness) waitForEntries(want int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.entries("waiting") >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("waiting queue entries never reached %d (have %d)", want, h.entries("waiting"))
}

func (h *queueHarness) entries(state string) int {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.lease_queue
		WHERE intent LIKE $1 AND state=$2`, "%"+h.table+"%", state).Scan(&count); err != nil {
		h.t.Fatalf("count queue entries: %v", err)
	}
	return count
}

func TestLeaseQueueGrantsImmediatelyWhenFree(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	manager := h.manager()

	id, err := queue.Acquire(boundedCtx(t), h.request("free", manager))

	if err != nil || id == "" {
		t.Fatalf("acquire on a free target = %q, %v; want a lease", id, err)
	}
	defer func() { _ = manager.ReleaseLease(context.Background(), id) }()
	if total := h.entries("waiting") + h.entries("granted"); total != 0 {
		t.Fatalf("free target created %d queue entries, want none", total)
	}
}

func TestLeaseQueueGrantsInFIFOOrder(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	release := h.hold()
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for i, name := range []string{"first", "second", "third"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			manager := h.manager()
			id, err := queue.Acquire(boundedCtx(t), h.request(name, manager))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			_ = manager.ReleaseLease(context.Background(), id)
		}(name)
		h.waitForEntries(i + 1)
	}

	release()
	wg.Wait()

	if fmt.Sprint(order) != "[first second third]" {
		t.Fatalf("grant order = %v, want FIFO [first second third]", order)
	}
	if granted := h.entries("granted"); granted != 3 {
		t.Fatalf("granted entries = %d, want 3", granted)
	}
}

func TestLeaseQueueTimeoutReleasesPosition(t *testing.T) {
	h := newQueueHarness(t)
	cfg := testQueueConfig()
	cfg.MaxWait = 300 * time.Millisecond
	queue := NewLeaseQueue(h.pool, nil, cfg, "instance-a")
	release := h.hold()
	started := time.Now()

	_, err := queue.Acquire(boundedCtx(t), h.request("late", h.manager()))

	elapsed := time.Since(started)
	if !errors.Is(err, ErrLeaseQueueTimeout) || !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("queue wait past MaxWait = %v, want ErrLeaseQueueTimeout", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("waited %s, want about MaxWait (300ms)", elapsed)
	}
	if h.entries("timed_out") != 1 || h.entries("waiting") != 0 {
		t.Fatalf("timed_out=%d waiting=%d, want the entry resolved as timed_out",
			h.entries("timed_out"), h.entries("waiting"))
	}
	release()
	manager := h.manager()
	id, err := queue.Acquire(boundedCtx(t), h.request("next", manager))
	if err != nil {
		t.Fatalf("acquire after a timed-out entry = %v, want granted", err)
	}
	_ = manager.ReleaseLease(context.Background(), id)
}

func TestLeaseQueueBoundedPerTarget(t *testing.T) {
	h := newQueueHarness(t)
	cfg := testQueueConfig()
	cfg.MaxDepthPerTarget = 2
	queue := NewLeaseQueue(h.pool, nil, cfg, "instance-a")
	release := h.hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, name := range []string{"one", "two"} {
		go func(name string) { _, _ = queue.Acquire(ctx, h.request(name, h.manager())) }(name)
		h.waitForEntries(i + 1)
	}
	started := time.Now()

	_, err := queue.Acquire(boundedCtx(t), h.request("three", h.manager()))

	if !errors.Is(err, ErrLeaseQueueFull) || !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("third waiter = %v, want ErrLeaseQueueFull", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("a full queue waited %s, want an immediate refusal", time.Since(started))
	}
	if h.entries("waiting") != 2 {
		t.Fatalf("waiting entries = %d, want only the two admitted", h.entries("waiting"))
	}
	cancel()
	release()
}

func TestLeaseQueueBoundedTotalDepth(t *testing.T) {
	h := newQueueHarness(t)
	other := newTypedFixture(t, h.store)
	otherTargets := mustResolve(t, h.pool, "public."+other.table)
	cfg := testQueueConfig()
	cfg.MaxDepth = 1
	queue := NewLeaseQueue(h.pool, nil, cfg, "instance-a")
	h.hold()
	holdOther := h.manager()
	otherID, err := holdOther.AcquireTyped(context.Background(), "holder", otherTargets, "x")
	if err != nil {
		t.Fatalf("hold other: %v", err)
	}
	defer func() { _ = holdOther.ReleaseLease(context.Background(), otherID) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = queue.Acquire(ctx, h.request("one", h.manager())) }()
	h.waitForEntries(1)

	request := h.request("elsewhere", h.manager())
	request.Targets = otherTargets
	_, err = queue.Acquire(boundedCtx(t), request)

	if !errors.Is(err, ErrLeaseQueueFull) {
		t.Fatalf("waiter on another target past MaxDepth = %v, want ErrLeaseQueueFull", err)
	}
}

// A restarted sidecar re-submits the same request: it takes over its
// durable entry and keeps its place ahead of later arrivals.
func TestLeaseQueueResumesAfterRestart(t *testing.T) {
	h := newQueueHarness(t)
	release := h.hold()
	resumed := h.request("resumed", nil)
	var deadID int64
	err := h.pool.QueryRow(context.Background(), `INSERT INTO sage.lease_queue
		(request_key, object_keys, kind, actor, intent, decision_id, instance,
		 enqueued_at, heartbeat_at, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'crashed-instance',
		        now() - interval '1 minute', now() - interval '1 minute',
		        now() + interval '1 minute') RETURNING id`,
		leaseRequestKey(resumed.Kind, resumed.Intent, TypedLeaseKeys(h.targets)),
		TypedLeaseKeys(h.targets), resumed.Kind, resumed.Actor, resumed.Intent,
		h.decision).Scan(&deadID)
	if err != nil {
		t.Fatalf("insert pre-restart entry: %v", err)
	}
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "restarted-instance")
	var mu sync.Mutex
	var order []string
	run := func(request QueuedLeaseRequest, name string, wg *sync.WaitGroup) {
		defer wg.Done()
		manager := h.manager()
		request.Manager = manager
		id, err := queue.Acquire(boundedCtx(t), request)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		_ = manager.ReleaseLease(context.Background(), id)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go run(h.request("later", nil), "later", &wg)
	h.waitForEntries(2)
	go run(resumed, "resumed", &wg)
	waitForResume(t, h.pool, deadID)

	release()
	wg.Wait()

	if fmt.Sprint(order) != "[resumed later]" {
		t.Fatalf("grant order = %v, want the resumed entry first", order)
	}
	var count int
	var instance string
	if err := h.pool.QueryRow(context.Background(), `SELECT resumed_count, instance
		FROM sage.lease_queue WHERE id=$1`, deadID).Scan(&count, &instance); err != nil {
		t.Fatalf("read resumed entry: %v", err)
	}
	if count != 1 || instance != "restarted-instance" {
		t.Fatalf("resumed entry count=%d instance=%q, want 1 restarted-instance",
			count, instance)
	}
}

func waitForResume(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := pool.QueryRow(context.Background(),
			"SELECT resumed_count FROM sage.lease_queue WHERE id=$1", id).
			Scan(&count); err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if count > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the restarted request never resumed its durable entry")
}

// An entry whose owner never comes back keeps its place only until its
// deadline: the queue is bounded even across a crash.
func TestLeaseQueueAbandonedEntryExpiresAtDeadline(t *testing.T) {
	h := newQueueHarness(t)
	_, err := h.pool.Exec(context.Background(), `INSERT INTO sage.lease_queue
		(request_key, object_keys, kind, actor, intent, decision_id, instance,
		 heartbeat_at, deadline_at)
		VALUES ('abandoned-'||$1, $2, 'finding', 'executor', 'abandoned on '||$1, $3,
		        'crashed', now() - interval '1 minute',
		        now() + interval '400 milliseconds')`,
		h.table, TypedLeaseKeys(h.targets), h.decision)
	if err != nil {
		t.Fatalf("insert abandoned entry: %v", err)
	}
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	manager := h.manager()
	started := time.Now()

	id, err := queue.Acquire(boundedCtx(t), h.request("behind", manager))

	if err != nil {
		t.Fatalf("acquire behind an abandoned entry = %v, want granted after it expires", err)
	}
	_ = manager.ReleaseLease(context.Background(), id)
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("waited %s behind the abandoned entry, want until its deadline", elapsed)
	}
	if h.entries("timed_out") != 1 {
		t.Fatalf("abandoned entry was not resolved as timed_out")
	}
}

func TestLeaseQueueLiveDuplicateRefused(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	release := h.hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = queue.Acquire(ctx, h.request("same", h.manager())) }()
	h.waitForEntries(1)

	_, err := queue.Acquire(boundedCtx(t), h.request("same", h.manager()))

	if !errors.Is(err, ErrLeaseQueueDuplicate) || !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("identical live request = %v, want ErrLeaseQueueDuplicate", err)
	}
	cancel()
	release()
}

func TestLeaseQueueDisjointTargetsDoNotWait(t *testing.T) {
	h := newQueueHarness(t)
	other := newTypedFixture(t, h.store)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	release := h.hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = queue.Acquire(ctx, h.request("waiting", h.manager())) }()
	h.waitForEntries(1)
	manager := h.manager()
	request := h.request("elsewhere", manager)
	request.Targets = mustResolve(t, h.pool, "public."+other.table)
	started := time.Now()

	id, err := queue.Acquire(boundedCtx(t), request)

	if err != nil || time.Since(started) > 2*time.Second {
		t.Fatalf("disjoint target = %v after %s, want an immediate grant", err,
			time.Since(started))
	}
	_ = manager.ReleaseLease(context.Background(), id)
	cancel()
	release()
}

func TestLeaseQueueCancelledWaitLeavesQueue(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	h.hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel only once the entry is queued: a slow server must not turn
	// this into a cancellation before the request ever queued.
	go func() {
		h.waitForEntries(1)
		cancel()
	}()

	_, err := queue.Acquire(ctx, h.request("cancelled", h.manager()))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v, want the caller's context error", err)
	}
	if h.entries("cancelled") != 1 || h.entries("waiting") != 0 {
		t.Fatalf("cancelled=%d waiting=%d, want the entry resolved as cancelled",
			h.entries("cancelled"), h.entries("waiting"))
	}
}

// Waiters never hold the target together and are granted in queue order.
func TestLeaseQueueConcurrentWaitersAreExclusiveAndOrdered(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	release := h.hold()
	const waiters = 6
	var inside, maxInside atomic.Int32
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manager := h.manager()
			id, err := queue.Acquire(boundedCtx(t),
				h.request(fmt.Sprintf("w%d", i), manager))
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			now := inside.Add(1)
			if now > maxInside.Load() {
				maxInside.Store(now)
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(15 * time.Millisecond)
			inside.Add(-1)
			_ = manager.ReleaseLease(context.Background(), id)
		}(i)
		h.waitForEntries(i + 1)
	}

	release()
	wg.Wait()

	if maxInside.Load() != 1 {
		t.Fatalf("%d waiters held the target at once, want 1", maxInside.Load())
	}
	if fmt.Sprint(order) != "[0 1 2 3 4 5]" {
		t.Fatalf("grant order = %v, want enqueue order", order)
	}
}

func TestLeaseQueueRejectsInvalidRequests(t *testing.T) {
	h := newQueueHarness(t)
	queue := NewLeaseQueue(h.pool, nil, testQueueConfig(), "instance-a")
	valid := h.request("valid", h.manager())
	for name, mutate := range map[string]func(*QueuedLeaseRequest){
		"nil manager":   func(r *QueuedLeaseRequest) { r.Manager = nil },
		"no targets":    func(r *QueuedLeaseRequest) { r.Targets = nil },
		"blank kind":    func(r *QueuedLeaseRequest) { r.Kind = " " },
		"blank intent":  func(r *QueuedLeaseRequest) { r.Intent = "" },
		"negative wait": func(r *QueuedLeaseRequest) { r.MaxWait = -time.Second },
	} {
		request := valid
		mutate(&request)
		if _, err := queue.Acquire(boundedCtx(t), request); err == nil {
			t.Errorf("%s: acquire succeeded, want a validation error", name)
		}
	}
	var nilQueue *LeaseQueue
	if _, err := nilQueue.Acquire(context.Background(), valid); err == nil {
		t.Fatal("nil queue acquired a lease")
	}
	if total := h.entries("waiting") + h.entries("granted"); total != 0 {
		t.Fatalf("invalid requests created %d entries, want 0", total)
	}
}

func TestDefaultLeaseQueueConfigIsBounded(t *testing.T) {
	cfg := DefaultLeaseQueueConfig()
	if cfg.MaxWait <= 0 || cfg.MaxWait > 10*time.Minute ||
		cfg.OperatorMaxWait <= 0 || cfg.OperatorMaxWait > cfg.MaxWait ||
		cfg.MaxDepthPerTarget <= 0 || cfg.MaxDepth < cfg.MaxDepthPerTarget ||
		cfg.PollInterval <= 0 || cfg.StaleAfter <= cfg.PollInterval {
		t.Fatalf("default queue config %+v is not bounded and consistent", cfg)
	}
}

func TestGateCarriesSerializeModeOnExecute(t *testing.T) {
	for _, mode := range []string{SerializePark, SerializeQueue} {
		doc := UnattendedProfile()
		doc.SerializeMode = mode
		decision := withDocumentBounds(doc, Decision{Verdict: VerdictExecute})
		if decision.SerializeMode != mode {
			t.Fatalf("execute decision serialize mode = %q, want %q",
				decision.SerializeMode, mode)
		}
		refused := withDocumentBounds(doc, Decision{Verdict: VerdictQueueApproval})
		if refused.SerializeMode != "" || refused.LockCeilingMS != 0 {
			t.Fatalf("a non-execute verdict carried execution bounds: %+v", refused)
		}
	}
}
