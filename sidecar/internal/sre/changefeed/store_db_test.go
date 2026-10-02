package changefeed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The durable change feed (sage.sre_change_events): one row per source
// and event id, scoped to a database or deployment-wide, read back in a
// window. A replayed submission is a duplicate, never a second row; the
// same id with other content is a conflict.

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// bindScope binds a fresh database identity in the pool's deployment.
func bindScope(t *testing.T, ctx context.Context, pool *pgxpool.Pool) sre.Scope {
	t.Helper()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.EnsureDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := st.BindDatabase(ctx, sre.Binding{DeploymentID: dep,
		RuntimeKey: "changefeed-test:" + string(sre.NewUUID()),
		Strength:   sre.StrengthConfigured, ClusterEpoch: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func liveStore(t *testing.T) (*Store, sre.Scope, context.Context) {
	t.Helper()
	pool, ctx := livePool(t)
	st, err := NewStore(pool)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st, bindScope(t, ctx, pool), ctx
}

func uniqueSubmission(at time.Time) Submission {
	s := validSubmission()
	s.EventID = "run-" + string(sre.NewUUID())
	s.OccurredAt = at
	return s
}

func TestNewStore_NilPool(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
}

func TestStore_RecordIsIdempotentPerSourceAndEventID(t *testing.T) {
	st, scope, ctx := liveStore(t)
	now := time.Now().UTC()
	e, err := uniqueSubmission(now.Add(-time.Minute)).Event(now)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := st.Record(ctx, scope, true, e)
	if err != nil || !created {
		t.Fatalf("first record: created=%v err=%v", created, err)
	}
	if _, err := sre.ParseUUID(string(first.ID)); err != nil {
		t.Fatalf("stored id %q: %v", first.ID, err)
	}
	replay, created, err := st.Record(ctx, scope, true, e)
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("replay: id=%s created=%v err=%v, want the first row", replay.ID,
			created, err)
	}
	changed := e
	changed.Summary = "a different summary"
	changed.Hash = hashOf(changed)
	if _, _, err := st.Record(ctx, scope, true, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("same id, other content: err = %v, want ErrConflict", err)
	}
	got, err := st.List(ctx, scope, Filter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range got {
		if g.EventID == e.EventID {
			n++
			if g.Summary != e.Summary || g.Signature != SignatureVerified ||
				!g.Scoped || g.Hash != e.Hash || g.Link != e.Link {
				t.Fatalf("stored event = %+v", g)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d rows for one event id", n)
	}
}

// A database-scoped event belongs to its database only; a deployment-wide
// event (no database) is visible to every database of the deployment.
func TestStore_ScopeIsolation(t *testing.T) {
	st, a, ctx := liveStore(t)
	b := bindScope(t, ctx, st.pool)
	now := time.Now().UTC()
	// The deployment-wide event is older than the other tests' windows:
	// every database of this test deployment sees it.
	onlyA, _ := uniqueSubmission(now.Add(-time.Minute)).Event(now)
	wide, _ := uniqueSubmission(now.Add(-2 * time.Hour)).Event(now)
	if _, _, err := st.Record(ctx, a, true, onlyA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Record(ctx, a, false, wide); err != nil {
		t.Fatal(err)
	}
	ids := func(scope sre.Scope) map[string]bool {
		evs, err := st.List(ctx, scope, Filter{Since: now.Add(-3 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, e := range evs {
			out[e.EventID] = true
		}
		return out
	}
	ga, gb := ids(a), ids(b)
	if !ga[onlyA.EventID] || !ga[wide.EventID] {
		t.Fatalf("database a sees %v", ga)
	}
	if gb[onlyA.EventID] || !gb[wide.EventID] {
		t.Fatalf("database b sees %v: a's scoped event leaked or the wide one is missing", gb)
	}
}

// The window and the limit bound a list; newest first.
func TestStore_ListWindowOrderAndLimit(t *testing.T) {
	st, scope, ctx := liveStore(t)
	now := time.Now().UTC()
	var want []string
	for i := 1; i <= 5; i++ {
		e, _ := uniqueSubmission(now.Add(-time.Duration(i) * time.Minute)).Event(now)
		if _, _, err := st.Record(ctx, scope, true, e); err != nil {
			t.Fatal(err)
		}
		want = append(want, e.EventID)
	}
	old, _ := uniqueSubmission(now.Add(-3 * time.Hour)).Event(now)
	if _, _, err := st.Record(ctx, scope, true, old); err != nil {
		t.Fatal(err)
	}
	got, err := st.List(ctx, scope, Filter{Since: now.Add(-10 * time.Minute), Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].EventID != want[0] || got[2].EventID != want[2] {
		t.Fatalf("list = %v, want the 3 newest of %v", eventIDs(got), want)
	}
	for _, g := range got {
		if g.EventID == old.EventID {
			t.Fatal("an event outside the window was listed")
		}
	}
	all, _ := st.List(ctx, scope, Filter{Since: now.Add(-10 * time.Minute)})
	if len(all) != 5 {
		t.Fatalf("default limit listed %d of 5", len(all))
	}
	if _, err := st.List(ctx, scope, Filter{Limit: MaxList + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("limit over the cap: err = %v", err)
	}
	if _, err := st.List(ctx, sre.Scope{}, Filter{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid scope: err = %v", err)
	}
}

func eventIDs(es []Event) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.EventID)
	}
	return out
}

// Retention deletes events received before the cutoff and keeps newer ones.
func TestStore_Purge(t *testing.T) {
	st, scope, ctx := liveStore(t)
	now := time.Now().UTC()
	e, _ := uniqueSubmission(now.Add(-time.Minute)).Event(now)
	if _, _, err := st.Record(ctx, scope, true, e); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE sage.sre_change_events
		SET received_at = now() - interval '100 days' WHERE event_id = $1`,
		e.EventID); err != nil {
		t.Fatal(err)
	}
	keep, _ := uniqueSubmission(now.Add(-time.Minute)).Event(now)
	if _, _, err := st.Record(ctx, scope, true, keep); err != nil {
		t.Fatal(err)
	}
	n, err := st.Purge(ctx, scope.DeploymentID, now.Add(-90*24*time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	got, _ := st.List(ctx, scope, Filter{Since: now.Add(-time.Hour)})
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.EventID] = true
	}
	if ids[e.EventID] || !ids[keep.EventID] {
		t.Fatalf("after purge: %v", ids)
	}
}

// Concurrent replays of one submission store exactly one row.
func TestStore_ConcurrentReplays(t *testing.T) {
	st, scope, ctx := liveStore(t)
	now := time.Now().UTC()
	e, _ := uniqueSubmission(now.Add(-time.Minute)).Event(now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, ids := 0, map[sre.UUID]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, c, err := st.Record(ctx, scope, true, e)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("record: %v", err)
				return
			}
			if c {
				created++
			}
			ids[got.ID] = true
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Fatalf("created=%d distinct ids=%d, want exactly one row", created, len(ids))
	}
}

// The feed binds a store to one database: ingest scopes by the submitted
// database, Recent reads its window and Probe renders typed evidence.
func TestFeed_IngestRecentAndProbe(t *testing.T) {
	st, scope, ctx := liveStore(t)
	feed := NewFeed(st, "orders", func(context.Context) (sre.Scope, error) {
		return scope, nil
	})
	now := time.Now().UTC()
	sub := uniqueSubmission(now.Add(-time.Minute))
	sub.Database = "orders"
	e, created, err := feed.Ingest(ctx, sub, now)
	if err != nil || !created || !e.Scoped {
		t.Fatalf("ingest: %+v created=%v err=%v", e, created, err)
	}
	recent, err := feed.Recent(ctx, time.Hour, 10)
	if err != nil || len(recent) == 0 || recent[0].EventID != sub.EventID {
		t.Fatalf("recent = %v err=%v", eventIDs(recent), err)
	}
	res := feed.Probe(ctx, probes.Args{Window: time.Hour})
	if res.ProbeID != probes.ChangeFeed || res.Status != probes.StatusOK ||
		res.Version != "v1" || res.ObservedAt.IsZero() {
		t.Fatalf("probe = %+v", res)
	}
	rowsOut, err := probes.ChangeRows(res)
	if err != nil || len(rowsOut) == 0 {
		t.Fatalf("decode: %v rows=%d", err, len(rowsOut))
	}
	r := rowsOut[0]
	if r.Kind != string(KindDeploy) || r.Source != "github-actions" ||
		r.Summary != sub.Summary || r.Signature != SignatureVerified || r.AgeS < 50 ||
		r.EventID != sub.EventID {
		t.Fatalf("row = %+v", r)
	}
}

func TestFeed_ProbeEmptyAndUnavailable(t *testing.T) {
	st, scope, ctx := liveStore(t)
	feed := NewFeed(st, "orders", func(context.Context) (sre.Scope, error) {
		return scope, nil
	})
	res := feed.Probe(ctx, probes.Args{Window: time.Minute})
	if res.Status != probes.StatusEmpty {
		t.Fatalf("no changes: status %s, want empty (an observed absence)", res.Status)
	}
	broken := NewFeed(st, "orders", func(context.Context) (sre.Scope, error) {
		return sre.Scope{}, fmt.Errorf("%w: down", sre.ErrMetadataUnavailable)
	})
	res = broken.Probe(ctx, probes.Args{})
	if res.Status != probes.StatusError || res.Reason != "store_unavailable" {
		t.Fatalf("unbound feed: %+v, want an error, never an empty (healthy) result", res)
	}
	var nilFeed *Feed
	if res := nilFeed.Probe(ctx, probes.Args{}); res.Status != probes.StatusError {
		t.Fatalf("nil feed: %+v", res)
	}
}

// A submission naming another database is refused by this feed.
func TestFeed_IngestRejectsAnotherDatabase(t *testing.T) {
	st, scope, ctx := liveStore(t)
	feed := NewFeed(st, "orders", func(context.Context) (sre.Scope, error) {
		return scope, nil
	})
	sub := uniqueSubmission(time.Now().UTC().Add(-time.Minute))
	sub.Database = "billing"
	if _, _, err := feed.Ingest(ctx, sub, time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other database: err = %v", err)
	}
	if feed.Name() != "orders" || feed.Store() != st || st.Key() == nil {
		t.Fatal("feed accessors are wrong")
	}
}
