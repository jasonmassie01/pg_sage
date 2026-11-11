package verify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-07: the open verifications' target queryids are the
// collector's durable pins, so a target leaving the top-N keeps its
// query_store samples, across a restart too.

// openPinWatch opens a watch on targets and returns its action id.
func openPinWatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	store *PostgresStateStore, targets []int64) int64 {
	t.Helper()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })
	now := time.Now().UTC()
	state := WatchState{
		ID: fmt.Sprintf("pin-target-%d", actionID), ActionID: actionID, ExecutedAt: now,
		Table: "public.orders", IndexName: "idx_orders", Status: "pending",
		Criterion: Criterion{Kind: "per_query_latency", TargetIDs: targets,
			Window: time.Minute, HardMax: time.Hour},
		Window: time.Minute, NextEvaluationAt: now.Add(time.Minute),
	}
	if err := store.Create(ctx, state); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	return actionID
}

// only keeps the ids of ids that are in mine, in order.
func only(ids, mine []int64) []int64 {
	out := []int64{}
	for _, id := range ids {
		if slices.Contains(mine, id) {
			out = append(out, id)
		}
	}
	return out
}

// Happy path and state transition: open watches' targets, oldest watch
// first, each once; a completed watch's target is no longer listed.
func TestOpenTargetIDs_OpenWatchesOldestFirst(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	store := NewPostgresStateStore(pool, 1)
	mine := []int64{-9101, 9102, 9103, 9104, 9105}
	openPinWatch(t, ctx, pool, store, []int64{9102, -9101})
	openPinWatch(t, ctx, pool, store, []int64{-9101, 9103})
	done := openPinWatch(t, ctx, pool, store, []int64{9104})
	ids, err := store.OpenTargetIDs(ctx)
	if err != nil {
		t.Fatalf("OpenTargetIDs: %v", err)
	}
	if got, want := only(ids, mine), []int64{-9101, 9102, 9103, 9104}; !slices.Equal(got, want) {
		t.Fatalf("open targets %v, want %v (oldest watch first, each once)", got, want)
	}
	state, err := store.Get(ctx, fmt.Sprintf("pin-target-%d", done))
	if err != nil {
		t.Fatalf("get watch: %v", err)
	}
	state.Status, state.Completed = "success", true
	if err := store.Update(ctx, state); err != nil {
		t.Fatalf("complete watch: %v", err)
	}
	ids, err = store.OpenTargetIDs(ctx)
	if err != nil || slices.Contains(ids, 9104) {
		t.Fatalf("after completion: %v (%v); want 9104 gone", only(ids, mine), err)
	}
}

// Nil/empty and invalid input: a watch with no targets, a null or scalar
// TargetIDs, non-numeric elements and integers outside int8 (19 digits,
// past 9223372036854775807) pin nothing and do not fail the read: one bad
// row must not drop every other watch's pins.
func TestOpenTargetIDs_MalformedCriterionPinsNothing(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	store := NewPostgresStateStore(pool, 1)
	empty := openPinWatch(t, ctx, pool, store, nil)
	scalar := openPinWatch(t, ctx, pool, store, []int64{1})
	mixed := openPinWatch(t, ctx, pool, store, []int64{1})
	for id, criterion := range map[int64]string{
		scalar: `{"Kind":"per_query_latency","TargetIDs":"9201"}`,
		mixed: `{"Kind":"per_query_latency","TargetIDs":["x9202", 9203, 1.5, null,
			9999999999999999999, -9223372036854775809, "9223372036854775808"]}`,
	} {
		mustExec(t, ctx, pool, `UPDATE sage.verification SET criterion = $2::jsonb
			WHERE action_log_id = $1`, id, criterion)
	}
	ids, err := store.OpenTargetIDs(ctx)
	if err != nil {
		t.Fatalf("OpenTargetIDs over malformed criteria: %v (watch %d)", err, empty)
	}
	if got := only(ids, []int64{9201, 9202, 9203}); !slices.Equal(got, []int64{9203}) {
		t.Fatalf("pins from malformed criteria %v, want only 9203", got)
	}
}

// Error propagation: an unavailable store is an error naming the read,
// never an empty list that would silently stop the pins.
func TestOpenTargetIDs_UnavailableStore(t *testing.T) {
	var nilStore *PostgresStateStore
	if _, err := nilStore.OpenTargetIDs(context.Background()); err == nil {
		t.Fatal("nil store: want an error")
	}
	if _, err := NewPostgresStateStore(nil, 1).OpenTargetIDs(context.Background()); err == nil {
		t.Fatal("store without a pool: want an error")
	}
	dsn := testdb.SkipUnlessLive(t)
	closed, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	closed.Close()
	_, err = NewPostgresStateStore(closed, 1).OpenTargetIDs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "open verification targets") {
		t.Fatalf("closed pool: %v, want an error naming the open-targets read", err)
	}
}

// No concurrent access test: OpenTargetIDs is one read-only statement with
// no state of its own.
