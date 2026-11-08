package lakebaseobs

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func dbStore(t *testing.T) (*Store, string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "lakebaseobs_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding := "fixture-" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", binding)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1", binding)
	})
	return s, binding
}

func TestDBInboxCursorAtomicCASAndAcknowledgement(t *testing.T) {
	s, b := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e := eventFor(t, Plans, now.Add(-time.Minute))
	after := Cursor{ForwardThrough: now, LastPoll: now}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.CommitBatch(ctx, b, Plans, Cursor{}, after, []Event{e, e}, SourceRetention)
		}()
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS success=%d conflict=%d", success, conflict)
	}
	c, err := s.LoadCursor(ctx, b, Plans)
	if err != nil || c.Revision != 1 || !c.ForwardThrough.Equal(now) {
		t.Fatalf("cursor %+v %v", c, err)
	}
	pending, err := s.Pending(ctx, b, "plan.v1", 10, now)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d %v", len(pending), err)
	}
	if err = s.Ack(ctx, b, e.ID, "plan.v1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(ctx, b, e.ID, "plan.v1"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending(ctx, b, "plan.v1", 10, now)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acked delivery %d %v", len(pending), err)
	}
	pending, err = s.Pending(ctx, b, "new-consumer.v1", 10, now)
	if err != nil || len(pending) != 1 {
		t.Fatal("new subscriber did not receive retained event")
	}
}

func TestDBRetentionDoesNotResurrectPayloadOrForgetAck(t *testing.T) {
	s, b := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e := eventFor(t, Plans, now.Add(-30*time.Minute))
	c := Cursor{}
	if err := s.CommitBatch(ctx, b, Plans, c, c, []Event{e}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, b, e.ID, "consumer.v1"); err != nil {
		t.Fatal(err)
	}
	later := now.Add(31 * time.Minute)
	if err := s.Purge(ctx, later); err != nil {
		t.Fatal(err)
	}
	var payloadNull bool
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT event IS NULL FROM sage.lakebaseobs_inbox WHERE binding=$1 AND event_id=$2", b, e.ID).Scan(&payloadNull); err != nil || !payloadNull {
		t.Fatalf("payload not erased %v %v", payloadNull, err)
	}
	c, _ = s.LoadCursor(ctx, b, Plans)
	if err := s.CommitBatch(ctx, b, Plans, c, c, []Event{e}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1", b).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ACK erased %d %v", count, err)
	}
	pending, err := s.Pending(ctx, b, "different.v1", 10, later)
	if err != nil || len(pending) != 0 {
		t.Fatal("replay resurrected erased payload")
	}
	if err = s.Purge(ctx, now.Add(9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1", b).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired tombstone ACK not purged")
	}
}

func TestDBHistoryScopesBoundsAndExpiry(t *testing.T) {
	s, b := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := eventFor(t, Plans, now.Add(-30*time.Minute))
	recent := eventFor(t, Plans, now.Add(-time.Minute))
	log := eventFor(t, Logs, now.Add(-time.Minute))
	if err := s.CommitBatch(ctx, b, Plans, Cursor{}, Cursor{}, []Event{old, recent}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitBatch(ctx, b, Logs, Cursor{}, Cursor{}, []Event{log}, time.Hour); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, b, Plans, now.Add(-time.Hour), now, 1, now)
	if err != nil || len(rows) != 1 || rows[0].ID != recent.ID {
		t.Fatalf("bounded history %+v %v", rows, err)
	}
	rows, err = s.History(ctx, "another-binding", Plans, now.Add(-time.Hour), now, 10, now)
	if err != nil || len(rows) != 0 {
		t.Fatal("cross-binding history leak")
	}
	rows, err = s.History(ctx, b, Plans, now.Add(-time.Hour), now, 10, now.Add(31*time.Minute))
	if err != nil || len(rows) != 1 || rows[0].ID != recent.ID {
		t.Fatal("expired payload leaked")
	}
	if _, err = s.History(ctx, b, Plans, now.Add(-time.Hour), now, 1001, now); !errors.Is(err, ErrScope) {
		t.Fatal("unbounded history accepted")
	}
}

type readerFunc func(context.Context, Stream, Scope, Window) (Batch, error)

func (f readerFunc) ReadWindow(ctx context.Context, s Stream, scope Scope, w Window) (Batch, error) {
	return f(ctx, s, scope, w)
}
func runtimeStore(t *testing.T, reader Reader, o Options, consumer ...Consumer) (*Runtime, *Store) {
	t.Helper()
	s, _ := dbStore(t)
	scope := testScope()
	scope.DatabaseID = uuid.NewString()
	rt, err := NewRuntime(reader, s, scope, o, consumer...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", rt.Binding())
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1", rt.Binding())
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_owners WHERE deployment_id=$1 AND database_id=$2", scope.DeploymentID, scope.DatabaseID)
	})
	return rt, s
}

func TestDBOwnerCleanupDisabledScopeRevocationAndSharedControl(t *testing.T) {
	s, _ := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first := testScope()
	first.DeploymentID = uuid.NewString()
	first.DatabaseID = uuid.NewString()
	next := first
	next.BranchID = "new-owned-branch"
	other := first
	other.DatabaseID = uuid.NewString()
	bindings := []string{}
	t.Cleanup(func() {
		for _, binding := range bindings {
			_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", binding)
			_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1", binding)
		}
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_owners WHERE deployment_id=$1", first.DeploymentID)
	})
	insert := func(scope Scope, metadata bool) (string, Event) {
		t.Helper()
		if err := s.BindOwner(ctx, scope); err != nil {
			t.Fatal(err)
		}
		mode := "payload"
		if metadata {
			mode = "metadata"
		}
		binding := scope.Hash() + ":v1:" + mode
		bindings = append(bindings, binding)
		f := testFields(Plans, now.Add(-2*time.Hour))
		f["branch_id"] = scope.BranchID
		e, err := NewEvent(Plans, scope, f, now, !metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention); err != nil {
			t.Fatal(err)
		}
		return binding, e
	}
	b1, e1 := insert(first, false)
	b2, _ := insert(next, false)
	bm, _ := insert(next, true)
	bo, _ := insert(other, false)
	if err := s.Ack(ctx, b1, e1.ID, "sink.v1"); err != nil {
		t.Fatal(err)
	}
	count := func(binding string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1 AND event IS NOT NULL", binding).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	owned, err := s.OwnerScopes(ctx, first.DeploymentID, first.DatabaseID)
	if err != nil || len(owned) != 2 {
		t.Fatalf("ownership %+v %v", owned, err)
	}
	if err = s.CleanupOwner(ctx, first.DeploymentID, first.DatabaseID, next.Hash(), true, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if count(b1) != 0 || count(b2) != 1 || count(bm) != 1 || count(bo) != 1 {
		t.Fatal("scope change did not erase only removed resource")
	}
	if err = s.CleanupOwner(ctx, first.DeploymentID, first.DatabaseID, next.Hash(), false, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if count(b2) != 0 || count(bm) != 1 || count(bo) != 1 {
		t.Fatal("payload revocation leaked raw payload or erased other database/metadata")
	}
	if err = s.CleanupOwner(ctx, first.DeploymentID, first.DatabaseID, "", false, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if count(bm) != 0 || count(bo) != 1 {
		t.Fatal("disabled source retained metadata or crossed owner boundary")
	}
	var ackCount int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1", b1).Scan(&ackCount); err != nil || ackCount != 1 {
		t.Fatal("revocation erased replay protection")
	}
	// A separate active owner can tighten event-age retention with collection off.
	if err = s.CleanupOwner(ctx, other.DeploymentID, other.DatabaseID, other.Hash(), true, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if count(bo) != 0 {
		t.Fatal("independent retention did not erase old raw payload")
	}
	if err = s.CleanupOwner(ctx, "not-a-uuid", first.DatabaseID, "", false, SourceRetention, now); !errors.Is(err, ErrScope) {
		t.Fatal("invalid ownership accepted")
	}
}

func TestDBCleanupRetainsVerifiedSamePolicyDuringSDKOutage(t *testing.T) {
	s, _ := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	scope := testScope()
	scope.DeploymentID = uuid.NewString()
	scope.DatabaseID = uuid.NewString()
	scope.PolicyBinding = strings.Repeat("a", 64)
	second := scope
	second.DatabaseOID = 124 // Earlier verified OID under same configured endpoint.
	bindings := []string{}
	t.Cleanup(func() {
		for _, b := range bindings {
			_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", b)
			_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1", b)
		}
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_owners WHERE deployment_id=$1", scope.DeploymentID)
	})
	for _, sc := range []Scope{scope, second} {
		if err := s.BindOwner(ctx, sc); err != nil {
			t.Fatal(err)
		}
		f := testFields(Plans, now.Add(-time.Minute))
		f["dbid"] = strconv.FormatInt(sc.DatabaseOID, 10)
		e, err := NewEvent(Plans, sc, f, now, true)
		if err != nil {
			t.Fatal(err)
		}
		b := sc.Hash() + ":v1:payload"
		bindings = append(bindings, b)
		if err = s.CommitBatch(ctx, b, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention); err != nil {
			t.Fatal(err)
		}
	}
	// No SDK calls/reader exist here. Startup can safely retain prior evidence.
	if err := s.CleanupOwnerPolicy(ctx, scope.DeploymentID, scope.DatabaseID, scope.PolicyBinding, true, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	for _, b := range bindings {
		events, err := s.History(ctx, b, Plans, now.Add(-time.Hour), now, 10, now)
		if err != nil || len(events) != 1 {
			t.Fatal("transient SDK failure erased verified same-policy evidence")
		}
	}
	allowed, err := s.PolicyOwnerScopes(ctx, scope.DeploymentID, scope.DatabaseID, scope.PolicyBinding)
	if err != nil || len(allowed) != 2 {
		t.Fatalf("policy lookup %+v %v", allowed, err)
	}
	// Once SDK verification succeeds, exact current scope can retire old OID.
	if err = s.CleanupOwner(ctx, scope.DeploymentID, scope.DatabaseID, second.Hash(), true, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	old, err := s.History(ctx, bindings[0], Plans, now.Add(-time.Hour), now, 10, now)
	if err != nil || len(old) != 0 {
		t.Fatal("verified scope change retained old payload")
	}
	// A changed configured binding revokes all old scopes immediately, even
	// before a new workspace credential/resource lookup can succeed.
	if err = s.CleanupOwnerPolicy(ctx, scope.DeploymentID, scope.DatabaseID, strings.Repeat("b", 64), true, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	latest, err := s.History(ctx, bindings[1], Plans, now.Add(-time.Hour), now, 10, now)
	if err != nil || len(latest) != 0 {
		t.Fatal("changed unverified policy retained previous source payload")
	}
}

func fenceOwner(t *testing.T) (*Store, Scope, string, Event) {
	t.Helper()
	s, _ := dbStore(t)
	scope := testScope()
	scope.DeploymentID = uuid.NewString()
	scope.DatabaseID = uuid.NewString()
	scope.PolicyBinding = strings.Repeat("c", 64)
	if err := s.ActivateOwner(context.Background(), scope, true); err != nil {
		t.Fatal(err)
	}
	binding := scope.Hash() + ":v1:payload:fence"
	e, err := NewEvent(Plans, scope, testFields(Plans, time.Now().Add(-time.Minute)), time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", binding)
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1", binding)
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_owners WHERE deployment_id=$1", scope.DeploymentID)
		_, _ = s.pool.Exec(ctx, "DELETE FROM sage.lakebaseobs_owner_policy WHERE deployment_id=$1", scope.DeploymentID)
	})
	return s, scope, binding, e
}

func waitForPolicyLock(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%lakebaseobs_owner_policy%' AND pid<>pg_backend_pid())`, s.pool.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("policy worker never reached the expected database lock barrier")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDBRevocationFencesAnAlreadyWaitingBatchCommit(t *testing.T) {
	s, scope, binding, e := fenceOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Hold the revoking transaction at the actual policy update row lock.
	revoke, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = revoke.Rollback(ctx) }()
	if err = s.setOwnerPolicy(ctx, revoke, scope.DeploymentID, scope.DatabaseID, nil, false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention) }()
	waitForPolicyLock(t, s)
	if err = revoke.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrPolicy) {
		t.Fatalf("stale in-flight batch accepted after revoke: %v", err)
	}
	c, err := s.LoadCursor(ctx, binding, Plans)
	if err != nil || c.Revision != 0 {
		t.Fatal("rejected batch advanced cursor")
	}
	var n int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1", binding).Scan(&n); err != nil || n != 0 {
		t.Fatal("rejected batch persisted payload")
	}
}

func TestDBDeliveryFenceOrdersSinkCommitBeforeCleanup(t *testing.T) {
	s, scope, binding, e := fenceOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now()
	if err := s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	sink := map[string]bool{}
	var mu sync.Mutex
	consumer := ConsumerFunc{"fenced_sink.v1", func(_ context.Context, event Event) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		sink[event.ID] = true
		mu.Unlock()
		return nil
	}}
	deliveryDone := make(chan error, 1)
	go func() { deliveryDone <- s.DeliverAndAck(ctx, binding, e.ID, consumer, now) }()
	<-entered
	cleanupDone := make(chan error, 1)
	go func() {
		err := s.CleanupOwner(ctx, scope.DeploymentID, scope.DatabaseID, "", false, SourceRetention, now)
		if err == nil {
			mu.Lock()
			delete(sink, e.ID)
			mu.Unlock()
		}
		cleanupDone <- err
	}()
	waitForPolicyLock(t, s)
	close(release)
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	remaining := len(sink)
	mu.Unlock()
	if remaining != 0 {
		t.Fatal("sink commit repopulated payload after cleanup")
	}
	var callback bool
	err := s.DeliverAndAck(ctx, binding, e.ID, ConsumerFunc{"another_sink.v1", func(context.Context, Event) error { callback = true; return nil }}, now)
	if !errors.Is(err, ErrPolicy) || callback {
		t.Fatalf("previously loaded event bypassed revoke: %v called=%v", err, callback)
	}
}

func TestDBOlderProcessCannotReactivateDisabledPolicyOrPoll(t *testing.T) {
	s, scope, binding, e := fenceOwner(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := s.WithPolicyEpoch(now.Add(-time.Minute))
	newer := s.WithPolicyEpoch(now)
	if err := old.ActivateOwner(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	if err := newer.CleanupOwner(ctx, scope.DeploymentID, scope.DatabaseID, "", false, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if err := old.ActivateOwner(ctx, scope, true); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("older first bind reactivated newer disable: %v", err)
	}
	if err := old.CleanupOwner(ctx, scope.DeploymentID, scope.DatabaseID, scope.Hash(), true, SourceRetention, now); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("older cleanup widened newer policy: %v", err)
	}
	if err := old.BindOwner(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := old.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention); !errors.Is(err, ErrPolicy) {
		t.Fatal("BindOwner reactivated disabled policy")
	}
	reads := 0
	o := testOptions()
	o.Streams = []Stream{Plans}
	runtime, err := NewRuntime(readerFunc(func(context.Context, Stream, Scope, Window) (Batch, error) { reads++; return Batch{}, nil }), old, scope, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Poll(ctx, now); !errors.Is(err, ErrPolicy) || reads != 0 {
		t.Fatalf("old Poll reactivated source or queried after revoke: %v reads=%d", err, reads)
	}
	// The older process may finish its very first provider lookup only after a
	// newer instance disabled the source. No prior ownership row is required
	// for the newer policy epoch to fence that late activation transaction.
	fresh := scope
	fresh.DatabaseID = uuid.NewString()
	if err = newer.CleanupOwner(ctx, fresh.DeploymentID, fresh.DatabaseID, "", false, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if err = old.ActivateOwner(ctx, fresh, true); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("late first bind bypassed newer disable: %v", err)
	}
	owners, err := s.OwnerScopes(ctx, fresh.DeploymentID, fresh.DatabaseID)
	if err != nil || len(owners) != 0 {
		t.Fatal("rejected activation did not roll back ownership insert")
	}
}

func TestDBPayloadRevocationAllowsMetadataButNotHeldRawEvent(t *testing.T) {
	s, scope, binding, e := fenceOwner(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Pending(ctx, binding, "sink.v1", 10, now)
	if err != nil || len(loaded) != 1 {
		t.Fatal("fixture pending event missing")
	}
	if err = s.CleanupOwner(ctx, scope.DeploymentID, scope.DatabaseID, scope.Hash(), false, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = s.DeliverAndAck(ctx, binding, loaded[0].ID, ConsumerFunc{"sink.v1", func(context.Context, Event) error { called = true; return nil }}, now); err != nil || called {
		t.Fatal("held raw event delivered after payload revoke")
	}
	c, err := s.LoadCursor(ctx, binding, Plans)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CommitBatch(ctx, binding, Plans, c, c, []Event{e}, SourceRetention); !errors.Is(err, ErrPolicy) {
		t.Fatal("raw batch accepted under metadata policy")
	}
	metadata, err := NewEvent(Plans, scope, testFields(Plans, now.Add(-time.Minute)), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CommitBatch(ctx, binding, Plans, c, c, []Event{metadata}, SourceRetention); err != nil {
		t.Fatalf("metadata policy rejected safe metadata: %v", err)
	}
}

func TestDBRuntimeReplayLateOldComputeAndSinkCrash(t *testing.T) {
	now := time.Now().UTC()
	delayed := false
	reader := readerFunc(func(_ context.Context, stream Stream, scope Scope, w Window) (Batch, error) {
		at := now.Add(-time.Hour)
		f := testFields(stream, at)
		f["compute_id"] = "old-compute"
		e, err := NewEvent(stream, scope, f, now, true)
		if err != nil {
			return Batch{}, err
		}
		events := []Event{}
		if !at.Before(w.From) && at.Before(w.To) {
			events = append(events, e, e)
		}
		if delayed {
			at = now.Add(-2 * time.Hour)
			f = testFields(stream, at)
			f["compute_id"] = "new-compute"
			e, _ = NewEvent(stream, scope, f, now, true)
			if !at.Before(w.From) && at.Before(w.To) {
				events = append(events, e)
			}
		}
		return Batch{Events: events}, nil
	})
	// Simulate durable sink commit followed by a process failure before ACK.
	sink := map[string]bool{}
	calls := 0
	fail := true
	consumer := ConsumerFunc{"sink.v1", func(_ context.Context, e Event) error {
		calls++
		sink[e.ID] = true
		if fail {
			fail = false
			return errors.New("crash after sink commit")
		}
		return nil
	}}
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.Window = SourceRetention
	rt, s := runtimeStore(t, reader, o, consumer)
	if err := rt.Poll(context.Background(), now); err == nil {
		t.Fatal("expected subscriber crash")
	}
	if len(sink) != 1 {
		t.Fatal("duplicate delivery created duplicate sink event")
	}
	if err := rt.Poll(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(context.Background(), rt.Binding(), "sink.v1", 10, now)
	if err != nil || len(pending) != 0 || len(sink) != 1 || calls != 2 {
		t.Fatalf("sink replay %+v calls=%d pending=%d err=%v", sink, calls, len(pending), err)
	}
	delayed = true
	if err = rt.Poll(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(sink) != 2 {
		t.Fatal("late old-epoch event was lost")
	}
	status := rt.Status()
	if len(status) != 1 || !status[0].HistoricalOnly || status[0].Complete || status[0].ExactMultiplicity {
		t.Fatal("invented completeness or live evidence")
	}
	// Payload upgrade starts a separate backfill, without changing resource scope.
	o.AllowPayload = false
	metadata, err := NewRuntime(reader, s, rt.scope, o)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Binding() == rt.Binding() {
		t.Fatal("payload policy did not reset pipeline epoch")
	}
}

func TestDBRuntimeSaturationAndOutageDoNotSkipUnknownHistory(t *testing.T) {
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.QueriesPerPoll = 2
	rt, s := runtimeStore(t, readerFunc(func(context.Context, Stream, Scope, Window) (Batch, error) { return Batch{}, ErrBudget }), o)
	if err := rt.Poll(context.Background(), time.Now()); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	c, err := s.LoadCursor(context.Background(), rt.Binding(), Plans)
	if err != nil || c.Revision != 0 || !c.ForwardThrough.IsZero() {
		t.Fatalf("saturated window skipped %+v %v", c, err)
	}
	now := time.Now().UTC()
	old := Cursor{LastPoll: now.Add(-9 * 24 * time.Hour), ForwardThrough: now.Add(-9 * 24 * time.Hour), ReconcileThrough: now.Add(-9 * 24 * time.Hour)}
	if err = s.CommitBatch(context.Background(), rt.Binding(), Plans, Cursor{}, old, nil, SourceRetention); err != nil {
		t.Fatal(err)
	}
	rt.reader = readerFunc(func(context.Context, Stream, Scope, Window) (Batch, error) { return Batch{}, nil })
	if err = rt.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	c, err = s.LoadCursor(context.Background(), rt.Binding(), Plans)
	if err != nil || c.GapCount == 0 || c.LastGap == "" {
		t.Fatalf("lost expired-source gap %+v %v", c, err)
	}
}
